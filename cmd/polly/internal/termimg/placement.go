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

// Bounds is the on-screen cell rectangle the placement paints.
func (p Placement) Bounds() image.Rectangle {
	x, y, cols, rows := p.drawRect()
	return image.Rect(x, y, x+cols, y+rows)
}

// clipSourceRect maps the visible cell sub-rectangle onto the pixel rectangle
// of the fitted slot image. The fitted image is drawn one pixel per screen
// pixel from the slot's top-left corner and usually covers only part of the
// slot along one axis, so cells map to pixels at the cell size rather than
// proportionally across the slot; the result is clamped to the image. An
// empty clip means the whole image; unknown pixel or cell sizes mean none.
func clipSourceRect(pixelWidth, pixelHeight, cellWidth, cellHeight int, clip Clip) image.Rectangle {
	if clip.Cols <= 0 || clip.Rows <= 0 {
		return image.Rect(0, 0, pixelWidth, pixelHeight)
	}
	if pixelWidth <= 0 || pixelHeight <= 0 || cellWidth <= 0 || cellHeight <= 0 {
		return image.Rectangle{}
	}
	cells := image.Rectangle{
		Min: image.Pt(max(0, clip.X)*cellWidth, max(0, clip.Y)*cellHeight),
		Max: image.Pt((max(0, clip.X)+clip.Cols)*cellWidth, (max(0, clip.Y)+clip.Rows)*cellHeight),
	}
	return cells.Intersect(image.Rect(0, 0, pixelWidth, pixelHeight))
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
		cellWidth = defaultCellWidth
	}
	if cellHeight <= 0 {
		cellHeight = defaultCellHeight
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
