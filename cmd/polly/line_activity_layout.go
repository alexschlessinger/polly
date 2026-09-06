package main

import (
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
)

func activityFieldsWidth(fields []turnDockField) int {
	if len(fields) == 0 {
		return 0
	}
	width := 2 + (len(fields)-1)*3
	for _, f := range fields {
		width += rw.StringWidth(f.raw)
	}
	return width
}

// Use two rows only when necessary. Each row is fitted independently to
// preserve activity, outcome, and elapsed time.
func renderLineActivityRows(activity, status []turnDockField, width int) []string {
	// At very small widths the outcome and elapsed time need both rows by
	// themselves. Drop activity/usage first, then indentation, to keep these
	// two facts whole wherever the terminal can hold their individual labels.
	if len(status) > 0 && status[0].elapsed != "" && activityFieldsWidth(status[:1]) > width {
		primary := status[0]
		var rows []string
		for _, field := range []turnDockField{turnOutcomeField(primary.outcome, ""), mutedField(primary.elapsed, false)} {
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
	if activityFieldsWidth(all) <= width {
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
		status = append(status, turnOutcomeField(a.outcome, formatElapsed(a.elapsed)))
	} else {
		activity = a.liveFields()
		activity[0].protected = true
		elapsed := mutedField(formatElapsed(time.Since(a.started)), false)
		elapsed.protected = true
		status = append(status, elapsed)
	}
	if field, ok := turnTokenField(a.in, a.out); ok {
		status = append(status, field)
	}
	width := unboundedStatusWidth
	if a.caps.live {
		width = max(1, ui.statusColumnsLocked()-1)
	}
	rows := renderLineActivityRows(activity, status, width)
	for i := range rows {
		rows[i] = styledMarkupToLine(rows[i], a.caps.color)
	}
	return rows
}
