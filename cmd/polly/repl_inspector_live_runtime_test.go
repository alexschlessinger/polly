package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/swarm"
)

type inspectorStreamingModel struct{ release <-chan struct{} }

func (m inspectorStreamingModel) ChatCompletionStream(ctx context.Context, _ *llm.CompletionRequest, _ llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan *messages.StreamEvent)
	go func() {
		defer close(ch)
		select {
		case ch <- &messages.StreamEvent{Type: messages.EventTypeContent, Content: "Actual **live assistant** text"}:
		case <-ctx.Done():
			return
		}
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Actual **live assistant** text", StopReason: messages.StopReasonEndTurn}
		select {
		case ch <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &msg}:
		case <-ctx.Done():
		}
	}()
	return ch
}

func TestInspectorWatchesSameProcessMemberBeforeCheckpoint(t *testing.T) {
	withDisplayTTY(t)
	release := make(chan struct{})
	content := make(chan struct{}, 1)
	r := newSwarmTestREPL(t, inspectorStreamingModel{release}, func(c *swarm.Config) {
		c.Callbacks = func(context.Context, swarm.Member) *llm.AgentCallbacks {
			return &llm.AgentCallbacks{OnContent: func(string) {
				select {
				case content <- struct{}{}:
				default:
				}
			}}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.state.swarm.Spawn(ctx, subagent.Request{Label: "Live inspection", Task: "assignment", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-content:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	root := r.visibleTab()
	root.swarmSnapshot, err = r.state.swarm.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root.swarmActivities = r.state.swarm.LiveActivities()
	store := r.state.sessionStore
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store.(sessions.ViewStore)}
	r.state.sessionStore = counted
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: result.Session}})
	v := waitInspector(t, r, 140)
	if !v.live.active || !strings.Contains(inspectorText(v), "live assistant") {
		t.Fatalf("pre-checkpoint content missing: source=%+v transcript=%q", v.live, inspectorText(v))
	}
	if text := overviewText(renderAgentOverview(v.model, 70, r.agentOverviewState())); !strings.Contains(text, "live assistant") {
		t.Fatalf("inspection omitted actual live update: %q", text)
	}
	base, rendered, histories := v.saved, v.model, counted.histories.Load()
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 140)
	if v.saved != base || v.model != rendered || counted.histories.Load() != histories {
		t.Fatal("unchanged live revision reread/rehydrated history or rerendered Markdown")
	}
	// Intermediate frame paints are coalesced, not an extra store read each.
	reads := counted.reads.Load()
	r.refreshInspector(140)
	settleInspectorWork(r)
	if counted.reads.Load() != reads {
		t.Fatal("live refresh ignored coalescing interval")
	}
	close(release)
	root.swarmSnapshot = waitSwarmIdle(t, r.state.swarm)
	root.swarmActivities = r.state.swarm.LiveActivities()
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 140)
	if v.live.active || strings.Count(inspectorText(v), "live assistant") != 1 {
		t.Fatalf("terminal checkpoint duplicated/lost live output: source=%+v transcript=%q", v.live, inspectorText(v))
	}
	if text := plainStyledText(r.inspectorHeader(100, 20, 0, 0).text); !strings.Contains(text, "Saved transcript") {
		t.Fatalf("terminal header still advertises live stream: %q", text)
	}
}
