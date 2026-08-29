package main

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

	"github.com/alexschlessinger/pollytool/messages"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func TestRenderMarkdownWithLocalImages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chart.png")
	writeImageFixture(t, path, 8, 4)

	rendered, images := renderMarkdownWithLocalImages("before\n\n![latency chart](chart.png)\n\nafter", dir)
	if len(images) != 1 {
		t.Fatalf("images = %d, want 1", len(images))
	}
	if images[0].Path != path || images[0].Alt != "latency chart" || images[0].DisplayPath != "chart.png" {
		t.Fatalf("image = %#v", images[0])
	}
	if images[0].Width != 8 || images[0].Height != 4 {
		t.Fatalf("image dimensions = %dx%d, want 8x4", images[0].Width, images[0].Height)
	}
	if got := strings.Count(rendered, string(transcriptImageMarker(0))); got != transcriptImageThumbnailRows {
		t.Fatalf("marker rows = %d, want %d\n%s", got, transcriptImageThumbnailRows, rendered)
	}
	if !strings.Contains(rendered, "image: latency chart · chart.png") {
		t.Fatalf("rendered caption missing: %q", rendered)
	}

	plain := renderMarkdown("![latency chart](chart.png)")
	if strings.ContainsRune(plain, transcriptImageMarker(0)) {
		t.Fatalf("ordinary markdown renderer leaked an image marker: %q", plain)
	}
}

func TestRenderMarkdownWithLocalImagesSanitizesPrivateMarkers(t *testing.T) {
	dir := t.TempDir()
	marker := string(transcriptImageMarker(0))
	filename := "chart" + marker + ".png"
	path := filepath.Join(dir, filename)
	writeImageFixture(t, path, 8, 4)

	rendered, images := renderMarkdownWithLocalImages(
		"before"+marker+"\n\n`code"+marker+"`\n\n![ok]("+filename+")", dir,
	)
	if len(images) != 1 || images[0].Path != path {
		t.Fatalf("resolved images = %#v", images)
	}
	if got := strings.Count(rendered, marker); got != transcriptImageThumbnailRows {
		t.Fatalf("rendered Markdown contains %d marker runes, want %d generated slot rows", got, transcriptImageThumbnailRows)
	}
	plain := plainStyledText(stripTranscriptImageMarkers(rendered))
	if !strings.Contains(plain, "before") || !strings.Contains(plain, "code") {
		t.Fatalf("sanitizing markers damaged source text: %q", plain)
	}
	_, spans := transcriptBlockRowsWithImages(rendered, false, 80, images, true, 10, 20)
	if len(spans) != 1 {
		t.Fatalf("source marker produced %d image spans, want one: %#v", len(spans), spans)
	}
}

func TestRenderMarkdownLeavesRemoteAndMissingImagesAsLinks(t *testing.T) {
	dir := t.TempDir()
	rendered, images := renderMarkdownWithLocalImages("![remote](https://example.com/a.png) ![missing](missing.png)", dir)
	if len(images) != 0 {
		t.Fatalf("images = %#v, want none", images)
	}
	if strings.ContainsRune(rendered, transcriptImageMarker(0)) {
		t.Fatalf("rendered leaked an image marker: %q", rendered)
	}
	if !strings.Contains(rendered, "https://example.com/a.png") || !strings.Contains(rendered, "missing.png") {
		t.Fatalf("fallback links missing: %q", rendered)
	}
}

func TestDiscoverToolOutputImagesIsExplicit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.png", "b.jpg", "c.gif", "d.png", "e.png", "f.png"} {
		writeImageFixture(t, filepath.Join(dir, name), 2, 2)
	}
	body := strings.Join([]string{
		"![plot](a.png)",
		"b.jpg",
		"see c.gif for details",
		"```text",
		"d.png",
		"```",
		`{"path":"e.png"}`,
		"    f.png",
	}, "\n")

	images := discoverToolOutputImages(body, dir)
	if len(images) != 2 {
		t.Fatalf("images = %#v, want Markdown a.png and standalone b.jpg", images)
	}
	if filepath.Base(images[0].Path) != "a.png" || filepath.Base(images[1].Path) != "b.jpg" {
		t.Fatalf("images = %#v", images)
	}
}

