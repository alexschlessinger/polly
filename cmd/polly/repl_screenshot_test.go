package main

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	path := filepath.Join(t.TempDir(), "shots", "frame.png")
	if handled, quit := r.runCommand("/screenshot " + path); !handled || quit {
		t.Fatalf("/screenshot handled=%v quit=%v", handled, quit)
	}
	// The capture waits for the frame the command's own submission paints, so
	// the image cannot show the typed command still sitting in the composer.
	if r.shot == nil {
		t.Fatal("no screenshot parked by the command")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("screenshot was written before the frame was painted")
	}
	r.render()
	if r.shot != nil {
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

func TestScreenshotPathsDefaultAndExpandHome(t *testing.T) {
	if got := expandUserPath("/tmp/shot.png"); got != "/tmp/shot.png" {
		t.Fatalf("absolute path = %q, want it unchanged", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if got, want := expandUserPath("~/shots/x.png"), filepath.Join(home, "shots/x.png"); got != want {
		t.Fatalf("expanded path = %q, want %q", got, want)
	}
	if got := defaultScreenshotPath(); !strings.HasPrefix(got, os.TempDir()) || !strings.HasSuffix(got, "polly-screenshot.png") {
		t.Fatalf("default path = %q, want one under the temp directory", got)
	}

	r, _ := affordanceTestREPL(t)
	if handled, _ := r.runCommand("/screenshot"); !handled {
		t.Fatal("/screenshot without a path was not handled")
	}
	if r.shot == nil || r.shot.path != expandUserPath(defaultScreenshotPath()) {
		t.Fatalf("parked shot = %#v, want the default path", r.shot)
	}
	// Leave nothing parked: this test never paints the frame that would write it.
	r.shot = nil
}
