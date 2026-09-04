package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	ui "github.com/metaspartan/gotui/v5"
)

// childTestRuns is a turn runner for child-tab tests. A prompt of "delegate"
// stands for a parent turn blocked in a tool call until release closes;
// "wait" blocks until the turn is canceled; "slow" waits for slow to close
// and then replies; a prompt in brackets is a report turn on the parent,
// whose message is recorded; anything else replies "found <prompt>".
type childTestRuns struct {
	release chan struct{}
	slow    chan struct{}
	mu      sync.Mutex
	reports []string
}

func (c *childTestRuns) run(ctx context.Context, prompt string, turnUI TurnUI) error {
	switch {
	case prompt == "delegate":
		<-c.release
		return nil
	case prompt == "wait":
		<-ctx.Done()
		return context.Cause(ctx)
	case strings.HasPrefix(prompt, "agent ") || strings.HasSuffix(prompt, " agent reports"):
		c.mu.Lock()
		c.reports = append(c.reports, turnUI.(*gotuiTurnUI).turn.userMessage.Content)
		c.mu.Unlock()
		return nil
	case prompt == "slow":
		<-c.slow
	}
	turnUI.AppendAssistantText("found " + prompt)
	turnUI.FinishTextTurn()
	turnUI.RecordTurnTokens(7, 3)
	return nil
}

func (c *childTestRuns) reported() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.reports...)
}

func newChildTestREPL(t *testing.T) (*managedREPL, *childTestRuns) {
	t.Helper()
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	runs := &childTestRuns{release: make(chan struct{}), slow: make(chan struct{})}
	r.runTurn = runs.run
	return r, runs
}

// beginParentToolCall puts the parent tab into a turn blocked in a tool
// call, with a disclosure row for the call, as a real spawn would find it.
func beginParentToolCall(t *testing.T, r *managedREPL, runs *childTestRuns, callID string) {
	t.Helper()
	r.model.mu.Lock()
	r.model.beginTurn("delegate")
	r.model.appendToolStartRow(callID, "spawn_agent explore")
	r.model.mu.Unlock()
	r.startTurn(context.Background(), "delegate", runs.run)
}

// spawnFromTool calls RunChild the way the spawn tool does, on a goroutine
// of its own; the test drives the event loop side.
func spawnFromTool(ctx context.Context, r *managedREPL, parent *replTab, req subagent.Request, callID string) <-chan childReport {
	parent.model.mu.Lock()
	turnID := parent.model.turnID
	parent.model.mu.Unlock()
	tui := &gotuiTurnUI{repl: r, model: parent.model, config: r.config, state: parent.state, turnID: turnID}
	ctx = withToolCall(ctx, messages.ChatMessageToolCall{ID: callID, Name: subagent.ToolName})
	out := make(chan childReport, 1)
	go func() {
		res, err := tui.RunChild(ctx, req)
		out <- childReport{result: res, err: err}
	}()
	return out
}

func runUITask(t *testing.T, r *managedREPL) {
	t.Helper()
	select {
	case task := <-r.uiTasks:
		task()
	case <-time.After(5 * time.Second):
		t.Fatal("no UI task arrived")
	}
}

// settleUntil drives the loop's settle step on each wake until cond holds.
// Wakes coalesce, so a single wake may precede the goroutine it announces.
func settleUntil(t *testing.T, r *managedREPL, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-r.tabEvents:
		case <-deadline:
			t.Fatal("the loop did not reach the expected state")
		}
		if err := r.settleTabs(context.Background(), r.runTurn); err != nil {
			t.Fatal(err)
		}
	}
}

func settled(tab *replTab) func() bool {
	return func() bool { return tab.turnDone == nil }
}

func awaitReport(t *testing.T, out <-chan childReport) childReport {
	t.Helper()
	select {
	case rep := <-out:
		return rep
	case <-time.After(5 * time.Second):
		t.Fatal("the spawn call did not return")
		return childReport{}
	}
}

