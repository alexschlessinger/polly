package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// paintChunks lays the transcript out at width and records the chunk headers
// a frame holding all of it shows, as painting does.
func paintChunks(m *replModel, width int) []disclosurePlacement {
	rows := (conversationView{}).Rows(m, width)
	m.disclosurePlacements[activityAll] = m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityAll)
	return m.disclosurePlacements[activityAll]
}

// clickHeader clicks the header of the chunk named key.
func clickHeader(t *testing.T, m *replModel, width int, key string) {
	t.Helper()
	for _, p := range paintChunks(m, width) {
		if p.key == key {
			if !m.toggleDisclosureAt(p.X+p.Cols-1, p.Y, width) {
				t.Fatalf("the header of %q did not toggle", key)
			}
			return
		}
	}
	t.Fatalf("no header on screen for %q", key)
}

// foldStates lists the transcript's chunks of calls in order: their keys and
// whether each shows its rows.
func foldStates(m *replModel, width int) (keys []string, open []bool) {
	for _, block := range activityBlocks(m, width) {
		for _, c := range block.chunks {
			if c.group != chunkThought {
				keys = append(keys, c.key)
				open = append(open, m.chunkOpen(c.key))
			}
		}
	}
	return keys, open
}

// callRow finds a call's row anywhere in the transcript, not only in the
// running turn.
func callRow(t *testing.T, m *replModel, callID string) (*toolDisclosureRecord, *toolDisclosureRow) {
	t.Helper()
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			if record.rows[i].callID == callID {
				return record, &record.rows[i]
			}
		}
	}
	t.Fatalf("no row for call %q", callID)
	return nil, nil
}

// noDetailOpen reports whether every thought's text and every output is
// closed: the things only a click on their own row opens.
func noDetailOpen(m *replModel) bool {
	for _, record := range m.reasoningRecords.all() {
		if record.anyOpen() {
			return false
		}
	}
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.outputExpanded {
				return false
			}
		}
	}
	return true
}

// A transcript opens collapsed: every chunk is its header alone while the
// turn runs, once it settles, in the next turn and when the session is
// reloaded.
func TestChunksStartFoldedLiveAndReloaded(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("fix")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "fix"}}
	step := func(n int, reasoning string, fail bool, calls ...messages.ChatMessageToolCall) {
		tui.ModelAttemptStarted(n, 0)
		if reasoning != "" {
			tui.ShowThinking(reasoning)
		}
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: reasoning, ToolCalls: calls})
		tui.AppendToolStart(calls)
		for _, c := range calls {
			var err error
			if fail {
				err = errors.New("exit 1")
			}
			tui.AppendToolEnd(c, "ok", time.Millisecond, err)
			result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: c.ID, ToolName: c.Name, Content: "ok"}
			result.SetToolSucceeded(err == nil)
			history = append(history, result)
		}
	}
	want := "▎ fix"
	for n, s := range []struct {
		reasoning string
		fail      bool
		calls     []messages.ChatMessageToolCall
		header    string
	}{
		{"look first", false, []messages.ChatMessageToolCall{readCall("a"), readCall("b")}, "▸ Read 2 files"},
		{"", false, []messages.ChatMessageToolCall{editCall("c")}, "▸ Edited c.go"},
		{"then check", true, []messages.ChatMessageToolCall{bashCall("t", "go test ./...")}, "▸ Tests failed"},
	} {
		step(n, s.reasoning, s.fail, s.calls...)
		want += "\n" + s.header
		if got := shown(m, width); got != want {
			t.Fatalf("after step %d the running turn shows %q, want %q", n, got, want)
		}
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Done."})
	activity := func(model *replModel) []string {
		var rows []string
		for _, row := range strings.Split(shown(model, width), "\n")[1:] {
			if !strings.HasPrefix(row, "▎") && row != "" {
				rows = append(rows, row)
			}
		}
		return rows
	}
	settled := []string{"▸ Read 2 files", "▸ Edited c.go", "▸ Tests failed", "Done."}
	if got := activity(m); !slices.Equal(got, settled) {
		t.Fatalf("the settled turn shows %q, want %q", got, settled)
	}
	m.beginTurn("again")
	next := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	next.AppendToolStart([]messages.ChatMessageToolCall{readCall("d")})
	if got := activity(m); !slices.Equal(got, append(slices.Clone(settled), "▸ Reading d.go")) {
		t.Fatalf("the next turn shows %q", got)
	}
	reloaded := newReplModel()
	reloaded.hydrateHistory(history, "ctx")
	if got := activity(reloaded); !slices.Equal(got, settled) {
		t.Fatalf("the reloaded session shows %q, want %q", got, settled)
	}
	for name, model := range map[string]*replModel{"live": m, "reloaded": reloaded} {
		if _, open := foldStates(model, width); model.depth != depthCollapsed || len(model.chunkFolds) != 0 || slices.Contains(open, true) || !noDetailOpen(model) {
			t.Fatalf("%s: state %s, folds %v, open %v", name, model.depth.label(), model.chunkFolds, open)
		}
	}
}

