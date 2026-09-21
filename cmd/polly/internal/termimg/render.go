package termimg

import (
	"image"
	"io"

	tcell "github.com/gdamore/tcell/v3"

	"github.com/alexschlessinger/pollytool/images"
)

// A render surface is one that draws the frame itself — a headless capture, or
// /screenshot — instead of sending escapes to a terminal. It needs the images
// polly placed as pixels rather than as kitty or sixel payloads, so
// ProtocolRender tracks the placements and encodes nothing; the surface paints
// them over its own cell grid (see Placements).

// PlacedImage is one image a manager has put on screen: the cell rectangle the
// image occupies and the pixels a terminal shows there.
type PlacedImage struct {
	// Slot is the placement's full cell rectangle, and Visible is the part of
	// it on screen — equal to Slot unless a thumbnail is scrolled partly out of
	// the pane. Visible maps onto Image at one cell per drawRect step, which is
	// how the encoded protocols position a clipped placement too.
	Slot    image.Rectangle
	Visible image.Rectangle
	// Image is the placement's source fitted to Slot: one pixel per screen
	// pixel from Slot's top-left corner.
	Image image.Image
}

// Placements reports the images currently placed, in placement order, for a
// surface that paints them itself, fitted to that surface's cell pixel size.
// It is nil for a manager that placed nothing or an invalid cell size.
func (m *Manager) Placements(cellWidth, cellHeight int) []PlacedImage {
	if m == nil || len(m.active) == 0 || cellWidth <= 0 || cellHeight <= 0 {
		return nil
	}
	placed := make([]PlacedImage, 0, len(m.active))
	for _, active := range m.active {
		placement := active.Placement
		x, y, cols, rows := placement.drawRect()
		if cols <= 0 || rows <= 0 {
			continue
		}
		source, err := loadPlacementImage(placement)
		if err != nil {
			continue
		}
		// Fit the full slot before clipping, using the capture's geometry:
		// terminal cells can have a different pixel size from the PNG's font.
		fitted := images.Fit(source, placement.Cols*cellWidth, placement.Rows*cellHeight)
		if fitted.Bounds().Empty() {
			continue
		}
		placed = append(placed, PlacedImage{
			Slot:    image.Rect(placement.X, placement.Y, placement.X+placement.Cols, placement.Y+placement.Rows),
			Visible: image.Rect(x, y, x+cols, y+rows),
			Image:   fitted,
		})
	}
	return placed
}

// commitRender records the placements a frame would have drawn, without
// encoding a payload or writing an escape.
func (m *Manager) commitRender() {
	for _, desired := range m.desired {
		if _, err := loadPlacementImage(desired.Placement); err != nil {
			continue
		}
		x, y, cols, rows := desired.drawRect()
		m.screen.LockRegion(x, y, cols, rows, true)
		m.active = append(m.active, activeTerminalImage{Desired: desired})
	}
}

// NewRenderManager builds a manager for a surface that paints the frame itself.
// It reports the cell pixel geometry the surface will draw at — so placements
// and images land on the same pixels — and writes nothing anywhere.
func NewRenderManager(screen tcell.Screen, cols, rows, cellWidth, cellHeight int) *Manager {
	if cellWidth <= 0 || cellHeight <= 0 {
		cellWidth, cellHeight = defaultCellWidth, defaultCellHeight
	}
	tty := renderTty{cols: cols, rows: rows, cellWidth: cellWidth, cellHeight: cellHeight}
	return NewManagerFor(screen, tty, ProtocolRender)
}

// renderTty stands in for the terminal a manager would write to: it answers the
// pixel geometry and discards every write.
type renderTty struct {
	cols, rows            int
	cellWidth, cellHeight int
}

func (t renderTty) Start() error               { return nil }
func (t renderTty) Stop() error                { return nil }
func (t renderTty) Drain() error               { return nil }
func (t renderTty) NotifyResize(_ chan<- bool) {}
func (t renderTty) Read(_ []byte) (int, error) { return 0, io.EOF }
func (t renderTty) Write(p []byte) (int, error) {
	return len(p), nil
}
func (t renderTty) Close() error { return nil }

func (t renderTty) WindowSize() (tcell.WindowSize, error) {
	return tcell.WindowSize{
		Width:       t.cols,
		Height:      t.rows,
		PixelWidth:  t.cols * t.cellWidth,
		PixelHeight: t.rows * t.cellHeight,
	}, nil
}
