package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// toolRows returns the semantic tool-disclosure entries in the transcript.
func toolRows(m *replModel) []string {
	var rows []string
	for i, e := range m.transcript {
		if m.toolDisclosureAt[i] != 0 {
			rows = append(rows, e.text)
		}
	}
	return rows
}

func clickToolDisclosure(t *testing.T, m *replModel, recordID int64, width int) {
	t.Helper()
	rows := m.transcriptRows(width)
	placements := m.visibleToolDisclosurePlacements(fullViewport(len(rows), width))
	m.toolDisclosurePlacements = placements
	for _, placement := range placements {
		if placement.recordID == recordID {
			if !m.toggleToolDisclosureAt(placement.X+1, placement.Y) {
				t.Fatalf("clicking tool disclosure %d did not toggle it", recordID)
			}
			return
		}
	}
	t.Fatalf("tool disclosure %d had no visible click target: %#v", recordID, placements)
}

func TestLiveToolDisclosureStartsCollapsedAndClickTogglesAllRows(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run tools")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	first := messages.ChatMessageToolCall{ID: "a", Name: "alpha"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{first})
	record := m.currentToolDisclosure()
	if record == nil || record.complete || record.expanded || len(record.rows) != 1 {
		t.Fatalf("first live disclosure = %#v, want one active collapsed row", record)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("first live call rendered %d tool entries, want one: %#v", len(rows), m.transcript)
	}
	collapsed := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(collapsed, "▸ 1 tool") || strings.Contains(collapsed, "alpha") {
		t.Fatalf("first live call was not private behind its header: %q", collapsed)
	}

	clickToolDisclosure(t, m, record.id, width)
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	if !record.expanded || !strings.Contains(expanded, "alpha") {
		t.Fatalf("first live expansion = %q record=%#v", expanded, record)
	}

	second := messages.ChatMessageToolCall{ID: "b", Name: "beta"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{second})
	expanded = plainStyledText(m.transcript[record.transcriptIndex].text)
	if strings.Count(expanded, "alpha") != 1 || strings.Count(expanded, "beta") != 1 {
		t.Fatalf("expanded live disclosure did not add the second call once: %q", expanded)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("expanded live calls escaped into %d transcript entries: %#v", len(rows), m.transcript)
	}

	tui.AppendToolEnd(first, "RAW ALPHA RESULT", time.Second, nil)
	expanded = plainStyledText(m.transcript[record.transcriptIndex].text)
	if !strings.Contains(expanded, "✓") || !strings.Contains(expanded, "alpha") || !strings.Contains(expanded, "beta") || strings.Contains(expanded, "RAW ALPHA RESULT") {
		t.Fatalf("expanded live completion did not update safely: %q", expanded)
	}

	clickToolDisclosure(t, m, record.id, width)
	collapsedAgain := plainStyledText(m.transcript[record.transcriptIndex].text)
	if record.expanded || strings.Contains(collapsedAgain, "alpha") {
		t.Fatalf("tool disclosure did not hide every row: %q record=%#v", collapsedAgain, record)
	}
}

