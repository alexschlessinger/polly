package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

// File changes: what edit_file, write_file and bash report having changed,
// decoded from the tool result's durable metadata and rendered as counts on
// the tool row, a bounded hunk under it, and full diffs in the inspector.

// inlineDiffLines bounds the hunk shown under an expanded tool row.
const inlineDiffLines = 12

// inlineChangeFiles bounds the per-file rows under a multi-file change.
const inlineChangeFiles = 5

// inspectorDiffLines bounds each file's diff in the tool inspector.
const inspectorDiffLines = 400

type fileChange struct {
	path, kind           string
	additions, deletions int
	diff                 string
	truncated, binary    bool
}

type fileChanges struct {
	root      string
	changes   []fileChange
	tracked   bool
	reason    string
	truncated bool
	omitted   int
}

// fileChangesFromResult decodes the FileChanges a tool stored as tool_data:
// the whole payload for edit_file and write_file, the "changes" member of a
// bash CommandResult. Values arrive JSON-decoded, so every field is read
// generically; anything else yields nil.
func fileChangesFromResult(msg messages.ChatMessage) *fileChanges {
	data, ok := msg.Metadata["tool_data"].(map[string]any)
	if !ok {
		return nil
	}
	if nested, ok := data["changes"].(map[string]any); ok {
		return decodeFileChanges(nested)
	}
	if _, ok := data["tracked"]; ok {
		return decodeFileChanges(data)
	}
	return nil
}

// exitCodeFromResult is the exit code a command stored in its result, so a
// hydrated failure can name it without the live error chain.
func exitCodeFromResult(msg messages.ChatMessage) int {
	data, ok := msg.Metadata["tool_data"].(map[string]any)
	if !ok {
		return 0
	}
	return jsonInt(data["exitCode"])
}

func decodeFileChanges(data map[string]any) *fileChanges {
	c := &fileChanges{}
	c.root, _ = data["root"].(string)
	c.tracked, _ = data["tracked"].(bool)
	c.reason, _ = data["reason"].(string)
	c.truncated, _ = data["truncated"].(bool)
	c.omitted = jsonInt(data["omitted"])
	if list, ok := data["changes"].([]any); ok {
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			change := fileChange{additions: jsonInt(entry["additions"]), deletions: jsonInt(entry["deletions"])}
			change.path, _ = entry["path"].(string)
			change.kind, _ = entry["kind"].(string)
			change.diff, _ = entry["diff"].(string)
			change.truncated, _ = entry["truncated"].(bool)
			change.binary, _ = entry["binary"].(bool)
			c.changes = append(c.changes, change)
		}
	}
	return c
}

func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func (c *fileChanges) totals() (adds, dels int) {
	if c == nil {
		return 0, 0
	}
	for _, change := range c.changes {
		adds += change.additions
		dels += change.deletions
	}
	return adds, dels
}

// single returns the one changed file, or nil for none or several.
func (c *fileChanges) single() *fileChange {
	if c == nil || len(c.changes) != 1 {
		return nil
	}
	return &c.changes[0]
}

// countText is the plain change summary for a tool row: "+3 −1", "new +12",
// "deleted −4", "binary", or "" when nothing is known. Line mode prints it
// as is; the TUI colors it with styledChangeCounts.
func (c *fileChanges) countText() string {
	if c == nil || !c.tracked {
		return ""
	}
	var parts []string
	if one := c.single(); one != nil {
		switch {
		case one.binary:
			parts = append(parts, "binary")
		case one.kind == "created":
			parts = append(parts, "new")
		case one.kind == "deleted":
			parts = append(parts, "deleted")
		}
	}
	adds, dels := c.totals()
	if adds > 0 {
		parts = append(parts, "+"+strconv.Itoa(adds))
	}
	if dels > 0 {
		parts = append(parts, "−"+strconv.Itoa(dels))
	}
	return strings.Join(parts, " ")
}

// styledChangeCounts colors a countText: additions green, deletions red,
// the rest muted.
func styledChangeCounts(counts string) string {
	if counts == "" {
		return ""
	}
	words := strings.Fields(counts)
	for i, word := range words {
		switch {
		case strings.HasPrefix(word, "+"):
			words[i] = style.Styled(word, "ok", "")
		case strings.HasPrefix(word, "−"):
			words[i] = style.Styled(word, "err", "")
		default:
			words[i] = style.Styled(word, "muted", "")
		}
	}
	return strings.Join(words, " ")
}

