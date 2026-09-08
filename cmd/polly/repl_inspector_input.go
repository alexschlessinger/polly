package main

import (
	"image"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func (r *managedREPL) inspectorAction(action string) {
	w := r.workspace()
	i := &w.inspector
	s := w.viewState(i.target)
	switch action {
	case "follow":
		s.follow = true
	case "root":
		r.closeInspector()
	case "close":
		r.closeInspector()
	case "back":
		r.inspectorHistory(-1)
	case "forward":
		r.inspectorHistory(1)
	case "prev":
		r.inspectorSequence(-1)
	case "next":
		r.inspectorSequence(1)
	case "maximize":
		i.maximized = !i.maximized
	case "wider":
		r.resizeInspector(1)
	case "narrower":
		r.resizeInspector(-1)
	case "find":
		i.searching = true
		i.searchInput.setText(s.search)
	case "prompt":
		if i.current != nil && i.current.model != nil && i.current.model.collapseInitialPrompt {
			s.promptExpanded = !s.promptExpanded
			s.lastRows = -1
			i.current.model.setInitialPromptExpanded(s.promptExpanded)
		}
	case "parent":
		// Navigation leaves the Find row behind; it belongs to the item.
		i.searching = false
		if i.target.kind != conversationViewKind {
			t := i.target
			if r.targetsVisibleTab(t) {
				r.closeInspector()
			} else {
				t.kind = conversationViewKind
				t.item = ""
				r.inspect(t)
			}
		} else {
			if v := i.current; v != nil && v.loading && v.info == nil {
				// The parent is unknown until the first read lands; that is
				// not the root, so do not close.
				return
			}
			if i.current != nil && i.current.info != nil && i.current.info.ParentID != "" && i.current.info.ParentID != r.visibleTab().viewID() {
				t := viewTarget{session: sessions.ViewTarget{ID: i.current.info.ParentID, Name: i.current.info.Metadata.Parent}}
				r.inspect(t)
			} else {
				r.closeInspector()
			}
		}
	case "message":
		r.messageInspectedAgent()
	case "stop":
		target := i.target
		r.workspaceActions = append(r.workspaceActions, func() { r.stopInspectedAgent(target) })
	case "review":
		r.reviewAgentApproval(i.target)
	case "agent":
		if i.current == nil || i.current.model == nil {
			return
		}
		for _, t := range i.current.model.inspections.tools {
			if t.key != i.target.item || t.call.Name != "spawn_agent" {
				continue
			}
			r.inspectLaunchedAgent(i.target, t.call.ID)
			return
		}
	}
}

func (r *managedREPL) resizeInspector(direction int) {
	i := &r.workspace().inspector
	width, _ := ui.TerminalDimensions()
	if !i.open || i.maximized || width < splitThreshold {
		return
	}
	// Start from the displayed divider, including after a drag or terminal
	// resize clamps a pane to its minimum. A width control must never reverse.
	lo, hi := splitLimits(width)
	left := max(lo, min(hi, r.splitColumn(width)-direction*max(1, (width-1)/20)))
	// Store the middle of this column's interval so float rounding does not
	// place the divider one column short when geometry converts back to int.
	r.inspectorRatio = (float64(left) + .5) / float64(width-1)
}

func (r *managedREPL) inspectorScroll(delta int) {
	w := r.workspace()
	i := &w.inspector
	if i.current == nil || i.current.model == nil {
		return
	}
	s := w.viewState(i.target)
	rows := len(i.current.model.visual.rows)
	height := max(1, r.chrome.inner.Dy()-r.inspectorHeaderRows)
	if s.follow {
		s.top = max(0, rows-height)
		s.lastRows = rows
	}
	s.top = max(0, min(max(0, rows-height), s.top+delta))
	s.follow = s.top >= max(0, rows-height)
}

func (r *managedREPL) inspectorFind() {
	w := r.workspace()
	i := &w.inspector
	s := w.viewState(i.target)
	s.search = i.searchInput.text()
	i.searching = false
	if s.search == "" || i.current == nil || i.current.model == nil {
		return
	}
	rows := i.current.model.visual.rows
	needle := strings.ToLower(s.search)
	for n := 1; n <= len(rows); n++ {
		idx := (s.top + n) % len(rows)
		var b strings.Builder
		for _, cell := range rows[idx] {
			b.WriteRune(cell.Rune)
		}
		if strings.Contains(strings.ToLower(b.String()), needle) {
			s.top = idx
			s.follow = false
			s.lastRows = len(rows)
			return
		}
	}
}

// Main model is locked by the event dispatcher. Inspector projections are
// loop-owned; events cannot redirect root input merely by changing a view.
func (r *managedREPL) handleInspectorEvent(e ui.Event) bool {
	w := r.workspace()
	i := &w.inspector
	if e.ID == "<C-c>" || e.ID == "<C-z>" || r.model.pasting || e.ID == pasteStartID || e.ID == pasteEndID {
		return false
	}
	if i.open && i.searching && e.Type == ui.KeyboardEvent {
		switch e.ID {
		case "<Escape>":
			i.searching = false
		case "<Enter>":
			r.inspectorFind()
		case "<Backspace>":
			text := i.searchInput.text()
			if text != "" {
				_, size := utf8.DecodeLastRuneInString(text)
				i.searchInput.setText(text[:len(text)-size])
			}
		default:
			if !strings.HasPrefix(e.ID, "<") {
				i.searchInput.setText(i.searchInput.text() + e.ID)
			}
		}
		return true
	}
	// Search and approval own Escape first: cancel the search, deny the call.
	if i.open && e.ID == "<Escape>" && !r.model.hist.searching && r.model.approval == nil {
		r.closeInspector()
		return true
	}
	if r.handleViewNavigation(e) {
		return true
	}
	if e.Type == ui.MouseEvent {
		mouse, ok := e.Payload.(ui.Mouse)
		if !ok {
			return false
		}
		point := image.Pt(mouse.X, mouse.Y)
		if e.ID == "<MouseLeft>" && point.In(r.chrome.divider) {
			r.inspectorDragging = true
			return true
		}
		if r.inspectorDragging {
			if e.ID == "<MouseRelease>" {
				r.inspectorDragging = false
				return true
			}
			if e.ID == "<MouseLeft>" {
				width, _ := ui.TerminalDimensions()
				r.inspectorRatio = float64(mouse.X) / float64(max(1, width-1))
				return true
			}
		}
		if i.open && point.In(r.chrome.frame) && !point.In(r.chrome.inner) {
			// The frame belongs to the inspector, including in full-width
			// mode; clicks must not reach the hidden conversation underneath.
			switch e.ID {
			case "<MouseWheelUp>":
				r.inspectorScroll(-3)
			case "<MouseWheelDown>":
				r.inspectorScroll(3)
			}
			return true
		}
		if i.open && point.In(r.chrome.inner) {
			switch e.ID {
			case "<MouseWheelUp>":
				r.inspectorScroll(-3)
				return true
			case "<MouseWheelDown>":
				r.inspectorScroll(3)
				return true
			case "<MouseLeft>":
				for _, b := range r.inspectorButtons {
					if point.In(b.rect) {
						r.inspectorAction(b.action)
						return true
					}
				}
				if mouse.Y < r.chrome.inner.Min.Y+r.inspectorHeaderRows {
					return true
				}
				if i.current != nil && i.current.model != nil {
					m := i.current.model
					if r.inspectViewAt(m, i.target, point) {
						return true
					}
					s := w.viewState(i.target)
					m.followBottom, m.scrollAnchor = s.follow, s.top
					x := mouse.X - r.chrome.inner.Min.X
					if m.toggleTurnTrailerAt(x, mouse.Y) || m.closeTurnDockOverlay() || m.toggleReasoningAt(x, mouse.Y, r.chrome.inner.Dx()) || m.toggleToolDisclosureAt(x, mouse.Y) || m.toggleAgentDisclosureAt(x, mouse.Y) || m.toggleImageDisclosureAt(x, mouse.Y) {
						s := w.viewState(i.target)
						s.top = m.scrollAnchor
						rememberViewSections(m, s)
						return true
					}
					// Images carry absolute geometry; the existing viewer resolves them.
					for _, p := range m.imagePlacements {
						if point.In(image.Rect(p.X, p.Y, p.X+p.Cols, p.Y+p.Rows)) && p.Path != "" {
							path := p.Path
							r.workspaceActions = append(r.workspaceActions, func() { _ = r.openImage(path) })
							return true
						}
					}
				}
				return true
			}
		}
		if e.ID == "<MouseLeft>" {
			return r.inspectViewAt(r.model, tabViewTarget(r.visibleTab()), point)
		}
	}
	return false
}

// Plain navigation keys follow the pointer across both transcript panes.
// Control-key editor shortcuts and text input continue to address the composer.
func (r *managedREPL) handleViewNavigation(e ui.Event) bool {
	if e.Type != ui.KeyboardEvent || !r.mousePositionKnown || r.model.hist.searching || r.model.approval != nil {
		return false
	}
	i := &r.workspace().inspector
	inspector := i.open && (r.mousePosition.In(r.chrome.inner) || r.mousePosition.In(r.chrome.frame))
	if !inspector && !r.mousePosition.In(r.chrome.main) {
		return false
	}
	height := r.chrome.main.Dy()
	if inspector {
		height = r.chrome.inner.Dy() - r.inspectorHeaderRows
	}
	delta := 0
	switch e.ID {
	case "<Left>", "<Right>":
		if !inspector || i.target.kind == conversationViewKind {
			return false
		}
		direction := -1
		if e.ID == "<Right>" {
			direction = 1
		}
		r.inspectorSequence(direction)
		return true
	case "<Up>":
		delta = -1
	case "<Down>":
		delta = 1
	case "<PageUp>":
		delta = -max(1, height/2)
	case "<PageDown>":
		delta = max(1, height/2)
	case "<Home>":
		if inspector {
			s := r.workspace().viewState(i.target)
			s.top, s.follow = 0, false
			if i.current != nil && i.current.model != nil {
				s.lastRows = len(i.current.model.visual.rows)
			}
		} else {
			r.model.scrollAnchor, r.model.followBottom = 0, false
		}
		return true
	case "<End>":
		if inspector {
			r.inspectorAction("follow")
		} else {
			r.model.scrollToBottom()
		}
		return true
	default:
		return false
	}
	if inspector {
		r.inspectorScroll(delta)
	} else {
		r.model.scrollByWidth(delta, height, r.chrome.main.Dx())
	}
	return true
}

func (r *managedREPL) inspectViewAt(m *replModel, parent viewTarget, point image.Point) bool {
	for _, link := range m.agentLinkPlacements {
		if point.Y == link.Y && point.X >= link.X && point.X < link.X+link.Cols {
			return r.inspectAgent(m, parent, link)
		}
	}
	for _, link := range m.inspectionLinks {
		if point.In(link.rect) {
			target := parent
			target.kind = link.kind
			target.item = link.key
			r.inspect(target)
			return true
		}
	}
	return false
}