func TestToolDisclosureSplitsAtInterleavedProse(t *testing.T) {
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
	if record == nil || record.transcriptIndex < 0 || record.expanded || len(record.rows) != 3 {
		t.Fatalf("post-prose disclosure = %#v", record)
	}
	if firstRecord == nil || firstRecord.id == record.id || len(firstRecord.rows) != 1 {
		t.Fatalf("pre-prose disclosure = %#v", firstRecord)
	}
	if rows := toolRows(m); len(rows) != 2 {
		t.Fatalf("interleaved activity rendered %d tool entries, want 2: %#v", len(rows), m.transcript)
	}
	collapsed := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	firstAt := strings.Index(collapsed, "▸ 1 tool")
	proseAt := strings.Index(collapsed, "thinking about it")
	secondAt := strings.Index(collapsed, "▸ 3 tools")
	if firstAt < 0 || proseAt <= firstAt || secondAt <= proseAt {
		t.Fatalf("interleaved order failed: %q", collapsed)
	}
	for _, hidden := range []string{"alpha", "tool_b", "tool_c", "tool_d"} {
		if strings.Contains(collapsed, hidden) {
			t.Fatalf("collapsed interleaved disclosure leaked %q: %q", hidden, collapsed)
		}
	}

	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("post-prose disclosure did not expand")
	}
	expanded := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	previous := strings.Index(expanded, "▾ 3 tools")
	for _, want := range []string{"tool_b", "tool_c", "tool_d"} {
		at := strings.Index(expanded, want)
		if at <= previous || strings.Count(expanded, want) != 1 {
			t.Fatalf("expanded post-prose order failed at %q: %q", want, expanded)
		}
		previous = at
	}
	if !m.toggleToolDisclosure(firstRecord.id) {
		t.Fatal("pre-prose disclosure did not expand")
	}
	expanded = plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if alpha := strings.Index(expanded, "alpha"); alpha < 0 || alpha > strings.Index(expanded, "thinking about it") {
		t.Fatalf("pre-prose detail escaped its position: %q", expanded)
	}
}

func TestExpandedParallelToolDisclosureUpdatesInStartOrder(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run parallel")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	// Five calls: the largest batch that stays fully visible under the
	// expanded-row cap, so every row's position is assertable.
	calls := make([]messages.ChatMessageToolCall, 5)
	for i := range calls {
		calls[i] = messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("tool%d", i)}
	}
	tui.AppendToolStart(calls)
	record := m.currentToolDisclosure()
	if record == nil || record.expanded || len(record.rows) != len(calls) {
		t.Fatalf("parallel live disclosure = %#v", record)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("collapsed parallel batch rendered %d tool entries: %#v", len(rows), m.transcript)
	}
	if collapsed := plainStyledText(strings.Join(transcriptTexts(m), "\n")); !strings.Contains(collapsed, "▸ 5 tools") || strings.Contains(collapsed, "tool0") {
		t.Fatalf("parallel batch did not start collapsed: %q", collapsed)
	}

	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("parallel live disclosure did not expand")
	}
	order := []int{4, 1, 3, 0, 2}
	for settled, i := range order {
		tui.AppendToolEnd(calls[i], "ok", time.Duration(i+1)*time.Second, nil)
		expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
		if !strings.Contains(expanded, "▾ 5 tools") || strings.Count(expanded, "✓") != settled+1 {
			t.Fatalf("after %d parallel completions, disclosure = %q", settled+1, expanded)
		}
		previous := strings.Index(expanded, "▾ 5 tools")
		for callIndex := range calls {
			name := fmt.Sprintf("tool%d", callIndex)
			at := strings.Index(expanded, name)
			if at <= previous || strings.Count(expanded, name) != 1 {
				t.Fatalf("parallel start order changed at %s after completion order %v: %q", name, order[:settled+1], expanded)
			}
			previous = at
		}
		if rows := toolRows(m); len(rows) != 1 {
			t.Fatalf("parallel updates escaped into %d transcript entries: %#v", len(rows), m.transcript)
		}
	}
	if m.runningTools != 0 {
		t.Fatalf("runningTools = %d, want 0", m.runningTools)
	}
}

