// Package termimg draws transcript thumbnails with the terminal's native
// graphics protocol, Kitty or Sixel: it detects which one the terminal
// speaks, prepares scaled pixels off the event loop, and places or deletes
// them by screen cell without disturbing the text layer.
package termimg

import (
	"bytes"
	"container/list"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"strings"
	"sync"

	tcell "github.com/gdamore/tcell/v3"
	"github.com/mattn/go-sixel"

	"github.com/alexschlessinger/pollytool/images"
)

type Protocol uint8

const (
	ProtocolNone Protocol = iota
	ProtocolKitty
	ProtocolSixel
)

func (p Protocol) String() string {
	switch p {
	case ProtocolKitty:
		return "kitty"
	case ProtocolSixel:
		return "sixel"
	default:
		return "none"
	}
}

// DetectProtocol is conservative: emitting an unsupported image
// protocol can put escape payloads into the terminal. POLLYTOOL_IMAGE_PROTOCOL
// is an escape hatch for compatible terminals that do not identify themselves.
func DetectProtocol(getenv func(string) string) Protocol {
	override := strings.ToLower(strings.TrimSpace(getenv("POLLYTOOL_IMAGE_PROTOCOL")))
	switch override {
	case "none", "off", "0":
		return ProtocolNone
	}
	// Multiplexer passthrough and placement ownership need a separate design.
	// Keep the first slice honest and fall back to captions inside them even
	// when a native protocol was explicitly requested.
	if getenv("TMUX") != "" || getenv("ZELLIJ") != "" || getenv("ZELLIJ_SESSION_NAME") != "" {
		return ProtocolNone
	}
	switch override {
	case "kitty":
		return ProtocolKitty
	case "sixel":
		return ProtocolSixel
	}

	if getenv("WT_SESSION") != "" {
		return ProtocolSixel
	}
	if getenv("KITTY_WINDOW_ID") != "" || getenv("WEZTERM_PANE") != "" || getenv("GHOSTTY_RESOURCES_DIR") != "" {
		return ProtocolKitty
	}
	identity := strings.ToLower(strings.Join([]string{
		getenv("TERM"),
		getenv("TERM_PROGRAM"),
		getenv("LC_TERMINAL"),
	}, " "))
	if strings.Contains(identity, "kitty") || strings.Contains(identity, "ghostty") || strings.Contains(identity, "wezterm") {
		return ProtocolKitty
	}
	if strings.Contains(identity, "foot") || strings.Contains(identity, "sixel") || strings.Contains(identity, "windows terminal") {
		return ProtocolSixel
	}
	return ProtocolNone
}

type Desired struct {
	Placement
	version string
}

type activeTerminalImage struct {
	Desired
	imageID     uint32
	placementID uint32
}

type kittyUpload struct {
	imageID   uint32
	fitByRows bool
	// pixelWidth and pixelHeight are the dimensions of the transmitted PNG,
	// which the placement's Clip is expressed against.
	pixelWidth, pixelHeight int
}

const maxSixelCacheEntries = 32

type Prepared struct {
	Data      []byte
	FitByRows bool
	// PixelWidth and PixelHeight describe the fitted slot image before any
	// clip crop, so a partially visible placement can name its source
	// rectangle in the uploaded pixels.
	PixelWidth, PixelHeight int
	Err                     error
}

type terminalImageCacheEntry struct {
	key   string
	image Prepared
}

// terminalImageLRU bounds encoded sixel payloads without throwing away every
// hot entry when one new thumbnail crosses the cache limit.
type terminalImageLRU struct {
	entries map[string]*list.Element
	order   list.List
}

func (c *terminalImageLRU) get(key string) (Prepared, bool) {
	if c.entries == nil {
		return Prepared{}, false
	}
	element, ok := c.entries[key]
	if !ok {
		return Prepared{}, false
	}
	c.order.MoveToFront(element)
	return element.Value.(terminalImageCacheEntry).image, true
}

