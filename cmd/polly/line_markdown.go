package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	tcell "github.com/gdamore/tcell/v3"
	tcellcolor "github.com/gdamore/tcell/v3/color"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

const (
	defaultLineCellWidth  = 10
	defaultLineCellHeight = 20
)

// renderLineMarkdown renders one settled assistant segment. It deliberately
// shares the TUI's Markdown walker and semantic palette so the two rich
// frontends do not drift while keeping the original Markdown source outside
// this display-only boundary.
func renderLineMarkdown(src, baseDir string, capabilities outputCapabilities) []byte {
	rendered, images, _ := markdown.RenderWithLocalImages(src, baseDir, false)
	if rendered == "" {
		return nil
	}

	lines := strings.Split(rendered, "\n")
	displayedImages := make(map[int]bool, len(images))
	var out bytes.Buffer
	for lineIndex, line := range lines {
		cells := style.ParseCells(line, ui.StyleClear)
		markerIndex, markerCell := lineImageMarker(cells, len(images))
		if markerIndex >= 0 {
			if !displayedImages[markerIndex] {
				displayedImages[markerIndex] = true
				prefix := cells[:markerCell]
				prefixWidth := styledCellsWidth(prefix)
				if payload := lineImagePayload(images[markerIndex], capabilities, prefixWidth); len(payload) > 0 {
					appendANSIStyledCells(&out, prefix, capabilities.lineColors())
					out.Write(payload)
				}
			}
			continue
		}

		appendANSIStyledCells(&out, cells, capabilities.lineColors())
		if lineIndex < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func lineImageMarker(cells []ui.Cell, imageCount int) (imageIndex, cellIndex int) {
	for i, cell := range cells {
		index, ok := style.ImageMarkerIndex(cell.Rune)
		if ok && index < imageCount {
			return index, i
		}
	}
	return -1, -1
}

func styledCellsWidth(cells []ui.Cell) int {
	width := 0
	for _, cell := range cells {
		width += max(0, rw.RuneWidth(cell.Rune))
	}
	return width
}

// appendANSIStyledCells writes cells to out. When the surface cannot show rich
// output the cells are written as plain text, so every caller gets the same
// renderer-owned escape boundary: model text must not be able to inject OSC/CSI
// commands or cursor controls.
func appendANSIStyledCells(out *bytes.Buffer, cells []ui.Cell, colors lineColorCapabilities) {
	if !colors.enabled {
		for _, cell := range cells {
			if unicode.IsControl(cell.Rune) && cell.Rune != '\t' {
				continue
			}
			out.WriteRune(cell.Rune)
		}
		return
	}
	current := ui.StyleClear
	for _, cell := range cells {
		if cell.Style != current {
			out.WriteString(ansiStyleSequence(cell.Style, colors))
			current = cell.Style
		}
		if unicode.IsControl(cell.Rune) && cell.Rune != '\t' {
			continue
		}
		out.WriteRune(cell.Rune)
	}
	if current != ui.StyleClear {
		out.WriteString("\x1b[0m")
	}
}

func ansiStyleSequence(style ui.Style, colors lineColorCapabilities) string {
	codes := []string{"0"}
	codes = append(codes, ansiColorParams(style.Fg, false, colors)...)
	codes = append(codes, ansiColorParams(style.Bg, true, colors)...)
	if style.Modifier&tcell.AttrBold != 0 {
		codes = append(codes, "1")
	}
	if style.Modifier&tcell.AttrDim != 0 {
		codes = append(codes, "2")
	}
	if style.Modifier&tcell.AttrItalic != 0 {
		codes = append(codes, "3")
	}
	if style.Modifier&tcell.AttrBlink != 0 {
		codes = append(codes, "5")
	}
	if style.Modifier&tcell.AttrReverse != 0 {
		codes = append(codes, "7")
	}
	if style.Modifier&tcell.AttrStrikeThrough != 0 {
		codes = append(codes, "9")
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// ansiPalette256 is the whole xterm palette, used to degrade a truecolor value
// on a surface that cannot render it. Slots 0-15 participate, as they do in
// tcell's own palette (termenv excludes them because terminals remap them), so
// a degraded color may land on a remappable slot - the same tradeoff tcell
// makes for the TUI.
var ansiPalette256 = func() []tcellcolor.Color {
	palette := make([]tcellcolor.Color, 256)
	for index := range palette {
		palette[index] = tcellcolor.PaletteColor(index)
	}
	return palette
}()

// nearestPaletteSlot degrades one RGB color to its nearest xterm palette slot.
// color.Find is a 256-entry CIE76 search and a sequence is recomputed on every
// style change, so the answer is memoized: a theme names a bounded set of
// colors, and without the memo the same searches would repeat for the life of
// the process. The nearest slot is a pure function of the color, so the memo
// never needs invalidation; the lock covers the answer and status writers,
// which emit from different goroutines.
var nearestPaletteMemo = struct {
	sync.Mutex
	slots map[ui.Color]ui.Color
}{slots: map[ui.Color]ui.Color{}}

func nearestPaletteSlot(color ui.Color) ui.Color {
	nearestPaletteMemo.Lock()
	nearest, ok := nearestPaletteMemo.slots[color]
	nearestPaletteMemo.Unlock()
	if ok {
		return nearest
	}
	nearest = tcellcolor.Find(color, ansiPalette256)
	nearestPaletteMemo.Lock()
	nearestPaletteMemo.slots[color] = nearest
	nearestPaletteMemo.Unlock()
	return nearest
}

// ansiColorParams returns the SGR parameters expressing one resolved color, or
// nil when the color must emit no code at all.
//
// Palette slots 0-15 keep exactly the classic codes (30-37 and 90-97 for the
// foreground, 40-47 and 100-107 for the background) because those slots are the
// terminal's own, remappable ones. Slots 16-255 use the xterm 38;5;N/48;5;N
// form with N = int(color)&0xffffff, tcell's own extraction rule. RGB uses
// 38;2;r;g;b/48;2;r;g;b only on a truecolor surface, and otherwise degrades to
// the nearest palette index, the way tcell degrades RGB for the TUI. A theme's
// "inherit" (ui.ColorClear) emits nothing, so the terminal default shows.
func ansiColorParams(color ui.Color, background bool, colors lineColorCapabilities) []string {
	if color == ui.ColorClear {
		return nil
	}
	if color.IsRGB() {
		if colors.truecolor {
			red, green, blue := color.RGB()
			return []string{
				ansiExtendedIntroducer(background), "2",
				strconv.Itoa(int(red)), strconv.Itoa(int(green)), strconv.Itoa(int(blue)),
			}
		}
		// Find always returns one of ansiPalette256's entries.
		color = nearestPaletteSlot(color)
	}
	index, ok := style.PaletteIndex(color)
	if !ok {
		return nil
	}
	if index < 16 {
		return []string{strconv.Itoa(ansiClassicCode(index, background))}
	}
	return []string{ansiExtendedIntroducer(background), "5", strconv.Itoa(index)}
}

// ansiClassicCode is the ECMA-48 code for palette slots 0-15: 30-37/40-47 for
// the first eight, 90-97/100-107 for the bright half.
func ansiClassicCode(index int, background bool) int {
	if index < 8 {
		if background {
			return 40 + index
		}
		return 30 + index
	}
	if background {
		return 100 + index - 8
	}
	return 90 + index - 8
}

// ansiExtendedIntroducer selects the 38/48 extended-color introducer.
func ansiExtendedIntroducer(background bool) string {
	if background {
		return "48"
	}
	return "38"
}

func lineImagePayload(img style.Image, capabilities outputCapabilities, prefixWidth int) []byte {
	if capabilities.imageProtocol == termimg.ProtocolNone {
		return nil
	}
	imageMaxCols, imageMaxRows := style.ImageBounds(img)
	maxCols := min(imageMaxCols, capabilities.columns-prefixWidth)
	cols, rows, fitByRows := termimg.CellGeometry(
		img,
		maxCols,
		imageMaxRows,
		defaultLineCellWidth,
		defaultLineCellHeight,
	)
	if cols <= 0 || rows <= 0 {
		return nil
	}
	desired := termimg.Desired{Placement: termimg.Placement{
		Path:      img.Path,
		Cols:      cols,
		Rows:      rows,
		FitByRows: fitByRows,
	}}
	maxWidth := cols * defaultLineCellWidth
	maxHeight := rows * defaultLineCellHeight

	var payload []byte
	switch capabilities.imageProtocol {
	case termimg.ProtocolKitty:
		prepared := termimg.PrepareKitty(desired, maxWidth, maxHeight)
		if prepared.Err != nil || len(prepared.Data) == 0 {
			return nil
		}
		payload = kittyDisplayPNG(prepared.Data, cols, rows, prepared.FitByRows)
	case termimg.ProtocolSixel:
		prepared := termimg.PrepareSixel(desired, maxWidth, maxHeight)
		if prepared.Err != nil || len(prepared.Data) == 0 {
			return nil
		}
		payload = append([]byte("\x1b7"), prepared.Data...)
		payload = append(payload, []byte("\x1b8")...)
	default:
		return nil
	}
	return append(payload, []byte(strings.Repeat("\n", rows))...)
}

// kittyDisplayPNG transmits and displays one anonymous image at the current
// cursor without letting the graphics command move it. The caller advances by
// the reserved text rows after the payload.
func kittyDisplayPNG(pngData []byte, cols, rows int, fitByRows bool) []byte {
	if len(pngData) == 0 || cols <= 0 || rows <= 0 {
		return nil
	}
	return termimg.KittyChunked(fmt.Sprintf("a=T,f=100,t=d,q=2,%s,C=1", termimg.KittySizeSpec(cols, rows, fitByRows)), pngData)
}
