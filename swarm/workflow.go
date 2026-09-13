package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/workflow"
)

type workflowHost struct {
	runtime    *Runtime
	controller string
	// Binding a registry starts fresh native tools and relaunches local MCP
	// servers; one binding per context serves every step run under the same
	// policy. calls keeps close from racing a step still executing.
	mu    sync.Mutex
	bound map[string]*boundRegistry
	calls sync.WaitGroup
}
type boundRegistry struct {
	policy   string
	registry *tools.ToolRegistry
}

// registry returns the context's bound tools, rebinding only when the policy
// derived from the current coordination state differs from the cached one.
func (h *workflowHost) registry(ctx context.Context, s *State, c *ExecutionContext) (*tools.ToolRegistry, error) {
	ec, err := h.runtime.contextPolicy(ctx, s, c)
	if err != nil {
		return nil, err
	}
	policy, err := json.Marshal(struct {
		Root     string
		ReadOnly bool
		Sandbox  sandbox.Config
	}{ec.Root, ec.ReadOnly, ec.Sandbox})
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cached := h.bound[c.ID]; cached != nil {
		if cached.policy == string(policy) {
			return cached.registry, nil
		}
		cached.registry.Close()
		delete(h.bound, c.ID)
	}
	registry, _, err := h.runtime.config.Registry.BindExecutionContext(ec, nil)
	if err != nil {
		return nil, err
	}
	if h.bound == nil {
		h.bound = map[string]*boundRegistry{}
	}
	h.bound[c.ID] = &boundRegistry{policy: string(policy), registry: registry}
	return registry, nil
}

// unbind closes the context's bound registry, if any. Callers hold the
// context lock, which every step using the registry holds as well.
func (h *workflowHost) unbind(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cached := h.bound[id]; cached != nil {
		cached.registry.Close()
		delete(h.bound, id)
	}
}

// close releases every bound registry once no step is still running.
func (h *workflowHost) close() {
	h.calls.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, cached := range h.bound {
		cached.registry.Close()
		delete(h.bound, id)
	}
}

