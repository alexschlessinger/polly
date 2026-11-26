package main

import (
	"image"
	"slices"

	ui "github.com/metaspartan/gotui/v5"
)

// Main-conversation disclosures and output controls use the agent panel's
// existing Shift-Tab action convention, without opening another view. Only
// painted targets participate; paging the transcript exposes older actions.
func (r *managedREPL) inlineKeyboardActions() []inspectorKeyboardAction {
	var actions []inspectorKeyboardAction
	seen := map[string]bool{}
	addMarked := func(key string, rect, mark image.Rectangle) {
		// A keyboard stop is a semantic action, not a screen position.
		if !rect.Empty() && !seen[key] {
			seen[key] = true
			actions = append(actions, inspectorKeyboardAction{key: key, rect: rect, mark: mark})
		}
	}
	add := func(key string, rect image.Rectangle) { addMarked(key, rect, rect) }
	add("todo", r.model.todoLink)
	if r.workspace().inspector.open && r.chrome.main.Empty() {
		return actions // The composer divider remains visible.
	}
	for _, kind := range disclosureKinds {
		for _, p := range r.model.disclosurePlacements[kind] {
			addMarked(p.keyboardKey(), p.rect(0), p.mark(0))
		}
	}
	for _, link := range r.model.toolOutputLinks {
		add(link.keyboardKey(), link.rect())
	}
	slices.SortStableFunc(actions, func(a, b inspectorKeyboardAction) int {
		if a.rect.Min.Y != b.rect.Min.Y {
			return a.rect.Min.Y - b.rect.Min.Y
		}
		return a.rect.Min.X - b.rect.Min.X
	})
	return actions
}

func (l toolOutputLink) keyboardKey() string {
	return "tool-output:" + l.key + ":" + l.action
}

func (r *managedREPL) selectedInlineAction() (inspectorKeyboardAction, bool) {
	if !r.inspectorFocused() {
		for _, action := range r.inlineKeyboardActions() {
			if action.key == r.model.inlineKeyboardAction {
				return action, true
			}
		}
	}
	return inspectorKeyboardAction{}, false
}

func (r *managedREPL) handleInlineActionKey(e ui.Event) bool {
	m := r.model
	if r.inspectorFocused() || r.frameSearching() || m.pasting || m.hist.searching || m.approval != nil || e.Type != ui.KeyboardEvent {
		m.inlineKeyboardAction = ""
		return false
	}
	selecting := m.inlineKeyboardAction != ""
	if e.ID != "<S-Tab>" && e.ID != "<Backtab>" && !selecting {
		return false
	}
	switch e.ID {
	case "<S-Tab>", "<Backtab>", "<Left>", "<Right>", "<Enter>":
	case "<Escape>":
		m.inlineKeyboardAction = ""
		return true
	default:
		m.inlineKeyboardAction = ""
		return false
	}
	actions := r.inlineKeyboardActions()
	index := -1
	for n, action := range actions {
		if action.key == m.inlineKeyboardAction {
			index = n
			break
		}
	}
	if e.ID == "<Enter>" {
		// A disappeared row must never redirect Enter to the draft or another
		// tool. Resolve identity against this frame, just as the panel does.
		if index >= 0 {
			point := actions[index].rect.Min
			switch actions[index].key {
			case "todo":
				r.openTodoPanel()
			default:
				if !r.toggleToolOutputAt(m, point, nil) {
					m.toggleDisclosureAt(point.X, point.Y, m.disclosureLayoutWidth(0))
				}
			}
		}
		m.inlineKeyboardAction = ""
		return true
	}
	if len(actions) == 0 {
		m.inlineKeyboardAction = ""
		return true
	}
	if index < 0 {
		index = 0
	} else if e.ID == "<Left>" {
		index = (index + len(actions) - 1) % len(actions)
	} else {
		index = (index + 1) % len(actions)
	}
	m.inlineKeyboardAction = actions[index].key
	return true
}
