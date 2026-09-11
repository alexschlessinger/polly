package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
)

// sessionsPickerRelist is how often the open picker re-reads the store, for
// ages, message counts, agents spawned since it opened, and sessions that
// finished elsewhere. Everything the tabs here know refreshes every paint.
const sessionsPickerRelist = 2 * time.Second

// sessionsPickerNestedWidth is the modal's width once agent rows are listed:
// room for a status and the brief after the columns.
const sessionsPickerNestedWidth = 80

// sessionsPicker is the Sessions modal's model while it is open. The rows
// keep the order chosen when it opened, so nothing moves under the cursor;
// what each row says is refreshed in place.
type sessionsPicker struct {
	current  string // Stable ID of the workspace that opened the picker.
	modal    *replModal
	rows     []sessionsPickerRow
	infos    map[string]sessions.SessionSummary // keyed by stable ID
	listedAt time.Time
	listing  bool
}

type sessionsPickerRow struct {
	id    string
	depth int
}

func (p *sessionsPicker) named(name string) (sessions.SessionSummary, bool) {
	for _, info := range p.infos {
		if info.Metadata.Name == name {
			return info, true
		}
	}
	return sessions.SessionSummary{}, false
}

func (r *managedREPL) summaryTab(info sessions.SessionSummary) int {
	for i, tab := range r.tabs {
		if info.ID != "" && tab.viewID() == info.ID || info.ID == "" && tab.name == info.Metadata.Name {
			return i
		}
	}
	return -1
}

func (r *managedREPL) openSessionsPicker() {
	r.openSessionsPickerSelected("")
}

// openSessionsPickerSelected lists every session this polly can reach: the
// open workspaces first, in tab order, with what each is doing, then the
// saved sessions, newest first. Agents nest under the session that spawned
// them; an open workspace with live agents starts expanded. Caller holds the
// visible model's lock.
func (r *managedREPL) openSessionsPickerSelected(preferred string) {
	if r.state == nil || r.state.sessionStore == nil {
		r.model.appendNoticeLine("Session picker unavailable")
		return
	}
	summaries, err := r.state.sessionStore.ListSummaries(r.work.ctx)
	if err != nil {
		r.model.appendNoticeLine("Session picker unavailable · " + err.Error())
		return
	}
	p := &sessionsPicker{current: r.visibleTab().viewID(), infos: make(map[string]sessions.SessionSummary), listedAt: time.Now()}
	infos := make([]sessions.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.Metadata != nil {
			infos = append(infos, summary)
			p.infos[summary.ID] = summary
		}
	}
	// Agents nest under the session that spawned them, collapsed until the
	// parent is expanded with Right, so a fan-out never buries the list.
	metas := make([]*sessions.Metadata, len(infos))
	for i, summary := range infos {
		meta := *summary.Metadata
		if parent, ok := p.infos[summary.ParentID]; !ok || parent.Metadata.Name != meta.Parent {
			meta.Parent = ""
		}
		metas[i] = &meta
	}
	workspaces := r.workspaceTabs()
	workspaceIndex := func(node sessionTreeNode) (int, bool) {
		for n, workspace := range workspaces {
			if workspace.viewID() == infos[node.Index].ID {
				return n, true
			}
		}
		return 0, false
	}
	priority := func(node sessionTreeNode) int {
		info := infos[node.Index]
		p, approval, owned := r.swarmListing(info.ID, info.Metadata.SwarmID)
		active := p.Busy
		if !owned {
			if tab := r.summaryTab(info); tab >= 0 {
				status := r.peekTabActivity(r.tabs[tab])
				approval = status == "approval needed"
				active = tabActivityBusy(status)
			}
		}
		if approval || owned && p.Attention {
			return 0
		}
		if active {
			return 1
		}
		return 2
	}
	for _, node := range orderSessionGroups(sessionTree(metas), workspaceIndex, priority) {
		p.rows = append(p.rows, sessionsPickerRow{id: infos[node.Index].ID, depth: node.Depth})
	}
	if len(p.rows) == 0 {
		r.model.appendNoticeLine("No saved sessions")
		return
	}
	target := r.visibleTab().name
	if preferred != "" {
		target = preferred
		if info, ok := p.infos[preferred]; ok {
			target = info.Metadata.Name
		}
	}
	for _, row := range p.rows {
		info := p.infos[row.id].Metadata
		if info.Name == target && row.depth > 0 {
			r.expandPickerParent(info.Parent)
		}
		// An open workspace with agents running here shows them without a keypress.
		if row.depth == 0 && r.summaryTab(p.infos[row.id]) >= 0 && r.hasLiveAgents(r.tabs[r.summaryTab(p.infos[row.id])]) {
			r.expandPickerParent(info.Name)
		}
	}
	if r.pickerExpanded == nil {
		r.pickerExpanded = map[string]bool{}
	}
	m := &replModal{
		title: "Sessions", expanded: r.pickerExpanded,
		width: 64, maxRows: 14, showCount: true,
		onSubmit: func(name string) {
			info, ok := p.named(name)
			if !ok || info.ID == p.current {
				return
			}
			if info.InUse && r.summaryTab(info) < 0 && info.Metadata.Parent == "" {
				r.model.appendErrorLine(name + " is open in another polly")
				return
			}
			r.requestOpenTargetLocked(sessions.ViewTarget{ID: info.ID, Name: info.Metadata.Name})
		},
		onEditTitle: func(name string) {
			if info, ok := p.named(name); ok {
				r.openSessionTitleInput(info)
			}
		},
	}
	r.sessionsPicker = p
	p.modal = m
	m.refresh = func() { r.refreshSessionsPicker(p, m) }
	m.items = r.sessionsPickerItems(p)
	expandPickerSelection(m, target)
	if m.nested() {
		m.width = sessionsPickerNestedWidth
	}
	for i, item := range m.filteredItems() {
		if item.value == target {
			m.selected = i
			break
		}
	}
	r.openModal(m)
}