// A click on a header folds or opens that chunk alone, so several can be
// open at once. What a reader opened inside a chunk is still open when the
// chunk is folded and opened again.
func TestHeaderClickFoldsOneChunkAlone(t *testing.T) {
	const width = 80
	m := chunkTurn{steps: [][]messages.ChatMessageToolCall{{readCall("a")}, {editCall("b")}, {bashCall("t", "go test ./...")}}}.run(t)
	keys, open := foldStates(m, width)
	if len(keys) != 3 || slices.Contains(open, true) {
		t.Fatalf("fixture wants three folded chunks: %q %v", keys, open)
	}
	states := func(want ...bool) {
		t.Helper()
		if _, got := foldStates(m, width); !slices.Equal(got, want) {
			t.Fatalf("chunks open = %v, want %v", got, want)
		}
	}
	clickHeader(t, m, width, keys[1])
	states(false, true, false)
	if rows := strings.Split(shown(m, width), "\n"); len(rows) != 5 || rows[1] != "▸ Read a.go" || rows[2] != "▾ Edited b.go" || !strings.HasPrefix(rows[3], "  ✓ edit b.go") || rows[4] != "▸ Tests passed" {
		t.Fatalf("one open chunk between two folded ones: %q", rows)
	}
	clickHeader(t, m, width, keys[0])
	states(true, true, false)

	// An output opened inside a chunk survives that chunk's fold.
	record, row := callRow(t, m, "b")
	row.outputExpanded = true
	m.refreshToolDisclosure(record)
	clickHeader(t, m, width, keys[1])
	states(true, false, false)
	if _, row := callRow(t, m, "b"); !row.outputExpanded {
		t.Fatal("folding a chunk closed the output a reader opened in it")
	}
	clickHeader(t, m, width, keys[1])
	states(true, true, false)
	if _, row := callRow(t, m, "b"); !row.outputExpanded {
		t.Fatal("reopening a chunk lost the output a reader opened in it")
	}
	// The bulk state is untouched: these are folds by hand.
	if m.depth != depthCollapsed || len(m.chunkFolds) != 2 {
		t.Fatalf("header clicks changed the bulk state: %s, folds %v", m.depth.label(), m.chunkFolds)
	}

	// Only the header is the fold: a click past its label, or on a call's
	// row under it, folds nothing.
	header := chunkHit(t, m, func(p disclosurePlacement) bool { return p.key == keys[0] })
	if m.toggleDisclosureAt(header.X+header.Cols, header.Y, width) || m.toggleDisclosureAt(header.X+2, header.Y+1, width) {
		t.Fatal("a click off the header toggled a chunk")
	}
	states(true, true, false)
}

