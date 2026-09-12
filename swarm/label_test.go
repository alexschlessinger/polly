package swarm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestNewAgentRequiresLabelBeforeAllocation(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	ctx := context.Background()
	for _, label := range []string{"", " \n ", strings.Repeat("x", 81), "bad\x00label"} {
		_, err := r.Agent(ctx, "", AgentRequest{Task: "Inspect code", Label: label, ReadOnly: true})
		if err == nil || !strings.Contains(err.Error(), "label") {
			t.Fatalf("label %q: %v", label, err)
		}
	}
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	if _, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"task": "Inspect code", "readOnly": true}}); err == nil || !strings.Contains(err.Error(), "label") {
		t.Fatalf("workflow accepted missing label: %v", err)
	}
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Members) != 0 || len(state.Contexts) != 0 || len(state.Executions) != 0 {
		t.Fatalf("invalid launch allocated state: %+v", state)
	}
}

func TestWorkflowLabelSeedsTitleBeforeToolFreeRunAndSurvivesContinuation(t *testing.T) {
	ctx := context.Background()
	const label = "Review llm and agent"
	var r *Runtime
	calls := 0
	r = runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		calls++
		if len(req.Tools) != 0 {
			t.Error("tool-free agent received tools")
		}
		state, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range state.Members {
			view, err := r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: member.ID}, "")
			if err != nil {
				t.Fatal(err)
			}
			if view.Metadata.Title != label || view.Metadata.TitleSource != sessions.TitleSourceAgent || member.Label != label {
				t.Errorf("title was not seeded before model call: %+v, %+v", view.Metadata, member)
			}
		}
		return answer("done")
	}), 1, 3)
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	value, err := h.Call(ctx, workflow.Operation{ID: "first", Kind: "agent", Args: map[string]any{"task": "Inspect the packages", "label": " Review  llm and agent ", "readOnly": true, "tools": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	first := value.(AgentResult)
	admitParent(t, r)
	awaitReleased(t, r, first.Session)
	_, err = h.Call(ctx, workflow.Operation{ID: "second", Kind: "agent", Args: map[string]any{"task": "Check once more", "session": first.Session}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("model calls=%d", calls)
	}
}
