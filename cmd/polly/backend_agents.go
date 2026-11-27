package main

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// The agents of a backend's session. The backend runs its children itself;
// polly shows them from what it says: the rows `agents` gives of a root's
// children (the Agents inspector), and, while a turn runs, its children's
// events, which say whose spawn_agent row each is.

// remoteAgentRoster is a root tab's children, as their rows last said. It
// is owned by the event loop; a refresh runs in the background and posts.
type remoteAgentRoster struct {
	since     uint64
	rows      map[control.ID]control.Row
	order     []control.ID
	loading   bool
	refreshAt time.Time
	// finals are the children that ended, whose final row was read.
	finals map[control.ID]bool
}

// remoteRosterEvery is how often a root's children are asked after while
// something of them may be moving, and remoteRosterIdle when nothing is.
const (
	remoteRosterEvery = time.Second
	remoteRosterIdle  = 10 * time.Second
)

func (r *remoteAgentRoster) running() bool {
	for _, row := range r.rows {
		if row.Running() {
			return true
		}
	}
	return false
}

// refreshRemoteAgents asks the backend for each backend root tab's children,
// as often as they may be moving.
func (r *managedREPL) refreshRemoteAgents() {
	if r.work == nil {
		return
	}
	for _, tab := range r.tabs {
		if tab.state == nil || tab.state.remote == nil || r.rootTab(tab) != tab {
			continue
		}
		roster := tab.remoteAgents
		if roster == nil {
			roster = &remoteAgentRoster{rows: map[control.ID]control.Row{}, finals: map[control.ID]bool{}}
			tab.remoteAgents = roster
		}
		if roster.loading || time.Now().Before(roster.refreshAt) || tab.state.remote.session.Pending() {
			continue
		}
		tab.model.mu.Lock()
		busy := tab.model.busy
		tab.model.mu.Unlock()
		if i := r.workspace().inspector; i.open && i.target.kind == agentsViewKind {
			busy = true
		}
		busy = busy || roster.running()
		every := remoteRosterIdle
		if busy {
			every = remoteRosterEvery
		}
		roster.loading = true
		roster.refreshAt = time.Now().Add(every)
		state, id, since := tab.state, tab.state.remote.session.ID(), roster.since
		var finals []control.ID
		for _, childID := range roster.order {
			if row, ok := roster.rows[childID]; (!ok || row.Running()) && !roster.finals[childID] {
				finals = append(finals, childID)
			}
		}
		if !r.background(func() {
			rows, over, err := state.remote.backend.childRows(r.work.ctx, id, since, finals)
			r.postUI(r.work.ctx, func() {
				roster.loading = false
				if err != nil || !slices.Contains(r.tabs, tab) || tab.state != state {
					return
				}
				roster.since = rows.Ver
				if len(rows.IDs) > 0 {
					roster.order = rows.IDs
				}
				for _, row := range rows.Rows {
					roster.rows[row.ID] = row
				}
				for _, row := range over {
					roster.rows[row.ID] = row
					roster.finals[row.ID] = true
				}
			})
		}) {
			roster.loading = false
		}
	}
}

// childRows asks for a window of a root's latest children, and the final
// rows of those that ended since they were last seen running.
func (b *remoteBackend) childRows(ctx context.Context, parent control.ID, since uint64, ended []control.ID) (control.Rows, []control.Row, error) {
	c, err := b.storeClient(ctx)
	if err != nil {
		return control.Rows{}, nil, err
	}
	offset := int64(-control.AgentsMost)
	rows, err := c.Agents(ctx, control.AgentsReq{Parent: parent, Offset: &offset, Limit: control.AgentsMost, Since: since})
	if err != nil {
		return control.Rows{}, nil, err
	}
	var finals []control.Row
	over := append(slices.Clone(rows.Over), ended...)
	slices.Sort(over)
	over = slices.Compact(over)
	if len(over) > control.AgentsMost {
		over = over[:control.AgentsMost]
	}
	if len(over) > 0 {
		if byID, err := c.Agents(ctx, control.AgentsReq{IDs: over}); err == nil {
			for _, row := range byID.Rows {
				if !row.Running() {
					finals = append(finals, row)
				}
			}
		}
	}
	return rows, finals, nil
}

// remoteRowStatus is a child row's status as the agents list shows one.
func remoteRowStatus(row control.Row) string {
	switch row.State {
	case control.StateRunning:
		parts := []string{}
		if row.Phase != "" {
			parts = append(parts, row.Phase)
		}
		if row.Activity != "" {
			parts = append(parts, style1Line(row.Activity))
		}
		if len(parts) == 0 {
			return "running"
		}
		return strings.Join(parts, " · ")
	case control.StateCancelled:
		return "canceled"
	case "":
		return "starting"
	}
	return row.State
}

