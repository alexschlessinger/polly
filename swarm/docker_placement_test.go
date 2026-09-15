package swarm

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

// recordingWorkspaces is a tools.WorkspaceBackend that records what the
// coordinator asks of it.
type recordingWorkspaces struct {
	mu         sync.Mutex
	destroyed  []string
	resynced   []string
	destroyErr error
	resyncErr  error
	onDestroy  func(root string)
}

func (w *recordingWorkspaces) Destroy(_ context.Context, root string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.onDestroy != nil {
		w.onDestroy(root)
	}
	w.destroyed = append(w.destroyed, root)
	return w.destroyErr
}

func (w *recordingWorkspaces) Resync(_ context.Context, root string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.resynced = append(w.resynced, root)
	return w.resyncErr
}

func (w *recordingWorkspaces) roots() (destroyed, resynced []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.destroyed...), append([]string(nil), w.resynced...)
}

func TestReleaseDestroysTheWorkspaceBackendBeforeRemovingFiles(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(map[bool]string{false: "destroyed", true: "destroy fails"}[failing], func(t *testing.T) {
			r := scratchRuntime(t, doneModel(), true)
			var events []Event
			var eventsMu sync.Mutex
			backend := &recordingWorkspaces{onDestroy: func(root string) {
				if _, err := os.Stat(root); err != nil {
					t.Errorf("checkout %s removed before the backend was destroyed: %v", root, err)
				}
			}}
			if failing {
				backend.destroyErr = errors.New("daemon unreachable")
			}
			r.config.Workspaces = backend
			r.config.OnEvent = func(e Event) {
				eventsMu.Lock()
				events = append(events, e)
				eventsMu.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// A read-only researcher gets an isolated checkout of the Git
			// root; once the parent admits its delivery the workspace is
			// released, and the backend's state with it.
			result, err := r.Agent(ctx, "", subagentToAgentRequest(subagent.Request{Label: "Test agent", Task: "inspect", ReadOnly: true}))
			if err != nil {
				t.Fatal(err)
			}
			admitParent(t, r)
			s := awaitReleased(t, r, result.Session)
			if s.Members[result.Session].Context != "" {
				t.Fatal("workspace not released")
			}
			destroyed, _ := backend.roots()
			if len(destroyed) != 1 || !strings.HasPrefix(destroyed[0], canonicalPath(t, r.config.Directory)) {
				t.Fatalf("destroyed %v", destroyed)
			}
			if _, err := os.Stat(destroyed[0]); !os.IsNotExist(err) {
				t.Fatalf("checkout survived release: %v", err)
			}
			eventsMu.Lock()
			defer eventsMu.Unlock()
			orphaned := 0
			for _, e := range events {
				if e.Kind == "container_orphaned" {
					orphaned++
				}
			}
			if orphaned != map[bool]int{false: 0, true: 1}[failing] {
				t.Fatalf("container_orphaned events = %d: %+v", orphaned, events)
			}
		})
	}
}

func TestParkKeepsTheBackendAndScopesCarrySessions(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "ask", Name: "send_message", Arguments: tools.Result(map[string]any{"target": r.ID, "message": "which option?"})}, {ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		return answer("completed")
	})
	r = runtimeTest(t, model, 1, 1)
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	backend := &recordingWorkspaces{}
	r.config.Workspaces = backend
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "ask parent", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("%+v %v", result, err)
	}
	if destroyed, _ := backend.roots(); len(destroyed) != 0 {
		t.Fatalf("parking destroyed the backend: %v", destroyed)
	}
	if len(rec.scopes) != 1 || rec.scopes[0].Session != result.Session {
		t.Fatalf("member scope session = %q, want member %s", rec.scopes[0].Session, result.Session)
	}
	if _, err = r.Send(ctx, r.ID, result.Session, "info", "", "choose A"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal("member failed to resume")
	}
	if rec.scopes[1].Session != result.Session {
		t.Fatalf("resumed scope session = %q", rec.scopes[1].Session)
	}

	// A workflow step's binding names the context's owner.
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	id, err := h.Call(ctx, workflow.Operation{Kind: "context", Args: map[string]any{"readOnly": true}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.registry(ctx, s, s.Contexts[id.(string)]); err != nil {
		t.Fatal(err)
	}
	if last := rec.scopes[len(rec.scopes)-1]; last.Session != s.Contexts[id.(string)].Owner || last.Session == "" {
		t.Fatalf("workflow scope session = %q, owner %q", last.Session, s.Contexts[id.(string)].Owner)
	}
}

func TestApplyResyncsTheParentWorkspace(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(map[bool]string{false: "resynced", true: "resync fails"}[failing], func(t *testing.T) {
			r, plan := applyFixture(t, false)
			backend := &recordingWorkspaces{}
			if failing {
				backend.resyncErr = errors.New("copy busy")
			}
			var events []Event
			var eventsMu sync.Mutex
			r.config.Workspaces = backend
			r.config.OnEvent = func(e Event) {
				eventsMu.Lock()
				events = append(events, e)
				eventsMu.Unlock()
			}
			ctx := context.Background()
			if _, err := r.ApplyIntegration(ctx, plan.ID); err != nil {
				t.Fatal(err)
			}
			s, err := r.read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if s.Applies[plan.ID] == nil || s.Applies[plan.ID].Status != "applied" {
				t.Fatalf("receipt %+v", s.Applies[plan.ID])
			}
			if _, resynced := backend.roots(); len(resynced) != 1 || resynced[0] != r.config.Root {
				t.Fatalf("resynced %v, want the parent root %s", resynced, r.config.Root)
			}
			eventsMu.Lock()
			defer eventsMu.Unlock()
			reported := false
			for _, e := range events {
				if e.Kind == "integration" && strings.Contains(e.Text, "workspace resync after apply") {
					reported = true
				}
			}
			if reported != failing {
				t.Fatalf("resync failure reported=%v, want %v: %+v", reported, failing, events)
			}
		})
	}
}

func subagentToAgentRequest(req subagent.Request) AgentRequest {
	return AgentRequest{Label: req.Label, Task: req.Task, ReadOnly: req.ReadOnly, Review: req.Review, Tools: req.Tools}
}
