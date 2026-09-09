package swarm

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

type integrationRequest struct {
	Op     string          `json:"op"`
	ID     string          `json:"id,omitempty"`
	Tasks  []TaskReference `json:"tasks,omitempty"`
	Repair TaskReference   `json:"repair,omitempty"`
	Drift  string          `json:"drift,omitempty"`
}

// integrationOperation is reached only through a parent-bound tool or the
// trusted parent workflow host. Identity is never part of the script contract.
func (r *Runtime) integrationOperation(ctx context.Context, args map[string]any) (any, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var request integrationRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil {
		return nil, fail("invalid_args", err.Error())
	}
	switch request.Op {
	case "prepare":
		return r.PrepareIntegration(ctx, request.Tasks, request.Drift)
	case "read":
		return r.ReadIntegration(ctx, request.ID)
	case "revise":
		return r.ReviseIntegration(ctx, request.ID, request.Repair)
	case "refresh":
		return r.RefreshIntegration(ctx, request.ID)
	case "accept":
		return r.AcceptIntegration(ctx, request.ID)
	case "apply":
		return r.ApplyIntegration(ctx, request.ID)
	case "reconcile":
		return r.ReconcileApply(ctx, request.ID)
	default:
		return nil, fail("invalid_args", "unknown integration operation")
	}
}

func (r *Runtime) registerIntegrationTool(registry *tools.ToolRegistry) {
	reference := map[string]any{"type": "object", "properties": schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Expected revision")}, "required": []string{"task", "revision"}, "additionalProperties": false}
	registry.Register(&tools.Func{Name: "swarm_integration", Coordinator: true, LongRunning: true,
		Desc:   "Parent-authorized integration: prepare ordered task revisions, read conflicts and receipts, revise from an exact repair snapshot, refresh, accept, apply, or reconcile an uncertain write. Default drift checks touched paths; tree requires full parent equality. Preparation allocates no checkout.",
		Params: schema.Params{"op": schema.S("prepare, read, revise, refresh, accept, apply, reconcile"), "id": schema.S("Candidate ID"), "tasks": schema.Array("Ordered task revisions", reference), "repair": reference, "drift": schema.S("paths (default) or tree")}, Required: []string{"op"},
		Run: func(ctx context.Context, a tools.Args) (string, error) {
			v, err := r.integrationOperation(ctx, a)
			return tools.Result(v), err
		},
	})
	registry.MarkAlwaysAllowed("swarm_integration")
}
