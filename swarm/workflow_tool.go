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
	"github.com/alexschlessinger/pollytool/workflow"
)

// workflowTool keeps delivery provenance outside model-authored source/output.
// Execute retains the full Go result; ExecuteOutput uses the usual attachments.
type workflowTool struct {
	*tools.Func
	runtime *Runtime
	// registry is where the tool is registered; its read_skill_file tool
	// loads a script named by skill and path.
	registry *tools.ToolRegistry
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
	t := &workflowTool{runtime: r, registry: registry, Func: &tools.Func{
		Name: "workflow_run", LongRunning: true, Coordinator: true,
		Desc: "Run a JavaScript workflow with JSON-encoded input, using the swarm scheduler and existing authority. Name a script that ships with a skill by skill and path, and the host reads the file; never retype one as source, because a copy differs from the file. Otherwise pass your own source text (never a file path). Default foreground waits for the report and delivers its output once in this tool result. background:true returns an ID immediately; continue your own work if you have any, since results reach you at each step, and park with wait_agent when you have nothing else to do. Act on needs_decision when it finishes. Inspect saved reports through swarm_read view=workflows. Use workflow_help for task dependencies, captured commits and integration repair. Interrupted JavaScript is never automatically replayed.",
		Params: schema.Params{
			"skill":      schema.S("Name of a discovered skill whose script to run. Give path too and omit source."),
			"path":       schema.S("Script path inside that skill's directory, as read_skill_file takes it."),
			"source":     schema.S("Omit when skill and path name the script. JavaScript source defining one polly.workflow(name, inputSchema, async run(input){...}) (or the object form polly.defineWorkflow({name,inputSchema,run})). polly.agent(label, task, options?) requires a short label for new agents; continuations inherit it. polly.research forces readOnly. taskID selects a precreated task. Read polly.tasks.get(result.task) for its current revision."),
			"input":      schema.S("JSON-encoded input string"),
			"background": schema.Bool("Return immediately with the workflow ID (default false)"),
		}, Required: []string{"input"},
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

// skillFileReader is the part of read_skill_file the workflow tool needs: the
// file's exact text under the skill catalog and sandbox read policy.
type skillFileReader interface {
	ReadSkillFile(skill, path string) (string, error)
}

// source returns the script to run. A model that copies a skill's script into
// the source argument re-emits every byte and changes some: a live run dropped
// a string operand and a sentence of a prompt from a 14 KB script. Naming the
// file lets the host load it instead; the saved report still holds the text
// that ran.
func (t *workflowTool) source(args tools.Args) (string, error) {
	source, skill, file := args.String("source"), strings.TrimSpace(args.String("skill")), strings.TrimSpace(args.String("path"))
	switch {
	case skill == "" && file == "":
		if strings.TrimSpace(source) == "" {
			return "", fail("invalid_args", "workflow_run needs source, or skill and path")
		}
		// The usual path passed as source names a skill's script.
		var refused *workflow.Error
		if err := workflow.CheckSource(source); errors.As(err, &refused) {
			return "", &workflow.Error{Code: refused.Code, Message: refused.Message + ", or name a skill's script by skill and path"}
		}
		return source, nil
	case strings.TrimSpace(source) != "":
		return "", fail("invalid_args", "pass either source or skill and path, not both")
	case skill == "" || file == "":
		return "", fail("invalid_args", "skill and path name a script together")
	}
	tool, ok := t.registry.Get("read_skill_file")
	if !ok {
		return "", fail("invalid_args", "no skills are available in this session; pass source instead")
	}
	reader, ok := tool.(skillFileReader)
	if !ok {
		return "", fail("invalid_args", "read_skill_file cannot load a script here; pass source instead")
	}
	text, err := reader.ReadSkillFile(skill, file)
	if err != nil {
		return "", fail("invalid_source", fmt.Sprintf("cannot read %s from skill %s: %v", file, skill, err))
	}
	return text, nil
}

func (t *workflowTool) run(ctx context.Context, args tools.Args) (string, *workflowDelivery, error) {
	input, err := schema.DecodeJSON(args.String("input"))
	if err != nil {
		return "", nil, err
	}
	source, err := t.source(args)
	if err != nil {
		return "", nil, err
	}
	r := t.runtime
	if args.Bool("background") {
		id, err := r.StartWorkflow(ctx, source, input)
		var value any
		if err == nil {
			value = map[string]any{"id": id, "status": "started", "next": "Continue your own work if you have any; results reach you at each step. Park with wait_agent when you have nothing else to do. Read the output delivered with its notice and act on needs_decision."}
		}
		text, err := coordinationToolResult(value, err)
		return text, nil, err
	}
	report, runErr := r.RunWorkflow(ctx, source, input)
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
