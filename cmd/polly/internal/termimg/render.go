package termimg

import (
	"image"
	"io"

	tcell "github.com/gdamore/tcell/v3"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
)

// A render surface is one that draws the frame itself — a headless capture, or
// /screenshot — instead of sending escapes to a terminal. It needs the images
// polly placed as pixels rather than as kitty or sixel payloads, so
// ProtocolRender tracks the placements and encodes nothing; the surface paints
// them over its own cell grid (see Placements).

// Placements reports the images currently placed, in placement order, as the
// overlays a capture paints over its cells: each placement's full cell slot,
// the part of it on screen (all of it unless a thumbnail is scrolled partly out
// of the pane), and its source image, which the capture fits to the slot at
// its own cell size. It is nil for a manager that placed nothing.
func (m *Manager) Placements() []screenimg.Overlay {
	if m == nil || len(m.active) == 0 {
		return nil
	}
	overlays := make([]screenimg.Overlay, 0, len(m.active))
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
		overlays = append(overlays, screenimg.Overlay{
			Rect:    image.Rect(placement.X, placement.Y, placement.X+placement.Cols, placement.Y+placement.Rows),
			Visible: image.Rect(x, y, x+cols, y+rows),
			Image:   source,
		})
	}
	return overlays
}

// commitRender records the placements a frame would have drawn, without
// encoding a payload or writing an escape. An image that cannot be loaded is
// left out by Placements when a capture is taken.
func (m *Manager) commitRender() {
	for _, desired := range m.desired {
		x, y, cols, rows := desired.drawRect()
		m.screen.LockRegion(x, y, cols, rows, true)
		m.active = append(m.active, activeTerminalImage{Desired: desired})
	}
}

// NewRenderManager builds a manager for a surface that paints the frame itself.
// It reports the cell pixel geometry the surface will draw at — so placements
// and images land on the same pixels — and writes nothing anywhere.
func NewRenderManager(screen tcell.Screen, cols, rows, cellWidth, cellHeight int) *Manager {
	tty := renderTty{window: tcell.WindowSize{
		Width:       cols,
		Height:      rows,
		PixelWidth:  cols * cellWidth,
		PixelHeight: rows * cellHeight,
	}}
	return NewManagerFor(screen, tty, ProtocolRender)
}

// renderTty stands in for the terminal a manager would write to: it answers the
// pixel geometry and discards every write.
type renderTty struct {
	window tcell.WindowSize
}

func (t renderTty) Start() error                          { return nil }
func (t renderTty) Stop() error                           { return nil }
func (t renderTty) Drain() error                          { return nil }
func (t renderTty) NotifyResize(_ chan<- bool)            {}
func (t renderTty) Read(_ []byte) (int, error)            { return 0, io.EOF }
func (t renderTty) Write(p []byte) (int, error)           { return len(p), nil }
func (t renderTty) Close() error                          { return nil }
func (t renderTty) WindowSize() (tcell.WindowSize, error) { return t.window, nil }
