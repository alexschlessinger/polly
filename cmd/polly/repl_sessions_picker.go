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
	ui "github.com/metaspartan/gotui/v5"
)

// sessionsPickerRelist bounds metadata refreshes to the newest root page and
// visible identities. Ages and local lease/activity presentation stay live.
const sessionsPickerRelist = 2 * time.Second

// sessionsPicker is the Sessions modal's model while it is open. The rows
// keep the order chosen when it opened, so nothing moves under the cursor;
// what each row says is refreshed in place.
type sessionsPicker struct {
	current         string // Stable ID of the workspace that opened the picker.
	modal           *replModal
	rows            []sessionsPickerRow
	infos           map[string]sessions.SessionSummary // keyed by stable ID
	listedAt        time.Time
	listing         bool
	failed          bool
	flashID         string
	flashUntil      time.Time
	ctx             context.Context
	cancel          context.CancelFunc
	generation      uint64
	query           string
	after           string
	started         bool
	done            bool
	preferred       string
	closedSelection string
	filtered        []replModalItem
	byName          map[string]string
	itemIndex       map[string]int
	filterIndex     map[string]int
	searchNext      string
	searchDone      bool
	filterKey       string
	filterValid     bool
}

type sessionsPickerRow struct {
	id    string
	depth int
}

func (p *sessionsPicker) named(name string) (sessions.SessionSummary, bool) {
	// Selection is an identity, not a mutable/reusable display handle.
	if info, ok := p.infos[p.closedSelection]; ok && info.Metadata.Name == name {
		return info, true
	}
	if p.modal != nil {
		items := p.modal.filteredItems()
		if len(items) > 0 {
			item := items[max(0, min(p.modal.selected, len(items)-1))]
			if item.value == name {
				info, ok := p.infos[item.identity]
				return info, ok
			}
		}
	}
	info, ok := p.infos[p.byName[name]]
	return info, ok
}

func (r *managedREPL) summaryTab(info sessions.SessionSummary) int {
	for i, tab := range r.tabs {
		if info.ID != "" && tab.viewID() == info.ID || info.ID == "" && tab.name == info.Metadata.Name {
			return i
		}
	}
	return -1
}

func (r *managedREPL) sessionLocked(info sessions.SessionSummary) bool {
	if !info.InUse {
		return false
	}
	if index := r.summaryTab(info); index >= 0 {
		tab := r.tabs[index]
		return tab.state == nil || tab.state.session == nil || tab.state.session.Context().Err() != nil
	}
	return true
}

func (r *managedREPL) openSessionsPicker() {
	r.openSessionsPickerSelected("")
}

