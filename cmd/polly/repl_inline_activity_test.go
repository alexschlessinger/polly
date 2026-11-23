package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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
		first, last                         bool
	}{
		{"open after prose", "prompt", "header\ndetail", "answer", "prompt\n\nheader\ndetail\n\nanswer", true, true},
		{"open after user prompt", formattedUserPrompt("check branch"), "header\ndetail", "answer", formattedUserPrompt("check branch") + "\nheader\ndetail\n\nanswer", true, true},
		{"open after multiline user prompt", formattedUserPrompt("check\nbranch"), "header\ndetail", "answer", formattedUserPrompt("check\nbranch") + "\nheader\ndetail\n\nanswer", true, true},
		{"collapsed", "prompt", "header", "answer", "prompt\nheader\nanswer", false, false},
		{"only the first chunk open", "prompt", "header\ndetail\nheader", "answer", "prompt\n\nheader\ndetail\nheader\nanswer", true, false},
		{"only the last chunk open", "prompt", "header\nheader\ndetail", "answer", "prompt\nheader\nheader\ndetail\n\nanswer", false, true},
		{"existing spacing", "prompt\n", "header\ndetail", "\nanswer", "prompt\n\nheader\ndetail\n\nanswer", true, true},
		{"empty neighbors", "", "header\ndetail", "", "\nheader\ndetail\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []transcriptDisplayBlock{
				{text: tc.before},
				{text: tc.activity, reasoningIDs: []int64{1}, openFirst: tc.first, openLast: tc.last},
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

// A chunk's viewed images open and fold with the chunk, and with nothing
// else: its thought stays a closed row.
func TestOpenChunkShowsItsViewedImages(t *testing.T) {
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

	activity := activityBlocks(m, 120)[0]
	if got := plainStyledText(activity.text); got != "▸ Viewed 2 images" {
		t.Fatalf("collapsed image chunk = %q", got)
	}
	if len(activity.images) != 0 {
		t.Fatalf("a folded chunk emitted image sidecars: %#v", activity.images)
	}

	rows := m.transcriptRows(120)
	hits := m.visibleDisclosurePlacements(fullViewport(len(rows), 120), activityAll)
	if len(hits) != 1 || hits[0].X != 0 || hits[0].Cols != len("> Viewed 2 images") || !slices.Equal(hits[0].thoughtIDs, activity.reasoningIDs) || !slices.Equal(hits[0].recordIDs, activity.toolDisclosureIDs) {
		t.Fatalf("expected one header covering its label and naming its line's records: %#v", hits)
	}
	m.disclosurePlacements[activityAll] = hits
	if !m.toggleDisclosureAt(hits[0].X, hits[0].Y, 0) {
		t.Fatal("the header did not open its chunk")
	}
	record := m.toolDisclosures.get(activity.toolDisclosureIDs[0])
	thought := m.reasoningRecords.get(activity.reasoningIDs[0])
	if !m.lineOpenOf(record) || thought.anyOpen() {
		t.Fatalf("header click: chunk open=%v thought open=%v", m.lineOpenOf(record), thought.anyOpen())
	}
	open := activityBlocks(m, 120)[0]
	lines := strings.Split(plainStyledText(open.text), "\n")
	if len(open.images) != 2 || lines[0] != "▾ Viewed 2 images" || lines[1] != "  "+thoughtGlyph+" thought" || !strings.Contains(lines[2], "view_image") {
		t.Fatalf("open image chunk = %#v / %q", open.images, lines)
	}

	tui.AppendAssistantText("done")
	r.endTurn(nil)
	if !m.lineOpenOf(record) {
		t.Fatalf("settlement folded a chunk opened by hand: record=%#v", record)
	}
	// The settled turn rides on its prompt; nothing repeats the activity.
	if trailer := m.turnTrailers.latest(); trailer != nil {
		t.Fatalf("settled turn with a prompt left a trailer row: %q", plainStyledText(m.transcript[trailer.transcriptIndex].text))
	}
	if settled := activityBlocks(m, 120)[0]; len(settled.images) != 2 || !strings.HasPrefix(plainStyledText(settled.text), "▾ Viewed 2 images") {
		t.Fatalf("settled image chunk = %q", plainStyledText(settled.text))
	}
}

// Every activity row shares one shape: an accent triangle, then muted labels.
func TestInlineActivityHeadersShareOneShape(t *testing.T) {
	if got, want := toolDisclosureHeader(2), activityRowHeader("▾", "2 tools"); got != want {
		t.Fatalf("canonical tool header = %q, want %q", got, want)
	}

	m := newReplModel()
	record := &reasoningRecord{complete: true, elapsed: 2 * time.Second}
	if got, want := m.reasoningRecordText(record, 80), activityRowHeader("▸", "thought "+formatElapsed(record.elapsed)); got != want {
		t.Fatalf("canonical thought header = %q, want %q", got, want)
	}
	// A chunk header is the same shape, bright only while a call runs or
	// when the whole label is a failure.
	settled := &activityChunk{label: "read 2 files", kind: activityTools}
	if text, cols, label := renderChunkHeader(settled, 80, false); text != activityRowHeader("▸", "Read 2 files") || cols != 14 || label != (turnDockPlacement{kind: activityTools, X: 2, Cols: 12}) {
		t.Fatalf("chunk header = %q cols=%d label=%#v", text, cols, label)
	}
	live := &activityChunk{label: "reading x.go", kind: activityTools, live: true}
	if text, _, _ := renderChunkHeader(live, 80, true); text != style.Styled("▾", "accent", "bold")+" "+style.Styled("Reading x.go", "active", "bold") {
		t.Fatalf("live chunk header = %q", text)
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
	if len(placements) != 1 || placements[0] != (turnDockPlacement{kind: activityAll, X: 0, Cols: 24}) {
		t.Fatalf("two-field placements = %#v", placements)
	}
	if len(labels) != 2 || labels[0] != (turnDockPlacement{kind: activityThought, X: 2, Cols: 12}) || labels[1] != (turnDockPlacement{kind: activityTools, X: 17, Cols: 7}) {
		t.Fatalf("two-field labels = %#v", labels)
	}
	// The right field sits against the right edge, two columns clear.
	row, placements, _ = renderActivityRow([]turnDockField{
		activityField("thought 0.7s", activityThought, false),
	}, turnDockField{raw: "34.0s", rendered: style.Styled("34.0s", "muted", "")}, 40)
	if plain := plainStyledText(row); plain != "▸ thought 0.7s"+strings.Repeat(" ", 21)+"34.0s" {
		t.Fatalf("row with right field = %q", plain)
	}
	if len(placements) != 1 || placements[0] != (turnDockPlacement{kind: activityAll, X: 0, Cols: 14}) {
		t.Fatalf("right-field placements = %#v", placements)
	}
	// A clipped line keeps its triangle and the right field, and its hitbox
	// stops at the ellipsis.
	row, placements, _ = renderActivityRow([]turnDockField{
		activityField("thought 0.7s", activityThought, false),
		activityField("2 tools", activityTools, false),
	}, turnDockField{raw: "1m 2s", rendered: style.Styled("1m 2s", "muted", "")}, 30)
	if plain := plainStyledText(row); plain != "▸ thought 0.7s · 2 too…  1m 2s" || !strings.Contains(row, style.Styled("▸", "accent", "bold")) {
		t.Fatalf("clipped row = %q (%q)", plain, row)
	}
	if len(placements) != 1 || placements[0] != (turnDockPlacement{kind: activityAll, X: 0, Cols: 23}) {
		t.Fatalf("clipped placements = %#v", placements)
	}
}

func TestInlineActivityRunningLabelsPreserveLiteralText(t *testing.T) {
	withDisplayTTY(t)
	for _, tc := range []struct {
		name, tool, arg, value, want, header string
	}{
		{"unmatched command bracket", "bash", "command", "grep '[' file", "$ grep '[' file", "Running grep '[' file"},
		{"command markup", "bash", "command", "printf '[ok](fg:err)'", "$ printf '[ok](fg:err)'", "Running printf '[ok](fg:err)'"},
		{"balanced command brackets", "bash", "command", "[ -f foo ] && sleep 30", "$ [ -f foo ] && sleep 30", "Running [ -f foo ] && sleep 30"},
		{"unmatched path bracket", "read_file", "path", "src/[draft.go", "read src/[draft.go", "Reading [draft.go"},
		{"path markup", "read_file", "path", "[draft](fg:err).go", "read [draft](fg:err).go", "Reading [draft](fg:err).go"},
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
			// Raw commands belong in the header and the evidence, where
			// brackets and text resembling style markup must survive
			// rendering literally.
			tui.AppendToolStart([]messages.ChatMessageToolCall{call, agentCall("agent", `{"label":"Trace sessions"}`)})
			rows := m.transcriptRows(160)
			if header := transcriptRowsText(rows)[len(rows)-1]; header != "▸ "+tc.header {
				t.Fatalf("live header = %q, want literal %q", header, tc.header)
			}
			hits := m.visibleDisclosurePlacements(fullViewport(len(rows), 160), activityAll)
			if len(hits) != 1 || hits[0].X != 0 {
				t.Fatalf("chunk header missing: %#v", hits)
			}
			m.disclosurePlacements[activityAll] = hits
			if !m.toggleDisclosureAt(hits[0].X, hits[0].Y, 160) {
				t.Fatal("clicking the header did not open the evidence")
			}
			got := strings.Join(transcriptRowsText(m.transcriptRows(160)), "\n")
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "Trace sessions") {
				t.Fatalf("running evidence = %q, want literal %q and the adjacent agent label", got, tc.want)
			}
		})
	}
}

