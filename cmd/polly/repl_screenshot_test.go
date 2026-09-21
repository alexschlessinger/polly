package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	tcell "github.com/gdamore/tcell/v3"
)

func TestScreenshotFitsImagesToCaptureCells(t *testing.T) {
	cw, ch := screenimg.CellSize()
	colors := []color.RGBA{{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255}}
	// The terminal uses cells twice the capture size. Each image quadrant must
	// survive the conversion, including when the top half is scrolled away.
	src := image.NewRGBA(image.Rect(0, 0, 16*cw, 8*ch))
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			src.SetRGBA(x, y, colors[(y/(4*ch))*2+x/(8*cw)])
		}
	}
	path := filepath.Join(t.TempDir(), "quadrants.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, src); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		clipped bool
	}{
		{name: "whole"},
		{name: "clipped", clipped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, screen := affordanceTestREPL(t)
			r.headless = &headlessRun{screen: screen}
			screen.SetSize(10, 6)
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 10, Height: 6, PixelWidth: 20 * cw, PixelHeight: 12 * ch}}
			r.images = termimg.NewManagerFor(screen, tty, termimg.ProtocolKitty)
			defer r.images.Shutdown()
			placement := termimg.Placement{Key: "quadrants", Path: path, X: 1, Y: 1, Cols: 8, Rows: 4}
			if tc.clipped {
				placement.Y = -2
				placement.Clip = termimg.Clip{Y: 2, Cols: 8, Rows: 2}
			}
			r.images.Commit(r.images.Prepare([]termimg.Placement{placement}))
			capturePath := filepath.Join(t.TempDir(), "capture.png")
			if _, err := r.writeScreenshot(capturePath); err != nil {
				t.Fatal(err)
			}
			captureFile, err := os.Open(capturePath)
			if err != nil {
				t.Fatal(err)
			}
			defer captureFile.Close()
			capture, err := png.Decode(captureFile)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range colors {
				if tc.clipped && i < 2 {
					continue
				}
				x := (placement.X + 2 + (i%2)*4) * cw
				y := (placement.Y + 1 + (i/2)*2) * ch
				if got := color.RGBAModel.Convert(capture.At(x, y)); got != want {
					t.Fatalf("quadrant %d pixel (%d,%d) = %v, want %v", i, x, y, got, want)
				}
			}
		})
	}
}

func TestScreenshotCommandRequiresTheManagedRepl(t *testing.T) {
	var lines []string
	ctx := &replCommandContext{reply: func(line string) error {
		lines = append(lines, line)
		return nil
	}}
	if res := replScreenshotCommand(ctx, []string{"/screenshot"}); res.err != nil {
		t.Fatalf("command error = %v", res.err)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "screenshots require the managed REPL") {
		t.Fatalf("reply = %q, want the unavailable notice", got)
	}
	if res := replScreenshotCommand(nil, []string{"/screenshot"}); res.err != nil {
		t.Fatalf("nil context error = %v", res.err)
	}
}

func TestScreenshotCapturesTheNextPaintedFrame(t *testing.T) {
	r, screen := affordanceTestREPL(t)
	r.headless = &headlessRun{screen: screen}
	path := filepath.Join(t.TempDir(), "shots", "frame.png")
	if handled, quit := r.runCommand("/screenshot " + path); !handled || quit {
		t.Fatalf("/screenshot handled=%v quit=%v", handled, quit)
	}
	// The capture waits for the frame the command's own submission paints, so
	// the image cannot show the typed command still sitting in the composer.
	if r.shotPath == "" {
		t.Fatal("no screenshot parked by the command")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("screenshot was written before the frame was painted")
	}
	r.render()
	if r.shotPath != "" {
		t.Fatal("parked screenshot survived the frame that captured it")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open screenshot: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode screenshot: %v", err)
	}
	width, height := screen.Size()
	dx, dy := img.Bounds().Dx(), img.Bounds().Dy()
	if dx%width != 0 || dy%height != 0 || dx <= width || dy <= height {
		t.Fatalf("screenshot is %dx%d, want whole font cells of the %dx%d screen", dx, dy, width, height)
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "screenshot: "+path) {
		t.Fatalf("transcript = %q, want the saved path", got)
	}
}

func TestScreenshotPathsDefaultToTheTempDirectory(t *testing.T) {
	if got := defaultScreenshotPath(); !strings.HasPrefix(got, os.TempDir()) || !strings.HasSuffix(got, "polly-screenshot.png") {
		t.Fatalf("default path = %q, want one under the temp directory", got)
	}

	r, _ := affordanceTestREPL(t)
	if handled, _ := r.runCommand("/screenshot"); !handled {
		t.Fatal("/screenshot without a path was not handled")
	}
	if r.shotPath != defaultScreenshotPath() {
		t.Fatalf("parked shot = %q, want the default path", r.shotPath)
	}
	// Leave nothing parked: this test never paints the frame that would write it.
	r.shotPath = ""
}

func TestHeadlessScreenshotReadsPresentedFrame(t *testing.T) {
	r, screen := affordanceTestREPL(t)
	r.headless = &headlessRun{screen: screen}
	screen.Show()
	capture := func(name string) []byte {
		t.Helper()
		path := filepath.Join(t.TempDir(), name+".png")
		if _, err := r.writeScreenshot(path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := capture("before")
	screen.Put(1, 1, "X", tcell.StyleDefault.Foreground(tcell.ColorRed))
	if !bytes.Equal(before, capture("unpresented")) {
		t.Fatal("screenshot included an unpresented write")
	}
	screen.Show()
	if bytes.Equal(before, capture("presented")) {
		t.Fatal("screenshot missed the presented write")
	}
}
