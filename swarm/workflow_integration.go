package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

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

func (h *workflowHost) release(ctx context.Context, id string) (any, error) {
	r := h.runtime
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	c, err := h.context(ctx, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	active := r.active[c.Owner] != nil
	lock := r.contextLocks[c.ID]
	if lock == nil {
		lock = &sync.Mutex{}
		r.contextLocks[c.ID] = lock
	}
	r.mu.Unlock()
	if active || !lock.TryLock() {
		return nil, fail("context_busy", "context still has an active agent or tool")
	}
	defer lock.Unlock()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	c, err = h.context(ctx, id)
	if err != nil {
		return nil, err
	}
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	var tree string
	if c.Checkout != nil {
		m, err := r.manager(ctx)
		if err != nil {
			return nil, err
		}
		current, err := m.Capture(ctx, c.Root)
		if err != nil {
			return nil, err
		}
		safe := current.Tree == c.Checkout.Base.Tree
		for _, task := range s.Tasks {
			snapshot := s.Snapshots[task.Snapshot]
			if task.Status == "done" && snapshot != nil && snapshot.Source == c.Root && snapshot.Tree == current.Tree {
				safe = true
			}
		}
		if !safe {
			return nil, &workflow.Error{Code: "unintegrated_changes", Message: "context contains unintegrated edits; retained at " + c.Root, Result: map[string]any{"context": c.ID, "root": c.Root}}
		}
		tree = current.Tree
	}
	// Save starting provenance before removing the context so even an empty
	// editing submission can still be prepared after its safe copy is released.
	err = r.update(ctx, func(s *State) error {
		for _, task := range s.Tasks {
			if task.Owner == c.Owner && task.StartingSnapshot == "" && c.Checkout != nil {
				task.StartingSnapshot = c.Checkout.Base.ID
			}
		}
		s.Contexts[c.ID].Retiring = true
		if member := s.Members[c.Owner]; member != nil {
			member.Status = "retired"
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Once retirement is recorded, other operations fail closed even if
	// cleanup or its receipt is interrupted. Keep snapshots/publications pinned.
	finishCtx, cancel := context.WithTimeout(r.config.Parent.Context(), 2*time.Minute)
	defer cancel()
	if c.Checkout != nil {
		if err := r.worktrees.Cleanup(finishCtx, *c.Checkout, tree); err != nil {
			return nil, err
		}
	}
	if err := r.update(finishCtx, func(s *State) error { delete(s.Contexts, c.ID); return nil }); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cached := h.bound[c.ID]; cached != nil {
		cached.registry.Close()
		delete(h.bound, c.ID)
	}
	return map[string]any{"released": c.ID}, nil
}