func TestInlineActivityToolEndRefreshesFoldedHeader(t *testing.T) {
	withDisplayTTY(t)
	for _, tc := range []struct {
		name, result, detail, afterFirst, afterBoth string
		err                                         error
	}{
		{"denied", llm.ToolDeniedContent, "denied", "Running sleep 30 · 1 denied", "Ran 2 commands · 2 denied", nil},
		{"failed", "", "failed", "Running sleep 30 · 1 failed", "Ran 2 commands · 1 denied, 1 failed", fmt.Errorf("command failed")},
		{"canceled", "", "canceled", "Running sleep 30 · 1 canceled", "Ran 2 commands · 1 denied, 1 canceled", context.Canceled},
		{"completed", "ok", "1 line", "Running sleep 30", "Ran 2 commands · 1 denied", nil},
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
			if !strings.HasSuffix(before, "▸ Running 2 commands") {
				t.Fatalf("initial activity = %q", before)
			}
			// Denied calls receive OnToolEnd without OnToolResult. Their
			// outcome and any remaining running call must appear immediately.
			tui.AppendToolEnd(first, tc.result, 0, tc.err)
			m.refreshActiveTools()
			m.refreshStreamCursor()
			m.refreshReasoningRecords(width)
			check := func(want string) {
				t.Helper()
				if got := activityText(t, m, width, 0); got != "▸ "+want {
					t.Fatalf("activity = %q, want %q", got, "▸ "+want)
				}
			}
			check(tc.afterFirst)
			tui.AppendToolEnd(second, llm.ToolDeniedContent, 0, nil)
			check(tc.afterBoth)
			record := m.currentToolDisclosure()
			if !m.toggleToolDisclosure(record.id) {
				t.Fatal("the chunk did not open")
			}
			expanded := strings.Join(transcriptRowsText(m.transcriptRows(width)), "\n")
			if !strings.Contains(expanded, "$ rm file · "+tc.detail) || !strings.Contains(expanded, "✗ $ sleep 30 · denied") {
				t.Fatalf("open chunk lost individual outcomes: %q", expanded)
			}
		})
	}
}

