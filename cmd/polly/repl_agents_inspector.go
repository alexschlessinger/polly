package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
)

// agentSection is where the list files an agent, in settlement order: waiting
// on a person, working, or done with nothing pending. Only Done folds.
type agentSection int

const (
	agentWaiting agentSection = iota
	agentWorking
	agentDone
)

type agentsInspectorEntry struct {
	target  sessions.ViewTarget
	label   string
	section agentSection
	// status is the word after the name, only where the section does not
	// say it: why a waiting agent waits, or a working agent's live phase.
	status string
	// warn colors a live status's clock (see agentRowStatus).
	warn bool
	// suffix qualifies a done agent: stopped, canceled or deferred.
	suffix string
	// why is the one line under a waiting agent that explains the wait, or
	// under a working agent the warning its highlighted clock stands for.
	why     string
	message *agentMessagePreview
	// progress is the latest turn's summary when the entry's own display
	// supplies it; swarm members are projected (see agentProgress).
	progress *agentProgressSummary
	ran      time.Duration
	hasRan   bool
	// ended orders done agents, newest finish first.
	ended time.Time
}

const agentsDoneAction = "agents-done"

// agentDetailIndent sets an agent's detail rows two cells in from its row.
const agentDetailIndent = "    "

type agentsInspectorState struct {
	order    []string
	entries  map[string]agentsInspectorEntry
	selected string // member ID, or agentsDoneAction for the section heading
	// doneCollapsed folds the Done section behind its heading.
	doneCollapsed bool
	// full is the Ctrl-O depth: every agent shows its latest call and text.
	full     bool
	progress map[string]*agentProgress
	width    int // fences progress projections across pane resizes
	// Cached rows are invalidated by display facts, live activity, approval,
	// navigation/depth, progress completion, theme, or a visible clock tick.
	renderKey        string
	progressRevision uint64
	progressWindow   map[string]bool
	progressOrder    []string
	// lanes is this paint's timeline, nil where the pane is too narrow or
	// nothing has run; laneRows are the rows its native images fill.
	lanes      *agentLanes
	laneRows   []agentLaneRow
	laneImages agentLaneImages
	// scrub is Ctrl-T: detail rows say what each agent was doing at cursor.
	scrub  bool
	cursor time.Time
}

const agentsTimelineAction = "agents-timeline"

func (list *agentsInspectorState) visible(id string) bool {
	e, exists := list.entries[id]
	return exists && (e.section != agentDone || !list.doneCollapsed)
}

