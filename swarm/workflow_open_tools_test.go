package swarm

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/workflow"
)

func workflowContext(t *testing.T, h *workflowHost) string {
	t.Helper()
	id, err := h.Call(context.Background(), workflow.Operation{Kind: "context", Args: map[string]any{"readOnly": true}})
	if err != nil {
		t.Fatal(err)
	}
	return id.(string)
}

func TestWorkflowHostOpensOncePerScopeAndReopensWhenItChanges(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	if _, err := r.config.Registry.LoadToolAuto("list_dir"); err != nil {
		t.Fatal(err)
	}
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	// runWorkflow registers the host so workspace release can unbind it.
	r.mu.Lock()
	r.workflowHosts[h.controller] = h
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.workflowHosts, h.controller)
		r.mu.Unlock()
	}()
	ctx := context.Background()
	first := workflowContext(t, h)
	list := func(id string) {
		t.Helper()
		value, err := h.Call(ctx, workflow.Operation{ID: "list", Kind: "tool", Args: map[string]any{"context": id, "name": "list_dir", "args": map[string]any{"path": "."}}})
		if err != nil {
			t.Fatal(err)
		}
		if value.(map[string]any)["text"] == "" {
			t.Fatal("empty listing")
		}
	}
	list(first)
	list(first)
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open"}) {
		t.Fatalf("two steps under one scope: %v", events)
	}
	// Another context's checkout becomes a denied read of the first, so its
	// scope changes and the next step reopens after closing the old binding.
	second := workflowContext(t, h)
	list(first)
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open", "close", "open"}) {
		t.Fatalf("scope change did not reopen: %v", events)
	}
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	latest := rec.scopes[len(rec.scopes)-1]
	if !slices.Contains(latest.Grant.DeniedReads, s.Contexts[second].Root) {
		t.Fatalf("reopened scope %+v does not deny the sibling %s", latest.Grant, s.Contexts[second].Root)
	}
	// Releasing a context closes its binding before the checkout goes; the
	// release itself finishes in the background.
	if _, err := h.Call(ctx, workflow.Operation{Kind: "release", Args: map[string]any{"context": first}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		_, bound := h.bound[first]
		h.mu.Unlock()
		events := rec.recorded()
		if !bound && events[len(events)-1] == "close" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("release left the binding open: bound=%v events=%v", bound, events)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkflowStepsRunOverAnIndependentToolset(t *testing.T) {
	set := &independentToolset{}
	r := runtimeWithToolset(t, runtimeTest(t, doneModel(), 1, 4), set, nil)
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	ctx := context.Background()
	id := workflowContext(t, h)
	_, err := h.Call(ctx, workflow.Operation{ID: "exec", Kind: "exec", Args: map[string]any{"context": id, "command": "true"}})
	var failure *workflow.Error
	if !errors.As(err, &failure) || failure.Code != "tool_denied" {
		t.Fatalf("exec without a bash in the toolset = %v, want tool_denied", err)
	}
	value, err := h.Call(ctx, workflow.Operation{ID: "read", Kind: "tool", Args: map[string]any{"context": id, "name": "read_mem", "args": map[string]any{"path": "notes.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := value.(map[string]any)["text"]; got != "IN-MEMORY NOTES" {
		t.Fatalf("tool step = %v", value)
	}
	if len(set.scopes) != 1 || set.scopes[0].AllowedTools != nil || !set.scopes[0].Grant.ReadOnly {
		t.Fatalf("workflow scope = %+v", set.scopes)
	}
}
