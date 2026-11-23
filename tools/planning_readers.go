package tools

import "context"

// PlanningReaders returns just the native text and directory readers selected
// and allowed on this registry, never a namesake from MCP, a script or a
// ReadOnly-marked plugin. The private turn registry must not inherit any other
// handles (including the always-allowed tools of ordinary derived views).
func (r *ToolRegistry) PlanningReaders() []Tool {
	if r == nil {
		return nil
	}
	var readers []Tool
	for _, name := range []string{"read_file", "list_dir"} {
		tool, ok := r.Get(name)
		if !ok {
			continue
		}
		switch t := tool.(type) {
		case *readFileTool:
			if t.registry != r {
				continue
			}
		case *listDirTool:
			if t.registry != r {
				continue
			}
		default:
			continue
		}
		readers = append(readers, &planningReader{Tool: tool, owner: r})
	}
	return readers
}

type planningReader struct {
	Tool
	owner *ToolRegistry
}

func (t *planningReader) ReadsOnly() bool { return true }
func (t *planningReader) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

func (t *planningReader) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	// Honor live policy changes and the execution gate of the original owner.
	execution, err := t.owner.ExecuteTool(ctx, t.Tool, args, 0)
	return execution.Output, err
}
