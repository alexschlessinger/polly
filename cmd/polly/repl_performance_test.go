package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

func TestAssistantMarkdownWaitsForPaint(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		m := newReplModel()
		m.hidden = hidden
		m.appendAssistant("## Result\n\n```go\nvar value = 1\n```\n")
		if m.streamShown != 0 || m.transcript[0].text != "" {
			t.Fatal("provider callback rendered Markdown")
		}
		m.finishAssistantBlock("")
		if m.transcript[0].text != "" || m.transcript[0].markdown == "" {
			t.Fatal("finalization rendered or lost the hidden result")
		}
		m.hidden = false
		m.renderPendingMarkdown()
		if !strings.Contains(plainStyledText(m.transcript[0].text), "var value = 1") {
			t.Fatal("paint lost the finalized text")
		}
		if m.markdownPending || m.transcript[0].markdown != "" {
			t.Fatal("paint retained pending Markdown")
		}
	}
}

func TestMarkdownCacheReusesCodeAndHonorsLateDefinitions(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("```go\nvar value = 1\n```\n\nSee [docs][ref].\n")
	m.renderPendingMarkdown()
	first := &m.streamCodeCache.blocks[0].lines[0]
	m.appendAssistant("\n[ref]: https://example.com\n")
	m.renderPendingMarkdown()
	if &m.streamCodeCache.blocks[0].lines[0] != first {
		t.Fatal("unchanged code was highlighted again")
	}
	m.finishAssistantBlock("")
	m.renderPendingMarkdown()
	if !strings.Contains(plainStyledText(m.transcript[0].text), "docs (https://example.com)") {
		t.Fatal("cached code prevented late reference resolution")
	}
}

func TestHiddenModelLockDoesNotBlockVisibleNotifications(t *testing.T) {
	r := newManagedREPL(&Config{}, "visible", 0, 0)
	defer r.closeTabs()
	hidden := newReplModel()
	hidden.hidden = true
	hidden.signalHiddenLocked(signalApprovalNeeded, "read file")
	hidden.pushNotice("done")
	r.tabs = append(r.tabs, &replTab{name: "child", model: hidden})
	hidden.mu.Lock()
	done := make(chan struct{})
	go func() {
		r.relayTabSignals()
		r.takeHiddenNotices(true, false)
		if _, ok := childActivity(hidden); ok {
			t.Error("busy hidden model should reuse its prior activity")
		}
		close(done)
	}()
	select {
	case <-done:
		hidden.mu.Unlock()
	case <-time.After(time.Second):
		hidden.mu.Unlock()
		<-done
		t.Fatal("visible paint waited for a hidden transcript lock")
	}
}

func TestSpawnCommandDoesNotWaitForChildIO(t *testing.T) {
	r, _ := newChildTestREPL(t)
	spawn := r.opener.spawn
	started, release := make(chan struct{}), make(chan struct{})
	r.opener.spawn = func(ctx context.Context, parent *conversationState, req subagent.Request) (*conversationState, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
		return spawn(ctx, parent, req)
	}
	returned := make(chan struct{})
	go func() { r.runTabCommand("/spawn wait"); close(returned) }()
	<-started
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(release)
		<-returned
		t.Fatal("/spawn blocked the event loop on child creation")
	}
	r.runTabCommand("/help")
	close(release)
	runUITask(t, r)
	if len(r.tabs) != 2 {
		t.Fatal("the prepared child was not published")
	}
}

func TestShutdownDiscardsAnUnclaimedChild(t *testing.T) {
	r, _ := newChildTestREPL(t)
	result := spawnFromTool(context.Background(), r, r.visibleTab(), subagent.Request{Task: "wait", Background: true}, "")
	var launch func()
	select {
	case launch = <-r.uiTasks:
	case <-time.After(time.Second):
		t.Fatal("child was not prepared")
	}
	if err := r.closeTabs(); err != nil {
		t.Fatal(err)
	}
	rep := awaitReport(t, result)
	if rep.err == nil || rep.result.Done == nil {
		t.Fatalf("abandoned launch = %+v, %v", rep.result, rep.err)
	}
	select {
	case <-rep.result.Done:
	default:
		t.Fatal("abandoned launch retained its concurrency slot")
	}
	launch()
	if len(r.tabs) != 0 {
		t.Fatal("a queued launch ran after shutdown")
	}
}

