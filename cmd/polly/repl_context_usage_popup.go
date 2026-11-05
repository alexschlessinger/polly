package main

import (
	"fmt"
	"image"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

const (
	contextUsageAction = "context-usage"
	contextBackAction  = "context-back"
)

// Both pages use physical rows for scrolling and hit testing. Wrap diagnostics
// rather than clipping their values on narrow terminals.
func contextPopoverRows(text string, width int) []replModalItem {
	plain := ui.CellsToString(style.ParseCells(text, ui.StyleClear))
	if rw.StringWidth(plain) <= width {
		return []replModalItem{{label: " " + plain, display: " " + text}}
	}
	var items []replModalItem
	for _, row := range style.VisualRows(style.Escape(plain), ui.StyleClear, width) {
		line := ui.CellsToString(row)
		items = append(items, replModalItem{label: " " + line, display: " " + style.Styled(line, "muted", "")})
	}
	return items
}

func contextUsageBar(label string, count contextTokenTotal, total, width, countWidth int) string {
	value := count.text()
	left := fmt.Sprintf("%-7s", label)
	barWidth := width - rw.StringWidth(left) - countWidth - 1
	if barWidth < 1 {
		return style.Styled(label+" "+value, "muted", "")
	}
	fraction := float64(count.tokens) / float64(max(1, total))
	bar := contextBarStyle(contextBar(fraction, barWidth), "accent")
	return style.Styled(left, "muted", "") + bar + strings.Repeat(" ", countWidth-rw.StringWidth(value)+1) + style.Styled(value, "", "")
}

func (d *contextPopover) usageItems(modalWidth int) []replModalItem {
	var items []replModalItem
	width := max(1, modalWidth-4)
	add := func(text, key string) {
		rows := contextPopoverRows(text, width)
		for i := range rows {
			rows[i].identity = fmt.Sprintf("%s:%d", key, i)
			if key == contextBackAction {
				rows[i].value = contextBackAction
			}
		}
		items = append(items, rows...)
	}
	muted := func(text string) string { return style.Styled(text, "muted", "") }
	add(style.Styled("‹ Context", "accent", ""), contextBackAction)
	add("", "heading-space")
	usage := d.snapshot.usage
	if d.err != nil {
		add(muted("Stats unavailable: "+d.err.Error()), "error")
	} else if usage.err != nil {
		add(muted("Subagent usage unavailable: "+usage.err.Error()), "error")
	} else if len(usage.agents) == 0 {
		add(muted("No subagents yet"), "empty")
	} else {
		// Align the values and compare each agent's share of recorded input and
		// output independently; an output bar is not dwarfed by input volume.
		countWidth := 1
		for _, agent := range usage.agents {
			countWidth = max(countWidth, rw.StringWidth(agent.usage.input.text()), rw.StringWidth(agent.usage.output.text()))
		}
		for i, agent := range usage.agents {
			key := "agent:" + agent.id
			if i > 0 {
				add("", key+":space")
			}
			add(style.Styled(agent.label, "", "bold"), key)
			add(contextUsageBar("↑ in", agent.usage.input, usage.subagents.input.tokens, width, countWidth), key+":input")
			add(contextUsageBar("↓ out", agent.usage.output, usage.subagents.output.tokens, width, countWidth), key+":output")
		}
	}
	return items
}

// Retain the selected row's position when a refreshed snapshot inserts another
// worker above it. Identities are stable even though labels and counts change.
func (d *contextPopover) refreshItems(m *replModal, width int) {
	identity := ""
	if m.selected >= 0 && m.selected < len(m.items) {
		identity = m.items[m.selected].identity
	}
	m.items = d.items(width)
	if d.usageOpen {
		d.updateDetails(m)
	}
	if identity != "" {
		for i, item := range m.items {
			if item.identity == identity {
				m.top = max(0, m.top+i-m.selected)
				m.selected = i
				break
			}
		}
	}
}

func (d *contextPopover) activate(m *replModal, action string) bool {
	switch {
	case action == contextUsageAction && !d.usageOpen:
		d.contextTop, d.contextSelected = m.top, m.selected
		d.contextIdentity = ""
		if m.selected >= 0 && m.selected < len(m.items) {
			d.contextIdentity = m.items[m.selected].identity
		}
		d.usageOpen = true
		m.title, m.top, m.selected = "Subagent usage", 0, 0
	case action == contextBackAction && d.usageOpen:
		d.usageOpen = false
		m.title = "Context"
		m.top, m.selected = d.contextTop, d.contextSelected
	default:
		return false
	}
	// Reuse the live reader rather than closing/reopening a modal (which
	// cancels it). Install rows now so a second key before paint sees this page.
	width, _ := ui.TerminalDimensions()
	m.items = d.items(modalWidthForTerminal(width, m.width))
	if !d.usageOpen {
		// The overview may have gained chart rows or wrapped while hidden.
		// Restore the action at its prior viewport offset, falling back to its
		// first row if a selected wrapped continuation no longer exists.
		selected := -1
		for i, item := range m.items {
			if item.value != contextUsageAction {
				continue
			}
			if selected < 0 {
				selected = i
			}
			if item.identity == d.contextIdentity {
				selected = i
				break
			}
		}
		if selected >= 0 {
			m.selected = selected
			m.top = max(0, selected-(d.contextSelected-d.contextTop))
		}
	}
	d.updateDetails(m)
	return true
}

func (d *contextPopover) handleEvent(m *replModal, e ui.Event) bool {
	if e.Type == ui.MouseEvent {
		mouse, ok := e.Payload.(ui.Mouse)
		if !ok || e.ID != "<MouseLeft>" || !image.Pt(mouse.X, mouse.Y).In(m.listBounds) {
			return false
		}
		index := m.top + mouse.Y - m.listBounds.Min.Y
		if index >= 0 && index < len(m.items) && m.items[index].value != "" {
			m.selected = index
			return d.activate(m, m.items[index].value)
		}
		return false
	}
	switch e.ID {
	case "<Escape>", "<Left>", "<Backspace>":
		return d.activate(m, contextBackAction)
	case "<Enter>", "<Right>":
		return d.activate(m, m.selectedValue()) || d.usageOpen
	case "<Tab>", "<S-Tab>", "<Backtab>":
		// Each page has one action: the breakdown, or its return link.
		for i, item := range m.items {
			if item.value != "" {
				m.selected = i
				m.settleListScroll(len(m.items), m.visible)
				break
			}
		}
		return true
	}
	return false
}