// renderDiffLines styles a unified diff body line by line: additions green,
// deletions red, hunk headers and the no-newline marker muted, context in the
// code color. The two file headers are dropped, since the row already names
// the file. Lines are tab-expanded and, for a positive width, truncated to
// it. Output stops after maxLines with a muted tail, which also reports a
// body the tool itself had truncated.
func renderDiffLines(diff string, width, maxLines int, truncated bool) []string {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if len(lines) >= 2 && strings.HasPrefix(lines[0], "--- ") && strings.HasPrefix(lines[1], "+++ ") {
		lines = lines[2:]
	}
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	out := make([]string, 0, min(len(lines), maxLines)+1)
	for i, line := range lines {
		if maxLines > 0 && i == maxLines {
			out = append(out, style.Styled(fmt.Sprintf("… %d more lines", len(lines)-i), "muted", ""))
			break
		}
		text := markdown.ExpandCodeTabs(strings.TrimRight(style.StripImageMarkers(line), "\r"))
		if width > 0 {
			text = rw.Truncate(text, width, "…")
		}
		tone := "code"
		switch {
		case strings.HasPrefix(line, "+"):
			tone = "ok"
		case strings.HasPrefix(line, "-"):
			tone = "err"
		case strings.HasPrefix(line, "@@"), strings.HasPrefix(line, "\\"):
			tone = "muted"
		}
		out = append(out, style.Styled(text, tone, ""))
	}
	if truncated {
		out = append(out, style.Styled("… truncated", "muted", ""))
	}
	return out
}

// changeDetailLines is the block under an expanded, settled tool row: the
// bounded hunk when exactly one file changed, else one row per changed file
// with its counts. Each line carries the block indent that railLines
// replaces with the rail. width bounds the lines when positive.
func (row toolDisclosureRow) changeDetailLines(width int) []string {
	c := row.pres.changes
	if c == nil || !c.tracked || len(c.changes) == 0 {
		return nil
	}
	var lines []string
	if one := c.single(); one != nil {
		if one.diff == "" {
			return nil
		}
		for _, line := range renderDiffLines(one.diff, width, inlineDiffLines, one.truncated) {
			lines = append(lines, reasoningBlockIndent+line)
		}
		return lines
	}
	for i, change := range c.changes {
		if i == inlineChangeFiles {
			break
		}
		counts := (&fileChanges{tracked: true, changes: []fileChange{change}}).countText()
		path := change.path
		if width > 0 {
			path = fitToolPath(path, max(1, width-rw.StringWidth(counts)-1))
		}
		line := style.Styled(path, "muted", "")
		if counts != "" {
			line += " " + styledChangeCounts(counts)
		}
		lines = append(lines, reasoningBlockIndent+line)
	}
	if more := len(c.changes) - inlineChangeFiles + c.omitted; more > 0 {
		lines = append(lines, reasoningBlockIndent+style.Styled(fmt.Sprintf("… %d more files", more), "muted", ""))
	}
	return lines
}

func (row toolDisclosureRow) changeDetail(width int) string {
	return strings.Join(row.changeDetailLines(width), "\n")
}

// setPresentation settles a row on its result: the line, its counts, and
// the canonical change block recorded so paint-time substitution can find it.
func (row *toolDisclosureRow) setPresentation(p toolPresentation) {
	row.pres = p
	row.setLine(p.inline())
	row.changeText = row.changeDetail(0)
}

// inspectorChangeMeta is the label suffix for the inspector's output section.
func (c *fileChanges) inspectorTitle(change fileChange) string {
	counts := (&fileChanges{tracked: true, changes: []fileChange{change}}).countText()
	title := "diff · " + change.path
	if counts != "" {
		title += " " + counts
	}
	return title
}

// noteUntrackedCommandChanges tells the user once per session that bash
// commands in this workspace are not observed for file changes.
func (m *replModel) noteUntrackedCommandChanges(reason string) {
	if m.commandChangesNoticeShown {
		return
	}
	m.commandChangesNoticeShown = true
	m.appendNoticeLine(untrackedCommandNotice(reason))
}
