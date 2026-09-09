package termimg

import (
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/images"
)

// Placement is one thumbnail's target on screen: the image source, its
// cell rectangle, and whether the reserved rows or columns bound the fit.
type Placement struct {
	Key  string
	Path string
	// Embedded names a compile-time asset in embeddedTerminalImages instead
	// of a file on disk. A string key keeps the struct comparable.
	Embedded   string
	X, Y       int
	Cols, Rows int
	FitByRows  bool
}

// CellGeometry fits an image inside a maximum cell rectangle while
// accounting for the fact that terminal cells are usually taller than they
// are wide. The returned axis tells Kitty which single dimension to constrain;
// Kitty derives the other from the source aspect ratio without distortion.
func CellGeometry(img style.Image, maxCols, maxRows, cellWidth, cellHeight int) (cols, rows int, fitByRows bool) {
	if maxCols < style.MinimumThumbnailCols || maxRows <= 0 {
		return 0, 0, false
	}
	if cellWidth <= 0 {
		cellWidth = 10
	}
	if cellHeight <= 0 {
		cellHeight = 20
	}
	if img.Width <= 0 || img.Height <= 0 {
		return maxCols, maxRows, false
	}

	maxPixelWidth := maxCols * cellWidth
	maxPixelHeight := maxRows * cellHeight
	pixelWidth, pixelHeight := images.FitDimensions(img.Width, img.Height, maxPixelWidth, maxPixelHeight)
	if pixelWidth <= 0 || pixelHeight <= 0 {
		return 0, 0, false
	}
	cols = min(maxCols, max(1, (pixelWidth+cellWidth-1)/cellWidth))
	rows = min(maxRows, max(1, (pixelHeight+cellHeight-1)/cellHeight))
	fitByRows = imageFitsByRows(img.Width, img.Height, maxPixelWidth, maxPixelHeight)
	return cols, rows, fitByRows
}
