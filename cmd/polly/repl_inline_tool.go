package main

import (
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
)

func inlineToolDetail(text string, rows []toolDisclosureRow, width int, root string) string {
	var b strings.Builder
	for _, row := range rows {
		if row.line == "" {
			continue
		}
		at := strings.Index(text, row.line)
		if at < 0 {
			continue
		}
		b.WriteString(text[:at])
		b.WriteString(row.inlineLineAt(width, root))
		text = text[at+len(row.line):]
	}
	b.WriteString(text)
	return b.String()
}

// inlineToolLine retains the parts needed to budget a row at paint time. The
// canonical line still backs the transcript; pane width never changes history.
type inlineToolLine struct {
	glyph, tone, modifier string
	meta, duration        string
}

func runningInlineTool(elapsed time.Duration) inlineToolLine {
	return inlineToolLine{glyph: "→", tone: "run", modifier: arrowPulse[int(elapsed/arrowPulsePeriod)%len(arrowPulse)], duration: formatElapsed(elapsed)}
}

func (d inlineToolLine) render(label string) string {
	return "  " + style.Styled(d.glyph, d.tone, d.modifier) + " " + styledToolText(toolLineBody(label, d.meta, d.duration))
}

func (row *toolDisclosureRow) setLine(d inlineToolLine) {
	row.inline = &d
	row.line = d.render(row.label)
}

func (row toolDisclosureRow) inlineLine(width int) string {
	return row.inlineLineAt(width, "")
}

func (row toolDisclosureRow) inlineLineAt(width int, root string) string {
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
	if d.glyph == "✓" && rw.StringWidth(toolLineBody(label, d.meta, d.duration))+4 > width {
		d.meta = ""
	}
	suffix := toolLineBody("", d.meta, d.duration)
	budget := width - 4 - rw.StringWidth(suffix)
	if budget < 3 && d.duration != "" {
		d.duration = ""
		suffix = toolLineBody("", d.meta, d.duration)
		budget = width - 4 - rw.StringWidth(suffix)
	}
	if budget < 1 {
		// Tiny panes still show the status. Normal pane sizes retain the whole
		// failure descriptor; clip only when even that cannot physically fit.
		return style.Styled(rw.Truncate(d.glyph+" "+d.meta, max(1, width), "…"), d.tone, d.modifier)
	}
	nameWidth := rw.StringWidth(name)
	if budget <= nameWidth+1 {
		return d.render(rw.Truncate(label, budget, "…"))
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
	return line + styledToolText(detail+suffix)
}
