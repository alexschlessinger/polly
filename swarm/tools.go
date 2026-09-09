package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func (r *Runtime) registerMemberTools(registry *tools.ToolRegistry, actor, execution string, session sessions.CoordinationSession) {
	registry.Register(&publishedArtifactTool{session: session})
	registry.MarkAlwaysAllowed("swarm_read_artifact")
	register := func(name, desc string, params schema.Params, required []string, fn func(context.Context, tools.Args) (any, error)) {
		registry.Register(&tools.Func{Name: name, LongRunning: strings.HasPrefix(name, "workflow_") || name == "swarm_wait", Exclusive: name == "swarm_snapshot", Coordinator: strings.HasPrefix(name, "workflow_") || name == "swarm_wait" || name == "swarm_apply" || name == "swarm_preview" || name == "swarm_integration", Desc: desc, Params: params, Required: required, Run: func(ctx context.Context, a tools.Args) (string, error) {
			v, err := fn(ctx, a)
			if err != nil {
				return tools.Result(v), err
			}
			return tools.Result(v), nil
		}})
		registry.MarkAlwaysAllowed(name)
	}
	register("list_agents", "List your parent and direct teammates, their assignments and status. Conversations remain private.", nil, nil, func(ctx context.Context, a tools.Args) (any, error) {
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"self": actor, "parent": r.ID, "members": s.Members}, nil
	})
	register("send_message", "Send addressed teammate information. A request expects a reply; information waits until the next active turn.", schema.Params{"to": schema.S("Stable member ID"), "kind": schema.S("info, request or reply"), "reply_to": schema.S("Request ID for replies"), "text": schema.S("Message")}, []string{"to", "kind", "text"}, func(ctx context.Context, a tools.Args) (any, error) {
		return r.Send(ctx, actor, a.String("to"), a.String("kind"), a.String("reply_to"), a.String("text"))
	})
	register("read_messages", "Read mail addressed to you. Only input admission marks delivery.", nil, nil, func(ctx context.Context, a tools.Args) (any, error) {
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		return inbox(s, actor, false), nil
	})
	register("swarm_tasks", "List shared task status and acceptance criteria.", nil, nil, func(ctx context.Context, a tools.Args) (any, error) {
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		return s.Tasks, nil
	})
	register("swarm_block", "Record a blocker on your assigned task. The parent updates dependencies or resumes work explicitly.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision"), "reason": schema.S("Blocker")}, []string{"task", "revision", "reason"}, func(ctx context.Context, a tools.Args) (any, error) {
		return "blocked", r.BlockTask(ctx, actor, a.String("task"), a.Int("revision", 0), a.String("reason"))
	})
	register("swarm_claim", "Atomically claim an unassigned pending task whose dependencies are accepted.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision")}, []string{"task", "revision"}, func(ctx context.Context, a tools.Args) (any, error) {
		return "claimed", r.Claim(ctx, actor, a.String("task"), a.Int("revision", 0))
	})
	register("swarm_submit", "Submit your task result for parent review. A snapshot records the exact editing candidate.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision"), "result": schema.S("Result and evidence"), "snapshot": schema.S("Published snapshot ID")}, []string{"task", "revision", "result"}, func(ctx context.Context, a tools.Args) (any, error) {
		return "submitted", r.Submit(ctx, actor, a.String("task"), a.Int("revision", 0), a.String("result"), a.String("snapshot"))
	})
	register("swarm_publish", "Publish attributed findings with source references; optionally supersede your earlier publication.", schema.Params{"artifacts": schema.Strings("IDs of your own artifacts to publish"), "text": schema.S("Finding and evidence"), "sources": schema.Strings("Source references"), "supersedes": schema.S("Earlier publication ID"), "snapshot": schema.S("Snapshot ID")}, []string{"text"}, func(ctx context.Context, a tools.Args) (any, error) {
		refs := []artifacts.Ref{}
		for _, id := range a.StringSlice("artifacts") {
			refs = append(refs, artifacts.Ref{ID: id, Kind: artifacts.KindBinary})
		}
		return r.Publish(ctx, actor, Publication{Artifacts: refs, Text: a.String("text"), Sources: a.StringSlice("sources"), Supersedes: a.String("supersedes"), Snapshot: a.String("snapshot")})
	})
	register("swarm_search", "Search explicitly published findings across this parent's retained runs.", schema.Params{"query": schema.S("Literal, case-insensitive query")}, nil, func(ctx context.Context, a tools.Args) (any, error) {
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		pubs := []*Publication{}
		q := strings.ToLower(a.String("query"))
		for _, p := range s.Publications {
			if strings.Contains(strings.ToLower(p.Text), q) {
				pubs = append(pubs, p)
			}
		}
		return pubs, nil
	})
	register("swarm_snapshot", "Capture an immutable candidate from your isolated files. This does not commit to a branch or accept a task.", nil, nil, func(ctx context.Context, a tools.Args) (any, error) { return r.captureMember(ctx, actor) })
	register("swarm_wait", "Yield after this tool batch until addressed input is available. The runtime releases your execution slot and preserves your iteration budget.", nil, nil, func(ctx context.Context, a tools.Args) (any, error) {
		if actor == r.ID {
			return "coordination changed; inspect messages and tasks", r.waitParent(ctx)
		}
		park, ok := ctx.Value(waitKey{}).(func())
		if !ok {
			return nil, errors.New("wait requires an active member execution")
		}
		park()
		return "yielded; the current invocation resumes on addressed input", nil
	})
}