func TestToolDisclosureAggregatesBatchesWithinRun(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run batches")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	// Three batches with no prose between them are one unbroken run: every
	// batch folds into the same disclosure and the header counts them all.
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
		plain := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
		if want := fmt.Sprintf("▸ %d tools", (batch+1)*5); !strings.Contains(plain, want) {
			t.Fatalf("after batch %d collapsed transcript = %q, want header %q", batch, plain, want)
		}
		if strings.Contains(plain, "tool_0_0") {
			t.Fatalf("after batch %d collapsed transcript leaked detail: %q", batch, plain)
		}
	}

	// Expanded, the aggregate shows the elision line and only the newest
	// rows, in start order.
	record := m.currentToolDisclosure()
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("aggregated disclosure did not expand")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	if !strings.Contains(expanded, "▾ 15 tools") || !strings.Contains(expanded, "… 10 earlier") {
		t.Fatalf("aggregated expansion missing header or elision: %q", expanded)
	}
	if strings.Contains(expanded, "tool_0_") || strings.Contains(expanded, "tool_1_") {
		t.Fatalf("aggregated expansion leaked capped rows: %q", expanded)
	}
	previous := strings.Index(expanded, "… 10 earlier")
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("tool_2_%d", i)
		at := strings.Index(expanded, name)
		if at <= previous || strings.Count(expanded, name) != 1 {
			t.Fatalf("aggregated order failed at %s: %q", name, expanded)
		}
		previous = at
	}
}

// Completion auto-collapses every disclosure of the turn, even ones the user
// opened live.
func TestCompletedTurnCollapsesToRollups(t *testing.T) {
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
	if record == nil || !m.toggleToolDisclosure(record.id) || !record.expanded {
		t.Fatalf("live disclosure did not open before completion: %#v", record)
	}
	tui.AppendAssistantText("Done.")
	if got := len(toolRows(m)); got != 1 {
		t.Fatalf("aggregated run = %d transcript entries, want 1", got)
	}

	r.endTurn(nil)
	rows := toolRows(m)
	if len(rows) != 1 || !strings.Contains(rows[0], "7 tools") {
		t.Fatalf("a completed turn should keep one aggregate rollup, got %#v", rows)
	}
	if record == nil || !record.complete || record.expanded {
		t.Fatalf("completed disclosure = %#v, want complete and collapsed", record)
	}
	collapsed := strings.Join(transcriptTexts(m), "\n")
	collapsedPlain := plainStyledText(collapsed)
	for i := 0; i < 7; i++ {
		if name := fmt.Sprintf("tool%d", i); strings.Contains(collapsedPlain, name) {
			t.Fatalf("collapsed disclosure leaked %s: %q", name, collapsedPlain)
		}
	}

	if !m.toggleToolDisclosure(record.id) || !record.expanded {
		t.Fatal("completed disclosure did not expand")
	}
	expanded := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(expanded, "▾ 7 tools") || !strings.Contains(expanded, "… 2 earlier") ||
		!strings.Contains(expanded, "tool6") || strings.Contains(expanded, "tool0") {
		t.Fatalf("expanded disclosure = %q", expanded)
	}
	if !m.toggleToolDisclosure(record.id) || record.expanded {
		t.Fatal("completed disclosure did not re-collapse")
	}
	if got := strings.Join(transcriptTexts(m), "\n"); got != collapsed {
		t.Fatalf("re-collapse did not restore transcript:\n got %q\nwant %q", got, collapsed)
	}
	// The prose either side of the activity is untouched.
	if !strings.Contains(strings.Join(transcriptTexts(m), "\n"), "Done.") {
		t.Fatalf("collapse ate the answer: %#v", m.transcript)
	}
}