func (c *terminalImageLRU) put(key string, image Prepared) {
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if element, ok := c.entries[key]; ok {
		element.Value = terminalImageCacheEntry{key: key, image: image}
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(terminalImageCacheEntry{key: key, image: image})
	c.entries[key] = element
	for len(c.entries) > maxSixelCacheEntries {
		oldest := c.order.Back()
		entry := oldest.Value.(terminalImageCacheEntry)
		delete(c.entries, entry.key)
		c.order.Remove(oldest)
	}
}

func (c *terminalImageLRU) clear() {
	c.entries = nil
	c.order.Init()
}

// Manager is the only component allowed to write graphics
// escapes. Terminal writes and cell locks stay on the gotui render goroutine;
// background workers only prepare immutable encoded pixel payloads.
type Manager struct {
	screen   tcell.Screen
	tty      tcell.Tty
	protocol Protocol

	desired      []Desired
	active       []activeTerminalImage
	kittyUploads map[string]kittyUpload

	preparationMu         sync.Mutex
	preparationGeneration uint64
	preparationPending    map[string]uint64
	preparationWanted     map[string]struct{}
	preparationDirty      bool
	preparationClosed     bool
	kittyPrepared         map[string]Prepared
	sixelCache            terminalImageLRU
	runAsync              func(func())
	ready                 chan struct{}
}

func NewManager(screen tcell.Screen) *Manager {
	protocol := DetectProtocol(os.Getenv)
	if protocol == ProtocolNone || screen == nil {
		return nil
	}
	tty, ok := screen.Tty()
	if !ok {
		return nil
	}
	return &Manager{
		screen:   screen,
		tty:      tty,
		protocol: protocol,
		runAsync: func(task func()) { go task() },
		ready:    make(chan struct{}, 1),
	}
}

// NewManagerFor builds a manager for a known protocol and tty, preparing
// pixels synchronously. It is for surfaces that already know what the
// terminal speaks; NewManager detects it from the environment.
func NewManagerFor(screen tcell.Screen, tty tcell.Tty, protocol Protocol) *Manager {
	return &Manager{screen: screen, tty: tty, protocol: protocol, ready: make(chan struct{}, 1)}
}

// ActiveCount reports how many thumbnails are currently placed on screen.
func (m *Manager) ActiveCount() int {
	if m == nil {
		return 0
	}
	return len(m.active)
}

// Prepare releases old locks before gotui paints the next ordinary text frame.
// commit then draws and locks the new placements after that frame is flushed.
func (m *Manager) Prepare(placements []Placement) bool {
	desired := make([]Desired, 0, len(placements))
	geometry := m.geometryVersion()
	for _, placement := range placements {
		if placement.Path == "" && placement.Embedded == "" {
			continue
		}
		// A slot scrolled half out of the pane keeps a negative origin; only
		// the visible sub-rectangle has to be on screen.
		x, y, cols, rows := placement.drawRect()
		if cols <= 0 || rows <= 0 || x < 0 || y < 0 {
			continue
		}
		desired = append(desired, Desired{
			Placement: placement,
			version: fmt.Sprintf("%s%s:thumb:%dx%d:%t",
				placementImageVersion(placement), geometry,
				placement.Cols, placement.Rows, placement.FitByRows),
		})
	}
	if desiredTerminalImagesEqual(m.desired, desired) {
		m.schedulePreparations(desired)
		if !m.takePreparationDirty() {
			return false
		}
		m.releaseActive(false)
		return true
	}
	m.releaseActive(false)
	m.desired = desired
	m.pruneKittyUploads()
	m.advancePreparationGeneration(len(desired) == 0)
	if len(desired) == 0 {
		return true
	}
	m.schedulePreparations(desired)
	// Synchronous preparation is used by small unit-level managers; consume its
	// dirty bit because this changed frame is already about to commit it.
	_ = m.takePreparationDirty()
	return true
}

func (m *Manager) Commit(changed bool) {
	if m == nil || !changed {
		return
	}
	switch m.protocol {
	case ProtocolKitty:
		m.commitKitty()
	case ProtocolSixel:
		m.commitSixel()
	}
}

func (m *Manager) Shutdown() {
	if m == nil {
		return
	}
	m.releaseActive(true)
	m.desired = nil
	m.preparationMu.Lock()
	m.preparationClosed = true
	m.preparationGeneration++
	m.preparationPending = nil
	m.preparationWanted = nil
	m.kittyPrepared = nil
	m.sixelCache.clear()
	m.preparationMu.Unlock()
}

func (m *Manager) ReadyEvents() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.ready
}

