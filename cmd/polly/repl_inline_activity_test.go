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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
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
	if !strings.HasPrefix(header, "  ▸ thought") || !strings.Contains(header, "2 tools · 2 images viewed") {
		t.Fatalf("three-part activity row = %q", header)
	}
	if len(activity.images) != 0 {
		t.Fatalf("collapsed Images control emitted sidecars: %#v", activity.images)
	}

	rows := m.transcriptRows(120)
	thoughts := m.visibleDisclosurePlacements(fullViewport(len(rows), 120), activityThought)
	tools := m.visibleDisclosurePlacements(fullViewport(len(rows), 120), activityTools)
	images := m.visibleDisclosurePlacements(fullViewport(len(rows), 120), activityImages)
	if len(thoughts) != 1 || len(tools) != 1 || len(images) != 1 || thoughts[0].Y != tools[0].Y || tools[0].Y != images[0].Y ||
		thoughts[0].X+thoughts[0].Cols > tools[0].X || tools[0].X+tools[0].Cols > images[0].X {
		t.Fatalf("three-part activity hitboxes: thought=%#v tools=%#v images=%#v", thoughts, tools, images)
	}
	m.disclosurePlacements[activityImages] = images
	if !m.toggleDisclosureAt(images[0].X, images[0].Y, 0) {
		t.Fatal("Images hitbox did not expand")
	}
	record := m.toolDisclosures.get(activity.toolDisclosureIDs[0])
	if record == nil || !record.imagesExpanded || record.expanded {
		t.Fatalf("Images expansion changed Tools state: %#v", record)
	}
	for _, block := range m.transcriptDisplayEntries(120) {
		if len(block.toolDisclosureIDs) == 0 || block.toolDisclosureIDs[0] != activity.toolDisclosureIDs[0] {
			continue
		}
		expanded := plainStyledText(strings.SplitN(block.text, "\n", 2)[0])
		if len(block.images) != 2 || !strings.HasPrefix(expanded, "  ▾ ") || !strings.Contains(expanded, "2 images viewed") {
			t.Fatalf("expanded two-image gallery = %#v / %q", block.images, expanded)
		}
	}

	tui.AppendAssistantText("done")
	r.endTurn(nil)
	if record.imagesExpanded {
		t.Fatalf("settlement did not collapse Images: record=%#v", record)
	}
	// The trailer is status only; the images stay on the inline row.
	if trailer := m.turnTrailers.latest(); trailer != nil && strings.Contains(plainStyledText(m.transcript[trailer.transcriptIndex].text), "images") {
		t.Fatalf("settled trailer repeated the activity: %q", plainStyledText(m.transcript[trailer.transcriptIndex].text))
	}
	for _, block := range m.transcriptDisplayEntries(120) {
		if len(block.toolDisclosureIDs) > 0 && block.toolDisclosureIDs[0] == activity.toolDisclosureIDs[0] {
			if header := plainStyledText(strings.SplitN(block.text, "\n", 2)[0]); !strings.Contains(header, "2 tools · 2 images viewed") {
				t.Fatalf("settled inline row = %q", header)
			}
		}
	}
}

// Every activity row shares one shape: an accent triangle, then muted labels.
func TestInlineActivityHeadersShareOneShape(t *testing.T) {
	if got, want := toolDisclosureHeader(1, false), "  "+styled("▸", "accent", "bold")+" "+styled("1 tool", "muted", ""); got != want {
		t.Fatalf("inline tool header = %q, want %q", got, want)
	}
	if got, want := toolDisclosureHeader(2, true), activityRowHeader("▾", "2 tools"); got != want {
		t.Fatalf("expanded inline tool header = %q, want %q", got, want)
	}

	m := newReplModel()
	record := &reasoningRecord{complete: true, elapsed: 2 * time.Second}
	if got, want := m.reasoningRecordText(record, 80), activityRowHeader("▸", "thought "+formatElapsed(record.elapsed)); got != want {
		t.Fatalf("inline thought header = %q, want %q", got, want)
	}
	row, placements := renderActivityRow(false, []turnDockField{
		activityField("thought 0.7s", activityThought, false),
		activityField("2 tools", activityTools, false),
	}, 80)
	if want := activityRowHeader("▸", "thought 0.7s") + styled(" · ", "muted", "") + styled("2 tools", "muted", ""); row != want {
		t.Fatalf("two-field row = %q, want %q", row, want)
	}
	// The triangle belongs to the first hitbox; later ones start at their label.
	if len(placements) != 2 || placements[0] != (turnDockPlacement{kind: activityThought, X: 2, Cols: 14}) || placements[1] != (turnDockPlacement{kind: activityTools, X: 19, Cols: 7}) {
		t.Fatalf("two-field placements = %#v", placements)
	}
}

