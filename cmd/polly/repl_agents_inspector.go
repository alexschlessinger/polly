package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
)

type agentsInspectorEntry struct {
	target                     sessions.ViewTarget
	label, status              string
	attention, active, history bool
}

type agentsInspectorState struct {
	order           []string
	entries         map[string]agentsInspectorEntry
	selected        string
	historyExpanded bool
}

func (r *managedREPL) openAgentsInspector() {
	target := tabViewTarget(r.visibleTab())
	target.kind = agentsViewKind
	w := r.workspace()
	// Revisit the list without resetting its scroll or expansion state.
	saved := *w.viewState(target)
	r.inspect(target)
	*w.viewState(target) = saved
	w.viewState(target).follow = false
	w.inspector.searching = false
}

// Read only cached coordination and tab display state. The existing swarm
// poller refreshes it without acquiring leases or starting child runtimes.
func (r *managedREPL) agentsInspectorEntries() map[string]agentsInspectorEntry {
	root := r.visibleTab()
	entries := map[string]agentsInspectorEntry{}
	root.model.mu.Lock()
	approvals := map[string]bool{}
	if a := root.model.approval; a != nil {
		approvals[a.requester] = true
	}
	for _, a := range root.model.approvalQueue {
		approvals[a.requester] = true
	}
	root.model.mu.Unlock()
	if snapshot := root.swarmSnapshot; snapshot != nil {
		for id, member := range snapshot.Members {
			p := swarmMemberActivity(snapshot, member)
			attention := p.Attention || approvals[id]
			entries[id] = agentsInspectorEntry{
				target: sessions.ViewTarget{ID: id, Name: member.Name},
				label:  swarmMemberLabel(snapshot, member), status: listingLabel(p, approvals[id]),
				attention: attention, active: p.Busy,
				history: !attention && !p.Busy && !p.Delivering,
			}
		}
	}
	// Legacy child tabs can exist without a swarm member record.
	for _, tab := range r.tabs {
		if tab == root || r.rootTab(tab) != root {
			continue
		}
		id := tab.viewID()
		if id == "" {
			id = "name:" + tab.name
		}
		if _, exists := entries[id]; exists {
			continue
		}
		tab.model.mu.Lock()
		status := modelTabActivity(tab.model)
		label := tab.model.status.displayLabel()
		tab.model.mu.Unlock()
		if label == "" {
			label = tab.name
		}
		attention := status == "approval needed"
		active := tabActivityBusy(status)
		entries[id] = agentsInspectorEntry{target: tabViewTarget(tab).session, label: label, status: status, attention: attention, active: active, history: !attention && !active}
	}
	return entries
}