func TestDetachedChildKeepsItsLifetimeUntilTheRunnerReturns(t *testing.T) {
	r, runs := newChildTestREPL(t)
	result := spawnFromTool(context.Background(), r, r.visibleTab(), subagent.Request{Task: "slow"}, "")
	runUITask(t, r)
	child := r.tabs[1]
	r.cancelTabTurn(child)
	r.abandonCanceledTurn(child)
	r.deliverChildReport(context.Background(), child, context.Canceled, r.runTurn)
	rep := awaitReport(t, result)
	if !errors.Is(rep.err, context.Canceled) || rep.result.Done == nil {
		t.Fatalf("detached result = %+v, %v", rep.result, rep.err)
	}
	select {
	case <-rep.result.Done:
		t.Fatal("UI detachment released a still-running child")
	default:
	}
	close(runs.slow)
	select {
	case <-rep.result.Done:
	case <-time.After(time.Second):
		t.Fatal("returned child retained its slot")
	}
}

func TestRemovedBlockingChildReturnsItsRunningLifetime(t *testing.T) {
	r, runs := newChildTestREPL(t)
	result := spawnFromTool(context.Background(), r, r.visibleTab(), subagent.Request{Task: "slow"}, "")
	runUITask(t, r)
	child := r.tabs[1]
	defer child.state.Close()
	r.cancelTabTurn(child)
	r.removeTab(1)
	rep := awaitReport(t, result)
	if rep.err == nil || rep.result.Done == nil {
		t.Fatalf("removed child = %+v, %v", rep.result, rep.err)
	}
	select {
	case <-rep.result.Done:
		t.Fatal("removing the tab released a running child")
	default:
	}
	close(runs.slow)
	select {
	case <-rep.result.Done:
	case <-time.After(time.Second):
		t.Fatal("child did not release its lifetime")
	}
}

