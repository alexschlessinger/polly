package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	ui "github.com/metaspartan/gotui/v5"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

// toolRows returns the semantic tool-disclosure entries in the transcript.
func toolRows(m *replModel) []string {
	var rows []string
	for i, e := range m.transcript {
		if m.toolDisclosures.idAt(i) != 0 {
			rows = append(rows, e.text)
		}
	}
	return rows
}

func clickToolDisclosure(t *testing.T, m *replModel, recordID int64, width int) {
	t.Helper()
	rows := m.transcriptRows(width)
	placements := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityAll)
	m.disclosurePlacements[activityAll] = placements
	for _, placement := range placements {
		if slices.Contains(placement.recordIDs, recordID) {
			if !m.toggleDisclosureAt(placement.X, placement.Y, 0) {
				t.Fatalf("clicking tool disclosure %d did not toggle it", recordID)
			}
			return
		}
	}
	t.Fatalf("tool disclosure %d had no visible click target: %#v", recordID, placements)
}

// A live chunk starts folded, its header naming the call in flight. A click
// on the header shows every call's row and another hides them all; calls
// that start meanwhile join the chunk as it stands.
func TestLiveToolChunkStartsFoldedAndClickTogglesAllRows(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run tools")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	first := messages.ChatMessageToolCall{ID: "a", Name: "alpha"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{first})
	record := m.currentToolDisclosure()
	if record == nil || record.complete || m.lineOpenOf(record) || len(record.rows) != 1 {
		t.Fatalf("first live disclosure = %#v, want one active folded row", record)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("first live call rendered %d tool entries, want one: %#v", len(rows), m.transcript)
	}
	if folded := shown(m, width); folded != "▎ run tools\n▸ Using alpha" {
		t.Fatalf("first live call wants its header alone: %q", folded)
	}

	clickToolDisclosure(t, m, record.id, width)
	if open := shown(m, width); !m.lineOpenOf(record) || !strings.HasPrefix(open, "▎ run tools\n▾ Using alpha\n  → alpha") {
		t.Fatalf("first live chunk did not open to its call's row: %q", open)
	}

	second := messages.ChatMessageToolCall{ID: "b", Name: "beta"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{second})
	open := strings.Split(shown(m, width), "\n")
	if len(open) != 4 || open[1] != "▾ Using 2 tools" || !strings.Contains(open[2], "→ alpha") || !strings.Contains(open[3], "→ beta") {
		t.Fatalf("open live chunk did not add the second call once: %q", open)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("open live calls escaped into %d transcript entries: %#v", len(rows), m.transcript)
	}

	tui.AppendToolEnd(first, "RAW ALPHA RESULT", time.Second, nil)
	open = strings.Split(shown(m, width), "\n")
	if len(open) != 4 || open[1] != "▾ Using beta" || !strings.Contains(open[2], "✓ alpha") || !strings.Contains(open[3], "→ beta") {
		t.Fatalf("open live completion did not update safely: %q", open)
	}

	clickToolDisclosure(t, m, record.id, width)
	if folded := shown(m, width); m.lineOpenOf(record) || folded != "▎ run tools\n▸ Using beta" {
		t.Fatalf("the header did not hide every row: %q record=%#v", folded, record)
	}
}