// placementImageVersion identifies the pixel source of a placement. Embedded
// assets are fixed at compile time, so their name and length suffice.
func placementImageVersion(placement Placement) string {
	if placement.Embedded != "" {
		return fmt.Sprintf("embedded:%s:%d", placement.Embedded, len(embeddedTerminalImages[placement.Embedded]))
	}
	return images.FileVersion(placement.Path)
}

// loadPlacementImage decodes a placement's pixels from its embedded asset or
// its file on disk.
func loadPlacementImage(placement Placement) (image.Image, error) {
	if placement.Embedded != "" {
		data, ok := embeddedTerminalImages[placement.Embedded]
		if !ok || len(data) == 0 {
			return nil, fmt.Errorf("unknown embedded image %q", placement.Embedded)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		return img, err
	}
	return LoadLocalImage(placement.Path)
}

func desiredTerminalImagesEqual(a, b []Desired) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *Manager) releaseActive(freeImages bool) {
	if len(m.active) == 0 && (!freeImages || len(m.kittyUploads) == 0) {
		return
	}
	if m.protocol == ProtocolKitty {
		for _, active := range m.active {
			_ = writeFull(m.tty, kittyDeletePlacement(active.imageID, active.placementID))
		}
		if freeImages {
			for _, upload := range m.kittyUploads {
				_ = writeFull(m.tty, kittyDeleteImage(upload.imageID))
			}
			m.kittyUploads = nil
		}
	}
	for _, active := range m.active {
		x, y, cols, rows := active.drawRect()
		m.screen.LockRegion(x, y, cols, rows, false)
	}
	m.active = nil
}

func (m *Manager) pruneKittyUploads() {
	if m.protocol != ProtocolKitty || len(m.kittyUploads) == 0 {
		return
	}
	keep := make(map[string]struct{}, len(m.desired))
	for _, desired := range m.desired {
		keep[desired.version] = struct{}{}
	}
	for version, upload := range m.kittyUploads {
		if _, ok := keep[version]; ok {
			continue
		}
		_ = writeFull(m.tty, kittyDeleteImage(upload.imageID))
		delete(m.kittyUploads, version)
	}
}

func (m *Manager) advancePreparationGeneration(clearCaches bool) {
	keepKitty := make(map[string]struct{}, len(m.desired))
	wanted := make(map[string]struct{}, len(m.desired))
	cw, ch := m.CellDimensions()
	for _, desired := range m.desired {
		keepKitty[desired.version] = struct{}{}
		if m.protocol == ProtocolKitty {
			wanted[ProtocolKitty.String()+":"+desired.version] = struct{}{}
		} else if m.protocol == ProtocolSixel {
			key := sixelImageCacheKey(desired, cw, ch)
			wanted[ProtocolSixel.String()+":"+key] = struct{}{}
		}
	}

	m.preparationMu.Lock()
	m.preparationGeneration++
	m.preparationWanted = wanted
	m.preparationDirty = false
	if clearCaches {
		m.kittyPrepared = nil
		m.sixelCache.clear()
	} else {
		for version := range m.kittyPrepared {
			if _, ok := keepKitty[version]; !ok {
				delete(m.kittyPrepared, version)
			}
		}
	}
	m.preparationMu.Unlock()
}

func (m *Manager) takePreparationDirty() bool {
	m.preparationMu.Lock()
	dirty := m.preparationDirty
	m.preparationDirty = false
	m.preparationMu.Unlock()
	return dirty
}

func (m *Manager) schedulePreparations(desired []Desired) {
	if len(desired) == 0 {
		return
	}
	cw, ch := m.CellDimensions()
	for _, item := range desired {
		switch m.protocol {
		case ProtocolKitty:
			if _, uploaded := m.kittyUploads[item.version]; uploaded {
				continue
			}
			m.schedulePreparation(ProtocolKitty, item.version, item, item.Cols*cw, item.Rows*ch)
		case ProtocolSixel:
			cacheKey := sixelImageCacheKey(item, cw, ch)
			m.schedulePreparation(ProtocolSixel, cacheKey, item, item.Cols*cw, item.Rows*ch)
		}
	}
}

