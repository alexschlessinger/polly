package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/schema"
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

// Coordination effects must run outside ordinary parent filesystem batches.
// In particular, cancellation and acknowledgment retain the workflow tools'
// coordinator and timeout traits after moving into swarm_control.
func registerCoordinationTool(registry *tools.ToolRegistry, name, desc string, params schema.Params, required []string, fn func(context.Context, tools.Args) (any, error)) {
	coordinator := name == "workflow_run" || name == "wait_agent" || name == "followup_task" || name == "interrupt_agent" || name == "spawn_agent" || name == "swarm_control"
	registry.Register(&inspectionTool{&tools.Func{Name: name, LongRunning: coordinator, Coordinator: coordinator, Desc: desc, Params: params, Required: required, Run: func(ctx context.Context, a tools.Args) (string, error) {
		v, err := fn(ctx, a)
		return coordinationToolResult(v, err)
	}}})
	registry.MarkAlwaysAllowed(name)
}

func (r *Runtime) registerMemberTools(registry *tools.ToolRegistry, actor string) {
	register := func(name, desc string, params schema.Params, required []string, fn func(context.Context, tools.Args) (any, error)) {
		registerCoordinationTool(registry, name, desc, params, required, fn)
	}
	r.registerReader(registry, actor)
	r.registerDelegationTools(registry, actor)
	if actor != r.ID {
		register("swarm_block", "Record a blocker on your assigned task. The parent updates dependencies or resumes work explicitly.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Observed revision"), "reason": schema.S("Blocker")}, []string{"task", "revision", "reason"}, func(ctx context.Context, a tools.Args) (any, error) {
			return mutationResult("blocked", r.BlockTask(ctx, actor, a.String("task"), a.Int("revision", 0), a.String("reason")))
		})
	}
	register("swarm_publish", "Publish attributed findings or artifacts another worker needs during ongoing work, with source references; optionally supersede your earlier publication. Final results are delivered through task completion and need no separate publication.", schema.Params{"artifacts": schema.Strings("IDs of your own artifacts to publish"), "text": schema.S("Finding and evidence"), "sources": schema.Strings("Source references"), "supersedes": schema.S("Earlier publication ID"), "commit": schema.S("Full Git commit from a retained capture")}, []string{"text"}, func(ctx context.Context, a tools.Args) (any, error) {
		if err := delegationArgs(a, "artifacts", "text", "sources", "supersedes", "commit"); err != nil {
			return nil, err
		}
		snapshot, err := r.commitArgument(ctx, a)
		if err != nil {
			return nil, err
		}
		refs := []artifacts.Ref{}
		for _, id := range a.StringSlice("artifacts") {
			refs = append(refs, artifacts.Ref{ID: id, Kind: artifacts.KindBinary})
		}
		value, err := r.Publish(ctx, actor, Publication{Artifacts: refs, Text: a.String("text"), Sources: a.StringSlice("sources"), Supersedes: a.String("supersedes"), Snapshot: snapshot})
		return r.publicResult(ctx, value, err)
	})
}

func (r *Runtime) waitParent(ctx context.Context) error {
	end := r.parentTurn.beginWait(subagent.CallID(ctx))
	defer end()
	s, err := r.read(ctx)
	if err != nil {
		return err
	}
	before, _ := coordinationEntries(s)
	for {
		r.mu.Lock()
		notify := r.notify
		active := len(r.active) + len(r.workflowCancels)
		r.mu.Unlock()
		s, err = r.read(ctx)
		if err != nil {
			return err
		}
		if active == 0 || len(inbox(s, r.ID, true)) > 0 || parentWaitChanged(before, s) {
			_, err := r.repairDeliveryNotices(ctx, s)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		}
	}
}

// coordinationEntries derives one comparable record per task, member and
// workflow, and marks the records a running workflow controls. Members hash
// by lifecycle, control and execution identity: a wake that moves the same
// execution from queued to running is not a transition, and labels never are.
func coordinationEntries(s *State) (entries map[string]any, controlled map[string]bool) {
	entries, controlled = map[string]any{}, map[string]bool{}
	for id, t := range s.Tasks {
		entries["task:"+id] = struct {
			Status, Owner, Execution, Snapshot, Feedback string
			Revision, Accepted                           int
			Deferred, PendingNotice                      bool
		}{t.Status, t.Owner, t.Execution, t.Snapshot, t.Feedback, t.Revision, t.AcceptedRevision, TaskDeferred(s, t), resultNotice(s, t) != nil && !resultNotice(s, t).Delivered}
		controlled["task:"+id] = workflowControlled(s, s.Executions[t.Execution])
	}
	for id, m := range s.Members {
		generation := 0
		if e := s.Executions[m.Execution]; e != nil {
			generation = e.Generation
		}
		p := MemberState(s, m)
		entries["member:"+id] = struct {
			Lifecycle  Lifecycle
			Control    MemberControl
			Execution  string
			Generation int
		}{p.Lifecycle, p.Control, m.Execution, generation}
		controlled["member:"+id] = memberControlled(s, m)
	}
	for id, w := range s.Workflows {
		entries["workflow:"+id] = struct {
			Status       string
			Acknowledged bool
		}{w.Status, w.Acknowledged}
	}
	return entries, controlled
}

// coordinationFingerprint hashes every entry. The settlement nudge in
// bindParent compares it: any coordination change, workflow-internal or not,
// earns the parent another nudge rather than a blocked turn.
func coordinationFingerprint(s *State) string {
	entries, _ := coordinationEntries(s)
	return tools.Result(entries)
}