// Smoke: full turn lifecycle with inline activity — the live header during
// the turn, the settled one after, the turn's time on the prompt row, and
// the evidence one click at a time.
func TestInlineActivitySmoke(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("do work")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	tui.ShowThinking("let me think about this")
	call := messages.ChatMessageToolCall{ID: "c1", Name: "read_file", Arguments: `{"path":"x.go"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})

	// The thought led the read, so both are one chunk; its header is the
	// call in flight.
	rows := m.transcriptRows(100)
	if lines := transcriptRowsText(rows); len(lines) != 2 || lines[1] != "▸ Reading x.go" {
		t.Fatalf("mid-turn activity should only show the running read: %q", lines)
	}
	block := activityBlocks(m, 100)[0]
	if want := style.Styled("▸", "accent", "bold") + " " + style.Styled("Reading x.go", "active", "bold"); block.text != want {
		t.Fatalf("live label styling = %q, want %q", block.text, want)
	}
	hits := m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityAll)
	if len(hits) != 1 || hits[0].X != 0 || hits[0].Cols != 14 || len(hits[0].thoughtIDs) != 1 || len(hits[0].recordIDs) != 1 {
		t.Fatalf("expected one header over the triangle and its label: %#v", hits)
	}

	tui.AppendToolEnd(call, "file contents", 50*time.Millisecond, nil)
	tui.AppendAssistantText("All done.")
	tui.RecordTurnTokens(100, 20, false)
	r.endTurn(nil)

	// Settled: one header that names the call, and the turn's time at the
	// right edge of the prompt's row. Tokens are /context's to report.
	m.renderPendingMarkdown()
	lines := transcriptRowsText(m.transcriptRows(100))
	final := strings.Join(lines, "\n")
	for _, want := range []string{"\n▸ Read x.go\n", "All done."} {
		if !strings.Contains(final, want) {
			t.Fatalf("settled transcript missing %q: %q", want, final)
		}
	}
	if strings.Contains(final, "100 in") {
		t.Fatalf("settled transcript repeats the token count: %q", final)
	}
	if !regexp.MustCompile(`^▎ do work +\d+(\.\d+)?s$`).MatchString(lines[0]) || len([]rune(lines[0])) != 100 {
		t.Fatalf("prompt row does not end with the turn's time at the edge: %q", lines[0])
	}
	settled := activityBlocks(m, 100)[0]
	if want := activityRowHeader("▸", "Read x.go"); settled.text != want {
		t.Fatalf("settled header = %q, want %q", settled.text, want)
	}
	if trailer := m.turnTrailers.latest(); trailer != nil {
		t.Fatalf("a turn with a prompt left a trailer row: %#v", trailer)
	}
	if got := m.lastTurnSummary(); !strings.HasSuffix(got, " · 100 in / 20 out") {
		t.Fatalf("last turn summary = %q", got)
	}

	// The header opens the chunk: a thought row and the call, no text.
	rows = m.transcriptRows(100)
	hits = m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityAll)
	m.disclosurePlacements[activityAll] = hits
	if len(hits) != 1 || !m.toggleDisclosureAt(hits[0].X, hits[0].Y, 100) {
		t.Fatal("settled header did not open")
	}
	open := activityText(t, m, 100, 0)
	if open != "▾ Read x.go\n  "+thoughtGlyph+" thought\n  ✓ read x.go · 1 line" {
		t.Fatalf("open chunk = %q", open)
	}
	// The thought's own row shows its text, above the call it led.
	clickThought(t, m, 100, settled.reasoningIDs[0], 0)
	expanded := strings.Join(transcriptRowsText((conversationView{}).Rows(m, 100)), "\n")
	if !strings.Contains(expanded, "    let me think about this") || !strings.Contains(expanded, "✓ read x.go") || strings.Contains(expanded, "file contents") {
		t.Fatalf("thought row did not show its text beside the closed call: %q", expanded)
	}
}

func TestInlineActivityAggregatesUntilAssistantProse(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("ls")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	for i, thought := range []string{"first pass", "second pass"} {
		tui.ModelAttemptStarted(i, 0)
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
	visible := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	if visible != "▎ ls\n▸ Ran 2 commands" {
		t.Fatalf("uninterrupted rounds should fold into one header: %q", visible)
	}
	rows := m.transcriptRows(100)
	m.disclosurePlacements[activityAll] = m.visibleDisclosurePlacements(fullViewport(len(rows), 100), activityAll)
	if len(m.disclosurePlacements[activityAll]) != 1 || len(m.disclosurePlacements[activityAll][0].thoughtIDs) != 1 {
		t.Fatalf("aggregate chunk hitbox = %#v", m.disclosurePlacements[activityAll])
	}
	p := m.disclosurePlacements[activityAll][0]
	if !m.toggleDisclosureAt(p.X, p.Y, 100) {
		t.Fatal("aggregate chunk did not open")
	}
	expanded := transcriptRowsText((conversationView{}).Rows(m, 100))
	if len(expanded) != 6 || strings.Count(strings.Join(expanded, "\n"), thoughtGlyph+" thought") != 2 || strings.Count(strings.Join(expanded, "\n"), "✓ $ ls") != 2 {
		t.Fatalf("open chunk wants each step's thought row above its call: %q", expanded)
	}
	if !m.toggleDisclosureAt(p.X, p.Y, 100) {
		t.Fatal("aggregate chunk did not fold")
	}

	tui.AppendAssistantText("Here is the result.")
	tui.ShowThinking("follow-up phase")
	next := messages.ChatMessageToolCall{ID: "c2", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{next})
	tui.AppendToolEnd(next, "ok", 50*time.Millisecond, nil)
	m.renderPendingMarkdown()
	visible = strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
	want := "▸ Ran 2 commands\nHere is the result.\n▸ Ran a command"
	if !strings.Contains(visible, want) || strings.Count(visible, "▸") != 2 {
		t.Fatalf("chunks did not follow the prose boundary: %q", visible)
	}
}

// A header too narrow for its label clips the label on its own row; nothing
// wraps, and the hit target is what was painted.
func TestNarrowChunkHeaderClipsOnOneRow(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("go") // a prompt that fits the narrowest width on one row
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	tui.ShowThinking("reasoning")
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})

	for _, w := range []int{18, 8} {
		rows := m.transcriptRows(w)
		text := transcriptRowsText(rows)
		hits := m.visibleDisclosurePlacements(fullViewport(len(rows), w), activityAll)
		if len(hits) != 1 || hits[0].X != 0 {
			t.Fatalf("width %d: want one header: %+v", w, hits)
		}
		line := []rune(text[hits[0].Y])
		if len(text) != 2 || len(line) > w || line[len(line)-1] != '…' || hits[0].Cols != len(line) || !strings.HasPrefix(string(line), "▸ Runn") {
			t.Fatalf("width %d: clipped header %q with hit %+v in %q", w, string(line), hits[0], text)
		}
		m.disclosurePlacements[activityAll] = hits
		if m.toggleDisclosureAt(hits[0].Cols, hits[0].Y, w) {
			t.Fatal("a click past the header toggled it")
		}
	}
}

func TestChunkTogglesReanchorProjectedVisualBlockOnce(t *testing.T) {
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
	toolIDs := append([]int64(nil), m.turnToolDisclosureIDs...)
	if len(ids) != 2 || len(toolIDs) != 2 {
		t.Fatalf("fixture IDs = %v / %v, want 2 of each", ids, toolIDs)
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
	for _, id := range toolIDs {
		if !m.toggleToolDisclosure(id) {
			t.Fatal("chunk open returned false")
		}
		assertAnchored("opening a chunk above the viewport")
	}
	// A thought's text renders on the paint after its click; both the click
	// and that paint keep the viewport.
	for _, id := range ids {
		clickThought(t, m, width, id, 0)
		assertAnchored("opening a thought above the viewport")
	}
	if !strings.Contains(strings.Join(transcriptRowsText(m.transcriptRows(width)), "\n"), "older reasoning") {
		t.Fatal("fixture did not show the opened thought's text")
	}
	for _, id := range ids {
		clickThought(t, m, width, id, 0)
		assertAnchored("closing a thought above the viewport")
	}
	for _, id := range toolIDs {
		if !m.toggleToolDisclosure(id) {
			t.Fatal("chunk fold returned false")
		}
		assertAnchored("folding a chunk above the viewport")
	}
}

// What is open under a chunk hangs from one muted rail: no corner row, every
// detail line behind the rail, and a single bare rail row between the calls
// and the agents. The click mappings for agents and calls follow their rows.
func TestOpenChunkSectionsStackUnderItsHeader(t *testing.T) {
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
	// An agent launch and a read requested together are one chunk, the
	// launch leading its header.
	if got := plainStyledText(block.text); got != "▸ 1 agent and read picker.go" {
		t.Fatalf("mixed batch header = %q", got)
	}
	// detail is every row under the header: each indented, no rail drawn,
	// no corner, a blank row only between two sections.
	detail := func() []string {
		t.Helper()
		lines := strings.Split(plainStyledText(activityBlocks(m, 80)[0].text), "\n")
		for i, line := range lines[1:] {
			blank := line == activityIndent
			if !strings.HasPrefix(line, activityIndent) || strings.ContainsAny(line, "│╭╰─") || blank && (i == 0 || i+2 >= len(lines)) {
				t.Fatalf("detail line %d is not a plain indented row: %q in %q", i+1, line, lines)
			}
		}
		return lines[1:]
	}
	if got := detail(); len(got) != 0 {
		t.Fatalf("folded chunk drew detail: %q", got)
	}
	// Opening the chunk shows its one step, the thought row above its call,
	// and the agents after a blank row.
	m.toggleDisclosureGroup(activityTools, ids, 80)
	got := detail()
	if len(got) != 4 || got[0] != activityIndent+thoughtGlyph+" thought" || !strings.HasPrefix(got[1], activityIndent+"✓ read picker.go") || got[2] != activityIndent || !strings.HasPrefix(got[3], activityIndent+"✓ Trace sessions") {
		t.Fatalf("open chunk wants thought row, call, blank, agent: %q", got)
	}

	rows := m.transcriptRows(80)
	text := transcriptRowsText(rows)
	links := m.visibleAgentLinks(fullViewport(len(rows), 80))
	if len(links) != 1 || !strings.Contains(text[links[0].Y], "Trace sessions") {
		t.Fatalf("agent link missed its indented row: %#v", links)
	}

	// Folding the chunk hides every evidence row, including the last call.
	m.toggleDisclosureGroup(activityAgents, ids, 80)
	if got := detail(); len(got) != 0 {
		t.Fatalf("folded chunk left detail visible: %q", got)
	}
}
