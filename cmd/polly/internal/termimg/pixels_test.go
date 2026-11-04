package termimg

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	tcell "github.com/gdamore/tcell/v3"
)

func pixelFixture(bounds image.Rectangle, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(bounds)
	draw.Draw(img, bounds, image.NewUniform(c), image.Point{}, draw.Src)
	return img
}

func TestPlacementPixelsSourceAndVersion(t *testing.T) {
	pixels := pixelFixture(image.Rect(0, 0, 20, 10), color.NRGBA{R: 210, A: 255})
	p := Placement{Key: "context", Pixels: pixels, PixelVersion: "1", Embedded: "missing", Path: "missing"}
	got, err := loadPlacementImage(p)
	if err != nil || got != pixels {
		t.Fatalf("in-memory source = %v, %v", got, err)
	}
	v := placementImageVersion(p)
	for _, changed := range []Placement{
		{Key: "context", Pixels: pixels, PixelVersion: "2"},
		{Key: "other", Pixels: pixels, PixelVersion: "1"},
		{Key: "context:1", Pixels: pixels, PixelVersion: ""},
		{Embedded: embeddedLogoAsset},
	} {
		if placementImageVersion(changed) == v {
			t.Fatalf("source version aliased %q: %+v", v, changed)
		}
	}
	p.PixelVersion = ""
	if _, err := loadPlacementImage(p); err == nil {
		t.Fatal("unversioned pixels accepted")
	}
	p.PixelVersion, p.Pixels = "empty", image.NewRGBA(image.Rectangle{})
	if _, err := loadPlacementImage(p); err == nil {
		t.Fatal("empty pixels accepted")
	}
}

// An image.Image need not be a pointer or even comparable. Version metadata,
// not interface equality, must decide whether the manager needs a new frame.
type nonComparablePixels struct {
	image.Image
	tags []int
}

func TestPixelVersionDrivesCachingAndPruning(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel, ProtocolRender} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := newTestScreen(t, 30, 10)
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 30, Height: 10, PixelWidth: 300, PixelHeight: 200}}
			m := NewManagerFor(screen, tty, protocol)
			p := Placement{Key: "context", PixelVersion: "1", Cols: 3, Rows: 1,
				Pixels: nonComparablePixels{Image: pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{R: 200, A: 255})}}
			m.Commit(m.Prepare([]Placement{p}))
			if m.ActiveCount() != 1 {
				t.Fatal("in-memory placement not drawn")
			}
			p.Pixels = nonComparablePixels{Image: pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{R: 200, A: 255})}
			if m.Prepare([]Placement{p}) {
				t.Fatal("equivalent pixels with the same version requested a redraw")
			}
			oldVersion := m.desired[0].version
			p.PixelVersion = "2"
			p.Pixels = nonComparablePixels{Image: pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{B: 200, A: 255})}
			if !m.Prepare([]Placement{p}) {
				t.Fatal("changed version did not request a redraw")
			}
			m.Commit(true)
			if m.ActiveCount() != 1 || m.active[0].version == oldVersion {
				t.Fatal("changed pixels did not replace the active image")
			}
			if protocol == ProtocolKitty {
				if len(m.kittyUploads) != 1 || len(m.kittyPrepared) != 0 || !strings.Contains(tty.String(), "a=d,d=I") {
					t.Fatal("replaced image upload was not pruned")
				}
			}
			m.Commit(m.Prepare(nil))
			if m.ActiveCount() != 0 || len(m.kittyUploads) != 0 || len(m.kittyPrepared) != 0 || len(m.sixelCache.entries) != 0 {
				t.Fatal("clearing placements retained image state")
			}
			p.PixelVersion = ""
			m.Commit(m.Prepare([]Placement{p}))
			if m.ActiveCount() != 0 {
				t.Fatal("unversioned image was placed")
			}
			m.Shutdown()
		})
	}
}

