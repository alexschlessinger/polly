package tools

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/schema"
)

const (
	// listDirPageEntries and listDirMaxBytes bound one page of directory
	// entries; the byte cap plus the continuation footer stays under
	// PageMaxBytes so a page is never externalized to an artifact.
	listDirPageEntries = 200
	listDirMaxBytes    = 36 << 10
)

// listDirTool lists one directory's entries. Non-recursive by design:
// grep locates content, and wrapped commands handle traversal.
// Listing honors the registry's base sandbox read policy so the tool cannot
// see what a sandboxed command could not.
type listDirTool struct {
	NativeTool
	registry *ToolRegistry
}

// NewListDirTool creates the list_dir tool bound to registry's sandbox
// policy.
func NewListDirTool(registry *ToolRegistry) Tool {
	return &listDirTool{registry: registry}
}

func (t *listDirTool) GetName() string { return "list_dir" }

// BindExecutionContext rebuilds the tool from the bound registry's own
// factory, under the context's root and policy.
func (t *listDirTool) BindExecutionContext(bound *ToolRegistry, _ ExecutionContext) (Tool, error) {
	return rebindNative(bound, t.GetName())
}

func (t *listDirTool) GetSchema() *schema.ToolSchema {
	description := "List a directory's entries (directories first, then files, with sizes). Not recursive."
	return schema.Tool(
		"list_dir",
		description,
		schema.Params{
			"path":   schema.S("Filesystem path of the directory to list"),
			"offset": schema.Int("1-based starting position in the listing (default 1)"),
		},
		"path",
	)
}

func (t *listDirTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	args := Args(raw)
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	offset := args.Int("offset", 1)
	if offset < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	abs, routes, resolved, err := resolveLocalRoutes(t.registry, path)
	if err != nil {
		return "", err
	}
	if err := checkReadPolicy(t.registry, routes...); err != nil {
		return "", err
	}
	// The resolved route is opened without following links, so the
	// directory listed is the one the policy judged even if a concurrent
	// command rewrites a link in between.
	dir, err := safefile.OpenDirectory(resolved)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", abs, err)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return "", fmt.Errorf("list %s: %w", abs, err)
	}
	if len(entries) == 0 {
		return fmt.Sprintf("Directory %s is empty.", abs), nil
	}
	// Directories first, each group alphabetical, so large listings scan
	// well.
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	var dirs, files []os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry)
		} else {
			files = append(files, entry)
		}
	}
	entries = append(dirs, files...)
	if offset > len(entries) {
		return fmt.Sprintf("Directory listing has no entries at or after offset %d (total %d).", offset, len(entries)), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%d entries in %s:\n", len(entries), abs)
	next := 0
	for i := offset - 1; i < len(entries); i++ {
		if i-(offset-1) >= listDirPageEntries {
			next = i + 1
			break
		}
		entry := listDirEntry(entries[i])
		if out.Len()+len(entry) > listDirMaxBytes {
			next = i + 1
			break
		}
		out.WriteString(entry)
	}
	if next > 0 {
		fmt.Fprintf(&out, "[more entries; continue with offset=%d]", next)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func listDirEntry(entry os.DirEntry) string {
	name := entry.Name()
	switch {
	case entry.IsDir():
		return name + "/\n"
	case entry.Type()&os.ModeSymlink != 0:
		return name + "@\n"
	default:
		if info, err := entry.Info(); err == nil {
			return fmt.Sprintf("%s (%d bytes)\n", name, info.Size())
		}
		return name + "\n"
	}
}