// Ctrl-O puts every chunk in the bulk state opposite the last one set and
// forgets the folds made by hand. If that would change nothing on screen it
// applies the other state, so a press never looks dead. No press opens a
// thought's text or an output.
func TestCtrlOAppliesTheOtherStateAndClearsHandFolds(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	if m.toggleAllDisclosures(width) || m.depth != depthCollapsed {
		t.Fatal("a view with no activity took the press")
	}
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.ShowThinking("plan the change")
	for step, calls := range [][]messages.ChatMessageToolCall{{readCall("a")}, {editCall("b")}, {bashCall("t", "go test ./...")}} {
		if step > 0 {
			tui.ModelAttemptStarted(step, 0)
		}
		tui.AppendToolStart(calls)
		for _, c := range calls {
			tui.AppendToolEnd(c, "output text", time.Millisecond, nil)
		}
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	keys, _ := foldStates(m, width)
	press := func(want disclosureDepth, why string) {
		t.Helper()
		paintChunks(m, width) // a press reads the last frame
		if !m.toggleAllDisclosures(width) {
			t.Fatalf("%s: the press was refused", why)
		}
		_, open := foldStates(m, width)
		if m.depth != want || len(m.chunkFolds) != 0 || slices.Contains(open, want != depthExpanded) {
			t.Fatalf("%s: state %s, hand folds %v, open %v", why, m.depth.label(), m.chunkFolds, open)
		}
		if !noDetailOpen(m) {
			t.Fatalf("%s: the press opened a thought's text or an output", why)
		}
	}
	press(depthExpanded, "first press")
	if text := shown(m, width); strings.Contains(text, "plan the change") || strings.Contains(text, "output text") || !strings.Contains(text, thoughtGlyph+" thought") || !strings.Contains(text, "✓ read a.go") {
		t.Fatalf("expanded shows the thought's row and the calls' rows, and no detail: %q", text)
	}
	press(depthCollapsed, "second press")

	// One chunk opened by hand in a collapsed view: the press expands all.
	clickHeader(t, m, width, keys[1])
	press(depthExpanded, "after one hand fold")

	// Every chunk on screen folded by hand in an expanded view: collapsing
	// would change nothing, so the press expands.
	for _, key := range keys {
		clickHeader(t, m, width, key)
	}
	if _, open := foldStates(m, width); slices.Contains(open, true) || m.depth != depthExpanded {
		t.Fatalf("fixture wants every chunk folded by hand: %v", open)
	}
	press(depthExpanded, "everything folded by hand")
	press(depthCollapsed, "and the press after it")

	// The reverse: every chunk on screen opened by hand in a collapsed view.
	for _, key := range keys {
		clickHeader(t, m, width, key)
	}
	press(depthCollapsed, "everything opened by hand")

	// A chunk above or below the screen does not count: with only the hand
	// opened chunk in view, expanding would change nothing a reader can see.
	clickHeader(t, m, width, keys[0])
	rows := (conversationView{}).Rows(m, width)
	header := chunkHit(t, m, func(p disclosurePlacement) bool { return p.key == keys[0] })
	onScreen := transcriptViewport{start: header.Y, end: header.Y + 2, width: width}
	if onScreen.end > len(rows) {
		t.Fatalf("fixture viewport runs past the transcript: %d rows", len(rows))
	}
	m.disclosurePlacements[activityAll] = m.visibleDisclosurePlacements(onScreen, activityAll)
	if got := m.disclosurePlacements[activityAll]; len(got) != 1 || got[0].key != keys[0] {
		t.Fatalf("fixture wants only the opened chunk's header in view: %+v", got)
	}
	if !m.toggleAllDisclosures(width) || m.depth != depthCollapsed || len(m.chunkFolds) != 0 {
		t.Fatalf("with only an opened chunk in view the press wants to collapse: %s %v", m.depth.label(), m.chunkFolds)
	}
}

// A chunk that begins later takes the bulk state; a call that joins a chunk
// takes that chunk's fold, since the chunk is keyed by its first call.
func TestNewChunksFollowTheBulkStateAndJoiningCallsTheirChunk(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	run := func(calls ...messages.ChatMessageToolCall) {
		tui.AppendToolStart(calls)
		for _, c := range calls {
			tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
		}
	}
	states := func(why string, want ...bool) {
		t.Helper()
		if _, got := foldStates(m, width); !slices.Equal(got, want) {
			t.Fatalf("%s: chunks open = %v, want %v", why, got, want)
		}
	}
	run(readCall("a"))
	keys, _ := foldStates(m, width)
	clickHeader(t, m, width, keys[0])
	run(readCall("b"))
	states("a read joined the chunk opened by hand", true)
	if rows := strings.Split(shown(m, width), "\n"); len(rows) != 4 || rows[1] != "▾ Read 2 files" || !strings.HasPrefix(rows[2], "  ✓ read a.go") || !strings.HasPrefix(rows[3], "  ✓ read b.go") {
		t.Fatalf("the joining call's row: %q", rows)
	}
	run(editCall("c"))
	states("an edit began a chunk in a collapsed view", true, false)
	paintChunks(m, width) // a press reads the last frame
	if !m.toggleAllDisclosures(width) || m.depth != depthExpanded {
		t.Fatal("the view did not expand")
	}
	run(bashCall("t", "go test ./..."))
	states("a test began a chunk in an expanded view", true, true, true)
	paintChunks(m, width)
	if !m.toggleAllDisclosures(width) || m.depth != depthCollapsed {
		t.Fatal("the view did not collapse")
	}
	tui.AppendAssistantText("Halfway.")
	run(readCall("d"))
	states("a read began a chunk in a collapsed view", false, false, false, false)
}

// A hundred calls are one row until asked for; opened, every one of them is
// a row a reader can reach, and nothing under a row opens by itself.
func TestHundredCallsStayBehindOneHeaderUntilAsked(t *testing.T) {
	const width = 100
	var steps [][]messages.ChatMessageToolCall
	for batch := range 10 {
		var calls []messages.ChatMessageToolCall
		for i := range 10 {
			calls = append(calls, readCall(fmt.Sprintf("f%02d", batch*10+i)))
		}
		steps = append(steps, calls)
	}
	m := chunkTurn{steps: steps}.run(t)
	if got := shown(m, width); got != "▎ work\n▸ Read 100 files" {
		t.Fatalf("folded: %q", got)
	}
	keys, _ := foldStates(m, width)
	clickHeader(t, m, width, keys[0])
	rows := (conversationView{}).Rows(m, width)
	text := transcriptRowsText(rows)
	if len(text) != 2+100 || text[1] != "▾ Read 100 files" {
		t.Fatalf("open: %d rows under %q", len(text), text[1])
	}
	for i := range 100 {
		if want := fmt.Sprintf("✓ read f%02d.go", i); !strings.Contains(text[2+i], want) {
			t.Fatalf("row %d = %q, want %q: every call in the order asked", i, text[2+i], want)
		}
	}
	links := m.visibleToolOutputLinks(fullViewport(len(rows), width), 0)
	seen := map[string]bool{}
	for _, link := range links {
		if link.action != "" || link.rect().Empty() || seen[link.key] {
			t.Fatalf("call row link %+v is not one reachable row of its own", link)
		}
		seen[link.key] = true
	}
	if len(links) != 100 || !noDetailOpen(m) {
		t.Fatalf("%d of 100 calls reachable, details closed=%v", len(links), noDetailOpen(m))
	}
}

// Opening the thought that is streaming follows the turn's thinking: the
// chunk that takes the thought in opens so the text stays in view, and each
// later thought of the turn opens as it starts. Closing a thought, or the
// turn ending, stops it.
func TestOpeningTheLiveThoughtFollowsTheTurnsThinking(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}

	tui.ModelAttemptStarted(0, 0)
	tui.ShowThinking("first reasoning")
	record := m.currentReasoningRecord()
	if got := shown(m, width); got != "▎ inspect\n"+thoughtGlyph+" Thinking…" || m.followThoughts {
		t.Fatalf("a streaming thought starts as its row alone: %q follow=%v", got, m.followThoughts)
	}
	clickThought(t, m, width, record.id, 0)
	if got := shown(m, width); !m.followThoughts || !strings.Contains(got, "first reasoning") {
		t.Fatalf("opening the live thought: follow=%v %q", m.followThoughts, got)
	}

	// The step's call arrives. The thought is now a row of the call's
	// chunk, which opens so the text a reader was following stays on screen.
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("a")})
	tui.AppendToolEnd(readCall("a"), "ok", time.Millisecond, nil)
	keys, open := foldStates(m, width)
	if len(keys) != 1 || !open[0] || m.depth != depthCollapsed {
		t.Fatalf("the chunk that took the followed thought: open=%v state=%s", open, m.depth.label())
	}
	rows := strings.Split(shown(m, width), "\n")
	if len(rows) < 5 || rows[1] != "▾ Read a.go" || !strings.HasPrefix(rows[2], "  "+thoughtGlyph+" thought") || !strings.Contains(rows[3], "first reasoning") || !strings.Contains(rows[len(rows)-1], "✓ read a.go") {
		t.Fatalf("the followed thought inside its chunk: %q", rows)
	}

	// The next response's thought opens as it starts.
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("second reasoning")
	record.ensureSegments()
	if len(record.segments) != 2 || !record.partOpen(1) || !strings.Contains(shown(m, width), "second reasoning") {
		t.Fatalf("a later thought of a followed turn started closed: %+v", record.segments)
	}

	// Closing it stops the following: the thought after it starts closed.
	clickThought(t, m, width, record.id, 1)
	if m.followThoughts || record.partOpen(1) {
		t.Fatalf("closing a thought kept following: follow=%v", m.followThoughts)
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("b")})
	tui.AppendToolEnd(readCall("b"), "ok", time.Millisecond, nil)
	tui.ModelAttemptStarted(2, 0)
	tui.ShowThinking("third reasoning")
	record.ensureSegments()
	if len(record.segments) != 3 || record.partOpen(2) || strings.Contains(shown(m, width), "third reasoning") {
		t.Fatalf("a thought after a closed one opened by itself: %+v", record.segments)
	}
	if !record.partOpen(0) {
		t.Fatal("closing one thought closed another")
	}

	// Following again lasts no longer than its turn.
	clickThought(t, m, width, record.id, 2)
	if !m.followThoughts {
		t.Fatal("reopening the live thought did not follow again")
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	m.beginTurn("next")
	next := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	next.ModelAttemptStarted(0, 0)
	next.ShowThinking("a new turn's reasoning")
	if later := m.currentReasoningRecord(); m.followThoughts || later.id == record.id || later.anyOpen() || strings.Contains(shown(m, width), "a new turn's reasoning") {
		t.Fatalf("following outlived its turn: follow=%v", m.followThoughts)
	}
}

