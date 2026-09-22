package main

import (
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

func (r *managedREPL) openHelp(lines []string, title string) {
	r.openModal(&replModal{title: title, width: 92, helpLines: lines})
}

// Match complete reference lines before wrapping them so a match keeps its
// whole description, even when the terminal is too narrow for the columns.
func (m *replModal) refreshHelpItems(width int) {
	m.items = nil
	needle := strings.ToLower(strings.TrimSpace(m.input.text()))
	for n, line := range m.helpLines {
		if needle != "" && !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		for row, cells := range style.VisualRows(style.Escape(line), ui.StyleClear, max(1, width-2)) {
			text := ui.CellsToString(cells)
			display := style.Escape(text)
			if strings.TrimSpace(line) == line && line != "" {
				display = style.Styled(text, "", "bold")
			}
			m.items = append(m.items, replModalItem{label: text, display: display, value: strconv.Itoa(n) + ":" + strconv.Itoa(row)})
		}
	}
}

// Help has no selectable actions: arrows move the viewport immediately.
func (m *replModal) scrollHelp(key string) bool {
	top := m.top
	switch key {
	case "<Up>":
		top--
	case "<Down>":
		top++
	case "<MouseWheelUp>":
		top -= 3
	case "<MouseWheelDown>":
		top += 3
	case "<PageUp>":
		top -= max(1, m.visible-1)
	case "<PageDown>":
		top += max(1, m.visible-1)
	case "<Home>":
		top = 0
	case "<End>":
		top = len(m.items) - m.visible
	default:
		return false
	}
	m.top = max(0, min(top, len(m.items)-max(1, m.visible)))
	m.selected = m.top
	return true
}