func (r *Runtime) waitParent(ctx context.Context) error {
	s, err := r.read(ctx)
	if err != nil {
		return err
	}
	before := coordinationFingerprint(s)
	for {
		r.mu.Lock()
		notify := r.notify
		active := len(r.active) + len(r.workflowCancels)
		r.mu.Unlock()
		s, err = r.read(ctx)
		if err != nil {
			return err
		}
		if active == 0 || len(inbox(s, r.ID, true)) > 0 || coordinationFingerprint(s) != before {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		}
	}
}

// coordinationFingerprint covers what a waiting parent acts on: task changes
// and member or workflow status transitions. A member that was already parked
// when the wait began is not news, so its steady state cannot end the wait.
func coordinationFingerprint(s *State) string {
	statuses := make(map[string]string, len(s.Members)+len(s.Workflows))
	for id, m := range s.Members {
		statuses["member:"+id] = m.Status
	}
	for id, w := range s.Workflows {
		statuses["workflow:"+id] = w.Status
	}
	return tools.Result(s.Tasks) + tools.Result(statuses)
}

// RegisterParentTools binds parent-only authority in closures, never in model
// arguments. A child cannot gain it by supplying a different caller identity.
func (r *Runtime) RegisterParentTools(registry *tools.ToolRegistry) {
	r.registerIntegrationTool(registry)
	r.registerMemberTools(registry, r.ID, "", r.parent)
	spawn := subagent.NewTool(r.Spawn, subagent.WithRuntimeScheduler())
	registry.Register(spawn)
	registry.MarkAlwaysAllowed(subagent.ToolName)
	register := func(name, desc string, params schema.Params, required []string, fn func(context.Context, tools.Args) (any, error)) {
		registry.Register(&tools.Func{Name: name, LongRunning: strings.HasPrefix(name, "workflow_") || name == "swarm_wait", Exclusive: name == "swarm_snapshot", Coordinator: strings.HasPrefix(name, "workflow_") || name == "swarm_wait" || name == "swarm_apply" || name == "swarm_preview" || name == "swarm_integration", Desc: desc, Params: params, Required: required, Run: func(ctx context.Context, a tools.Args) (string, error) {
			v, err := fn(ctx, a)
			if err != nil {
				return tools.Result(v), err
			}
			return tools.Result(v), nil
		}})
		registry.MarkAlwaysAllowed(name)
	}
	register("swarm_create_task", "Create and prioritize shared work. Only you may create, reassign, accept and integrate tasks.", schema.Params{"description": schema.S("Assignment"), "criteria": schema.S("Acceptance criteria"), "dependencies": schema.Strings("Task IDs"), "owner": schema.S("Optional member ID")}, []string{"description", "criteria"}, func(ctx context.Context, a tools.Args) (any, error) {
		return r.CreateTask(ctx, a.String("description"), a.String("criteria"), a.StringSlice("dependencies"), a.String("owner"))
	})
	register("swarm_update_task", "Reassign or unblock a task by setting its owner and dependencies. Stop an active owner first; dependency cycles are refused.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision"), "owner": schema.S("Member ID or empty for claims"), "dependencies": schema.Strings("Replacement dependency IDs")}, []string{"task", "revision", "owner", "dependencies"}, func(ctx context.Context, a tools.Args) (any, error) {
		return "updated", r.UpdateTask(ctx, a.String("task"), a.Int("revision", 0), a.String("owner"), a.StringSlice("dependencies"))
	})
	register("swarm_review", "Accept the current submitted revision or request changes with feedback. Editing tasks finish after applying the accepted snapshot.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Submitted revision"), "accept": schema.Bool("Accept result"), "feedback": schema.S("Changes requested")}, []string{"task", "revision", "accept"}, func(ctx context.Context, a tools.Args) (any, error) {
		return "review recorded", r.Review(ctx, a.String("task"), a.Int("revision", 0), a.Bool("accept"), a.String("feedback"))
	})
	register("swarm_preview", "Prepare a three-way integration preview against the current parent files.", schema.Params{"task": schema.S("Task ID")}, []string{"task"}, func(ctx context.Context, a tools.Args) (any, error) { return r.preview(ctx, a.String("task")) })
	register("swarm_apply", "Apply an accepted clean preview under the parent execution gate. The parent's branch and index remain unchanged.", schema.Params{"task": schema.S("Accepted task ID"), "preview": schema.S("Preview ID"), "reconcile": schema.Bool("Observe an interrupted apply without writing")}, []string{"task", "preview"}, func(ctx context.Context, a tools.Args) (any, error) {
		if a.Bool("reconcile") {
			return r.ReconcileApply(ctx, a.String("preview"))
		}
		return "applied", r.apply(ctx, a.String("task"), a.String("preview"))
	})
	register("swarm_control", "Stop or explicitly resume a member using its remaining allowance, cancel a task, or clean an integrated/unchanged execution context. Iteration exhaustion retains saved work and requires a user-directed client grant. Extra execution budgets also require a user-directed client control.", schema.Params{"action": schema.S("stop, resume, cancel_task or cleanup"), "id": schema.S("Member, task or context ID")}, []string{"action"}, func(ctx context.Context, a tools.Args) (any, error) {
		switch a.String("action") {
		case "stop":
			return "stopped", r.StopMember(ctx, a.String("id"))
		case "resume":
			if err := r.Resume(ctx, a.String("id"), 0); err != nil {
				return nil, err
			}
			return "resumed", nil
		case "cancel_task":
			return "canceled", r.CancelTask(ctx, a.String("id"))
		case "cleanup":
			if a.String("id") == "" {
				return nil, errors.New("name an inactive execution context; whole-family cleanup is a client control")
			}
			return "retired", r.Cleanup(ctx, a.String("id"))
		default:
			return nil, errors.New("budget grants require explicit user-directed resume through the client")
		}
	})
	register("workflow_run", "Run a JavaScript workflow using the same swarm scheduler and tool authority. Supply source containing polly.defineWorkflow and JSON input. Parent workflows can review tasks and prepare, repair, accept, and apply integration candidates. Children retain their existing permissions.", schema.Params{"source": schema.S("JavaScript source"), "input": schema.S("JSON input")}, []string{"source", "input"}, func(ctx context.Context, a tools.Args) (any, error) {
		input, err := schema.DecodeJSON(a.String("input"))
		if err != nil {
			return nil, err
		}
		report, err := r.RunWorkflow(ctx, a.String("source"), input)
		if report == nil {
			return nil, err
		}
		return map[string]any{"id": report.ID, "status": report.Status, "output": report.Output, "steps": len(report.Steps)}, err
	})
	register("workflow_start", "Start a background JavaScript workflow. Inspect its saved report and coordinate until the swarm settles.", schema.Params{"source": schema.S("JavaScript source"), "input": schema.S("JSON input")}, []string{"source", "input"}, func(ctx context.Context, a tools.Args) (any, error) {
		input, err := schema.DecodeJSON(a.String("input"))
		if err != nil {
			return nil, err
		}
		id, err := r.StartWorkflow(ctx, a.String("source"), input)
		return map[string]any{"id": id, "status": "started"}, err
	})
	register("workflow_cancel", "Cancel a running workflow and pause its reserved members.", schema.Params{"id": schema.S("Workflow ID")}, []string{"id"}, func(ctx context.Context, a tools.Args) (any, error) {
		return "canceled", r.CancelWorkflow(a.String("id"))
	})
	register("workflow_acknowledge", "Acknowledge a terminal workflow failure after inspecting its report and arranging recovery or reporting the blocker. This does not accept tasks, discard edits, or resume members.", schema.Params{"id": schema.S("Workflow report ID")}, []string{"id"}, func(ctx context.Context, a tools.Args) (any, error) {
		return "acknowledged", r.AcknowledgeWorkflow(ctx, a.String("id"))
	})
	register("workflow_read", "Inspect saved workflow source, inputs, steps and result without resuming JavaScript.", schema.Params{"id": schema.S("Workflow report ID")}, []string{"id"}, func(ctx context.Context, a tools.Args) (any, error) {
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		w := s.Workflows[a.String("id")]
		if w == nil {
			return nil, errors.New("unknown workflow")
		}
		return w, nil
	})
}

