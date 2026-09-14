package swarm

import (
	"context"
	"errors"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

func (r *Runtime) registerReader(registry *tools.ToolRegistry, actor string) {
	views := []string{"status", "tasks", "messages", "publications"}
	desc := "Requests awaiting your reply, teammates still working, and your remaining iteration allowance."
	sections := "status: decisions or working; tasks: summary (default), details (provenance and captured commits) or result"
	selection := "Select a task or addressed message"
	pointer := "JSON Pointer within selected task content"
	workflowDesc := ""
	params := schema.Params{}
	if actor == r.ID {
		views = append(views, "workflows")
		desc = "What needs your decision and what is still working; counts are totals and next names the first action."
		sections += "; workflows: summary (default), steps, step, source, input or output"
		selection += "; required for workflow inspection"
		pointer = "JSON Pointer within selected task or workflow content"
		workflowDesc = " Parents can also inspect workflows by id."
		params["step"] = schema.S("Stable workflow step ID for section=step")
	}
	params["view"] = schema.Enum("View to read (default status)", views...)
	params["id"] = schema.S(selection)
	params["section"] = schema.S(sections)
	params["pointer"] = schema.S(pointer)
	params["query"] = schema.S("publications: literal, case-insensitive text query")
	desc += " Select view for tasks, messages or publications." + workflowDesc + " Use id and section for saved evidence. Use list_agents to inspect workers. Messages are restricted to your inbox. Page lists using next as offset. Large selections attach full content for read_artifact. Reading never acknowledges delivery, accepts work or changes task state."
	registerCoordinationTool(registry, "swarm_read", desc, inspectionParams(params), nil, func(ctx context.Context, a tools.Args) (any, error) {
		return r.inspect(ctx, actor, a)
	})
}

func (r *Runtime) inspect(ctx context.Context, actor string, a tools.Args) (any, error) {
	switch a.String("view") {
	case "", "status":
		return r.status(ctx, actor, a)
	case "tasks":
		return r.inspectTasks(ctx, a)
	case "messages":
		return r.inspectMessages(ctx, actor, a)
	case "publications":
		return r.inspectPublications(ctx, a)
	case "workflows":
		// Schema visibility alone is not an authority boundary.
		if actor != r.ID {
			return nil, errors.New("workflow inspection requires parent authority")
		}
		if a.String("id") == "" {
			return nil, errors.New("workflow inspection requires an id")
		}
		return r.inspectWorkflow(ctx, a)
	default:
		return nil, errors.New("unknown swarm view")
	}
}

func (r *Runtime) inspectMessages(ctx context.Context, actor string, a tools.Args) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	if id := a.String("id"); id != "" {
		m := s.Messages[id]
		if m == nil || m.To != actor {
			return nil, errors.New("unknown addressed message")
		}
		return m, nil
	}
	items := []any{}
	for _, m := range inbox(s, actor, false) {
		copy := *m
		copy.Text = clipInspection(copy.Text, 512)
		items = append(items, copy)
	}
	return pageInspection(items, a)
}

func (r *Runtime) inspectPublications(ctx context.Context, a tools.Args) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	items := []any{}
	query := strings.ToLower(a.String("query"))
	for _, id := range sortedInspectionIDs(s.Publications) {
		p := s.Publications[id]
		if strings.Contains(strings.ToLower(p.Text), query) {
			items = append(items, PresentPublication(s, p))
		}
	}
	return pageInspection(items, a)
}