// A single call collapses too, and reads as one rather than "1 tools".
func TestSingleCallTurnCollapsesWithSingularWording(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run the tests")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	call := messages.ChatMessageToolCall{ID: "only", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	live := m.currentToolDisclosure()
	if live == nil || live.expanded || live.complete || len(live.rows) != 1 {
		t.Fatalf("single live disclosure = %#v", live)
	}
	if plain := plainStyledText(strings.Join(transcriptTexts(m), "\n")); !strings.Contains(plain, "▸ 1 tool") || strings.Contains(plain, "bash") {
		t.Fatalf("single live call did not start collapsed: %q", plain)
	}
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	rows := toolRows(m)
	if len(rows) != 1 || !strings.Contains(rows[0], "1 tool") {
		t.Fatalf("want a singular one-line rollup, got %#v", rows)
	}
	if strings.Contains(rows[0], "1 tools") {
		t.Fatalf("plural wording for a single call: %q", rows[0])
	}
	record := m.currentToolDisclosure()
	if record == nil || !record.complete || record.expanded {
		t.Fatalf("single-call disclosure = %#v, want complete and collapsed", record)
	}
	collapsed := strings.Join(transcriptTexts(m), "\n")
	if plain := plainStyledText(collapsed); !strings.Contains(plain, "▸ 1 tool") || strings.Contains(plain, "bash") {
		t.Fatalf("single-call collapsed transcript = %q", plain)
	}
	if !m.toggleToolDisclosure(record.id) || !record.expanded {
		t.Fatal("single-call disclosure did not expand")
	}
	if plain := plainStyledText(strings.Join(transcriptTexts(m), "\n")); !strings.Contains(plain, "▾ 1 tool") ||
		!strings.Contains(plain, "✓") || !strings.Contains(plain, "bash") || strings.Count(plain, "Done.") != 1 {
		t.Fatalf("single-call expanded transcript = %q", plain)
	}
	if !m.toggleToolDisclosure(record.id) || record.expanded {
		t.Fatal("single-call disclosure did not re-collapse")
	}
	if got := strings.Join(transcriptTexts(m), "\n"); got != collapsed {
		t.Fatalf("single-call re-collapse did not restore transcript:\n got %q\nwant %q", got, collapsed)
	}
}

func TestInterruptedTurnAutoCollapsesAndRemainsExpandable(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "slow", Name: "slow_tool"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) || !record.expanded {
		t.Fatalf("interrupted fixture did not open live disclosure: %#v", record)
	}

	r.endTurn(errors.New("provider unavailable"))
	collapsed := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if record.expanded || !record.complete || !strings.Contains(collapsed, "▸ 1 tool") || strings.Contains(collapsed, "slow_tool") {
		t.Fatalf("interrupted turn did not auto-collapse: record=%#v transcript=%q", record, collapsed)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interrupted disclosure did not reopen")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	if !strings.Contains(expanded, "failed slow_tool") {
		t.Fatalf("interrupted expansion lost failure status: %q", expanded)
	}
}

func TestInterruptedParallelBatchAutoCollapsesInStartOrder(t *testing.T) {
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
	if record == nil || len(record.rows) != 5 || record.expanded {
		t.Fatalf("interrupted parallel fixture = %#v", record)
	}
	r.endTurn(errors.New("provider unavailable"))
	if !record.complete || record.expanded {
		t.Fatalf("interrupted parallel disclosure = %#v, want completed and collapsed", record)
	}
	collapsed := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(collapsed, "▸ 5 tools") || strings.Contains(collapsed, "tool0") {
		t.Fatalf("interrupted parallel collapse = %q", collapsed)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interrupted parallel disclosure did not expand")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	previous := strings.Index(expanded, "▾ 5 tools")
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("tool%d", i)
		at := strings.Index(expanded, "failed "+name)
		if at <= previous || strings.Count(expanded, name) != 1 {
			t.Fatalf("interrupted expansion lost order/status at %s: %q", name, expanded)
		}
		previous = at
	}
}