// summary is the agent's projected latest turn, or the one its own display
// supplied.
func (list *agentsInspectorState) summary(id string) agentProgressSummary {
	if p := list.progress[id]; p != nil && len(p.fields) > 0 {
		return p.agentProgressSummary
	}
	if e := list.entries[id]; e.progress != nil {
		return *e.progress
	}
	return agentProgressSummary{}
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
	approvals := root.model.approvalRequesters()
	root.model.mu.Unlock()
	now := time.Now()
	if snapshot := root.displaySnapshot(); snapshot != nil {
		parent := root.viewID()
		messages := latestAgentMessages(snapshot, parent)
		decisions := agentDecisions(snapshot, parent)
		for id, member := range snapshot.Members {
			e := swarmAgentEntry(snapshot, member, root.swarmActivities[id], decisions[id], approvals[id], now)
			e.message = messages[id]
			entries[id] = e
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
		if snapshot := root.displaySnapshot(); snapshot != nil && snapshot.Members[id] != nil {
			continue // Swarm state owns this member's status.
		}
		tab.model.mu.Lock()
		status := modelTabActivity(tab.model)
		label := tab.model.status.displayLabel()
		progress := tab.model.agentProgressSummary(width)
		tab.model.mu.Unlock()
		if label == "" {
			label = tab.name
		}
		e := agentsInspectorEntry{target: tabViewTarget(tab).session, label: label, progress: &progress}
		switch {
		case status == "approval needed":
			e.section, e.status = agentWaiting, "approval"
		case status == "failed" || status == "paused" || status == "incomplete":
			e.section, e.status = agentWaiting, status
		case tabActivityBusy(status):
			e.section, e.status = agentWorking, status
		default:
			e.section = agentDone
		}
		entries[id] = e
	}
	return entries
}

// approvalRequesters are the agents with an approval pending, shown or
// queued. Caller holds m.mu.
func (m *replModel) approvalRequesters() map[string]bool {
	approvals := map[string]bool{}
	if a := m.approval; a != nil {
		approvals[a.requester] = true
	}
	for _, a := range m.approvalQueue {
		approvals[a.requester] = true
	}
	return approvals
}

// agentDecisions keys the parent's decisions by the member they concern. An
// unread reply is the parent's to read, not a wait of the member's.
func agentDecisions(view swarm.DisplayView, parent string) map[string]*swarm.DecisionItem {
	return view.DisplaySnapshot(parent).FirstMemberDecision
}

// swarmAgentEntry files a member by the swarm's own facts: an approval
// outranks everything; a request the parent has not answered waits even
// while its sender is parked; busy and delivering members work; other
// decisions the parent must take, and open work no execution advances,
// wait; the rest are done, qualified by how they ended.
func swarmAgentEntry(view swarm.DisplayView, m *swarm.Member, a swarm.LiveActivity, decision *swarm.DecisionItem, approval bool, now time.Time) agentsInspectorEntry {
	s := view.DisplaySnapshot("")
	p := swarmMemberActivity(s, m)
	e := agentsInspectorEntry{target: sessions.ViewTarget{ID: m.ID, Name: m.Name}, label: swarmMemberLabel(s, m)}
	if execution := s.Executions[m.Execution]; execution != nil {
		e.ran, e.hasRan = executionRunTime(execution, now)
		e.ended = execution.Ended
	}
	switch {
	case approval:
		e.section, e.status = agentWaiting, "approval"
	case decision != nil && decision.Kind == swarm.KindMail:
		e.section, e.status = agentWaiting, "reply" // the message row carries the request
	case p.Busy || p.Delivering:
		e.section = agentWorking
		e.status, e.warn = agentRowStatus(s, m, a, now)
		if e.warn {
			e.why = agentLiveExtras(a, now)
		}
		if e.status == "" {
			switch {
			case p.Delivering:
				e.status = "delivering"
			case p.Lifecycle == swarm.LifecycleWaiting:
				e.status = "parked"
			case p.Outcome == "queued":
				e.status = "queued"
			}
		}
	case decision != nil || p.Attention:
		e.section = agentWaiting
		e.status, e.why = agentWaitWord(s, m, p, decision)
	default:
		e.section = agentDone
		switch {
		case p.Control == swarm.MemberControlStopped:
			e.suffix = "stopped"
		case p.TaskStatus == "canceled":
			e.suffix = "canceled"
		case p.Deferred:
			e.suffix = "deferred"
		}
	}
	return e
}

// agentWaitWord names what a waiting member waits for and, where the row
// alone does not say it, the one line that explains it.
func agentWaitWord(view swarm.DisplayView, m *swarm.Member, p swarm.AgentPresentation, d *swarm.DecisionItem) (word, why string) {
	s := view.DisplaySnapshot("")
	e := s.Executions[m.Execution]
	switch {
	case p.Control == swarm.MemberControlStopped:
		return "stopped", ""
	case e != nil && e.Status == "failed":
		return "failed", agentFailureReason(s, m)
	case p.Lifecycle == swarm.LifecyclePaused:
		if d != nil && d.Kind == swarm.KindTask && d.Why != "" {
			return "paused", d.Why
		}
		return "paused", agentFailureReason(s, m)
	case p.TaskStatus == "awaiting review":
		return "review", taskResultLine(s.Tasks[m.Task])
	case p.TaskStatus == "pending":
		if d != nil {
			return "pending", d.Action
		}
		return "pending", ""
	}
	word = p.TaskStatus
	if word == "" {
		word = p.Detail
	}
	if word == "" {
		word = "waiting"
	}
	if d != nil {
		why = d.Why
	}
	return word, why
}

// taskResultLine is the first line of prose in a result awaiting review,
// when the result is text; a result that is only headings gives its first.
func taskResultLine(t *swarm.Task) string {
	if t == nil {
		return ""
	}
	text, ok := t.Result.(string)
	if !ok {
		return ""
	}
	heading := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			if heading == "" {
				heading = strings.TrimLeft(line, "# ")
			}
		default:
			return style.SanitizeImageText(line)
		}
	}
	return style.SanitizeImageText(heading)
}

