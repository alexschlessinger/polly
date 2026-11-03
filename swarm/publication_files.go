package swarm

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/tools"
)

const (
	publicationMaxFiles       = 16
	publicationFileBytes      = 16 << 20
	publicationTotalFileBytes = 64 << 20
)

// capturePublicationFiles stores selected files privately in the caller's
// session. Publish pins all refs with the publication in one transaction; a
// failed capture or publication never makes a partial handoff visible.
func capturePublicationFiles(ctx context.Context, registry *tools.ToolRegistry, store artifacts.Store, args tools.Args) ([]artifacts.Ref, error) {
	refs := []artifacts.Ref{}
	raw, present := args["files"]
	if !present {
		return refs, nil
	}
	var paths []string
	switch value := raw.(type) {
	case []string:
		paths = value
	case []any:
		for _, item := range value {
			path, ok := item.(string)
			if !ok {
				return nil, tools.NewToolError("files must be an array of file paths", "invalid_args")
			}
			paths = append(paths, path)
		}
	default:
		return nil, tools.NewToolError("files must be an array of file paths", "invalid_args")
	}
	if len(paths) > publicationMaxFiles {
		return nil, tools.NewToolError(fmt.Sprintf("files accepts at most %d paths", publicationMaxFiles), "invalid_args")
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return nil, tools.NewToolError("files paths must not be blank", "invalid_args")
		}
	}
	if len(paths) > 0 && store == nil {
		return nil, tools.NewToolError("file capture requires an artifact store", "artifact_store_unavailable")
	}
	remaining := int64(publicationTotalFileBytes)
	for _, path := range paths {
		blob, err := tools.ReadArtifactFile(ctx, registry, path, min(int64(publicationFileBytes), remaining))
		if err != nil {
			return nil, err
		}
		ref, err := store.Put(ctx, blob)
		if err != nil {
			return nil, fmt.Errorf("store captured file %q: %w", path, err)
		}
		remaining -= int64(len(blob.Data))
		refs = append(refs, ref)
	}
	return refs, nil
}
