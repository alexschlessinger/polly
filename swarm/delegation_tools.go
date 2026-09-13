package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func delegationArgs(a tools.Args, allowed ...string) error {
	if err := rejectSnapshotArgument(a); err != nil {
		return err
	}
	for key := range a {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown argument %q", key)
		}
	}
	return nil
}

func (r *Runtime) registerDelegationTools(registry *tools.ToolRegistry, actor string) {
	registerCoordinationTool(registry, "list_agents", "List workers with compact execution and task state, including idle workers after workspace release. An idle worker may still have unfinished work. Use details:true for context/task/execution IDs, budgets and full state; swarm_read shows decisions. Filter by canonical path_prefix and page with offset/limit.", inspectionParams(schema.Params{"path_prefix": schema.S("Canonical path prefix, for example /root/cache_audit"), "details": schema.Bool("Include workspace and execution provenance (default false)")}), nil, func(ctx context.Context, a tools.Args) (any, error) {
		if err := delegationArgs(a, "path_prefix", "offset", "limit", "details"); err != nil {
			return nil, err
		}
		if value, present := a["details"]; present {
			if _, ok := value.(bool); !ok {
				return nil, fail("invalid_args", "details must be a boolean")
			}
		}
		return r.inspectAgents(ctx, actor, a)
	})
	registerCoordinationTool(registry, "send_message", "Deliver information to a teammate without starting an idle worker. Active workers receive it at the next safe input boundary. The parent uses followup_task to start an idle worker.", schema.Params{"target": schema.S("Member ID, canonical name or relative teammate name"), "message": schema.S("Complete message")}, []string{"target", "message"}, func(ctx context.Context, a tools.Args) (any, error) {
		if err := delegationArgs(a, "target", "message"); err != nil {
			return nil, err
		}
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		id, err := r.resolveTarget(s, actor, a.String("target"))
		if err != nil {
			return nil, err
		}
		// Old Go/API request records remain answerable through the current
		// model interface, oldest first. New messages are informational.
		for _, mail := range inbox(s, actor, false) {
			if mail.From == id && mail.Kind == "request" && mail.ReplyID == "" {
				return r.Send(ctx, actor, id, "reply", mail.ID, a.String("message"))
			}
		}
		return r.Send(ctx, actor, id, "info", "", a.String("message"))
	})
	registerCoordinationTool(registry, "wait_agent", "Wait for addressed messages, directly delegated work or a workflow reaching terminal status. Use this instead of sleeping or polling. A running workflow's internal workers do not wake its parent. Returns a brief update summary; content arrives through durable addressed delivery. Use swarm_read for saved results or decisions. timeout_ms defaults to 30000, minimum 10000, maximum 3600000. A child yields its slot until input or timeout.", schema.Params{"timeout_ms": schema.Int("Wait timeout in milliseconds (10000-3600000, default 30000)")}, nil, func(ctx context.Context, a tools.Args) (any, error) {
		if err := delegationArgs(a, "timeout_ms"); err != nil {
			return nil, err
		}
		n := a.Int("timeout_ms", 30000)
		if n < 10000 || n > 3600000 {
			return nil, errors.New("timeout_ms must be between 10000 and 3600000")
		}
		duration := time.Duration(n) * time.Millisecond
		if actor != r.ID {
			park, ok := ctx.Value(waitKey{}).(func(time.Duration))
			if !ok {
				return nil, errors.New("wait requires an active member execution")
			}
			park(duration)
			return map[string]any{"message": "Waiting for teammate input or timeout; execution resumes at its next input boundary.", "timed_out": false}, nil
		}
		waitCtx, cancel := context.WithTimeout(ctx, duration)
		defer cancel()
		err := r.waitParent(waitCtx)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return map[string]any{"message": "No update before timeout.", "timed_out": true}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"message": "Agent or workflow update available, or no workers remain active. Read addressed delivery and use swarm_read for decisions.", "timed_out": false}, nil
	})
	if actor != r.ID {
		return
	}
	params := schema.Params{
		"task_name":  schema.S("Unique worker name: lowercase letters, digits and underscores; begins with a letter, maximum 64 characters"),
		"message":    schema.S("Complete assignment, including relevant paths, constraints, authorization, validation and expected result"),
		"label":      schema.S("Optional short human-readable purpose for the worker's session title; defaults to task_name"),
		"source":     schema.S("Absolute source checkout root in the parent's repository; seeds the assigned isolated snapshot"),
		"commit":     schema.S("Full Git commit from a retained capture to seed the assigned workspace"),
		"read_only":  schema.Bool("Required: true for research or review without edits; false for editing that requires parent integration"),
		"review":     schema.Bool("Require explicit parent acceptance for research (default false)"),
		"tools":      schema.Strings("Inherit when omitted; [] disables all tools; otherwise select compatible tools"),
		"model":      schema.S("Optional provider/model; inherits the parent by default"),
		"model_host": schema.S("Optional upstream routing identifier"),
	}
	desc := "Delegate a complete assignment with scope, authorization, validation and expected result; private conversations are separate. Choose read_only:true for research or false for editing. Git workers receive isolated copies of current files; use repository-relative paths in briefs. Tasks and result capture are automatic. Returns immediately; wait_agent waits for delivered results. Continue with followup_task; accept editing results through swarm_integrate. Research needs acceptance only with review:true."
	registerCoordinationTool(registry, "spawn_agent", desc, params, []string{"task_name", "message", "read_only"}, func(ctx context.Context, a tools.Args) (any, error) {
		if err := delegationArgs(a, "task_name", "message", "label", "source", "commit", "read_only", "review", "tools", "model", "model_host"); err != nil {
			return nil, err
		}
		readOnly, ok := a["read_only"].(bool)
		if !ok {
			return nil, fail("invalid_args", "read_only is required and must be a boolean: use true for research or false for editing")
		}
		if _, err := requirementFor(a.Bool("review"), readOnly); err != nil {
			return nil, err
		}
		if !taskNamePattern.MatchString(a.String("task_name")) {
			return nil, errors.New("task_name must be 1-64 lowercase letters, digits or underscores, starting with a letter")
		}
		label := a.String("label")
		if label == "" {
			label = strings.ReplaceAll(a.String("task_name"), "_", " ")
		}
		var selected []string
		if _, present := a["tools"]; present {
			selected = a.StringSlice("tools")
			if selected == nil {
				selected = []string{}
			}
		}
		snapshot, err := r.commitArgument(ctx, a)
		if err != nil {
			return nil, err
		}
		i, err := r.start(ctx, "", AgentRequest{TaskName: a.String("task_name"), Label: label, Task: a.String("message"), Source: a.String("source"), Snapshot: snapshot, ReadOnly: readOnly, Review: a.Bool("review"), Tools: selected, Model: a.String("model"), ModelHost: a.String("model_host"), CallID: subagent.CallID(ctx)})
		if err != nil {
			return nil, err
		}
		return map[string]any{"task_name": "/root/" + a.String("task_name"), "member": i.member}, nil
	})
	registerCoordinationTool(registry, "followup_task", "Give an existing worker a correction or more work. Steers active work or starts an idle worker using its saved code. To examine current parent files, use refresh:true after its assignment is done; conversation and authority are preserved. Read the returned baseline before assuming which files it sees. Extra budget requires a client grant; use a new worker for independent review.", schema.Params{"target": schema.S("Worker ID, canonical or relative name"), "message": schema.S("Complete follow-up assignment or correction"), "refresh": schema.Bool("Select current parent code for a finished, settled worker; default false. Live research keeps its saved source.")}, []string{"target", "message"}, func(ctx context.Context, a tools.Args) (any, error) {
		if err := delegationArgs(a, "target", "message", "refresh"); err != nil {
			return nil, err
		}
		if value, present := a["refresh"]; present {
			if _, ok := value.(bool); !ok {
				return nil, fail("invalid_args", "refresh must be a boolean")
			}
		}
		mail, err := r.followupTask(ctx, a.String("target"), a.String("message"), subagent.CallID(ctx), a.Bool("refresh"))
		if err != nil {
			return nil, err
		}
		return r.followupResult(ctx, mail)
	})
	registerCoordinationTool(registry, "interrupt_agent", "Interrupt a worker's current turn, preserving its assignment, evidence and availability. Idle workers are unchanged. Returns the previous execution status. Continue with followup_task; additional budget requires a client grant.", schema.Params{"target": schema.S("Worker ID, canonical or relative name")}, []string{"target"}, func(ctx context.Context, a tools.Args) (any, error) {
		if err := delegationArgs(a, "target"); err != nil {
			return nil, err
		}
		return r.InterruptAgent(ctx, a.String("target"))
	})
}
