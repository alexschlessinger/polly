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
	for _, e := range m.transcript {
		// "tool call" matches the rollup in both its singular and plural forms.
		if strings.Contains(e, "→") || strings.Contains(e, "✓") ||
			strings.Contains(e, "✗") || strings.Contains(e, "tool call") {
			rows = append(rows, e)
		}
	}
	return rows
}

func clickToolDisclosure(t *testing.T, m *replModel, recordID int64, width int) {
	t.Helper()
	rows := m.transcriptRows(width)
	placements := m.visibleToolDisclosurePlacements(len(rows), len(rows), 0, 0, width, false, false)
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
	tui := &gotuiTurnUI{repl: r, config: r.config}

	first := messages.ChatMessageToolCall{ID: "a", Name: "alpha"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{first})
	record := m.currentToolDisclosure()
	if record == nil || record.complete || record.expanded || len(record.rows) != 1 {
		t.Fatalf("first live disclosure = %#v, want one active collapsed row", record)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("first live call rendered %d tool entries, want one: %#v", len(rows), m.transcript)
	}
	collapsed := plainStyledText(strings.Join(m.transcript, "\n"))
	if !strings.Contains(collapsed, "▸ 1 tool call") || strings.Contains(collapsed, "alpha") {
		t.Fatalf("first live call was not private behind its header: %q", collapsed)
	}

	clickToolDisclosure(t, m, record.id, width)
	expanded := plainStyledText(m.transcript[record.transcriptIndex])
	if !record.expanded || !strings.Contains(expanded, "▾ 1 tool call") || !strings.Contains(expanded, "alpha") {
		t.Fatalf("first live expansion = %q record=%#v", expanded, record)
	}

	second := messages.ChatMessageToolCall{ID: "b", Name: "beta"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{second})
	expanded = plainStyledText(m.transcript[record.transcriptIndex])
	if !strings.Contains(expanded, "▾ 2 tool calls") || strings.Count(expanded, "alpha") != 1 || strings.Count(expanded, "beta") != 1 {
		t.Fatalf("expanded live disclosure did not add the second call once: %q", expanded)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("expanded live calls escaped into %d transcript entries: %#v", len(rows), m.transcript)
	}

	tui.AppendToolEnd(first, "RAW ALPHA RESULT", time.Second, nil)
	expanded = plainStyledText(m.transcript[record.transcriptIndex])
	if !strings.Contains(expanded, "✓") || !strings.Contains(expanded, "alpha") || !strings.Contains(expanded, "beta") || strings.Contains(expanded, "RAW ALPHA RESULT") {
		t.Fatalf("expanded live completion did not update safely: %q", expanded)
	}

	clickToolDisclosure(t, m, record.id, width)
	recollapsed := plainStyledText(strings.Join(m.transcript, "\n"))
	if record.expanded || !strings.Contains(recollapsed, "▸ 2 tool calls") || strings.Contains(recollapsed, "alpha") || strings.Contains(recollapsed, "beta") {
		t.Fatalf("live disclosure did not hide every row: %q record=%#v", recollapsed, record)
	}
}

func TestToolDisclosureHoldsPositionWhileProseInterleaves(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	first := messages.ChatMessageToolCall{ID: "a", Name: "alpha"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{first})
	tui.AppendToolEnd(first, "ok", time.Second, nil)
	tui.AppendAssistantText("thinking about it")
	for _, id := range []string{"b", "c", "d"} {
		call := messages.ChatMessageToolCall{ID: id, Name: "tool_" + id}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Second, nil)
	}

	record := m.currentToolDisclosure()
	if record == nil || record.transcriptIndex < 0 || record.expanded || len(record.rows) != 4 {
		t.Fatalf("interleaved disclosure = %#v", record)
	}
	if rows := toolRows(m); len(rows) != 1 {
		t.Fatalf("interleaved activity rendered %d tool entries: %#v", len(rows), m.transcript)
	}
	collapsed := plainStyledText(strings.Join(m.transcript, "\n"))
	headerAt := strings.Index(collapsed, "▸ 4 tool calls")
	proseAt := strings.Index(collapsed, "thinking about it")
	if headerAt < 0 || proseAt <= headerAt {
		t.Fatalf("assistant prose moved ahead of its tool disclosure: %q", collapsed)
	}
	for _, hidden := range []string{"alpha", "tool_b", "tool_c", "tool_d"} {
		if strings.Contains(collapsed, hidden) {
			t.Fatalf("collapsed interleaved disclosure leaked %q: %q", hidden, collapsed)
		}
	}

	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interleaved disclosure did not expand")
	}
	expanded := plainStyledText(strings.Join(m.transcript, "\n"))
	previous := strings.Index(expanded, "▾ 4 tool calls")
	for _, want := range []string{"alpha", "tool_b", "tool_c", "tool_d"} {
		at := strings.Index(expanded, want)
		if at <= previous || strings.Count(expanded, want) != 1 {
			t.Fatalf("expanded interleaved order failed at %q: %q", want, expanded)
		}
		previous = at
	}
	if proseAt = strings.Index(expanded, "thinking about it"); proseAt <= previous {
		t.Fatalf("expansion moved prose ahead of tool detail: %q", expanded)
	}
}