// Windows' coarse monotonic clock can bank an exactly-zero thinking elapsed,
// which drops the duration from the label — assert the muted span, not the
// timing.
var mutedThought = regexp.MustCompile(`\[thought[^\]]*\]\(fg:muted`)

// Smoke: full turn lifecycle with inline activity — live rows visible during
// the turn, trailer appended after, blocks stay inline after settle.
func TestInlineActivitySmoke(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("do work")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

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
			if !strings.HasPrefix(header, "  "+styled("▸", "accent", "bold")+" ") || !mutedThought.MatchString(header) ||
				!strings.Contains(header, "1 tool](fg:muted") {
				t.Fatalf("activity row should be one accent triangle with muted labels: %q", header)
			}
		}
	}

	rows := m.transcriptRows(100)
	reasoningHit := m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityThought)
	toolHit := m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityTools)
	if len(reasoningHit) != 1 || len(toolHit) != 1 || reasoningHit[0].Y != toolHit[0].Y ||
		reasoningHit[0].X+reasoningHit[0].Cols > toolHit[0].X {
		t.Fatalf("one-line activity controls need distinct same-row hitboxes: thought=%#v tools=%#v", reasoningHit, toolHit)
	}

	tui.AppendToolEnd(call, "file contents", 50*time.Millisecond, nil)
	tui.AppendAssistantText("All done.")
	tui.RecordTurnTokens(100, 20)
	r.endTurn(nil)

	// Settled: reasoning says "thought", tool block collapsed, trailer present.
	m.renderPendingMarkdown()
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
		if !strings.HasPrefix(header, "  "+styled("▸", "accent", "bold")+" ") || !strings.Contains(header, "1 tool](fg:muted") {
			t.Fatalf("settled inline activity keeps its shape: %q", header)
		}
	}
	trailer := m.turnTrailers.latest()
	if trailer == nil || strings.Contains(plainStyledText(m.transcript[trailer.transcriptIndex].text), "tool") {
		t.Fatalf("final trailer should be status only: %#v", trailer)
	}
	if got := plainStyledText(m.transcript[trailer.transcriptIndex].text); !strings.HasPrefix(got, "  ✓ ") || !strings.HasSuffix(got, " · 100 in / 20 out") {
		t.Fatalf("final trailer = %q", got)
	}

	// Expand the settled reasoning inline.
	record := m.reasoningRecords.get(m.reasoningOrder[0])
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

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
	m.reasoningRecords.get(m.reasoningOrder[0]).elapsed = 700 * time.Millisecond
	m.visual.invalidate()

	visible := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	var activityLines []string
	for _, line := range strings.Split(visible, "\n") {
		if strings.Contains(line, "thought") || strings.Contains(line, "tools") {
			activityLines = append(activityLines, line)
		}
	}
	if len(activityLines) != 1 || !strings.Contains(activityLines[0], "▸ thought 0.7s · 2 tools") {
		t.Fatalf("uninterrupted activity should aggregate into one row: %#v", activityLines)
	}
	rows := m.transcriptRows(100)
	m.disclosurePlacements[activityThought] = m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityThought)
	if len(m.disclosurePlacements[activityThought]) != 1 || len(m.disclosurePlacements[activityThought][0].recordIDs) != 1 {
		t.Fatalf("aggregate thought hitbox = %#v", m.disclosurePlacements[activityThought])
	}
	p := m.disclosurePlacements[activityThought][0]
	if !m.toggleDisclosureAt(p.X, p.Y, 100) {
		t.Fatal("aggregate thought control did not expand")
	}
	expanded := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	if !strings.Contains(expanded, "first pass") || !strings.Contains(expanded, "second pass") {
		t.Fatalf("aggregate thought expansion omitted a phase: %q", expanded)
	}
	if !m.toggleDisclosureAt(p.X, p.Y, 100) {
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
		!strings.Contains(activityHeaders[1], "1 tool](fg:muted") {
		t.Fatalf("activity groups did not follow the prose boundary: %#v", activityHeaders)
	}
}