// refreshSessionsPicker brings the open picker up to date before a paint:
// the rows are rebuilt from the tabs here, and every sessionsPickerRelist
// the store is read again off the event loop. The selection stays on the
// row it was on. Runs on the event loop with the visible model's lock held.
func (r *managedREPL) refreshSessionsPicker(p *sessionsPicker, m *replModal) {
	if !p.listing && time.Since(p.listedAt) >= sessionsPickerRelist && r.state != nil && r.state.sessionStore != nil {
		p.listing = true
		store, owner := r.state.sessionStore, r.model
		if !r.background(func() {
			summaries, err := store.ListSummaries(r.work.ctx)
			r.postUI(context.Background(), func() {
				owner.mu.Lock()
				defer owner.mu.Unlock()
				p.listing = false
				p.listedAt = time.Now()
				if err == nil && owner == r.model && owner.modal == m {
					selected := pickerSelection(m)
					p.merge(summaries, m.expanded)
					r.refreshSessionsPickerItems(p, m, selected)
				}
			})
		}) {
			p.listing = false
		}
	}
	r.refreshSessionsPickerItems(p, m, pickerSelection(m))
}

func pickerSelection(m *replModal) string {
	items := m.filteredItems()
	if len(items) == 0 {
		return ""
	}
	return items[max(0, min(m.selected, len(items)-1))].identity
}

func (r *managedREPL) refreshSessionsPickerItems(p *sessionsPicker, m *replModal, selected string) {
	m.items = r.sessionsPickerItems(p)
	expandPickerSelection(m, selected)
	if m.nested() {
		m.width = sessionsPickerNestedWidth
	}
	items := m.filteredItems()
	m.selected = max(0, min(m.selected, len(items)-1))
	for i, item := range items {
		if item.identity == selected {
			m.selected = i
			break
		}
	}
}

