package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// restoreArtifactTool is a mutation, deliberately not a RecallTool. Its reader
// shares read_artifact's conversation/publication authorization, not its paging
// or media projection: restoration always copies the original stored bytes.
type restoreArtifactTool struct {
	tools.NativeTool
	registry *tools.ToolRegistry
	reader   *readArtifactTool
}

func (t *restoreArtifactTool) GetName() string { return BuiltinRestoreArtifact }

func (t *restoreArtifactTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		BuiltinRestoreArtifact,
		"Restore an authorized artifact's exact bytes into a NEW regular file. IDs must be referenced by this conversation or explicitly shared with it; private artifacts remain inaccessible. The destination directory must already exist and allow temporary sibling files. Refuses overwrites, validates size and SHA-256, and preserves no executable permissions. Does not execute the file.",
		schema.Params{
			"id":   schema.S("Artifact ID from this conversation or a shared publication"),
			"path": schema.S("New destination file path, relative to the execution root or absolute"),
		},
		"id", "path",
	)
}

// BindExecutionContext must replace the registry pointer: keeping the source
// registry would retain its filesystem root and write authority in the child.
func (t *restoreArtifactTool) BindExecutionContext(bound *tools.ToolRegistry, _ tools.ExecutionContext) (tools.Tool, error) {
	return &restoreArtifactTool{registry: bound, reader: t.reader}, nil
}

func (t *restoreArtifactTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	args := tools.Args(raw)
	id := strings.TrimSpace(args.String("id"))
	path := strings.TrimSpace(args.String("path"))
	if !artifacts.ValidID(id) {
		return "", tools.NewToolError("invalid artifact ID", "invalid_argument")
	}
	if path == "" {
		return "", tools.NewToolError("path is required", "invalid_argument")
	}
	if err := ctx.Err(); err != nil {
		return "", tools.NewToolError(err.Error(), "artifact_restore_failed")
	}
	ref, source, err := t.reader.openArtifact(ctx, id)
	if err != nil {
		return "", tools.NewToolError(err.Error(), "artifact_unavailable")
	}
	abs, err := tools.RestoreArtifact(ctx, t.registry, path, ref, source)
	if err != nil {
		return "", tools.NewToolError(err.Error(), "artifact_restore_failed")
	}
	return fmt.Sprintf("Restored artifact %s to %s (%d bytes; SHA-256 verified).", id, abs, ref.Bytes), nil
}