func TestExpandedParallelToolDisclosureUpdatesInStartOrder(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run parallel")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	calls := make([]messages.ChatMessageToolCall, 6)
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
	if collapsed := plainStyledText(strings.Join(m.transcript, "\n")); !strings.Contains(collapsed, "▸ 6 tool calls") || strings.Contains(collapsed, "tool0") {
		t.Fatalf("parallel batch did not start collapsed: %q", collapsed)
	}

	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("parallel live disclosure did not expand")
	}
	order := []int{5, 1, 3, 0, 4, 2}
	for settled, i := range order {
		tui.AppendToolEnd(calls[i], "ok", time.Duration(i+1)*time.Second, nil)
		expanded := plainStyledText(m.transcript[record.transcriptIndex])
		if !strings.Contains(expanded, "▾ 6 tool calls") || strings.Count(expanded, "✓") != settled+1 {
			t.Fatalf("after %d parallel completions, disclosure = %q", settled+1, expanded)
		}
		previous := strings.Index(expanded, "▾ 6 tool calls")
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

func TestToolDisclosureAggregatesBatchesWithinTurn(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run batches")
	tui := &gotuiTurnUI{repl: r, config: r.config}

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
		plain := plainStyledText(strings.Join(m.transcript, "\n"))
		wantHeader := fmt.Sprintf("▸ %d tool calls", (batch+1)*len(calls))
		if !strings.Contains(plain, wantHeader) || strings.Contains(plain, "tool_0_0") {
			t.Fatalf("after batch %d collapsed transcript = %q, want %q with no detail", batch, plain, wantHeader)
		}
	}
	record := m.currentToolDisclosure()
	if record == nil || len(record.rows) != 15 || record.expanded {
		t.Fatalf("batched disclosure = %#v", record)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("batched disclosure did not expand")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex])
	previous := strings.Index(expanded, "▾ 15 tool calls")
	for batch := 0; batch < 3; batch++ {
		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("tool_%d_%d", batch, i)
			at := strings.Index(expanded, name)
			if at <= previous || strings.Count(expanded, name) != 1 {
				t.Fatalf("batch order failed at %s: %q", name, expanded)
			}
			previous = at
		}
	}
}

