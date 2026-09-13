package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/alexschlessinger/pollytool/workflow"
)

func strictRequest(args map[string]any, request any) error {
	data, err := json.Marshal(args)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return fail("invalid_args", err.Error())
	}
	return nil
}

func (r *Runtime) ReadTask(ctx context.Context, id string) (*Task, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	t := s.Tasks[id]
	if t == nil {
		return nil, fail("unknown_task", "unknown task")
	}
	return t, nil
}
func (r *Runtime) taskOperation(ctx context.Context, args map[string]any) (any, error) {
	// Decode each operation separately so unsupported fields cannot silently
	// change meaning (for example, changing a creation-only requirement).
	switch args["op"] {
	case "create":
		var req struct {
			Op           string   `json:"op"`
			Description  string   `json:"description"`
			Criteria     string   `json:"criteria"`
			Dependencies []string `json:"dependencies"`
			Owner        string   `json:"owner"`
			Review       bool     `json:"review"`
			Requirement  string   `json:"requirement"`
		}
		if err := strictRequest(args, &req); err != nil {
			return nil, err
		}
		return r.CreateTask(ctx, req.Description, req.Criteria, req.Dependencies, req.Owner, CreateTaskOptions{Review: req.Review, Requirement: req.Requirement})
	case "update":
		var req struct {
			Op           string   `json:"op"`
			Task         string   `json:"task"`
			Revision     int      `json:"revision"`
			Owner        string   `json:"owner"`
			Dependencies []string `json:"dependencies"`
		}
		if err := strictRequest(args, &req); err != nil {
			return nil, err
		}
		for _, key := range []string{"task", "revision", "owner", "dependencies"} {
			if _, ok := args[key]; !ok {
				return nil, fail("invalid_args", "task update requires "+key)
			}
		}
		if err := r.UpdateTask(ctx, req.Task, req.Revision, req.Owner, req.Dependencies); err != nil {
			return nil, err
		}
		return r.ReadTask(ctx, req.Task)
	}
	var req struct {
		Op       string `json:"op"`
		Task     string `json:"task"`
		Revision int    `json:"revision,omitempty"`
		Accept   bool   `json:"accept,omitempty"`
		Feedback string `json:"feedback,omitempty"`
	}
	if err := strictRequest(args, &req); err != nil {
		return nil, err
	}
	switch req.Op {
	case "read":
		return r.ReadTask(ctx, req.Task)
	case "review":
		if err := r.Review(ctx, req.Task, req.Revision, req.Accept, req.Feedback); err != nil {
			return nil, err
		}
		return r.ReadTask(ctx, req.Task)
	default:
		return nil, fail("invalid_args", "unknown task operation")
	}
}

// release has no blocking context-lock acquisition under a scheduler lock.
// A concurrent removal is awaited through a cancellable completion signal.
func (h *workflowHost) release(ctx context.Context, id string) (any, error) {
	r := h.runtime
	defer r.notifyRelease(id)
	for {
		signal := r.releaseSignal(id)
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		c := s.Contexts[id]
		if !h.releaseAuthority(s, id) {
			return nil, fail("unknown_context", "unknown or unauthorized execution context")
		}
		if c == nil {
			h.unbind(id)
			return map[string]any{"released": id, "dormant": true}, nil
		}
		if c.Release == WorkspaceRetained {
			return nil, retainedReleaseError(c)
		}
		lock := r.contextMutex(id)
		if !lock.TryLock() {
			if c.Release != WorkspaceReleasing {
				return nil, fail("context_busy", "context still has an active agent or tool")
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-signal:
				continue
			}
		}
		result, err := h.releaseHeld(ctx, id)
		lock.Unlock()
		r.notifyRelease(id)
		return result, err
	}
}

func retainedReleaseError(c *ExecutionContext) error {
	return &workflow.Error{Code: "unintegrated_changes", Message: c.Reason, Result: map[string]any{"context": c.ID, "root": c.Root, "reason": c.Reason}}
}

func (h *workflowHost) releaseAuthority(s *State, id string) bool {
	if id == "" {
		return false
	}
	if c := s.Contexts[id]; c != nil {
		if c.Owner == h.controller {
			return true
		}
		if m := s.Members[c.Owner]; m != nil && m.Context == id && m.Controller == h.controller {
			return true
		}
		if c.Release != WorkspaceReleasing {
			return false
		}
	}
	for _, e := range s.Executions {
		if e.Workspace == id && e.Workflow == h.controller {
			return true
		}
	}
	if w := s.Workflows[h.controller]; w != nil {
		for _, step := range w.Steps {
			value, _ := step.Value.(string)
			if step.Kind == "context" && step.Status == "completed" && value == id {
				return true
			}
		}
	}
	return false
}

func (h *workflowHost) releaseHeld(ctx context.Context, id string) (any, error) {
	r := h.runtime
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	c := s.Contexts[id]
	if c == nil || !h.releaseAuthority(s, id) {
		return nil, fail("unknown_context", "unknown or unauthorized execution context")
	}
	r.mu.Lock()
	ok, why := explicitReleaseEligible(s, c, h.controller, r.active)
	r.mu.Unlock()
	if !ok && c.Release != WorkspaceReleasing {
		return nil, fail("context_busy", why)
	}
	tree, err := r.contextCleanupTree(ctx, s, c)
	if err != nil {
		return nil, err
	}
	r.launchMu.Lock()
	r.parentTools.Lock()
	err = r.update(ctx, func(s *State) error {
		current := s.Contexts[id]
		if current == nil || !h.releaseAuthority(s, id) {
			return fail("unknown_context", "workspace changed during release")
		}
		if r.closing {
			return context.Canceled
		}
		if current.Release == WorkspaceRetained {
			return retainedReleaseError(current)
		}
		r.mu.Lock()
		ok, why := explicitReleaseEligible(s, current, h.controller, r.active)
		r.mu.Unlock()
		if !ok && current.Release != WorkspaceReleasing {
			return fail("context_busy", why)
		}
		current.Release = WorkspaceReleasing
		current.Reason = ""
		return nil
	})
	r.parentTools.Unlock()
	r.launchMu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := r.finishRelease(ctx, []*ExecutionContext{c}, map[string]string{id: tree}); err != nil {
		return nil, err
	}
	return map[string]any{"released": id}, nil
}