// swarmWorkflowNames heads the list with the workflows its agents belong
// to, oldest first; agents without workflows leave no header.
func swarmWorkflowNames(view swarm.DisplayView) string {
	if view == nil {
		return ""
	}
	s := view.DisplaySnapshot("")
	if s == nil {
		return ""
	}
	named := map[string]bool{}
	for _, m := range s.Members {
		if id := s.ExecutionWorkflow(s.Executions[m.Execution]); id != "" && s.Workflows[id] != nil {
			named[id] = true
		} else if m.Controller != "" && s.Workflows[m.Controller] != nil {
			named[m.Controller] = true
		}
	}
	ids := slices.Collect(maps.Keys(named))
	slices.SortFunc(ids, func(a, b string) int {
		if n := s.Workflows[a].Started.Compare(s.Workflows[b].Started); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	var names []string
	for _, id := range ids {
		name := strings.Join(strings.Fields(style.SanitizeImageText(s.Workflows[id].Name)), " ")
		if name == "" {
			name = id
		}
		names = append(names, name)
	}
	return strings.Join(names, " · ")
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
	root := r.rootTab(r.visibleTab())
	snapshot := root.displaySnapshot()
	clock := int64(0)
	if root.swarmActive {
		clock = time.Now().Unix()
	}
	root.model.mu.Lock()
	approvalKey := fmt.Sprintf("%p", root.model.approval)
	for _, a := range root.model.approvalQueue {
		approvalKey += fmt.Sprintf("/%p", a)
	}
	baseDir := root.model.toolBaseDir
	root.model.mu.Unlock()
	native := r.agentLanesNative(geometry)
	if width >= agentLanesMinWidth {
		r.readAgentTimelines(root, snapshot)
	}
	key := fmt.Sprintf("%p/%d/%d/%s/%d/%t/%t/%s/%d/%d/%t/%t/%t/%d/%d", snapshot, root.swarmActivityRevision, clock, approvalKey, width, list.doneCollapsed, list.full, list.selected, list.progressRevision, style.Epoch(), snapshot == nil && root.swarmLoading, native, list.scrub, list.cursor.UnixMilli(), root.agentTimeline.revision)
	// Legacy child tabs have their own mutable display state, not the native
	// projection token. Keep their existing render path until their state lands.
	for _, tab := range r.tabs {
		if tab != root && r.rootTab(tab) == root && (snapshot == nil || snapshot.Members[tab.viewID()] == nil) {
			key = ""
			break
		}
	}
	if key != "" && key == list.renderKey && i.current != nil && i.current.geometry == geometry {
		r.setAgentProgressWindow(list, state.top, i.current.agentsActions)
		r.refreshAgentProgress(list, width)
		return
	}
	list.renderKey = key
	entries := r.agentsInspectorEntries(width)
	// Listed agents keep their place; new ones join by name. Sections are
	// read off the entries at each paint, so an agent moves between them
	// without reordering the others.
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
	slices.SortFunc(added, func(a, b string) int {
		if n := strings.Compare(entries[a].label, entries[b].label); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	list.order, list.entries = append(order, added...), entries
	sections := map[agentSection][]string{}
	for _, id := range list.order {
		sections[entries[id].section] = append(sections[entries[id].section], id)
	}
	// Done lists the newest finish first; finishes without a time keep
	// their listing order after them.
	slices.SortStableFunc(sections[agentDone], func(a, b string) int {
		return entries[b].ended.Compare(entries[a].ended)
	})
	if e, exists := entries[list.selected]; exists {
		if e.section == agentDone && list.doneCollapsed {
			list.selected = agentsDoneAction
		}
	} else if list.selected != agentsDoneAction || len(sections[agentDone]) == 0 {
		list.selected = ""
	}
	list.lanes = nil
	if snapshot != nil && width >= agentLanesMinWidth {
		done := map[string]bool{}
		for _, id := range sections[agentDone] {
			done[id] = true
		}
		// Every listed agent sets the axis, folded ones too, so folding Done
		// never rescales the lanes.
		if lanes := buildAgentLanes(snapshot, &root.agentTimeline, list.order, done, time.Now()); lanes != nil {
			lanes.cols, lanes.native, lanes.baseDir = width-agentLaneOffset, native, baseDir
			if list.scrub {
				if list.cursor.IsZero() || list.cursor.After(lanes.to) {
					list.cursor = lanes.to
				}
				if list.cursor.Before(lanes.from) {
					list.cursor = lanes.from
				}
				lanes.scrub, lanes.cursor = true, list.cursor
			}
			list.lanes = lanes
		}
	}
	if list.lanes == nil {
		list.scrub = false
	}
	rows, actions := list.rows(sections, swarmWorkflowNames(snapshot), width)
	r.setAgentProgressWindow(list, state.top, actions)
	r.refreshAgentProgress(list, width)
	if len(rows) == 0 {
		text := "No agents yet"
		if r.rootTab(r.visibleTab()).swarmLoading {
			text = "Loading agents…"
		}
		rows = append(rows, style.Styled(text, "muted", ""))
		actions = append(actions, "")
	}
	body := strings.Join(rows, "\n")
	if i.current != nil && i.current.revision == body && i.current.geometry == geometry && slices.Equal(i.current.agentsActions, actions) {
		// Native lanes repaint without changing a cell.
		i.current.agentsLanes, i.current.agentsLaneRows = list.lanes, list.laneRows
		return
	}
	model := newReplModel()
	model.appendLine(body)
	model.transcriptRows(width)
	i.current = &viewInstance{target: i.target, view: conversationView{}, model: model, revision: body, geometry: geometry, agentsActions: actions, agentsLanes: list.lanes, agentsLaneRows: list.laneRows}
	state.follow = false
	state.lastRows = len(rows)
}

// rows paints the sections: a heading with its count, one row per agent,
// and the detail rows the depth allows. Every row of an agent opens it;
// headings open nothing, except Done, which folds. A blank row separates
// sections, never agents.
func (list *agentsInspectorState) rows(sections map[agentSection][]string, header string, width int) (rows, actions []string) {
	entries := list.entries
	if len(sections[agentWaiting])+len(sections[agentWorking])+len(sections[agentDone]) == 0 {
		return nil, nil
	}
	// Statuses share one left-aligned column as wide as the widest of them,
	// so names keep their truncation while live clocks tick; run times are
	// right-aligned in a column of their own, which a narrow pane drops.
	statusCap := min(64, max(1, (width-5)/2))
	column, runColumn := 0, 0
	for _, section := range []agentSection{agentWaiting, agentWorking, agentDone} {
		for _, id := range sections[section] {
			if !list.visible(id) {
				continue
			}
			e := entries[id]
			column = max(column, rw.StringWidth(rw.Truncate(e.status, statusCap, "…")))
			if e.hasRan {
				runColumn = max(runColumn, rw.StringWidth(activityElapsed(e.ran)))
			}
		}
	}
	if width < 52 {
		runColumn = 0
	}
	right := 0
	if runColumn > 0 {
		right = 2 + runColumn
	}
	body := max(1, width-2-right)
	add := func(row, action string) {
		rows = append(rows, row)
		actions = append(actions, action)
	}
	if header != "" {
		add(style.Styled(style.Escape(rw.Truncate(header, max(1, width), "…")), "muted", ""), "")
	}
	lanes := list.lanes
	list.laneRows = nil
	// A native image fills a blank row; the cell fallback draws the row.
	laneRow := func(id, cells, action string) {
		if lanes.native {
			list.laneRows = append(list.laneRows, agentLaneRow{row: len(rows), id: id})
			add(strings.Repeat(" ", width), action)
			return
		}
		add(cells, action)
	}
	if lanes != nil {
		if list.scrub {
			if lanes.native {
				list.laneRows = append(list.laneRows, agentLaneRow{row: len(rows)})
				for range agentRibbonRows {
					add(strings.Repeat(" ", width), agentsTimelineAction)
				}
			} else {
				add(lanes.ribbonRow(), agentsTimelineAction)
			}
		}
		add(lanes.axisRow(), agentsTimelineAction)
	}
	first := true
	for _, section := range []agentSection{agentWaiting, agentWorking, agentDone} {
		ids := sections[section]
		if len(ids) == 0 {
			continue
		}
		if !first {
			add("", "")
		}
		first = false
		switch section {
		case agentWaiting:
			add(style.Styled("Waiting · ", "muted", "")+style.Styled(fmt.Sprint(len(ids)), "active", ""), "")
		case agentWorking:
			add(style.Styled(fmt.Sprintf("Working · %d", len(ids)), "muted", ""), "")
		case agentDone:
			triangle, color := "▾", "muted"
			if list.doneCollapsed {
				triangle = "▸"
			}
			if list.selected == agentsDoneAction {
				color = "accent"
			}
			add(style.Styled(rw.Truncate(fmt.Sprintf("%s Done · %d", triangle, len(ids)), max(1, width), "…"), color, ""), agentsDoneAction)
			if list.doneCollapsed {
				continue
			}
		}
		for _, id := range ids {
			action := "agents-open:" + id
			add(list.row(id, body, column, statusCap, runColumn), action)
			if lanes == nil {
				for _, row := range list.detailRows(id, width) {
					add(row, action)
				}
				continue
			}
			laneRow(id, lanes.laneRow(id), action)
			if list.scrub {
				if row := lanes.scrubRow(id, width); row != "" {
					add(row, action)
				}
				continue
			}
			for _, row := range list.detailRows(id, width) {
				add(row, action)
			}
		}
	}
	return rows, actions
}

// row is one agent: its name (with how it ended, for a done agent), its
// status word where the section does not say it, and the right-aligned run
// time. Names take the status column when there is nothing to put in it.
func (list *agentsInspectorState) row(id string, body, column, statusCap, runColumn int) string {
	e := list.entries[id]
	prefix := "  "
	if list.selected == id {
		prefix = style.Styled("› ", "accent", "")
	}
	nameWidth, status := body, ""
	if e.status != "" {
		status = rw.Truncate(e.status, statusCap, "…")
		nameWidth = max(1, body-2-column)
	}
	suffix := ""
	if e.suffix != "" && nameWidth-rw.StringWidth(" · "+e.suffix) >= 4 {
		suffix = " · " + e.suffix
	}
	label := rw.Truncate(strings.Join(strings.Fields(e.label), " "), max(1, nameWidth-rw.StringWidth(suffix)), "…")
	color, counts := "code", ""
	if e.section == agentDone {
		color, counts = "", "muted"
	}
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(style.Styled(style.Escape(label), color, ""))
	b.WriteString(style.Styled(suffix, "muted", ""))
	b.WriteString(strings.Repeat(" ", max(0, nameWidth-rw.StringWidth(label)-rw.StringWidth(suffix))))
	if status != "" {
		statusColor := "muted"
		if e.section == agentWaiting {
			statusColor = "active"
		}
		b.WriteString("  " + rowStatusMarkup(style.Escape(status), statusColor, e.warn && status == e.status) + strings.Repeat(" ", max(0, column-rw.StringWidth(status))))
	}
	if runColumn > 0 {
		ran := ""
		if e.hasRan {
			ran = activityElapsed(e.ran)
		}
		b.WriteString("  " + strings.Repeat(" ", max(0, runColumn-rw.StringWidth(ran))) + style.Styled(ran, counts, ""))
	}
	return b.String()
}

// detailRows are the rows under an agent: its latest call, running or
// settled; why a waiting agent waits, or what a working agent's warned
// clock stands for; and its newest message. Every depth shows the last two.
// Abridged, the call appears under working agents, under an agent awaiting
// approval (it is the call in question), under a waiting agent with nothing
// else to say, and under the selected agent; the full depth shows it under
// every agent.
func (list *agentsInspectorState) detailRows(id string, width int) []string {
	e := list.entries[id]
	room := width - len(agentDetailIndent)
	call := ""
	if field, ok := list.summary(id).latest(); ok && room >= 2 {
		// A call fitted for another width is cut until its refit lands.
		if rw.StringWidth(field.raw) <= room {
			call = agentDetailIndent + field.rendered
		} else {
			call = agentDetailIndent + style.Styled(style.Escape(rw.Truncate(field.raw, room, "…")), "muted", "")
		}
	}
	why := ""
	if e.why != "" && room >= 2 {
		why = agentDetailIndent + style.Styled(style.Escape(rw.Truncate(strings.Join(strings.Fields(e.why), " "), room, "…")), "muted", "")
	}
	message := e.message.row(width)
	showCall := list.full || list.selected == id
	switch {
	case showCall:
	case e.section == agentWaiting:
		showCall = e.status == "approval" || why == "" && message == ""
	case e.section == agentWorking:
		showCall = true
	}
	var rows []string
	if showCall && call != "" {
		rows = append(rows, call)
	}
	if why != "" {
		rows = append(rows, why)
	}
	if message != "" {
		rows = append(rows, message)
	}
	return rows
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
	if action == agentsTimelineAction {
		r.toggleAgentScrub()
		return true
	}
	if action == agentsDoneAction {
		list.doneCollapsed = !list.doneCollapsed
		list.selected = agentsDoneAction
		return true
	}
	if !strings.HasPrefix(action, "agents-open:") {
		return false
	}
	id := strings.TrimPrefix(action, "agents-open:")
	entry := list.entries[id]
	if !list.visible(id) {
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
	if r.scrubAgents(key) {
		return true
	}
	switch key {
	case "<Left>":
		i := &r.workspace().inspector
		list := r.workspace().viewState(i.target).agents
		if list != nil && list.selected == agentsDoneAction && !list.doneCollapsed {
			return r.agentsInspectorAction(agentsDoneAction)
		}
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
	// An agent spans its row and detail rows; keys move by agent.
	var starts []int
	for n, action := range actions {
		// The time axis takes clicks, not the selection.
		if action != "" && action != agentsTimelineAction && (n == 0 || action != actions[n-1]) {
			starts = append(starts, n)
		}
	}
	if len(starts) == 0 {
		return true
	}
	selectedAction := "agents-open:" + list.selected
	if list.selected == agentsDoneAction {
		selectedAction = agentsDoneAction
	}
	pos := slices.IndexFunc(starts, func(n int) bool { return actions[n] == selectedAction })
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
		action := actions[starts[max(0, pos)]]
		if key == "<Right>" && action == agentsDoneAction && !list.doneCollapsed {
			return true
		}
		return r.agentsInspectorAction(action)
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