func TestQueuedTurnsHaveIndependentAdjacentChunks(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	first := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	queued := textManagedTurn("second")
	item := queuedREPLInput{text: queued.displayText, turn: &queued}
	m.appendQueuedInput(&item)
	// Once the queued prompt has been echoed, the first turn can still
	// produce work below it. There is no intervening prose or prompt at
	// the actual boundary between these two otherwise compatible reads.
	a, b := readCall("first"), readCall("second")
	a.DisplayID, b.DisplayID = "first-call", "second-call"
	first.AppendToolStart([]messages.ChatMessageToolCall{a})
	first.AppendToolEnd(a, "ok", time.Millisecond, nil)
	r.endTurn(nil)
	m.activateQueuedInput(item)
	m.beginManagedTurnState(queued)
	second := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	second.AppendToolStart([]messages.ChatMessageToolCall{b})
	second.AppendToolEnd(b, "ok", time.Millisecond, nil)
	r.endTurn(nil)
	priorRecord, _ := callRow(t, m, a.ID)
	queuedRecord, _ := callRow(t, m, b.ID)
	if priorRecord.transcriptIndex+1 != queuedRecord.transcriptIndex {
		t.Fatal("fixture activity is not adjacent")
	}
	want := []string{"call:first-call", "call:second-call"}
	if keys, open := foldStates(m, width); !slices.Equal(keys, want) || !slices.Equal(open, []bool{false, false}) {
		t.Fatalf("adjacent turns did not start in separate folds: keys=%v open=%v", keys, open)
	}
	for i, key := range want {
		clickHeader(t, m, width, key)
		_, open := foldStates(m, width)
		if !open[i] || open[1-i] {
			t.Fatalf("opening %q affected the other turn: %v", key, open)
		}
		clickHeader(t, m, width, key)
	}
}

