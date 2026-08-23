package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"strings"

	tcell "github.com/gdamore/tcell/v3"
	"github.com/mattn/go-sixel"
	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
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
	switch strings.ToLower(strings.TrimSpace(getenv("POLLYTOOL_IMAGE_PROTOCOL"))) {
	case "kitty":
		return terminalImageKitty
	case "sixel":
		return terminalImageSixel
	case "none", "off", "0":
		return terminalImageNone
	}
	// Multiplexer passthrough and placement ownership need a separate design.
	// Keep the first slice honest and fall back to captions inside them.
	if getenv("TMUX") != "" || getenv("ZELLIJ") != "" || getenv("ZELLIJ_SESSION_NAME") != "" {
		return terminalImageNone
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

// terminalImageManager is the only component allowed to write graphics
// escapes. It runs on the gotui event/render goroutine, locks image cells in
// tcell, and redraws only when the visible placement set changes.
type terminalImageManager struct {
	screen   tcell.Screen
	tty      tcell.Tty
	protocol terminalImageProtocol

	desired      []desiredTerminalImage
	active       []activeTerminalImage
	kittyUploads map[string]uint32
	sixelCache   map[string][]byte
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
	return &terminalImageManager{screen: screen, tty: tty, protocol: protocol}
}

// prepare releases old locks before gotui paints the next ordinary text frame.
// commit then draws and locks the new placements after that frame is flushed.
func (m *terminalImageManager) prepare(placements []terminalImagePlacement) bool {
	desired := make([]desiredTerminalImage, 0, len(placements))
	geometry := m.geometryVersion()
	for _, placement := range placements {
		if placement.Path == "" || placement.Cols <= 0 || placement.Rows <= 0 || placement.X < 0 || placement.Y < 0 {
			continue
		}
		desired = append(desired, desiredTerminalImage{
			terminalImagePlacement: placement,
			version: fmt.Sprintf("%s%s:thumb:%dx%d:%t",
				localImageVersion(placement.Path), geometry,
				placement.Cols, placement.Rows, placement.FitByRows),
		})
	}
	if desiredTerminalImagesEqual(m.desired, desired) {
		return false
	}
	m.releaseActive(false)
	m.desired = desired
	m.pruneKittyUploads()
	if len(desired) == 0 {
		m.sixelCache = nil
	}
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
}

func localImageVersion(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path + ":missing"
	}
	return fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano())
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
			for _, imageID := range m.kittyUploads {
				_ = writeFull(m.tty, kittyDeleteImage(imageID))
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
	for version, imageID := range m.kittyUploads {
		if _, ok := keep[version]; ok {
			continue
		}
		_ = writeFull(m.tty, kittyDeleteImage(imageID))
		delete(m.kittyUploads, version)
	}
}

func (m *terminalImageManager) commitKitty() {
	usedIDs := make(map[uint32]string)
	for version, imageID := range m.kittyUploads {
		usedIDs[imageID] = "image:" + version
	}
	cw, ch := m.cellDimensions()

	for _, desired := range m.desired {
		imageID, ok := m.kittyUploads[desired.version]
		if !ok {
			img, err := loadLocalImage(desired.Path)
			if err != nil {
				continue
			}
			img = fitImage(img, desired.Cols*cw, desired.Rows*ch)
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, img); err != nil {
				continue
			}
			imageID = uniqueTerminalImageID("image:"+desired.version, usedIDs)
			if err := writeFull(m.tty, kittyTransmitPNG(imageID, pngData.Bytes())); err != nil {
				continue
			}
			if m.kittyUploads == nil {
				m.kittyUploads = make(map[string]uint32)
			}
			m.kittyUploads[desired.version] = imageID
		}

		placementID := stableTerminalImageID("placement:" + desired.Key)
		if err := writeFull(m.tty, kittyPlaceImage(imageID, placementID, desired.terminalImagePlacement)); err != nil {
			continue
		}
		m.screen.LockRegion(desired.X, desired.Y, desired.Cols, desired.Rows, true)
		m.active = append(m.active, activeTerminalImage{
			desiredTerminalImage: desired,
			imageID:              imageID,
			placementID:          placementID,
		})
	}
}

func (m *terminalImageManager) commitSixel() {
	cw, ch := m.cellDimensions()
	if m.sixelCache == nil {
		m.sixelCache = make(map[string][]byte)
	}
	if len(m.sixelCache) > 32 {
		m.sixelCache = make(map[string][]byte)
	}
	for _, desired := range m.desired {
		cacheKey := fmt.Sprintf("%s:%dx%d", desired.version, desired.Cols*cw, desired.Rows*ch)
		data, ok := m.sixelCache[cacheKey]
		if !ok {
			img, err := loadLocalImage(desired.Path)
			if err != nil {
				continue
			}
			img = fitImage(img, desired.Cols*cw, desired.Rows*ch)
			var sixelData bytes.Buffer
			encoder := sixel.NewEncoder(&sixelData)
			encoder.Colors = 256
			encoder.Transparent = true
			if err := encoder.Encode(img); err != nil {
				continue
			}
			data = sixelData.Bytes()
			m.sixelCache[cacheKey] = data
		}
		if err := writeFull(m.tty, terminalBytesAt(desired.X, desired.Y, data)); err != nil {
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxLocalImageBytes {
		return nil, fmt.Errorf("image size is outside the supported range")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxLocalImagePixels {
		return nil, fmt.Errorf("image dimensions are outside the supported range")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

func fitImage(src image.Image, maxWidth, maxHeight int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 {
		return src
	}
	targetWidth, targetHeight := fitPixelDimensions(width, height, maxWidth, maxHeight)
	if targetWidth == width && targetHeight == height && bounds.Min.X == 0 && bounds.Min.Y == 0 {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Over, nil)
	return dst
}

func fitPixelDimensions(width, height, maxWidth, maxHeight int) (int, int) {
	if width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 {
		return 0, 0
	}
	scale := math.Min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
	return max(1, int(math.Round(float64(width)*scale))), max(1, int(math.Round(float64(height)*scale)))
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
	encoded := base64.StdEncoding.EncodeToString(pngData)
	var out bytes.Buffer
	for offset := 0; offset < len(encoded); offset += 4096 {
		end := min(offset+4096, len(encoded))
		more := 0
		if end < len(encoded) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(&out, "\x1b_Ga=t,f=100,t=d,i=%d,q=2,m=%d;", imageID, more)
		} else {
			fmt.Fprintf(&out, "\x1b_Gq=2,m=%d;", more)
		}
		out.WriteString(encoded[offset:end])
		out.WriteString("\x1b\\")
	}
	return out.Bytes()
}

func kittyPlaceImage(imageID, placementID uint32, placement terminalImagePlacement) []byte {
	size := fmt.Sprintf("c=%d", placement.Cols)
	if placement.FitByRows {
		size = fmt.Sprintf("r=%d", placement.Rows)
	}
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
