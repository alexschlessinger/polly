package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestInlineActivityAddsIndependentImagesViewedControl(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("compare screenshots")
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}
	tui.ShowThinking("compare the two frames")

	for i, dimensions := range [][2]int{{8, 4}, {4, 8}} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("frame-%d.png", i+1))
		writeImageFixture(t, path, dimensions[0], dimensions[1])
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("view-%d", i+1), Name: "view_image"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "attached", time.Millisecond, nil)
		tui.AppendToolMedia(call, inspectionTranscriptImages(testToolImageResult(t, path, call.ID), nil))
	}

	var activity transcriptDisplayBlock
	for _, block := range m.transcriptDisplayEntries(120) {
		if len(block.reasoningIDs) > 0 && len(block.toolDisclosureIDs) > 0 {
			activity = block
			break
		}
	}
	header := plainStyledText(strings.SplitN(activity.text, "\n", 2)[0])
	if !strings.Contains(header, "thought") || !strings.Contains(header, "2 tools · ▸ 2 images viewed") {
		t.Fatalf("three-part activity row = %q", header)
	}
	if len(activity.images) != 0 {
		t.Fatalf("collapsed Images control emitted sidecars: %#v", activity.images)
	}

	rows := m.transcriptRows(120)
	thoughts := m.visibleReasoningPlacements(fullViewport(len(rows), 120))
	tools := m.visibleToolDisclosurePlacements(fullViewport(len(rows), 120))
	images := m.visibleImageDisclosurePlacements(fullViewport(len(rows), 120))
	if len(thoughts) != 1 || len(tools) != 1 || len(images) != 1 || thoughts[0].Y != tools[0].Y || tools[0].Y != images[0].Y ||
		thoughts[0].X+thoughts[0].Cols > tools[0].X || tools[0].X+tools[0].Cols > images[0].X {
		t.Fatalf("three-part activity hitboxes: thought=%#v tools=%#v images=%#v", thoughts, tools, images)
	}
	m.imageDisclosurePlacements = images
	if !m.toggleImageDisclosureAt(images[0].X, images[0].Y) {
		t.Fatal("Images hitbox did not expand")
	}
	record := m.toolDisclosures[activity.toolDisclosureIDs[0]]
	if record == nil || !record.imagesExpanded || record.expanded {
		t.Fatalf("Images expansion changed Tools state: %#v", record)
	}
	for _, block := range m.transcriptDisplayEntries(120) {
		if len(block.toolDisclosureIDs) == 0 || block.toolDisclosureIDs[0] != activity.toolDisclosureIDs[0] {
			continue
		}
		if len(block.images) != 2 || !strings.Contains(plainStyledText(block.text), "▾ 2 images viewed") {
			t.Fatalf("expanded two-image gallery = %#v / %q", block.images, plainStyledText(block.text))
		}
	}

	tui.AppendAssistantText("done")
	r.endTurn(nil)
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if trailer == nil || record.imagesExpanded {
		t.Fatalf("settlement did not collapse Images into a trailer: record=%#v trailer=%#v", record, trailer)
	}
	trailerHeader := plainStyledText(strings.SplitN(m.transcript[trailer.transcriptIndex], "\n", 2)[0])
	if !strings.Contains(trailerHeader, "2 tools · ▸ 2 images viewed") {
		t.Fatalf("settled three-part trailer = %q", trailerHeader)
	}
}

func TestInlineActivityHeadersUseTrailerControls(t *testing.T) {
	if got, want := toolDisclosureHeader(1, false, false), "  "+turnActivityControl("▸", "1 tool"); got != want {
		t.Fatalf("inline tool header = %q, want trailer control %q", got, want)
	}
	if got, want := toolDisclosureHeader(2, true, false), "  "+turnActivityControl("▾", "2 tools"); got != want {
		t.Fatalf("expanded inline tool header = %q, want trailer control %q", got, want)
	}
	if got, want := toolDisclosureHeader(1, false, true), "  "+inlineActivityControl("▸", "1 tool", true); got != want {
		t.Fatalf("completed inline tool header = %q, want muted metadata control %q", got, want)
	}

	m := newReplModel()
	record := &reasoningRecord{complete: true, elapsed: 2 * time.Second}
	if got, want := m.reasoningRecordText(record, 80), "  "+inlineActivityControl("▸", "thought "+formatElapsed(record.elapsed), true); got != want {
		t.Fatalf("inline thought header = %q, want muted metadata control %q", got, want)
	}
}

