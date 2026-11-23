package main

import (
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestCollapsedActivityHandoffUsesBatchIdentity(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("NO_COLOR", "")
	const width = 100
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.affordances.enabled = true
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	at := time.Unix(100, 0)
	spansAt := func(now time.Time) []affordanceSpan {
		t.Helper()
		rows := m.transcriptRowsAt(width, now)
		spans := m.affordanceSpans(now, fullViewport(len(rows), width), image.Point{}, false)
		for _, span := range spans {
			if !span.handoff {
				t.Fatalf("unexpected animation in collapsed activity: %+v", span)
			}
		}
		return spans
	}

	tui.ShowThinking("which files?")
	if spans := spansAt(at); len(spans) != 0 {
		t.Fatalf("initial label animated: %+v", spans)
	}
	m.thinkingSegmentStart = time.Now().Add(-3 * time.Second)
	m.visual.invalidate()
	if spans := spansAt(at.Add(time.Second)); len(spans) != 0 {
		t.Fatalf("thought timer restarted animation: %+v", spans)
	}

	first, second := readCall("first"), readCall("second")
	tui.AppendToolStart([]messages.ChatMessageToolCall{first, second})
	started := at.Add(2 * time.Second)
	spans := spansAt(started)
	if got := plainStyledText(activityBlocks(m, width)[0].text); got != "▸ Reading 2 files" {
		t.Fatalf("handoff didn't immediately show the new label: %q", got)
	}
	if len(spans) != 1 || spans[0].at != started || spans[0].x != 2 || spans[0].cols != len("Reading 2 files") {
		t.Fatalf("handoff should cover just the new label: %+v", spans)
	}
	tui.AppendToolEnd(first, "ok", time.Millisecond, nil)
	if spans := spansAt(started.Add(50 * time.Millisecond)); len(spans) != 1 || spans[0].at != started {
		t.Fatalf("count change restarted animation: %+v", spans)
	}
	tui.AppendToolEnd(second, "ok", time.Millisecond, nil)
	if spans := spansAt(started.Add(activityHandoffDuration)); len(spans) != 0 {
		t.Fatalf("handoff didn't expire: %+v", spans)
	}

	// The next completed batch merges into the same phrase. Its identity
	// still changes even if no frame saw it running on its own.
	tui.ModelAttemptStarted(1, 0)
	third := readCall("third")
	tui.AppendToolStart([]messages.ChatMessageToolCall{third})
	tui.AppendToolEnd(third, "ok", time.Millisecond, nil)
	later := started.Add(time.Second)
	if spans := spansAt(later); len(spans) != 1 || spans[0].at != later {
		t.Fatalf("new batch didn't animate after merging: %+v", spans)
	}
	t.Setenv("NO_COLOR", "1")
	if spans := spansAt(later.Add(50 * time.Millisecond)); len(spans) != 0 {
		t.Fatalf("NO_COLOR still emitted a handoff: %+v", spans)
	}
}

func TestCollapsedActivityHoldsLabelAndCatchesUp(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("NO_COLOR", "1")
	const width = 80
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	at := time.Unix(100, 0)
	check := func(now time.Time, want string) {
		t.Helper()
		rows := transcriptRowsText(m.transcriptRowsAt(width, now))
		for _, row := range rows {
			if row == want {
				return
			}
		}
		t.Fatalf("at %s: want row %q, got %q", now.Sub(at), want, rows)
	}
	start := func(id string) messages.ChatMessageToolCall {
		tui.ModelAttemptStarted(1, 0)
		call := readCall(id)
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		return call
	}
	first := start("first")
	check(at, "▸ Reading first.go")
	tui.AppendToolEnd(first, "ok", time.Millisecond, nil)
	check(at.Add(time.Millisecond), "▸ Reading first.go")
	second := start("second")
	check(at.Add(100*time.Millisecond), "▸ Reading first.go")
	tui.AppendToolEnd(second, "ok", time.Millisecond, nil)
	third := start("third")
	check(at.Add(collapsedActivityMinimum-time.Millisecond), "▸ Reading first.go")
	// No new event at the deadline: the visual cache must let the label advance.
	next := at.Add(collapsedActivityMinimum)
	check(next, "▸ Reading third.go")
	tui.AppendToolEnd(third, "ok", time.Millisecond, nil)
	check(next.Add(time.Millisecond), "▸ Reading third.go")
	tui.CompleteTurn(turnCompletion{Elapsed: time.Second, ProgressSaved: true})
	r.endTurn(nil)
	check(next.Add(2*time.Millisecond), "▸ Read 3 files")
}

func TestCollapsedActivityDoesNotHoldBackFailure(t *testing.T) {
	withDisplayTTY(t)
	const width = 80
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("test")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "test", Name: "bash", Arguments: `{"command":"go test ./..."}`}
	at := time.Unix(100, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	m.transcriptRowsAt(width, at)
	tui.AppendToolEnd(call, "", time.Millisecond, errors.New("tests failed"))
	if got := strings.Join(transcriptRowsText(m.transcriptRowsAt(width, at.Add(time.Millisecond))), "\n"); !strings.Contains(got, "▸ Tests failed") {
		t.Fatalf("failure didn't replace the live label immediately: %q", got)
	}
}

func TestActivityHandoffFrameChangesOnlyInkAndRestoresIt(t *testing.T) {
	at := time.Unix(100, 0)
	base := ui.Cell{Rune: 'R', Style: ui.NewStyle(ui.NewColorRGB(120, 120, 120), ui.ColorBlack, ui.ModifierBold|ui.ModifierDim)}
	c := affordanceCell{base: base, span: affordanceSpan{handoff: true, color: ui.NewColorRGB(220, 220, 220), at: at, duration: activityHandoffDuration}}
	for _, sample := range []struct {
		elapsed time.Duration
		want    ui.Color
	}{
		{0, ui.NewColorRGB(180, 180, 180)},
		{activityHandoffDuration / 2, ui.NewColorRGB(150, 150, 150)},
		{activityHandoffDuration, base.Style.Fg},
	} {
		want := base
		want.Style.Fg = sample.want
		if got := c.frame(at.Add(sample.elapsed)); got != want {
			t.Fatalf("at %s: got %+v, want %+v", sample.elapsed, got, want)
		}
	}
	c.base.Style.Fg, c.span.color = ui.ColorYellow, ui.ColorBlue
	if c.frame(at).Style.Fg != ui.ColorBlue || c.frame(at.Add(activityHandoffDuration/2)) != c.base {
		t.Fatal("palette-only handoff should use the native accent and restore the native ink")
	}
}

func TestCollapsedActivityKeepsFailureAfterAdvancing(t *testing.T) {
	withDisplayTTY(t)
	const width = 80
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	bad := messages.ChatMessageToolCall{ID: "bad", Name: "bash", Arguments: `{"command":"false"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{bad})
	tui.AppendToolEnd(bad, "", time.Millisecond, errors.New("command failed"))
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("try a read instead")
	good := readCall("next")
	tui.AppendToolStart([]messages.ChatMessageToolCall{good})
	check := func(prefix, suffix string) {
		t.Helper()
		blocks := activityBlocks(m, width)
		if len(blocks) != 1 {
			t.Fatalf("activity split into %d blocks", len(blocks))
		}
		if got := plainStyledText(blocks[0].text); !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) || strings.ContainsAny(got, "\n→") {
			t.Fatalf("want one line with %q and %q, got %q", prefix, suffix, got)
		}
	}
	// The command and the read share a run: the header names the call in
	// flight and keeps the cue for the one that failed.
	check("▸ Reading next.go", "1 failed")
	tui.AppendToolEnd(good, "ok", time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	tui.CompleteTurn(turnCompletion{Elapsed: 34 * time.Second, ProgressSaved: true})
	r.endTurn(nil)
	check("▸ Read next.go and ran a command", "1 failed")
	rows := m.transcriptRows(width)
	// The turn's time is on the prompt's row, not beside a chunk.
	if prompt := transcriptRowsText(rows)[0]; !strings.HasPrefix(prompt, "▎ inspect") || !strings.HasSuffix(prompt, "34.0s") {
		t.Fatalf("prompt row = %q, want the turn's time at its right edge", prompt)
	}
	m.placeDisclosures(fullViewport(len(rows), width))
	header := chunkHit(t, m, func(disclosurePlacement) bool { return true })
	if !m.toggleDisclosureAt(header.X, header.Y, width) {
		t.Fatal("the header didn't open its chunk")
	}
	b := activityBlocks(m, width)[0]
	calls := 0
	for _, target := range b.activityTargets {
		if target.key != "" && target.action == "" {
			calls++
		}
	}
	if calls != 2 || !strings.Contains(plainStyledText(b.text), "$ false · failed") {
		t.Fatalf("the open chunk lost the earlier failure: %q", plainStyledText(b.text))
	}
}
