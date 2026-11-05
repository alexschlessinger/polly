package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
)

// toolRowChunk is one call as an open activity line shows it: its row, then
// whatever is open under it (the command, the output, the recorded diff),
// with the targets that make those clickable at line offsets within the
// chunk. open says the chunk has rows under its line.
type toolRowChunk struct {
	text    string
	lines   int
	open    bool
	targets []toolOutputTarget
}

// inlineToolRowChunk lays row out at width, with right against the right
// edge of its line. A row's body sits two columns in from the row.
func inlineToolRowChunk(row toolDisclosureRow, width int, root string, recordID int64, right string) toolRowChunk {
	var b strings.Builder
	lineCount := 0
	write := func(text string) {
		b.WriteString(text)
		lineCount += strings.Count(text, "\n")
	}
	line := row.inlineDetailLine(width, root, right)
	targets := []toolOutputTarget{{recordID: recordID, key: row.sectionKey, cols: style.TextWidth(line) - 2}}
	write(line)
	if row.outputExpanded {
		if command := row.inlineCommandText(width); command != "" {
			write("\n" + command)
		}
		if !row.hasInlineEditDiff() {
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
		}
		// The change detail follows its row on the next line.
		if row.changeText != "" {
			write("\n" + row.changeText)
		}
	}
	return toolRowChunk{text: b.String(), lines: lineCount + 1, open: lineCount > 0, targets: targets}
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
// the wrapper repeats all four columns on continuation rows and nothing
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
	// elapsed is the time behind duration: the measured call, or the time
	// a running call has taken so far.
	elapsed time.Duration
	// counts is the plain change summary ("+3 −1") a successful file or
	// command call reported; it renders colored after the label.
	counts string
}

// inlineDurationFloor is the shortest settled call an open activity line
// times: quicker calls say nothing about time, so their rows carry no
// duration. A running call's clock always shows.
const inlineDurationFloor = time.Second

func runningInlineTool(elapsed time.Duration) inlineToolLine {
	return inlineToolLine{glyph: "→", tone: "run", modifier: arrowPulse[int(elapsed/arrowPulsePeriod)%len(arrowPulse)], duration: formatElapsed(elapsed), elapsed: elapsed}
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
	return row.inlineLineLayout(width, root, "", false)
}

// inlineDetailLine is the row as an open activity line shows it: right, when
// given, against the right edge, and a settled duration only from
// inlineDurationFloor up, so the column reads the step's time and the row
// names only a call that took a while.
func (row toolDisclosureRow) inlineDetailLine(width int, root string, right string) string {
	return row.inlineLineLayout(width, root, right, true)
}

func (row toolDisclosureRow) inlineLineLayout(width int, root string, right string, inBody bool) string {
	if width <= 0 || row.inline == nil || row.toolName == "" {
		return row.line
	}
	d := *row.inline
	if inBody && row.settled && d.elapsed < inlineDurationFloor {
		d.duration = ""
	}
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
	// A row in an open line keeps the step column at the right edge, the
	// right text in it, so every row ends at one edge; a row too narrow
	// for the column drops it, then its duration.
	suffix, rightWidth := toolLineBody("", d.meta, d.duration), 0
	if inBody {
		rightWidth = max(stepColumn(width), rw.StringWidth(right)+2)
	} else if right != "" {
		rightWidth = rw.StringWidth(right) + 2
	}
	budget := width - 4 - rw.StringWidth(suffix) - rightWidth - d.countsWidth()
	if budget < 3 && rightWidth > 0 {
		right, rightWidth = "", 0
		budget = width - 4 - rw.StringWidth(suffix) - d.countsWidth()
	}
	if budget < 3 && d.duration != "" {
		d.duration = ""
		suffix = toolLineBody("", d.meta, d.duration)
		budget = width - 4 - rw.StringWidth(suffix) - d.countsWidth()
	}
	finish := func(line string) string {
		if right == "" {
			return line
		}
		return line + strings.Repeat(" ", max(2, width-style.TextWidth(line)-rw.StringWidth(right))) + styledToolText(right)
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
