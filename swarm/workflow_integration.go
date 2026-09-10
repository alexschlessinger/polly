package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
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
		if req.Accept {
			// Acceptance stands even when a copy cannot be removed now; the
			// next sweep or an explicit cleanup finishes it.
			if _, err := r.RetireAcceptedResearch(ctx); err != nil {
				r.event("retirement_incomplete", "", err.Error())
			}
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
		if h.retiredMemberContext(ctx, id) {
			return map[string]any{"released": id, "retired": true}, nil
		}
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
	tree, err := r.contextCleanupTree(ctx, s, c)
	if err != nil {
		return nil, err
	}
	if err := r.retireContext(ctx, c, tree); err != nil {
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

// retiredMemberContext reports whether id named the context of a member the
// runtime has already retired, so a script releasing a copy after accepting
// its research sees a release rather than an unknown context.
func (h *workflowHost) retiredMemberContext(ctx context.Context, id string) bool {
	s, err := h.runtime.read(ctx)
	if err != nil || id == "" || s.Contexts[id] != nil {
		return false
	}
	for _, m := range s.Members {
		if m.Context == id && m.Control == MemberControlRetired {
			return true
		}
	}
	return false
}