func (m *Manager) schedulePreparation(
	protocol Protocol,
	cacheKey string,
	desired Desired,
	maxWidth, maxHeight int,
) {
	pendingKey := protocol.String() + ":" + cacheKey
	m.preparationMu.Lock()
	if m.preparationClosed {
		m.preparationMu.Unlock()
		return
	}
	var cached bool
	if protocol == ProtocolKitty {
		_, cached = m.kittyPrepared[cacheKey]
	} else {
		_, cached = m.sixelCache.get(cacheKey)
	}
	if cached {
		m.preparationMu.Unlock()
		return
	}
	generation := m.preparationGeneration
	if _, pending := m.preparationPending[pendingKey]; pending {
		m.preparationMu.Unlock()
		return
	}
	if m.preparationPending == nil {
		m.preparationPending = make(map[string]uint64)
	}
	m.preparationPending[pendingKey] = generation
	m.preparationMu.Unlock()

	task := func() {
		var prepared Prepared
		switch protocol {
		case ProtocolKitty:
			prepared = PrepareKitty(desired, maxWidth, maxHeight)
		case ProtocolSixel:
			prepared = PrepareSixel(desired, maxWidth, maxHeight)
		}
		m.finishPreparation(protocol, cacheKey, pendingKey, generation, prepared)
	}
	if m.runAsync == nil {
		task()
	} else {
		m.runAsync(task)
	}
}