func TestPixelPreparationRejectsSupersededAndClosedFrames(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := newTestScreen(t, 30, 10)
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 30, Height: 10, PixelWidth: 300, PixelHeight: 200}}
			m := NewManagerFor(screen, tty, protocol)
			var tasks []func()
			m.runAsync = func(task func()) { tasks = append(tasks, task) }
			p := Placement{Key: "context", PixelVersion: "old", Cols: 3, Rows: 1,
				Pixels: pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{R: 200, A: 255})}
			m.Commit(m.Prepare([]Placement{p}))
			p.Pixels = pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{B: 200, A: 255})
			p.PixelVersion = "new"
			m.Commit(m.Prepare([]Placement{p}))
			if len(tasks) != 2 || m.ActiveCount() != 0 {
				t.Fatalf("queued frames = %d, active = %d", len(tasks), m.ActiveCount())
			}
			tasks[0]()
			if len(m.kittyPrepared) != 0 || len(m.sixelCache.entries) != 0 {
				t.Fatal("stale pixels entered the prepared cache")
			}
			tasks[1]()
			m.Commit(m.Prepare([]Placement{p}))
			if m.ActiveCount() != 1 || m.active[0].PixelVersion != "new" {
				t.Fatal("latest immutable source was not drawn")
			}
			p.PixelVersion = "closing"
			m.Commit(m.Prepare([]Placement{p}))
			m.Shutdown()
			tasks[2]()
			if len(m.kittyPrepared) != 0 || len(m.sixelCache.entries) != 0 || len(m.preparationPending) != 0 || m.ActiveCount() != 0 {
				t.Fatal("completion after shutdown retained image state")
			}
		})
	}
}

func TestPixelPreparationConcurrentReplacement(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := newTestScreen(t, 30, 10)
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 30, Height: 10, PixelWidth: 300, PixelHeight: 200}}
			m := NewManagerFor(screen, tty, protocol)
			var workers sync.WaitGroup
			m.runAsync = func(task func()) {
				workers.Add(1)
				go func() { defer workers.Done(); task() }()
			}
			p := Placement{Key: "context", Cols: 3, Rows: 1}
			for i := range 20 {
				p.Pixels = pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{R: uint8(i * 10), A: 255})
				p.PixelVersion = fmt.Sprint(i)
				m.Commit(m.Prepare([]Placement{p}))
			}
			workers.Wait()
			m.Commit(m.Prepare([]Placement{p}))
			if m.ActiveCount() != 1 || m.active[0].PixelVersion != "19" {
				t.Fatal("latest version missing after concurrent replacement")
			}
			m.Shutdown()
		})
	}
}

func TestPixelSourcesEncodeAndClip(t *testing.T) {
	// Equal source and slot dimensions preserve this nonzero origin through Fit.
	pixels := pixelFixture(image.Rect(7, 11, 47, 51), color.NRGBA{R: 200, A: 255})
	draw.Draw(pixels, image.Rect(7, 31, 47, 51), image.NewUniform(color.NRGBA{B: 200, A: 255}), image.Point{}, draw.Src)
	p := Placement{Key: "context", Pixels: pixels, PixelVersion: "1", X: 1, Y: -1, Cols: 4, Rows: 2,
		Clip: Clip{Y: 1, Cols: 4, Rows: 1}}
	prepared := PrepareKitty(Desired{Placement: p}, 40, 40)
	if prepared.Err != nil {
		t.Fatal(prepared.Err)
	}
	decoded, err := png.Decode(bytes.NewReader(prepared.Data))
	if err != nil || decoded.Bounds() != image.Rect(0, 0, 40, 40) {
		t.Fatalf("prepared PNG = %v, %v", decoded, err)
	}
	if color.NRGBAModel.Convert(decoded.At(0, 30)) != (color.NRGBA{B: 200, A: 255}) {
		t.Fatal("prepared PNG lost source pixels")
	}
	clipped := PrepareSixel(Desired{Placement: p}, 40, 40)
	if clipped.Err != nil {
		t.Fatal(clipped.Err)
	}
	// Cropping the bottom half must produce the same sixel as a zero-origin,
	// solid blue image fitted to that visible half.
	want := p
	want.Pixels = pixelFixture(image.Rect(0, 0, 40, 20), color.NRGBA{B: 200, A: 255})
	want.Clip, want.Rows = Clip{}, 1
	full := PrepareSixel(Desired{Placement: want}, 40, 20)
	if full.Err != nil || !bytes.Equal(clipped.Data, full.Data) {
		t.Fatalf("nonzero-origin clip differs from visible pixels: %v", full.Err)
	}
	screen := newTestScreen(t, 30, 10)
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 30, Height: 10, PixelWidth: 300, PixelHeight: 200}}
	m := NewManagerFor(screen, tty, ProtocolKitty)
	m.Commit(m.Prepare([]Placement{p}))
	if m.ActiveCount() != 1 || !strings.Contains(tty.String(), "x=0,y=20,w=40,h=20,r=1") {
		t.Fatalf("clipped native placement missing: %q", tty.String())
	}
	m.Shutdown()
}

type pixelLockScreen struct {
	tcell.Screen
	unlocks int
}

func (s *pixelLockScreen) LockRegion(x, y, cols, rows int, lock bool) {
	if !lock {
		s.unlocks++
	}
	s.Screen.LockRegion(x, y, cols, rows, lock)
}