// Keep surviving identities in their previous order, but rebuild ancestry
// from ParentID. A newly listed child can precede its parent in the store;
// a renamed or deleted parent must never attach it to a reused handle.
func (p *sessionsPicker) merge(summaries []sessions.SessionSummary, expanded map[string]bool) {
	next := make(map[string]sessions.SessionSummary, len(summaries))
	for _, info := range summaries {
		if info.Metadata == nil {
			continue
		}
		if old, ok := p.infos[info.ID]; ok && old.Metadata.Name != info.Metadata.Name && expanded[old.Metadata.Name] {
			delete(expanded, old.Metadata.Name)
			expanded[info.Metadata.Name] = true
		}
		next[info.ID] = info
	}
	order, seen := []string{}, map[string]bool{}
	for _, row := range p.rows {
		if _, ok := next[row.id]; ok {
			order = append(order, row.id)
			seen[row.id] = true
		}
	}
	for _, info := range summaries {
		if info.Metadata != nil && !seen[info.ID] {
			order = append(order, info.ID)
			seen[info.ID] = true
		}
	}
	children := map[string][]string{}
	roots := []string{}
	for _, id := range order {
		parent := next[id].ParentID
		if _, ok := next[parent]; ok && parent != id {
			children[parent] = append(children[parent], id)
		} else {
			roots = append(roots, id)
		}
	}
	p.infos, p.rows = next, nil
	placed := map[string]bool{}
	var walk func(string, int)
	walk = func(id string, depth int) {
		if placed[id] {
			return
		}
		placed[id] = true
		p.rows = append(p.rows, sessionsPickerRow{id: id, depth: depth})
		for _, child := range children[id] {
			walk(child, depth+1)
		}
	}
	for _, id := range roots {
		walk(id, 0)
	}
	for _, id := range order {
		walk(id, 0)
	}
}