func TestBlockingChildRunsInATabAndAnswersTheCall(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	beginParentToolCall(t, r, runs, "call-1")
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "look around", Label: "explore"}, "call-1")
	runUITask(t, r)

	if len(r.tabs) != 2 || r.tabs[1].parent != parent || r.visibleTab() != parent {
		t.Fatalf("tabs after spawning: %d, visible %d", len(r.tabs), r.visibleTabIndex())
	}
	child := r.tabs[1]
	if got := strings.Join(r.tabLines(), "\n"); !strings.Contains(got, "2  ↳ "+child.name) {
		t.Fatalf("tab list does not nest the child: %q", got)
	}
	if got := plainStyledText(r.model.fullTranscript()); strings.Contains(got, "started in tab") {
		t.Fatalf("a blocking spawn announced itself: %q", got)
	}
	// The parent's tool row names the child and shows what it is doing.
	r.model.mu.Lock()
	r.model.activeToolsPhase = -1
	r.model.refreshActiveTools()
	record := r.model.currentToolDisclosure()
	record.expanded = true
	rowText, _ := toolDisclosureText(record)
	r.model.mu.Unlock()
	if got := plainStyledText(rowText); !strings.Contains(got, "spawn_agent explore · "+child.name+" · ") {
		t.Fatalf("parent tool row = %q", got)
	}

	settleUntil(t, r, settled(child))
	rep := awaitReport(t, result)
	if rep.err != nil || rep.result.Text != "found look around" || rep.result.Session != child.name || rep.result.InputTokens != 7 {
		t.Fatalf("spawn result = %+v, %v", rep.result, rep.err)
	}
	if child.report != nil || child.waiter != nil || child.turnDone != nil {
		t.Fatal("the child kept its report after answering")
	}
	if len(r.tabs) != 2 {
		t.Fatal("the child closed before the parent's turn settled")
	}

	// The parent settles: nobody looked at the child, so it goes.
	close(runs.release)
	settleUntil(t, r, settled(parent))
	if len(r.tabs) != 1 || r.visibleTab() != parent {
		t.Fatalf("tabs after the parent settled: %d", len(r.tabs))
	}
}

func TestBackgroundChildReportsToTheIdleParent(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "look", Background: true}, "")
	runUITask(t, r)
	rep := awaitReport(t, result)
	if rep.err != nil || !rep.result.Started || len(r.tabs) != 2 || rep.result.Session != r.tabs[1].name {
		t.Fatalf("background spawn = %+v, %v, tabs %d", rep.result, rep.err, len(r.tabs))
	}
	child := r.tabs[1]

	// The child settles; the idle parent takes the report as its next turn.
	settleUntil(t, r, settled(child))
	if parent.turnDone == nil {
		t.Fatal("the report did not start a parent turn")
	}
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "> agent "+child.name+" finished") {
		t.Fatalf("parent transcript lacks the report echo: %q", got)
	}
	settleUntil(t, r, settled(parent))
	reports := runs.reported()
	want := "agent " + child.name + " finished\nfound look\n\n(agent session " + child.name + " · 7 in / 3 out)"
	if len(reports) != 1 || reports[0] != want {
		t.Fatalf("parent got %q, want %q", reports, want)
	}
	if len(r.tabs) != 1 {
		t.Fatal("the reported child stayed open unviewed")
	}
}

func TestReportsArrivingDuringAParentTurnArriveAsOneMessage(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	beginParentToolCall(t, r, runs, "call-1")
	first := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "alpha", Background: true}, "")
	runUITask(t, r)
	second := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "beta", Background: true}, "")
	runUITask(t, r)
	awaitReport(t, first)
	awaitReport(t, second)
	settleUntil(t, r, func() bool { return r.tabs[1].turnDone == nil && r.tabs[2].turnDone == nil })
	if len(parent.reports) != 2 {
		t.Fatalf("%d reports pending on the busy parent, want 2", len(parent.reports))
	}
	if got := runs.reported(); len(got) != 0 {
		t.Fatalf("reports reached the parent mid-turn: %q", got)
	}

	close(runs.release)
	settleUntil(t, r, func() bool { return len(parent.reports) == 0 })
	if parent.turnDone == nil {
		t.Fatal("the pending reports did not start a parent turn")
	}
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "> 2 agent reports") {
		t.Fatalf("parent transcript lacks the coalesced echo: %q", got)
	}
	settleUntil(t, r, settled(parent))
	reports := runs.reported()
	if len(reports) != 1 || !strings.Contains(reports[0], "found alpha") || !strings.Contains(reports[0], "found beta") {
		t.Fatalf("parent got %q, want one message with both replies", reports)
	}
	if len(r.tabs) != 1 {
		t.Fatalf("%d tabs after the report turn, want the parent alone", len(r.tabs))
	}
}

