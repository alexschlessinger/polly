package swarm

import (
	"context"

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

// integrationOperation is reached only through the trusted parent workflow
// host. Identity is never part of the script contract.
func (r *Runtime) integrationOperation(ctx context.Context, args map[string]any) (any, error) {
	var request integrationRequest
	if err := strictRequest(args, &request); err != nil {
		return nil, err
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

func (r *Runtime) integrateOperation(ctx context.Context, args map[string]any) (any, error) {
	var request IntegrateRequest
	if err := strictRequest(args, &request); err != nil {
		return nil, err
	}
	return r.Integrate(ctx, request)
}

func (r *Runtime) registerIntegrationTool(registry *tools.ToolRegistry) {
	reference := map[string]any{"type": "object", "properties": schema.Params{"task": schema.S("Task ID"), "revision": schema.Int("Expected revision")}, "required": []string{"task", "revision"}, "additionalProperties": false}
	registry.Register(&tools.Func{Name: "swarm_integrate", Coordinator: true, LongRunning: true,
		Desc:   "Accept and integrate exact editing task revisions in one call. Name tasks to prepare and apply them, or a ready candidate to finish a repair or refresh. Unchanged tasks finish without an apply. Conflicts retain one candidate with repair guidance. Research completes on delivery or explicit review. Drift belongs only to tasks: paths (default) or tree.",
		Params: schema.Params{"tasks": schema.Array("Exact task revisions to integrate together", reference), "candidate": schema.S("Existing candidate ID; omit tasks and drift"), "drift": schema.S("paths (default) or tree; only with tasks")},
		Run: func(ctx context.Context, a tools.Args) (string, error) {
			v, err := r.integrateOperation(ctx, a)
			v, err = r.publicResult(ctx, v, err)
			return tools.Result(v), err
		},
	})
	registry.MarkAlwaysAllowed("swarm_integrate")
}