func TestToolDisclosureSplitsAtInterleavedProse(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	first := messages.ChatMessageToolCall{ID: "a", Name: "alpha"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{first})
	tui.AppendToolEnd(first, "ok", time.Second, nil)
	firstRecord := m.currentToolDisclosure()
	tui.AppendAssistantText("thinking about it")
	// One parallel batch after the prose stays a single disclosure.
	var batch []messages.ChatMessageToolCall
	for _, id := range []string{"b", "c", "d"} {
		batch = append(batch, messages.ChatMessageToolCall{ID: id, Name: "tool_" + id})
	}
	tui.AppendToolStart(batch)
	for _, call := range batch {
		tui.AppendToolEnd(call, "ok", time.Second, nil)
	}

	// Interleaved prose splits the turn's tool activity into two disclosures,
	// each holding its own position in the transcript.
	record := m.currentToolDisclosure()
	if record == nil || record.transcriptIndex < 0 || m.lineOpenOf(record) || len(record.rows) != 3 {
		t.Fatalf("post-prose disclosure = %#v", record)
	}
	if firstRecord == nil || firstRecord.id == record.id || len(firstRecord.rows) != 1 {
		t.Fatalf("pre-prose disclosure = %#v", firstRecord)
	}
	if rows := toolRows(m); len(rows) != 2 {
		t.Fatalf("interleaved activity rendered %d tool entries, want 2: %#v", len(rows), m.transcript)
	}
	// Folded, each side of the prose is one header: a run of one call names
	// it, a longer one counts.
	if folded := shown(m, width); folded != "▎ work\n▸ Used alpha\nthinking about it\n▸ Used 3 tools" {
		t.Fatalf("interleaved order failed: %q", folded)
	}

	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("post-prose disclosure did not expand")
	}
	expanded := shown(m, width)
	previous := strings.Index(expanded, "▾ Used 3 tools")
	for _, want := range []string{"tool_b", "tool_c", "tool_d"} {
		at := strings.Index(expanded, want)
		if previous < 0 || at <= previous || strings.Count(expanded, want) != 1 {
			t.Fatalf("expanded post-prose order failed at %q: %q", want, expanded)
		}
		previous = at
	}
	if !strings.Contains(expanded, "▸ Used alpha") {
		t.Fatalf("opening one chunk opened the other: %q", expanded)
	}
	if !m.toggleToolDisclosure(firstRecord.id) {
		t.Fatal("pre-prose disclosure did not expand")
	}
	expanded = shown(m, width)
	if row := strings.Index(expanded, "✓ alpha"); row < 0 || row > strings.Index(expanded, "thinking about it") {
		t.Fatalf("pre-prose detail escaped its position: %q", expanded)
	}
}

func TestExpandedParallelToolDisclosureUpdatesInStartOrder(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run parallel")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	calls := make([]messages.ChatMessageToolCall, 5)
	for i := range calls {
		calls[i] = messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("tool%d", i)}
	}
	tui.AppendToolStart(calls)
	record := m.currentToolDisclosure()
	if record == nil || m.lineOpenOf(record) || len(record.rows) != len(calls) {
		t.Fatalf("parallel live disclosure = %#v", record)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("folded parallel batch rendered %d tool entries: %#v", len(rows), m.transcript)
	}
	if folded := shown(m, width); folded != "▎ run parallel\n▸ Using 5 tools" {
		t.Fatalf("parallel batch did not start folded: %q", folded)
	}

	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("parallel live disclosure did not expand")
	}
	order := []int{4, 1, 3, 0, 2}
	for settled, i := range order {
		tui.AppendToolEnd(calls[i], "ok", time.Duration(i+1)*time.Second, nil)
		rows := strings.Split(shown(m, width), "\n")
		if len(rows) != 2+len(calls) || !strings.HasPrefix(rows[1], "▾ ") {
			t.Fatalf("after %d parallel completions, chunk = %q", settled+1, rows)
		}
		// The rows keep the order the calls were requested in, whatever
		// order their results arrive in.
		body := rows[2:]
		if got := strings.Count(strings.Join(body, "\n"), "✓"); got != settled+1 {
			t.Fatalf("after %d parallel completions, %d rows read done: %q", settled+1, got, body)
		}
		for callIndex := range calls {
			if name := fmt.Sprintf("tool%d", callIndex); !strings.Contains(body[callIndex], name) {
				t.Fatalf("parallel start order changed at %s after completion order %v: %q", name, order[:settled+1], body)
			}
		}
		if rows := toolRows(m); len(rows) != 1 {
			t.Fatalf("parallel updates escaped into %d transcript entries: %#v", len(rows), m.transcript)
		}
	}
	if header := strings.Split(shown(m, width), "\n")[1]; header != "▾ Used 5 tools" {
		t.Fatalf("settled parallel header = %q", header)
	}
	if m.runningTools != 0 {
		t.Fatalf("runningTools = %d, want 0", m.runningTools)
	}
}

