package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
)

func inlineToolDetail(text string, rows []toolDisclosureRow, width int, root string, recordID int64) (string, []toolOutputTarget) {
	var targets []toolOutputTarget
	var b strings.Builder
	lineCount := 0
	write := func(text string) {
		b.WriteString(text)
		lineCount += strings.Count(text, "\n")
	}
	// A row's body sits two columns in from the row; a blank row follows
	// it before the next call.
	open := false
	for _, row := range rows {
		if row.line == "" {
			continue
		}
		at := strings.Index(text, row.line)
		if at < 0 {
			continue
		}
		if open {
			write("\n" + reasoningBlockIndent)
		}
		open = false
		write(text[:at])
		line := row.inlineLineAligned(width, root)
		targets = append(targets, toolOutputTarget{recordID: recordID, key: row.sectionKey, line: lineCount, cols: style.TextWidth(line) - 2})
		write(line)
		if row.outputExpanded {
			if command := row.inlineCommandText(width); command != "" {
				write("\n" + command)
				open = true
			}
		}
		if row.outputExpanded && !row.hasInlineEditDiff() {
			label, body, controls, links := row.inlineOutputSection()
			write("\n" + toolBodyIndent + label)
			if body != "" {
				write("\n" + body)
			}
			if controls != "" {
				write("\n")
				for _, link := range links {
					targets = append(targets, toolOutputTarget{recordID: recordID, key: row.sectionKey, action: link.action, line: lineCount, x: toolBodyCols + link.x, cols: link.cols})
				}
				write(toolBodyIndent + controls)
			}
			open = true
		}
		text = text[at+len(row.line):]
		if row.changeText == "" {
			continue
		}
		// The change detail follows its row on the next line.
		if rest, ok := strings.CutPrefix(text, "\n"+row.changeText); ok {
			write("\n")
			write(row.changeText)
			text = rest
			open = true
		}
	}
	b.WriteString(text)
	return b.String(), targets
}

// inlineCommandText is the command as written, for a Bash row whose label
// lost something: setup folded behind "…", a path shortened to fit, or lines
// beyond the first. A row whose label is the whole command shows nothing
// more; the label already is the call. Other tools never dump their
// arguments: their label names the call, and an edit or write shows its
// recorded diff.
func (row toolDisclosureRow) inlineCommandText(width int) string {
	if row.bash == nil || compactToolName(row.toolName) != "bash" {
		return ""
	}
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(row.arguments), &args) != nil {
		return ""
	}
	command := strings.TrimSpace(args.Command)
	if command == "" || row.bash.fit(width) == command {
		return ""
	}
	text := style.Styled("command", "muted", "") + "\n" + strings.Join(markdown.HighlightCodeLines(cleanInlineToolOutput(command), "bash"), "\n")
	return toolBodyIndent + strings.ReplaceAll(text, "\n", "\n"+toolBodyIndent)
}

// toolBodyIndent is the indent of everything under a tool row: its command,
// output label and body, diff and notices. indentLines swaps the block
// indent for the rail and keeps toolBodyPad, a muted, dim pair of spaces:
// dim is what tells the renderer's pad from muted source indentation, so
// the wrapper repeats all six columns on continuation rows and nothing
// else. toolBodyCols is the pad's width; toolBodyRail is the full prefix
// as laid out.
var (
	toolBodyPad    = style.Styled("  ", "muted", "dim")
	toolBodyIndent = reasoningBlockIndent + toolBodyPad
	toolBodyRail   = style.Rail + toolBodyPad
)

const toolBodyCols = 2

// inlineToolLine retains the parts needed to budget a row at paint time. The
// canonical line still backs the transcript; pane width never changes history.
type inlineToolLine struct {
	glyph, tone, modifier string
	meta, duration        string
	// counts is the plain change summary ("+3 −1") a successful file or
	// command call reported; it renders colored after the label.
	counts string
}

func runningInlineTool(elapsed time.Duration) inlineToolLine {
	return inlineToolLine{glyph: "→", tone: "run", modifier: arrowPulse[int(elapsed/arrowPulsePeriod)%len(arrowPulse)], duration: formatElapsed(elapsed)}
}

func (d inlineToolLine) render(label string) string {
	if d.counts == "" {
		return "  " + style.Styled(d.glyph, d.tone, d.modifier) + " " + styledToolText(toolLineBody(label, d.meta, d.duration))
	}
	return "  " + style.Styled(d.glyph, d.tone, d.modifier) + " " + styledToolText(label) + " " + styledChangeCounts(d.counts) + styledToolText(toolLineBody("", d.meta, d.duration))
}

