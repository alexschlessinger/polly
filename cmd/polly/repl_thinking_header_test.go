package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// thinkFor makes the thought streaming in m have lasted d.
func thinkFor(m *replModel, d time.Duration) {
	m.thinkingSegmentStart = time.Now().Add(-d)
	m.visual.invalidate()
}

// While the model thinks between calls, the newest run's header says so, as
// it would name a call in flight. The thought has no row of its own, so a
// response that thinks before it calls adds no row and takes none away.
func TestStreamingThoughtIsTheNewestHeadersLabel(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	a, b, c := readCall("a"), readCall("b"), editCall("c")

	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{a})
	tui.AppendToolEnd(a, "ok", time.Millisecond, nil)
	if got := activityText(t, m, width, 0); got != "▸ Read a.go" {
		t.Fatalf("the settled run = %q", got)
	}

	// The next response starts with a thought. Too young to be read, it
	// leaves the header as it was: no row, no new label.
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("which file next")
	chunks := onlyChunks(t, m)
	if len(chunks) != 1 || chunks[0].live || chunks[0].label != "read a.go" || len(chunks[0].parts) != 2 || chunks[0].parts[1].thought == nil {
		t.Fatalf("a thought just begun = %q live=%v parts=%d, want the run's own header and the thought as its last row", chunkHeaders(chunks), chunks[0].live, len(chunks[0].parts))
	}
	if got := activityText(t, m, width, 0); got != "▸ Read a.go" {
		t.Fatalf("a thought just begun shows %q", got)
	}
	key := chunks[0].key

	// Once it has outlasted the time a label is held, the header is the
	// thought, lit as a call in flight is, with its clock after a second.
	// The fold is still the run's.
	thinkFor(m, thinkingLabelDelay)
	chunks = onlyChunks(t, m)
	if len(chunks) != 1 || !chunks[0].live || chunks[0].kind != activityTools || chunks[0].label != "thinking…" || chunks[0].key != key {
		t.Fatalf("a streaming thought = %q live=%v key=%q", chunkHeaders(chunks), chunks[0].live, chunks[0].key)
	}
	if !strings.HasPrefix(chunks[0].latest, "thought:") {
		t.Fatalf("the thought takes the header over as a call would: latest = %q", chunks[0].latest)
	}
	thinkFor(m, 3*time.Second)
	if got := activityText(t, m, width, 0); got != "▸ Thinking… 3s" {
		t.Fatalf("a streaming thought shows %q", got)
	}

	// Its call joins the run: the header names the call, the thought is the
	// row above it, and the line is still one row.
	tui.AppendToolStart([]messages.ChatMessageToolCall{b})
	if got := activityText(t, m, width, 0); got != "▸ Reading b.go" {
		t.Fatalf("the call in flight shows %q", got)
	}
	tui.AppendToolEnd(b, "ok", time.Millisecond, nil)
	chunks = onlyChunks(t, m)
	if len(chunks) != 1 || chunks[0].label != "read 2 files" || chunks[0].live {
		t.Fatalf("the run after its thought = %q", chunkHeaders(chunks))
	}
	for i, wantThought := range []bool{false, true, false} {
		if (chunks[0].parts[i].thought != nil) != wantThought {
			t.Fatalf("part %d: thought=%v, want the thought above the call it led", i, chunks[0].parts[i].thought != nil)
		}
	}

	// A thought that leads to another kind of work has that run's header
	// until its call arrives; then it is the new run's first row.
	tui.ModelAttemptStarted(2, 0)
	tui.ShowThinking("now change it")
	thinkFor(m, 2*time.Second)
	if got := activityText(t, m, width, 0); got != "▸ Thinking… 2s" {
		t.Fatalf("a thought before an edit shows %q", got)
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{c})
	tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	if got := activityText(t, m, width, 0); got != "▸ Read 2 files\n▸ Edited c.go" {
		t.Fatalf("the edit's run shows %q", got)
	}
	chunks = onlyChunks(t, m)
	if len(chunks) != 2 || len(chunks[0].parts) != 3 || len(chunks[1].parts) != 2 || chunks[1].parts[0].thought == nil {
		t.Fatalf("the thought did not move to the run of the call it led: %d and %d parts", len(chunks[0].parts), len(chunks[1].parts))
	}
}