func TestToolDisclosureAggregatesBatchesWithinRun(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run batches")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	// Three batches with no prose between them are one unbroken run: every
	// batch folds into the same chunk and the header counts them all.
	for batch := 0; batch < 3; batch++ {
		calls := make([]messages.ChatMessageToolCall, 5)
		for i := range calls {
			calls[i] = messages.ChatMessageToolCall{
				ID:   fmt.Sprintf("b%d-%d", batch, i),
				Name: fmt.Sprintf("tool_%d_%d", batch, i),
			}
		}
		tui.AppendToolStart(calls)
		for _, c := range calls {
			tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
		}
		if rows := toolRows(m); len(rows) != 1 {
			t.Fatalf("after batch %d: %d disclosure entries, want 1", batch, len(rows))
		}
		record := m.currentToolDisclosure()
		if record == nil || len(record.rows) != (batch+1)*5 || record.complete {
			t.Fatalf("batch %d disclosure = %#v", batch, record)
		}
		if got, want := shown(m, width), fmt.Sprintf("▎ run batches\n▸ Used %d tools", (batch+1)*5); got != want {
			t.Fatalf("after batch %d folded transcript = %q, want %q", batch, got, want)
		}
	}

	// Opening the chunk shows every recorded row, in start order.
	record := m.currentToolDisclosure()
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("aggregated chunk did not open")
	}
	rows := strings.Split(shown(m, width), "\n")
	if len(rows) != 2+15 || rows[1] != "▾ Used 15 tools" {
		t.Fatalf("aggregated chunk missing its header or hid rows: %q", rows)
	}
	for i := 0; i < 15; i++ {
		if name := fmt.Sprintf("tool_%d_%d", i/5, i%5); !strings.Contains(rows[2+i], name) {
			t.Fatalf("aggregated order failed at %s: %q", name, rows)
		}
	}
}

// Completion leaves a chunk as the reader left it, open or folded.
func TestCompletedTurnKeepsItsRollupAsLeft(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	for i := 0; i < 7; i++ {
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("tool%d", i)}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Second, nil)
	}
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) || !m.lineOpenOf(record) {
		t.Fatalf("live chunk did not open before completion: %#v", record)
	}
	tui.AppendAssistantText("Done.")
	if got := len(toolRows(m)); got != 1 {
		t.Fatalf("aggregated run = %d transcript entries, want 1", got)
	}

	r.endTurn(nil)
	if rows := toolRows(m); len(rows) != 1 || len(record.rows) != 7 {
		t.Fatalf("a completed turn should keep one aggregate record, got %#v", rows)
	}
	// Settlement leaves the chunk as the user left it; folding it by hand
	// hides every row again.
	if !record.complete || !m.lineOpenOf(record) {
		t.Fatalf("completed disclosure = %#v, want complete and still open", record)
	}
	if !m.toggleToolDisclosure(record.id) || m.lineOpenOf(record) {
		t.Fatal("completed chunk did not fold by hand")
	}
	folded := shown(m, width)
	if !strings.HasPrefix(folded, "▎ explain") || !strings.HasSuffix(folded, "\n▸ Used 7 tools\nDone.") || strings.Contains(folded, "tool0") {
		t.Fatalf("folded chunk = %q", folded)
	}

	if !m.toggleToolDisclosure(record.id) || !m.lineOpenOf(record) {
		t.Fatal("completed chunk did not open")
	}
	open := shown(m, width)
	if !strings.Contains(open, "▾ Used 7 tools") || strings.Contains(open, " earlier") ||
		!strings.Contains(open, "tool6") || !strings.Contains(open, "tool0") || strings.Count(open, "Done.") != 1 {
		t.Fatalf("open chunk = %q", open)
	}
	if !m.toggleToolDisclosure(record.id) || m.lineOpenOf(record) {
		t.Fatal("completed chunk did not fold again")
	}
	if got := shown(m, width); got != folded {
		t.Fatalf("folding again did not restore the transcript:\n got %q\nwant %q", got, folded)
	}
}

