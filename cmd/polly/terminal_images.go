package main

import (
	"bytes"
	"container/list"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image"
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

type terminalImageProtocol uint8

const (
	terminalImageNone terminalImageProtocol = iota
	terminalImageKitty
	terminalImageSixel
)

func (p terminalImageProtocol) String() string {
	switch p {
	case terminalImageKitty:
		return "kitty"
	case terminalImageSixel:
		return "sixel"
	default:
		return "none"
	}
}

// detectTerminalImageProtocol is conservative: emitting an unsupported image
// protocol can put escape payloads into the terminal. POLLYTOOL_IMAGE_PROTOCOL
// is an escape hatch for compatible terminals that do not identify themselves.
func detectTerminalImageProtocol(getenv func(string) string) terminalImageProtocol {
	override := strings.ToLower(strings.TrimSpace(getenv("POLLYTOOL_IMAGE_PROTOCOL")))
	switch override {
	case "none", "off", "0":
		return terminalImageNone
	}
	// Multiplexer passthrough and placement ownership need a separate design.
	// Keep the first slice honest and fall back to captions inside them even
	// when a native protocol was explicitly requested.
	if getenv("TMUX") != "" || getenv("ZELLIJ") != "" || getenv("ZELLIJ_SESSION_NAME") != "" {
		return terminalImageNone
	}
	switch override {
	case "kitty":
		return terminalImageKitty
	case "sixel":
		return terminalImageSixel
	}

	if getenv("WT_SESSION") != "" {
		return terminalImageSixel
	}
	if getenv("KITTY_WINDOW_ID") != "" || getenv("WEZTERM_PANE") != "" || getenv("GHOSTTY_RESOURCES_DIR") != "" {
		return terminalImageKitty
	}
	identity := strings.ToLower(strings.Join([]string{
		getenv("TERM"),
		getenv("TERM_PROGRAM"),
		getenv("LC_TERMINAL"),
	}, " "))
	if strings.Contains(identity, "kitty") || strings.Contains(identity, "ghostty") || strings.Contains(identity, "wezterm") {
		return terminalImageKitty
	}
	if strings.Contains(identity, "foot") || strings.Contains(identity, "sixel") || strings.Contains(identity, "windows terminal") {
		return terminalImageSixel
	}
	return terminalImageNone
}

type desiredTerminalImage struct {
	terminalImagePlacement
	version string
}

type activeTerminalImage struct {
	desiredTerminalImage
	imageID     uint32
	placementID uint32
}

type kittyUpload struct {
	imageID   uint32
	fitByRows bool
}

const maxSixelCacheEntries = 32

type preparedTerminalImage struct {
	data      []byte
	fitByRows bool
	err       error
}

type terminalImageCacheEntry struct {
	key   string
	image preparedTerminalImage
}

// terminalImageLRU bounds encoded sixel payloads without throwing away every
// hot entry when one new thumbnail crosses the cache limit.
type terminalImageLRU struct {
	entries map[string]*list.Element
	order   list.List
}

func (c *terminalImageLRU) get(key string) (preparedTerminalImage, bool) {
	if c.entries == nil {
		return preparedTerminalImage{}, false
	}
	element, ok := c.entries[key]
	if !ok {
		return preparedTerminalImage{}, false
	}
	c.order.MoveToFront(element)
	return element.Value.(terminalImageCacheEntry).image, true
}

func (c *terminalImageLRU) put(key string, image preparedTerminalImage) {
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

// terminalImageManager is the only component allowed to write graphics
// escapes. Terminal writes and cell locks stay on the gotui render goroutine;
// background workers only prepare immutable encoded pixel payloads.
type terminalImageManager struct {
	screen   tcell.Screen
	tty      tcell.Tty
	protocol terminalImageProtocol

	desired      []desiredTerminalImage
	active       []activeTerminalImage
	kittyUploads map[string]kittyUpload

	preparationMu         sync.Mutex
	preparationGeneration uint64
	preparationPending    map[string]uint64
	preparationWanted     map[string]struct{}
	preparationDirty      bool
	preparationClosed     bool
	kittyPrepared         map[string]preparedTerminalImage
	sixelCache            terminalImageLRU
	runAsync              func(func())
	ready                 chan struct{}
}

func newTerminalImageManager(screen tcell.Screen) *terminalImageManager {
	protocol := detectTerminalImageProtocol(os.Getenv)
	if protocol == terminalImageNone || screen == nil {
		return nil
	}
	tty, ok := screen.Tty()
	if !ok {
		return nil
	}
	return &terminalImageManager{
		screen:   screen,
		tty:      tty,
		protocol: protocol,
		runAsync: func(task func()) { go task() },
		ready:    make(chan struct{}, 1),
	}
}

// prepare releases old locks before gotui paints the next ordinary text frame.
// commit then draws and locks the new placements after that frame is flushed.
func (m *terminalImageManager) prepare(placements []terminalImagePlacement) bool {
	desired := make([]desiredTerminalImage, 0, len(placements))
	geometry := m.geometryVersion()
	for _, placement := range placements {
		if (placement.Path == "" && placement.Embedded == "") ||
			placement.Cols <= 0 || placement.Rows <= 0 || placement.X < 0 || placement.Y < 0 {
			continue
		}
		desired = append(desired, desiredTerminalImage{
			terminalImagePlacement: placement,
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

func (m *terminalImageManager) commit(changed bool) {
	if m == nil || !changed {
		return
	}
	switch m.protocol {
	case terminalImageKitty:
		m.commitKitty()
	case terminalImageSixel:
		m.commitSixel()
	}
}

func (m *terminalImageManager) shutdown() {
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

func (m *terminalImageManager) readyEvents() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.ready
}

func localImageVersion(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path + ":missing"
	}
	return fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano())
}

// placementImageVersion identifies the pixel source of a placement. Embedded
// assets are fixed at compile time, so their name and length suffice.
func placementImageVersion(placement terminalImagePlacement) string {
	if placement.Embedded != "" {
		return fmt.Sprintf("embedded:%s:%d", placement.Embedded, len(embeddedTerminalImages[placement.Embedded]))
	}
	return localImageVersion(placement.Path)
}

// loadPlacementImage decodes a placement's pixels from its embedded asset or
// its file on disk.
func loadPlacementImage(placement terminalImagePlacement) (image.Image, error) {
	if placement.Embedded != "" {
		data, ok := embeddedTerminalImages[placement.Embedded]
		if !ok || len(data) == 0 {
			return nil, fmt.Errorf("unknown embedded image %q", placement.Embedded)
		}
		img, _, err := image.Decode(bytes.NewReader(data))
		return img, err
	}
	return loadLocalImage(placement.Path)
}

func desiredTerminalImagesEqual(a, b []desiredTerminalImage) bool {
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

func (m *terminalImageManager) releaseActive(freeImages bool) {
	if len(m.active) == 0 && (!freeImages || len(m.kittyUploads) == 0) {
		return
	}
	if m.protocol == terminalImageKitty {
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
		m.screen.LockRegion(active.X, active.Y, active.Cols, active.Rows, false)
	}
	m.active = nil
}

func (m *terminalImageManager) pruneKittyUploads() {
	if m.protocol != terminalImageKitty || len(m.kittyUploads) == 0 {
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

func (m *terminalImageManager) advancePreparationGeneration(clearCaches bool) {
	keepKitty := make(map[string]struct{}, len(m.desired))
	wanted := make(map[string]struct{}, len(m.desired))
	cw, ch := m.cellDimensions()
	for _, desired := range m.desired {
		keepKitty[desired.version] = struct{}{}
		if m.protocol == terminalImageKitty {
			wanted[terminalImageKitty.String()+":"+desired.version] = struct{}{}
		} else if m.protocol == terminalImageSixel {
			key := sixelImageCacheKey(desired, cw, ch)
			wanted[terminalImageSixel.String()+":"+key] = struct{}{}
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

func (m *terminalImageManager) takePreparationDirty() bool {
	m.preparationMu.Lock()
	dirty := m.preparationDirty
	m.preparationDirty = false
	m.preparationMu.Unlock()
	return dirty
}

func (m *terminalImageManager) schedulePreparations(desired []desiredTerminalImage) {
	if len(desired) == 0 {
		return
	}
	cw, ch := m.cellDimensions()
	for _, item := range desired {
		switch m.protocol {
		case terminalImageKitty:
			if _, uploaded := m.kittyUploads[item.version]; uploaded {
				continue
			}
			m.schedulePreparation(terminalImageKitty, item.version, item, item.Cols*cw, item.Rows*ch)
		case terminalImageSixel:
			cacheKey := sixelImageCacheKey(item, cw, ch)
			m.schedulePreparation(terminalImageSixel, cacheKey, item, item.Cols*cw, item.Rows*ch)
		}
	}
}

func (m *terminalImageManager) schedulePreparation(
	protocol terminalImageProtocol,
	cacheKey string,
	desired desiredTerminalImage,
	maxWidth, maxHeight int,
) {
	pendingKey := protocol.String() + ":" + cacheKey
	m.preparationMu.Lock()
	if m.preparationClosed {
		m.preparationMu.Unlock()
		return
	}
	var cached bool
	if protocol == terminalImageKitty {
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
		var prepared preparedTerminalImage
		switch protocol {
		case terminalImageKitty:
			prepared = prepareKittyImage(desired, maxWidth, maxHeight)
		case terminalImageSixel:
			prepared = prepareSixelImage(desired, maxWidth, maxHeight)
		}
		m.finishPreparation(protocol, cacheKey, pendingKey, generation, prepared)
	}
	if m.runAsync == nil {
		task()
	} else {
		m.runAsync(task)
	}
}

func (m *terminalImageManager) finishPreparation(
	protocol terminalImageProtocol,
	cacheKey, pendingKey string,
	generation uint64,
	prepared preparedTerminalImage,
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
	if protocol == terminalImageKitty {
		if m.kittyPrepared == nil {
			m.kittyPrepared = make(map[string]preparedTerminalImage)
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

func (m *terminalImageManager) preparedKittyImage(version string) (preparedTerminalImage, bool) {
	m.preparationMu.Lock()
	prepared, ok := m.kittyPrepared[version]
	m.preparationMu.Unlock()
	return prepared, ok
}

func (m *terminalImageManager) dropPreparedKittyImage(version string) {
	m.preparationMu.Lock()
	delete(m.kittyPrepared, version)
	m.preparationMu.Unlock()
}

func (m *terminalImageManager) preparedSixelImage(key string) (preparedTerminalImage, bool) {
	m.preparationMu.Lock()
	prepared, ok := m.sixelCache.get(key)
	m.preparationMu.Unlock()
	return prepared, ok
}

func prepareKittyImage(desired desiredTerminalImage, maxWidth, maxHeight int) preparedTerminalImage {
	img, err := loadPlacementImage(desired.terminalImagePlacement)
	if err != nil {
		return preparedTerminalImage{err: err}
	}
	bounds := img.Bounds()
	prepared := preparedTerminalImage{
		fitByRows: imageFitsByRows(bounds.Dx(), bounds.Dy(), maxWidth, maxHeight),
	}
	img = images.Fit(img, maxWidth, maxHeight)
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, img); err != nil {
		prepared.err = err
		return prepared
	}
	prepared.data = pngData.Bytes()
	return prepared
}

func prepareSixelImage(desired desiredTerminalImage, maxWidth, maxHeight int) preparedTerminalImage {
	img, err := loadPlacementImage(desired.terminalImagePlacement)
	if err != nil {
		return preparedTerminalImage{err: err}
	}
	img = images.Fit(img, maxWidth, maxHeight)
	var sixelData bytes.Buffer
	encoder := sixel.NewEncoder(&sixelData)
	encoder.Colors = 256
	encoder.Transparent = true
	if err := encoder.Encode(img); err != nil {
		return preparedTerminalImage{err: err}
	}
	return preparedTerminalImage{data: sixelData.Bytes()}
}

func sixelImageCacheKey(desired desiredTerminalImage, cellWidth, cellHeight int) string {
	return fmt.Sprintf("%s:%dx%d", desired.version, desired.Cols*cellWidth, desired.Rows*cellHeight)
}

func (m *terminalImageManager) commitKitty() {
	usedIDs := make(map[uint32]string)
	for version, upload := range m.kittyUploads {
		usedIDs[upload.imageID] = "image:" + version
	}
	usedPlacementIDs := make(map[uint32]string, len(m.desired))

	for _, desired := range m.desired {
		upload, ok := m.kittyUploads[desired.version]
		if !ok {
			prepared, ready := m.preparedKittyImage(desired.version)
			if !ready || prepared.err != nil || len(prepared.data) == 0 {
				continue
			}
			upload.fitByRows = prepared.fitByRows
			upload.imageID = uniqueTerminalImageID("image:"+desired.version, usedIDs)
			if err := writeFull(m.tty, kittyTransmitPNG(upload.imageID, prepared.data)); err != nil {
				continue
			}
			if m.kittyUploads == nil {
				m.kittyUploads = make(map[string]kittyUpload)
			}
			m.kittyUploads[desired.version] = upload
			m.dropPreparedKittyImage(desired.version)
		}

		placement := desired.terminalImagePlacement
		placement.FitByRows = upload.fitByRows
		placementKey := "placement:" + desired.Key
		placementID := uniqueTerminalImageID(placementKey, usedPlacementIDs)
		if err := writeFull(m.tty, kittyPlaceImage(upload.imageID, placementID, placement)); err != nil {
			continue
		}
		m.screen.LockRegion(desired.X, desired.Y, desired.Cols, desired.Rows, true)
		m.active = append(m.active, activeTerminalImage{
			desiredTerminalImage: desired,
			imageID:              upload.imageID,
			placementID:          placementID,
		})
	}
}

func (m *terminalImageManager) commitSixel() {
	cw, ch := m.cellDimensions()
	for _, desired := range m.desired {
		cacheKey := sixelImageCacheKey(desired, cw, ch)
		prepared, ready := m.preparedSixelImage(cacheKey)
		if !ready || prepared.err != nil || len(prepared.data) == 0 {
			continue
		}
		if err := writeFull(m.tty, terminalBytesAt(desired.X, desired.Y, prepared.data)); err != nil {
			continue
		}
		m.screen.LockRegion(desired.X, desired.Y, desired.Cols, desired.Rows, true)
		m.active = append(m.active, activeTerminalImage{desiredTerminalImage: desired})
	}
}

func (m *terminalImageManager) cellDimensions() (int, int) {
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

func (m *terminalImageManager) geometryVersion() string {
	window, err := m.tty.WindowSize()
	if err != nil {
		return ":geometry:unknown"
	}
	return fmt.Sprintf(":geometry:%dx%d:%dx%d", window.Width, window.Height, window.PixelWidth, window.PixelHeight)
}

func loadLocalImage(path string) (image.Image, error) {
	img, _, err := images.DecodeBoundedFile(path, maxLocalImageBytes)
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
	return kittyChunked(fmt.Sprintf("a=t,f=100,t=d,i=%d,q=2", imageID), pngData)
}

// kittyChunked emits pngData as base64 in the 4096-byte chunks the Kitty
// graphics protocol requires. first is the opening command's control data
// (without its m= flag); continuation commands carry only q=2 and the flag.
func kittyChunked(first string, pngData []byte) []byte {
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

// kittySizeSpec is the placement size control: columns normally, rows when
// the image is fitted by height.
func kittySizeSpec(cols, rows int, fitByRows bool) string {
	if fitByRows {
		return fmt.Sprintf("r=%d", rows)
	}
	return fmt.Sprintf("c=%d", cols)
}

func kittyPlaceImage(imageID, placementID uint32, placement terminalImagePlacement) []byte {
	size := kittySizeSpec(placement.Cols, placement.Rows, placement.FitByRows)
	command := fmt.Sprintf("\x1b_Ga=p,i=%d,p=%d,%s,C=1,q=2;\x1b\\", imageID, placementID, size)
	return terminalBytesAt(placement.X, placement.Y, []byte(command))
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