func TestEscOnAChildCancelsOnlyTheChildAndAViewedChildStays(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	beginParentToolCall(t, r, runs, "call-1")
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "wait"}, "call-1")
	runUITask(t, r)
	child := r.tabs[1]

	r.runTabCommand("/tab 2")
	if r.visibleTab() != child || !child.viewed {
		t.Fatal("/tab 2 did not show the child")
	}
	r.model.mu.Lock()
	r.cancelBusyTurn()
	r.model.mu.Unlock()
	settleUntil(t, r, settled(child))
	rep := awaitReport(t, result)
	if !errors.Is(rep.err, context.Canceled) || rep.result.Session != child.name {
		t.Fatalf("spawn result after Esc = %+v, %v", rep.result, rep.err)
	}
	if parent.turnDone == nil {
		t.Fatal("canceling the child canceled the parent")
	}

	r.runTabCommand("/tab 1")
	close(runs.release)
	settleUntil(t, r, settled(parent))
	if len(r.tabs) != 2 || r.tabs[1] != child || child.model.busy {
		t.Fatalf("a viewed child did not stay: %d tabs", len(r.tabs))
	}
}

func TestOrphanedBlockingChildReportsLikeABackgroundOne(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	ctx, cancel := context.WithCancel(context.Background())
	result := spawnFromTool(ctx, r, parent, subagent.Request{Task: "slow"}, "")
	runUITask(t, r)
	child := r.tabs[1]

	// The parent's call stops waiting; the child works on.
	cancel()
	rep := awaitReport(t, result)
	if !errors.Is(rep.err, context.Canceled) || rep.result.Session != child.name {
		t.Fatalf("spawn result after cancel = %+v, %v", rep.result, rep.err)
	}
	runUITask(t, r) // orphanChild
	if child.waiter != nil || child.turnDone == nil {
		t.Fatal("the child did not carry on without a waiter")
	}
	close(runs.slow)
	settleUntil(t, r, settled(child))
	if parent.turnDone == nil {
		t.Fatal("the orphaned child's reply did not reach the parent")
	}
	settleUntil(t, r, settled(parent))
	if got := runs.reported(); len(got) != 1 || !strings.Contains(got[0], "found slow") {
		t.Fatalf("parent got %q", got)
	}
}

func TestSpawnCommandStartsABackgroundChild(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	r.runTabCommand("/spawn count the files")
	if len(r.tabs) != 2 || r.tabs[1].parent != parent || r.visibleTab() != parent {
		t.Fatalf("tabs after /spawn: %d, visible %d", len(r.tabs), r.visibleTabIndex())
	}
	child := r.tabs[1]
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "agent "+child.name+" started in tab 2 · /tab "+child.name) {
		t.Fatalf("/spawn was not announced: %q", got)
	}
	settleUntil(t, r, settled(child))
	settleUntil(t, r, settled(parent))
	if got := runs.reported(); len(got) != 1 || !strings.Contains(got[0], "found count the files") {
		t.Fatalf("parent got %q", got)
	}
	r.runTabCommand("/spawn")
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "usage: /spawn <brief>") {
		t.Fatalf("empty /spawn was not refused: %q", got)
	}
}

func TestAltKeysSwitchTabs(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "a-work", "b-work", "c-work")
	press := func(id string) {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
		r.applyTabRequests()
	}
	for _, step := range []struct {
		key  string
		want int
	}{{"<M-1>", 0}, {"<M-]>", 1}, {"<M-[>", 0}, {"<M-[>", 2}, {"<M-7>", 2}, {"<M-2>", 1}} {
		press(step.key)
		if got := r.visibleTabIndex(); got != step.want {
			t.Fatalf("after %s visible tab = %d, want %d", step.key, got, step.want)
		}
	}
	if _, ok := tabShortcut("<M-x>", 0, 3); ok {
		t.Fatal("an unrelated Alt key switched tabs")
	}
	if i, ok := tabShortcut("<M-[>", -1, 1); !ok || i != 0 {
		t.Fatalf("Alt-[ on a lone placeholder tab = %d %v", i, ok)
	}
}