// A run of one call folds too, and its header says what the call was rather
// than counting one of something.
func TestSingleCallTurnFoldsUnderItsCall(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run the tests")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	call := messages.ChatMessageToolCall{ID: "only", Name: "bash", Arguments: `{"command":"make lint"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	live := m.currentToolDisclosure()
	if live == nil || m.lineOpenOf(live) || live.complete || len(live.rows) != 1 {
		t.Fatalf("single live disclosure = %#v", live)
	}
	if got := shown(m, width); got != "▎ run the tests\n▸ Running make lint" {
		t.Fatalf("single live call did not start folded under its command: %q", got)
	}
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	record := m.currentToolDisclosure()
	if rows := toolRows(m); len(rows) != 1 || record == nil || !record.complete || m.lineOpenOf(record) {
		t.Fatalf("single-call disclosure = %#v, want one complete folded record", record)
	}
	folded := shown(m, width)
	if !strings.HasSuffix(folded, "\n▸ Ran make lint\nDone.") {
		t.Fatalf("single-call folded transcript = %q", folded)
	}
	if !m.toggleToolDisclosure(record.id) || !m.lineOpenOf(record) {
		t.Fatal("single-call chunk did not open")
	}
	if open := shown(m, width); !strings.Contains(open, "▾ Ran make lint\n  ✓ $ make lint") || strings.Count(open, "Done.") != 1 {
		t.Fatalf("single-call open transcript = %q", open)
	}
	if !m.toggleToolDisclosure(record.id) || m.lineOpenOf(record) {
		t.Fatal("single-call chunk did not fold again")
	}
	if got := shown(m, width); got != folded {
		t.Fatalf("single-call fold did not restore transcript:\n got %q\nwant %q", got, folded)
	}
}

func TestInterruptedTurnKeepsItsLineOpenAndFoldable(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "slow", Name: "slow_tool"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) || !m.lineOpenOf(record) {
		t.Fatalf("interrupted fixture did not open live disclosure: %#v", record)
	}

	r.endTurn(errors.New("provider unavailable"))
	if !m.lineOpenOf(record) || !record.complete {
		t.Fatalf("interrupted turn changed the line's state: record=%#v", record)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interrupted chunk did not fold by hand")
	}
	// Folded, the header still says the call failed; its row is gone.
	folded := shown(m, width)
	if m.lineOpenOf(record) || !strings.Contains(folded, "\n▸ Used slow_tool · failed\n") || strings.Contains(folded, "✗ slow_tool") {
		t.Fatalf("folded interrupted chunk: record=%#v transcript=%q", record, folded)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interrupted chunk did not reopen")
	}
	if open := shown(m, width); !strings.Contains(open, "✗ slow_tool · failed") {
		t.Fatalf("interrupted chunk lost its call's failure: %q", open)
	}
}

func TestInterruptedParallelBatchStaysFoldedInStartOrder(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("parallel")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	var calls []messages.ChatMessageToolCall
	for i := 0; i < 5; i++ {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("x%d", i), Name: fmt.Sprintf("tool%d", i)})
	}
	tui.AppendToolStart(calls)
	record := m.currentToolDisclosure()
	if record == nil || len(record.rows) != 5 || m.lineOpenOf(record) {
		t.Fatalf("interrupted parallel fixture = %#v", record)
	}
	r.endTurn(errors.New("provider unavailable"))
	if !record.complete || m.lineOpenOf(record) {
		t.Fatalf("interrupted parallel disclosure = %#v, want completed and folded", record)
	}
	// The folded header counts the failures; no call's row shows.
	if folded := shown(m, width); !strings.Contains(folded, "\n▸ Used 5 tools · 5 failed\n") || strings.Contains(folded, "tool0") {
		t.Fatalf("interrupted parallel fold = %q", folded)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interrupted parallel chunk did not open")
	}
	open := shown(m, width)
	previous := strings.Index(open, "▾ Used 5 tools · 5 failed")
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("tool%d", i)
		at := strings.Index(open, name+" · failed")
		if previous < 0 || at <= previous || strings.Count(open, name) != 1 {
			t.Fatalf("interrupted chunk lost order/status at %s: %q", name, open)
		}
		previous = at
	}
}

