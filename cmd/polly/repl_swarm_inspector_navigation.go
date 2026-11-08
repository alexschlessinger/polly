package main

import "time"

type swarmInspectorPosition struct {
	top, lastRows, lastWidth, lastTotal int
	follow                              bool
	anchor                              viewAnchor
}

func (r *managedREPL) navigateSwarmInspectorPage(forward bool) bool {
	w := r.workspace()
	i := &w.inspector
	s := w.viewState(i.target)
	if s.swarm == nil || i.current != nil && i.current.loading {
		return true
	}
	page := *s.swarm
	page.previous = append([]string(nil), page.previous...)
	after := ""
	if forward {
		if page.next == "" {
			return true
		}
		after = page.next
		page.previous = append(page.previous, page.after)
	} else {
		if len(page.previous) == 0 {
			return true
		}
		after = page.previous[len(page.previous)-1]
		page.previous = page.previous[:len(page.previous)-1]
	}
	if i.current != nil && i.current.model != nil {
		rememberViewPosition(i.current.model, s)
	}
	positions := make(map[string]swarmInspectorPosition, len(page.positions)+1)
	for key, value := range page.positions {
		positions[key] = value
	}
	positions[page.after] = swarmInspectorPosition{s.top, s.lastRows, s.lastWidth, s.lastTotal, s.follow, s.anchor}
	page.positions = positions
	if saved, ok := positions[after]; ok {
		s.top, s.lastRows, s.lastWidth, s.lastTotal, s.follow, s.anchor = saved.top, saved.lastRows, saved.lastWidth, saved.lastTotal, saved.follow, saved.anchor
	} else {
		s.top, s.lastRows, s.lastWidth, s.lastTotal, s.follow, s.anchor = 0, -1, 0, 0, false, viewAnchor{}
	}
	page.after, page.next, page.revision = after, "", ""
	s.swarm = &page
	s.revision++
	i.keyboardAction = ""
	r.inspectorRefreshAt = time.Time{}
	return true
}

// The old page must not overwrite a restored destination-page scroll anchor
// while an asynchronous read lands. Geometry-only refreshes still retain it.
func rememberSwarmInspectorPosition(state viewState) bool {
	return state.swarm == nil || state.swarm.after == state.swarm.renderedAfter
}
