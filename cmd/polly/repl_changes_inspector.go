package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// Changes inspector: every tracked file change this session's tool results
// reported, oldest first, each a folded row that opens to its full diff. The
// status row's counts come from the same tools, so the two always agree.

// openChangesInspector shows the visible session's diffs. A re-open starts at
// the summary rather than resuming a scroll position: the list is a settled
// report, not a live feed.
func (r *managedREPL) openChangesInspector() {
	target := tabViewTarget(r.visibleTab())
	target.kind = changesViewKind
	r.inspect(target)
	r.workspace().viewState(target).follow = false
	r.workspace().inspector.searching = false
}

// Items are immutable projections; a collapsed change never renders its diff.
type changesInspectorItem struct {
	key      string
	title    string
	body     string
	expanded bool
}

type changesInspectorList struct {
	summary string
	items   []changesInspectorItem
}

func changesInspectorBlock(key, section string) string { return "change-list/" + section + "/" + key }

// trackedChangeKeys names every change the list shows, in list order: the
// reporting call's key plus the change's place in that call's result.
func trackedChangeKeys(tools []inspectedTool) []string {
	var keys []string
	for _, tool := range tools {
		if changes := tool.pres.changes; changes != nil && changes.tracked {
			for n := range changes.changes {
				keys = append(keys, fmt.Sprintf("%s#%d", tool.key, n))
			}
		}
	}
	return keys
}

func (changesView) Project(ctx context.Context, source viewSource, state viewState) (*replModel, error) {
	m := newReplModel()
	m.inspectorWrap = true
	if source.info != nil {
		m.artifactStore = source.info.Artifacts
	}
	var tools []inspectedTool
	if source.model != nil {
		tools = source.model.inspections.tools
		// The header counts files from the same body-free records.
		m.inspections = source.model.inspections.navigation()
	}
	additions, deletions, files := sessionChangeStats(tools)
	list := &changesInspectorList{summary: changesSummaryLine(additions, deletions, files)}
	for _, tool := range tools {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		changes := tool.pres.changes
		if changes == nil || !changes.tracked {
			continue
		}
		for n, change := range changes.changes {
			// A body-less change (binary, oversized) still gets its titled row
			// so the list covers every file the counts include.
			item := changesInspectorItem{key: fmt.Sprintf("%s#%d", tool.key, n), title: changeItemTitle(change)}
			item.expanded = state.changeItemExpanded(item.key)
			if item.expanded && change.diff != "" {
				lines := renderDiffLines(change.diff, 0, inspectorDiffLines, change.truncated)
				item.body = strings.Join(markdown.RenderFence("diff", lines), "\n")
			}
			list.items = append(list.items, item)
		}
	}
	m.changesInspector = list
	return m, nil
}

func (list *changesInspectorList) blocks() []transcriptDisplayBlock {
	blocks := []transcriptDisplayBlock{{key: "changes-summary", text: list.summary}}
	for n, item := range list.items {
		if n > 0 && list.items[n-1].expanded && list.items[n-1].body != "" {
			blocks = append(blocks, transcriptDisplayBlock{key: changesInspectorBlock(item.key, "gap")})
		}
		glyph := "▸"
		if item.expanded {
			glyph = "▾"
		}
		blocks = append(blocks, transcriptDisplayBlock{key: changesInspectorBlock(item.key, "title"), text: style.Styled(glyph, "accent", "bold") + " " + item.title})
		if item.expanded && item.body != "" {
			blocks = append(blocks, transcriptDisplayBlock{key: changesInspectorBlock(item.key, "body"), text: item.body})
		}
	}
	return blocks
}

// changeItemTitle is one change's disclosure row: the path, what happened to
// the file when it is not a plain modification, and the styled line counts.
func changeItemTitle(change fileChange) string {
	title := change.path
	switch {
	case change.binary:
		title += style.Styled(" binary", "muted", "")
	case change.kind == "created":
		title += style.Styled(" new", "muted", "")
	case change.kind == "deleted":
		title += style.Styled(" deleted", "muted", "")
	}
	if _, counts := changeTotalsText(change.additions, change.deletions); counts != "" {
		title += " " + counts
	}
	return title
}

// toggleChangesInspectorItems is Ctrl-O in the changes list: every diff opens,
// or every diff closes when nothing is left closed. The decision is sticky
// like the tools list's: while it opens, changes that arrive later open too.
// Caller must hold r.model.mu.
func (r *managedREPL) toggleChangesInspectorItems() {
	i := &r.workspace().inspector
	if i.current == nil || i.current.model == nil {
		return
	}
	keys := trackedChangeKeys(i.current.model.inspections.tools)
	if len(keys) == 0 {
		return
	}
	s := r.workspace().viewState(i.target)
	expand := false
	for _, key := range keys {
		if !s.changeItemExpanded(key) {
			expand = true
			break
		}
	}
	s.changeExpanded = make(map[string]bool, len(keys))
	for _, key := range keys {
		s.changeExpanded[key] = expand
	}
	s.expandAll = expand
	relayoutToolList(i.current.model, s)
}

func (r *managedREPL) changesInspectorAction(action string) bool {
	rest, ok := strings.CutPrefix(action, "change-list/")
	if !ok {
		return false
	}
	section, key, ok := strings.Cut(rest, "/")
	i := &r.workspace().inspector
	if !ok || section != "title" || i.target.kind != changesViewKind || i.current == nil || i.current.model == nil {
		return true
	}
	s := r.workspace().viewState(i.target)
	if s.changeExpanded == nil {
		s.changeExpanded = make(map[string]bool)
	}
	s.changeExpanded[key] = !s.changeItemExpanded(key)
	relayoutToolList(i.current.model, s)
	return true
}

// changesSummaryLine heads the list with the session's totals: the styled
// line counts, then how many distinct files they cover.
func changesSummaryLine(additions, deletions, files int) string {
	if files == 0 {
		return style.Styled("No file changes in this session", "muted", "")
	}
	word := "files"
	if files == 1 {
		word = "file"
	}
	tail := style.Styled(fmt.Sprintf("%d %s", files, word), "muted", "")
	counts, styled := changeTotalsText(additions, deletions)
	if counts == "" {
		return tail
	}
	return styled + style.Styled(" · ", "muted", "") + tail
}
