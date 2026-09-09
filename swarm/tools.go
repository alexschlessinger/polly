package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// Mutations have no successful outcome to report when they fail. Keep this
// separate from tool serialization, where partial workflow reports are useful.
func mutationResult(value any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return value, nil
}

func coordinationToolResult(value any, err error) (string, error) {
	if err != nil && value == nil {
		return "", err
	}
	data, encodeErr := json.MarshalIndent(value, "", "  ")
	if encodeErr != nil {
		return "", errors.Join(err, encodeErr)
	}
	return string(data), err
}

type taskToolView struct {
	*Task
	DisplayStatus string `json:"displayStatus"`
}

func (r *Runtime) registerMemberTools(registry *tools.ToolRegistry, actor, execution string, session sessions.CoordinationSession, structured bool) {
	registry.Register(&publishedArtifactTool{session: session})
	registry.MarkAlwaysAllowed("swarm_read_artifact")
	register := func(name, desc string, params schema.Params, required []string, fn func(context.Context, tools.Args) (any, error)) {
		if structured && name == "swarm_submit" {
			return
		}
		registry.Register(&inspectionTool{&tools.Func{Name: name, LongRunning: strings.HasPrefix(name, "workflow_") || name == "swarm_wait", Exclusive: name == "swarm_snapshot", Coordinator: strings.HasPrefix(name, "workflow_") || name == "swarm_wait" || name == "swarm_integration", Desc: desc, Params: params, Required: required, Run: func(ctx context.Context, a tools.Args) (string, error) {
			v, err := fn(ctx, a)
			return coordinationToolResult(v, err)
		}}})
		registry.MarkAlwaysAllowed(name)
	}
	register("list_agents", "List this family's agents with execution outcome, task disposition and context IDs. Page using next as offset; conversations remain private.", inspectionParams(nil), nil, func(ctx context.Context, a tools.Args) (any, error) { return r.inspectAgents(ctx, actor, a) })
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
	register("swarm_tasks", "List compact shared task summaries. Select task=<id> and section=details for criteria/feedback or section=result for the saved value. pointer is a JSON Pointer within the selection. Page using next as offset.", inspectionParams(schema.Params{"task": schema.S("Stable task ID"), "section": schema.S("summary (default), details or result"), "pointer": schema.S("Optional JSON Pointer within selected task content")}), nil, r.inspectTasks)
	register("swarm_block", "Record a blocker on your assigned task. The parent updates dependencies or resumes work explicitly.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision"), "reason": schema.S("Blocker")}, []string{"task", "revision", "reason"}, func(ctx context.Context, a tools.Args) (any, error) {
		return mutationResult("blocked", r.BlockTask(ctx, actor, a.String("task"), a.Int("revision", 0), a.String("reason")))
	})
	register("swarm_claim", "Atomically claim an unassigned pending task whose dependencies are accepted.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision")}, []string{"task", "revision"}, func(ctx context.Context, a tools.Args) (any, error) {
		return mutationResult("claimed", r.Claim(ctx, actor, a.String("task"), a.Int("revision", 0)))
	})
	register("swarm_submit", "Submit your task result for parent review. A snapshot records the exact editing candidate.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision"), "result": schema.S("Result and evidence"), "snapshot": schema.S("Published snapshot ID")}, []string{"task", "revision", "result"}, func(ctx context.Context, a tools.Args) (any, error) {
		return mutationResult("submitted", r.Submit(ctx, actor, a.String("task"), a.Int("revision", 0), a.String("result"), a.String("snapshot")))
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
			return mutationResult("coordination changed; inspect messages and tasks", r.waitParent(ctx))
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
// Members hash by lifecycle, control and execution identity: a wake that moves
// the same execution from queued to running is not a transition, and labels
// never are.
func coordinationFingerprint(s *State) string {
	statuses := map[string]any{}
	for id, t := range s.Tasks {
		statuses["task:"+id] = struct {
			Status, Owner, Execution, Snapshot, Feedback string
			Revision, Accepted                           int
			Deferred                                     bool
		}{t.Status, t.Owner, t.Execution, t.Snapshot, t.Feedback, t.Revision, t.AcceptedRevision, TaskDeferred(s, t)}
	}
	for id, m := range s.Members {
		generation := 0
		if e := s.Executions[m.Execution]; e != nil {
			generation = e.Generation
		}
		p := MemberState(s, m)
		statuses["member:"+id] = struct {
			Lifecycle  Lifecycle
			Control    MemberControl
			Execution  string
			Generation int
		}{p.Lifecycle, p.Control, m.Execution, generation}
	}
	for id, w := range s.Workflows {
		statuses["workflow:"+id] = struct {
			Status       string
			Acknowledged bool
		}{w.Status, w.Acknowledged}
	}
	return tools.Result(statuses)
}

// RegisterParentTools binds parent-only authority in closures, never in model
// arguments. A child cannot gain it by supplying a different caller identity.
func (r *Runtime) RegisterParentTools(registry *tools.ToolRegistry) {
	r.registerIntegrationTool(registry)
	r.registerMemberTools(registry, r.ID, "", r.parent, false)
	spawn := subagent.NewTool(r.Spawn, subagent.WithRuntimeScheduler())
	registry.Register(spawn)
	registry.MarkAlwaysAllowed(subagent.ToolName)
	register := func(name, desc string, params schema.Params, required []string, fn func(context.Context, tools.Args) (any, error)) {
		registry.Register(&inspectionTool{&tools.Func{Name: name, LongRunning: strings.HasPrefix(name, "workflow_") || name == "swarm_wait", Exclusive: name == "swarm_snapshot", Coordinator: strings.HasPrefix(name, "workflow_") || name == "swarm_wait" || name == "swarm_integration", Desc: desc, Params: params, Required: required, Run: func(ctx context.Context, a tools.Args) (string, error) {
			v, err := fn(ctx, a)
			return coordinationToolResult(v, err)
		}}})
		registry.MarkAlwaysAllowed(name)
	}
	register("swarm_create_task", "Create and prioritize shared work. Only you may create, reassign, accept and integrate tasks.", schema.Params{"description": schema.S("Assignment"), "criteria": schema.S("Acceptance criteria"), "dependencies": schema.Strings("Task IDs"), "owner": schema.S("Optional member ID")}, []string{"description", "criteria"}, func(ctx context.Context, a tools.Args) (any, error) {
		return r.CreateTask(ctx, a.String("description"), a.String("criteria"), a.StringSlice("dependencies"), a.String("owner"))
	})
	register("swarm_update_task", "Reassign or unblock a task by setting its owner and dependencies. Stop an active owner first; dependency cycles are refused.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision"), "owner": schema.S("Member ID or empty for claims"), "dependencies": schema.Strings("Replacement dependency IDs")}, []string{"task", "revision", "owner", "dependencies"}, func(ctx context.Context, a tools.Args) (any, error) {
		return mutationResult("updated", r.UpdateTask(ctx, a.String("task"), a.Int("revision", 0), a.String("owner"), a.StringSlice("dependencies")))
	})
	register("swarm_review", "Accept the current submitted revision or request changes with feedback. Accepted unchanged candidates finish immediately; changed candidates still require swarm_integration. Read the returned status and next action.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Submitted revision"), "accept": schema.Bool("Accept result"), "feedback": schema.S("Changes requested")}, []string{"task", "revision", "accept"}, func(ctx context.Context, a tools.Args) (any, error) {
		if err := r.Review(ctx, a.String("task"), a.Int("revision", 0), a.Bool("accept"), a.String("feedback")); err != nil {
			return nil, err
		}
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		task := s.Tasks[a.String("task")]
		if task == nil {
			return nil, errors.New("reviewed task is no longer available")
		}
		result := map[string]any{"task": task.ID, "revision": task.Revision, "acceptedRevision": task.AcceptedRevision, "status": task.Status, "displayStatus": TaskStatus(task)}
		if task.Status == "awaiting_review" && task.AcceptedRevision == task.Revision && task.Snapshot != "" {
			if base, _ := taskSnapshots(s, task); base == nil {
				result["nextAction"] = "Snapshot provenance is unavailable; restore the original task snapshots or cancel the task."
			} else {
				result["nextAction"] = fmt.Sprintf("Use swarm_integration to prepare task %s revision %d, inspect the candidate, then accept and apply it. Cleanup does not integrate this result.", task.ID, task.Revision)
			}
		} else if task.Status == "changes_requested" {
			owner := s.Members[task.Owner]
			var execution *Execution
			if owner != nil {
				execution = s.Executions[owner.Execution]
			}
			switch {
			case owner == nil || owner.Control == MemberControlRetired || s.Contexts[owner.Context] == nil:
				result["nextAction"] = "The previous member cannot resume; use swarm_update_task to reassign the task to an available member, or cancel the task."
			case owner.Controller != "":
				if w := s.Workflows[owner.Controller]; w != nil && w.Status == "running" {
					result["nextAction"] = "Explicitly continue the member through its owning workflow to produce a revised submission."
				} else {
					result["nextAction"] = "Use swarm_control resume with the member ID after other active work settles. The terminal workflow is not replayed."
				}
			case owner.Control == MemberControlStopped || execution != nil && (execution.Status == "paused" || execution.Status == "failed"):
				result["nextAction"] = "Use swarm_control resume with the member ID to request a revised submission; an exhausted iteration allowance requires a user-directed grant."
			default:
				result["nextAction"] = "Wait for the member's revised submission."
			}
		}
		return result, nil
	})
	register("swarm_control", "Stop or explicitly resume a member using its remaining allowance, cancel a task, or clean an integrated/unchanged execution context. Cleanup retires the context; it does not accept or integrate tasks. Iteration exhaustion retains saved work and requires a user-directed client grant. Extra execution budgets also require a user-directed client control.", schema.Params{"action": schema.S("stop, resume, cancel_task or cleanup"), "id": schema.S("stop/resume: member ID; cancel_task: task ID; cleanup: context ID from list_agents.items[].context, not the execution ID")}, []string{"action"}, func(ctx context.Context, a tools.Args) (any, error) {
		switch a.String("action") {
		case "stop":
			return mutationResult("stopped", r.StopMember(ctx, a.String("id")))
		case "resume":
			if err := r.Resume(ctx, a.String("id"), 0); err != nil {
				return nil, err
			}
			return "resumed", nil
		case "cancel_task":
			return mutationResult("canceled", r.CancelTask(ctx, a.String("id")))
		case "cleanup":
			if a.String("id") == "" {
				return nil, errors.New("name an inactive execution context; whole-family cleanup is a client control")
			}
			return mutationResult("retired", r.Cleanup(ctx, a.String("id")))
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
		return mutationResult(map[string]any{"id": id, "status": "started"}, err)
	})
	register("workflow_cancel", "Cancel a running workflow and interrupt its active executions; retain finished outcomes and unresolved tasks.", schema.Params{"id": schema.S("Workflow ID")}, []string{"id"}, func(ctx context.Context, a tools.Args) (any, error) {
		return mutationResult("canceled", r.CancelWorkflow(a.String("id")))
	})
	register("workflow_acknowledge", "Acknowledge a terminal workflow report. After reporting a failure, defer=true with a nonblank note retains its unresolved work for later without accepting, applying or canceling it.", schema.Params{"id": schema.S("Workflow report ID"), "defer": schema.Bool("Explicitly defer unresolved work from a terminal failure"), "note": schema.S("Required explanation when deferring")}, []string{"id"}, func(ctx context.Context, a tools.Args) (any, error) {
		if a.Bool("defer") {
			return mutationResult("acknowledged and deferred", r.DeferWorkflow(ctx, a.String("id"), a.String("note")))
		}
		return mutationResult("acknowledged", r.AcknowledgeWorkflow(ctx, a.String("id")))
	})
	register("workflow_read", "Inspect a saved workflow without executing it. Defaults to a compact summary. List steps, then select a stable step ID; pointer selects within a section. Large selections are attached as readable artifacts.", inspectionParams(schema.Params{"id": schema.S("Workflow report ID"), "section": schema.S("summary (default), steps, step, source, input or output"), "step": schema.S("Stable step ID for section=step"), "pointer": schema.S("Optional JSON Pointer within the selected section, e.g. /value/value/claims/0")}), []string{"id"}, r.inspectWorkflow)
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