// sessionsPickerItems renders the picker's rows as they stand now: name,
// age, length, then how the session is placed and what it is doing, and for
// an agent the brief it was given. Caller holds the visible model's lock.
func (r *managedREPL) sessionsPickerItems(p *sessionsPicker) []replModalItem {
	nameWidth, lengthWidth := 0, 0
	for _, row := range p.rows {
		summary := p.infos[row.id]
		nameWidth = max(nameWidth, rw.StringWidth(sessionTreeName(summary.Metadata, row.depth)))
		lengthWidth = max(lengthWidth, rw.StringWidth(formatSessionMessageCount(summary.MessageCount)))
	}
	nameWidth = min(nameWidth, 24)
	children := make(map[string]int)
	for _, row := range p.rows {
		if row.depth > 0 {
			children[p.infos[row.id].ParentID]++
		}
	}
	items := make([]replModalItem, 0, len(p.rows))
	for _, row := range p.rows {
		summary := p.infos[row.id]
		info := summary.Metadata
		name := style.Truncate(sessionTreeName(info, row.depth), nameWidth)
		age := formatCompactDuration(time.Since(info.LastUsed))
		length := formatSessionMessageCount(summary.MessageCount)
		nameColumn := fmt.Sprintf("%-*s", nameWidth, name)
		ageColumn := fmt.Sprintf("%4s", age)
		lengthColumn := fmt.Sprintf("%*s", lengthWidth, length)
		label := nameColumn + "  " + ageColumn + "  " + lengthColumn
		display := style.Escape(nameColumn) + "  " + style.Styled(ageColumn, "muted", "") + "  " + style.Styled(lengthColumn, "muted", "")
		selectedDisplay := style.Styled(nameColumn, "accent", "bold") + "  " + style.Styled(ageColumn, "muted", "") + "  " + style.Styled(lengthColumn, "muted", "")
		if sessions.DisplayLabel(info) != info.Name {
			label += "  " + info.Name
			display += "  " + style.Styled(style.Escape(info.Name), "muted", "")
			selectedDisplay += "  " + style.Styled(style.Escape(info.Name), "muted", "")
		}
		mark, color, status := "", "", ""
		tab := r.summaryTab(summary)
		listing, approval, owned := r.swarmListing(summary.ID, info.SwarmID)
		if owned {
			status = listingLabel(listing, approval)
		}
		if !owned && tab >= 0 {
			status = r.tabActivityLine(r.tabs[tab])
			// A root's own swarm lifecycle follows its turn outcome once the
			// turn is over; while it runs, the turn's own label speaks.
			if p, ok := r.parentPresentation(r.tabs[tab]); ok && row.depth == 0 && tabHasSwarm(r.tabs[tab]) && parentInformative(p) && !tabActivityBusy(r.peekTabActivity(r.tabs[tab])) {
				status = joinStatus(status, p.Display)
			}
		}
		switch {
		case summary.ID == p.current:
			mark, color = "current", "accent"
		case owned:
		case tab >= 0:
			mark, color = "active agent", "ok"
			for n, workspace := range r.workspaceTabs() {
				if workspace == r.tabs[tab] {
					mark = fmt.Sprintf("workspace %d", n+1)
					break
				}
			}
			if row.depth > 0 && status != "" {
				mark = ""
			}
		case summary.InUse:
			mark, color = "in use", "active"
		default:
			if info.SpawnOutcome != "" {
				status = spawnOutcomeStatus(info.SpawnOutcome)
			}
		}
		var plain, shown []string
		if mark != "" {
			plain, shown = append(plain, mark), append(shown, style.Styled(mark, color, ""))
		}
		if status != "" {
			plain, shown = append(plain, status), append(shown, style.Styled(status, "muted", ""))
		}
		if len(plain) > 0 {
			label += "  " + strings.Join(plain, " · ")
			display += "  " + strings.Join(shown, style.Styled(" · ", "muted", ""))
			selectedDisplay += "  " + strings.Join(shown, style.Styled(" · ", "muted", ""))
		}
		item := replModalItem{
			label: label, value: info.Name, identity: summary.ID, display: display, selectedDisplay: selectedDisplay,
			searchText: info.Title + " " + info.Name + " " + info.Description,
			children:   children[summary.ID],
		}
		if row.depth > 0 {
			item.parent = p.infos[summary.ParentID].Metadata.Name
			if brief := strings.Join(strings.Fields(info.Description), " "); brief != "" && brief != sessions.DisplayLabel(info) {
				item.display += "  " + style.Styled(style.Escape(brief), "muted", "")
				item.selectedDisplay += "  " + style.Styled(style.Escape(brief), "muted", "")
			}
		} else if item.children > 0 {
			item.nestDetail = r.agentsSummary(info.Name)
		}
		items = append(items, item)
	}
	return r.agentHistoryItems(p, items)
}

// tabActivityLine is what a tab is doing, for a listing: the live activity
// with its clock, or how its last turn ended with the time it took.
func (r *managedREPL) tabActivityLine(tab *replTab) string {
	activity, elapsed := r.peekTab(tab)
	switch activity {
	case "done", "failed", "incomplete":
		if elapsed > 0 {
			activity += " · " + formatElapsed(elapsed)
		}
	}
	return activity
}

// peekTab samples a tab's activity and last turn time without waiting on a
// tab that is mid-update; such a tab reports what it last said.
func (r *managedREPL) peekTab(tab *replTab) (string, time.Duration) {
	if tab.model == r.model {
		return modelTabActivity(tab.model), tab.model.lastElapsed
	}
	if tab.model.mu.TryLock() {
		defer tab.model.mu.Unlock()
		return modelTabActivity(tab.model), tab.model.lastElapsed
	}
	return "working", 0
}

