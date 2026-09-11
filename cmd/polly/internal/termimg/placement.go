package termimg

import (
	"image"

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
	// Clip trims the placement to the part of its slot that is inside the
	// pane, so a thumbnail scrolled half out of the viewport still draws.
	// The zero value draws the whole slot.
	Clip Clip
}

// Clip is the visible sub-rectangle of a thumbnail slot, in cells: X and Y
// are its offsets inside the full Cols x Rows rectangle, and its own Cols and
// Rows are the size that is actually on screen.
type Clip struct {
	X, Y       int
	Cols, Rows int
}

// drawRect is the on-screen cell rectangle a placement paints and locks: the
// whole slot, or its visible sub-rectangle when Clip trims it.
func (p Placement) drawRect() (x, y, cols, rows int) {
	if p.Clip.Cols > 0 && p.Clip.Rows > 0 {
		return p.X + p.Clip.X, p.Y + p.Clip.Y, p.Clip.Cols, p.Clip.Rows
	}
	return p.X, p.Y, p.Cols, p.Rows
}

// clipSourceRect maps the visible cell sub-rectangle onto the pixel rectangle
// of the fitted slot image. Fitting keeps that image inside the slot box, so
// the mapping is proportional; every edge keeps at least one pixel, and an
// empty clip or unknown pixel size means the whole image.
func clipSourceRect(pixelWidth, pixelHeight, cols, rows int, clip Clip) image.Rectangle {
	if clip.Cols <= 0 || clip.Rows <= 0 {
		return image.Rect(0, 0, pixelWidth, pixelHeight)
	}
	if pixelWidth <= 0 || pixelHeight <= 0 || cols <= 0 || rows <= 0 {
		return image.Rectangle{}
	}
	x := min(max(0, clip.X)*pixelWidth/cols, pixelWidth-1)
	y := min(max(0, clip.Y)*pixelHeight/rows, pixelHeight-1)
	width := max(1, clip.Cols*pixelWidth/cols)
	height := max(1, clip.Rows*pixelHeight/rows)
	width = min(width, pixelWidth-x)
	height = min(height, pixelHeight-y)
	return image.Rect(x, y, x+width, y+height)
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
