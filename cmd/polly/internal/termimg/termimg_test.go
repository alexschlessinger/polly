package termimg

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tcell "github.com/gdamore/tcell/v3"
)

func TestDetectTerminalImageProtocol(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Protocol
	}{
		{name: "kitty", env: map[string]string{"KITTY_WINDOW_ID": "1"}, want: ProtocolKitty},
		{name: "ghostty", env: map[string]string{"TERM_PROGRAM": "ghostty"}, want: ProtocolKitty},
		{name: "wezterm", env: map[string]string{"WEZTERM_PANE": "2"}, want: ProtocolKitty},
		{name: "windows terminal", env: map[string]string{"WT_SESSION": "id"}, want: ProtocolSixel},
		{name: "foot", env: map[string]string{"TERM": "foot"}, want: ProtocolSixel},
		{name: "override", env: map[string]string{"POLLYTOOL_IMAGE_PROTOCOL": "sixel", "KITTY_WINDOW_ID": "1"}, want: ProtocolSixel},
		{name: "disabled", env: map[string]string{"POLLYTOOL_IMAGE_PROTOCOL": "none", "KITTY_WINDOW_ID": "1"}, want: ProtocolNone},
		{name: "tmux fallback", env: map[string]string{"TMUX": "/tmp/tmux", "KITTY_WINDOW_ID": "1"}, want: ProtocolNone},
		{name: "tmux ignores forced kitty", env: map[string]string{"TMUX": "/tmp/tmux", "POLLYTOOL_IMAGE_PROTOCOL": "kitty"}, want: ProtocolNone},
		{name: "tmux ignores forced sixel", env: map[string]string{"TMUX": "/tmp/tmux", "POLLYTOOL_IMAGE_PROTOCOL": "sixel"}, want: ProtocolNone},
		{name: "zellij fallback", env: map[string]string{"ZELLIJ": "0", "TERM_PROGRAM": "ghostty"}, want: ProtocolNone},
		{name: "unknown", env: map[string]string{"TERM": "xterm-256color"}, want: ProtocolNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectProtocol(func(key string) string { return tt.env[key] })
			if got != tt.want {
				t.Fatalf("protocol = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestKittyCommandsAreChunkedAndPositioned(t *testing.T) {
	data := bytes.Repeat([]byte{0xab}, 5000)
	command := string(kittyTransmitPNG(42, data))
	parts := strings.Split(command, "\x1b\\")
	if len(parts) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(parts)-1)
	}
	for i, part := range parts[:len(parts)-1] {
		semicolon := strings.IndexByte(part, ';')
		if semicolon < 0 {
			t.Fatalf("chunk %d has no payload delimiter: %q", i, part)
		}
		if payload := part[semicolon+1:]; len(payload) > 4096 {
			t.Fatalf("chunk %d payload = %d bytes", i, len(payload))
		}
	}
	if !strings.Contains(parts[0], "a=t,f=100,t=d,i=42,q=2,m=1") || !strings.Contains(parts[len(parts)-2], "m=0") {
		t.Fatalf("unexpected kitty chunks: %q", command)
	}

	placed := string(kittyPlaceImage(42, 7, Placement{X: 3, Y: 4, Cols: 50, Rows: 3}))
	if !strings.HasPrefix(placed, "\x1b7\x1b[5;4H") || !strings.Contains(placed, "a=p,i=42,p=7,c=50,C=1,q=2") || strings.Contains(placed, ",r=") || !strings.HasSuffix(placed, "\x1b8") {
		t.Fatalf("placement command = %q", placed)
	}
	heightPlaced := string(kittyPlaceImage(42, 8, Placement{Cols: 20, Rows: 10, FitByRows: true}))
	if !strings.Contains(heightPlaced, "a=p,i=42,p=8,r=10,C=1,q=2") || strings.Contains(heightPlaced, ",c=") {
		t.Fatalf("height-bound placement command = %q", heightPlaced)
	}
}

func TestTerminalImageManagerDrawsKittyAndSixel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 8, 4)
	placement := Placement{Key: "transcript:1:image:0", Path: path, X: 2, Y: 3, Cols: 20, Rows: 5}

	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(80, 24)
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
			manager := &Manager{screen: screen, tty: tty, protocol: protocol}

			changed := manager.Prepare([]Placement{placement})
			if !changed {
				t.Fatal("first placement did not change the image frame")
			}
			manager.Commit(changed)
			if len(manager.active) != 1 {
				t.Fatalf("active placements = %d, want 1", len(manager.active))
			}
			if protocol == ProtocolKitty && !strings.Contains(tty.String(), "\x1b_Ga=t,f=100") {
				t.Fatalf("kitty transmission missing: %q", tty.String())
			}
			if protocol == ProtocolSixel && !strings.Contains(tty.String(), "\x1bP") {
				t.Fatalf("sixel transmission missing: %q", tty.String())
			}
			if manager.Prepare([]Placement{placement}) {
				t.Fatal("unchanged placement requested a redraw")
			}
			moved := placement
			moved.Y++
			beforeTransfers := strings.Count(tty.String(), "\x1b_Ga=t,f=100")
			if !manager.Prepare([]Placement{moved}) {
				t.Fatal("moved placement did not request a redraw")
			}
			manager.Commit(true)
			if protocol == ProtocolKitty && strings.Count(tty.String(), "\x1b_Ga=t,f=100") != beforeTransfers {
				t.Fatal("moving a kitty placement retransmitted unchanged pixels")
			}
			if !manager.Prepare(nil) || len(manager.active) != 0 {
				t.Fatalf("clearing placements did not release the active image: %#v", manager.active)
			}
		})
	}
}