func TestPixelAnimationRetainsLastDrawnFrameUntilReplacement(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := &pixelLockScreen{Screen: newTestScreen(t, 30, 10)}
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 30, Height: 10, PixelWidth: 300, PixelHeight: 200}}
			m := NewManagerFor(screen, tty, protocol)
			defer m.Shutdown()
			var tasks []func()
			m.runAsync = func(task func()) { tasks = append(tasks, task) }
			p := Placement{Key: "context", PixelVersion: "1", Cols: 3, Rows: 1,
				Pixels: pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{R: 200, A: 255})}
			m.Commit(m.Prepare([]Placement{p}))
			tasks[0]()
			m.Commit(m.Prepare([]Placement{p}))
			if m.ActiveCount() != 1 {
				t.Fatal("initial frame not drawn")
			}
			old := m.active[0]
			tty.Reset()
			for _, version := range []string{"2", "3"} {
				p.PixelVersion = version
				p.Pixels = pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{B: 200, A: 255})
				changed := m.Prepare([]Placement{p})
				if m.ActiveCount() != 1 || m.active[0].PixelVersion != "1" {
					t.Fatal("Prepare exposed an empty slot while the new frame encoded")
				}
				m.Commit(changed)
				if m.ActiveCount() != 1 || m.active[0].PixelVersion != "1" || screen.unlocks != 0 || tty.Len() != 0 {
					t.Fatal("pending animation replaced or unlocked the last drawn pixels")
				}
				if overlays := m.Placements(); len(overlays) != 1 || overlays[0].Image != old.Pixels {
					t.Fatal("capture did not retain the last visible pixels")
				}
			}
			if len(tasks) != 3 {
				t.Fatalf("preparation tasks = %d, want 3", len(tasks))
			}
			tasks[1]() // Superseded animation frame must not evict the visible one.
			m.Commit(m.Prepare([]Placement{p}))
			if m.ActiveCount() != 1 || m.active[0].PixelVersion != "1" {
				t.Fatal("obsolete completion replaced the retained frame")
			}
			tasks[2]()
			changed := m.Prepare([]Placement{p})
			if protocol == ProtocolKitty && (m.ActiveCount() != 1 || screen.unlocks != 0 || tty.Len() != 0) {
				t.Fatal("Kitty removed old pixels before committing the ready replacement")
			}
			if protocol == ProtocolSixel && (m.ActiveCount() != 0 || screen.unlocks != 1) {
				t.Fatal("Sixel did not unlock ready frame for clearing the previous raster")
			}
			m.Commit(changed)
			if m.ActiveCount() != 1 || m.active[0].PixelVersion != "3" {
				t.Fatal("ready replacement did not atomically become the active frame")
			}
			if protocol == ProtocolKitty {
				placed := strings.Index(tty.String(), "a=p,")
				deleted := strings.Index(tty.String(), string(kittyDeletePlacement(old.imageID, old.placementID)))
				if placed < 0 || deleted <= placed || screen.unlocks != 0 {
					t.Fatalf("replacement was not placed before old deletion: %q", tty.String())
				}
				if _, kept := m.kittyUploads[old.version]; kept || len(m.kittyUploads) != 1 {
					t.Fatal("old upload survived the successful swap")
				}
			}
			// A failed encoder keeps the last good image, without a retry loop;
			// removal still clears it rather than leaving a stale modal behind.
			p.PixelVersion, p.Pixels = "bad", image.NewRGBA(image.Rectangle{})
			m.Commit(m.Prepare([]Placement{p}))
			tasks[3]()
			m.Commit(m.Prepare([]Placement{p}))
			if m.ActiveCount() != 1 || m.active[0].PixelVersion != "3" || len(tasks) != 4 {
				t.Fatal("failed replacement discarded the last good frame or retried")
			}
			m.Commit(m.Prepare(nil))
			if m.ActiveCount() != 0 || len(m.kittyUploads) != 0 {
				t.Fatal("removed animation retained pixels")
			}
		})
	}
}

