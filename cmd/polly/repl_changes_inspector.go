package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// Changes inspector: every tracked file change this session's tool results
// reported, oldest first, one folded row per file that opens to the file's
// full diff. The status row's counts come from the same tools, so the two
// always agree.

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

// trackedChangeKeys names every row the list shows, in list order: one key per
// distinct file, since a row folds every change to the same path.
func trackedChangeKeys(tools []inspectedTool) []string {
	changes := sessionChanges(tools)
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		keys = append(keys, change.path)
	}
	return keys
}

// sessionChange is every change to one path folded into the single row the
// list shows: counts add up, the diffs follow one another in report order, and
// the kind describes the file across the session. The path is the row's key,
// so a file keeps its row, its diff and its open/closed state however many
// calls touch it.
type sessionChange struct {
	path                 string
	kind                 string
	additions, deletions int
	truncated, binary    bool
	// diffs are the path's diffs in the order the tools reported them. They
	// stay unparsed until the row opens, so a folded list never renders a diff.
	diffs []string
}

// bodyLines is an opened row's body: every diff the row carries, joined under
// the inspector's line limit, since one file's changes read as one diff.
func (c sessionChange) bodyLines() []string {
	var lines []string
	for _, diff := range c.diffs {
		lines = append(lines, diffBodyLines(diff)...)
	}
	return renderDiffBodyLines(lines, 0, inspectorDiffLines, c.truncated)
}

// sessionChanges folds the tracked changes the tools reported into one entry
// per path, in first-appearance order.
func sessionChanges(tools []inspectedTool) []sessionChange {
	var out []sessionChange
	index := make(map[string]int)
	for _, tool := range tools {
		changes := tool.pres.changes
		if changes == nil || !changes.tracked {
			continue
		}
		for _, change := range changes.changes {
			n, seen := index[change.path]
			if !seen {
				n = len(out)
				index[change.path] = n
				out = append(out, sessionChange{path: change.path, kind: change.kind})
			}
			row := &out[n]
			if seen {
				row.kind = foldedChangeKind(row.kind, change.kind)
			}
			row.additions += change.additions
			row.deletions += change.deletions
			row.truncated = row.truncated || change.truncated
			row.binary = row.binary || change.binary
			if change.diff != "" {
				row.diffs = append(row.diffs, change.diff)
			}
		}
	}
	// A row the session turned binary carries no body: there is nothing
	// readable to show, and the title says so.
	for n := range out {
		if out[n].binary {
			out[n].diffs = nil
		}
	}
	return out
}

// foldedChangeKind folds one more change to a path into the kind its row
// reports: a file whose latest change removed it reads as deleted, a file the
// session created stays new while it is only written to afterwards, and
// anything else is a plain modification. The row's first change decides the
// rest, so a file that existed at the session's start never reads as new.
func foldedChangeKind(kind, next string) string {
	switch {
	case next == "deleted":
		return "deleted"
	case kind == "created":
		return "created"
	}
	return "modified"
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
	for _, change := range sessionChanges(tools) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A body-less row (binary, oversized) still gets its titled entry so
		// the list covers every file the counts include.
		item := changesInspectorItem{key: change.path, title: changeItemTitle(change)}
		item.expanded = state.changeItemExpanded(item.key)
		if item.expanded {
			if lines := change.bodyLines(); len(lines) > 0 {
				item.body = strings.Join(markdown.RenderFence("diff", lines), "\n")
			}
		}
		list.items = append(list.items, item)
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

// changeItemTitle is one file's disclosure row: the path, what happened to the
// file across the session when it is not a plain modification, and the styled
// line counts of every change folded into the row.
func changeItemTitle(change sessionChange) string {
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