// A turn detached after its cancellation timed out settles like any other:
// its chunk stays as the reader left it.
func TestDetachedCancellationKeepsToolChunkAsLeft(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("detach")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "slow", Name: "slow_tool"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) || !m.lineOpenOf(record) {
		t.Fatalf("detached fixture did not open its chunk: %#v", record)
	}
	tui.ShowThinking("pondering")
	m.canceling = true

	r.abandonCanceledTurn(r.visibleTab())
	if !record.complete || !m.lineOpenOf(record) {
		t.Fatalf("detached disclosure = %#v, want completed and still open", record)
	}
	if open := shown(m, 80); !strings.Contains(open, "▾ Use slow_tool · canceled\n  "+thoughtGlyph+" thought · not saved\n  ✗ slow_tool · canceled") {
		t.Fatalf("detached chunk lost its canceled call: %q", open)
	}

	// The canceled turn rides on its last activity record, with a dock
	// that derives solely from the ID copies in abandonCanceledTurn.
	dock := m.settledDock
	if !dock.settled || m.turnTrailers.latest() != nil {
		t.Fatalf("canceled turn did not settle onto its activity: %#v trailers=%d", dock, m.turnTrailers.count())
	}
	if len(dock.toolIDs) != 1 || dock.toolIDs[0] != record.id {
		t.Fatalf("canceled dock toolIDs = %v, want [%d]", dock.toolIDs, record.id)
	}
	if len(dock.reasoningIDs) != 1 {
		t.Fatalf("canceled dock reasoningIDs = %v, want one record", dock.reasoningIDs)
	}
	if got := m.promptTrailerField(dock).raw; !strings.HasPrefix(got, "canceled") {
		t.Fatalf("canceled prompt right field = %q", got)
	}
}

func TestToolDisclosureSanitizesPrivateImageMarkerRunes(t *testing.T) {
	withDisplayTTY(t)
	marker := string(style.ImageMarker(0))

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "private-rune", Name: "screen" + marker + "shot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatal("private-rune disclosure did not expand")
	}
	if strings.Contains(m.transcript[record.transcriptIndex].text, marker) {
		t.Fatal("running tool label was mistaken for an image marker")
	}

	tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	entry := m.transcript[record.transcriptIndex].text
	if strings.Contains(entry, marker) {
		t.Fatalf("settled tool label leaked a private image marker: %q", entry)
	}
	if plain := plainStyledText(style.StripImageMarkers(entry)); !strings.Contains(plain, "screenshot") {
		t.Fatalf("sanitized tool label was not preserved as text: %q", plain)
	}
}

func TestToolDockTogglePreservesPhysicalViewportAnchor(t *testing.T) {
	const width = 24
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.reasoningWidth = width
	m.beginTurn("wrap")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	longName := "tool_" + strings.Repeat("wrapped_argument_", 8)
	// Two rows ensure expansion grows beyond the collapsed latest-call row,
	// without relying on a blank row between the prompt and activity.
	for _, id := range []string{"wrapped", "second"} {
		call := messages.ChatMessageToolCall{ID: id, Name: longName}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Second, nil)
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	record := m.currentToolDisclosure()
	if record == nil || !record.complete || m.lineOpenOf(record) {
		t.Fatalf("wrapped disclosure fixture = %#v", record)
	}
	var prose []string
	for i := 0; i < 30; i++ {
		prose = append(prose, fmt.Sprintf("ctx-%02d", i))
	}
	m.appendLine(strings.Join(prose, "\n"))

	m.followBottom = false
	beforeRows := transcriptRowsText(m.transcriptRows(width))
	collapsedAnchor := slices.Index(beforeRows, "ctx-10")
	if collapsedAnchor < 0 {
		t.Fatalf("fixture row missing from %q", beforeRows)
	}
	m.scrollAnchor = collapsedAnchor
	beforeTop := beforeRows[collapsedAnchor]

	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("wrapped tool disclosure did not expand")
	}
	if m.scrollAnchor <= collapsedAnchor {
		t.Fatalf("inline expansion did not preserve the held row: anchor=%d, was %d", m.scrollAnchor, collapsedAnchor)
	}
	expandedRows := transcriptRowsText(m.transcriptRows(width))
	if got := expandedRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("top physical row moved under overlay: %q -> %q", beforeTop, got)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("wrapped tool disclosure did not re-collapse")
	}
	if m.scrollAnchor != collapsedAnchor {
		t.Fatalf("re-collapsed scrollAnchor = %d, want %d", m.scrollAnchor, collapsedAnchor)
	}
	recollapsedRows := transcriptRowsText(m.transcriptRows(width))
	if got := recollapsedRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("top physical row moved on re-collapse: %q -> %q", beforeTop, got)
	}
}