func (r *managedREPL) refreshAgentsInspector(geometry viewGeometry) {
	width := geometry.width
	r.inspectorRefreshAt = time.Now()
	w := r.workspace()
	i := &w.inspector
	state := w.viewState(i.target)
	if state.agents == nil {
		state.agents = &agentsInspectorState{}
	}
	list := state.agents
	entries := r.agentsInspectorEntries()
	order := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, id := range list.order {
		if _, ok := entries[id]; ok {
			order = append(order, id)
			seen[id] = true
		}
	}
	var added []string
	for id := range entries {
		if !seen[id] {
			added = append(added, id)
		}
	}
	priority := func(id string) int {
		e := entries[id]
		if e.attention {
			return 0
		}
		if e.active {
			return 1
		}
		if !e.history {
			return 2
		}
		return 3
	}
	slices.SortFunc(added, func(a, b string) int {
		if priority(a) != priority(b) {
			return priority(a) - priority(b)
		}
		if n := strings.Compare(entries[a].label, entries[b].label); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	order = append(order, added...)
	list.order, list.entries = order, entries
	if e, ok := entries[list.selected]; ok && e.history {
		list.historyExpanded = true
	}
	var rows, actions []string
	appendEntry := func(id string) {
		e := entries[id]
		statusWidth := min(24, max(1, (width-5)/2))
		status := rw.Truncate(e.status, statusWidth, "…")
		nameWidth := max(1, width-rw.StringWidth(status)-4)
		label := rw.Truncate(strings.Join(strings.Fields(e.label), " "), nameWidth, "…")
		color := "code"
		if e.history {
			color = "muted"
		}
		prefix := "  "
		if list.selected == id {
			prefix = style.Styled("› ", "accent", "")
		}
		statusColor := "muted"
		if e.attention {
			statusColor = "active"
		}
		rows = append(rows, prefix+style.Styled(style.Escape(label), color, "")+strings.Repeat(" ", max(2, nameWidth-rw.StringWidth(label)+2))+style.Styled(style.Escape(status), statusColor, ""))
		actions = append(actions, "agents-open:"+id)
	}
	for _, id := range order {
		if !entries[id].history {
			appendEntry(id)
		}
	}
	historyCount := 0
	for _, id := range order {
		if entries[id].history {
			historyCount++
		}
	}
	if historyCount > 0 {
		glyph := "▸"
		if list.historyExpanded {
			glyph = "▾"
		}
		prefix := ""
		if list.selected == "history:" {
			prefix = style.Styled("› ", "accent", "")
		}
		rows = append(rows, prefix+style.Styled(fmt.Sprintf("%s History (%d)", glyph, historyCount), "muted", ""))
		actions = append(actions, "agents-history")
		if list.historyExpanded {
			for _, id := range order {
				if entries[id].history {
					appendEntry(id)
				}
			}
		}
	}
	if len(rows) == 0 {
		text := "No agents yet"
		if r.visibleTab().swarmLoading {
			text = "Loading agents…"
		}
		rows = append(rows, style.Styled(text, "muted", ""))
		actions = append(actions, "")
	}
	body := strings.Join(rows, "\n")
	if i.current != nil && i.current.revision == body && i.current.geometry == geometry {
		return
	}
	model := newReplModel()
	model.appendLine(body)
	model.transcriptRows(width)
	i.current = &viewInstance{target: i.target, view: conversationView{}, model: model, revision: body, geometry: geometry, agentsActions: actions}
	state.follow = false
	state.lastRows = len(rows)
}

func (r *managedREPL) agentsInspectorAction(action string) bool {
	i := &r.workspace().inspector
	if i.target.kind != agentsViewKind {
		return false
	}
	state := r.workspace().viewState(i.target)
	if state.agents == nil {
		return false
	}
	list := state.agents
	if action == "agents-history" {
		list.historyExpanded = !list.historyExpanded
		if !list.historyExpanded && list.entries[list.selected].history {
			list.selected = ""
		}
		return true
	}
	if !strings.HasPrefix(action, "agents-open:") {
		return false
	}
	id := strings.TrimPrefix(action, "agents-open:")
	entry, ok := list.entries[id]
	if !ok {
		return true
	}
	list.selected = id
	parent := i.target
	target := viewTarget{session: entry.target}
	r.inspect(target)
	r.workspace().viewState(target).agentsParent = &parent
	return true
}

func (r *managedREPL) returnToAgents(parent viewTarget) {
	w := r.workspace()
	saved := *w.viewState(parent)
	r.inspect(parent)
	*w.viewState(parent) = saved
}

func (r *managedREPL) navigateAgentsInspector(key string) bool {
	w := r.workspace()
	i := &w.inspector
	state := w.viewState(i.target)
	if i.current == nil || state.agents == nil {
		return false
	}
	list := state.agents
	actions := i.current.agentsActions
	if len(actions) == 0 {
		return false
	}
	selected := "agents-open:" + list.selected
	if list.selected == "history:" {
		selected = "agents-history"
	}
	index := slices.Index(actions, selected)
	if index < 0 {
		index = 0
		if key == "<Down>" {
			index = -1
		}
	}
	height := max(1, r.chrome.inner.Dy()-r.inspectorHeaderRows)
	switch key {
	case "<Up>":
		index--
	case "<Down>":
		index++
	case "<Home>":
		index = 0
	case "<End>":
		index = len(actions) - 1
	case "<PageUp>":
		index -= height
	case "<PageDown>":
		index += height
	case "<Enter>":
		return r.agentsInspectorAction(actions[index])
	case "<Right>":
		if actions[index] == "agents-history" {
			list.historyExpanded = true
		}
		return true
	case "<Left>":
		list.historyExpanded = false
		list.selected = "history:"
		return true
	default:
		return false
	}
	index = max(0, min(index, len(actions)-1))
	if actions[index] == "agents-history" {
		list.selected = "history:"
	} else {
		list.selected = strings.TrimPrefix(actions[index], "agents-open:")
	}
	state.follow = false
	if index < state.top {
		state.top = index
	}
	if index >= state.top+height {
		state.top = index - height + 1
	}
	return true
}