// Windows' coarse monotonic clock can bank an exactly-zero thinking elapsed,
// which drops the duration from the label — assert the accent span, not the
// timing.
var accentThought = regexp.MustCompile(`\[thought[^\]]*\]\(fg:accent`)

// Smoke: full turn lifecycle with inline activity — live rows visible during
// the turn, trailer appended after, blocks stay inline after settle.
func TestInlineActivitySmoke(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("do work")
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}

	tui.ShowThinking("let me think about this")
	call := messages.ChatMessageToolCall{ID: "c1", Name: "read_file", Arguments: `{"path":"x.go"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})

	// Mid-turn: both blocks visible inline, collapsed. The reasoning run
	// paused when the tool phase began, so it already reads "Thought".
	mid := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	if !strings.Contains(mid, "thought") || !strings.Contains(mid, "1 tool") {
		t.Fatalf("mid-turn transcript missing inline activity: %q", mid)
	}
	var activityLines []string
	for _, line := range strings.Split(mid, "\n") {
		if strings.Contains(line, "thought") || strings.Contains(line, "1 tool") {
			activityLines = append(activityLines, line)
		}
	}
	if len(activityLines) != 1 || !strings.Contains(activityLines[0], "thought") ||
		!strings.Contains(activityLines[0], " · ") || !strings.Contains(activityLines[0], "1 tool") {
		t.Fatalf("inline activity should be one trailer-style row: %#v", activityLines)
	}
	if strings.Contains(mid, "let me think") {
		t.Fatalf("collapsed reasoning leaked detail: %q", mid)
	}
	for _, block := range m.transcriptDisplayEntries(100) {
		if len(block.reasoningIDs) > 0 && len(block.toolDisclosureIDs) > 0 {
			header := strings.SplitN(block.text, "\n", 2)[0]
			if !accentThought.MatchString(header) ||
				!strings.Contains(header, "1 tool](fg:accent") {
				t.Fatalf("one active control should keep the whole activity row blue: %q", header)
			}
		}
	}

	rows := m.transcriptRows(100)
	reasoningHit := m.visibleReasoningPlacements(fullViewport(len(rows), 100))
	toolHit := m.visibleToolDisclosurePlacements(fullViewport(len(rows), 100))
	if len(reasoningHit) != 1 || len(toolHit) != 1 || reasoningHit[0].Y != toolHit[0].Y ||
		reasoningHit[0].X+reasoningHit[0].Cols > toolHit[0].X {
		t.Fatalf("one-line activity controls need distinct same-row hitboxes: thought=%#v tools=%#v", reasoningHit, toolHit)
	}

	tui.AppendToolEnd(call, "file contents", 50*time.Millisecond, nil)
	for _, block := range m.transcriptDisplayEntries(100) {
		if len(block.reasoningIDs) > 0 && len(block.toolDisclosureIDs) > 0 {
			header := strings.SplitN(block.text, "\n", 2)[0]
			if !accentThought.MatchString(header) ||
				!strings.Contains(header, "1 tool](fg:accent") {
				t.Fatalf("activity row greyed between phases before moving on: %q", header)
			}
		}
	}
	tui.AppendAssistantText("All done.")
	tui.RecordTurnTokens(100, 20)
	r.endTurn(nil)

	// Settled: reasoning says "thought", tool block collapsed, trailer present.
	final := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	for _, want := range []string{"thought", "1 tool", "All done.", "✓", "100 in / 20 out"} {
		if !strings.Contains(final, want) {
			t.Fatalf("settled transcript missing %q: %q", want, final)
		}
	}
	for _, block := range m.transcriptDisplayEntries(100) {
		if !block.isActivity() {
			continue
		}
		header := strings.SplitN(block.text, "\n", 2)[0]
		if strings.Contains(header, "fg:accent") || !strings.Contains(header, "fg:muted") {
			t.Fatalf("settled inline activity should use metadata grey: %q", header)
		}
	}
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if trailer == nil || !strings.Contains(strings.SplitN(m.transcript[trailer.transcriptIndex], "\n", 2)[0], "fg:accent") {
		t.Fatalf("final trailer should retain accent controls: %#v", trailer)
	}

	// Expand the settled reasoning inline.
	record := m.reasoningRecords[m.reasoningOrder[0]]
	if !m.toggleReasoning(record.id, 100) {
		t.Fatal("settled reasoning did not toggle")
	}
	expanded := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	if !strings.Contains(expanded, "let me think about this") {
		t.Fatalf("expanded settled reasoning missing tail: %q", expanded)
	}
}

func TestInlineActivityAggregatesUntilAssistantProse(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("ls")
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}

	for i, thought := range []string{"first pass", "second pass"} {
		tui.ShowThinking(thought)
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", 50*time.Millisecond, nil)
	}
	// Unbroken rounds aggregate into one record pair: the same reasoning
	// record resumed across the tool phases, and one disclosure holds both
	// tool rows.
	if len(m.reasoningOrder) != 1 || len(m.turnToolDisclosureIDs) != 1 {
		t.Fatalf("unbroken rounds should share one record pair: reasoning=%v tools=%v", m.reasoningOrder, m.turnToolDisclosureIDs)
	}
	m.reasoningRecords[m.reasoningOrder[0]].elapsed = 700 * time.Millisecond
	m.visual.invalidate()

	visible := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	var activityLines []string
	for _, line := range strings.Split(visible, "\n") {
		if strings.Contains(line, "thought") || strings.Contains(line, "tools") {
			activityLines = append(activityLines, line)
		}
	}
	if len(activityLines) != 1 || !strings.Contains(activityLines[0], "thought 0.7s · ▸ 2 tools") {
		t.Fatalf("uninterrupted activity should aggregate into one row: %#v", activityLines)
	}
	rows := m.transcriptRows(100)
	m.reasoningPlacements = m.visibleReasoningPlacements(fullViewport(len(rows), 100))
	if len(m.reasoningPlacements) != 1 || len(m.reasoningPlacements[0].recordIDs) != 1 {
		t.Fatalf("aggregate thought hitbox = %#v", m.reasoningPlacements)
	}
	p := m.reasoningPlacements[0]
	if !m.toggleReasoningAt(p.X, p.Y, 100) {
		t.Fatal("aggregate thought control did not expand")
	}
	expanded := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	if !strings.Contains(expanded, "first pass") || !strings.Contains(expanded, "second pass") {
		t.Fatalf("aggregate thought expansion omitted a phase: %q", expanded)
	}
	if !m.toggleReasoningAt(p.X, p.Y, 100) {
		t.Fatal("aggregate thought control did not collapse")
	}

	tui.AppendAssistantText("Here is the result.")
	tui.ShowThinking("follow-up phase")
	next := messages.ChatMessageToolCall{ID: "c2", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{next})
	tui.AppendToolEnd(next, "ok", 50*time.Millisecond, nil)
	visible = strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	activityLines = activityLines[:0]
	for _, line := range strings.Split(visible, "\n") {
		if strings.Contains(line, "thought") || strings.Contains(line, "tool") {
			activityLines = append(activityLines, line)
		}
	}
	if len(activityLines) != 2 {
		t.Fatalf("assistant prose should split activity rows: %#v", activityLines)
	}
	var activityHeaders []string
	for _, block := range m.transcriptDisplayEntries(100) {
		if block.isActivity() {
			activityHeaders = append(activityHeaders, strings.SplitN(block.text, "\n", 2)[0])
		}
	}
	if len(activityHeaders) != 2 ||
		!strings.Contains(activityHeaders[0], "thought 0.7s](fg:muted") ||
		!strings.Contains(activityHeaders[0], "2 tools](fg:muted") ||
		!strings.Contains(activityHeaders[1], "fg:accent") {
		t.Fatalf("activity group colors did not follow the prose boundary: %#v", activityHeaders)
	}
}

func TestAggregatedReasoningUsesOneGlobalPreviewBudget(t *testing.T) {
	withDisplayTTY(t)
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}

	for i, prefix := range []string{"older-phase", "newer-phase"} {
		tui.ShowThinking(strings.Repeat(prefix+" ", 24))
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", 10*time.Millisecond, nil)
	}
	ids := append([]int64(nil), m.reasoningOrder...)
	if len(ids) != 1 || !m.toggleReasoningGroup(ids, width) {
		t.Fatalf("aggregated reasoning did not expand as one record: %#v", ids)
	}

	var inlineDetail string
	for _, block := range m.transcriptDisplayEntries(width) {
		if len(block.reasoningIDs) == len(ids) {
			inlineDetail = plainStyledText(block.activityReasoningDetail)
			break
		}
	}
	assertGlobalPreview := func(stage, detail string) {
		t.Helper()
		lines := strings.Split(detail, "\n")
		if len(lines) != reasoningPreviewLines {
			t.Fatalf("%s reasoning rows = %d, want %d: %q", stage, len(lines), reasoningPreviewLines, detail)
		}
		if strings.Contains(detail, "older-phase") || !strings.Contains(detail, "newer-phase") {
			t.Fatalf("%s preview did not retain the newest global tail: %q", stage, detail)
		}
	}
	assertGlobalPreview("inline aggregate", inlineDetail)

	r.endTurn(nil)
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if trailer == nil {
		t.Fatal("settled turn did not attach a trailer")
	}
	trailer.dock.overlay = turnDockOverlayThought
	settledDetail := plainStyledText(m.turnTrailerDetailText(trailer.dock, width))
	assertGlobalPreview("settled trailer", settledDetail)
}

func TestTruncatedInlineActivityKeepsOnlyFullyVisibleHitboxes(t *testing.T) {
	withDisplayTTY(t)
	const width = 18
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}

	tui.ShowThinking("reasoning")
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.reasoningRecords[m.reasoningOrder[0]]
	record.elapsed = 0
	m.visual.invalidate()

	rows := m.transcriptRows(width)
	thoughts := m.visibleReasoningPlacements(fullViewport(len(rows), width))
	tools := m.visibleToolDisclosurePlacements(fullViewport(len(rows), width))
	if len(thoughts) != 1 || thoughts[0].X != 2 || thoughts[0].Cols != 9 {
		t.Fatalf("fully visible thought hitbox = %#v, want x=2 cols=9", thoughts)
	}
	if len(tools) != 0 {
		t.Fatalf("truncated tool control retained an overlapping hitbox: %#v", tools)
	}

	const fullyTruncatedWidth = 8
	rows = m.transcriptRows(fullyTruncatedWidth)
	thoughts = m.visibleReasoningPlacements(fullViewport(len(rows), fullyTruncatedWidth))
	tools = m.visibleToolDisclosurePlacements(fullViewport(len(rows), fullyTruncatedWidth))
	if len(thoughts) != 0 || len(tools) != 0 {
		t.Fatalf("fully truncated controls retained fallback hitboxes: thoughts=%#v tools=%#v", thoughts, tools)
	}
}

func TestActivityGroupTogglesReanchorProjectedVisualBlockOnce(t *testing.T) {
	withDisplayTTY(t)
	const width = 28
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	for i, thought := range []string{
		strings.Repeat("older reasoning detail ", 20),
		strings.Repeat("newer reasoning detail ", 20),
	} {
		tui.ShowThinking(thought)
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("anchor-%d", i), Name: "inspect"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
		// Prose separates the rounds so each keeps its own record pair.
		tui.AppendAssistantText(fmt.Sprintf("phase %d done. ", i))
	}
	ids := append([]int64(nil), m.turnReasoningIDs...)
	if len(ids) != 2 {
		t.Fatalf("fixture reasoning IDs = %v, want 2", ids)
	}

	const sentinel = "viewport anchor sentinel"
	m.appendLine(sentinel)
	rows := transcriptRowsText(m.transcriptRows(width))
	anchor := -1
	for i, row := range rows {
		if strings.Contains(row, sentinel) {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		t.Fatalf("sentinel missing from transcript rows: %#v", rows)
	}
	m.followBottom = false
	m.scrollAnchor = anchor

	assertAnchored := func(stage string) {
		t.Helper()
		rows := transcriptRowsText(m.transcriptRows(width))
		if m.scrollAnchor < 0 || m.scrollAnchor >= len(rows) || !strings.Contains(rows[m.scrollAnchor], sentinel) {
			t.Fatalf("%s moved held viewport: anchor=%d rows=%#v", stage, m.scrollAnchor, rows)
		}
	}
	if !m.toggleReasoningGroup(ids, width) {
		t.Fatal("group expansion returned false")
	}
	assertAnchored("expanding merged reasoning group")
	if !m.toggleReasoningGroup(ids, width) {
		t.Fatal("group collapse returned false")
	}
	assertAnchored("collapsing merged reasoning group")

	toolIDs := append([]int64(nil), m.turnToolDisclosureIDs...)
	if len(toolIDs) != 2 {
		t.Fatalf("fixture tool disclosure IDs = %v, want 2", toolIDs)
	}
	if !m.toggleToolDisclosureGroup(toolIDs) {
		t.Fatal("tool group expansion returned false")
	}
	assertAnchored("expanding merged tool group")
	if !m.toggleToolDisclosureGroup(toolIDs) {
		t.Fatal("tool group collapse returned false")
	}
	assertAnchored("collapsing merged tool group")
}
