package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

type workflowHost struct {
	runtime    *Runtime
	controller string
}

func (h *workflowHost) SaveWorkflow(ctx context.Context, report workflow.Report) error {
	return h.runtime.SaveWorkflow(ctx, report)
}
func (h *workflowHost) Call(ctx context.Context, op workflow.Operation) (any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	r := h.runtime
	a := tools.Args(op.Args)
	switch op.Kind {
	case "agent":
		req, err := decodeRequest(op.Args)
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
		req, err := decodeRequest(op.Args)
		if err != nil {
			return nil, err
		}
		if req.Context != "" {
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
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		ec, err := r.contextPolicy(ctx, s, c)
		if err != nil {
			return nil, err
		}
		registry, _, err := r.config.Registry.BindExecutionContext(ec, nil)
		if err != nil {
			return nil, err
		}
		defer registry.Close()
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
		var cancel context.CancelFunc
		timeout := r.currentDefaults().agent.ToolTimeout
		if untimed, ok := tool.(tools.UntimedTool); timeout > 0 && !(ok && untimed.Untimed()) {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout))
			defer cancel()
		}
		var output tools.ToolOutput
		if rich, ok := tool.(tools.OutputTool); ok {
			output, err = rich.ExecuteOutput(ctx, args)
		} else {
			output.Text, err = tool.Execute(ctx, args)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
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
	if c == nil {
		return nil, errors.New("unknown execution context")
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
