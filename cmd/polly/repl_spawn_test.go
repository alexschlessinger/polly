package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestSpawnCommandUsesSwarmAuthorityAndCurrentSettings(t *testing.T) {
	var calls atomic.Int32
	model := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, tool := range req.Tools {
			switch tool.GetName() {
			case "spawn_agent", "workflow_run", "swarm_integration", "swarm_create_task", "swarm_review", "swarm_control":
				t.Errorf("child inherited parent authority: %s", tool.GetName())
			}
		}
		if req.Model != "test/reviewer" {
			t.Errorf("stale model: %s", req.Model)
		}
		if calls.Add(1) == 1 {
			return spawnTestToolCall("swarm_create_task", `{"description":"forged","criteria":"anything"}`)
		}
		return spawnTestReply("reviewed")
	})
	r := newSwarmTestREPL(t, model, nil)
	parent := r.visibleTab()
	r.state.settings.Model = "test/reviewer"
	r.state.settings.MaxIterations = 7
	r.state.settings.SystemPrompt = "Check invariants carefully."
	r.runTabCommand("/spawn --read-only review the tests")
	runUITask(t, r)
	s := waitSwarmIdle(t, r.state.swarm)
	if len(r.tabs) != 1 || r.visibleTab() != parent || r.workspace().inspector.open {
		t.Fatal("spawn moved focus or created an execution tab")
	}
	if len(s.Members) != 1 || len(s.Tasks) != 1 || len(s.Executions) != 1 || len(s.Workflows) != 0 {
		t.Fatalf("not one direct member: %+v", s)
	}
	for _, m := range s.Members {
		if !m.ReadOnly || m.Controller != "" || m.Model != "test/reviewer" {
			t.Fatalf("member: %+v", m)
		}
		e := s.Executions[m.Execution]
		if e.Request.Task != "review the tests" || e.Request.MaxIterations != 7 || e.Status != "completed" {
			t.Fatalf("execution: %+v", e)
		}
		if s.Tasks[m.Task].Description != "review the tests" {
			t.Fatal("forged child operation changed shared tasks")
		}
		if s.Contexts[m.Context].Checkout != nil {
			t.Fatal("outside-Git research allocated a checkout")
		}
		notice := plainStyledText(parent.model.fullTranscript())
		if !strings.Contains(notice, "Agent "+m.Name+" started") || strings.Contains(notice, m.ID) || !strings.Contains(notice, "/sessions") {
			t.Fatalf("missing human session handle: %s", notice)
		}
		if text := swarmInspectorText(s, nil, "members"); strings.Contains(text, "unknown") || !strings.Contains(text, "delivering") {
			t.Fatalf("member status: %s", text)
		}
	}
	reports, err := parent.state.session.PeekReports(context.Background())
	if err != nil || len(reports) != 0 {
		t.Fatalf("used old report mailbox: %v %v", reports, err)
	}
	if len(s.Messages) == 0 {
		t.Fatal("no shared completion message")
	}
}

func TestSpawnCommandAndWorkflowShareExecutionBudget(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), func(c *swarm.Config) { c.MaxExecutions = 1 })
	r.runTabCommand("/spawn --read-only inspect")
	runUITask(t, r)
	waitSwarmIdle(t, r.state.swarm)
	report, err := r.state.swarm.RunWorkflow(context.Background(), `polly.defineWorkflow({name:"review",inputSchema:polly.schema.object({}),async run(){return await polly.agent({task:"review again",readOnly:true});}})`, map[string]any{})
	if err == nil || report == nil || report.Status != "failed" {
		t.Fatalf("workflow bypassed budget: %+v %v", report, err)
	}
	s := waitSwarmIdle(t, r.state.swarm)
	if len(s.Executions) != 1 {
		t.Fatal("workflow acquired a separate execution allowance")
	}
}

func TestSpawnCommandKeepsUIResponsiveAndFencesShutdown(t *testing.T) {
	for _, end := range []string{"ready", "shutdown", "failed"} {
		t.Run(end, func(t *testing.T) {
			var calls atomic.Int32
			r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				return spawnTestReply("done")
			}), nil)
			ready := make(chan struct{})
			probe := &sandboxProbe{done: ready}
			r.state.sandboxProbe = probe
			returned := make(chan struct{})
			go func() { r.runTabCommand("/spawn --read-only inspect"); close(returned) }()
			select {
			case <-returned:
			case <-time.After(time.Second):
				close(ready)
				t.Fatal("spawn blocked UI on sandbox startup")
			}
			r.runTabCommand("/help")
			if calls.Load() != 0 {
				t.Fatal("model ran before sandbox readiness")
			}
			if end == "shutdown" {
				if err := r.closeTabs(); err != nil {
					t.Fatal(err)
				}
				close(ready)
				if calls.Load() != 0 {
					t.Fatal("shutdown admitted a pending launch")
				}
				return
			}
			if end == "failed" {
				probe.err = errors.New("sandbox unavailable")
			}
			close(ready)
			runUITask(t, r)
			s := waitSwarmIdle(t, r.state.swarm)
			if end == "failed" {
				if calls.Load() != 0 || len(s.Members) != 0 {
					t.Fatal("failed sandbox admitted a member")
				}
				if !strings.Contains(plainStyledText(r.model.fullTranscript()), "sandbox unavailable") {
					t.Fatal("sandbox failure hidden")
				}
			} else if len(s.Members) != 1 {
				t.Fatal("ready launch did not reach runtime")
			}
		})
	}
}

