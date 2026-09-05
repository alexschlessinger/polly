package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
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
		tui := turnUI.(*gotuiTurnUI)
		if err := tui.state.session.AddReportMessage(ctx, tui.turn.userMessage, tui.turn.reportIDs); err != nil {
			return err
		}
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
	r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: callID, Name: subagent.ToolName, Arguments: `{"label":"explore"}`})
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
		case task := <-r.uiTasks:
			task()
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
	// The parent's Agents row links the child and shows its initial run.
	r.refreshAgentActivities()
	r.model.mu.Lock()
	r.model.activeToolsPhase = -1
	r.model.refreshActiveTools()
	record := r.model.currentToolDisclosure()
	rowText, links := r.model.agentDetail([]int64{record.id}, 100)
	r.model.mu.Unlock()
	if got := plainStyledText(rowText); !strings.Contains(got, "explore · ") || len(links) != 1 || record.rows[0].agent.session != child.name {
		t.Fatalf("parent agent row = %q, links %+v", got, links)
	}

	settleUntil(t, r, settled(child))
	rep := awaitReport(t, result)
	if rep.err != nil || rep.result.Text != "found look around" || rep.result.Session != child.name || rep.result.InputTokens != 7 {
		t.Fatalf("spawn result = %+v, %v", rep.result, rep.err)
	}
	if child.report != nil || child.waiter != nil || child.turnDone != nil {
		t.Fatal("the child kept its report after answering")
	}
	settleUntil(t, r, func() bool { return len(r.tabs) == 1 })
	if parent.turnDone == nil {
		t.Fatal("the parent stopped while closing the delivered child")
	}

	// The parent can finish normally after the delivered child closes.
	close(runs.release)
	settleUntil(t, r, settled(parent))
	settleUntil(t, r, func() bool { return len(r.tabs) == 1 })
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
	settleUntil(t, r, func() bool { return parent.turnDone != nil })
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
	settleUntil(t, r, func() bool { return len(r.tabs) == 1 })
	// The reports wait in the store: nothing is queued on the busy parent.
	parent.model.mu.Lock()
	queued := len(parent.model.queue)
	parent.model.mu.Unlock()
	if queued != 0 || len(runs.reported()) != 0 {
		t.Fatalf("reports reached the busy parent: queue %d, turns %q", queued, runs.reported())
	}

	close(runs.release)
	settleUntil(t, r, func() bool { return len(runs.reported()) == 1 })
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

func TestEscOnAChildCancelsOnlyTheChildAndClosesAfterLeaving(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	beginParentToolCall(t, r, runs, "call-1")
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "wait"}, "call-1")
	runUITask(t, r)
	child := r.tabs[1]

	r.runTabCommand("/tab 2")
	if r.visibleTab() != child {
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
	settleUntil(t, r, func() bool { return len(r.tabs) == 1 })
	if r.visibleTab() != parent {
		t.Fatal("leaving the child did not return to its parent")
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
	settleUntil(t, r, func() bool { return parent.turnDone != nil })
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
	runUITask(t, r)
	if len(r.tabs) != 2 || r.tabs[1].parent != parent || r.visibleTab() != parent {
		t.Fatalf("tabs after /spawn: %d, visible %d", len(r.tabs), r.visibleTabIndex())
	}
	child := r.tabs[1]
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "agent "+child.name+" started in tab 2 · /tab "+child.name) {
		t.Fatalf("/spawn was not announced: %q", got)
	}
	settleUntil(t, r, settled(child))
	settleUntil(t, r, func() bool { return len(runs.reported()) > 0 })
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

func TestChildOfAClosedParentReportsThroughTheStore(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "parent-work", "other-work")
	runs := &childTestRuns{release: make(chan struct{}), slow: make(chan struct{})}
	r.runTurn = runs.run
	parent := r.tabs[0]
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "slow", Background: true}, "")
	runUITask(t, r)
	awaitReport(t, result)
	child := r.tabs[1]
	if child.parent != parent || child.parentName != "parent-work" {
		t.Fatalf("child tab = %+v, want it under parent-work", child)
	}

	// The parent tab goes away (its lease lost, say) while its child works.
	r.removeTab(0)
	if err := parent.state.Close(); err != nil {
		t.Fatal(err)
	}
	if child.parent != nil || child.signalName() != child.name+" (agent of parent-work)" {
		t.Fatalf("orphaned child = parent %v, signal name %q", child.parent, child.signalName())
	}

	// Its report goes to the store and the unviewed tab closes itself.
	close(runs.slow)
	settleUntil(t, r, func() bool { return r.tabIndexOfModel(child.model) < 0 })
	if len(r.tabs) != 1 || r.tabs[0].name != "other-work" {
		t.Fatalf("%d tabs after the orphan reported, want other-work alone", len(r.tabs))
	}
	if got := runs.reported(); len(got) != 0 {
		t.Fatalf("the report ran a turn somewhere: %q", got)
	}

	// Reopening the parent takes the report as its first input.
	resolved, settings, err := r.opener.prepare(context.Background(), "parent-work", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.opener.open(context.Background(), resolved, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	r.finishOpen(openResult{name: resolved, state: state})
	reopened := r.visibleTab()
	settleUntil(t, r, func() bool { return reopened.turnDone != nil })
	if reopened.name != "parent-work" || reopened.turnDone == nil {
		t.Fatalf("reopened tab %q running=%v; want parent-work on its agent's report", reopened.name, reopened.turnDone != nil)
	}
	settleUntil(t, r, settled(reopened))
	want := "agent " + child.name + " finished\nfound slow\n\n(agent session " + child.name + " · 7 in / 3 out)"
	if got := runs.reported(); len(got) != 1 || got[0] != want {
		t.Fatalf("reopened parent got %q, want %q", got, want)
	}
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "> agent "+child.name+" finished") {
		t.Fatalf("reopened parent transcript lacks the report echo: %q", got)
	}
}