func (h *workflowHost) SaveWorkflow(ctx context.Context, report workflow.Report) error {
	return h.runtime.SaveWorkflow(ctx, report)
}
func (h *workflowHost) Call(ctx context.Context, op workflow.Operation) (value any, callErr error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	h.calls.Add(1)
	defer h.calls.Done()
	defer func() { value, callErr = h.runtime.publicResult(ctx, value, callErr) }()
	r := h.runtime
	a := tools.Args(op.Args)
	if op.Kind != "agent" && op.Kind != "followup" {
		// Every operation but an agent await is work of the parent's
		// workflow call; a parallel step running a tool keeps it active.
		end := r.parentTurn.beginWork(subagent.CallID(ctx))
		defer end()
	}
	switch op.Kind {
	case "integrate":
		return r.integrateOperation(ctx, op.Args)
	case "integration":
		return r.integrationOperation(ctx, op.Args)
	case "task":
		return r.taskOperation(ctx, op.Args)
	case "release":
		var request struct {
			Context string `json:"context"`
		}
		if err := strictRequest(op.Args, &request); err != nil {
			return nil, err
		}
		return h.release(ctx, request.Context)
	case "followup":
		req, err := r.decodeCommitFollowup(ctx, op.Args)
		if err != nil {
			return nil, err
		}
		req.CallID = op.ID
		task, err := r.Followup(ctx, h.controller, req)
		if err != nil {
			return nil, err
		}
		return r.Agent(ctx, h.controller, AgentRequest{Session: task.Owner, TaskID: task.ID, Task: req.Question, Label: req.Label, CallID: op.ID})
	case "agent":
		req, err := r.decodeCommitRequest(ctx, op.Args)
		if err != nil {
			return nil, err
		}
		req.CallID = op.ID
		// A scope supplies a source for new members. Continuations inherit
		// their own authority even when invoked from another scope.
		if req.Session != "" {
			req.Context = ""
		}
		if req.Context != "" {
			if _, err = h.context(ctx, req.Context); err != nil {
				return nil, err
			}
		}
		result, err := r.Agent(ctx, h.controller, req)
		if err != nil {
			code := "agent_failed"
			if llm.IsIterationLimit(err) {
				code = "iteration_limit"
			}
			return result, &workflow.Error{Code: code, Message: err.Error(), Session: result.Session, Usage: result.Usage, Result: result, Cause: err}
		}
		return result, nil
	case "context":
		req, err := r.decodeCommitRequest(ctx, op.Args)
		if err != nil {
			return nil, err
		}
		if req.Context != "" {
			unlock := r.lockContext(req.Context)
			defer unlock()
			if _, err = h.context(ctx, req.Context); err != nil {
				return nil, err
			}
		}
		c, err := r.makeContext(ctx, r.ID, req)
		if err != nil {
			return nil, err
		}
		err = r.update(ctx, func(s *State) error { s.Contexts[c.ID].Owner = h.controller; return nil })
		return c.ID, err
	case "snapshot":
		c, err := h.context(ctx, a.String("context"))
		if err != nil {
			return nil, err
		}
		if c.Checkout == nil {
			return nil, errors.New("snapshot requires an isolated Git checkout")
		}
		unlock := r.lockContext(c.ID)
		defer unlock()
		c, err = h.context(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		m, err := r.manager(ctx)
		if err != nil {
			return nil, err
		}
		snapshot, err := m.Capture(ctx, c.Root)
		if err != nil {
			return nil, err
		}
		err = r.update(ctx, func(s *State) error { s.Snapshots[snapshot.ID] = &snapshot; return nil })
		return snapshot, err
	case "tool", "exec":
		c, err := h.context(ctx, a.String("context"))
		if err != nil {
			return nil, err
		}
		unlock := r.lockContext(c.ID)
		defer unlock()
		c, err = h.context(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		registry, err := h.registry(ctx, s, c)
		if err != nil {
			return nil, err
		}
		name := a.String("name")
		args, _ := op.Args["args"].(map[string]any)
		if op.Kind == "exec" {
			name = "bash"
			args = map[string]any{"command": a.String("command")}
		}
		tool, exists, allowed := registry.GetIfAllowed(name)
		if !exists || !allowed {
			return nil, fail("tool_denied", fmt.Sprintf("tool %q is unavailable in this context", name))
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		call := messages.ChatMessageToolCall{ID: op.ID, Name: name, Arguments: string(encoded)}
		if r.config.Callbacks != nil {
			cb := r.config.Callbacks(ctx, Member{ID: c.Owner, Context: c.ID, ReadOnly: c.ReadOnly})
			if cb != nil {
				if cb.ApproveToolCalls != nil {
					approved := cb.ApproveToolCalls([]messages.ChatMessageToolCall{call})
					if len(approved) != 1 || !approved[0] {
						return nil, fail("tool_denied", "tool call was denied")
					}
				}
				if cb.BeforeToolExecute != nil {
					ctx = cb.BeforeToolExecute(ctx, call, args)
				}
			}
		}
		execution, err := registry.ExecuteTool(ctx, tool, args, r.currentDefaults().agent.ToolTimeout)
		if execution.ContextErr != nil {
			return nil, execution.ContextErr
		}
		output := execution.Output
		refs := []artifacts.Ref{}
		for _, media := range output.Media {
			kind := artifacts.KindBinary
			if strings.HasPrefix(media.MIMEType, "image/") {
				kind = artifacts.KindImage
			}
			ref, e := r.config.Parent.ArtifactStore().Put(ctx, artifacts.Blob{Kind: kind, Data: media.Data, MIMEType: media.MIMEType, Name: media.Name, Reference: media.Reference})
			if e != nil {
				return nil, e
			}
			refs = append(refs, ref)
		}
		value := map[string]any{"text": output.Text, "data": output.Data, "artifacts": refs, "step": op.ID}
		if op.Kind == "exec" {
			if result, ok := output.Data.(tools.CommandResult); ok {
				value["exitCode"] = result.ExitCode
			}
			if check, ok := op.Args["check"].(bool); ok && !check {
				var command *tools.CommandError
				if errors.As(err, &command) {
					err = nil
				}
			}
		}
		if err != nil {
			code := "tool_failed"
			var command *tools.CommandError
			if errors.As(err, &command) {
				code = "command_failed"
			}
			return value, &workflow.Error{Code: code, Message: err.Error(), Result: value, Cause: err}
		}
		return value, nil
	case "log":
		r.event("workflow_log", h.controller, a.String("message"))
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported workflow operation %q", op.Kind)
	}
}

func (h *workflowHost) context(ctx context.Context, id string) (*ExecutionContext, error) {
	if id == "" {
		return nil, errors.New("operation requires an execution context; use an agent result's context or polly.context")
	}
	s, err := h.runtime.read(ctx)
	if err != nil {
		return nil, err
	}
	c := s.Contexts[id]
	if c == nil || c.Release != "" {
		return nil, errors.New("unknown or releasing execution context")
	}
	if c.Owner == h.controller {
		return c, nil
	}
	m := s.Members[c.Owner]
	if m == nil || m.Controller != h.controller {
		return nil, fail("context_denied", "execution context is not reserved by this workflow")
	}
	return c, nil
}