func TestTranscriptImageSlotsCollapseWithoutNativeBackend(t *testing.T) {
	img := transcriptImage{Path: "/tmp/chart.png", DisplayPath: "chart.png", Alt: "chart", Width: 8, Height: 4}
	text := renderTranscriptImages([]transcriptImage{img}, "")

	fallbackRows, fallbackSpans := transcriptBlockRowsWithImages(text, false, 80, []transcriptImage{img}, false, 10, 20)
	if len(fallbackRows) != 1 || len(fallbackSpans) != 0 {
		t.Fatalf("fallback rows/spans = %d/%d, want 1/0", len(fallbackRows), len(fallbackSpans))
	}

	nativeRows, nativeSpans := transcriptBlockRowsWithImages(text, false, 80, []transcriptImage{img}, true, 10, 20)
	if len(nativeRows) != 1+transcriptImageThumbnailRows {
		t.Fatalf("native rows = %d, want %d", len(nativeRows), 1+transcriptImageThumbnailRows)
	}
	if len(nativeSpans) != 1 || nativeSpans[0].row != 1 || nativeSpans[0].x != 0 || nativeSpans[0].cols != 40 || nativeSpans[0].rows != transcriptImageThumbnailRows || !nativeSpans[0].fitByRows {
		t.Fatalf("native spans = %#v", nativeSpans)
	}
	for _, row := range nativeRows {
		for _, cell := range row {
			if _, marker := transcriptImageMarkerIndex(cell.Rune); marker {
				t.Fatalf("private marker reached final cells: %#v", cell)
			}
		}
	}
}

func TestAssistantAndToolResultsAttachImageSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.png")
	writeImageFixture(t, path, 4, 4)

	m := newReplModel()
	m.imageBaseDir = dir
	m.appendAssistant("![result](result.png)")
	if len(m.transcriptImages[m.currentAssistant]) != 1 {
		t.Fatalf("assistant sidecar = %#v", m.transcriptImages)
	}
	m.finishAssistantBlock("")

	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.imageBaseDir = dir
	r.model.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}
	call := messages.ChatMessageToolCall{ID: "image-call", Name: "screenshot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, path, time.Millisecond, nil)
	record := r.model.currentToolDisclosure()
	if record == nil || record.expanded || len(record.rows) != 1 || len(record.rows[0].images) != 1 {
		t.Fatalf("collapsed tool image record = %#v", record)
	}
	toolIndex := record.transcriptIndex
	if len(r.model.transcriptImages[toolIndex]) != 0 || strings.Contains(r.model.transcript[toolIndex], "image: result.png") {
		t.Fatalf("collapsed tool image leaked sidecar or caption: text=%q sidecars=%#v", r.model.transcript[toolIndex], r.model.transcriptImages)
	}
	if !r.model.toggleToolDisclosure(record.id) {
		t.Fatal("tool image disclosure did not expand")
	}
	if len(r.model.transcriptImages[toolIndex]) != 1 {
		t.Fatalf("tool sidecar = %#v", r.model.transcriptImages)
	}
	if !strings.Contains(r.model.transcript[toolIndex], "image: result.png") {
		t.Fatalf("tool image caption missing: %q", r.model.transcript[toolIndex])
	}
	toolRows, toolSpans := transcriptBlockRowsWithImages(
		r.model.transcript[toolIndex], false, 80,
		r.model.transcriptImages[toolIndex], true, 10, 20,
	)
	if len(toolSpans) != 1 || toolSpans[0].row < 2 || toolSpans[0].x != 4 ||
		toolSpans[0].rows != 10 || len(toolRows) != toolSpans[0].row+toolSpans[0].rows {
		t.Fatalf("tool image layout rows/spans = %d/%#v", len(toolRows), toolSpans)
	}
}