// A view remembers its bulk state, the chunks folded or opened by hand in
// it, and the thoughts and outputs opened in it; a later projection of the
// same session shows the same rows.
func TestViewStateKeepsFoldsThoughtsAndOutputs(t *testing.T) {
	const width = 80
	result := func(c messages.ChatMessageToolCall) messages.ChatMessage {
		msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: c.ID, ToolName: c.Name, Content: "the " + c.ID + " output"}
		msg.SetToolSucceeded(true)
		return msg
	}
	a, b, c := readCall("a"), editCall("b"), readCall("c")
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "first"},
		{Role: messages.MessageRoleAssistant, Reasoning: "why read a", ToolCalls: []messages.ChatMessageToolCall{a}}, result(a),
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{b}}, result(b),
		{Role: messages.MessageRoleAssistant, Content: "done"},
		{Role: messages.MessageRoleUser, Content: "second"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{c}}, result(c),
		{Role: messages.MessageRoleAssistant, Content: "done again"},
	}
	project := func(state viewState) *replModel {
		m := newReplModel()
		m.hydrateHistory(history, "agent")
		applyViewSections(m, state)
		return m
	}
	state := viewState{depth: depthExpanded}
	view := project(state)
	keys, open := foldStates(view, width)
	if len(keys) != 3 || slices.Contains(open, false) {
		t.Fatalf("an expanded view wants three open chunks: %q %v", keys, open)
	}
	clickHeader(t, view, width, keys[1])
	clickThought(t, view, width, view.reasoningOrder[0], 0)
	record, row := callRow(t, view, "a")
	row.outputExpanded = true
	view.refreshToolDisclosure(record)
	rememberViewSections(view, &state)
	want := shown(view, width)
	if !strings.Contains(want, "why read a") || !strings.Contains(want, "the a output") || !strings.Contains(want, "▸ Edited b.go") || strings.Contains(want, "the c output") {
		t.Fatalf("fixture view: %q", want)
	}

	later := project(state)
	if _, open := foldStates(later, width); later.depth != depthExpanded || !slices.Equal(open, []bool{true, false, true}) {
		t.Fatalf("a later projection lost the view's folds: %s %v", later.depth.label(), open)
	}
	if got := shown(later, width); got != want {
		t.Fatalf("a later projection shows\n%s\nwant\n%s", got, want)
	}
	// The same choices under the other bulk state: the hand fold stands,
	// the other chunks follow the state, the opened details are untouched.
	state.depth = depthCollapsed
	collapsed := project(state)
	if _, open := foldStates(collapsed, width); !slices.Equal(open, []bool{false, false, false}) {
		t.Fatalf("a collapsed projection: %v", open)
	}
	if _, row := callRow(t, collapsed, "a"); !row.outputExpanded || !collapsed.reasoningRecords.get(collapsed.reasoningOrder[0]).partOpen(0) {
		t.Fatal("the bulk state closed a detail the reader opened")
	}
}