func style1Line(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 80 {
		line = line[:79] + "…"
	}
	return line
}

// remoteAgentEntries are the Agents inspector's rows of a backend root's
// children.
func remoteAgentEntries(root *replTab) map[string]agentsInspectorEntry {
	entries := map[string]agentsInspectorEntry{}
	if root == nil || root.remoteAgents == nil {
		return entries
	}
	for id, row := range root.remoteAgents.rows {
		label := row.Label
		if label == "" {
			label = "agent " + id[:8]
		}
		active := row.Running()
		entries[id] = agentsInspectorEntry{
			target:  sessions.ViewTarget{ID: id},
			label:   label,
			status:  remoteRowStatus(row),
			warn:    row.State == control.StateFailed,
			active:  active,
			history: !active,
		}
	}
	return entries
}

// stopRemoteAgent stops a backend child's run alone: its parent reads that
// it was stopped, and goes on.
func (r *managedREPL) stopRemoteAgent(target viewTarget) bool {
	root := r.rootTab(r.visibleTab())
	if root == nil || root.state == nil || root.state.remote == nil || root.remoteAgents == nil {
		return false
	}
	id := target.session.ID
	if _, ok := root.remoteAgents.rows[id]; !ok || id == "" {
		return false
	}
	backend, model := root.state.remote.backend, r.model
	r.background(func() {
		c, err := backend.storeClient(r.work.ctx)
		if err == nil {
			_, err = c.Stop(r.work.ctx, control.StopReq{Agent: id})
		}
		if err != nil {
			model.mu.Lock()
			model.appendNoticeLine("stop agent: " + err.Error())
			model.mu.Unlock()
		}
	})
	return true
}

// remoteAgentLinker is the part of a turn's UI that links a backend child
// to the spawn_agent row that made it, and shows what it does.
type remoteAgentLinker interface {
	remoteAgentActivity(callID, session, live string)
}

func (t *gotuiTurnUI) remoteAgentActivity(callID, session, live string) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if !t.activeLocked() {
		return
	}
	m := t.model
	record, row := m.toolDisclosureRowForCall(callID)
	if row == nil || row.agent == nil {
		return
	}
	if session != "" {
		row.agent.session = session
	}
	row.agent.live = live
	m.refreshAgentRecord(record)
}

// remoteChildren follows a backend turn's children: which spawn_agent call
// made each (by its label, else in spawn order), and what each does.
type remoteChildren struct {
	calls   []messages.ChatMessageToolCall
	byChild map[control.ID]string
	taken   map[string]bool
	labels  map[string]string
}

func newRemoteChildren() *remoteChildren {
	return &remoteChildren{byChild: map[control.ID]string{}, taken: map[string]bool{}, labels: map[string]string{}}
}

// spawned notes a spawn_agent call as it starts.
func (c *remoteChildren) spawned(call messages.ChatMessageToolCall) {
	if call.Name != subagent.ToolName {
		return
	}
	c.calls = append(c.calls, call)
	row := &toolDisclosureRow{}
	row.setCall(call)
	if row.agent != nil {
		c.labels[call.ID] = row.agent.label
	}
}

// callOf is the spawn_agent call that made child, labeled label.
func (c *remoteChildren) callOf(child control.ID, label string) string {
	if call, ok := c.byChild[child]; ok {
		return call
	}
	pick := ""
	for _, call := range c.calls {
		if c.taken[call.ID] {
			continue
		}
		if label != "" && c.labels[call.ID] == label {
			pick = call.ID
			break
		}
		if pick == "" {
			pick = call.ID
		}
	}
	if pick != "" {
		c.byChild[child] = pick
		c.taken[pick] = true
	}
	return pick
}

// event shows a child's event on its row.
func (c *remoteChildren) event(ui TurnUI, ev control.Event) {
	linker, ok := ui.(remoteAgentLinker)
	if !ok || ev.Parent == "" {
		return
	}
	call := c.callOf(ev.Agent, ev.Label)
	if call == "" {
		return
	}
	switch {
	case ev.Tool != nil && ev.Tool.State == "start":
		live := ev.Tool.Name
		if ev.Tool.Summary != "" {
			live += " " + style1Line(ev.Tool.Summary)
		}
		linker.remoteAgentActivity(call, "", live)
	case ev.Notice != nil && ev.Notice.Kind == "retry" && ev.Notice.Waiting:
		linker.remoteAgentActivity(call, "", "retrying")
	}
}

// result links a spawn_agent call's row to the session its result names.
func (c *remoteChildren) result(ui TurnUI, call messages.ChatMessageToolCall, content string) {
	if call.Name != subagent.ToolName {
		return
	}
	if linker, ok := ui.(remoteAgentLinker); ok {
		if name := control.SpawnedSession(content); name != "" {
			linker.remoteAgentActivity(call.ID, name, "")
		}
	}
}
