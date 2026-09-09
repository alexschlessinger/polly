package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
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
	current string
	rows    []sessionsPickerRow
	infos   map[string]sessions.SessionSummary
	// settled remembers how an agent that ran here ended, with its time,
	// after its tab has closed and only the store's outcome remains.
	settled  map[string]string
	listedAt time.Time
	listing  bool
}

type sessionsPickerRow struct {
	name  string
	depth int
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
	p := &sessionsPicker{current: r.visibleTab().name, infos: make(map[string]sessions.SessionSummary), settled: make(map[string]string), listedAt: time.Now()}
	infos := make([]sessions.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.Metadata != nil {
			infos = append(infos, summary)
			p.infos[summary.Metadata.Name] = summary
		}
	}
	// Agents nest under the session that spawned them, collapsed until the
	// parent is expanded with Right, so a fan-out never buries the list.
	metas := make([]*sessions.Metadata, len(infos))
	for i, summary := range infos {
		metas[i] = summary.Metadata
	}
	r.syncWorkspaces()
	workspaceIndex := func(node sessionTreeNode) (int, bool) {
		for n, workspace := range r.workspaces {
			if workspace.name == infos[node.Index].Metadata.Name {
				return n, true
			}
		}
		return 0, false
	}
	priority := func(node sessionTreeNode) int {
		tab := r.tabIndexOf(infos[node.Index].Metadata.Name)
		if tab < 0 {
			return 2
		}
		switch v := r.peekTabActivity(r.tabs[tab]); {
		case v == "approval needed":
			return 0
		case v != "" && v != "done" && v != "failed" && v != "incomplete":
			return 1
		}
		return 2
	}
	for _, node := range orderSessionGroups(sessionTree(metas), workspaceIndex, priority) {
		p.rows = append(p.rows, sessionsPickerRow{name: infos[node.Index].Metadata.Name, depth: node.Depth})
	}
	if len(p.rows) == 0 {
		r.model.appendNoticeLine("No saved sessions")
		return
	}
	target := p.current
	if preferred != "" {
		target = preferred
	}
	for _, row := range p.rows {
		info := p.infos[row.name].Metadata
		if row.name == target && row.depth > 0 {
			r.expandPickerParent(info.Parent)
		}
		// An open workspace with agents running here shows them without a keypress.
		if row.depth == 0 && r.tabIndexOf(row.name) >= 0 && r.hasLiveAgents(r.tabs[r.tabIndexOf(row.name)]) {
			r.expandPickerParent(row.name)
		}
	}
	m := &replModal{
		title: "Sessions", expanded: r.pickerExpanded,
		width: 64, maxRows: 14, showCount: true,
		onSubmit: func(name string) {
			if name == "" || name == p.current {
				return
			}
			// A session open in one of this polly's tabs is reached by
			// showing that tab. One leased by another polly cannot be
			// opened here: Acquire would wait out the lease timeout and
			// then fail. Refuse up front.
			if info, ok := p.infos[name]; ok && info.InUse && r.tabIndexOf(name) < 0 && info.Metadata.Parent == "" {
				r.model.appendErrorLine(name + " is open in another polly")
				return
			}
			r.requestOpenLocked(name)
		},
		onRename: r.openSessionRenameInput,
	}
	m.picker = p
	m.refresh = func() { r.refreshSessionsPicker(p, m) }
	m.items = r.sessionsPickerItems(p)
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
		store := r.state.sessionStore
		if !r.background(func() {
			summaries, err := store.ListSummaries(r.work.ctx)
			r.postUI(context.Background(), func() {
				p.listing = false
				p.listedAt = time.Now()
				if err == nil && r.model.modal == m {
					p.merge(summaries)
				}
			})
		}) {
			p.listing = false
		}
	}
	selected := ""
	if items := m.filteredItems(); len(items) > 0 {
		selected = items[max(0, min(m.selected, len(items)-1))].value
	}
	m.items = r.sessionsPickerItems(p)
	if m.nested() {
		m.width = sessionsPickerNestedWidth
	}
	for i, item := range m.filteredItems() {
		if item.value == selected {
			m.selected = i
			break
		}
	}
}

// merge takes a fresh listing into the picker: known rows keep their place,
// a session spawned since joins the end of its parent's agents, and one the
// store no longer has leaves. Runs on the event loop.
func (p *sessionsPicker) merge(summaries []sessions.SessionSummary) {
	seen := make(map[string]bool, len(summaries))
	for _, summary := range summaries {
		if summary.Metadata == nil {
			continue
		}
		name := summary.Metadata.Name
		seen[name] = true
		_, known := p.infos[name]
		p.infos[name] = summary
		if known {
			continue
		}
		at, depth := len(p.rows), 0
		if parent := summary.Metadata.Parent; parent != "" {
			for i, row := range p.rows {
				if row.name != parent {
					continue
				}
				at, depth = i+1, row.depth+1
				for at < len(p.rows) && p.rows[at].depth > row.depth {
					at++
				}
				break
			}
		}
		p.rows = slices.Insert(p.rows, at, sessionsPickerRow{name: name, depth: depth})
	}
	p.rows = slices.DeleteFunc(p.rows, func(row sessionsPickerRow) bool {
		if seen[row.name] {
			return false
		}
		delete(p.infos, row.name)
		return true
	})
}