func TestImageCellGeometryPreservesAspectRatio(t *testing.T) {
	wide := transcriptImage{Path: "/tmp/headcam.png", DisplayPath: "headcam.png", Width: 2400, Height: 270}
	cols, rows, fitByRows := imageCellGeometry(wide, 50, 10, 10, 20)
	if cols != 50 || rows != 3 || fitByRows {
		t.Fatalf("wide geometry = %dx%d fitByRows=%t, want 50x3 width-bound", cols, rows, fitByRows)
	}
	wideRows, wideSpans := transcriptBlockRowsWithImages(
		renderTranscriptImages([]transcriptImage{wide}, ""), false, 80,
		[]transcriptImage{wide}, true, 10, 20,
	)
	if len(wideRows) != 4 || len(wideSpans) != 1 || wideSpans[0].cols != 50 || wideSpans[0].rows != 3 {
		t.Fatalf("wide slot rows/spans = %d/%#v, want caption plus 50x3 slot", len(wideRows), wideSpans)
	}

	square := transcriptImage{Width: 100, Height: 100}
	cols, rows, fitByRows = imageCellGeometry(square, 50, 10, 10, 20)
	if cols != 20 || rows != 10 || !fitByRows {
		t.Fatalf("square geometry = %dx%d fitByRows=%t, want 20x10 height-bound", cols, rows, fitByRows)
	}

	fitted := fitImage(image.NewNRGBA(image.Rect(0, 0, 2400, 270)), 500, 60)
	if got := fitted.Bounds().Size(); got.X != 500 || got.Y != 56 {
		t.Fatalf("fitted pixels = %v, want (500,56)", got)
	}
}

func TestChangedImageAspectReflowsTranscriptSlot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changing.png")
	writeImageFixture(t, path, 2400, 270)
	img, ok := resolveLocalTranscriptImage(path, "changing", "")
	if !ok {
		t.Fatal("wide image did not resolve")
	}
	m := newReplModel()
	m.nativeImages = true
	m.imageCellWidth = 10
	m.imageCellHeight = 20
	m.transcript = []string{renderTranscriptImages([]transcriptImage{img}, "")}
	m.transcriptImages[0] = []transcriptImage{img}
	m.transcriptRows(80)
	if spans := m.visualBlocks[0].imageSpans; len(spans) != 1 || spans[0].cols != 50 || spans[0].rows != 3 || spans[0].fitByRows {
		t.Fatalf("initial wide spans = %#v", spans)
	}

	writeImageFixture(t, path, 270, 2400)
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	m.transcriptRows(80)
	if spans := m.visualBlocks[0].imageSpans; len(spans) != 1 || spans[0].cols != 3 || spans[0].rows != 10 || !spans[0].fitByRows {
		t.Fatalf("changed tall spans = %#v", spans)
	}
}

func TestVisibleImagePlacementsRespectViewport(t *testing.T) {
	m := newReplModel()
	m.nativeImages = true
	m.visualBlocks = []transcriptVisualBlock{{
		key:        "transcript:4",
		rows:       make([][]ui.Cell, 14),
		images:     []transcriptImage{{Path: "/tmp/chart.png"}},
		imageSpans: []transcriptImageSpan{{imageIndex: 0, row: 2, x: 3, cols: 50, rows: 10}},
	}}

	placements := m.visibleImagePlacements(14, 14, 0, 2, 80, false, false)
	if len(placements) != 1 {
		t.Fatalf("placements = %#v", placements)
	}
	got := placements[0]
	if got.Key != "transcript:4:image:0" || got.X != 3 || got.Y != 4 || got.Cols != transcriptImageThumbnailCols || got.Rows != 10 || got.FitByRows {
		t.Fatalf("placement = %#v", got)
	}
	if clipped := m.visibleImagePlacements(14, 8, 0, 0, 80, false, false); len(clipped) != 0 {
		t.Fatalf("partially clipped placement should be omitted: %#v", clipped)
	}
}

