package main

import (
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// startupLogoHeight includes one blank row below the six-row mark. On short
// terminals the logo is omitted whole rather than clipped into something ugly.
const startupLogoHeight = 7

var (
	pollyGreen = tcell.NewRGBColor(0x01, 0xab, 0x46)
	pollyLight = tcell.NewRGBColor(0x57, 0xcd, 0x75)
	pollyWing  = tcell.NewRGBColor(0xb7, 0xd0, 0x19)
	pollyCrown = tcell.NewRGBColor(0xff, 0x2b, 0x23)
	pollyBeak  = tcell.NewRGBColor(0xff, 0xd5, 0x1d)
	pollyMouth = tcell.NewRGBColor(0xff, 0x60, 0x0d)
	pollyFace  = tcell.NewRGBColor(0xec, 0x80, 0xaf)
	pollyEye   = tcell.NewRGBColor(0x31, 0x2f, 0x2a)
	pollyFoot  = tcell.NewRGBColor(0xfe, 0xba, 0x02)
)

// pollyLogoPixels is a compact rendering of the supplied vector mark: red
// crown, yellow/orange left-facing beak, pink face, green body, lime wing,
// swept tail, and gold feet. Two pixel rows become one terminal half-block row
// so the bird keeps the vector's proportions in a character cell grid.
var pollyLogoPixels = []string{
	"  RRR        ",
	" YRRLL       ",
	"YPPDLG       ",
	" OPGGG       ",
	"  GGGAA      ",
	" GGGGAAA     ",
	" GGGGAAAA    ",
	"  GGGAAATT   ",
	"   GGGGATTT  ",
	"    GGGTT    ",
	"   F G F     ",
	"  FFF FFF    ",
}

func pollyPixelColor(pixel rune) (ui.Color, bool) {
	switch pixel {
	case 'G', 'T':
		return pollyGreen, true
	case 'L':
		return pollyLight, true
	case 'A':
		return pollyWing, true
	case 'R':
		return pollyCrown, true
	case 'Y':
		return pollyBeak, true
	case 'O':
		return pollyMouth, true
	case 'P':
		return pollyFace, true
	case 'D':
		return pollyEye, true
	case 'F':
		return pollyFoot, true
	default:
		return ui.ColorClear, false
	}
}

func pollyHalfBlock(topPixel, bottomPixel rune) ui.Cell {
	top, topPainted := pollyPixelColor(topPixel)
	bottom, bottomPainted := pollyPixelColor(bottomPixel)
	switch {
	case !topPainted && !bottomPainted:
		return ui.Cell{Rune: ' ', Style: ui.StyleClear}
	case topPainted && !bottomPainted:
		return ui.Cell{Rune: '▀', Style: ui.NewStyle(top)}
	case !topPainted && bottomPainted:
		return ui.Cell{Rune: '▄', Style: ui.NewStyle(bottom)}
	case top == bottom:
		return ui.Cell{Rune: '█', Style: ui.NewStyle(top)}
	default:
		return ui.Cell{Rune: '▀', Style: ui.NewStyle(top, bottom)}
	}
}

func pollyLogoRows(width int) [][]ui.Cell {
	rows := make([][]ui.Cell, startupLogoHeight)
	if width <= 0 {
		return rows
	}
	logoWidth := len([]rune(pollyLogoPixels[0]))
	sourceLeft := max(0, (logoWidth-width)/2)

	for sourceRow := 0; sourceRow < len(pollyLogoPixels); sourceRow += 2 {
		top := []rune(pollyLogoPixels[sourceRow])
		bottom := []rune(pollyLogoPixels[sourceRow+1])
		row := make([]ui.Cell, 0, min(width, logoWidth))
		for column := sourceLeft; column < logoWidth && len(row) < width; column++ {
			row = append(row, pollyHalfBlock(top[column], bottom[column]))
		}
		rows[sourceRow/2] = row
	}
	return rows
}

// startupLogoRowCount preserves at least one transcript row. This keeps the
// normal TUI usable immediately even when the terminal is short. Terminals
// with native graphics get the taller image splash; when they are too short
// for it they degrade to the half-block art, then to nothing.
func startupLogoRowCount(contentHeight int, visible, nativeImages bool) int {
	if !visible {
		return 0
	}
	if nativeImages && contentHeight > imageLogoHeight {
		return imageLogoHeight
	}
	if contentHeight <= startupLogoHeight {
		return 0
	}
	return startupLogoHeight
}