// An inspected view opens expanded while the conversation beside it stays
// collapsed, and Ctrl-O changes the pane that has the keys and no other.
func TestInspectedViewOpensExpandedAndCtrlOStaysInItsPane(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(160, 40)
	turn := func(m *replModel, name string) {
		m.hydrateHistory([]messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: name},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{readCall(name)}},
			{Role: messages.MessageRoleTool, ToolCallID: name, ToolName: "read_file", Content: "ok"},
			{Role: messages.MessageRoleAssistant, Content: "done"},
		}, name)
	}
	turn(r.model, "main")
	agent := newReplModel()
	turn(agent, "agent")
	watchTestAgent(r, agent)
	v := waitInspector(t, r, 160)
	r.render()
	target := r.workspace().inspector.target
	state := r.workspace().viewState(target)
	paneOpen := func() bool {
		t.Helper()
		v = waitInspector(t, r, 160)
		r.render()
		_, open := foldStates(v.model, r.chrome.inner.Dx())
		if len(open) != 1 {
			t.Fatalf("the agent's view wants one chunk: %v", open)
		}
		return open[0]
	}
	mainOpen := func() bool {
		_, open := foldStates(r.model, 80)
		return open[0]
	}
	if !paneOpen() || mainOpen() || state.depth != depthExpanded || r.model.depth != depthCollapsed {
		t.Fatalf("at first: pane open=%v (%s), main open=%v (%s)", paneOpen(), state.depth.label(), mainOpen(), r.model.depth.label())
	}
	if len(v.model.toolOutputLinks) != 1 || len(r.model.toolOutputLinks) != 0 {
		t.Fatalf("the agent's call row is reachable, the conversation's is behind its fold: %d / %d", len(v.model.toolOutputLinks), len(r.model.toolOutputLinks))
	}
	press := func() { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"}) }

	r.workspace().inspector.focused = true
	press()
	if paneOpen() || mainOpen() || state.depth != depthCollapsed || r.model.depth != depthCollapsed {
		t.Fatalf("Ctrl-O in the pane: pane open=%v (%s), main open=%v (%s)", paneOpen(), state.depth.label(), mainOpen(), r.model.depth.label())
	}
	r.workspace().inspector.focused = false
	press()
	if paneOpen() || !mainOpen() || state.depth != depthCollapsed || r.model.depth != depthExpanded {
		t.Fatalf("Ctrl-O in the conversation: pane open=%v (%s), main open=%v (%s)", paneOpen(), state.depth.label(), mainOpen(), r.model.depth.label())
	}
}

