package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/images"
	"github.com/alexschlessinger/pollytool/messages"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func TestRenderMarkdownWithLocalImages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chart.png")
	writeImageFixture(t, path, 8, 4)

	rendered, images, _ := markdown.RenderWithLocalImages("before\n\n![latency chart](chart.png)\n\nafter", dir, false)
	if len(images) != 1 {
		t.Fatalf("images = %d, want 1", len(images))
	}
	if images[0].Path != path || images[0].Alt != "latency chart" || images[0].DisplayPath != "chart.png" {
		t.Fatalf("image = %#v", images[0])
	}
	if images[0].Width != 8 || images[0].Height != 4 {
		t.Fatalf("image dimensions = %dx%d, want 8x4", images[0].Width, images[0].Height)
	}
	if got := strings.Count(rendered, string(style.ImageMarker(0))); got != style.ThumbnailRows {
		t.Fatalf("marker rows = %d, want %d\n%s", got, style.ThumbnailRows, rendered)
	}
	if !strings.Contains(rendered, "latency chart · chart.png") {
		t.Fatalf("rendered caption missing: %q", rendered)
	}

	plain := renderMarkdown("![latency chart](chart.png)")
	if strings.ContainsRune(plain, style.ImageMarker(0)) {
		t.Fatalf("ordinary markdown renderer leaked an image marker: %q", plain)
	}
}

func TestRenderMarkdownWithLocalImagesSanitizesPrivateMarkers(t *testing.T) {
	dir := t.TempDir()
	marker := string(style.ImageMarker(0))
	filename := "chart" + marker + ".png"
	path := filepath.Join(dir, filename)
	writeImageFixture(t, path, 8, 4)

	rendered, images, _ := markdown.RenderWithLocalImages(
		"before"+marker+"\n\n`code"+marker+"`\n\n![ok]("+filename+")", dir, false,
	)
	if len(images) != 1 || images[0].Path != path {
		t.Fatalf("resolved images = %#v", images)
	}
	if got := strings.Count(rendered, marker); got != style.ThumbnailRows {
		t.Fatalf("rendered Markdown contains %d marker runes, want %d generated slot rows", got, style.ThumbnailRows)
	}
	plain := plainStyledText(style.StripImageMarkers(rendered))
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	marker := string(style.ImageMarker(0))
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
	if got := strings.Count(activity.text, marker); got != style.ThumbnailRows {
		t.Fatalf("merged activity contains %d slot markers, want %d tool-generated rows", got, style.ThumbnailRows)
	}
	visible := strings.Join(transcriptRowsText(m.transcriptRows(80)), "\n")
	if !strings.Contains(visible, "provider") || !strings.Contains(visible, "reasoning survives") {
		t.Fatalf("tool image consumed expanded reasoning row: %q", visible)
	}
}