// An open run lists a streaming thought as its last row from the start: the
// row is evidence, and only the header waits to be readable.
func TestStreamingThoughtIsTheLastRowOfAnOpenRun(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	a := readCall("a")
	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{a})
	tui.AppendToolEnd(a, "ok", time.Millisecond, nil)
	m.depth = depthExpanded
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("which file next")
	rows := strings.Split(activityText(t, m, width, 0), "\n")
	if len(rows) != 3 || rows[0] != "▾ Read a.go" || !strings.Contains(rows[1], "read a.go") || !strings.HasPrefix(strings.TrimSpace(rows[2]), thoughtGlyph+" thinking…") {
		t.Fatalf("an open run with a thought just begun = %q", rows)
	}
	thinkFor(m, 2*time.Second)
	rows = strings.Split(activityText(t, m, width, 0), "\n")
	if len(rows) != 3 || rows[0] != "▾ Thinking… 2s" || !strings.HasPrefix(strings.TrimSpace(rows[2]), thoughtGlyph+" thinking… 2s") {
		t.Fatalf("an open run with a streaming thought = %q", rows)
	}
}

// The cue for a failed call stays beside the header while it says the model
// is thinking, as it does beside a call in flight.
func TestThinkingHeaderKeepsTheFailureCue(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	a, b := bashCall("a", "make lint"), bashCall("b", "make build")
	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{a, b})
	tui.AppendToolEnd(a, "boom", time.Millisecond, errors.New("exit 1"))
	tui.AppendToolEnd(b, "ok", time.Millisecond, nil)
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("why did it fail")
	thinkFor(m, 4*time.Second)
	if got := activityText(t, m, 80, 0); got != "▸ Thinking… 4s · 1 failed" {
		t.Fatalf("a thought after a failure shows %q", got)
	}

	// A lone failed test run is its own cue settled; the thinking label is
	// not, so the cue comes back beside it.
	tests := bashCall("t", "go test ./...")
	tui.AppendToolStart([]messages.ChatMessageToolCall{tests})
	tui.AppendToolEnd(tests, "boom", time.Millisecond, errors.New("exit 1"))
	if got := activityText(t, m, 80, 0); got != "▸ Ran 2 commands · 1 failed\n▸ Tests failed" {
		t.Fatalf("the failed test run shows %q", got)
	}
	tui.ModelAttemptStarted(2, 0)
	tui.ShowThinking("why did the tests fail")
	thinkFor(m, 2*time.Second)
	if got := activityText(t, m, 80, 0); got != "▸ Ran 2 commands · 1 failed\n▸ Thinking… 2s · failed" {
		t.Fatalf("a thought after a failed test run shows %q", got)
	}
}

// A thought no call followed is the last row of the run before it, live and
// reloaded: no thought stands among a turn's headers. A turn that made no
// call has nothing to hold its thought, which keeps a row of its own.
func TestThoughtThatLedNoCallClosesTheRunBeforeIt(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("check")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	a := readCall("a")
	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{a})
	tui.AppendToolEnd(a, "ok", time.Millisecond, nil)
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("that settles it")
	tui.AppendAssistantText("It is fine.")
	tui.FinishTextTurn()
	r.endTurn(nil)

	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: a.ID, ToolName: a.Name, Content: "ok"}
	result.SetToolSucceeded(true)
	reloaded := newReplModel()
	reloaded.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "check"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{a}},
		result,
		{Role: messages.MessageRoleAssistant, Reasoning: "that settles it", Content: "It is fine."},
		{Role: messages.MessageRoleUser, Content: "sure?"},
		{Role: messages.MessageRoleAssistant, Reasoning: "nothing to look up", Content: "Yes."},
	}, "ctx")

	for name, model := range map[string]*replModel{"live": m, "reloaded": reloaded} {
		rows := strings.Split(shown(model, width), "\n")
		at := slices.Index(rows, "▸ Read a.go")
		if at < 0 || at+1 >= len(rows) || rows[at+1] != "It is fine." {
			t.Fatalf("%s: the answer does not follow its run directly: %q", name, rows)
		}
		chunks := activityBlocks(model, width)[0].chunks
		if len(chunks) != 1 || len(chunks[0].parts) != 2 || chunks[0].parts[1].thought == nil {
			t.Fatalf("%s: the thought is not the run's last row: %d chunks", name, len(chunks))
		}
		// Open, the run reads in the order the turn made it.
		model.depth = depthExpanded
		model.visual.invalidate()
		open := strings.Split(activityText(t, model, width, 0), "\n")
		if len(open) != 3 || !strings.Contains(open[1], "read a.go") || !strings.HasPrefix(strings.TrimSpace(open[2]), thoughtGlyph+" thought") {
			t.Fatalf("%s: the open run = %q", name, open)
		}
	}
	rows := strings.Split(shown(reloaded, width), "\n")
	if at := slices.Index(rows, thoughtGlyph+" Thought"); at < 0 || at+1 >= len(rows) || rows[at+1] != "Yes." {
		t.Fatalf("a turn with no call lost its thought's row: %q", rows)
	}
}

