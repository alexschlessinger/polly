package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/images"
	"github.com/alexschlessinger/pollytool/messages"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func TestRenderMarkdownWithLocalImages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chart.png")
	writeImageFixture(t, path, 8, 4)

	rendered, images, _ := renderMarkdownWithLocalImages("before\n\n![latency chart](chart.png)\n\nafter", dir, false)
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

	rendered, images, _ := renderMarkdownWithLocalImages(
		"before"+marker+"\n\n`code"+marker+"`\n\n![ok]("+filename+")", dir, false,
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

func TestExpandedReasoningCannotClaimAdjacentToolImage(t *testing.T) {
	withDisplayTTY(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 8, 4)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.imageBaseDir = dir
	m.beginTurn("inspect image")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	marker := string(transcriptImageMarker(0))
	tui.ShowThinking("provider " + marker + " reasoning survives")
	reasoning := m.currentReasoningRecord()
	if reasoning == nil || !m.toggleReasoning(reasoning.id, 80) {
		t.Fatal("reasoning disclosure did not expand")
	}

	call := messages.ChatMessageToolCall{ID: "image", Name: "screenshot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, path, time.Millisecond, nil)
	tool := m.currentToolDisclosure()
	if tool == nil || !m.toggleToolDisclosure(tool.id) {
		t.Fatal("tool disclosure did not expand")
	}

	var activity transcriptDisplayBlock
	for _, block := range m.transcriptDisplayEntries(80) {
		if len(block.reasoningIDs) > 0 && len(block.toolDisclosureIDs) > 0 {
			activity = block
			break
		}
	}
	if len(activity.images) != 1 {
		t.Fatalf("merged activity images = %#v, want one tool image", activity.images)
	}
	if strings.Contains(plainStyledText(strings.SplitN(activity.text, "\n", 2)[0]), "image viewed") {
		t.Fatalf("path-discovered tool output was promoted to Images viewed: %q", plainStyledText(activity.text))
	}
	if got := strings.Count(activity.text, marker); got != transcriptImageThumbnailRows {
		t.Fatalf("merged activity contains %d slot markers, want %d tool-generated rows", got, transcriptImageThumbnailRows)
	}
	visible := strings.Join(transcriptRowsText(m.transcriptRows(80)), "\n")
	if !strings.Contains(visible, "provider") || !strings.Contains(visible, "reasoning survives") {
		t.Fatalf("tool image consumed expanded reasoning row: %q", visible)
	}
}

func TestRenderMarkdownLeavesRemoteAndMissingImagesAsLinks(t *testing.T) {
	dir := t.TempDir()
	rendered, images, _ := renderMarkdownWithLocalImages("![remote](https://example.com/a.png) ![missing](missing.png)", dir, false)
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

func TestTypedToolImageUsesIndependentCollapsedDisclosure(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "inspected.png")
	writeImageFixture(t, path, 8, 4)
	result := testToolImageResult(t, path, "view-call")
	images := inspectionTranscriptImages(result, nil)
	if len(images) != 1 || !images[0].Inspection || images[0].MaxCols != inspectionImageThumbnailCols || images[0].MaxRows != inspectionImageThumbnailRows {
		t.Fatalf("inspection images = %#v", images)
	}

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}
	call := messages.ChatMessageToolCall{ID: "view-call", Name: "view_image", Arguments: `{"source":"inspected.png"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "Attached image inspected.png.", time.Millisecond, nil)
	tui.AppendToolMedia(call, images)

	record := r.model.currentToolDisclosure()
	if record == nil || record.expanded || record.imagesExpanded || len(record.rows) != 1 || len(record.rows[0].inspectionImages) != 1 {
		t.Fatalf("collapsed typed-image disclosure = %#v", record)
	}
	toolIndex := record.transcriptIndex
	if got := len(r.model.transcriptImages[toolIndex]); got != 0 {
		t.Fatalf("collapsed tool entry sidecars = %d, want 0", got)
	}
	if plain := plainStyledText(stripTranscriptImageMarkers(r.model.transcript[toolIndex])); strings.Contains(plain, "viewed ·") {
		t.Fatalf("collapsed tool entry leaked inspection detail: %q", plain)
	}

	var collapsed transcriptDisplayBlock
	for _, block := range r.model.transcriptDisplayEntries(100) {
		if len(block.toolDisclosureIDs) > 0 && block.toolDisclosureIDs[0] == record.id {
			collapsed = block
			break
		}
	}
	if plain := plainStyledText(collapsed.text); !strings.Contains(plain, "1 tool · ▸ 1 image viewed") || strings.Contains(plain, "viewed · inspected.png") {
		t.Fatalf("collapsed activity row = %q", plain)
	}
	if len(collapsed.images) != 0 {
		t.Fatalf("collapsed Images disclosure sidecars = %#v", collapsed.images)
	}

	rows := r.model.transcriptRows(100)
	r.model.imageDisclosurePlacements = r.model.visibleImageDisclosurePlacements(len(rows), len(rows), 0, 0, 100, false, 0)
	if len(r.model.imageDisclosurePlacements) != 1 {
		t.Fatalf("Images hitboxes = %#v", r.model.imageDisclosurePlacements)
	}
	p := r.model.imageDisclosurePlacements[0]
	if !r.model.toggleImageDisclosureAt(p.X, p.Y) {
		t.Fatal("Images control did not expand")
	}
	if !record.imagesExpanded || record.expanded {
		t.Fatalf("image/tool expansion was not independent: %#v", record)
	}

	var expanded transcriptDisplayBlock
	for _, block := range r.model.transcriptDisplayEntries(100) {
		if len(block.toolDisclosureIDs) > 0 && block.toolDisclosureIDs[0] == record.id {
			expanded = block
			break
		}
	}
	plain := plainStyledText(stripTranscriptImageMarkers(expanded.text))
	if !strings.Contains(plain, "▾ 1 image viewed") || !strings.Contains(plain, "viewed · inspected.png · 8×4") || !strings.Contains(plain, "│") {
		t.Fatalf("expanded Images disclosure = %q", plain)
	}
	if len(expanded.images) != 1 || strings.Count(expanded.text, string(transcriptImageMarker(0))) != inspectionImageThumbnailRows {
		t.Fatalf("expanded inspection sidecars/markers = %#v / %q", expanded.images, expanded.text)
	}
	imageRows, spans := transcriptBlockRowsWithImages(expanded.text, false, 100, expanded.images, true, 10, 20)
	if len(spans) != 1 || spans[0].x != 4 || spans[0].cols != 24 || spans[0].rows != inspectionImageThumbnailRows || len(imageRows) < 1+inspectionImageThumbnailRows {
		t.Fatalf("compact inspection geometry rows/spans = %d/%#v", len(imageRows), spans)
	}
	if !r.model.toggleToolDisclosure(record.id) || !record.expanded || !record.imagesExpanded {
		t.Fatalf("opening Tools changed Images state: %#v", record)
	}
}

func TestImagesDisclosureTogglePreservesHeldViewport(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const width = 32
	path := filepath.Join(t.TempDir(), "anchor.png")
	writeImageFixture(t, path, 8, 4)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "anchor-view", Name: "view_image"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "attached", time.Millisecond, nil)
	tui.AppendToolMedia(call, inspectionTranscriptImages(testToolImageResult(t, path, call.ID), nil))
	record := m.currentToolDisclosure()
	if record == nil {
		t.Fatal("image tool did not create a disclosure")
	}

	const sentinel = "held viewport sentinel"
	m.appendLine(sentinel)
	m.reasoningWidth = width
	rows := transcriptRowsText(m.transcriptRows(width))
	anchor := -1
	for i, row := range rows {
		if strings.Contains(row, sentinel) {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		t.Fatalf("sentinel missing from rows: %#v", rows)
	}
	m.followBottom = false
	m.scrollAnchor = anchor
	assertHeld := func(stage string) {
		t.Helper()
		rows := transcriptRowsText(m.transcriptRows(width))
		if m.scrollAnchor < 0 || m.scrollAnchor >= len(rows) || !strings.Contains(rows[m.scrollAnchor], sentinel) {
			t.Fatalf("%s moved held viewport: anchor=%d rows=%#v", stage, m.scrollAnchor, rows)
		}
	}
	if !m.toggleImageDisclosureGroup([]int64{record.id}) {
		t.Fatal("Images expansion returned false")
	}
	assertHeld("expanding Images")
	if !m.toggleImageDisclosureGroup([]int64{record.id}) {
		t.Fatal("Images collapse returned false")
	}
	assertHeld("collapsing Images")
}

func TestToolAndImagesDisclosuresKeepIndependentImageMarkers(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	discoveredPath := filepath.Join(dir, "tool-output.png")
	inspectedPath := filepath.Join(dir, "model-viewed.png")
	writeImageFixture(t, discoveredPath, 8, 4)
	writeImageFixture(t, inspectedPath, 4, 8)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.imageBaseDir = dir
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "mixed-images", Name: "view_image"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, discoveredPath, time.Millisecond, nil)
	tui.AppendToolMedia(call, inspectionTranscriptImages(testToolImageResult(t, inspectedPath, call.ID), nil))
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) || !m.toggleImageDisclosureGroup([]int64{record.id}) {
		t.Fatalf("mixed image disclosures did not expand: %#v", record)
	}

	var activity transcriptDisplayBlock
	for _, block := range m.transcriptDisplayEntries(100) {
		if len(block.toolDisclosureIDs) > 0 && block.toolDisclosureIDs[0] == record.id {
			activity = block
			break
		}
	}
	if len(activity.images) != 2 || activity.images[0].Path != discoveredPath || activity.images[1].Path == discoveredPath {
		t.Fatalf("tool/inspection sidecar order = %#v", activity.images)
	}
	if got := strings.Count(activity.text, string(transcriptImageMarker(0))); got != transcriptImageThumbnailRows {
		t.Fatalf("tool detail marker rows = %d, want %d", got, transcriptImageThumbnailRows)
	}
	if got := strings.Count(activity.text, string(transcriptImageMarker(1))); got != inspectionImageThumbnailRows {
		t.Fatalf("Images gallery marker rows = %d, want %d", got, inspectionImageThumbnailRows)
	}
	_, spans := transcriptBlockRowsWithImages(activity.text, false, 100, activity.images, true, 10, 20)
	if len(spans) != 2 || spans[0].imageIndex != 0 || spans[1].imageIndex != 1 || spans[0].x != 4 || spans[1].x != 4 || spans[0].row >= spans[1].row {
		t.Fatalf("tool/inspection image spans = %#v", spans)
	}
}

func TestHydratedToolImageRestoresImagesViewedDisclosure(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "durable.png")
	writeImageFixture(t, path, 6, 3)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store := testArtifactStore(t)
	ref, err := store.Put(context.Background(), artifacts.Blob{
		Kind: artifacts.KindImage, MIMEType: "image/png", Name: "durable.png", Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	toolResult := messages.ChatMessage{
		Role: messages.MessageRoleTool, ToolCallID: "view-call", ToolName: "view_image",
		Content: "Attached image durable.png.",
		Parts: []messages.ContentPart{{
			Type: "image_artifact", MimeType: ref.MIMEType, FileName: ref.Name, Artifact: &ref,
		}},
	}
	toolResult.SetToolSucceeded(true)

	m := newReplModel()
	m.artifactStore = store
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "inspect it"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "view-call", Name: "view_image"}}},
		toolResult,
		{Role: messages.MessageRoleAssistant, Content: "done"},
	}, "ctx")

	var record *toolDisclosureRecord
	for _, candidate := range m.toolDisclosures {
		if len(candidate.rows) == 1 && candidate.rows[0].callID == "view-call" {
			record = candidate
			break
		}
	}
	if record == nil || record.expanded || len(record.rows[0].inspectionImages) != 1 {
		t.Fatalf("hydrated inspection disclosure = %#v", record)
	}
	if got := len(m.transcriptImages[record.transcriptIndex]); got != 0 {
		t.Fatalf("collapsed hydrated tool sidecars = %d, want 0", got)
	}
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if trailer == nil {
		t.Fatal("hydrated image turn did not restore its trailer")
	}
	header := plainStyledText(strings.SplitN(m.transcript[trailer.transcriptIndex], "\n", 2)[0])
	if !strings.Contains(header, "1 tool") || !strings.Contains(header, "1 image viewed") {
		t.Fatalf("hydrated image trailer = %q", header)
	}
	if !m.toggleTurnTrailerOverlay(trailer, turnDockOverlayImages) {
		t.Fatal("hydrated Images trailer did not expand")
	}
	if got := len(m.transcriptImages[trailer.transcriptIndex]); got != 1 {
		t.Fatalf("expanded hydrated image sidecars = %d, want 1", got)
	}
	plain := plainStyledText(stripTranscriptImageMarkers(m.transcript[trailer.transcriptIndex]))
	if !strings.Contains(plain, "viewed · durable.png · 6×3") || !strings.Contains(plain, "│") {
		t.Fatalf("hydrated inspection gallery = %q", plain)
	}
	if !m.toggleTurnTrailerOverlay(trailer, turnDockOverlayImages) || len(m.transcriptImages[trailer.transcriptIndex]) != 0 {
		t.Fatalf("hydrated Images trailer did not collapse cleanly: %#v", m.transcriptImages[trailer.transcriptIndex])
	}
}

func TestTypedToolImageKeepsReceiptWhenPreviewCannotMaterialize(t *testing.T) {
	result := messages.ChatMessage{Parts: []messages.ContentPart{{
		Type: "image_url", ImageURL: "https://example.invalid/frame.png", FileName: "frame.png",
	}}}
	images := inspectionTranscriptImages(result, nil)
	if len(images) != 1 || !images[0].Inspection || images[0].Path != "" {
		t.Fatalf("fallback inspection images = %#v", images)
	}
	rendered := renderTranscriptImages(images, "    ")
	if plain := plainStyledText(rendered); !strings.Contains(plain, "viewed · frame.png") {
		t.Fatalf("fallback inspection receipt = %q", plain)
	}
	if strings.ContainsRune(rendered, transcriptImageMarker(0)) {
		t.Fatalf("fallback receipt reserved an unusable image slot: %q", rendered)
	}
}

func TestInspectionCaptionSanitizesToolMediaName(t *testing.T) {
	img := transcriptImage{Inspection: true, Alt: "[frame]\n\x1b]evil.png"}
	caption := transcriptImageCaptionText(img)
	for _, r := range caption {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("control rune survived inspection caption: %q", caption)
		}
	}
	if plain := plainStyledText(transcriptImageCaption(img)); !strings.Contains(plain, "[frame]  ]evil.png") {
		t.Fatalf("literal brackets were not preserved: %q", plain)
	}
}

func testToolImageResult(t *testing.T, path, callID string) messages.ChatMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		ToolCallID: callID,
		ToolName:   "view_image",
		Content:    "Attached image " + filepath.Base(path) + ".",
		Parts: []messages.ContentPart{{
			Type:      "image_base64",
			ImageData: base64.StdEncoding.EncodeToString(data),
			MimeType:  "image/png",
			FileName:  filepath.Base(path),
		}},
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

	fitted := images.Fit(image.NewNRGBA(image.Rect(0, 0, 2400, 270)), 500, 60)
	if got := fitted.Bounds().Size(); got.X != 500 || got.Y != 56 {
		t.Fatalf("fitted pixels = %v, want (500,56)", got)
	}
}

// TestTranscriptImageLaneLockstep pins the documented invariant: the image
// lane grows, shrinks, and resets only alongside the transcript.
// clearTranscriptForTest empties both transcript lanes together, the test-side
// twin of clearDisplay's lane reset for tests that isolate one command's
// output without disturbing the rest of the model.
func clearTranscriptForTest(m *replModel) {
	m.transcript = nil
	m.transcriptImages = nil
}

func TestTranscriptImageLaneLockstep(t *testing.T) {
	m := newReplModel()
	check := func(step string) {
		t.Helper()
		if len(m.transcriptImages) != len(m.transcript) {
			t.Fatalf("%s: image lane len %d, transcript len %d", step, len(m.transcriptImages), len(m.transcript))
		}
	}
	check("empty model")
	m.appendLine("notice")
	check("appendLine")
	m.appendAssistant("hello")
	check("appendAssistant")
	// Settle the stream the way production does, so the later empty-stream
	// leg starts a genuinely fresh entry instead of extending this one.
	m.finishAssistantBlock("")
	check("finishAssistantBlock")
	m.appendQueuedInput(&queuedREPLInput{text: "queued"})
	check("appendQueuedInput")

	img := transcriptImage{Path: "/tmp/x.png", Alt: "x"}
	last := len(m.transcript) - 1
	m.setTranscriptImages(last, []transcriptImage{img})
	m.deleteTranscriptEntry(0)
	check("deleteTranscriptEntry")
	if got := m.transcriptImages[last-1]; len(got) != 1 || got[0] != img {
		t.Fatalf("images did not follow their entry across the delete: %#v", got)
	}

	// An empty assistant stream deletes its transcript entry on settle.
	before := len(m.transcript)
	m.appendAssistant("\n")
	if len(m.transcript) != before+1 {
		t.Fatalf("empty-stream leg did not append a fresh entry")
	}
	m.finishAssistantBlock("")
	if len(m.transcript) != before {
		t.Fatalf("empty-stream settle did not delete its entry")
	}
	check("finishAssistantBlock empty-stream delete")

	m.clearDisplay()
	check("clearDisplay")
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
	m.setTranscriptImages(m.appendTranscriptEntry(renderTranscriptImages([]transcriptImage{img}, "")), []transcriptImage{img})
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

	placements := m.visibleImagePlacements(14, 14, 0, 2, 80, false, 0)
	if len(placements) != 1 {
		t.Fatalf("placements = %#v", placements)
	}
	got := placements[0]
	if got.Key != "transcript:4:image:0" || got.X != 3 || got.Y != 4 || got.Cols != transcriptImageThumbnailCols || got.Rows != 10 || got.FitByRows {
		t.Fatalf("placement = %#v", got)
	}
	if clipped := m.visibleImagePlacements(14, 8, 0, 0, 80, false, 0); len(clipped) != 0 {
		t.Fatalf("partially clipped placement should be omitted: %#v", clipped)
	}
	if covered := m.visibleImagePlacements(14, 14, 0, 0, 80, false, 3); len(covered) != 0 {
		t.Fatalf("drawer-covered placement should be omitted: %#v", covered)
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

func TestResolveLocalTranscriptImageFoldsUnicodeSpaces(t *testing.T) {
	dir := t.TempDir()
	// macOS screenshot names use U+202F (narrow no-break space) before AM/PM;
	// model output normalizes it to U+0020, so the emitted path never
	// byte-matches the file on disk.
	realName := "Screenshot 2026-08-26 at 10.09.27\u202FPM.png"
	realPath := filepath.Join(dir, realName)
	writeImageFixture(t, realPath, 64, 32)

	plainSpacePath := filepath.Join(dir, "Screenshot 2026-08-26 at 10.09.27 PM.png")
	img, ok := resolveLocalTranscriptImage(plainSpacePath, "shot", "")
	if !ok {
		t.Fatal("plain-space spelling did not resolve to U+202F file")
	}
	if img.Path != realPath {
		t.Fatalf("resolved path = %q, want %q", img.Path, realPath)
	}
	if img.Width != 64 || img.Height != 32 {
		t.Fatalf("dims = %dx%d, want 64x32", img.Width, img.Height)
	}
}

func TestResolveLocalTranscriptImageExactMatchWinsOverFold(t *testing.T) {
	dir := t.TempDir()
	writeImageFixture(t, filepath.Join(dir, "a b.png"), 64, 32)
	writeImageFixture(t, filepath.Join(dir, "a\u202Fb.png"), 128, 16)

	img, ok := resolveLocalTranscriptImage(filepath.Join(dir, "a b.png"), "", "")
	if !ok {
		t.Fatal("exact path did not resolve")
	}
	if img.Width != 64 {
		t.Fatalf("exact match lost to fold match: width = %d, want 64", img.Width)
	}
}

func TestResolveSpaceFoldedPathMissReturnsInput(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.png")
	if got := resolveSpaceFoldedPath(missing); got != missing {
		t.Fatalf("resolveSpaceFoldedPath(missing) = %q, want input unchanged", got)
	}
}