func TestRenderMarkdownLeavesRemoteAndMissingImagesAsLinks(t *testing.T) {
	dir := t.TempDir()
	rendered, images, _ := markdown.RenderWithLocalImages("![remote](https://example.com/a.png) ![missing](missing.png)", dir, false)
	if len(images) != 0 {
		t.Fatalf("images = %#v, want none", images)
	}
	if strings.ContainsRune(rendered, style.ImageMarker(0)) {
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

	images := markdown.DiscoverToolOutputImages(body, dir)
	if len(images) != 2 {
		t.Fatalf("images = %#v, want Markdown a.png and standalone b.jpg", images)
	}
	if filepath.Base(images[0].Path) != "a.png" || filepath.Base(images[1].Path) != "b.jpg" {
		t.Fatalf("images = %#v", images)
	}
}

func TestTranscriptImageSlotsCollapseWithoutNativeBackend(t *testing.T) {
	img := style.Image{Path: "/tmp/chart.png", DisplayPath: "chart.png", Alt: "chart", Width: 8, Height: 4}
	text := style.RenderImages([]style.Image{img}, "")

	fallbackRows, fallbackSpans := transcriptBlockRowsWithImages(text, false, 80, []style.Image{img}, false, 10, 20)
	if len(fallbackRows) != 1 || len(fallbackSpans) != 0 {
		t.Fatalf("fallback rows/spans = %d/%d, want 1/0", len(fallbackRows), len(fallbackSpans))
	}

	nativeRows, nativeSpans := transcriptBlockRowsWithImages(text, false, 80, []style.Image{img}, true, 10, 20)
	if len(nativeRows) != 1+style.ThumbnailRows {
		t.Fatalf("native rows = %d, want %d", len(nativeRows), 1+style.ThumbnailRows)
	}
	if len(nativeSpans) != 1 || nativeSpans[0].row != 1 || nativeSpans[0].x != 0 || nativeSpans[0].cols != 40 || nativeSpans[0].rows != style.ThumbnailRows || !nativeSpans[0].fitByRows {
		t.Fatalf("native spans = %#v", nativeSpans)
	}
	for _, row := range nativeRows {
		for _, cell := range row {
			if _, marker := style.ImageMarkerIndex(cell.Rune); marker {
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
	m.renderPendingMarkdown()
	if len(m.transcript[m.currentAssistant].images) != 1 {
		t.Fatalf("assistant sidecar = %#v", m.transcript)
	}
	m.finishAssistantBlock("")

	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.imageBaseDir = dir
	r.model.busy = true
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "image-call", Name: "screenshot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, path, time.Millisecond, nil)
	record := r.model.currentToolDisclosure()
	if record == nil || record.expanded || len(record.rows) != 1 || len(record.rows[0].images) != 1 {
		t.Fatalf("collapsed tool image record = %#v", record)
	}
	toolIndex := record.transcriptIndex
	if len(r.model.transcript[toolIndex].images) != 0 || strings.Contains(r.model.transcript[toolIndex].text, "result.png · ") {
		t.Fatalf("collapsed tool image leaked sidecar or caption: text=%q sidecars=%#v", r.model.transcript[toolIndex].text, r.model.transcript)
	}
	if !r.model.toggleToolDisclosure(record.id) {
		t.Fatal("tool image disclosure did not expand")
	}
	if len(r.model.transcript[toolIndex].images) != 1 {
		t.Fatalf("tool sidecar = %#v", r.model.transcript)
	}
	if !strings.Contains(r.model.transcript[toolIndex].text, "result.png · ") {
		t.Fatalf("tool image caption missing: %q", r.model.transcript[toolIndex].text)
	}
	toolRows, toolSpans := transcriptBlockRowsWithImages(
		r.model.transcript[toolIndex].text, false, 80,
		r.model.transcript[toolIndex].images, true, 10, 20,
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
	if len(images) != 1 || !images[0].Inspection || images[0].MaxCols != style.InspectionThumbnailCols || images[0].MaxRows != style.InspectionThumbnailRows {
		t.Fatalf("inspection images = %#v", images)
	}

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.busy = true
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "view-call", Name: "view_image", Arguments: `{"source":"inspected.png"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "Attached image inspected.png.", time.Millisecond, nil)
	tui.AppendToolMedia(call, images)

	record := r.model.currentToolDisclosure()
	if record == nil || record.expanded || record.imagesExpanded || len(record.rows) != 1 || len(record.rows[0].inspectionImages) != 1 {
		t.Fatalf("collapsed typed-image disclosure = %#v", record)
	}
	toolIndex := record.transcriptIndex
	if got := len(r.model.transcript[toolIndex].images); got != 0 {
		t.Fatalf("collapsed tool entry sidecars = %d, want 0", got)
	}
	if plain := plainStyledText(style.StripImageMarkers(r.model.transcript[toolIndex].text)); strings.Contains(plain, "viewed ·") {
		t.Fatalf("collapsed tool entry leaked inspection detail: %q", plain)
	}

	var collapsed transcriptDisplayBlock
	for _, block := range r.model.transcriptDisplayEntries(100) {
		if len(block.toolDisclosureIDs) > 0 && block.toolDisclosureIDs[0] == record.id {
			collapsed = block
			break
		}
	}
	if plain := plainStyledText(collapsed.text); !strings.Contains(plain, "▸ 1 tool · 1 image viewed") || strings.Contains(plain, "viewed · inspected.png") {
		t.Fatalf("collapsed activity row = %q", plain)
	}
	if len(collapsed.images) != 0 {
		t.Fatalf("collapsed Images disclosure sidecars = %#v", collapsed.images)
	}

	rows := r.model.transcriptRows(100)
	r.model.disclosurePlacements[activityImages] = r.model.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityImages)
	if len(r.model.disclosurePlacements[activityImages]) != 1 {
		t.Fatalf("Images hitboxes = %#v", r.model.disclosurePlacements[activityImages])
	}
	p := r.model.disclosurePlacements[activityImages][0]
	if !r.model.toggleDisclosureAt(p.X, p.Y, 0) {
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
	plain := plainStyledText(style.StripImageMarkers(expanded.text))
	if !strings.Contains(plain, "▾ 1 tool · 1 image viewed") || !strings.Contains(plain, "viewed · inspected.png · 8×4") || !strings.Contains(plain, "│") {
		t.Fatalf("expanded Images disclosure = %q", plain)
	}
	if len(expanded.images) != 1 || strings.Count(expanded.text, string(style.ImageMarker(0))) != style.InspectionThumbnailRows {
		t.Fatalf("expanded inspection sidecars/markers = %#v / %q", expanded.images, expanded.text)
	}
	imageRows, spans := transcriptBlockRowsWithImages(expanded.text, false, 100, expanded.images, true, 10, 20)
	if len(spans) != 1 || spans[0].x != 4 || spans[0].cols != 24 || spans[0].rows != style.InspectionThumbnailRows || len(imageRows) < 1+style.InspectionThumbnailRows {
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
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
	if !m.toggleDisclosureGroup(activityImages, []int64{record.id}, 0) {
		t.Fatal("Images expansion returned false")
	}
	assertHeld("expanding Images")
	if !m.toggleDisclosureGroup(activityImages, []int64{record.id}, 0) {
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "mixed-images", Name: "view_image"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, discoveredPath, time.Millisecond, nil)
	tui.AppendToolMedia(call, inspectionTranscriptImages(testToolImageResult(t, inspectedPath, call.ID), nil))
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) || !m.toggleDisclosureGroup(activityImages, []int64{record.id}, 0) {
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
	if got := strings.Count(activity.text, string(style.ImageMarker(0))); got != style.ThumbnailRows {
		t.Fatalf("tool detail marker rows = %d, want %d", got, style.ThumbnailRows)
	}
	if got := strings.Count(activity.text, string(style.ImageMarker(1))); got != style.InspectionThumbnailRows {
		t.Fatalf("Images gallery marker rows = %d, want %d", got, style.InspectionThumbnailRows)
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
	for _, candidate := range m.toolDisclosures.all() {
		if len(candidate.rows) == 1 && candidate.rows[0].callID == "view-call" {
			record = candidate
			break
		}
	}
	if record == nil || record.expanded || len(record.rows[0].inspectionImages) != 1 {
		t.Fatalf("hydrated inspection disclosure = %#v", record)
	}
	if got := len(m.transcript[record.transcriptIndex].images); got != 0 {
		t.Fatalf("collapsed hydrated tool sidecars = %d, want 0", got)
	}
	activity := func() transcriptDisplayBlock {
		for _, block := range m.transcriptDisplayEntries(80) {
			if len(block.toolDisclosureIDs) > 0 && block.toolDisclosureIDs[0] == record.id {
				return block
			}
		}
		t.Fatal("hydrated image turn has no activity row")
		return transcriptDisplayBlock{}
	}
	header := plainStyledText(strings.SplitN(activity().text, "\n", 2)[0])
	if !strings.Contains(header, "1 tool") || !strings.Contains(header, "1 image viewed") {
		t.Fatalf("hydrated image activity row = %q", header)
	}
	if !m.toggleDisclosureGroup(activityImages, []int64{record.id}, 0) {
		t.Fatal("hydrated Images disclosure did not expand")
	}
	expanded := activity()
	if got := len(expanded.images); got != 1 {
		t.Fatalf("expanded hydrated image sidecars = %d, want 1", got)
	}
	plain := plainStyledText(style.StripImageMarkers(expanded.text))
	if !strings.Contains(plain, "viewed · durable.png · 6×3") || !strings.Contains(plain, "│") {
		t.Fatalf("hydrated inspection gallery = %q", plain)
	}
	if !m.toggleDisclosureGroup(activityImages, []int64{record.id}, 0) || len(activity().images) != 0 {
		t.Fatalf("hydrated Images disclosure did not collapse cleanly: %#v", activity().images)
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
	rendered := style.RenderImages(images, "    ")
	if plain := plainStyledText(rendered); !strings.Contains(plain, "viewed · frame.png") {
		t.Fatalf("fallback inspection receipt = %q", plain)
	}
	if strings.ContainsRune(rendered, style.ImageMarker(0)) {
		t.Fatalf("fallback receipt reserved an unusable image slot: %q", rendered)
	}
}

func TestInspectionCaptionSanitizesToolMediaName(t *testing.T) {
	img := style.Image{Inspection: true, Alt: "[frame]\n\x1b]evil.png"}
	caption := style.ImageCaptionText(img)
	for _, r := range caption {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("control rune survived inspection caption: %q", caption)
		}
	}
	if plain := plainStyledText(style.ImageCaption(img)); !strings.Contains(plain, "[frame]  ]evil.png") {
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

func clearTranscriptForTest(m *replModel) {
	m.transcript = nil
}

// TestTranscriptImagesFollowTheirEntry pins that an entry's images travel
// with it: across a delete ahead of it, through an empty-stream settle, and
// away on clearDisplay.
func TestTranscriptImagesFollowTheirEntry(t *testing.T) {
	m := newReplModel()
	m.appendLine("notice")
	m.appendAssistant("hello")
	// Settle the stream the way production does, so the later empty-stream
	// leg starts a genuinely fresh entry instead of extending this one.
	m.finishAssistantBlock("")
	m.appendQueuedInput(&queuedREPLInput{text: "queued"})

	img := style.Image{Path: "/tmp/x.png", Alt: "x"}
	last := len(m.transcript) - 1
	m.setTranscriptImages(last, []style.Image{img})
	m.deleteTranscriptEntry(0)
	if got := m.transcript[last-1].images; len(got) != 1 || got[0] != img {
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

	m.clearDisplay()
	if len(m.transcript) != 0 {
		t.Fatalf("clearDisplay left %d entries", len(m.transcript))
	}
}

func TestChangedImageAspectReflowsTranscriptSlot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changing.png")
	writeImageFixture(t, path, 2400, 270)
	img, ok := markdown.ResolveLocalImage(path, "changing", "")
	if !ok {
		t.Fatal("wide image did not resolve")
	}
	m := newReplModel()
	m.nativeImages = true
	m.imageCellWidth = 10
	m.imageCellHeight = 20
	m.setTranscriptImages(m.appendTranscriptEntry(style.RenderImages([]style.Image{img}, "")), []style.Image{img})
	m.transcriptRows(80)
	if spans := m.visual.blocks[0].imageSpans; len(spans) != 1 || spans[0].cols != 50 || spans[0].rows != 3 || spans[0].fitByRows {
		t.Fatalf("initial wide spans = %#v", spans)
	}

	writeImageFixture(t, path, 270, 2400)
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	m.transcriptRows(80)
	if spans := m.visual.blocks[0].imageSpans; len(spans) != 1 || spans[0].cols != 3 || spans[0].rows != 10 || !spans[0].fitByRows {
		t.Fatalf("changed tall spans = %#v", spans)
	}
}

func TestVisibleImagePlacementsRespectViewport(t *testing.T) {
	m := newReplModel()
	m.nativeImages = true
	m.visual.blocks = []transcriptVisualBlock{{
		key:        "transcript:4",
		rows:       make([][]ui.Cell, 14),
		images:     []style.Image{{Path: "/tmp/chart.png"}},
		imageSpans: []transcriptImageSpan{{imageIndex: 0, row: 2, x: 3, cols: 50, rows: 10}},
	}}

	placements := m.visibleImagePlacements(frameLayout{width: 80, logoRows: 2, transcriptHeight: 14}.transcriptViewport(14, 0, false, 0))
	if len(placements) != 1 {
		t.Fatalf("placements = %#v", placements)
	}
	got := placements[0]
	if got.Key != "transcript:4:image:0" || got.X != 3 || got.Y != 4 || got.Cols != style.ThumbnailCols || got.Rows != 10 || got.FitByRows {
		t.Fatalf("placement = %#v", got)
	}
	if clipped := m.visibleImagePlacements(frameLayout{width: 80, transcriptHeight: 8}.transcriptViewport(14, 0, false, 0)); len(clipped) != 0 {
		t.Fatalf("partially clipped placement should be omitted: %#v", clipped)
	}
	if covered := m.visibleImagePlacements(frameLayout{width: 80, transcriptHeight: 14}.transcriptViewport(14, 0, false, 3)); len(covered) != 0 {
		t.Fatalf("drawer-covered placement should be omitted: %#v", covered)
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
	img, ok := markdown.ResolveLocalImage(plainSpacePath, "shot", "")
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

	img, ok := markdown.ResolveLocalImage(filepath.Join(dir, "a b.png"), "", "")
	if !ok {
		t.Fatal("exact path did not resolve")
	}
	if img.Width != 64 {
		t.Fatalf("exact match lost to fold match: width = %d, want 64", img.Width)
	}
}

func TestImageCellGeometryPreservesAspectRatio(t *testing.T) {
	wide := style.Image{Path: "/tmp/headcam.png", DisplayPath: "headcam.png", Width: 2400, Height: 270}
	cols, rows, fitByRows := termimg.CellGeometry(wide, 50, 10, 10, 20)
	if cols != 50 || rows != 3 || fitByRows {
		t.Fatalf("wide geometry = %dx%d fitByRows=%t, want 50x3 width-bound", cols, rows, fitByRows)
	}
	wideRows, wideSpans := transcriptBlockRowsWithImages(
		style.RenderImages([]style.Image{wide}, ""), false, 80,
		[]style.Image{wide}, true, 10, 20,
	)
	if len(wideRows) != 4 || len(wideSpans) != 1 || wideSpans[0].cols != 50 || wideSpans[0].rows != 3 {
		t.Fatalf("wide slot rows/spans = %d/%#v, want caption plus 50x3 slot", len(wideRows), wideSpans)
	}

	square := style.Image{Width: 100, Height: 100}
	cols, rows, fitByRows = termimg.CellGeometry(square, 50, 10, 10, 20)
	if cols != 20 || rows != 10 || !fitByRows {
		t.Fatalf("square geometry = %dx%d fitByRows=%t, want 20x10 height-bound", cols, rows, fitByRows)
	}

	fitted := images.Fit(image.NewNRGBA(image.Rect(0, 0, 2400, 270)), 500, 60)
	if got := fitted.Bounds().Size(); got.X != 500 || got.Y != 56 {
		t.Fatalf("fitted pixels = %v, want (500,56)", got)
	}
}

// clearTranscriptForTest empties the transcript, the test-side twin of
// clearDisplay's reset for tests that isolate one command's output without
// disturbing the rest of the model.
