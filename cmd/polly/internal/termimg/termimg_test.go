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

	placed := string(kittyPlaceImage(42, 7, Placement{X: 3, Y: 4, Cols: 50, Rows: 3}, 500, 60))
	if !strings.HasPrefix(placed, "\x1b7\x1b[5;4H") || !strings.Contains(placed, "a=p,i=42,p=7,c=50,C=1,q=2") || strings.Contains(placed, ",r=") || !strings.HasSuffix(placed, "\x1b8") {
		t.Fatalf("placement command = %q", placed)
	}
	heightPlaced := string(kittyPlaceImage(42, 8, Placement{Cols: 20, Rows: 10, FitByRows: true}, 60, 200))
	if !strings.Contains(heightPlaced, "a=p,i=42,p=8,r=10,C=1,q=2") || strings.Contains(heightPlaced, ",c=") {
		t.Fatalf("height-bound placement command = %q", heightPlaced)
	}
}

func TestClipSourceRect(t *testing.T) {
	tests := []struct {
		name                    string
		pixelWidth, pixelHeight int
		cols, rows              int
		clip                    Clip
		want                    image.Rectangle
	}{
		{name: "unclipped", pixelWidth: 200, pixelHeight: 100, cols: 20, rows: 5, want: image.Rect(0, 0, 200, 100)},
		{name: "top two rows", pixelWidth: 200, pixelHeight: 100, cols: 20, rows: 5, clip: Clip{Y: 2, Cols: 20, Rows: 3}, want: image.Rect(0, 40, 200, 100)},
		{name: "bottom three rows", pixelWidth: 200, pixelHeight: 100, cols: 20, rows: 5, clip: Clip{Cols: 20, Rows: 3}, want: image.Rect(0, 0, 200, 60)},
		{name: "right half", pixelWidth: 200, pixelHeight: 100, cols: 20, rows: 5, clip: Clip{X: 10, Cols: 10, Rows: 5}, want: image.Rect(100, 0, 200, 100)},
		{name: "last row of a tall image", pixelWidth: 30, pixelHeight: 200, cols: 3, rows: 10, clip: Clip{Y: 9, Cols: 3, Rows: 1}, want: image.Rect(0, 180, 30, 200)},
		{name: "unknown pixels", pixelWidth: 0, pixelHeight: 0, cols: 20, rows: 5, clip: Clip{Cols: 20, Rows: 3}, want: image.Rectangle{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clipSourceRect(tt.pixelWidth, tt.pixelHeight, tt.cols, tt.rows, tt.clip)
			if got != tt.want {
				t.Fatalf("clipSourceRect = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCropToClipKeepsVisiblePixels(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 4, 6))
	for y := range 6 {
		for x := range 4 {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(y * 10), A: 255})
		}
	}
	cropped := cropToClip(src, image.Rect(0, 4, 4, 6))
	if bounds := cropped.Bounds(); bounds.Dx() != 4 || bounds.Dy() != 2 {
		t.Fatalf("cropped bounds = %v, want 4x2", bounds)
	}
	got := color.NRGBAModel.Convert(cropped.At(1, 0)).(color.NRGBA)
	if got.R != 40 {
		t.Fatalf("cropped top row kept the wrong source row: %+v", got)
	}
	if whole := cropToClip(src, image.Rectangle{}); whole != src {
		t.Fatal("an empty crop must leave the source untouched")
	}
	if uncut := cropToClip(src, src.Bounds()); uncut != src {
		t.Fatal("a full crop must leave the source untouched")
	}
}

func TestKittyClippedPlacementCropsToVisibleCells(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 8, 4)
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(80, 24)
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
	manager := &Manager{screen: screen, tty: tty, protocol: ProtocolKitty}

	// The slot starts above the pane: only its last three rows are on screen.
	placement := Placement{
		Key: "transcript:1:image:0", Path: path,
		X: 2, Y: 3, Cols: 20, Rows: 5,
		Clip: Clip{X: 0, Y: 2, Cols: 20, Rows: 3},
	}
	manager.Commit(manager.Prepare([]Placement{placement}))
	if len(manager.active) != 1 {
		t.Fatalf("active placements = %d, want 1", len(manager.active))
	}
	placed := tty.String()
	// The uploaded image is the full slot (200x100 pixels), so the visible
	// slice is its lower 60 pixels, drawn at the first on-screen row.
	if !strings.Contains(placed, "a=p,i=") || !strings.Contains(placed, "x=0,y=40,w=200,h=60,c=20,r=3,C=1,q=2") {
		t.Fatalf("clipped placement command = %q", placed)
	}
	if !strings.Contains(placed, "\x1b7\x1b[6;3H") {
		t.Fatalf("clipped placement was not pinned to the visible rows: %q", placed)
	}

	// Scrolling one more row re-crops the placement but reuses the upload.
	tty.Reset()
	placement.Clip.Y++
	placement.Clip.Rows--
	if !manager.Prepare([]Placement{placement}) {
		t.Fatal("clipped placement change did not request a redraw")
	}
	manager.Commit(true)
	scrolled := tty.String()
	if strings.Contains(scrolled, "a=t,f=100") {
		t.Fatalf("scrolling retransmitted pixels: %q", scrolled)
	}
	if !strings.Contains(scrolled, "x=0,y=60,w=200,h=40,c=20,r=2,C=1,q=2") {
		t.Fatalf("scrolled placement command = %q", scrolled)
	}
}

func TestSixelClippedPlacementEncodesVisibleSlice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 240, 100)
	full := Desired{Placement: Placement{Key: "transcript:1:image:0", Path: path, X: 2, Y: 3, Cols: 20, Rows: 5}}
	clipped := full
	clipped.Clip = Clip{Y: 2, Cols: 20, Rows: 3}

	whole := PrepareSixel(full, 20*10, 5*20)
	if whole.Err != nil || len(whole.Data) == 0 {
		t.Fatalf("unclipped sixel = %#v", whole.Err)
	}
	if whole.PixelWidth != 200 || whole.PixelHeight != 83 {
		t.Fatalf("fitted pixels = %dx%d, want 200x83", whole.PixelWidth, whole.PixelHeight)
	}
	slice := PrepareSixel(clipped, 20*10, 5*20)
	if slice.Err != nil || len(slice.Data) == 0 {
		t.Fatalf("clipped sixel = %#v", slice.Err)
	}
	if bytes.Equal(slice.Data, whole.Data) || len(slice.Data) >= len(whole.Data) {
		t.Fatalf("clipped sixel did not encode a smaller slice: %d vs %d bytes", len(slice.Data), len(whole.Data))
	}
	if sixelImageCacheKey(clipped, 10, 20) == sixelImageCacheKey(full, 10, 20) {
		t.Fatal("clipped sixel shares the cache key of the whole image")
	}

	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(80, 24)
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
	manager := &Manager{screen: screen, tty: tty, protocol: ProtocolSixel}
	manager.Commit(manager.Prepare([]Placement{clipped.Placement}))
	if len(manager.active) != 1 {
		t.Fatalf("active placements = %d, want 1", len(manager.active))
	}
	if !strings.Contains(tty.String(), "\x1b7\x1b[6;3H") {
		t.Fatalf("clipped sixel was not pinned to the visible rows: %q", tty.String()[:min(200, tty.Len())])
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
