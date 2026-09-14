package swarm

import (
	"context"
	"errors"
	"time"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// workflowTool keeps delivery provenance outside model-authored source/output.
// Execute retains the full Go result; ExecuteOutput uses the usual attachments.
type workflowTool struct {
	*tools.Func
	runtime *Runtime
}

type workflowDelivery struct {
	Kind     string `json:"kind"`
	Workflow string `json:"workflow"`
	CallID   string `json:"callID"`
	Status   string `json:"status"`
}

const workflowDeliveryKind = "workflow_result"

func terminalWorkflow(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted" || status == "canceled"
}

func (r *Runtime) registerWorkflowTool(registry *tools.ToolRegistry) {
	t := &workflowTool{runtime: r, Func: &tools.Func{
		Name: "workflow_run", LongRunning: true, Coordinator: true,
		Desc: "Run JavaScript source text (never a file path) with JSON-encoded input, using the swarm scheduler and existing authority. Default foreground waits for the report and delivers its output once in this tool result. background:true returns an ID immediately; then park with wait_agent and act on needs_decision when it finishes. Inspect saved reports through swarm_read view=workflows. Use workflow_help for task dependencies, captured commits and integration repair. Interrupted JavaScript is never automatically replayed.",
		Params: schema.Params{
			"source":     schema.S("JavaScript source defining one polly.defineWorkflow({name,inputSchema,async run(input){...}}). New polly.agent calls require a short label; continuations inherit it. taskID selects a precreated task. Read polly.tasks.read(result.task) for its current revision."),
			"input":      schema.S("JSON-encoded input string"),
			"background": schema.Bool("Return immediately with the workflow ID (default false)"),
		}, Required: []string{"source", "input"},
	}}
	t.Run = func(ctx context.Context, args tools.Args) (string, error) {
		text, _, err := t.run(ctx, args)
		return text, err
	}
	registry.Register(t)
	registry.MarkAlwaysAllowed(t.Name)
}

func (t *workflowTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	text, delivery, err := t.run(ctx, tools.Args(args))
	output := inspectionOutput(t.Name, text)
	if delivery != nil {
		output.Data = *delivery
	}
	return output, err
}

func (t *workflowTool) run(ctx context.Context, args tools.Args) (string, *workflowDelivery, error) {
	input, err := schema.DecodeJSON(args.String("input"))
	if err != nil {
		return "", nil, err
	}
	r := t.runtime
	if args.Bool("background") {
		id, err := r.StartWorkflow(ctx, args.String("source"), input)
		var value any
		if err == nil {
			value = map[string]any{"id": id, "status": "started", "next": "Park with wait_agent; it returns an update summary when this workflow is terminal or mail addresses you. Read the output delivered with its notice and act on needs_decision."}
		}
		text, err := coordinationToolResult(value, err)
		return text, nil, err
	}
	report, runErr := r.RunWorkflow(ctx, args.String("source"), input)
	if report == nil {
		return "", nil, runErr
	}
	// Cancellation must not hide an earned terminal result or its guidance.
	readCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	state, readErr := r.read(readCtx)
	if state == nil {
		state = &State{}
	}
	value := map[string]any{"id": report.ID, "status": report.Status, "output": report.Output, "steps": len(report.Steps), "next": workflowTerminalText(state, report)}
	if report.Error != nil {
		value["error"] = report.Error
	}
	text, encodeErr := coordinationToolResult(value, nil)
	if encodeErr != nil {
		return "", nil, errors.Join(runErr, readErr, encodeErr)
	}
	var delivery *workflowDelivery
	callID := subagent.CallID(ctx)
	if saved := state.Workflows[report.ID]; saved != nil && callID != "" && saved.CallID == callID && saved.Status == report.Status && terminalWorkflow(saved.Status) {
		delivery = &workflowDelivery{Kind: workflowDeliveryKind, Workflow: report.ID, CallID: callID, Status: report.Status}
	}
	return text, delivery, errors.Join(runErr, readErr)
}
