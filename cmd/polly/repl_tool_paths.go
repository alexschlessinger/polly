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
	case "zvec_grep_search":
		return "search"
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
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
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
	if call.Name == "read_file" || call.Name == "read" {
		if args.Query != "" {
			f.detail = fmt.Sprintf(" (query %q)", rw.Truncate(bashSummaryLine(args.Query), 30, "…"))
		} else if args.Offset > 0 {
			f.detail = fmt.Sprintf(":%d", args.Offset)
			if args.Limit > 0 {
				f.detail += fmt.Sprintf("–%d", args.Offset+args.Limit-1)
			} else {
				f.detail += "…"
			}
		}
	}
	return f
}

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
	store, ok := reader.(sessions.CoordinationViewStore)
	if !ok {
		return
	}
	state, err := swarm.ReadStateView(ctx, store, info.Metadata.SwarmID)
	if err != nil {
		return
	}
	member := state.Members[info.ID]
	execution := state.Contexts[info.Metadata.ExecutionContext]
	if member != nil && member.Context == info.Metadata.ExecutionContext && execution != nil && execution.Owner == info.ID {
		m.toolBaseDir = execution.Root
		m.visual.invalidate()
	}
}