// openSessionsPickerSelected lists every session this polly can reach: the
// open workspaces first, in tab order, then saved root sessions, newest first.
// Caller holds the visible model's lock.
func (r *managedREPL) openSessionsPickerSelected(preferred string) {
	if r.state == nil || r.state.sessionStore == nil {
		r.model.appendNoticeLine("Session picker unavailable")
		return
	}
	ctx, cancel := context.WithCancel(r.work.ctx)
	p := &sessionsPicker{current: r.visibleTab().viewID(), infos: make(map[string]sessions.SessionSummary), ctx: ctx, cancel: cancel, preferred: preferred}
	// Show open workspaces immediately, in tab order, without touching storage
	// or waiting for another model's lock. Durable metadata follows off-loop.
	for _, tab := range r.workspaceTabs() {
		id := tab.viewID()
		if id == "" {
			continue
		}
		meta := &sessions.Metadata{Name: tab.name, LastUsed: time.Now()}
		if tab.model == r.model {
			meta.Title = tab.model.status.title
		} else if tab.model.mu.TryLock() {
			meta.Title = tab.model.status.title
			tab.model.mu.Unlock()
		}
		p.infos[id] = sessions.SessionSummary{ID: id, Metadata: meta, InUse: true}
		p.rows = append(p.rows, sessionsPickerRow{id: id})
	}
	m := &replModal{
		title: "Sessions", hideHelp: true, width: sessionsPickerModalWidth,
		onWipe: func() {
			p.closedSelection = pickerSelection(p.modal)
			p.cancel()
		},
		canSubmit: func(name string) bool {
			info, ok := p.named(name)
			if ok && r.sessionLocked(info) {
				p.flashID, p.flashUntil = info.ID, time.Now().Add(350*time.Millisecond)
				return false
			}
			return ok
		},
		onSubmit: func(name string) {
			info, ok := p.named(name)
			if !ok || info.ID == p.current {
				return
			}
			if r.sessionLocked(info) {
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
	r.sessionsPicker, p.modal = p, m
	m.filterItems = func() []replModalItem { return p.filter(m.input.text()) }
	m.renderItem = func(item replModalItem) replModalItem { return r.renderSessionPickerItem(p, item) }
	m.refresh = func() { r.refreshSessionsPicker(p, m) }
	r.refreshSessionsPickerItems(p, m, p.current)
	r.openModal(m)
	r.refreshSessionsPicker(p, m)
}

// refreshSessionsPicker does no storage work and never rebuilds the catalogue
// on paint. It schedules a bounded page or visible-row refresh off-loop.
func (r *managedREPL) refreshSessionsPicker(p *sessionsPicker, m *replModal) {
	if p.ctx.Err() != nil || r.state == nil || r.state.sessionStore == nil {
		return
	}
	query := strings.ToLower(strings.TrimSpace(m.input.text()))
	if query != p.query {
		p.cancel()
		p.ctx, p.cancel = context.WithCancel(r.work.ctx)
		p.generation++
		p.listing, p.failed = false, false
		p.query, p.searchNext, p.searchDone = query, "", false
	}
	if p.flashID != "" && time.Now().After(p.flashUntil) {
		p.flashID = ""
	}
	if p.listing || p.failed && time.Since(p.listedAt) < sessionsPickerRelist {
		return
	}
	if query != "" && !p.searchDone {
		r.loadSessionsPickerPage(p, m, pickerSearch)
	} else if !p.started {
		r.loadSessionsPickerPage(p, m, pickerFirst)
	} else if time.Since(p.listedAt) >= sessionsPickerRelist {
		r.loadSessionsPickerPage(p, m, pickerRefresh)
	} else if query == "" && !p.done && m.selected+max(8, m.visible) >= len(m.items) {
		r.loadSessionsPickerPage(p, m, pickerMore)
	}
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
	p.itemIndex = make(map[string]int, len(m.items))
	for i, item := range m.items {
		p.itemIndex[item.identity] = i
	}
	p.filterValid = false
	m.width = sessionsPickerModalWidth
	p.selectIdentity(selected)
}

func (p *sessionsPicker) selectIdentity(selected string) {
	m := p.modal
	items := m.filteredItems()
	m.selected = max(0, min(m.selected, len(items)-1))
	index := p.itemIndex
	if p.filterKey != "" {
		index = p.filterIndex
	}
	if i, ok := index[selected]; ok {
		m.selected = i
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

const sessionsPickerModalWidth = 96

// sessionsPickerItems renders the picker's rows as they stand now: name,
// age, and message count. Only root sessions appear; the current one is highlighted.
// Caller holds the visible model's lock.
func (r *managedREPL) sessionsPickerItems(p *sessionsPicker) []replModalItem {
	items := make([]replModalItem, 0, len(p.rows))
	p.byName = make(map[string]string, len(p.infos))
	for id, summary := range p.infos {
		p.byName[summary.Metadata.Name] = id
	}
	for _, row := range p.rows {
		summary := p.infos[row.id]
		info := summary.Metadata
		if summary.ParentID != "" || info.Parent != "" {
			continue
		}
		items = append(items, sessionPickerDescriptor(summary))
	}
	return items
}

func sessionPickerDescriptor(summary sessions.SessionSummary) replModalItem {
	info := summary.Metadata
	return replModalItem{
		label: sessionTreeName(info, 0), value: info.Name, identity: summary.ID,
		searchText: strings.ToLower(info.Title + " " + info.Name + " " + info.Description),
	}
}

// Only the viewport is styled, including ages, locks and terminal resizing.
// Fixed metadata columns avoid a catalogue-wide width scan on every paint.
func (r *managedREPL) renderSessionPickerItem(p *sessionsPicker, item replModalItem) replModalItem {
	summary, ok := p.infos[item.identity]
	if !ok {
		return item
	}
	info := summary.Metadata
	terminalWidth, _ := ui.TerminalDimensions()
	width := sessionsPickerModalWidth
	if terminalWidth > 0 {
		width = modalWidthForTerminal(terminalWidth, width)
	}
	const lengthWidth = 10
	nameWidth := max(1, width-5-4-4-lengthWidth)
	locked := r.sessionLocked(summary)
	name := sessionTreeName(info, 0)
	if locked {
		name = "× " + name
	}
	name = style.Truncate(name, nameWidth)
	nameColumn := name + strings.Repeat(" ", max(0, nameWidth-rw.StringWidth(name)))
	ageColumn := fmt.Sprintf("%4s", formatCompactDuration(time.Since(info.LastUsed)))
	lengthColumn := fmt.Sprintf("%*s", lengthWidth, formatSessionMessageCount(summary.MessageCount))
	color, modifier := "muted", ""
	switch {
	case locked:
		if p.flashID == summary.ID && time.Now().Before(p.flashUntil) {
			color = "active"
		}
	case r.summaryTab(summary) >= 0:
		color, modifier = "code", "bold"
	}
	nameColor := color
	if summary.ID == p.current && !locked {
		nameColor = "accent"
	}
	metadata := "  " + style.Styled(ageColumn, color, "") + "  " + style.Styled(lengthColumn, color, "")
	item.label = nameColumn + "  " + ageColumn + "  " + lengthColumn
	item.display = style.Styled(style.Escape(nameColumn), nameColor, modifier) + metadata
	item.selectedDisplay = style.Styled(style.Escape(nameColumn), nameColor, "bold") + metadata
	return item
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
	running, approvals, decisions, _ := r.agentCountsFor(r.tabs[tab])
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

// agentCountsFor sorts every agent of a workspace into one of three counts:
// waiting on an approval, running (a member parked on a message included), or
// finished, its latest run ended however it ended. It also totals the
// decisions the workspace's swarm needs from the parent (over the cached
// snapshot, never a truncated page).
func (r *managedREPL) agentCountsFor(root *replTab) (running, approvals, decisions, finished int) {
	snapshot := root.displaySnapshot()
	if snapshot != nil {
		decisions = snapshot.Status.Counts.NeedsDecision
		running, finished = snapshot.BusyMembers, len(snapshot.Members)-snapshot.BusyMembers
		// Approval overlays only inspect the pending requests, not every retained
		// member. A busy foreign model retains the previous nonblocking behavior.
		func() {
			m := root.model
			if m != r.model {
				if !m.mu.TryLock() {
					return
				}
				defer m.mu.Unlock()
			}
			seen := map[string]bool{}
			add := func(a *approvalState) {
				if a == nil || seen[a.requester] || snapshot.Members[a.requester] == nil {
					return
				}
				seen[a.requester] = true
				approvals++
				if snapshot.MemberState(a.requester).Busy {
					running--
				} else {
					finished--
				}
			}
			add(m.approval)
			for _, a := range m.approvalQueue {
				add(a)
			}
		}()
	}
	for _, child := range r.tabs {
		if child == root || r.rootTab(child) != root || snapshot != nil && snapshot.Members[child.viewID()] != nil {
			continue
		}
		switch r.peekTabActivity(child) {
		case "approval needed":
			approvals++
		case "", "done", "failed", "incomplete":
			finished++
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
		if root.viewID() != swarmID {
			continue
		}
		snapshot := root.displaySnapshot()
		if snapshot == nil {
			continue
		}
		member := snapshot.Members[id]
		if member == nil {
			return
		}
		p = snapshot.MemberState(id)
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

// agentsStatus is the status row's word on the visible workspace's agents,
// shown whenever it has any: "Agents · 1 needs approval · 2 running · 3
// finished", zero counts omitted, and its colored form. Coordination
// decisions stay in the swarm views; a paused parent, the swarm's own
// unfinished business, follows the counts. Empty when there is neither.
// Runs on the event loop with the visible model lock held.
func (r *managedREPL) agentsStatus() (text, styled string) {
	running, approvals, _, finished := r.agentCountsFor(r.visibleTab())
	var raw, rendered []string
	add := func(part, color string) {
		raw = append(raw, part)
		rendered = append(rendered, style.Styled(part, color, ""))
	}
	if approvals+running+finished > 0 {
		add("Agents", "muted")
	}
	if approvals > 0 {
		add(needsLabel(approvals, "approval"), "active")
	}
	if running > 0 {
		add(fmt.Sprintf("%d running", running), "run")
	}
	if finished > 0 {
		add(fmt.Sprintf("%d finished", finished), "muted")
	}
	if root := r.rootTab(r.visibleTab()); root != nil {
		if p, ok := r.parentPresentation(root); ok && tabHasSwarm(root) && p.Lifecycle == swarm.LifecyclePaused {
			add("swarm "+p.Display, "active")
		}
	}
	const sep = " · "
	return strings.Join(raw, sep), strings.Join(rendered, style.Styled(sep, "muted", ""))
}