func TestAggregatedReasoningUsesOneGlobalPreviewBudget(t *testing.T) {
	withDisplayTTY(t)
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	for i, prefix := range []string{"older-phase", "newer-phase"} {
		tui.ShowThinking(strings.Repeat(prefix+" ", 24))
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", 10*time.Millisecond, nil)
	}
	ids := append([]int64(nil), m.reasoningOrder...)
	if len(ids) != 1 || !m.toggleDisclosureGroup(activityThought, ids, width) {
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

	// Settling collapses the group; reopening it inline keeps the same budget.
	r.endTurn(nil)
	if !m.toggleDisclosureGroup(activityThought, ids, width) {
		t.Fatal("settled aggregate did not reopen")
	}
	for _, block := range m.transcriptDisplayEntries(width) {
		if len(block.reasoningIDs) == len(ids) {
			assertGlobalPreview("settled aggregate", plainStyledText(block.activityReasoningDetail))
		}
	}
}

func TestTruncatedInlineActivityKeepsOnlyFullyVisibleHitboxes(t *testing.T) {
	withDisplayTTY(t)
	const width = 18
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	tui.ShowThinking("reasoning")
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.reasoningRecords.get(m.reasoningOrder[0])
	record.elapsed = 0
	m.visual.invalidate()

	rows := m.transcriptRows(width)
	thoughts := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityThought)
	tools := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityTools)
	if len(thoughts) != 1 || thoughts[0].X != 2 || thoughts[0].Cols != 9 {
		t.Fatalf("fully visible thought hitbox = %#v, want x=2 cols=9", thoughts)
	}
	if len(tools) != 0 {
		t.Fatalf("truncated tool control retained an overlapping hitbox: %#v", tools)
	}

	const fullyTruncatedWidth = 8
	rows = m.transcriptRows(fullyTruncatedWidth)
	thoughts = m.visibleDisclosurePlacements(fullViewport(len(rows), fullyTruncatedWidth), activityThought)
	tools = m.visibleDisclosurePlacements(fullViewport(len(rows), fullyTruncatedWidth), activityTools)
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

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
	if !m.toggleDisclosureGroup(activityThought, ids, width) {
		t.Fatal("group expansion returned false")
	}
	assertAnchored("expanding merged reasoning group")
	if !m.toggleDisclosureGroup(activityThought, ids, width) {
		t.Fatal("group collapse returned false")
	}
	assertAnchored("collapsing merged reasoning group")

	toolIDs := append([]int64(nil), m.turnToolDisclosureIDs...)
	if len(toolIDs) != 2 {
		t.Fatalf("fixture tool disclosure IDs = %v, want 2", toolIDs)
	}
	if !m.toggleDisclosureGroup(activityTools, toolIDs, 0) {
		t.Fatal("tool group expansion returned false")
	}
	assertAnchored("expanding merged tool group")
	if !m.toggleDisclosureGroup(activityTools, toolIDs, 0) {
		t.Fatal("tool group collapse returned false")
	}
	assertAnchored("collapsing merged tool group")
}
