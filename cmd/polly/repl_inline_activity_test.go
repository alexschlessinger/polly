package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestExpandedActivityBoundarySpacing(t *testing.T) {
	for _, tc := range []struct {
		name, before, activity, after, want string
	}{
		{"expanded", "prompt", "header\ndetail", "answer", "prompt\n\nheader\ndetail\n\nanswer"},
		{"collapsed", "prompt", "header", "answer", "prompt\nheader\nanswer"},
		{"existing spacing", "prompt\n", "header\ndetail", "\nanswer", "prompt\n\nheader\ndetail\n\nanswer"},
		{"empty neighbors", "", "header\ndetail", "", "\nheader\ndetail\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []transcriptDisplayBlock{
				{text: tc.before},
				{text: tc.activity, reasoningIDs: []int64{1}},
				{text: tc.after},
			}
			spaceExpandedActivityBlocks(blocks)
			got := blocks[0].text + "\n" + blocks[1].text + "\n" + blocks[2].text
			if got != tc.want {
				t.Fatalf("spacing = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInlineActivityLineOpensImagesWithIt(t *testing.T) {
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
	if !strings.HasPrefix(header, "  ▸ thought") || !strings.HasSuffix(header, " · 2 images viewed") {
		t.Fatalf("three-part activity row = %q", header)
	}
	if len(activity.images) != 0 {
		t.Fatalf("collapsed Images control emitted sidecars: %#v", activity.images)
	}

	rows := m.transcriptRows(120)
	hits := m.visibleDisclosurePlacements(fullViewport(len(rows), 120), activityAll)
	if len(hits) != 1 || len(hits[0].thoughtIDs) != 1 || len(hits[0].recordIDs) != 1 {
		t.Fatalf("three-part activity line wants one hitbox: %#v", hits)
	}
	m.disclosurePlacements[activityAll] = hits
	if !m.toggleDisclosureAt(hits[0].X, hits[0].Y, 0) {
		t.Fatal("activity line did not expand")
	}
	// One line, one control: the thought, the rows and the images open
	// together.
	record := m.toolDisclosures.get(activity.toolDisclosureIDs[0])
	if record == nil || !record.imagesExpanded || !record.expanded || !m.reasoningRecords.get(activity.reasoningIDs[0]).expanded {
		t.Fatalf("line toggle left a disclosure closed: %#v", record)
	}
	for _, block := range m.transcriptDisplayEntries(120) {
		if len(block.toolDisclosureIDs) == 0 || block.toolDisclosureIDs[0] != activity.toolDisclosureIDs[0] {
			continue
		}
		expanded := plainStyledText(strings.SplitN(block.text, "\n", 2)[0])
		if len(block.images) != 2 || !strings.HasPrefix(expanded, "  ▾ thought") || !strings.HasSuffix(expanded, "· 2 images viewed") {
			t.Fatalf("expanded two-image gallery = %#v / %q", block.images, expanded)
		}
	}

	tui.AppendAssistantText("done")
	r.endTurn(nil)
	if record.imagesExpanded {
		t.Fatalf("settlement did not collapse Images: record=%#v", record)
	}
	// The settled turn rides on the line; nothing repeats the activity.
	if trailer := m.turnTrailers.latest(); trailer != nil {
		t.Fatalf("settled turn with activity left a trailer row: %q", plainStyledText(m.transcript[trailer.transcriptIndex].text))
	}
	for _, block := range m.transcriptDisplayEntries(120) {
		if len(block.toolDisclosureIDs) > 0 && block.toolDisclosureIDs[0] == activity.toolDisclosureIDs[0] {
			if header := plainStyledText(strings.SplitN(block.text, "\n", 2)[0]); !strings.Contains(header, "▸ thought") || !strings.Contains(header, "· 2 images viewed") || strings.Contains(header, "tool") {
				t.Fatalf("settled inline row = %q", header)
			}
		}
	}
}

// Every activity row shares one shape: an accent triangle, then muted labels.
func TestInlineActivityHeadersShareOneShape(t *testing.T) {
	if got, want := toolDisclosureHeader(1, false), "  "+style.Styled("▸", "accent", "bold")+" "+style.Styled("1 tool", "muted", ""); got != want {
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
	row, placements, labels := renderActivityRow([]turnDockField{
		activityField("thought 0.7s", activityThought, false),
		activityField("2 tools", activityTools, true),
	}, turnDockField{}, 80)
	// One triangle for the line, down while any of its disclosures is open;
	// the fields follow it joined by dots.
	if want := activityRowHeader("▾", "thought 0.7s") + style.Styled(" · ", "muted", "") + style.Styled("2 tools", "muted", ""); row != want {
		t.Fatalf("two-field row = %q, want %q", row, want)
	}
	// The whole line is one hitbox; the labels keep their own geometry for
	// painting.
	if len(placements) != 1 || placements[0] != (turnDockPlacement{kind: activityAll, X: 2, Cols: 24}) {
		t.Fatalf("two-field placements = %#v", placements)
	}
	if len(labels) != 2 || labels[0] != (turnDockPlacement{kind: activityThought, X: 4, Cols: 12}) || labels[1] != (turnDockPlacement{kind: activityTools, X: 19, Cols: 7}) {
		t.Fatalf("two-field labels = %#v", labels)
	}
	// The right field sits against the right edge, two columns clear.
	row, placements, _ = renderActivityRow([]turnDockField{
		activityField("thought 0.7s", activityThought, false),
	}, turnDockField{raw: "34.0s", rendered: style.Styled("34.0s", "muted", "")}, 40)
	if plain := plainStyledText(row); plain != "  ▸ thought 0.7s"+strings.Repeat(" ", 19)+"34.0s" {
		t.Fatalf("row with right field = %q", plain)
	}
	if len(placements) != 1 || placements[0] != (turnDockPlacement{kind: activityAll, X: 2, Cols: 14}) {
		t.Fatalf("right-field placements = %#v", placements)
	}
	// A clipped line keeps its triangle and the right field, and its hitbox
	// stops at the ellipsis.
	row, placements, _ = renderActivityRow([]turnDockField{
		activityField("thought 0.7s", activityThought, false),
		activityField("2 tools", activityTools, false),
	}, turnDockField{raw: "1m 2s", rendered: style.Styled("1m 2s", "muted", "")}, 30)
	if plain := plainStyledText(row); plain != "  ▸ thought 0.7s · 2 t…  1m 2s" || !strings.Contains(row, style.Styled("▸", "accent", "bold")) {
		t.Fatalf("clipped row = %q (%q)", plain, row)
	}
	if len(placements) != 1 || placements[0] != (turnDockPlacement{kind: activityAll, X: 2, Cols: 21}) {
		t.Fatalf("clipped placements = %#v", placements)
	}
}

func TestInlineActivityRunningLabelsPreserveLiteralText(t *testing.T) {
	withDisplayTTY(t)
	for _, tc := range []struct {
		name, tool, arg, value, want string
	}{
		{"unmatched command bracket", "bash", "command", "grep '[' file", "$ grep '[' file"},
		{"command markup", "bash", "command", "printf '[ok](fg:err)'", "$ printf '[ok](fg:err)'"},
		{"balanced command brackets", "bash", "command", "[ -f foo ] && sleep 30", "$ [ -f foo ] && sleep 30"},
		{"unmatched path bracket", "read_file", "path", "src/[draft.go", "read src/[draft.go"},
		{"path markup", "read_file", "path", "[draft](fg:err).go", "read [draft](fg:err).go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("work")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			args, err := json.Marshal(map[string]string{tc.arg: tc.value})
			if err != nil {
				t.Fatal(err)
			}
			call := messages.ChatMessageToolCall{ID: "tool", Name: tc.tool, Arguments: string(args)}
			// A following styled field must stay intact even when the command
			// or path contains an unmatched bracket.
			tui.AppendToolStart([]messages.ChatMessageToolCall{call, agentCall("agent", `{"label":"Trace sessions"}`)})
			got := strings.Join(transcriptRowsText(m.transcriptRows(160)), "\n")
			want := "  ▸ running " + tc.want + " · 1 agent running"
			if !strings.HasSuffix(got, want) {
				t.Fatalf("running activity = %q, want suffix %q", got, want)
			}
		})
	}
}

func TestInlineActivityToolEndRefreshesCollapsedHeader(t *testing.T) {
	withDisplayTTY(t)
	for _, tc := range []struct {
		name, result, detail string
		err                  error
	}{
		{"denied", llm.ToolDeniedContent, "denied", nil},
		{"failed", "", "failed", fmt.Errorf("command failed")},
		{"canceled", "", "canceled", context.Canceled},
		{"completed", "ok", "1 line", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const width = 160
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("work")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			first := messages.ChatMessageToolCall{ID: "first", Name: "bash", Arguments: `{"command":"rm file"}`}
			second := messages.ChatMessageToolCall{ID: "second", Name: "bash", Arguments: `{"command":"sleep 30"}`}
			tui.AppendToolStart([]messages.ChatMessageToolCall{first, second})
			before := strings.Join(transcriptRowsText(m.transcriptRows(width)), "\n")
			if !strings.HasSuffix(before, "  ▸ running $ rm file") {
				t.Fatalf("initial activity = %q", before)
			}
			// Denied calls receive OnToolEnd without OnToolResult. The header
			// must refresh at settlement, not wait for the result callback.
			tui.AppendToolEnd(first, tc.result, 0, tc.err)
			m.refreshActiveTools()
			m.refreshStreamCursor()
			m.refreshReasoningRecords(width)
			after := strings.Join(transcriptRowsText(m.transcriptRows(width)), "\n")
			if want := "  ▸ ran 1 command · running $ sleep 30"; !strings.HasSuffix(after, want) {
				t.Fatalf("settled activity = %q, want suffix %q", after, want)
			}
			tui.AppendToolEnd(second, llm.ToolDeniedContent, 0, nil)
			after = strings.Join(transcriptRowsText(m.transcriptRows(width)), "\n")
			// Neither one nor several unsuccessful calls add a failure count.
			if !strings.HasSuffix(after, "  ▸ ran 2 commands") {
				t.Fatalf("completed batch activity = %q", after)
			}
			record := m.currentToolDisclosure()
			if !m.toggleToolDisclosure(record.id) {
				t.Fatal("tool disclosure did not open")
			}
			expanded := strings.Join(transcriptRowsText(m.transcriptRows(width)), "\n")
			if !strings.Contains(expanded, "$ rm file · "+tc.detail) || !strings.Contains(expanded, "✗ $ sleep 30 · denied") {
				t.Fatalf("expanded activity lost individual outcomes: %q", expanded)
			}
		})
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
	if !strings.Contains(mid, "thought") || !strings.Contains(mid, "running read x.go") {
		t.Fatalf("mid-turn transcript missing inline activity: %q", mid)
	}
	var activityLines []string
	for _, line := range strings.Split(mid, "\n") {
		if strings.Contains(line, "thought") || strings.Contains(line, "running read x.go") {
			activityLines = append(activityLines, line)
		}
	}
	if len(activityLines) != 1 || !strings.Contains(activityLines[0], "thought") ||
		!strings.Contains(activityLines[0], " · ") || !strings.Contains(activityLines[0], "running read x.go") {
		t.Fatalf("inline activity should be one line: %#v", activityLines)
	}
	if strings.Contains(mid, "let me think") {
		t.Fatalf("collapsed reasoning leaked detail: %q", mid)
	}
	for _, block := range m.transcriptDisplayEntries(100) {
		if len(block.reasoningIDs) > 0 && len(block.toolDisclosureIDs) > 0 {
			header := strings.SplitN(block.text, "\n", 2)[0]
			if !strings.HasPrefix(header, "  "+style.Styled("▸", "accent", "bold")+" ") || !mutedThought.MatchString(header) ||
				!strings.Contains(header, "running ](fg:muted") {
				t.Fatalf("activity line should be one accent triangle with muted labels: %q", header)
			}
		}
	}

	rows := m.transcriptRows(100)
	hits := m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityAll)
	if len(hits) != 1 || len(hits[0].thoughtIDs) != 1 || len(hits[0].recordIDs) != 1 {
		t.Fatalf("one-line activity wants one hitbox over both records: %#v", hits)
	}

	tui.AppendToolEnd(call, "file contents", 50*time.Millisecond, nil)
	tui.AppendAssistantText("All done.")
	tui.RecordTurnTokens(100, 20, false)
	r.endTurn(nil)

	// Settled: reasoning says "thought", the tool row is counted by verb,
	// and the turn's time sits at the line's right edge. Tokens are
	// /context's to report.
	m.renderPendingMarkdown()
	final := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	for _, want := range []string{"thought", "read 1 file", "All done."} {
		if !strings.Contains(final, want) {
			t.Fatalf("settled transcript missing %q: %q", want, final)
		}
	}
	if strings.Contains(final, "100 in") {
		t.Fatalf("settled transcript repeats the token count: %q", final)
	}
	for _, block := range m.transcriptDisplayEntries(100) {
		if !block.isActivity() {
			continue
		}
		header := strings.SplitN(block.text, "\n", 2)[0]
		if !strings.HasPrefix(header, "  "+style.Styled("▸", "accent", "bold")+" ") || !strings.Contains(header, "read 1 file](fg:muted") {
			t.Fatalf("settled inline activity keeps its shape: %q", header)
		}
		if plain := plainStyledText(header); !regexp.MustCompile(`  \d+(\.\d+)?s$`).MatchString(plain) {
			t.Fatalf("settled line does not end with the turn's time: %q", plain)
		}
	}
	if trailer := m.turnTrailers.latest(); trailer != nil {
		t.Fatalf("a turn with an activity line left a trailer row: %#v", trailer)
	}
	if got := m.lastTurnSummary(); !strings.HasSuffix(got, " · 100 in / 20 out") {
		t.Fatalf("last turn summary = %q", got)
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
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash", Arguments: `{"command":"ls"}`}
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
	if len(activityLines) != 1 || !strings.Contains(activityLines[0], "▸ thought 0.7s · ran 2 commands") {
		t.Fatalf("uninterrupted activity should aggregate into one row: %#v", activityLines)
	}
	rows := m.transcriptRows(100)
	m.disclosurePlacements[activityAll] = m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityAll)
	if len(m.disclosurePlacements[activityAll]) != 1 || len(m.disclosurePlacements[activityAll][0].thoughtIDs) != 1 {
		t.Fatalf("aggregate activity hitbox = %#v", m.disclosurePlacements[activityAll])
	}
	p := m.disclosurePlacements[activityAll][0]
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
		!strings.Contains(activityHeaders[0], "ran 2 commands](fg:muted") ||
		!strings.Contains(activityHeaders[1], "ran 1 command](fg:muted") {
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

	// The clipped line is still one control, up to its ellipsis; the tool
	// label past the ellipsis keeps no painted columns.
	rows := m.transcriptRows(width)
	hits := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityAll)
	if len(hits) != 1 || hits[0].X != 2 || hits[0].Cols != width-2 {
		t.Fatalf("clipped line hitbox = %#v, want x=2 cols=%d", hits, width-2)
	}
	for _, block := range m.visual.blocks {
		for _, label := range block.activityLabels {
			if label.kind == activityTools && label.X+label.Cols > width-1 {
				t.Fatalf("clipped tool label painted past the ellipsis: %#v", label)
			}
		}
	}

	const fullyTruncatedWidth = 8
	rows = m.transcriptRows(fullyTruncatedWidth)
	hits = m.visibleDisclosurePlacements(fullViewport(len(rows), fullyTruncatedWidth), activityAll)
	if len(hits) != 1 || hits[0].Cols != fullyTruncatedWidth-2 {
		t.Fatalf("fully truncated line lost its control: %#v", hits)
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

// The open sections of an activity row hang from one muted rail: no corner
// row, every detail line behind the rail, and a single bare rail row between
// two open sections. Sections stack in control order, and the click mappings
// for agents, tools and the thought tail follow their rows behind the rail.
func TestOpenActivitySectionsStackUnderTheLine(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("compare the two approaches")
	calls := []messages.ChatMessageToolCall{
		agentCall("a", `{"label":"Trace sessions"}`),
		{ID: "b", Name: "read_file", Arguments: `{"path":"picker.go"}`},
	}
	tui.AppendToolStart(calls)
	for _, c := range calls {
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	// A launched agent with a session is the row that carries an inspector link.
	_, agentRow := m.toolDisclosureRowForCall("a")
	agentRow.agent.session = "child-a"
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	block := activityBlocks(m, 80)[0]
	ids := block.toolDisclosureIDs
	// detail is every row under the line: each indented, none blank, no
	// rail, no corner.
	detail := func() []string {
		t.Helper()
		lines := strings.Split(plainStyledText(activityBlocks(m, 80)[0].text), "\n")
		for i, line := range lines[1:] {
			if !strings.HasPrefix(line, activityIndent) || strings.TrimSpace(line) == "" || strings.ContainsAny(line, "│╭╰─") {
				t.Fatalf("detail line %d is not a plain indented row: %q in %q", i+1, line, lines)
			}
		}
		return lines[1:]
	}
	if got := detail(); len(got) != 0 {
		t.Fatalf("collapsed line drew detail: %q", got)
	}
	m.toggleDisclosureGroup(activityTools, ids, 80)
	got := detail()
	if len(got) != 1 || !strings.HasPrefix(got[0], activityIndent+"✓ read picker.go") {
		t.Fatalf("a lone open section wants its rows straight under the line: %q", got)
	}
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	got = detail()
	if len(got) < 2 || !strings.Contains(got[0], "compare the two approaches") || !strings.HasPrefix(got[len(got)-1], activityIndent+"✓ read picker.go") {
		t.Fatalf("thought section wants to lead, tools to follow with no row between: %q", got)
	}
	m.toggleDisclosureGroup(activityAgents, ids, 80)
	got = detail()
	if len(got) < 3 || !strings.HasPrefix(got[len(got)-1], activityIndent+"✓ Trace sessions") {
		t.Fatalf("three open sections want the agent row last: %q", got)
	}

	rows := m.transcriptRows(80)
	text := transcriptRowsText(rows)
	links := m.visibleAgentLinks(fullViewport(len(rows), 80))
	if len(links) != 1 || !strings.Contains(text[links[0].Y], "Trace sessions") {
		t.Fatalf("agent link missed its indented row: %#v", links)
	}

	// Closing a section removes its rows, nothing else.
	before := len(got)
	m.toggleDisclosureGroup(activityTools, ids, 80)
	if got := detail(); len(got) != before-1 {
		t.Fatalf("closing tools wants one row fewer: %q", got)
	}
}