func (m *Manager) finishPreparation(
	protocol Protocol,
	cacheKey, pendingKey string,
	generation uint64,
	prepared Prepared,
) {
	m.preparationMu.Lock()
	if m.preparationPending[pendingKey] == generation {
		delete(m.preparationPending, pendingKey)
	}
	_, stillWanted := m.preparationWanted[pendingKey]
	if m.preparationClosed || m.preparationGeneration != generation && !stillWanted {
		m.preparationMu.Unlock()
		return
	}
	if protocol == ProtocolKitty {
		if m.kittyPrepared == nil {
			m.kittyPrepared = make(map[string]Prepared)
		}
		m.kittyPrepared[cacheKey] = prepared
	} else {
		m.sixelCache.put(cacheKey, prepared)
	}
	m.preparationDirty = true
	ready := m.ready
	m.preparationMu.Unlock()

	if ready != nil {
		select {
		case ready <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) preparedKittyImage(version string) (Prepared, bool) {
	m.preparationMu.Lock()
	prepared, ok := m.kittyPrepared[version]
	m.preparationMu.Unlock()
	return prepared, ok
}

func (m *Manager) dropPreparedKittyImage(version string) {
	m.preparationMu.Lock()
	delete(m.kittyPrepared, version)
	m.preparationMu.Unlock()
}

func (m *Manager) preparedSixelImage(key string) (Prepared, bool) {
	m.preparationMu.Lock()
	prepared, ok := m.sixelCache.get(key)
	m.preparationMu.Unlock()
	return prepared, ok
}

func PrepareKitty(desired Desired, maxWidth, maxHeight int) Prepared {
	img, err := loadPlacementImage(desired.Placement)
	if err != nil {
		return Prepared{Err: err}
	}
	bounds := img.Bounds()
	prepared := Prepared{
		FitByRows: imageFitsByRows(bounds.Dx(), bounds.Dy(), maxWidth, maxHeight),
	}
	img = images.Fit(img, maxWidth, maxHeight)
	fitted := img.Bounds()
	prepared.PixelWidth, prepared.PixelHeight = fitted.Dx(), fitted.Dy()
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, img); err != nil {
		prepared.Err = err
		return prepared
	}
	prepared.Data = pngData.Bytes()
	return prepared
}

func PrepareSixel(desired Desired, maxWidth, maxHeight int) Prepared {
	img, err := loadPlacementImage(desired.Placement)
	if err != nil {
		return Prepared{Err: err}
	}
	img = images.Fit(img, maxWidth, maxHeight)
	fitted := img.Bounds()
	prepared := Prepared{PixelWidth: fitted.Dx(), PixelHeight: fitted.Dy()}
	// Sixel has no source rectangle, so a partially visible placement encodes
	// only the visible slice of the fitted image.
	source := clipSourceRect(prepared.PixelWidth, prepared.PixelHeight, desired.Cols, desired.Rows, desired.Clip)
	if source.Empty() {
		return Prepared{Err: fmt.Errorf("empty sixel crop for %s", desired.Key)}
	}
	img = cropToClip(img, source)
	var sixelData bytes.Buffer
	encoder := sixel.NewEncoder(&sixelData)
	encoder.Colors = 256
	encoder.Transparent = true
	if err := encoder.Encode(img); err != nil {
		return Prepared{Err: err}
	}
	prepared.Data = sixelData.Bytes()
	return prepared
}

// cropToClip cuts rect out of src into a fresh image. The zero rectangle
// leaves src untouched.
func cropToClip(src image.Image, rect image.Rectangle) image.Image {
	bounds := src.Bounds()
	rect = rect.Intersect(image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Max.Y))
	if rect.Empty() || rect == bounds {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	return dst
}

// sixelImageCacheKey identifies an encoded sixel payload. Unlike the kitty
// upload key it includes the clip: the bytes differ for every visible slice.
func sixelImageCacheKey(desired Desired, cellWidth, cellHeight int) string {
	key := fmt.Sprintf("%s:%dx%d", desired.version, desired.Cols*cellWidth, desired.Rows*cellHeight)
	if desired.Clip.Cols > 0 && desired.Clip.Rows > 0 {
		key += fmt.Sprintf(":clip,%d,%d,%d,%d", desired.Clip.X, desired.Clip.Y, desired.Clip.Cols, desired.Clip.Rows)
	}
	return key
}

func (m *Manager) commitKitty() {
	usedIDs := make(map[uint32]string)
	for version, upload := range m.kittyUploads {
		usedIDs[upload.imageID] = "image:" + version
	}
	usedPlacementIDs := make(map[uint32]string, len(m.desired))

	for _, desired := range m.desired {
		upload, ok := m.kittyUploads[desired.version]
		if !ok {
			prepared, ready := m.preparedKittyImage(desired.version)
			if !ready || prepared.Err != nil || len(prepared.Data) == 0 {
				continue
			}
			upload.fitByRows = prepared.FitByRows
			upload.pixelWidth, upload.pixelHeight = prepared.PixelWidth, prepared.PixelHeight
			upload.imageID = uniqueTerminalImageID("image:"+desired.version, usedIDs)
			if err := writeFull(m.tty, kittyTransmitPNG(upload.imageID, prepared.Data)); err != nil {
				continue
			}
			if m.kittyUploads == nil {
				m.kittyUploads = make(map[string]kittyUpload)
			}
			m.kittyUploads[desired.version] = upload
			m.dropPreparedKittyImage(desired.version)
		}

		placement := desired.Placement
		placement.FitByRows = upload.fitByRows
		placementKey := "placement:" + desired.Key
		placementID := uniqueTerminalImageID(placementKey, usedPlacementIDs)
		command := kittyPlaceImage(upload.imageID, placementID, placement, upload.pixelWidth, upload.pixelHeight)
		if len(command) == 0 {
			continue
		}
		if err := writeFull(m.tty, command); err != nil {
			continue
		}
		x, y, cols, rows := placement.drawRect()
		m.screen.LockRegion(x, y, cols, rows, true)
		m.active = append(m.active, activeTerminalImage{
			Desired:     desired,
			imageID:     upload.imageID,
			placementID: placementID,
		})
	}
}

func (m *Manager) commitSixel() {
	cw, ch := m.CellDimensions()
	for _, desired := range m.desired {
		cacheKey := sixelImageCacheKey(desired, cw, ch)
		prepared, ready := m.preparedSixelImage(cacheKey)
		if !ready || prepared.Err != nil || len(prepared.Data) == 0 {
			continue
		}
		x, y, cols, rows := desired.drawRect()
		if err := writeFull(m.tty, terminalBytesAt(x, y, prepared.Data)); err != nil {
			continue
		}
		m.screen.LockRegion(x, y, cols, rows, true)
		m.active = append(m.active, activeTerminalImage{Desired: desired})
	}
}

func (m *Manager) CellDimensions() (int, int) {
	const defaultCellWidth, defaultCellHeight = 10, 20
	window, err := m.tty.WindowSize()
	if err != nil {
		return defaultCellWidth, defaultCellHeight
	}
	cw, ch := window.CellDimensions()
	if cw <= 0 {
		cw = defaultCellWidth
	}
	if ch <= 0 {
		ch = defaultCellHeight
	}
	return cw, ch
}

func (m *Manager) geometryVersion() string {
	window, err := m.tty.WindowSize()
	if err != nil {
		return ":geometry:unknown"
	}
	return fmt.Sprintf(":geometry:%dx%d:%dx%d", window.Width, window.Height, window.PixelWidth, window.PixelHeight)
}

// LoadLocalImage decodes a raster file within the source bounds.
func LoadLocalImage(path string) (image.Image, error) {
	img, _, err := images.DecodeBoundedFile(path, images.MaxSourceBytes)
	return img, err
}

func imageFitsByRows(width, height, maxWidth, maxHeight int) bool {
	return width > 0 && height > 0 && maxWidth > 0 && maxHeight > 0 &&
		int64(maxHeight)*int64(width) < int64(maxWidth)*int64(height)
}

func stableTerminalImageID(key string) uint32 {
	hash := fnv.New32a()
	_, _ = io.WriteString(hash, "polly:"+key)
	id := hash.Sum32()
	if id == 0 {
		return 1
	}
	return id
}

func uniqueTerminalImageID(key string, used map[uint32]string) uint32 {
	id := stableTerminalImageID(key)
	for {
		if previous, exists := used[id]; !exists || previous == key {
			used[id] = key
			return id
		}
		id++
		if id == 0 {
			id = 1
		}
	}
}

func kittyTransmitPNG(imageID uint32, pngData []byte) []byte {
	if len(pngData) == 0 {
		return nil
	}
	return KittyChunked(fmt.Sprintf("a=t,f=100,t=d,i=%d,q=2", imageID), pngData)
}

// KittyChunked emits pngData as base64 in the 4096-byte chunks the Kitty
// graphics protocol requires. first is the opening command's control data
// (without its m= flag); continuation commands carry only q=2 and the flag.
func KittyChunked(first string, pngData []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(pngData)
	var out bytes.Buffer
	for offset := 0; offset < len(encoded); offset += 4096 {
		end := min(offset+4096, len(encoded))
		more := 0
		if end < len(encoded) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(&out, "\x1b_G%s,m=%d;", first, more)
		} else {
			fmt.Fprintf(&out, "\x1b_Gq=2,m=%d;", more)
		}
		out.WriteString(encoded[offset:end])
		out.WriteString("\x1b\\")
	}
	return out.Bytes()
}