func TestDetectTerminalImageProtocol(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want terminalImageProtocol
	}{
		{name: "kitty", env: map[string]string{"KITTY_WINDOW_ID": "1"}, want: terminalImageKitty},
		{name: "ghostty", env: map[string]string{"TERM_PROGRAM": "ghostty"}, want: terminalImageKitty},
		{name: "wezterm", env: map[string]string{"WEZTERM_PANE": "2"}, want: terminalImageKitty},
		{name: "windows terminal", env: map[string]string{"WT_SESSION": "id"}, want: terminalImageSixel},
		{name: "foot", env: map[string]string{"TERM": "foot"}, want: terminalImageSixel},
		{name: "override", env: map[string]string{"POLLYTOOL_IMAGE_PROTOCOL": "sixel", "KITTY_WINDOW_ID": "1"}, want: terminalImageSixel},
		{name: "disabled", env: map[string]string{"POLLYTOOL_IMAGE_PROTOCOL": "none", "KITTY_WINDOW_ID": "1"}, want: terminalImageNone},
		{name: "tmux fallback", env: map[string]string{"TMUX": "/tmp/tmux", "KITTY_WINDOW_ID": "1"}, want: terminalImageNone},
		{name: "tmux ignores forced kitty", env: map[string]string{"TMUX": "/tmp/tmux", "POLLYTOOL_IMAGE_PROTOCOL": "kitty"}, want: terminalImageNone},
		{name: "tmux ignores forced sixel", env: map[string]string{"TMUX": "/tmp/tmux", "POLLYTOOL_IMAGE_PROTOCOL": "sixel"}, want: terminalImageNone},
		{name: "zellij fallback", env: map[string]string{"ZELLIJ": "0", "TERM_PROGRAM": "ghostty"}, want: terminalImageNone},
		{name: "unknown", env: map[string]string{"TERM": "xterm-256color"}, want: terminalImageNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTerminalImageProtocol(func(key string) string { return tt.env[key] })
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

	placed := string(kittyPlaceImage(42, 7, terminalImagePlacement{X: 3, Y: 4, Cols: 50, Rows: 3}))
	if !strings.HasPrefix(placed, "\x1b7\x1b[5;4H") || !strings.Contains(placed, "a=p,i=42,p=7,c=50,C=1,q=2") || strings.Contains(placed, ",r=") || !strings.HasSuffix(placed, "\x1b8") {
		t.Fatalf("placement command = %q", placed)
	}
	heightPlaced := string(kittyPlaceImage(42, 8, terminalImagePlacement{Cols: 20, Rows: 10, FitByRows: true}))
	if !strings.Contains(heightPlaced, "a=p,i=42,p=8,r=10,C=1,q=2") || strings.Contains(heightPlaced, ",c=") {
		t.Fatalf("height-bound placement command = %q", heightPlaced)
	}
}

func TestTerminalImageManagerDrawsKittyAndSixel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 8, 4)
	placement := terminalImagePlacement{Key: "transcript:1:image:0", Path: path, X: 2, Y: 3, Cols: 20, Rows: 5}

	for _, protocol := range []terminalImageProtocol{terminalImageKitty, terminalImageSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(80, 24)
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
			manager := &terminalImageManager{screen: screen, tty: tty, protocol: protocol}

			changed := manager.prepare([]terminalImagePlacement{placement})
			if !changed {
				t.Fatal("first placement did not change the image frame")
			}
			manager.commit(changed)
			if len(manager.active) != 1 {
				t.Fatalf("active placements = %d, want 1", len(manager.active))
			}
			if protocol == terminalImageKitty && !strings.Contains(tty.String(), "\x1b_Ga=t,f=100") {
				t.Fatalf("kitty transmission missing: %q", tty.String())
			}
			if protocol == terminalImageSixel && !strings.Contains(tty.String(), "\x1bP") {
				t.Fatalf("sixel transmission missing: %q", tty.String())
			}
			if manager.prepare([]terminalImagePlacement{placement}) {
				t.Fatal("unchanged placement requested a redraw")
			}
			moved := placement
			moved.Y++
			beforeTransfers := strings.Count(tty.String(), "\x1b_Ga=t,f=100")
			if !manager.prepare([]terminalImagePlacement{moved}) {
				t.Fatal("moved placement did not request a redraw")
			}
			manager.commit(true)
			if protocol == terminalImageKitty && strings.Count(tty.String(), "\x1b_Ga=t,f=100") != beforeTransfers {
				t.Fatal("moving a kitty placement retransmitted unchanged pixels")
			}
			if !manager.prepare(nil) || len(manager.active) != 0 {
				t.Fatalf("clearing placements did not release the active image: %#v", manager.active)
			}
		})
	}
}

