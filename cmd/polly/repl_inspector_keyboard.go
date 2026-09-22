package main

import (
	"fmt"
	"image"
	"slices"
)

var swarmInspectorSections = []string{"members", "tasks", "messages", "publications", "workflows", "integrations", "previews", "raw"}

// Inspector actions use the same visible hitboxes and activation path as the
// mouse. Identity, rather than an index or screen coordinate, survives repaint.
// Paging exposes further actions without trapping focus inside the inspector.
type inspectorKeyboardAction struct {
	key  string
	rect image.Rectangle
}

func (r *managedREPL) inspectorKeyboardActions() []inspectorKeyboardAction {
	i := &r.workspace().inspector
	if !i.open || i.keyboardPaintedTarget != i.target.key() {
		return nil
	}
	var actions []inspectorKeyboardAction
	seen := map[string]bool{}
	add := func(key string, rect image.Rectangle) {
		rect = rect.Intersect(r.chrome.inner)
		if seen[key] || rect.Empty() {
			return
		}
		seen[key] = true
		// Earlier targets own overlapping hitboxes, just as in the click path.
		for _, a := range actions {
			if rect.Min.In(a.rect) {
				return
			}
		}
		actions = append(actions, inspectorKeyboardAction{key, rect})
	}
	for _, b := range r.inspectorButtons {
		add("button:"+b.action, b.rect)
	}
	if i.current != nil && i.current.model != nil && i.current.target.key() == i.target.key() {
		m := i.current.model
		for _, link := range m.agentLinkPlacements {
			add(fmt.Sprintf("agent:%d:%d:%s", link.recordID, link.rowIndex, link.workflow), image.Rect(link.X, link.Y, link.X+link.Cols, link.Y+1))
		}
		for _, link := range m.inspectionLinks {
			add(fmt.Sprintf("inspection:%d:%s", link.kind, link.key), link.rect)
		}
		for _, kind := range disclosureKinds {
			for _, p := range m.disclosurePlacements[kind] {
				x := p.X + r.chrome.inner.Min.X
				add(fmt.Sprintf("disclosure:%d:%d", kind, p.recordID), image.Rect(x, p.Y, x+p.Cols, p.Y+1))
			}
		}
		for _, p := range m.imagePlacements {
			if p.Path != "" {
				body := r.chrome.inner
				body.Min.Y += r.inspectorHeaderRows
				add("image:"+p.Key, image.Rect(p.X, p.Y, p.X+p.Cols, p.Y+p.Rows).Intersect(body))
			}
		}
	}
	slices.SortStableFunc(actions, func(a, b inspectorKeyboardAction) int {
		if a.rect.Min.Y != b.rect.Min.Y {
			return a.rect.Min.Y - b.rect.Min.Y
		}
		return a.rect.Min.X - b.rect.Min.X
	})
	return actions
}

func (r *managedREPL) selectedInspectorAction() (inspectorKeyboardAction, bool) {
	i := &r.workspace().inspector
	if !r.inspectorFocused() || i.searching || i.keyboardTarget != i.target.key() || i.keyboardAction == "" {
		return inspectorKeyboardAction{}, false
	}
	for _, action := range r.inspectorKeyboardActions() {
		if action.key == i.keyboardAction {
			return action, true
		}
	}
	return inspectorKeyboardAction{}, false
}

func (r *managedREPL) navigateInspectorActions(key string) bool {
	i := &r.workspace().inspector
	selecting := i.keyboardAction != "" && i.keyboardTarget == i.target.key()
	cycle := key == "<S-Tab>" || key == "<Backtab>"
	if !cycle && !selecting {
		return false
	}
	switch key {
	case "<S-Tab>", "<Backtab>", "<Left>", "<Right>", "<Enter>":
	default:
		i.keyboardAction = ""
		return false
	}
	actions := r.inspectorKeyboardActions()
	index := -1
	if selecting {
		for n, a := range actions {
			if a.key == i.keyboardAction {
				index = n
				break
			}
		}
	}
	if key == "<Enter>" {
		// A disappearing action must not redirect Enter to a different control.
		if index >= 0 {
			r.activateInspectorPoint(actions[index].rect.Min)
		}
		i.keyboardAction = ""
		return true
	}
	if len(actions) == 0 {
		i.keyboardAction = ""
		return true
	}
	if index < 0 {
		index = 0
	} else if key == "<Left>" {
		index = (index + len(actions) - 1) % len(actions)
	} else {
		index = (index + 1) % len(actions)
	}
	i.keyboardAction, i.keyboardTarget = actions[index].key, i.target.key()
	return true
}

func (r *managedREPL) navigateSwarmInspector(key string) bool {
	if key != "<Left>" && key != "<Right>" {
		return false
	}
	i := &r.workspace().inspector
	index := slices.Index(swarmInspectorSections, i.target.item)
	if index < 0 {
		index = 0
	}
	if key == "<Left>" {
		index = max(0, index-1)
	} else {
		index = min(len(swarmInspectorSections)-1, index+1)
	}
	r.inspectorAction("swarm_" + swarmInspectorSections[index])
	return true
}

func (r *managedREPL) inspectorKeyboardHint() string {
	i := &r.workspace().inspector
	if !i.focused {
		return "Tab focus · Shift-Tab actions"
	}
	if i.keyboardAction != "" && i.keyboardTarget == i.target.key() {
		return "←→ action · Enter activate · ↑↓ return"
	}
	switch i.target.kind {
	case toolViewKind, changesViewKind:
		return "↑↓ select · Enter toggle · Shift-Tab actions"
	case agentsViewKind:
		return "↑↓ select · Enter open · Shift-Tab actions"
	case thoughtViewKind:
		return "←→ thought · ↑↓ scroll · Shift-Tab actions"
	case swarmViewKind:
		return "←→ section · ↑↓ scroll · Shift-Tab actions"
	default:
		return "↑↓ scroll · Shift-Tab actions · ← back"
	}
}