func TestDetachedCancellationAutoCollapsesToolDisclosure(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("detach")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "slow", Name: "slow_tool"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatalf("detached fixture did not expand: %#v", record)
	}
	tui.ShowThinking("pondering")
	m.canceling = true

	r.abandonCanceledTurn(r.visibleTab())
	if !record.complete || record.expanded {
		t.Fatalf("detached disclosure = %#v, want completed and collapsed", record)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("detached disclosure did not reopen")
	}
	if expanded := plainStyledText(m.transcript[record.transcriptIndex].text); !strings.Contains(expanded, "canceled slow_tool") {
		t.Fatalf("detached disclosure lost canceled row: %q", expanded)
	}

	// The canceled trailer is the only way back to the turn's activity, and its
	// controls derive solely from the dock ID copies in abandonCanceledTurn.
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if trailer == nil {
		t.Fatal("canceled turn left no settled trailer")
	}
	if len(trailer.dock.toolIDs) != 1 || trailer.dock.toolIDs[0] != record.id {
		t.Fatalf("canceled trailer toolIDs = %v, want [%d]", trailer.dock.toolIDs, record.id)
	}
	if len(trailer.dock.reasoningIDs) != 1 {
		t.Fatalf("canceled trailer reasoningIDs = %v, want one record", trailer.dock.reasoningIDs)
	}
	var hasTools bool
	for _, f := range trailer.fields {
		if f.overlay == turnDockOverlayTools {
			hasTools = true
		}
	}
	if !hasTools {
		t.Fatalf("canceled trailer lost its tools control: %#v", trailer.fields)
	}
}

func TestToolDisclosureImagesOnlyAppearExpanded(t *testing.T) {
	withDisplayTTY(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 4, 4)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.imageBaseDir = dir
	m.busy = true
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	// The image belongs to the third semantic row of one parallel batch; every
	// call stays hidden behind the same disclosure header.
	calls := make([]messages.ChatMessageToolCall, 0, 6)
	for i := 0; i < 2; i++ {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("s%d", i), Name: "bash"})
	}
	shot := messages.ChatMessageToolCall{ID: "shot", Name: "screenshot"}
	calls = append(calls, shot)
	for i := 0; i < 3; i++ {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("t%d", i), Name: "bash"})
	}
	tui.AppendToolStart(calls)
	for _, call := range calls {
		result := "ok"
		if call.ID == shot.ID {
			result = path
		}
		tui.AppendToolEnd(call, result, time.Millisecond, nil)
	}

	// Collapsed disclosures expose no thumbnail sidecars or detail text.
	for index, entry := range m.transcript {
		if len(entry.images) == 0 {
			continue
		}
		if !strings.Contains(entry.text, "image: shot.png") {
			t.Fatalf("sidecar at %d does not match its row %q", index, entry.text)
		}
	}
	record := m.currentToolDisclosure()
	if record == nil || record.expanded {
		t.Fatalf("image tool disclosure = %#v", record)
	}
	if images := m.transcript[record.transcriptIndex].images; len(images) != 0 {
		t.Fatalf("collapsed disclosure leaked image sidecars: %#v", images)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("image tool disclosure did not expand")
	}
	if images := m.transcript[record.transcriptIndex].images; len(images) != 1 || images[0].Path != path {
		t.Fatalf("expanded disclosure image sidecars = %#v", images)
	}
	if plain := plainStyledText(m.transcript[record.transcriptIndex].text); !strings.Contains(plain, "image: shot.png") {
		t.Fatalf("expanded disclosure lost image caption: %q", plain)
	}
	writeImageFixture(t, path, 8, 2)
	if !m.refreshTranscriptImageSources(80) {
		t.Fatal("regenerated tool image did not refresh")
	}
	var canonical []transcriptImage
	for _, row := range record.rows {
		if row.callID == shot.ID {
			canonical = row.images
			break
		}
	}
	if len(canonical) != 1 || canonical[0].Width != 8 || canonical[0].Height != 2 {
		t.Fatalf("canonical tool image stayed stale: %#v", canonical)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("image tool disclosure did not re-collapse")
	}
	if images := m.transcript[record.transcriptIndex].images; len(images) != 0 {
		t.Fatalf("re-collapsed disclosure retained image sidecars: %#v", images)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("image tool disclosure did not reopen")
	}
	if images := m.transcript[record.transcriptIndex].images; len(images) != 1 || images[0].Width != 8 || images[0].Height != 2 {
		t.Fatalf("reopened disclosure resurrected stale image dimensions: %#v", images)
	}
}