// Ctrl-O names the state it set in the status bar, over the turn clock, for
// a moment; then the slot is as it was.
func TestCtrlONamesTheStateInTheStatusBar(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("a")})
	left := func() (string, string) {
		raw := m.statusRow(120)
		plain := plainStyledText(raw)
		return strings.TrimSpace(plain[:strings.Index(plain, "  ")]), raw
	}
	if word, _ := left(); !strings.HasSuffix(word, "s") || word == "expanded" || word == "collapsed" {
		t.Fatalf("a running turn wants its clock before any press: %q", word)
	}
	for _, want := range []string{"expanded", "collapsed", "expanded"} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		word, raw := left()
		if word != want || m.depth.label() != want || !strings.Contains(raw, style.Styled(want, "accent", "")) {
			t.Fatalf("a fresh press wants %q named brightly over the clock: %q", want, word)
		}
	}
	// Once the notice is old the clock is back.
	m.affordances.depthAt = time.Now().Add(-depthNoticeDuration)
	if !m.expireAffordances(time.Now()) || !m.affordances.depthAt.IsZero() {
		t.Fatal("the expired notice did not ask for a repaint")
	}
	if word, _ := left(); !strings.HasSuffix(word, "s") || word == "expanded" {
		t.Fatalf("the clock wants its slot back: %q", word)
	}
	// A press that changes nothing, in a view with no activity, names nothing.
	empty := newManagedREPL(&Config{}, "ctx", 0, 0)
	empty.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if plain := plainStyledText(empty.model.statusRow(120)); strings.Contains(plain, "expanded") || strings.Contains(plain, "collapsed") {
		t.Fatalf("a refused press named a state: %q", plain)
	}
}

// Ctrl-O in an agent's view keeps a held line where the reader left it: the
// chunk above it folds and opens, and the row under the pane's top edge is
// the same row.
func TestAgentViewCtrlOKeepsItsHeldLine(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	var answer strings.Builder
	for n := range 100 {
		fmt.Fprintf(&answer, "stable-line-%03d\n", n)
	}
	r.tabs[1].model.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Repeat("multiline assignment\n", 20)},
		{Role: messages.MessageRoleAssistant, Reasoning: "reasoning before the held answer", ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "bash"}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "bash", Content: strings.Repeat("tool detail\n", 8)},
		{Role: messages.MessageRoleAssistant, Content: answer.String()},
	}, "agent")
	screen.SetSize(160, 36)
	r.inspect(tabViewTarget(r.tabs[1]))
	waitInspector(t, r, 160)
	settle := func() {
		r.render()
		waitInspector(t, r, 160)
		r.render()
	}
	settle()
	s := r.workspace().viewState(r.workspace().inspector.target)
	s.follow = false
	found := false
	for n, row := range transcriptRowsText(r.inspectorW.Rows) {
		if strings.Contains(row, "stable-line-050") {
			s.top, found = n, true
			break
		}
	}
	if !found {
		t.Fatal("fixture omitted the held answer line")
	}
	settle()
	before := transcriptRowsText(r.inspectorW.Rows[s.top : s.top+1])[0]
	rows := len(r.inspectorW.Rows)
	r.workspace().inspector.focused = true
	for _, depth := range []disclosureDepth{depthCollapsed, depthExpanded, depthCollapsed} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		settle()
		after := transcriptRowsText(r.inspectorW.Rows[s.top : s.top+1])[0]
		if after != before || s.depth != depth || s.follow {
			t.Fatalf("%s moved the held line: before=%q after=%q follow=%v", depth.label(), before, after, s.follow)
		}
		// The press did change the rows above the held line.
		if grew := len(r.inspectorW.Rows) - rows; (depth == depthCollapsed) != (grew < 0) {
			t.Fatalf("%s changed the view by %d rows", depth.label(), grew)
		}
		rows = len(r.inspectorW.Rows)
	}
}
