package main

import (
	"fmt"
	"image"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// scrollBenchmarkTTY exercises the real tcell terminal renderer without a
// terminal emulator or PTY. Output bytes are counted and discarded; input waits
// until shutdown. Each instance is started once and never resumed.
type scrollBenchmarkTTY struct {
	w, h  int
	done  chan struct{}
	once  sync.Once
	bytes atomic.Int64
}

func (t *scrollBenchmarkTTY) Start() error { return nil }
func (t *scrollBenchmarkTTY) Stop() error  { return t.Close() }
func (t *scrollBenchmarkTTY) Drain() error { return t.Close() }
func (t *scrollBenchmarkTTY) Close() error {
	t.once.Do(func() { close(t.done) })
	return nil
}
func (t *scrollBenchmarkTTY) Read([]byte) (int, error) {
	<-t.done
	return 0, io.EOF
}
func (t *scrollBenchmarkTTY) Write(p []byte) (int, error) {
	t.bytes.Add(int64(len(p)))
	return len(p), nil
}
func (t *scrollBenchmarkTTY) NotifyResize(chan<- bool) {}
func (t *scrollBenchmarkTTY) WindowSize() (tcell.WindowSize, error) {
	return tcell.WindowSize{Width: t.w, Height: t.h}, nil
}

type scrollBenchmarkNoFlush struct {
	tcell.Screen
	paintCellAccess
}

func (scrollBenchmarkNoFlush) Show() {}

func newScrollBenchmarkREPL(b *testing.B, w, h int) (*managedREPL, tcell.Screen, *scrollBenchmarkTTY) {
	b.Helper()
	tty := &scrollBenchmarkTTY{w: w, h: h, done: make(chan struct{})}
	screen, err := tcell.NewTerminfoScreenFromTty(tty)
	if err != nil {
		b.Fatal(err)
	}
	if err := screen.Init(); err != nil {
		b.Fatal(err)
	}
	old := ui.DefaultBackend.Screen
	ui.DefaultBackend.Screen = themedScreen{screen}
	b.Cleanup(func() {
		ui.DefaultBackend.Screen = old
		screen.Fini()
	})
	r := newManagedREPL(&Config{}, "scroll-benchmark", 0, 0)
	b.Cleanup(func() { _ = r.closeTabs() })
	r.setupWidgets()
	r.affordanceW = &affordanceLayer{}
	// Fixed history across sizes, with distinct rows so a three-row scroll
	// changes visible text. Most columns in the huge case remain blank.
	var transcript strings.Builder
	for row := range 6000 {
		fmt.Fprintf(&transcript, "row %06d: inspect worker.go and report the completed task with its validation results.\n", row)
	}
	r.model.appendLine(transcript.String())
	r.model.followBottom = false
	r.model.scrollAnchor = 1500
	r.render() // Warm layout, row caches, terminal negotiation, and first paint.
	if gotW, gotH := screen.Size(); gotW != w || gotH != h {
		b.Fatalf("screen size = %dx%d, want %dx%d", gotW, gotH, w, h)
	}
	return r, screen, tty
}

// BenchmarkREPLScroll measures isolated wheel events and eight-event batches.
// It excludes provider activity, native images, and terminal app rasterization.
// Modes are independent measurements, not additive timings.
func BenchmarkREPLScroll(b *testing.B) {
	b.Setenv("TERM", "xterm-256color")
	b.Setenv("COLORTERM", "truecolor")
	b.Setenv("NO_COLOR", "")
	for _, size := range []image.Point{{200, 40}, {500, 250}, {1000, 1000}, {2000, 2500}} {
		b.Run(fmt.Sprintf("%dx%d", size.X, size.Y), func(b *testing.B) {
			for _, mode := range []string{"wheel", "full", "batch-8", "no-flush", "unchanged-show", "new-buffer"} {
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					if mode == "new-buffer" {
						for b.Loop() {
							_ = ui.NewBuffer(image.Rect(0, 0, size.X, size.Y))
						}
						return
					}
					r, screen, tty := newScrollBenchmarkREPL(b, size.X, size.Y)
					b.Cleanup(r.cancelWheelPaint)
					if mode == "no-flush" {
						ui.DefaultBackend.Screen = themedScreen{scrollBenchmarkNoFlush{screen, screen.(paintCellAccess)}}
						r.render() // Warm the new screen wrapper outside the timed loop.
					}
					events := [2]ui.Event{
						{Type: ui.MouseEvent, ID: "<MouseWheelUp>", Payload: ui.Mouse{X: 10, Y: 10}},
						{Type: ui.MouseEvent, ID: "<MouseWheelDown>", Payload: ui.Mouse{X: 10, Y: 10}},
					}
					// Prove the fixture actually scrolls rather than benchmarking
					// a clamped viewport or a wheel event routed to another pane.
					before := r.model.scrollAnchor
					r.handleEvent(events[0])
					if r.model.scrollAnchor != before-3 {
						b.Fatalf("wheel anchor = %d, want %d", r.model.scrollAnchor, before-3)
					}
					r.handleEvent(events[1])
					outputBefore := tty.bytes.Load()
					i := 0
					for b.Loop() {
						if mode == "unchanged-show" {
							screen.Show()
							continue
						}
						if mode == "batch-8" {
							for range 8 {
								r.handlePaintEvent(events[i%2])
								r.paintAfterEvent(events[i%2])
							}
						} else {
							r.handleEvent(events[i%2])
						}
						i++
						if mode != "wheel" {
							r.render()
						}
					}
					b.ReportMetric(float64(tty.bytes.Load()-outputBefore)/float64(b.N), "output-B/op")
					if mode == "batch-8" {
						b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*8), "ns/event")
					}
				})
			}
		})
	}
}