// KittySizeSpec is the placement size control: columns normally, rows when
// the image is fitted by height.
func KittySizeSpec(cols, rows int, fitByRows bool) string {
	if fitByRows {
		return fmt.Sprintf("r=%d", rows)
	}
	return fmt.Sprintf("c=%d", cols)
}

// kittyPlaceImage places an already transmitted image. pixelWidth and
// pixelHeight are the dimensions of that image: a clipped placement names the
// visible slice as a source rectangle and pins both destination dimensions so
// the slice lands exactly on the cells that are on screen.
func kittyPlaceImage(imageID, placementID uint32, placement Placement, pixelWidth, pixelHeight int) []byte {
	size := KittySizeSpec(placement.Cols, placement.Rows, placement.FitByRows)
	if placement.Clip.Cols > 0 && placement.Clip.Rows > 0 {
		// Without the transmitted pixel size the visible slice cannot be named;
		// drawing the whole image would land it in the wrong cells.
		source := clipSourceRect(pixelWidth, pixelHeight, placement.Cols, placement.Rows, placement.Clip)
		if source.Empty() {
			return nil
		}
		size = fmt.Sprintf(
			"x=%d,y=%d,w=%d,h=%d,c=%d,r=%d",
			source.Min.X, source.Min.Y, source.Dx(), source.Dy(),
			placement.Clip.Cols, placement.Clip.Rows,
		)
	}
	x, y, _, _ := placement.drawRect()
	command := fmt.Sprintf("\x1b_Ga=p,i=%d,p=%d,%s,C=1,q=2;\x1b\\", imageID, placementID, size)
	return terminalBytesAt(x, y, []byte(command))
}

func kittyDeletePlacement(imageID, placementID uint32) []byte {
	return []byte(fmt.Sprintf("\x1b_Ga=d,d=i,i=%d,p=%d,q=2;\x1b\\", imageID, placementID))
}

func kittyDeleteImage(imageID uint32) []byte {
	return []byte(fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2;\x1b\\", imageID))
}

func terminalBytesAt(x, y int, payload []byte) []byte {
	var out bytes.Buffer
	out.WriteString("\x1b7")
	fmt.Fprintf(&out, "\x1b[%d;%dH", y+1, x+1)
	out.Write(payload)
	out.WriteString("\x1b8")
	return out.Bytes()
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