// countsWidth is the columns the colored counts take after the label.
func (d inlineToolLine) countsWidth() int {
	if d.counts == "" {
		return 0
	}
	return rw.StringWidth(d.counts) + 1
}

func (row *toolDisclosureRow) setLine(d inlineToolLine) {
	row.inline = &d
	row.line = d.render(row.label)
}

func (row toolDisclosureRow) inlineLine(width int) string {
	return row.inlineLineAt(width, "")
}

func (row toolDisclosureRow) inlineLineAt(width int, root string) string {
	return row.inlineLineLayout(width, root, false)
}

// inlineLineAligned is the row with its duration against the right edge, so
// a list of calls reads its timings in one column.
func (row toolDisclosureRow) inlineLineAligned(width int, root string) string {
	return row.inlineLineLayout(width, root, true)
}

func (row toolDisclosureRow) inlineLineLayout(width int, root string, alignDuration bool) string {
	if width <= 0 || row.inline == nil || row.toolName == "" {
		return row.line
	}
	d := *row.inline
	name := bashSummaryLine(compactToolName(row.toolName))
	if name == "" {
		name = "tool"
	}
	subject := strings.TrimPrefix(row.label, row.toolName)
	subject = strings.TrimSpace(subject)
	detail := ""
	if row.bash != nil {
		name, subject = "$", row.bash.fit(width)
	} else if row.file != nil {
		subject, detail = relativeToolPath(row.file.path, root), row.file.detail
	}
	label := strings.TrimSpace(name+" "+subject) + detail
	// Output counts are the first thing to go. Failure/denial information and
	// elapsed time retain their space before the command receives its budget.
	if d.glyph == "✓" && rw.StringWidth(toolLineBody(label, d.meta, d.duration))+d.countsWidth()+4 > width {
		d.meta = ""
		if rw.StringWidth(toolLineBody(label, "", d.duration))+d.countsWidth()+4 > width {
			d.counts = ""
		}
	}
	// An aligned duration leaves the suffix and takes the right edge, at
	// least two columns clear of the rest of the row.
	suffix, right, rightWidth := toolLineBody("", d.meta, d.duration), "", 0
	if alignDuration && d.duration != "" {
		suffix, right, rightWidth = toolLineBody("", d.meta, ""), d.duration, rw.StringWidth(d.duration)+2
	}
	budget := width - 4 - rw.StringWidth(suffix) - rightWidth - d.countsWidth()
	if budget < 3 && d.duration != "" {
		d.duration, right, rightWidth = "", "", 0
		suffix = toolLineBody("", d.meta, d.duration)
		budget = width - 4 - rw.StringWidth(suffix) - d.countsWidth()
	}
	finish := func(line string) string {
		if right == "" {
			return line
		}
		return line + strings.Repeat(" ", max(2, width-style.TextWidth(line)-rw.StringWidth(right))) + styledToolText(right)
	}
	if right != "" {
		d.duration = ""
	}
	if budget < 1 {
		// Tiny panes still show the status. Normal pane sizes retain the whole
		// failure descriptor; clip only when even that cannot physically fit.
		return style.Styled(rw.Truncate(d.glyph+" "+d.meta, max(1, width), "…"), d.tone, d.modifier)
	}
	nameWidth := rw.StringWidth(name)
	if budget <= nameWidth+1 {
		return finish(d.render(rw.Truncate(label, budget, "…")))
	}
	available := budget - nameWidth - 1
	if rw.StringWidth(detail) >= available {
		detail = rw.Truncate(detail, max(0, available/2), "…")
	}
	available -= rw.StringWidth(detail)
	switch {
	case row.bash != nil:
		subject = row.bash.fit(available)
	case row.file != nil:
		subject = fitToolPath(subject, available)
	default:
		subject = rw.Truncate(bashSummaryLine(subject), available, "…")
	}
	line := "  " + style.Styled(d.glyph, d.tone, d.modifier) + " " + styledToolText(name)
	if subject != "" {
		line += " " + style.Styled(style.StripImageMarkers(subject), "", "")
	}
	if d.counts != "" {
		return finish(line + styledToolText(detail) + " " + styledChangeCounts(d.counts) + styledToolText(suffix))
	}
	return finish(line + styledToolText(detail+suffix))
}
