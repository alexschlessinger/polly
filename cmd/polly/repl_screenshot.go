package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// requestScreenshot parks a /screenshot for the next painted frame. The capture
// has to read the screen after the composer cleared and before the command's
// own notice lands, which is exactly the frame the loop paints once the command
// has run.
func (r *managedREPL) requestScreenshot(path string) {
	if path == "" {
		path = defaultScreenshotPath()
	}
	r.shotPath = expandHomePath(path)
}

// finishPendingScreenshot writes the frame render just painted and appends its
// notice, which therefore lands on the following frame rather than inside the
// image it reports. Caller must not hold the model lock: the capture reads the
// screen and encodes a PNG.
func (r *managedREPL) finishPendingScreenshot() {
	path := r.shotPath
	if path == "" {
		return
	}
	r.shotPath = ""
	saved, err := r.writeScreenshot(path)
	r.model.mu.Lock()
	if err != nil {
		r.model.appendNoticeLine("screenshot: " + err.Error())
	} else {
		r.model.appendNoticeLine("screenshot: " + saved)
	}
	r.model.mu.Unlock()
	r.postUITask(func() {})
}

// writeScreenshot writes a PNG of the cells the screen currently holds, with the
// images polly placed painted over them, and returns the absolute path it
// wrote. Native graphics are escape sequences rather than cells, so without
// those overlays a capture would show empty cells where a terminal shows a
// thumbnail.
func (r *managedREPL) writeScreenshot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve screenshot path: %w", err)
	}
	screen := ui.DefaultBackend.Screen
	if screen == nil {
		return "", errors.New("no screen")
	}
	var source screenimg.Source = screen
	if r.headless != nil && r.headless.screen != nil {
		frame, err := r.headless.screen.Snapshot()
		if err != nil {
			return "", err
		}
		source = frame
	}
	fg, bg := screenshotSurface()
	if err := screenimg.SavePNG(abs, source, fg, bg, r.images.Placements()...); err != nil {
		return "", err
	}
	return abs, nil
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