func TestToolDisclosureSanitizesPrivateImageMarkerRunes(t *testing.T) {
	withDisplayTTY(t)
	dir := t.TempDir()
	marker := string(transcriptImageMarker(0))
	path := filepath.Join(dir, "shot"+marker+".png")
	writeImageFixture(t, path, 4, 4)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.imageBaseDir = dir
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

	tui.AppendToolEnd(call, "![preview"+marker+"]("+path+")", time.Millisecond, nil)
	entry := m.transcript[record.transcriptIndex].text
	if got := strings.Count(entry, marker); got != transcriptImageThumbnailRows {
		t.Fatalf("expanded image row contains %d marker runes, want %d generated slot rows", got, transcriptImageThumbnailRows)
	}
	if plain := plainStyledText(stripTranscriptImageMarkers(entry)); !strings.Contains(plain, "screenshot") {
		t.Fatalf("sanitized tool label was not preserved as text: %q", plain)
	}
	_, spans := transcriptBlockRowsWithImages(
		entry, false, 80, m.transcript[record.transcriptIndex].images, true, 10, 20,
	)
	if len(spans) != 1 {
		t.Fatalf("crafted label produced %d image spans, want one: %#v", len(spans), spans)
	}
}

func TestRegeneratedExpandedToolImagePreservesPhysicalViewportAnchor(t *testing.T) {
	const width = 80
	withDisplayTTY(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "changing.png")
	writeImageFixture(t, path, 2400, 270)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.imageBaseDir = dir
	m.nativeImages = true
	m.imageCellWidth = 10
	m.imageCellHeight = 20
	m.reasoningWidth = width
	m.beginTurn("inspect image")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	call := messages.ChatMessageToolCall{ID: "changing-image", Name: "screenshot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, path, time.Millisecond, nil)
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatal("changing-image disclosure did not expand")
	}
	var prose []string
	for i := 0; i < 30; i++ {
		prose = append(prose, fmt.Sprintf("ctx-%02d", i))
	}
	m.appendLine(strings.Join(prose, "\n"))
	proseIndex := len(m.transcript) - 1

	beforeRows := transcriptRowsText(m.transcriptRows(width))
	oldCount := m.entryVisualLineCount(record.transcriptIndex, width)
	m.followBottom = false
	m.scrollAnchor = m.entryVisualStart(proseIndex, width) + 10
	oldAnchor := m.scrollAnchor
	if oldAnchor >= len(beforeRows) {
		t.Fatalf("fixture anchor %d outside %d visual rows", oldAnchor, len(beforeRows))
	}
	if beforeRows[oldAnchor] != "ctx-10" {
		t.Fatalf("fixture top row at %d = %q", oldAnchor, beforeRows[oldAnchor])
	}

	writeImageFixture(t, path, 270, 2400)
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	afterRows := transcriptRowsText(m.transcriptRows(width))
	newCount := m.entryVisualLineCount(record.transcriptIndex, width)
	wantAnchor := oldAnchor + newCount - oldCount
	if m.scrollAnchor != wantAnchor {
		t.Fatalf("regenerated image anchor = %d, want %d (height %d -> %d)", m.scrollAnchor, wantAnchor, oldCount, newCount)
	}
	if m.scrollAnchor >= len(afterRows) {
		t.Fatalf("regenerated image anchor %d outside %d visual rows", m.scrollAnchor, len(afterRows))
	}
	if afterRows[m.scrollAnchor] != "ctx-10" {
		t.Fatalf("regenerated image shifted held top row at %d: %q", m.scrollAnchor, afterRows[m.scrollAnchor])
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
	call := messages.ChatMessageToolCall{ID: "wrapped", Name: longName}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	record := m.currentToolDisclosure()
	if record == nil || !record.complete || record.expanded {
		t.Fatalf("wrapped disclosure fixture = %#v", record)
	}
	var prose []string
	for i := 0; i < 30; i++ {
		prose = append(prose, fmt.Sprintf("ctx-%02d", i))
	}
	m.appendLine(strings.Join(prose, "\n"))
	proseIndex := len(m.transcript) - 1

	m.followBottom = false
	m.scrollAnchor = m.entryVisualStart(proseIndex, width) + 10
	collapsedAnchor := m.scrollAnchor
	beforeRows := transcriptRowsText(m.transcriptRows(width))
	if collapsedAnchor >= len(beforeRows) {
		t.Fatalf("fixture anchor %d outside %d visual rows", collapsedAnchor, len(beforeRows))
	}
	beforeTop := beforeRows[collapsedAnchor]
	if beforeTop != "ctx-10" {
		t.Fatalf("fixture top row = %q, want ctx-10", beforeTop)
	}

	trailer := m.turnTrailers[m.turnTrailerSeq]
	if !m.toggleTurnTrailerOverlay(trailer, turnDockOverlayTools) {
		t.Fatal("wrapped tool trailer did not expand")
	}
	if m.scrollAnchor <= collapsedAnchor {
		t.Fatalf("inline expansion did not preserve the held row: anchor=%d, was %d", m.scrollAnchor, collapsedAnchor)
	}
	expandedRows := transcriptRowsText(m.transcriptRows(width))
	if got := expandedRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("top physical row moved under overlay: %q -> %q", beforeTop, got)
	}
	if !m.toggleTurnTrailerOverlay(trailer, turnDockOverlayTools) {
		t.Fatal("wrapped tool trailer did not re-collapse")
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
	if record == nil || !record.complete || record.expanded {
		t.Fatalf("completed disclosure = %#v, want complete and collapsed", record)
	}
	for i := 0; i < 6; i++ {
		m.appendLine(fmt.Sprintf("later row %d", i))
	}

	// The settled tool block stays inline and clickable. Both batches of the
	// unbroken run share one aggregated record.
	rows := m.transcriptRows(width)
	visible := m.visibleToolDisclosurePlacements(fullViewport(len(rows), width))
	if len(visible) != 1 || len(visible[0].recordIDs) != 1 || visible[0].recordIDs[0] != record.id {
		t.Fatalf("completed tools lost their transcript hitbox: %#v", visible)
	}

	placements := m.visibleTurnTrailerPlacements(fullViewport(len(rows), width))
	var toolPlacement *turnTrailerPlacement
	for i := range placements {
		if placements[i].overlay == turnDockOverlayTools {
			toolPlacement = &placements[i]
		}
	}
	if toolPlacement == nil {
		t.Fatalf("completed tool dock placement = %#v record=%#v", placements, record)
	}
	m.turnTrailerPlacements = placements
	p := *toolPlacement
	if !m.toggleTurnTrailerAt(p.X+1, p.Y) || m.openTurnTrailerID == 0 {
		t.Fatal("clicking the completed tool trailer control did not open it")
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
	if first == nil || second == nil || first.id == second.id || !first.complete || !second.complete || first.expanded || second.expanded {
		t.Fatalf("per-turn disclosures: first=%#v second=%#v", first, second)
	}
	if len(first.rows) != 4 || len(second.rows) != 4 || len(m.toolDisclosures) != 2 {
		t.Fatalf("per-turn row counts: first=%d second=%d records=%d", len(first.rows), len(second.rows), len(m.toolDisclosures))
	}
	if rows := toolRows(m); len(rows) != 2 {
		t.Fatalf("completed turns rendered %d disclosure entries, want 2: %#v", len(rows), m.transcript)
	}

	if !m.toggleToolDisclosure(first.id) || !first.expanded || second.expanded {
		t.Fatalf("opening first turn affected second: first=%#v second=%#v", first, second)
	}
	if !m.toggleToolDisclosure(second.id) || !first.expanded || !second.expanded {
		t.Fatalf("opening second turn affected first: first=%#v second=%#v", first, second)
	}
	if !m.toggleToolDisclosure(first.id) || first.expanded || !second.expanded {
		t.Fatalf("re-collapsing first turn affected second: first=%#v second=%#v", first, second)
	}
}