func TestTerminalImageManagerPreparesPixelsOffThread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 8, 4)
	placement := terminalImagePlacement{Key: "transcript:1:image:0", Path: path, Cols: 20, Rows: 5}

	for _, protocol := range []terminalImageProtocol{terminalImageKitty, terminalImageSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
			var tasks []func()
			manager := &terminalImageManager{
				screen: screen, tty: tty, protocol: protocol,
				runAsync: func(task func()) { tasks = append(tasks, task) },
				ready:    make(chan struct{}, 1),
			}

			manager.commit(manager.prepare([]terminalImagePlacement{placement}))
			if len(tasks) != 1 || len(manager.active) != 0 || tty.Len() != 0 {
				t.Fatalf("before preparation: tasks=%d active=%d output=%d", len(tasks), len(manager.active), tty.Len())
			}
			moved := placement
			moved.X++
			manager.commit(manager.prepare([]terminalImagePlacement{moved}))
			if len(tasks) != 1 {
				t.Fatalf("moving an in-flight placement scheduled %d preparations, want 1", len(tasks))
			}
			tasks[0]()
			select {
			case <-manager.readyEvents():
			default:
				t.Fatal("completed preparation did not wake the render loop")
			}
			if !manager.prepare([]terminalImagePlacement{moved}) {
				t.Fatal("completed preparation did not dirty the image frame")
			}
			manager.commit(true)
			if len(manager.active) != 1 || tty.Len() == 0 {
				t.Fatalf("after preparation: active=%d output=%d", len(manager.active), tty.Len())
			}
		})
	}
}

func TestTerminalImageManagerConcurrentPreparation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thumb.png")
	writeImageFixture(t, path, 32, 16)
	placement := terminalImagePlacement{Key: "transcript:1:image:0", Path: path, Cols: 20, Rows: 5}

	for _, protocol := range []terminalImageProtocol{terminalImageKitty, terminalImageSixel} {
		t.Run(protocol.String(), func(t *testing.T) {
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
			manager := &terminalImageManager{
				screen: screen, tty: tty, protocol: protocol,
				runAsync: func(task func()) { go task() },
				ready:    make(chan struct{}, 1),
			}

			manager.commit(manager.prepare([]terminalImagePlacement{placement}))
			select {
			case <-manager.readyEvents():
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for image preparation")
			}
			manager.commit(manager.prepare([]terminalImagePlacement{placement}))
			if len(manager.active) != 1 {
				t.Fatalf("active placements = %d, want 1", len(manager.active))
			}
			manager.shutdown()
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
	manager := &terminalImageManager{screen: screen, tty: tty, protocol: terminalImageKitty}
	placements := []terminalImagePlacement{
		{Key: firstKey, Path: path, X: 0, Y: 0, Cols: 10, Rows: 3},
		{Key: secondKey, Path: path, X: 10, Y: 0, Cols: 10, Rows: 3},
	}
	manager.commit(manager.prepare(placements))
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
		cache.put(fmt.Sprintf("image-%d", i), preparedTerminalImage{data: []byte{byte(i)}})
	}
	if _, ok := cache.get("image-0"); !ok {
		t.Fatal("missing cache fixture")
	}
	cache.put("newest", preparedTerminalImage{data: []byte{255}})

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
	placement := terminalImagePlacement{Key: "transcript:1:image:0", Path: path, Cols: 50, Rows: 3}
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 80, Height: 24, PixelWidth: 800, PixelHeight: 480}}
	manager := &terminalImageManager{screen: screen, tty: tty, protocol: terminalImageKitty}
	manager.commit(manager.prepare([]terminalImagePlacement{placement}))

	writeImageFixture(t, path, 270, 2400)
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	tty.Reset()
	if !manager.prepare([]terminalImagePlacement{placement}) {
		t.Fatal("changed image version did not request a redraw")
	}
	manager.commit(true)
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