// agentsSummary counts what a workspace's agents here are doing, for the
// row that lists them: "1 running", "2 need approval", "1 needs decision".
func (r *managedREPL) agentsSummary(name string) string {
	tab := r.tabIndexOf(name)
	if tab < 0 {
		return ""
	}
	running, approvals, decisions := r.agentCountsFor(r.tabs[tab])
	var parts []string
	if approvals > 0 {
		parts = append(parts, needsLabel(approvals, "approval"))
	}
	if decisions > 0 {
		parts = append(parts, needsLabel(decisions, "decision"))
	}
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", running))
	}
	return strings.Join(parts, " · ")
}

// needsLabel reads "1 needs decision" or "3 need approval".
func needsLabel(n int, noun string) string {
	if n == 1 {
		return "1 needs " + noun
	}
	return fmt.Sprintf("%d need %s", n, noun)
}

// agentCountsFor counts the agents of a workspace that are running here and
// that wait on an approval, and the decisions its swarm needs from the
// parent (a total over the cached snapshot, never a truncated page).
func (r *managedREPL) agentCountsFor(root *replTab) (running, approvals, decisions int) {
	seen := map[string]bool{}
	if root.swarmSnapshot != nil {
		decisions = swarm.StatusCounts(root.swarmSnapshot, root.viewID()).NeedsDecision
		for id := range root.swarmSnapshot.Members {
			p, approval, _ := r.swarmListing(id, root.viewID())
			seen[id] = true
			if approval {
				approvals++
			} else if p.Busy {
				running++
			}
		}
	}
	for _, child := range r.tabs {
		if child == root || r.rootTab(child) != root || seen[child.viewID()] {
			continue
		}
		switch r.peekTabActivity(child) {
		case "approval needed":
			approvals++
		case "", "done", "failed", "incomplete":
		default:
			running++
		}
	}
	return
}

// swarmListing projects the cached parent-owned runtime and its approval
// queue by stable member ID. Caller holds the visible model's lock.
func (r *managedREPL) swarmListing(id, swarmID string) (p swarm.AgentPresentation, approval, owned bool) {
	if id == "" || swarmID == "" {
		return
	}
	for _, root := range r.tabs {
		if root.viewID() != swarmID || root.swarmSnapshot == nil {
			continue
		}
		member := root.swarmSnapshot.Members[id]
		if member == nil {
			return
		}
		p = swarmMemberActivity(root.swarmSnapshot, member)
		approvals := func() bool {
			m := root.model
			if m != r.model {
				if !m.mu.TryLock() {
					return false
				}
				defer m.mu.Unlock()
			}
			return m.memberApproval(id) != nil
		}
		return p, approvals(), true
	}
	return
}

// agentsStatus is the status row's word on the visible workspace's agents
// here: the approvals they wait on first, then the decisions the swarm needs
// from the parent, else how many run. Empty when none does. Runs on the
// event loop with the visible model lock held.
func (r *managedREPL) agentsStatus() (text, color string) {
	running, approvals, decisions := r.agentCountsFor(r.visibleTab())
	delivering := 0
	if root := r.rootTab(r.visibleTab()); root != nil && root.swarmSnapshot != nil {
		delivering = swarm.StatusCounts(root.swarmSnapshot, root.viewID()).Delivering
	}

	switch {
	case approvals > 0:
		return needsLabel(approvals, "approval"), "active"
	case decisions > 0:
		return needsLabel(decisions, "decision"), "active"
	case running > 0:
		text := turnAgentLabel(running) + " running"
		if delivering > 0 {
			text += fmt.Sprintf(" · %d delivering", delivering)
		}
		return text, "run"
	case delivering > 0:
		return fmt.Sprintf("%d delivering", delivering), "run"
	}
	// A paused parent is the swarm's own unfinished business.
	if root := r.rootTab(r.visibleTab()); root != nil {
		if p, ok := r.parentPresentation(root); ok && tabHasSwarm(root) && p.Lifecycle == swarm.LifecyclePaused {
			return "swarm " + p.Display, "active"
		}
	}
	return "", ""
}