func TestReportsPostedWhileNoPollyHeldTheParentArriveAtStartup(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	if err := testAcquireSession(t, store, "helper").Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "parent-work")
	if err := store.PostReport(ctx, "parent-work", sessions.Report{Child: "helper", Status: sessions.ReportCanceled, Text: "half done"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PostReport(ctx, "parent-work", sessions.Report{Child: "helper", Status: sessions.ReportFailed, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	runs := &childTestRuns{release: make(chan struct{}), slow: make(chan struct{})}
	r.runTurn = runs.run

	// Run pulls once before its loop; the poll does the same later.
	if !r.pullAllReports(ctx, runs.run) {
		t.Fatal("startup found no reports")
	}
	parent := r.visibleTab()
	settleUntil(t, r, func() bool { return parent.turnDone != nil })
	if parent.turnDone == nil {
		t.Fatal("the waiting reports did not start a turn")
	}
	settleUntil(t, r, settled(parent))
	got := runs.reported()
	if len(got) != 1 || !strings.Contains(got[0], "agent helper canceled\nhalf done") || !strings.Contains(got[0], "agent helper failed: boom") {
		t.Fatalf("parent got %q, want both reports in one message", got)
	}
	if transcript := plainStyledText(r.model.fullTranscript()); !strings.Contains(transcript, "> 2 agent reports") {
		t.Fatalf("transcript lacks the coalesced echo: %q", transcript)
	}
	r.pullAllReports(ctx, runs.run)
	settleUntil(t, r, func() bool { return !parent.reportsLoading })
	if len(runs.reported()) != 1 || parent.turnDone != nil {
		t.Fatal("reports were delivered twice")
	}
}

func TestClosingATabWithRunningAgentsIsRefused(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "slow", Background: true}, "")
	runUITask(t, r)
	awaitReport(t, result)

	r.model.mu.Lock()
	r.requestCloseTabLocked()
	r.model.mu.Unlock()
	if r.closeTabRequest {
		t.Fatal("closing a tab with a running agent was allowed")
	}
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "cancel this tab's agents (Esc in their tabs) before closing it") {
		t.Fatalf("no refusal notice: %q", got)
	}

	// Once the agent has settled and reported, the tab may close.
	child := r.tabs[1]
	close(runs.slow)
	settleUntil(t, r, settled(child))
	settleUntil(t, r, func() bool { return len(runs.reported()) == 1 && parent.turnDone == nil })
	r.model.mu.Lock()
	r.requestCloseTabLocked()
	r.model.mu.Unlock()
	if !r.closeTabRequest {
		t.Fatal("closing was still refused after the agent settled")
	}
}

func TestBackgroundChildHoldsItsSlotUntilItSettles(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "slow", Background: true}, "")
	runUITask(t, r)
	rep := awaitReport(t, result)
	if rep.err != nil || !rep.result.Started || rep.result.Done == nil {
		t.Fatalf("background spawn = %+v, %v", rep.result, rep.err)
	}
	select {
	case <-rep.result.Done:
		t.Fatal("Done closed while the child was still running")
	default:
	}
	child := r.tabs[1]
	close(runs.slow)
	settleUntil(t, r, settled(child))
	select {
	case <-rep.result.Done:
	default:
		t.Fatal("Done stayed open after the child settled")
	}
}

func TestClosingAChildTabSettlesItsSpawnSlot(t *testing.T) {
	r, _ := newChildTestREPL(t)
	parent := r.visibleTab()
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "wait", Background: true}, "")
	runUITask(t, r)
	rep := awaitReport(t, result)
	if rep.err != nil || rep.result.Done == nil {
		t.Fatalf("background spawn = %+v, %v", rep.result, rep.err)
	}
	r.cancelTabTurn(r.tabs[1])
	r.removeTab(1)
	select {
	case <-rep.result.Done:
	case <-time.After(time.Second):
		t.Fatal("Done stayed open after the canceled child returned")
	}
}

func TestSpawnIsRefusedOnceQuitting(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	beginParentToolCall(t, r, runs, "call-1")
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "look", Background: true}, "")
	// The launch closure is queued; the user quits before the loop runs it.
	if r.beginQuit() {
		t.Fatal("quit returned with the parent turn still running")
	}
	runUITask(t, r)
	rep := awaitReport(t, result)
	if rep.err == nil || !strings.Contains(rep.err.Error(), "quitting") {
		t.Fatalf("spawn after quit = %+v, %v, want a refusal", rep.result, rep.err)
	}
	if len(r.tabs) != 1 {
		t.Fatalf("%d tabs after a refused spawn, want the parent alone", len(r.tabs))
	}
	close(runs.release)
}

func TestSpawnCommandIsDroppedOnceQuitting(t *testing.T) {
	r, _ := newChildTestREPL(t)
	r.model.mu.Lock()
	r.requestSpawnLocked("look")
	r.model.mu.Unlock()
	r.quitting = true
	r.applySpawnRequests()
	if len(r.tabs) != 1 || len(r.spawnRequests) != 0 {
		t.Fatalf("%d tabs, %d requests after /spawn while quitting", len(r.tabs), len(r.spawnRequests))
	}
}
