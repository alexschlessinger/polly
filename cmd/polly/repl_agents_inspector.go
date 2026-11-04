package main

import (
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
)

type agentsInspectorEntry struct {
	target                     sessions.ViewTarget
	label, status              string
	attention, active, history bool
	// warn colors a live status's clock (see agentRowStatus).
	warn bool
	// progress is the activity line under the row when the entry's own
	// display supplies it; swarm members are projected (see agentProgress).
	progress []turnDockField
	message  *agentMessagePreview
}

type agentsInspectorState struct {
	order    []string
	entries  map[string]agentsInspectorEntry
	selected string
	progress map[string]*agentProgress
	width    int // fences progress projections across pane resizes
}

func (r *managedREPL) openAgentsInspector() {
	target := tabViewTarget(r.rootTab(r.visibleTab()))
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
func (r *managedREPL) agentsInspectorEntries(width int) map[string]agentsInspectorEntry {
	root := r.rootTab(r.visibleTab())
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
		messages := latestAgentMessages(snapshot, root.viewID())
		for id, member := range snapshot.Members {
			p := swarmMemberActivity(snapshot, member)
			attention := p.Attention || approvals[id]
			if p.Lifecycle == swarm.LifecycleIdle && p.TaskStatus == "done" && !attention && !p.Delivering {
				continue
			}
			status, warn := agentListStatus(snapshot, member, p, root.swarmActivities[id], approvals[id], time.Now())
			entries[id] = agentsInspectorEntry{
				target: sessions.ViewTarget{ID: id, Name: member.Name},
				label:  swarmMemberLabel(snapshot, member), status: status, warn: warn,
				attention: attention, active: p.Busy,
				history: !attention && !p.Busy && !p.Delivering, message: messages[id],
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
		if snapshot := root.swarmSnapshot; snapshot != nil && snapshot.Members[id] != nil {
			continue // Swarm state also owns members omitted from the list.
		}
		tab.model.mu.Lock()
		status := modelTabActivity(tab.model)
		label := tab.model.status.displayLabel()
		progress := tab.model.agentProgressFields(width)
		tab.model.mu.Unlock()
		if label == "" {
			label = tab.name
		}
		attention := status == "approval needed"
		active := tabActivityBusy(status)
		entries[id] = agentsInspectorEntry{target: tabViewTarget(tab).session, label: label, status: status, attention: attention, active: active, history: !attention && !active, progress: progress}
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
	entries := r.agentsInspectorEntries(width)
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
	if _, visible := entries[list.selected]; !visible {
		list.selected = ""
	}
	r.refreshAgentProgress(list, width)
	// Statuses share one left-aligned column as wide as the widest of them,
	// so names keep their truncation while live clocks tick.
	statusWidth, column := min(64, max(1, (width-5)/2)), 0
	for _, e := range entries {
		column = max(column, rw.StringWidth(rw.Truncate(e.status, statusWidth, "…")))
	}
	nameWidth := max(1, width-column-4)
	var rows, actions []string
	appendEntry := func(id string) {
		if len(rows) > 0 {
			rows = append(rows, "")
			actions = append(actions, "")
		}
		e := entries[id]
		status := rw.Truncate(e.status, statusWidth, "…")
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
		rows = append(rows, prefix+style.Styled(style.Escape(label), color, "")+strings.Repeat(" ", max(2, nameWidth-rw.StringWidth(label)+2))+rowStatusMarkup(style.Escape(status), statusColor, e.warn && status == e.status))
		actions = append(actions, "agents-open:"+id)
		// Activity and message previews sit under the agent's row; every
		// row opens the same agent.
		fields := e.progress
		if p := list.progress[id]; p != nil && len(p.fields) > 0 {
			fields = p.fields
		}
		for _, row := range agentProgressRows(fields, width) {
			rows = append(rows, row)
			actions = append(actions, "agents-open:"+id)
		}
		if row := e.message.row(width); row != "" {
			rows = append(rows, row)
			actions = append(actions, "agents-open:"+id)
		}
	}
	for _, id := range order {
		if !entries[id].history {
			appendEntry(id)
		}
	}
	// Other inactive agents follow the live ones, dimmed by appendEntry.
	for _, id := range order {
		if entries[id].history {
			appendEntry(id)
		}
	}
	if len(rows) == 0 {
		text := "No agents yet"
		root := r.rootTab(r.visibleTab())
		if snapshot := root.swarmSnapshot; snapshot != nil && len(snapshot.Members) > 0 {
			text = "No active agents"
		} else if root.swarmLoading {
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
	switch key {
	case "<Left>":
		r.inspectorAction("parent")
		return true
	case "<Up>", "<Down>", "<Home>", "<End>", "<PageUp>", "<PageDown>", "<Enter>", "<Right>":
	default:
		return false
	}
	w := r.workspace()
	i := &w.inspector
	state := w.viewState(i.target)
	if i.current == nil || state.agents == nil {
		return true
	}
	list := state.agents
	actions := i.current.agentsActions
	if len(actions) == 0 {
		return true
	}
	// An agent spans its status, activity and message rows; keys move by agent.
	var starts []int
	for n, action := range actions {
		if action != "" && (n == 0 || action != actions[n-1]) {
			starts = append(starts, n)
		}
	}
	if len(starts) == 0 {
		return true
	}
	pos := slices.IndexFunc(starts, func(n int) bool { return actions[n] == "agents-open:"+list.selected })
	if pos < 0 {
		pos = 0
		if key == "<Down>" {
			pos = -1
		}
	}
	height := max(1, r.chrome.inner.Dy()-r.inspectorHeaderRows)
	switch key {
	case "<Up>":
		pos--
	case "<Down>":
		pos++
	case "<Home>":
		pos = 0
	case "<End>":
		pos = len(starts) - 1
	case "<PageUp>":
		pos -= max(1, height/2)
	case "<PageDown>":
		pos += max(1, height/2)
	case "<Enter>", "<Right>":
		return r.agentsInspectorAction(actions[starts[max(0, pos)]])
	default:
		return false
	}
	pos = max(0, min(pos, len(starts)-1))
	first := starts[pos]
	last := first
	for last+1 < len(actions) && actions[last+1] == actions[first] {
		last++
	}
	list.selected = strings.TrimPrefix(actions[first], "agents-open:")
	state.follow = false
	if first < state.top {
		state.top = first
	}
	if last >= state.top+height {
		state.top = max(first, last-height+1)
	}
	return true
}