func TestCompletedToolsKeepInlineHitboxAndTrailerControl(t *testing.T) {
	const width = 50
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run tools")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	for i := 0; i < 2; i++ {
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("tool%d", i)}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Second, nil)
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	record := m.currentToolDisclosure()
	if record == nil || !record.complete || m.lineOpenOf(record) {
		t.Fatalf("completed disclosure = %#v, want complete and collapsed", record)
	}
	for i := 0; i < 6; i++ {
		m.appendLine(fmt.Sprintf("later row %d", i))
	}

	// The settled tool block stays inline and clickable. Both batches of the
	// unbroken run share one aggregated record.
	rows := m.transcriptRows(width)
	visible := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityAll)
	if len(visible) != 1 || visible[0].Cols < 3 {
		t.Fatalf("completed tools lost their recap hitbox: %#v", visible)
	}
	for _, hit := range visible {
		if len(hit.recordIDs) != 1 || hit.recordIDs[0] != record.id {
			t.Fatalf("completed tools lost their transcript hitbox: %#v", visible)
		}
	}

	m.disclosurePlacements[activityAll] = visible
	if !m.toggleDisclosureAt(visible[0].X, visible[0].Y, 0) || !m.lineOpenOf(record) {
		t.Fatal("clicking the completed tool control did not open it")
	}
}

func TestToolDisclosureResetsPerTurn(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	runTurn := func(prompt, prefix string) *toolDisclosureRecord {
		m.beginTurn(prompt)
		tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
		// One parallel batch per turn keeps each turn to one disclosure.
		var calls []messages.ChatMessageToolCall
		for i := 0; i < 4; i++ {
			calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("%s%d", prefix, i), Name: fmt.Sprintf("%s_tool%d", prefix, i)})
		}
		tui.AppendToolStart(calls)
		for _, call := range calls {
			tui.AppendToolEnd(call, "ok", time.Second, nil)
		}
		tui.AppendAssistantText("Done.")
		record := m.currentToolDisclosure()
		r.endTurn(nil)
		return record
	}

	first := runTurn("first", "a")
	second := runTurn("second", "b")
	if first == nil || second == nil || first.id == second.id || !first.complete || !second.complete || m.lineOpenOf(first) || m.lineOpenOf(second) {
		t.Fatalf("per-turn disclosures: first=%#v second=%#v", first, second)
	}
	if len(first.rows) != 4 || len(second.rows) != 4 || m.toolDisclosures.count() != 2 {
		t.Fatalf("per-turn row counts: first=%d second=%d records=%d", len(first.rows), len(second.rows), m.toolDisclosures.count())
	}
	if rows := toolRows(m); len(rows) != 2 {
		t.Fatalf("completed turns rendered %d disclosure entries, want 2: %#v", len(rows), m.transcript)
	}

	if !m.toggleToolDisclosure(first.id) || !m.lineOpenOf(first) || m.lineOpenOf(second) {
		t.Fatalf("opening first turn affected second: first=%#v second=%#v", first, second)
	}
	if !m.toggleToolDisclosure(second.id) || !m.lineOpenOf(first) || !m.lineOpenOf(second) {
		t.Fatalf("opening second turn affected first: first=%#v second=%#v", first, second)
	}
	if !m.toggleToolDisclosure(first.id) || m.lineOpenOf(first) || !m.lineOpenOf(second) {
		t.Fatalf("re-collapsing first turn affected second: first=%#v second=%#v", first, second)
	}
}

func TestDraggedMouseSampleDoesNotRetoggleDisclosure(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(80, 24)
	m := r.model
	m.beginTurn("run tools")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config}
	tui.AppendToolStart([]messages.ChatMessageToolCall{{ID: "a", Name: "alpha"}})
	record := m.currentToolDisclosure()
	r.render()
	if len(m.disclosurePlacements[activityAll]) == 0 {
		t.Fatal("no disclosure placement after render")
	}
	p := m.disclosurePlacements[activityAll][0]
	press := ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: p.X, Y: p.Y}}
	r.handleEvent(press)
	if !m.lineOpenOf(record) {
		t.Fatal("press did not toggle the disclosure")
	}
	drag := ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: p.X + 2, Y: p.Y, Drag: true}}
	r.handleEvent(drag)
	if !m.lineOpenOf(record) {
		t.Fatal("held-button motion toggled the disclosure again")
	}
	if r.wantsRenderForEvent(drag) {
		t.Fatal("ignored drag sample still requests a repaint")
	}
	if r.mousePosition.X != p.X+2 {
		t.Fatal("drag sample did not update the pointer position")
	}
}
