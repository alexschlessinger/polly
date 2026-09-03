package main

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"

	tcell "github.com/gdamore/tcell/v3"
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
	rendered, images, _ := renderMarkdownWithLocalImages(src, baseDir, false)
	if rendered == "" {
		return nil
	}

	lines := strings.Split(rendered, "\n")
	displayedImages := make(map[int]bool, len(images))
	var out bytes.Buffer
	for lineIndex, line := range lines {
		cells := parseStyledCells(line, ui.StyleClear)
		markerIndex, markerCell := lineImageMarker(cells, len(images))
		if markerIndex >= 0 {
			if !displayedImages[markerIndex] {
				displayedImages[markerIndex] = true
				prefix := cells[:markerCell]
				prefixWidth := styledCellsWidth(prefix)
				if payload := lineImagePayload(images[markerIndex], capabilities, prefixWidth); len(payload) > 0 {
					appendANSIStyledCells(&out, prefix)
					out.Write(payload)
				}
			}
			continue
		}

		appendANSIStyledCells(&out, cells)
		if lineIndex < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func lineImageMarker(cells []ui.Cell, imageCount int) (imageIndex, cellIndex int) {
	for i, cell := range cells {
		index, ok := transcriptImageMarkerIndex(cell.Rune)
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

func appendANSIStyledCells(out *bytes.Buffer, cells []ui.Cell) {
	current := ui.StyleClear
	for _, cell := range cells {
		if cell.Style != current {
			out.WriteString(ansiStyleSequence(cell.Style))
			current = cell.Style
		}
		// The renderer owns every escape sequence in rich line output. Model
		// text must not be able to inject OSC/CSI commands or cursor controls.
		if unicode.IsControl(cell.Rune) && cell.Rune != '\t' {
			continue
		}
		out.WriteRune(cell.Rune)
	}
	if current != ui.StyleClear {
		out.WriteString("\x1b[0m")
	}
}

func ansiStyleSequence(style ui.Style) string {
	codes := []string{"0"}
	if code, ok := ansiPaletteCode(style.Fg, false); ok {
		codes = append(codes, fmt.Sprint(code))
	}
	if code, ok := ansiPaletteCode(style.Bg, true); ok {
		codes = append(codes, fmt.Sprint(code))
	}
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

func ansiPaletteCode(color ui.Color, background bool) (int, bool) {
	if color == ui.ColorClear {
		return 0, false
	}
	for index := range 16 {
		if color != tcell.PaletteColor(index) {
			continue
		}
		if index < 8 {
			if background {
				return 40 + index, true
			}
			return 30 + index, true
		}
		if background {
			return 100 + index - 8, true
		}
		return 90 + index - 8, true
	}
	return 0, false
}

func lineImagePayload(img transcriptImage, capabilities outputCapabilities, prefixWidth int) []byte {
	if capabilities.imageProtocol == terminalImageNone {
		return nil
	}
	imageMaxCols, imageMaxRows := transcriptImageBounds(img)
	maxCols := min(imageMaxCols, capabilities.columns-prefixWidth)
	cols, rows, fitByRows := imageCellGeometry(
		img,
		maxCols,
		imageMaxRows,
		defaultLineCellWidth,
		defaultLineCellHeight,
	)
	if cols <= 0 || rows <= 0 {
		return nil
	}
	desired := desiredTerminalImage{terminalImagePlacement: terminalImagePlacement{
		Path:      img.Path,
		Cols:      cols,
		Rows:      rows,
		FitByRows: fitByRows,
	}}
	maxWidth := cols * defaultLineCellWidth
	maxHeight := rows * defaultLineCellHeight

	var payload []byte
	switch capabilities.imageProtocol {
	case terminalImageKitty:
		prepared := prepareKittyImage(desired, maxWidth, maxHeight)
		if prepared.err != nil || len(prepared.data) == 0 {
			return nil
		}
		payload = kittyDisplayPNG(prepared.data, cols, rows, prepared.fitByRows)
	case terminalImageSixel:
		prepared := prepareSixelImage(desired, maxWidth, maxHeight)
		if prepared.err != nil || len(prepared.data) == 0 {
			return nil
		}
		payload = append([]byte("\x1b7"), prepared.data...)
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
	return kittyChunked(fmt.Sprintf("a=T,f=100,t=d,q=2,%s,C=1", kittySizeSpec(cols, rows, fitByRows)), pngData)
}