// A run still at work with its agents keeps its header, which counts them. A
// thought that streams after it has the row below, and the next call's header
// takes that row.
func TestThoughtAfterWorkingAgentsHasItsOwnRow(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{agentCall("child", `{}`)})
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("while it works")
	thinkFor(m, 2*time.Second)
	if got := activityText(t, m, width, 0); got != "▸ 1 agent running\n"+thoughtGlyph+" Thinking… 2s" {
		t.Fatalf("a thought beside a working agent shows %q", got)
	}
	// Its call is a new run, whose header has the thought's row.
	b := readCall("b")
	tui.AppendToolStart([]messages.ChatMessageToolCall{b})
	if got := activityText(t, m, width, 0); got != "▸ 1 agent running\n▸ Reading b.go" {
		t.Fatalf("the call after that thought shows %q", got)
	}
}

// A thought shorter than the time a label is held never shows: the header
// goes from one call to the next. A longer one takes the header when the
// call before it has had its time.
func TestShortThoughtNeverTakesTheHeader(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	at := time.Unix(100, 0)
	header := func(now time.Time) string {
		t.Helper()
		rows := transcriptRowsText(m.transcriptRowsAt(width, now))
		return rows[len(rows)-1]
	}
	a, b, c := readCall("a"), readCall("b"), readCall("c")
	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{a})
	if got := header(at); got != "▸ Reading a.go" {
		t.Fatalf("the first call shows %q", got)
	}
	tui.AppendToolEnd(a, "ok", time.Millisecond, nil)

	// A thought of 300 ms, then its call: the label never said thinking.
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("quick")
	thinkFor(m, 300*time.Millisecond)
	if got := header(at.Add(collapsedActivityMinimum + 300*time.Millisecond)); strings.Contains(got, "hinking") {
		t.Fatalf("a 300 ms thought took the header: %q", got)
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{b})
	next := at.Add(collapsedActivityMinimum + 400*time.Millisecond)
	if got := header(next); got != "▸ Reading b.go" {
		t.Fatalf("the next call shows %q", got)
	}
	tui.AppendToolEnd(b, "ok", time.Millisecond, nil)

	// A thought of two seconds takes it once b's label has had its time, and
	// keeps it for its own before the call it led shows.
	tui.ModelAttemptStarted(2, 0)
	tui.ShowThinking("longer")
	thinkFor(m, 2*time.Second)
	if got := header(next.Add(collapsedActivityMinimum - time.Millisecond)); got != "▸ Reading b.go" {
		t.Fatalf("b's label was cut short: %q", got)
	}
	thinking := next.Add(collapsedActivityMinimum)
	if got := header(thinking); got != "▸ Thinking… 2s" {
		t.Fatalf("a two second thought shows %q", got)
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{c})
	if got := header(thinking.Add(collapsedActivityMinimum - time.Millisecond)); got != "▸ Thinking… 2s" {
		t.Fatalf("the thinking label was cut short: %q", got)
	}
	if got := header(thinking.Add(collapsedActivityMinimum)); got != "▸ Reading c.go" {
		t.Fatalf("the call after the thought shows %q", got)
	}
}