func TestSpawnCommandRefusesChildAndDropsClosedOrQuittingRequests(t *testing.T) {
	for _, mode := range []string{"child", "quitting", "closed"} {
		t.Run(mode, func(t *testing.T) {
			r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				t.Error("unexpected model request")
				return spawnTestReply("done")
			}), nil)
			parent := r.visibleTab()
			runtime := parent.state.swarm
			if mode == "child" {
				parent.state.swarm = nil
				defer func() { parent.state.swarm = runtime }()
			}
			r.model.mu.Lock()
			r.requestSpawnLocked(subagent.Request{Task: "inspect", ReadOnly: true})
			r.model.mu.Unlock()
			if mode == "quitting" {
				r.quitting = true
			}
			if mode == "closed" {
				r.removeTab(0)
				defer parent.state.Close()
			}
			r.applySpawnRequests()
			s, err := runtime.State(context.Background())
			if err != nil || len(s.Members) != 0 {
				t.Fatalf("invalid launch: %+v %v", s, err)
			}
		})
	}
}

func TestSpawnCommandStopInspectorAndCloseUseRuntime(t *testing.T) {
	started := make(chan struct{})
	r := newSwarmTestREPL(t, integrationModel(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		close(started)
		<-ctx.Done()
		return spawnTestReply("stopped")
	}), nil)
	parent := r.visibleTab()
	r.runTabCommand("/spawn --read-only inspect")
	runUITask(t, r)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not start")
	}
	if !r.needsTick() || !r.hasLiveAgents(parent) {
		t.Fatal("background runtime not refreshed")
	}
	r.model.mu.Lock()
	r.requestCloseTabLocked()
	r.model.mu.Unlock()
	if r.closeTabRequest {
		t.Fatal("allowed parent close while member active")
	}
	s, err := r.state.swarm.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Members {
		target := viewTarget{session: sessions.ViewTarget{ID: m.ID, Name: m.Name}}
		r.inspect(target)
		waitInspector(t, r, 140)
		r.stopInspectedAgent(target)
	}
	s = waitSwarmIdle(t, r.state.swarm)
	for _, e := range s.Executions {
		if e.Status != "paused" {
			t.Fatalf("stop outcome: %+v", e)
		}
	}
	if r.visibleTab() != parent || !r.workspace().inspector.open || len(r.tabs) != 1 {
		t.Fatal("inspector stop changed parent navigation")
	}
	r.model.mu.Lock()
	r.requestCloseTabLocked()
	r.model.mu.Unlock()
	if !r.closeTabRequest {
		t.Fatal("parent remains blocked after runtime stops")
	}
}

func TestSpawnCommandEditingRequiresGitAndEmptyBriefIsRefused(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		t.Error("unexpected model request")
		return spawnTestReply("done")
	}), nil)
	for _, cmd := range []string{"/spawn", "/spawn --read-only", "/spawn --review inspect"} {
		r.runTabCommand(cmd)
		if !strings.Contains(plainStyledText(r.model.fullTranscript()), "usage: /spawn [--read-only] [--review] <brief>") {
			t.Fatal("missing usage")
		}
	}
	r.runTabCommand("/spawn edit the file")
	runUITask(t, r)
	if !strings.Contains(plainStyledText(r.model.fullTranscript()), "Git") {
		t.Fatal("editing outside Git was not refused")
	}
	s := waitSwarmIdle(t, r.state.swarm)
	if len(s.Members) != 0 {
		t.Fatal("non-Git editing member created")
	}
}

func TestTypedSpawnSeedsTitleAndUsesSessionHandle(t *testing.T) {
	const brief = "Review the portable session picker"
	started, finish := make(chan struct{}), make(chan struct{})
	r := newTitleSwarm(t, integrationModel(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		close(started)
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return spawnTestReply("reviewed")
	}), nil)
	r.runTabCommand("/spawn --read-only " + brief)
	runUITask(t, r)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("typed member did not start")
	}
	s, err := r.state.swarm.State(context.Background())
	if err != nil || len(s.Members) != 1 {
		t.Fatalf("typed member state: %+v %v", s, err)
	}
	var id string
	for key, member := range s.Members {
		id = key
		if member.Label != brief {
			t.Fatalf("typed brief omitted from member: %+v", member)
		}
	}
	view, err := r.state.sessionStore.(sessions.ViewStore).ReadView(context.Background(), sessions.ViewTarget{ID: id}, "")
	if err != nil {
		t.Fatal(err)
	}
	md := view.Metadata
	if md.Title != brief || md.Description != brief || md.TitleSource != sessions.TitleSourceAgent || md.Name == id {
		t.Fatalf("typed title seed: %+v", md)
	}
	var notice string
	var item replModalItem
	var tracked bool
	func() {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
		r.model.renderPendingMarkdown()
		notice = plainStyledText(r.model.fullTranscript())
		_, tracked = r.visibleTab().swarmAnnounced[id]
		r.openSessionsPickerSelected(id)
		item = pickerItem(t, r.model.modal, id)
	}()
	if !strings.Contains(notice, "Agent "+md.Name+" started") || strings.Contains(notice, id) {
		t.Fatalf("launch notice did not use picker handle: %q", notice)
	}
	if !tracked {
		t.Fatal("completion tracking lost stable member ID")
	}
	if item.value != md.Name || !strings.Contains(item.searchText, brief) || !strings.Contains(item.label, md.Name) {
		t.Fatalf("typed picker identity: %+v", item)
	}
	close(finish)
	waitSwarmIdle(t, r.state.swarm)
	refreshPickerSwarm(t, r)
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.renderPendingMarkdown()
	notice = plainStyledText(r.model.fullTranscript())
	if strings.Count(notice, md.Name+" · idle · delivering") != 1 {
		t.Fatalf("completion did not match launch handle: %q", notice)
	}
}
