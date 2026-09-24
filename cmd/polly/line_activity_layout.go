package main

import (
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
)

// Use two rows only when necessary. Each row is fitted independently to
// preserve activity, outcome, and elapsed time.
func renderLineActivityRows(activity, status []turnDockField, width int) []string {
	// At very small widths the outcome and elapsed time need both rows by
	// themselves. Drop activity/usage first, then indentation, to keep these
	// two facts whole wherever the terminal can hold their individual labels.
	if len(status) > 0 && status[0].elapsed != "" && turnDockFieldsWidth(status[:1]) > width {
		primary := status[0]
		var rows []string
		for _, field := range []turnDockField{lineOutcomeField(primary.outcome, ""), mutedField(primary.elapsed, false)} {
			if field.raw == "" {
				continue
			}
			rowWidth := width
			withoutIndent := rw.StringWidth(field.raw)+2 > width
			if withoutIndent {
				rowWidth += 2
			}
			row, _ := renderTurnActivityRow([]turnDockField{field}, rowWidth)
			if withoutIndent {
				row = strings.TrimPrefix(row, "  ")
			}
			rows = append(rows, row)
		}
		return rows
	}
	all := append(append([]turnDockField(nil), activity...), status...)
	if len(all) == 0 {
		return nil
	}
	if turnDockFieldsWidth(all) <= width {
		row, _ := renderTurnActivityRow(all, width)
		return []string{row}
	}
	var rows []string
	for _, fields := range [][]turnDockField{activity, status} {
		if len(fields) == 0 {
			continue
		}
		row, _ := renderTurnActivityRow(fields, width)
		if strings.TrimSpace(row) != "" {
			rows = append(rows, row)
		}
	}
	return rows
}

// lineOutcomeField is the trailer's outcome. A successful turn needs no mark
// and opens with its elapsed time alone; the other outcomes keep the TUI's
// words, and a failure keeps its ✗.
func lineOutcomeField(outcome turnOutcome, elapsed string) turnDockField {
	field := turnOutcomeField(outcome, elapsed)
	if outcome == turnOutcomeDone {
		field.raw, field.rendered = elapsed, style.Styled(elapsed, "muted", "")
	}
	return field
}

// activityRowsLocked renders the live row or the settled trailer as plain
// text. The live row is always one line: fields drop before the row wraps.
// The trailer prints once and may take two rows to keep every fact.
func (ui *lineTurnUI) activityRowsLocked(settled bool) []string {
	a := ui.activity
	if a == nil || ui.config.Quiet {
		return nil
	}
	var activity []turnDockField
	var status []turnDockField
	if settled {
		if a.reasoned {
			activity = append(activity, accentField(reasoningDisclosureLabel(false, false, a.thought)))
		}
		activity = append(activity, a.countFields()...)
		status = append(status, lineOutcomeField(a.outcome, formatElapsed(a.elapsed)))
	} else {
		activity = a.liveFields()
		activity[0].protected = true
		elapsed := mutedField(liveElapsed(time.Since(a.started)), false)
		elapsed.protected = true
		status = append(status, elapsed)
	}
	if field, ok := turnTokenField(a.in, a.out, a.estimated); ok {
		status = append(status, field)
	}
	if settled {
		if field, ok := turnCacheField(a.cache); ok {
			status = append(status, field)
		}
	}
	if field, ok := turnCostField(a.cost); ok {
		status = append(status, field)
	}
	width := unboundedStatusWidth
	if a.caps.live {
		width = max(1, ui.statusColumnsLocked()-1)
	}
	var rows []string
	if settled {
		rows = renderLineActivityRows(activity, status, width)
	} else {
		row, _ := renderTurnActivityRow(append(activity, status...), width)
		rows = []string{row}
	}
	for i := range rows {
		rows[i] = ui.statusTextLocked(plainStatusText(rows[i]))
	}
	return rows
}