// parentWaitChanged is what ends a parked parent's wait: an entry that
// differs from before, unless a running workflow controls it now. Workflow
// progress reaches the parent once, through the workflow's own status entry,
// and a record a workflow takes over during the wait is not news either. A
// member that was already parked when the wait began is not news at all.
func parentWaitChanged(before map[string]any, s *State) bool {
	entries, controlled := coordinationEntries(s)
	for key, value := range entries {
		if !controlled[key] && before[key] != value {
			return true
		}
	}
	return false
}

// RegisterParentTools binds parent-only authority in closures, never in model
// arguments. A child cannot gain it by supplying a different caller identity.
func (r *Runtime) RegisterParentTools(registry *tools.ToolRegistry) {
	registerHelpTools(registry)
	r.registerIntegrationTool(registry)
	r.registerMemberTools(registry, r.ID)
	register := func(name, desc string, params schema.Params, required []string, fn func(context.Context, tools.Args) (any, error)) {
		registerCoordinationTool(registry, name, desc, params, required, fn)
	}
	register("swarm_review", "Accept the current submitted revision of research you asked to review, or request changes with feedback on any submitted task. Editing work is accepted by integrating it: use swarm_integrate. Ordinary research completes on delivery and refuses review; use followup_task for a correction. Read the returned status and next action.", schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Submitted revision"), "accept": schema.Bool("Accept result"), "feedback": schema.S("Changes requested")}, []string{"task", "revision", "accept"}, func(ctx context.Context, a tools.Args) (any, error) {
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
		result := map[string]any{"task": task.ID, "revision": task.Revision, "acceptedRevision": task.AcceptedRevision, "status": task.Status, "displayStatus": TaskStatusIn(s, task)}
		if task.Status == "awaiting_review" && task.AcceptedRevision == task.Revision && task.Snapshot != "" {
			if base, _ := taskSnapshots(s, task); base == nil {
				result["nextAction"] = "Captured commit provenance is unavailable; cancel the task with swarm_control and start a new assignment."
			} else {
				result["nextAction"] = fmt.Sprintf("Integrate task %s revision %d with swarm_integrate; cleanup does not integrate this result.", task.ID, task.Revision)
			}
		} else if task.Status == "changes_requested" {
			owner := s.Members[task.Owner]
			var execution *Execution
			if owner != nil {
				execution = s.Executions[owner.Execution]
			}
			switch {
			case owner == nil:
				result["nextAction"] = "The previous member cannot resume; use polly.tasks.update in workflow_run to reassign the task to an available member, or cancel the task."
			case owner.Controller != "":
				if w := s.Workflows[owner.Controller]; w != nil && w.Status == "running" {
					result["nextAction"] = "Explicitly continue the member through its owning workflow to produce a revised submission."
				} else {
					result["nextAction"] = "Use followup_task with the member ID after other active work settles. The terminal workflow is not replayed."
				}
			case owner.Control == MemberControlStopped || execution != nil && (execution.Status == "paused" || execution.Status == "failed"):
				result["nextAction"] = "Use followup_task with the member ID to request a revised submission; an exhausted iteration allowance requires a user-directed grant."
			default:
				result["nextAction"] = "Use followup_task with the member target and revision instructions, or explicitly reassign through workflow_run."
			}
		}
		return result, nil
	})
	register("swarm_control", "Cancel a task or workflow, acknowledge a terminal workflow, or release an eligible workspace. Completed reports need no acknowledgment. After reporting a failure, acknowledge_workflow with defer:true and a nonblank note retains unresolved work without accepting, applying or canceling it. Release preserves member/task provenance and returns context, status and a reason when not released. Iteration exhaustion and extra execution budgets require a user-directed client grant.", schema.Params{
		"action": schema.Enum("Control operation", "cancel_task", "release", "cancel_workflow", "acknowledge_workflow"),
		"id":     schema.S("Member, task, workflow or context ID for the action; release uses list_agents({details:true}) items[].context"),
		"defer":  schema.Bool("acknowledge_workflow: explicitly defer unresolved work from a terminal failure"),
		"note":   schema.S("Required explanation when deferring"),
	}, []string{"action", "id"}, func(ctx context.Context, a tools.Args) (any, error) {
		switch a.String("action") {
		case "cancel_task":
			return mutationResult("canceled", r.CancelTask(ctx, a.String("id")))
		case "release":
			return r.releaseWorkspace(ctx, a.String("id"))
		case "cancel_workflow":
			return mutationResult("canceled", r.CancelWorkflow(a.String("id")))
		case "acknowledge_workflow":
			if a.Bool("defer") {
				return mutationResult("acknowledged and deferred", r.DeferWorkflow(ctx, a.String("id"), a.String("note")))
			}
			return mutationResult("acknowledged", r.AcknowledgeWorkflow(ctx, a.String("id")))
		default:
			return nil, errors.New("unknown control action; budget grants require explicit user-directed resume through the client")
		}
	})
	r.registerWorkflowTool(registry)
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
	if err := delegationArgs(tools.Args(args), "taskName", "task", "label", "session", "taskID", "source", "commit", "context", "readOnly", "review", "tools", "modelHost", "model", "schema", "input", "callID"); err != nil {
		return AgentRequest{}, err
	}
	data, err := json.Marshal(args)
	if err != nil {
		return AgentRequest{}, err
	}
	var req AgentRequest
	err = json.Unmarshal(data, &req)
	return req, err
}