// Completion auto-collapses the disclosure even if the user opened it live.
func TestCompletedTurnCollapsesToASingleRollup(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}

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
		t.Fatalf("expanded mid-turn disclosure escaped into %d transcript entries", got)
	}

	r.endTurn(nil)
	rows := toolRows(m)
	if len(rows) != 1 {
		t.Fatalf("a completed turn should keep one tool line, got %d: %#v", len(rows), m.transcript)
	}
	if !strings.Contains(rows[0], "7 tool calls") {
		t.Fatalf("rollup = %q", rows[0])
	}
	if record == nil || !record.complete || record.expanded {
		t.Fatalf("completed disclosure = %#v, want complete and collapsed", record)
	}
	collapsed := strings.Join(m.transcript, "\n")
	collapsedPlain := plainStyledText(collapsed)
	if !strings.Contains(collapsedPlain, "▸ 7 tool calls") {
		t.Fatalf("collapsed disclosure header = %q", collapsedPlain)
	}
	for i := 0; i < 7; i++ {
		if name := fmt.Sprintf("tool%d", i); strings.Contains(collapsedPlain, name) {
			t.Fatalf("collapsed disclosure leaked %s: %q", name, collapsedPlain)
		}
	}

	if !m.toggleToolDisclosure(record.id) || !record.expanded {
		t.Fatal("completed disclosure did not expand")
	}
	expanded := plainStyledText(strings.Join(m.transcript, "\n"))
	if !strings.Contains(expanded, "▾ 7 tool calls") {
		t.Fatalf("expanded disclosure header = %q", expanded)
	}
	previous := strings.Index(expanded, "▾ 7 tool calls")
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("tool%d", i)
		at := strings.Index(expanded, name)
		if at <= previous {
			t.Fatalf("completed rows are not in start order at %s: %q", name, expanded)
		}
		if got := strings.Count(expanded, name); got != 1 {
			t.Fatalf("completed row %s appears %d times: %q", name, got, expanded)
		}
		previous = at
	}
	if at := strings.Index(expanded, "Done."); at <= previous {
		t.Fatalf("assistant prose moved ahead of tool detail: %q", expanded)
	}
	if !m.toggleToolDisclosure(record.id) || record.expanded {
		t.Fatal("completed disclosure did not re-collapse")
	}
	if got := strings.Join(m.transcript, "\n"); got != collapsed {
		t.Fatalf("re-collapse did not restore transcript:\n got %q\nwant %q", got, collapsed)
	}
	// The prose either side of the activity is untouched.
	if !strings.Contains(strings.Join(m.transcript, "\n"), "Done.") {
		t.Fatalf("collapse ate the answer: %#v", m.transcript)
	}
}

// A single call collapses too, and reads as one rather than "1 tool calls".
func TestSingleCallTurnCollapsesWithSingularWording(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run the tests")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	call := messages.ChatMessageToolCall{ID: "only", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	live := m.currentToolDisclosure()
	if live == nil || live.expanded || live.complete || len(live.rows) != 1 {
		t.Fatalf("single live disclosure = %#v", live)
	}
	if plain := plainStyledText(strings.Join(m.transcript, "\n")); !strings.Contains(plain, "▸ 1 tool call") || strings.Contains(plain, "bash") {
		t.Fatalf("single live call did not start collapsed: %q", plain)
	}
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	rows := toolRows(m)
	if len(rows) != 1 || !strings.Contains(rows[0], "1 tool call") {
		t.Fatalf("want a singular one-line rollup, got %#v", rows)
	}
	if strings.Contains(rows[0], "1 tool calls") {
		t.Fatalf("plural wording for a single call: %q", rows[0])
	}
	record := m.currentToolDisclosure()
	if record == nil || !record.complete || record.expanded {
		t.Fatalf("single-call disclosure = %#v, want complete and collapsed", record)
	}
	collapsed := strings.Join(m.transcript, "\n")
	if plain := plainStyledText(collapsed); !strings.Contains(plain, "▸ 1 tool call") || strings.Contains(plain, "bash") {
		t.Fatalf("single-call collapsed transcript = %q", plain)
	}
	if !m.toggleToolDisclosure(record.id) || !record.expanded {
		t.Fatal("single-call disclosure did not expand")
	}
	if plain := plainStyledText(strings.Join(m.transcript, "\n")); !strings.Contains(plain, "▾ 1 tool call") ||
		!strings.Contains(plain, "✓") || !strings.Contains(plain, "bash") || strings.Count(plain, "Done.") != 1 {
		t.Fatalf("single-call expanded transcript = %q", plain)
	}
	if !m.toggleToolDisclosure(record.id) || record.expanded {
		t.Fatal("single-call disclosure did not re-collapse")
	}
	if got := strings.Join(m.transcript, "\n"); got != collapsed {
		t.Fatalf("single-call re-collapse did not restore transcript:\n got %q\nwant %q", got, collapsed)
	}
}

