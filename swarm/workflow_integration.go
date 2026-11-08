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
	if id == "" {
		return nil, fail("unknown_context", "unknown or unauthorized execution context")
	}
	for {
		signal := r.releaseSignal(id)
		unlock, err := r.lockMaintenance(ctx)
		if err != nil {
			return nil, err
		}
		var c *ExecutionContext
		err = r.readReleaseRecords(ctx, func(rd *recordReader) error {
			var err error
			c, err = rd.Context(id)
			if err != nil {
				return err
			}
			ok, err := h.releaseAuthorityRecords(rd, c, id)
			if err == nil && !ok {
				err = fail("unknown_context", "unknown or unauthorized execution context")
			}
			return err
		})
		if err != nil {
			unlock()
			return nil, err
		}
		if c == nil {
			unlock()
			h.unbind(id)
			return map[string]any{"released": id, "dormant": true}, nil
		}
		if c.Release == WorkspaceRetained {
			unlock()
			return nil, retainedReleaseError(c)
		}
		lock := r.contextMutex(id)
		if !lock.TryLock() {
			unlock()
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
		unlock()
		r.notifyRelease(id)
		return result, err
	}
}

func retainedReleaseError(c *ExecutionContext) error {
	return &workflow.Error{Code: "unintegrated_changes", Message: c.Reason, Result: map[string]any{"context": c.ID, "root": c.Root, "reason": c.Reason}}
}

func (h *workflowHost) releaseHeld(ctx context.Context, id string) (any, error) {
	r := h.runtime
	check := func(rd *recordReader, c *ExecutionContext) (bool, error) {
		ok, err := h.releaseAuthorityRecords(rd, c, id)
		if err != nil {
			return false, err
		}
		if c == nil || !ok {
			return false, fail("unknown_context", "unknown or unauthorized execution context")
		}
		if c.Release == WorkspaceRetained {
			return false, retainedReleaseError(c)
		}
		ok, why, err := r.releaseEligibleRecords(rd, c, h.controller)
		if err == nil && !ok {
			err = fail("context_busy", why)
		}
		return ok, err
	}
	var proof *releaseProof
	err := r.readReleaseRecords(ctx, func(rd *recordReader) error {
		c, err := rd.Context(id)
		if err != nil {
			return err
		}
		if _, err := check(rd, c); err != nil {
			return err
		}
		proof, err = releaseProofRecords(rd, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	tree, err := r.contextCleanupTreeRecords(ctx, proof.context)
	if err != nil {
		return nil, err
	}
	r.launchMu.Lock()
	r.parentTools.Lock()
	var marked []*releaseProof
	if r.closing {
		err = context.Canceled
	} else {
		marked, err = r.markReleaseRecords(ctx, []*releaseProof{proof}, false, true, check)
	}
	r.parentTools.Unlock()
	r.launchMu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := r.finishRelease(ctx, marked, map[string]string{id: tree}); err != nil {
		return nil, err
	}
	return map[string]any{"released": id}, nil
}
