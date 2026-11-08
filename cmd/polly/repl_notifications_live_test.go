package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestREPLNotificationInspectorParentStateWithoutDurableChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, _ := newNotificationREPL(t)
	if _, err := r.state.swarm.RunParent(ctx, r.state.agent, &llm.CompletionRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	tab := r.visibleTab()
	target := tabViewTarget(tab)
	target.kind, target.item = swarmViewKind, "members"
	r.inspect(target)
	v := &viewInstance{target: target, view: viewFor(target.kind)}
	r.workspace().inspector.current = v
	source, err := tab.notificationSource(r.work.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.changes.bind(r.work.ctx, source, swarmInspectorScopes()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-v.changes.sub.Changes():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitInspector(t, r, 140)
	if text := plainStyledText(v.model.fullTranscript()); !strings.Contains(text, "Parent — idle") {
		t.Fatalf("initial parent: %s", text)
	}
	r.state.swarm.ParentTurnSettled(errors.New("disk full"))
	if actual := r.state.swarm.ParentState(nil).Display; actual != "paused · error" {
		t.Fatalf("actual parent: %s", actual)
	}
	r.inspectorRefreshAt = time.Unix(1, 0)
	if !r.needsTick() {
		t.Fatal("idle parent transition did not request a frame")
	}
	waitInspector(t, r, 140)
	if text := plainStyledText(v.model.fullTranscript()); !strings.Contains(text, "Parent — paused · error") {
		t.Fatalf("inspector ignored in-memory parent transition: %s", text)
	}
}

type notificationProgressModel struct{ entered, advance chan struct{} }

func (m notificationProgressModel) ChatCompletionStream(ctx context.Context, _ *llm.CompletionRequest, p llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage)
	core := streaming.NewStreamingCore(ctx, ch, nil)
	go func() {
		defer close(ch)
		close(m.entered)
		select {
		case <-m.advance:
		case <-ctx.Done():
			return
		}
		core.EmitContent("stream-progress")
		<-ctx.Done()
	}()
	return p.ProcessMessagesToEvents(ctx, ch)
}

func TestREPLNotificationOverviewLiveProgressWithoutDurableChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	model := notificationProgressModel{make(chan struct{}), make(chan struct{})}
	observed := make(chan struct{}, 1)
	r := newSwarmTestREPL(t, model, func(c *swarm.Config) {
		c.Callbacks = func(context.Context, swarm.Member) *llm.AgentCallbacks {
			return &llm.AgentCallbacks{OnContent: func(string) { observed <- struct{}{} }}
		}
	})
	result, err := r.state.swarm.Spawn(ctx, subagent.Request{Label: "Live progress", Task: "live progress", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tab := r.visibleTab()
	source, err := tab.notificationSource(r.work.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.swarmChanges.bind(r.work.ctx, source, swarmOverviewScopes()); err != nil {
		t.Fatal(err)
	}
	// Establish the sole poller's stable baseline before the first visible read.
	select {
	case <-tab.swarmChanges.sub.Changes():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Measure only TUI reads; the runtime's shared poller retains its separate
	// owner context. No UI background work has been dispatched yet.
	metric := runtimemetrics.New()
	r.work.ctx = runtimemetrics.WithRecorder(r.work.ctx, metric)
	refresh := func() {
		tab.swarmRefreshAt = time.Unix(1, 0)
		r.refreshSwarmActivities()
		waitNotificationOverview(t, r)
	}
	refresh()
	reads := metric.Snapshot().Count(runtimemetrics.SQLStatements)
	if reads == 0 {
		t.Fatal("initial display did not exercise storage instrumentation")
	}
	before := tab.swarmActivities[result.Session]
	if before.Phase != "requesting" {
		t.Fatalf("unexpected initial live phase %+v", before)
	}
	close(model.advance)
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	current := r.state.swarm.LiveActivities()[result.Session]
	if current.StreamedBytes == 0 || current.Phase != "generating" {
		t.Fatalf("missing real progress: %+v", current)
	}
	refresh()
	if after := metric.Snapshot().Count(runtimemetrics.SQLStatements); after != reads {
		t.Fatalf("live-only update queried storage: SQL %d -> %d", reads, after)
	}
	shown := tab.swarmActivities[result.Session]
	if shown.StreamedBytes != current.StreamedBytes || shown.Phase != current.Phase {
		t.Fatalf("overview live activity froze without durable commits: shown phase=%s bytes=%d, actual phase=%s bytes=%d", shown.Phase, shown.StreamedBytes, current.Phase, current.StreamedBytes)
	}
}