// sessionsPickerItems renders the picker's rows as they stand now: name,
// age, length, then how the session is placed and what it is doing, and for
// an agent the brief it was given. Caller holds the visible model's lock.
func (r *managedREPL) sessionsPickerItems(p *sessionsPicker) []replModalItem {
	nameWidth, lengthWidth := 0, 0
	for _, row := range p.rows {
		summary := p.infos[row.name]
		nameWidth = max(nameWidth, rw.StringWidth(sessionTreeName(summary.Metadata, row.depth)))
		lengthWidth = max(lengthWidth, rw.StringWidth(formatSessionMessageCount(summary.MessageCount)))
	}
	nameWidth = min(nameWidth, 24)
	children := make(map[string]int)
	for _, row := range p.rows {
		if row.depth > 0 {
			children[p.infos[row.name].Metadata.Parent]++
		}
	}
	items := make([]replModalItem, 0, len(p.rows))
	for _, row := range p.rows {
		summary := p.infos[row.name]
		info := summary.Metadata
		name := truncate(sessionTreeName(info, row.depth), nameWidth)
		age := formatCompactDuration(time.Since(info.LastUsed))
		length := formatSessionMessageCount(summary.MessageCount)
		nameColumn := fmt.Sprintf("%-*s", nameWidth, name)
		ageColumn := fmt.Sprintf("%4s", age)
		lengthColumn := fmt.Sprintf("%*s", lengthWidth, length)
		label := nameColumn + "  " + ageColumn + "  " + lengthColumn
		display := styleEscape(nameColumn) + "  " + styled(ageColumn, "muted", "") + "  " + styled(lengthColumn, "muted", "")
		selectedDisplay := styled(nameColumn, "accent", "bold") + "  " + styled(ageColumn, "muted", "") + "  " + styled(lengthColumn, "muted", "")
		mark, color, status := "", "", ""
		tab := r.tabIndexOf(info.Name)
		if tab >= 0 {
			status = r.tabActivityLine(r.tabs[tab])
			if row.depth > 0 && status != "" {
				p.settled[info.Name] = status
			}
		}
		switch {
		case info.Name == p.current:
			mark, color = "current", "accent"
		case tab >= 0:
			mark, color = "active agent", "ok"
			for n, workspace := range r.workspaces {
				if workspace == r.tabs[tab] {
					mark = fmt.Sprintf("workspace %d", n+1)
					break
				}
			}
			if row.depth > 0 && status != "" {
				// The status says it is an agent doing something here.
				mark = ""
			}
		case summary.InUse:
			mark, color = "in use", "active"
		default:
			if row.depth > 0 {
				status = p.settledStatus(info)
			}
		}
		var plain, shown []string
		if mark != "" {
			plain, shown = append(plain, mark), append(shown, styled(mark, color, ""))
		}
		if status != "" {
			plain, shown = append(plain, status), append(shown, styled(status, "muted", ""))
		}
		if len(plain) > 0 {
			label += "  " + strings.Join(plain, " · ")
			display += "  " + strings.Join(shown, styled(" · ", "muted", ""))
			selectedDisplay += "  " + strings.Join(shown, styled(" · ", "muted", ""))
		}
		item := replModalItem{
			label: label, value: info.Name, display: display, selectedDisplay: selectedDisplay,
			children: children[info.Name],
		}
		if row.depth > 0 {
			item.parent = info.Parent
			if brief := strings.Join(strings.Fields(info.Description), " "); brief != "" {
				item.searchText = brief
				item.display += "  " + styled(styleEscape(brief), "muted", "")
				item.selectedDisplay += "  " + styled(styleEscape(brief), "muted", "")
			}
		} else if item.children > 0 {
			item.nestDetail = r.agentsSummary(info.Name)
		}
		items = append(items, item)
	}
	return items
}

// settledStatus is how a closed agent ended: as it ran here, with its
// time, or else as the store recorded it.
func (p *sessionsPicker) settledStatus(info *sessions.Metadata) string {
	if status, ok := p.settled[info.Name]; ok {
		return status
	}
	if info.SpawnOutcome == "" {
		return ""
	}
	return spawnOutcomeStatus(info.SpawnOutcome)
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
	return tab.agentStatus, 0
}

// agentsSummary counts what a workspace's agents here are doing, for the
// row that lists them: "1 running", "2 need approval".
func (r *managedREPL) agentsSummary(name string) string {
	tab := r.tabIndexOf(name)
	if tab < 0 {
		return ""
	}
	running, approvals := r.agentCountsFor(r.tabs[tab])
	var parts []string
	if approvals > 0 {
		verb := "need"
		if approvals == 1 {
			verb = "needs"
		}
		parts = append(parts, fmt.Sprintf("%d %s approval", approvals, verb))
	}
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", running))
	}
	return strings.Join(parts, " · ")
}

// agentCountsFor counts the agents of a workspace that are running here and
// that wait on an approval.
func (r *managedREPL) agentCountsFor(root *replTab) (running, approvals int) {
	for _, child := range r.tabs {
		if child == root || r.rootTab(child) != root {
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
	return running, approvals
}

// agentsStatus is the status row's word on the visible workspace's agents
// here: the approvals they wait on first, else how many run. Empty when
// none does. Runs on the event loop with no model lock held.
func (r *managedREPL) agentsStatus() (text, color string) {
	running, approvals := r.agentCountsFor(r.visibleTab())
	switch {
	case approvals > 1:
		return fmt.Sprintf("%d need approval", approvals), "active"
	case approvals == 1:
		return "1 needs approval", "active"
	case running > 0:
		return turnAgentLabel(running) + " running", "run"
	}
	return "", ""
}