func TestInterruptedTurnAutoCollapsesAndRemainsExpandable(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	call := messages.ChatMessageToolCall{ID: "slow", Name: "slow_tool"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) || !record.expanded {
		t.Fatalf("interrupted fixture did not open live disclosure: %#v", record)
	}

	r.endTurn(errors.New("provider unavailable"))
	collapsed := plainStyledText(strings.Join(m.transcript, "\n"))
	if record.expanded || !record.complete || !strings.Contains(collapsed, "▸ 1 tool call") || strings.Contains(collapsed, "slow_tool") {
		t.Fatalf("interrupted turn did not auto-collapse: record=%#v transcript=%q", record, collapsed)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interrupted disclosure did not reopen")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex])
	if !strings.Contains(expanded, "failed slow_tool") {
		t.Fatalf("interrupted expansion lost failure status: %q", expanded)
	}
}

func TestInterruptedParallelBatchAutoCollapsesInStartOrder(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("parallel")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	var calls []messages.ChatMessageToolCall
	for i := 0; i < 6; i++ {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("x%d", i), Name: fmt.Sprintf("tool%d", i)})
	}
	tui.AppendToolStart(calls)
	record := m.currentToolDisclosure()
	if record == nil || len(record.rows) != 6 || record.expanded {
		t.Fatalf("interrupted parallel fixture = %#v", record)
	}
	r.endTurn(errors.New("provider unavailable"))
	if !record.complete || record.expanded {
		t.Fatalf("interrupted parallel disclosure = %#v, want completed and collapsed", record)
	}
	collapsed := plainStyledText(strings.Join(m.transcript, "\n"))
	if !strings.Contains(collapsed, "▸ 6 tool calls") || strings.Contains(collapsed, "tool0") {
		t.Fatalf("interrupted parallel collapse = %q", collapsed)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("interrupted parallel disclosure did not expand")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex])
	previous := strings.Index(expanded, "▾ 6 tool calls")
	for i := 0; i < 6; i++ {
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
	tui := &gotuiTurnUI{repl: r, config: r.config}
	call := messages.ChatMessageToolCall{ID: "slow", Name: "slow_tool"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatalf("detached fixture did not expand: %#v", record)
	}
	m.canceling = true

	r.abandonCanceledTurn()
	if !record.complete || record.expanded {
		t.Fatalf("detached disclosure = %#v, want completed and collapsed", record)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("detached disclosure did not reopen")
	}
	if expanded := plainStyledText(m.transcript[record.transcriptIndex]); !strings.Contains(expanded, "canceled slow_tool") {
		t.Fatalf("detached disclosure lost canceled row: %q", expanded)
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
	tui := &gotuiTurnUI{repl: r, config: r.config}

	// The image belongs to the third semantic row even though every call stays
	// hidden behind the same disclosure header.
	for i := 0; i < 2; i++ {
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("s%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	}
	shot := messages.ChatMessageToolCall{ID: "shot", Name: "screenshot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{shot})
	tui.AppendToolEnd(shot, path, time.Millisecond, nil)

	for i := 0; i < 3; i++ {
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("t%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	}

	// Collapsed disclosures expose no thumbnail sidecars or detail text.
	for index, images := range m.transcriptImages {
		if len(images) == 0 {
			continue
		}
		if index >= len(m.transcript) || !strings.Contains(m.transcript[index], "image: shot.png") {
			t.Fatalf("sidecar at %d does not match its row %q", index, m.transcript[index])
		}
	}
	record := m.currentToolDisclosure()
	if record == nil || record.expanded {
		t.Fatalf("image tool disclosure = %#v", record)
	}
	if images := m.transcriptImages[record.transcriptIndex]; len(images) != 0 {
		t.Fatalf("collapsed disclosure leaked image sidecars: %#v", images)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("image tool disclosure did not expand")
	}
	if images := m.transcriptImages[record.transcriptIndex]; len(images) != 1 || images[0].Path != path {
		t.Fatalf("expanded disclosure image sidecars = %#v", images)
	}
	if plain := plainStyledText(m.transcript[record.transcriptIndex]); !strings.Contains(plain, "image: shot.png") {
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
	if images := m.transcriptImages[record.transcriptIndex]; len(images) != 0 {
		t.Fatalf("re-collapsed disclosure retained image sidecars: %#v", images)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("image tool disclosure did not reopen")
	}
	if images := m.transcriptImages[record.transcriptIndex]; len(images) != 1 || images[0].Width != 8 || images[0].Height != 2 {
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
	tui := &gotuiTurnUI{repl: r, config: r.config}
	call := messages.ChatMessageToolCall{ID: "private-rune", Name: "screen" + marker + "shot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatal("private-rune disclosure did not expand")
	}
	if strings.Contains(m.transcript[record.transcriptIndex], marker) {
		t.Fatal("running tool label was mistaken for an image marker")
	}

	tui.AppendToolEnd(call, "![preview"+marker+"]("+path+")", time.Millisecond, nil)
	entry := m.transcript[record.transcriptIndex]
	if got := strings.Count(entry, marker); got != transcriptImageThumbnailRows {
		t.Fatalf("expanded image row contains %d marker runes, want %d generated slot rows", got, transcriptImageThumbnailRows)
	}
	if plain := plainStyledText(stripTranscriptImageMarkers(entry)); !strings.Contains(plain, "screenshot") {
		t.Fatalf("sanitized tool label was not preserved as text: %q", plain)
	}
	_, spans := transcriptBlockRowsWithImages(
		entry, false, 80, m.transcriptImages[record.transcriptIndex], true, 10, 20,
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
	tui := &gotuiTurnUI{repl: r, config: r.config}
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

func TestToolDisclosureTogglePreservesPhysicalViewportAnchor(t *testing.T) {
	const width = 24
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.reasoningWidth = width
	m.beginTurn("wrap")
	tui := &gotuiTurnUI{repl: r, config: r.config}
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

	oldCount := m.entryVisualLineCount(record.transcriptIndex, width)
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("wrapped disclosure did not expand")
	}
	newCount := m.entryVisualLineCount(record.transcriptIndex, width)
	if newCount <= oldCount+1 {
		t.Fatalf("wrapped detail fixture grew %d -> %d rows", oldCount, newCount)
	}
	wantExpandedAnchor := collapsedAnchor + newCount - oldCount
	if m.scrollAnchor != wantExpandedAnchor {
		t.Fatalf("expanded scrollAnchor = %d, want %d", m.scrollAnchor, wantExpandedAnchor)
	}
	expandedRows := transcriptRowsText(m.transcriptRows(width))
	if got := expandedRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("top physical row moved on expansion: %q -> %q", beforeTop, got)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("wrapped disclosure did not re-collapse")
	}
	if m.scrollAnchor != collapsedAnchor {
		t.Fatalf("re-collapsed scrollAnchor = %d, want %d", m.scrollAnchor, collapsedAnchor)
	}
	recollapsedRows := transcriptRowsText(m.transcriptRows(width))
	if got := recollapsedRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("top physical row moved on re-collapse: %q -> %q", beforeTop, got)
	}
}

func TestCompletedToolDisclosureMouseHitboxTracksViewport(t *testing.T) {
	const width = 50
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run tools")
	tui := &gotuiTurnUI{repl: r, config: r.config}
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

	rows := m.transcriptRows(width)
	visible := m.visibleToolDisclosurePlacements(len(rows), 3, 0, 0, width, false, false)
	if len(visible) != 1 || visible[0].recordID != record.id {
		t.Fatalf("visible completed disclosure placement = %#v", visible)
	}
	if hidden := m.visibleToolDisclosurePlacements(len(rows), 3, 4, 0, width, false, false); len(hidden) != 0 {
		t.Fatalf("offscreen disclosure retained a click target: %#v", hidden)
	}

	m.toolDisclosurePlacements = visible
	p := visible[0]
	if !m.toggleToolDisclosureAt(p.X+1, p.Y) || !record.expanded {
		t.Fatal("clicking the completed tool header did not expand it")
	}
	if m.toggleToolDisclosureAt(p.X+p.Cols, p.Y) {
		t.Fatal("click immediately outside the header hitbox toggled the tool disclosure")
	}
	if m.toggleToolDisclosureAt(p.X+1, p.Y+1) {
		t.Fatal("clicking a tool detail row toggled the disclosure")
	}
}

func TestToolDisclosureResetsPerTurn(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	runTurn := func(prompt, prefix string) *toolDisclosureRecord {
		m.beginTurn(prompt)
		tui := &gotuiTurnUI{repl: r, config: r.config}
		for i := 0; i < 4; i++ {
			call := messages.ChatMessageToolCall{ID: fmt.Sprintf("%s%d", prefix, i), Name: fmt.Sprintf("%s_tool%d", prefix, i)}
			tui.AppendToolStart([]messages.ChatMessageToolCall{call})
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