func TestPixelAnimationRetentionEndsAtLifecycleChanges(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel} {
		for _, change := range []string{"removed", "moved", "resized", "clipped", "key", "terminal geometry", "invalidated", "invalidated and removed", "embedded", "shutdown"} {
			t.Run(protocol.String()+"/"+change, func(t *testing.T) {
				screen := &pixelLockScreen{Screen: newTestScreen(t, 30, 10)}
				tty := &imageTestTTY{window: tcell.WindowSize{Width: 30, Height: 10, PixelWidth: 300, PixelHeight: 200}}
				m := NewManagerFor(screen, tty, protocol)
				defer m.Shutdown()
				var tasks []func()
				m.runAsync = func(task func()) { tasks = append(tasks, task) }
				p := Placement{Key: "context", PixelVersion: "1", Cols: 3, Rows: 1,
					Pixels: pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{R: 200, A: 255})}
				m.Commit(m.Prepare([]Placement{p}))
				tasks[0]()
				m.Commit(m.Prepare([]Placement{p}))
				p.PixelVersion = "2"
				m.Commit(m.Prepare([]Placement{p}))
				if m.ActiveCount() != 1 {
					t.Fatal("pending frame was not retained")
				}
				placements := []Placement{p}
				switch change {
				case "removed":
					placements = nil
				case "moved":
					placements[0].X++
				case "resized":
					placements[0].Cols++
				case "clipped":
					placements[0].Clip = Clip{X: 1, Cols: 2, Rows: 1}
				case "key":
					placements[0].Key = "other"
				case "terminal geometry":
					tty.window.PixelWidth += 30
				case "invalidated":
					m.Invalidate()
				case "invalidated and removed":
					m.Invalidate()
					placements = nil
				case "embedded":
					placements[0].Pixels = nil
					placements[0].Embedded = embeddedLogoAsset
				case "shutdown":
					m.Shutdown()
					placements = nil
				}
				m.Commit(m.Prepare(placements))
				if m.ActiveCount() != 0 || screen.unlocks != 1 || len(m.kittyUploads) != 0 {
					t.Fatal("lifecycle change kept stale pixels or cell locks")
				}
				m.Commit(m.Prepare(nil))
				for _, task := range tasks[1:] {
					task()
				}
				m.Commit(m.Prepare(nil))
				if m.ActiveCount() != 0 || len(m.kittyPrepared) != 0 || len(m.sixelCache.entries) != 0 {
					t.Fatal("late preparation resurrected a closed placement")
				}
			})
		}
	}
}

func TestSixelPixelCompletionBetweenPrepareAndCommitWaitsForClear(t *testing.T) {
	screen := &pixelLockScreen{Screen: newTestScreen(t, 30, 10)}
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 30, Height: 10, PixelWidth: 300, PixelHeight: 200}}
	m := NewManagerFor(screen, tty, ProtocolSixel)
	defer m.Shutdown()
	var tasks []func()
	m.runAsync = func(task func()) { tasks = append(tasks, task) }
	p := Placement{Key: "context", PixelVersion: "1", Cols: 3, Rows: 1,
		Pixels: pixelFixture(image.Rect(0, 0, 30, 20), color.NRGBA{R: 200, A: 255})}
	m.Commit(m.Prepare([]Placement{p}))
	tasks[0]()
	m.Commit(m.Prepare([]Placement{p}))
	p.PixelVersion = "2"
	changed := m.Prepare([]Placement{p})
	tasks[1]()
	m.Commit(changed)
	if m.ActiveCount() != 1 || m.active[0].PixelVersion != "1" || screen.unlocks != 0 {
		t.Fatal("late Sixel preparation painted over an uncleared raster")
	}
	m.Commit(m.Prepare([]Placement{p}))
	if m.ActiveCount() != 1 || m.active[0].PixelVersion != "2" || screen.unlocks != 1 {
		t.Fatal("next frame did not clear and replace the Sixel raster")
	}
}

func TestRenderManagerCapturesPixelPlacement(t *testing.T) {
	screen := newTestScreen(t, 12, 6)
	cw, ch := screenimg.CellSize()
	m := NewRenderManager(screen, 12, 6, cw, ch)
	pixels := pixelFixture(image.Rect(0, 0, 4*cw, 2*ch), color.NRGBA{R: 210, A: 255})
	p := Placement{Key: "context", Pixels: pixels, PixelVersion: "1", X: 2, Y: 1, Cols: 4, Rows: 2,
		Clip: Clip{X: 1, Y: 1, Cols: 3, Rows: 1}}
	m.Commit(m.Prepare([]Placement{p}))
	overlays := m.Placements()
	if len(overlays) != 1 || overlays[0].Image != pixels || overlays[0].Visible != image.Rect(3, 2, 6, 3) {
		t.Fatalf("capture overlays = %+v", overlays)
	}
	shot := screenimg.Render(screen, screenimg.DefaultForeground, screenimg.DefaultBackground, overlays...)
	if got := shot.RGBAAt(3*cw, 2*ch); got != (color.RGBA{R: 210, A: 255}) {
		t.Fatalf("capture did not draw pixel overlay: %v", got)
	}
	if got := shot.RGBAAt(2*cw, ch); got == (color.RGBA{R: 210, A: 255}) {
		t.Fatal("capture drew pixels outside the visible clip")
	}
	m.Commit(m.Prepare(nil))
	if len(m.Placements()) != 0 {
		t.Fatal("cleared capture kept pixels")
	}
}