func (r *Runtime) captureMember(ctx context.Context, actor string) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	m := s.Members[actor]
	if m == nil {
		return nil, errors.New("snapshot requires a member")
	}
	c := s.Contexts[m.Context]
	if c == nil || c.Checkout == nil {
		return nil, errors.New("snapshot requires an isolated Git checkout")
	}
	manager, err := r.manager(ctx)
	if err != nil {
		return nil, err
	}
	snapshot, err := manager.Capture(ctx, c.Root)
	if err != nil {
		return nil, err
	}
	err = r.update(ctx, func(s *State) error { s.Snapshots[snapshot.ID] = &snapshot; return nil })
	return snapshot, err
}
func (r *Runtime) preview(ctx context.Context, taskID string) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	task := s.Tasks[taskID]
	if task == nil {
		return nil, errors.New("unknown task")
	}
	return r.PrepareIntegration(ctx, []TaskReference{{Task: taskID, Revision: task.Revision}}, "paths")
}
func (r *Runtime) apply(ctx context.Context, taskID, candidateID string) error {
	c, err := r.ReadIntegration(ctx, candidateID)
	if err != nil {
		return err
	}
	if len(c.Inputs) != 1 || len(c.Repairs) != 0 || c.Inputs[0].Task != taskID {
		return errors.New("single-task apply requires that task's candidate")
	}
	if c.Receipt == nil || c.Receipt.Status != "applied" {
		s, err := r.read(ctx)
		if err != nil {
			return err
		}
		if err = acceptedTasks(s, c.references()); err != nil {
			return err
		}
		// The explicit parent apply call names this exact candidate. Preserve the
		// direct-tool sequence (task review, preview, apply) through the same service.
		if _, err = r.AcceptIntegration(ctx, candidateID); err != nil {
			return err
		}
	}
	_, err = r.ApplyIntegration(ctx, candidateID)
	return err
}

func decodeRequest(args map[string]any) (AgentRequest, error) {
	// JavaScript is model-supplied orchestration, not host configuration.
	// Reject case variants too: encoding/json matches struct keys loosely.
	for key := range args {
		if strings.EqualFold(strings.ReplaceAll(key, "_", ""), "maxIterations") {
			return AgentRequest{}, fail("invalid_args", "maxIterations is controlled by the host; omit it to inherit the configured limit")
		}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return AgentRequest{}, err
	}
	var req AgentRequest
	err = json.Unmarshal(data, &req)
	return req, err
}