func TestTerminalImageManagerPreparesPixelsOffThread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 8, 4)
	placement := Placement{Key: "transcript:1:image:0", Path: path, Cols: 20, Rows: 5}

	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
			var tasks []func()
			manager := &Manager{
				screen: screen, tty: tty, protocol: protocol,
				runAsync: func(task func()) { tasks = append(tasks, task) },
				ready:    make(chan struct{}, 1),
			}

			manager.Commit(manager.Prepare([]Placement{placement}))
			if len(tasks) != 1 || len(manager.active) != 0 || tty.Len() != 0 {
				t.Fatalf("before preparation: tasks=%d active=%d output=%d", len(tasks), len(manager.active), tty.Len())
			}
			moved := placement
			moved.X++
			manager.Commit(manager.Prepare([]Placement{moved}))
			if len(tasks) != 1 {
				t.Fatalf("moving an in-flight placement scheduled %d preparations, want 1", len(tasks))
			}
			tasks[0]()
			select {
			case <-manager.ReadyEvents():
			default:
				t.Fatal("completed preparation did not wake the render loop")
			}
			if !manager.Prepare([]Placement{moved}) {
				t.Fatal("completed preparation did not dirty the image frame")
			}
			manager.Commit(true)
			if len(manager.active) != 1 || tty.Len() == 0 {
				t.Fatalf("after preparation: active=%d output=%d", len(manager.active), tty.Len())
			}
		})
	}
}

func TestTerminalImageManagerConcurrentPreparation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 32, 16)
	placement := Placement{Key: "transcript:1:image:0", Path: path, Cols: 20, Rows: 5}

	for _, protocol := range []Protocol{ProtocolKitty, ProtocolSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
			manager := &Manager{
				screen: screen, tty: tty, protocol: protocol,
				runAsync: func(task func()) { go task() },
				ready:    make(chan struct{}, 1),
			}

			manager.Commit(manager.Prepare([]Placement{placement}))
			select {
			case <-manager.ReadyEvents():
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for image preparation")
			}
			manager.Commit(manager.Prepare([]Placement{placement}))
			if len(manager.active) != 1 {
				t.Fatalf("active placements = %d, want 1", len(manager.active))
			}
			manager.Shutdown()
		})
	}
}

func TestKittyPlacementIDsProbeHashCollisions(t *testing.T) {
	const firstKey = "I7osYYO9nZRS"
	const secondKey = "r6EQsMhzbXtS"
	if stableTerminalImageID("placement:"+firstKey) != stableTerminalImageID("placement:"+secondKey) {
		t.Fatal("test fixture no longer collides")
	}

	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 8, 4)
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
	manager := &Manager{screen: screen, tty: tty, protocol: ProtocolKitty}
	placements := []Placement{
		{Key: firstKey, Path: path, X: 0, Y: 0, Cols: 10, Rows: 3},
		{Key: secondKey, Path: path, X: 10, Y: 0, Cols: 10, Rows: 3},
	}
	manager.Commit(manager.Prepare(placements))
	if len(manager.active) != 2 {
		t.Fatalf("active placements = %d, want 2", len(manager.active))
	}
	if manager.active[0].placementID == manager.active[1].placementID {
		t.Fatalf("colliding placement IDs were not probed: %d", manager.active[0].placementID)
	}
}

func TestTerminalImageLRUEvictsOnlyOldestEntry(t *testing.T) {
	var cache terminalImageLRU
	for i := 0; i < maxSixelCacheEntries; i++ {
		cache.put(fmt.Sprintf("image-%d", i), Prepared{Data: []byte{byte(i)}})
	}
	if _, ok := cache.get("image-0"); !ok {
		t.Fatal("missing cache fixture")
	}
	cache.put("newest", Prepared{Data: []byte{255}})

	if len(cache.entries) != maxSixelCacheEntries {
		t.Fatalf("cache entries = %d, want %d", len(cache.entries), maxSixelCacheEntries)
	}
	if _, ok := cache.get("image-1"); ok {
		t.Fatal("least-recently-used entry was retained")
	}
	if _, ok := cache.get("image-0"); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := cache.get("newest"); !ok {
		t.Fatal("new entry was evicted")
	}
}

func TestKittyReloadConstrainsChangedAspectToReservedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changing.png")
	writeImageFixture(t, path, 2400, 270)
	placement := Placement{Key: "transcript:1:image:0", Path: path, Cols: 50, Rows: 3}
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
	manager := &Manager{screen: screen, tty: tty, protocol: ProtocolKitty}
	manager.Commit(manager.Prepare([]Placement{placement}))

	writeImageFixture(t, path, 270, 2400)
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	tty.Reset()
	if !manager.Prepare([]Placement{placement}) {
		t.Fatal("changed image version did not request a redraw")
	}
	manager.Commit(true)
	command := tty.String()
	if !strings.Contains(command, ",r=3,C=1") || strings.Contains(command, ",c=50,C=1") {
		t.Fatalf("changed tall image was not height-constrained: %q", command)
	}
}

type imageTestTTY struct {
	bytes.Buffer
	window tcell.WindowSize
}

func (t *imageTestTTY) Start() error                          { return nil }
func (t *imageTestTTY) Stop() error                           { return nil }
func (t *imageTestTTY) Drain() error                          { return nil }
func (t *imageTestTTY) NotifyResize(chan<- bool)              {}
func (t *imageTestTTY) WindowSize() (tcell.WindowSize, error) { return t.window, nil }
func (t *imageTestTTY) Close() error                          { return nil }

func writeImageFixture(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(20 * x), G: uint8(30 * y), B: 180, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, img); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
