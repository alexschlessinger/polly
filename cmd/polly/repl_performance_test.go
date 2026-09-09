package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
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
	_, lines := m.streamCodeCache.Block(0)
	first := &lines[0]
	m.appendAssistant("\n[ref]: https://example.com\n")
	m.renderPendingMarkdown()
	if _, lines := m.streamCodeCache.Block(0); &lines[0] != first {
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

type delayedReportSession struct {
	sessions.Session
	readStarted, readRelease chan struct{}
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
	if transcript := plainStyledText(parent.model.fullTranscript()); !strings.Contains(transcript, "Agent reports for parent-work unavailable · inbox unavailable") {
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
	if a, b := strings.Index(transcript, "agent helper finished"), strings.Index(transcript, "▎ queued followup"); a < 0 || b < a {
		t.Fatalf("queue order changed: %s", transcript)
	}
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