func TestShutdownSavesAReplyReturnedByItsCanceledWaiter(t *testing.T) {
	r, _ := newChildTestREPL(t)
	parent := r.visibleTab()
	state, err := r.opener.spawn(context.Background(), parent.state, subagent.Request{Task: "reply"})
	if err != nil {
		t.Fatal(err)
	}
	name, model, err := r.newTabModel(state)
	if err != nil {
		_ = state.Close()
		t.Fatal(err)
	}
	child := &replTab{name: name, state: state, model: model, parent: parent, parentName: parent.name, deliveryPending: true}
	r.tabs = append(r.tabs, child)
	waiter := make(chan childReport, 1)
	waiter <- childReport{result: subagent.Result{Session: name, Text: "buffered reply"}}
	r.childDoneWaiting(child, waiter, true)
	// Stop before the UI acknowledges the returned reply.
	select {
	case <-r.uiTasks:
	case <-time.After(time.Second):
		t.Fatal("return was not queued")
	}
	if err := r.closeTabs(); err != nil {
		t.Fatal(err)
	}
	reopened, err := parent.state.sessionStore.Acquire(context.Background(), parent.name, sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reports, err := reopened.PeekReports(context.Background())
	if err != nil || len(reports) != 1 || reports[0].Text != "buffered reply" {
		t.Fatalf("shutdown lost buffered reply: %v, %v", reports, err)
	}
}

type delayedReportSession struct {
	sessions.Session
	readStarted, readRelease   chan struct{}
	writeStarted, writeRelease chan struct{}
}

func (s *delayedReportSession) PeekReports(ctx context.Context) ([]sessions.Report, error) {
	if s.readStarted != nil {
		close(s.readStarted)
		select {
		case <-s.readRelease:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	return s.Session.PeekReports(ctx)
}

func (s *delayedReportSession) Report(ctx context.Context, report sessions.Report) error {
	close(s.writeStarted)
	select {
	case <-s.writeRelease:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	return s.Session.Report(ctx, report)
}

func TestReportReadsDoNotBlockTheUIOrConsumeUnstartedInput(t *testing.T) {
	r, _ := newChildTestREPL(t)
	parent := r.visibleTab()
	session := parent.state.session
	if err := parent.state.sessionStore.PostReport(context.Background(), parent.name, sessions.Report{Child: "helper", Status: sessions.ReportFinished, Text: "kept"}); err != nil {
		t.Fatal(err)
	}
	delayed := &delayedReportSession{Session: session, readStarted: make(chan struct{}), readRelease: make(chan struct{})}
	parent.state.session = delayed
	r.runTurn = nil // Queue the input, but do not run/persist it.
	returned := make(chan struct{})
	go func() { r.pullReports(context.Background(), parent); close(returned) }()
	<-delayed.readStarted
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(delayed.readRelease)
		<-returned
		t.Fatal("report read blocked the UI")
	}
	r.runTabCommand("/help")
	close(delayed.readRelease)
	runUITask(t, r)
	if len(parent.model.queue) != 1 {
		t.Fatal("report did not reach the queue")
	}
	if reports, err := session.PeekReports(context.Background()); err != nil || len(reports) != 1 {
		t.Fatalf("unstarted report was lost: %v %v", reports, err)
	}
	parent.state.session = session
	if err := r.closeTabs(); err != nil {
		t.Fatal(err)
	}
	reopened, err := parent.state.sessionStore.Acquire(context.Background(), parent.name, sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reports, err := reopened.PeekReports(context.Background()); err != nil || len(reports) != 1 {
		t.Fatalf("closed queue lost reports: %v %v", reports, err)
	}
}

func TestFailedReportReadStillStartsQueuedInput(t *testing.T) {
	r, _ := newChildTestREPL(t)
	parent := r.visibleTab()
	session := parent.state.session
	parent.state.session = &failingReportSession{Session: session}
	turn := textManagedTurn("queued followup")
	parent.model.queue = []queuedREPLInput{{text: turn.displayText, turn: &turn}}
	if !r.pullReports(context.Background(), parent) {
		t.Fatal("report read did not start")
	}
	runUITask(t, r)
	if parent.turnDone == nil {
		t.Fatal("queued input stalled behind the failed report read")
	}
	settleUntil(t, r, func() bool { return parent.turnDone == nil })
	if transcript := plainStyledText(parent.model.fullTranscript()); !strings.Contains(transcript, "agent reports for parent-work: inbox unavailable") {
		t.Fatalf("read failure was not reported: %s", transcript)
	}
	parent.state.session = session
}

type failingReportSession struct{ sessions.Session }

func (s *failingReportSession) PeekReports(context.Context) ([]sessions.Report, error) {
	return nil, errors.New("inbox unavailable")
}

func TestReportWrittenDuringAReadIsReadAgain(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	session := parent.state.session
	stale := &staleReportSession{Session: session, readStarted: make(chan struct{}), readRelease: make(chan struct{})}
	parent.state.session = stale
	ctx := context.Background()
	if !r.pullReports(ctx, parent) {
		t.Fatal("report read did not start")
	}
	<-stale.readStarted
	// The child's report commits after the read in flight took its rows.
	if err := parent.state.sessionStore.PostReport(ctx, parent.name, sessions.Report{Child: "helper", Status: sessions.ReportFinished, Text: "late"}); err != nil {
		t.Fatal(err)
	}
	if r.pullReports(ctx, parent) {
		t.Fatal("a second read started beside the first")
	}
	close(stale.readRelease)
	runUITask(t, r) // the stale read found nothing and reads again
	runUITask(t, r) // the fresh read queues the report
	settleUntil(t, r, func() bool { return len(runs.reported()) == 1 && parent.turnDone == nil })
	parent.state.session = session
}

// staleReportSession lets a report land between a read's query and its result.
type staleReportSession struct {
	sessions.Session
	readStarted, readRelease chan struct{}
	once                     sync.Once
}

func (s *staleReportSession) PeekReports(ctx context.Context) ([]sessions.Report, error) {
	reports, err := s.Session.PeekReports(ctx)
	s.once.Do(func() {
		close(s.readStarted)
		<-s.readRelease
	})
	return reports, err
}

func TestPendingReportPrecedesOtherQueuedInputs(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	ctx := context.Background()
	if err := parent.state.sessionStore.PostReport(ctx, parent.name, sessions.Report{Child: "helper", Status: sessions.ReportFinished, Text: "reply"}); err != nil {
		t.Fatal(err)
	}
	turn := textManagedTurn("queued followup")
	parent.model.queue = []queuedREPLInput{{text: turn.displayText, turn: &turn}}
	r.pullReports(ctx, parent)
	r.startQueued(ctx, parent, r.runTurn)
	if parent.turnDone != nil {
		t.Fatal("queued input overtook the pending report read")
	}
	runUITask(t, r)
	settleUntil(t, r, func() bool {
		return len(runs.reported()) == 1 && len(parent.model.queue) == 0 && parent.turnDone == nil
	})
	transcript := plainStyledText(parent.model.fullTranscript())
	if a, b := strings.Index(transcript, "> agent helper finished"), strings.Index(transcript, "> queued followup"); a < 0 || b < a {
		t.Fatalf("queue order changed: %s", transcript)
	}
}

func TestChildReportWriteDoesNotBlockSettlement(t *testing.T) {
	r, _ := newChildTestREPL(t)
	spawn := r.opener.spawn
	started, release := make(chan struct{}), make(chan struct{})
	r.opener.spawn = func(ctx context.Context, parent *conversationState, req subagent.Request) (*conversationState, error) {
		child, err := spawn(ctx, parent, req)
		if err == nil {
			child.session = &delayedReportSession{Session: child.session, writeStarted: started, writeRelease: release}
		}
		return child, err
	}
	result := spawnFromTool(context.Background(), r, r.visibleTab(), subagent.Request{Task: "answer", Background: true}, "")
	runUITask(t, r)
	awaitReport(t, result)
	child := r.tabs[1]
	returned := make(chan struct{})
	go func() { settleUntil(t, r, settled(child)); close(returned) }()
	<-started
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(release)
		<-returned
		t.Fatal("report write blocked settlement")
	}
	r.runTabCommand("/help")
	close(release)
	settleUntil(t, r, func() bool { return !child.reporting })
}

func BenchmarkAssistantStreaming(b *testing.B) {
	raw := strings.Repeat("Review **results** for `worker.go`: the task finished normally.\n\n", 1024)
	b.ReportAllocs()
	for b.Loop() {
		m := newReplModel()
		for i := 0; i < len(raw); i += 128 {
			m.appendAssistant(raw[i:min(i+128, len(raw))])
			// Several provider chunks may arrive within one 50 ms frame.
			if (i+128)%4096 == 0 {
				m.renderPendingMarkdown()
			}
		}
		m.finishAssistantBlock("")
		m.renderPendingMarkdown()
	}
}

func BenchmarkHiddenAssistantFinalization(b *testing.B) {
	raw := "```go\n" + strings.Repeat("if value > 0 { fmt.Println(value) }\n", 4096) + "```\n"
	b.ReportAllocs()
	for b.Loop() {
		m := newReplModel()
		m.hidden = true
		m.appendAssistant(raw)
		m.finishAssistantBlock("")
	}
}
