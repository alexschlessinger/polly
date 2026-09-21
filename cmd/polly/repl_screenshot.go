package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// screenshotRequest is a /screenshot parked for the next painted frame. The
// capture has to read the screen after the composer cleared and before the
// command's own notice lands, which is exactly the frame render paints next.
type screenshotRequest struct {
	path string
}

// requestScreenshot parks a capture of the next frame. A no-op UI task makes
// the loop paint one, since the command itself changes no visible state.
func (r *managedREPL) requestScreenshot(path string) {
	if path == "" {
		path = defaultScreenshotPath()
	}
	r.shot = &screenshotRequest{path: expandUserPath(path)}
	r.postUITask(func() {})
}

// finishPendingScreenshot writes the frame render just painted and appends its
// notice, which therefore lands on the following frame rather than inside the
// image it reports. Caller must not hold the model lock: the capture reads the
// screen and encodes a PNG.
func (r *managedREPL) finishPendingScreenshot() {
	shot := r.shot
	if shot == nil {
		return
	}
	r.shot = nil
	saved, err := r.writeScreenshot(shot.path)
	r.model.mu.Lock()
	if err != nil {
		r.model.appendNoticeLine("screenshot: " + err.Error())
	} else {
		r.model.appendNoticeLine("screenshot: " + saved)
	}
	r.model.mu.Unlock()
	r.postUITask(func() {})
}

// writeScreenshot encodes the live screen to path and returns the absolute path
// it wrote.
func (r *managedREPL) writeScreenshot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve screenshot path: %w", err)
	}
	if err := r.captureScreen(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// captureScreen writes a PNG of the cells the screen currently holds, with the
// images polly placed painted over them. Native graphics are escape sequences
// rather than cells, so without those overlays a capture would show empty cells
// where a terminal shows a thumbnail.
func (r *managedREPL) captureScreen(path string) error {
	screen := ui.DefaultBackend.Screen
	if screen == nil {
		return errors.New("no screen")
	}
	fg, bg := screenshotSurface()
	return screenimg.SavePNG(path, screen, fg, bg, screenshotOverlays(r.images)...)
}

// screenshotOverlays converts the images a frame placed into capture overlays.
func screenshotOverlays(images *termimg.Manager) []screenimg.Overlay {
	placed := images.Placements()
	overlays := make([]screenimg.Overlay, 0, len(placed))
	for _, image := range placed {
		overlays = append(overlays, screenimg.Overlay{Rect: image.Slot, Visible: image.Visible, Image: image.Image})
	}
	return overlays
}

// screenshotSurface is the pair a cell that names no color of its own is
// painted with: the theme's resolved surface roles, or the capture's own
// assumption about the terminal for a theme that leaves them at "inherit",
// since a PNG cell cannot be transparent.
func screenshotSurface() (tcell.Color, tcell.Color) {
	fg, bg := style.Surface()
	if !fg.Valid() {
		fg = screenimg.DefaultForeground
	}
	if !bg.Valid() {
		bg = screenimg.DefaultBackground
	}
	return fg, bg
}

// defaultScreenshotPath is where /screenshot writes without an argument.
func defaultScreenshotPath() string {
	return filepath.Join(os.TempDir(), "polly-screenshot.png")
}

// expandUserPath resolves a leading ~ the way the file paths typed into other
// commands are resolved.
func expandUserPath(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
