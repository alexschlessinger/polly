package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
)

type inlineFileSummary struct{ path, detail string }

func compactToolName(name string) string {
	switch name {
	case "read_file":
		return "read"
	case "write_file":
		return "write"
	case "edit_file":
		return "edit"
	case "list_dir":
		return "list"
	}
	return name
}

func fileSummaryOf(call messages.ChatMessageToolCall) *inlineFileSummary {
	switch call.Name {
	case "read_file", "write_file", "edit_file", "list_dir", "read", "write", "edit":
	default:
		return nil
	}
	var args struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
		Query    string `json:"query"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return nil
	}
	path := args.Path
	if path == "" {
		path = args.FilePath
	}
	if path == "" {
		return nil
	}
	f := &inlineFileSummary{path: path}
	// A read names the file only; the line range it covered is noise in a
	// transcript row.
	if isReadTool(call.Name) && args.Query != "" {
		f.detail = fmt.Sprintf(" (query %q)", rw.Truncate(bashSummaryLine(args.Query), 30, "…"))
	}
	return f
}

func isReadTool(name string) bool { return name == "read_file" || name == "read" }

func relativeToolPath(path, root string) string {
	if filepath.IsAbs(path) && filepath.IsAbs(root) {
		if relative, err := filepath.Rel(root, path); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			path = relative
		}
	}
	return bashSummaryLine(filepath.ToSlash(path))
}

func fitToolPath(path string, width int) string {
	if width <= 0 {
		return ""
	}
	if rw.StringWidth(path) <= width {
		return path
	}
	end := strings.LastIndexByte(strings.TrimRight(path, "/"), '/')
	if end >= 0 {
		leaf := path[end+1:]
		first := strings.IndexByte(path, '/')
		for _, candidate := range []string{path[:first+1] + "…/" + leaf, "…/" + leaf, "…" + leaf} {
			if rw.StringWidth(candidate) <= width {
				return candidate
			}
		}
		return rw.Truncate("…/"+leaf, width, "…")
	}
	return rw.Truncate(path, width, "…")
}

// resolveToolBaseDir is a read-only display lookup. Never infer the owner's
// workspace by stripping a familiar-looking worktree prefix from a tool path.
func resolveToolBaseDir(ctx context.Context, reader sessions.ViewStore, info *sessions.SessionView, m *replModel) {
	if info == nil || info.Metadata == nil || info.Metadata.SwarmID == "" || info.Metadata.ExecutionContext == "" {
		return
	}
	path, err := swarm.ReadMemberWorkspaceView(ctx, reader, info.Metadata.SwarmID, info.ID, info.Metadata.ExecutionContext)
	if err == nil && path != "" && path != m.toolBaseDir {
		m.toolBaseDir = path
		m.visual.invalidate()
	}
}
