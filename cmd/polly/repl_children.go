package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// Child tabs. A subagent the model spawns, or the user starts with /spawn,
// runs on a tab of its own nested under its parent's: an ordinary tab with
// its own session, turn, composer, and Esc, so it can be watched, steered,
// or canceled like any other. What differs is where its first reply goes.
// A blocking child (the default) answers the parent's spawn_agent call,
// whose disclosure row shows the child's progress meanwhile. A background
// child, or one whose call stopped waiting, reports to the store, addressed
// to the parent session rather than to the parent tab: the parent takes the
// reports waiting for it whenever it is open and idle, here or in a later
// polly, as one message that starts a parent turn. Only that first turn
// reports; later turns on the child are between the user and the child. A
// child nobody looked at closes once the parent turn that took its report
// settles, or, when its parent tab is gone, once it has reported.

// childReport is what a blocking spawn call receives when its child settles.
type childReport struct {
	result subagent.Result
	err    error
}

// spawnRequest is a /spawn typed on a tab, applied by the event loop.
type spawnRequest struct {
	parent *replTab
	req    subagent.Request
}

// descendsFrom reports whether ancestor is above t in the tab tree.
func (t *replTab) descendsFrom(ancestor *replTab) bool {
	for p := t.parent; p != nil; p = p.parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

// depth is how many parents t has.
func (t *replTab) depth() int {
	n := 0
	for p := t.parent; p != nil; p = p.parent {
		n++
	}
	return n
}

// signalName names the tab in notices for the visible tab, placing a child
// under its parent.
func (t *replTab) signalName() string {
	if t.parentName != "" {
		return t.name + " (agent of " + t.parentName + ")"
	}
	return t.name
}

// reportPollInterval is how often idle tabs look in the store for reports
// their children posted from elsewhere: a parent reopened here while its
// child works on in another polly hears from it within this.
const reportPollInterval = 15 * time.Second

// RunChild runs a child of this turn on a tab of its own (see childHost).
// Runs on the tool goroutine; the tab work happens on the event loop.
func (t *gotuiTurnUI) RunChild(ctx context.Context, req subagent.Request) (subagent.Result, error) {
	r := t.repl
	if r == nil || r.opener == nil || r.opener.spawn == nil {
		return subagent.Result{}, errNoChildHost
	}
	child, err := r.opener.spawn(ctx, t.state, req)
	if err != nil {
		return subagent.Result{}, err
	}
	var waiter chan childReport
	if !req.Background {
		waiter = make(chan childReport, 1)
	}
	callID := toolCallFrom(ctx).ID
	type outcome struct {
		tab *replTab
		err error
	}
	spawned := make(chan outcome, 1)
	r.uiTasks <- func() {
		w := waiter
		if ctx.Err() != nil {
			// The call stopped waiting before the tab existed; the child
			// runs on and reports the way a background child does.
			w = nil
		}
		tab, err := r.spawnChildTab(t.model, child, req, w, callID)
		spawned <- outcome{tab, err}
	}
	started := <-spawned
	if started.err != nil {
		return subagent.Result{}, started.err
	}
	tab := started.tab
	if req.Background {
		return subagent.Result{Session: tab.name, Started: true}, nil
	}
	select {
	case rep := <-waiter:
		return rep.result, rep.err
	case <-ctx.Done():
		r.uiTasks <- func() { r.orphanChild(tab) }
		return subagent.Result{Session: tab.name}, context.Cause(ctx)
	}
}

// spawnChildTab opens child as a tab under the tab that owns parentModel,
// starts its first turn on the brief, and, for a blocking spawn, points the
// parent's tool row at it so the row can show the child's progress. Runs on
// the event loop with no model lock held.
func (r *managedREPL) spawnChildTab(parentModel *replModel, child *conversationState, req subagent.Request, waiter chan childReport, callID string) (*replTab, error) {
	pi := r.tabIndexOfModel(parentModel)
	if pi < 0 || r.runTurn == nil {
		_ = child.Close()
		return nil, errors.New("the parent tab is closed")
	}
	parent := r.tabs[pi]
	name, m, err := r.newTabModel(child)
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	tab := &replTab{name: name, state: child, model: m, parent: parent, parentName: parent.name, report: &childTurnUI{}, waiter: waiter}
	if child.session != nil {
		tab.stopWatch = context.AfterFunc(child.session.Context(), r.wakeTabs)
	}
	// Children sit right after their parent, after any earlier siblings.
	at := pi + 1
	for at < len(r.tabs) && r.tabs[at].descendsFrom(parent) {
		at++
	}
	r.tabs = slices.Insert(r.tabs, at, tab)

	turn := managedTurnInput{displayText: req.Task, userMessage: messages.ChatMessage{Role: messages.MessageRoleUser, Content: req.Task}}
	m.mu.Lock()
	m.beginManagedTurn(turn)
	m.mu.Unlock()
	r.startManagedTurn(r.runCtx, tab, turn, r.runTurn)
	if callID != "" {
		parentModel.mu.Lock()
		parentModel.attachChildToTool(callID, m, name)
		parentModel.mu.Unlock()
	}
	r.model.mu.Lock()
	if waiter == nil {
		label := req.Label
		if label == "" {
			label = name
		}
		r.model.appendNoticeLine(fmt.Sprintf("agent %s started in tab %d · /tab %s", label, at+1, name))
	}
	r.model.mu.Unlock()
	return tab, nil
}

// deliverChildReport hands a settled child's first reply to whoever waits
// for it: the parent's blocking call, or else the store, for the parent
// session. Runs on the event loop with no model lock held.
func (r *managedREPL) deliverChildReport(ctx context.Context, tab *replTab, err error, runTurn turnRunner) {
	if tab.report == nil {
		return
	}
	rec := tab.report
	tab.report = nil
	res := subagent.Result{Session: tab.name}
	res.Text, res.InputTokens, res.OutputTokens = rec.result()
	if tab.waiter != nil {
		select {
		case tab.waiter <- childReport{result: res, err: err}:
		default:
		}
		tab.waiter = nil
		return
	}
	r.postChildReport(ctx, tab, res, err, runTurn)
}

// postChildReport posts a child's reply to the store for its parent session,
// which takes it the next time it is open and idle, wherever that is. When
// the parent is an idle tab here, that is now. Runs on the event loop with
// no model lock held.
func (r *managedREPL) postChildReport(ctx context.Context, child *replTab, res subagent.Result, err error, runTurn turnRunner) {
	report := sessions.Report{
		Child: child.name, Status: sessions.ReportFinished, Text: res.Text,
		InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
	}
	switch {
	case errors.Is(err, context.Canceled):
		report.Status = sessions.ReportCanceled
	case err != nil:
		report.Status = sessions.ReportFailed
		report.Error = err.Error()
	}
	parent := r.liveParent(child)
	parentName := child.parentName
	if parent != nil {
		// The tab's name follows a rename; the child's record may not.
		parentName = parent.name
	}
	if child.state == nil || child.state.sessionStore == nil || parentName == "" {
		return
	}
	if err := child.state.sessionStore.PostReport(ctx, parentName, report); err != nil {
		r.model.mu.Lock()
		r.model.appendNoticeLine(fmt.Sprintf("agent %s's reply could not be delivered to %s: %s", child.name, parentName, err.Error()))
		r.model.mu.Unlock()
		return
	}
	if parent != nil && r.pullReports(ctx, parent) {
		r.startQueued(ctx, parent, runTurn)
	}
}

// liveParent is child's parent tab while it is open here, else nil.
func (r *managedREPL) liveParent(child *replTab) *replTab {
	if child.parent != nil && r.tabIndexOfModel(child.parent.model) >= 0 {
		return child.parent
	}
	return nil
}

// pullReports takes the reports waiting in the store for tab's session and
// queues them as its next input: one message, echoed by its headers alone.
// Nothing is taken while the tab is busy or polly is leaving; a report left
// in the store waits, one taken into the queue would not. It reports whether
// input was queued. Runs on the event loop with no model lock held.
func (r *managedREPL) pullReports(ctx context.Context, tab *replTab) bool {
	if r.quitting || tab.turnDone != nil || tab.state == nil || tab.state.session == nil {
		return false
	}
	m := tab.model
	m.mu.Lock()
	busy := m.busy
	m.mu.Unlock()
	if busy {
		return false
	}
	reports, err := tab.state.session.TakeReports(ctx)
	if err != nil {
		// A lost lease drops the tab on its own; anything else is news.
		if tab.state.session.Context().Err() == nil {
			r.model.mu.Lock()
			r.model.appendNoticeLine("agent reports for " + tab.name + ": " + err.Error())
			r.model.mu.Unlock()
		}
		return false
	}
	if len(reports) == 0 {
		return false
	}
	bodies := make([]string, len(reports))
	for i, rep := range reports {
		bodies[i] = reportBody(rep)
	}
	display := reportHeader(reports[0])
	if n := len(reports); n > 1 {
		display = fmt.Sprintf("%d agent reports", n)
	}
	turn := managedTurnInput{displayText: display, userMessage: messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Join(bodies, "\n\n")}}
	m.mu.Lock()
	m.queue = slices.Insert(m.queue, 0, queuedREPLInput{text: display, turn: &turn})
	m.mu.Unlock()
	return true
}

// pullAllReports pulls for every idle tab and runs what that queued, at
// startup and on the report poll. It reports whether any tab got input.
// Runs on the event loop with no model lock held.
func (r *managedREPL) pullAllReports(ctx context.Context, runTurn turnRunner) bool {
	queued := false
	for _, tab := range append([]*replTab(nil), r.tabs...) {
		if r.pullReports(ctx, tab) {
			queued = true
			r.startQueued(ctx, tab, runTurn)
		}
	}
	return queued
}

// reportHeader is a report's one-line summary: how the child ended. Headers
// stay free of square brackets: the transcript's style parser reads those as
// markup.
func reportHeader(rep sessions.Report) string {
	switch rep.Status {
	case sessions.ReportCanceled:
		return fmt.Sprintf("agent %s canceled", rep.Child)
	case sessions.ReportFailed:
		return fmt.Sprintf("agent %s failed: %s", rep.Child, rep.Error)
	}
	return fmt.Sprintf("agent %s finished", rep.Child)
}

// reportBody is the message a report makes for the parent: its header, then
// the child's reply with the session trailer a blocking call returns.
func reportBody(rep sessions.Report) string {
	res := subagent.Result{Text: rep.Text, Session: rep.Child, InputTokens: rep.InputTokens, OutputTokens: rep.OutputTokens}
	return reportHeader(rep) + "\n" + res.String()
}

// closeSpentChildren closes parent's children whose report was taken and
// that nobody looked at, now that parent's turn settled. Runs on the event
// loop with no model lock held.
func (r *managedREPL) closeSpentChildren(parent *replTab) {
	for i := len(r.tabs) - 1; i >= 0; i-- {
		tab := r.tabs[i]
		if tab.parent != parent || tab.viewed || tab.report != nil || tab.turnDone != nil || tab.model == r.model {
			continue
		}
		r.removeTab(i)
		_ = tab.state.Close()
	}
}

// closeSpentOrphan closes a settled child whose parent tab is gone, once its
// report is in the store, unless someone looked at it: nothing here will take
// the report, so nothing here needs the tab. It reports whether it closed the
// tab. Runs on the event loop with no model lock held.
func (r *managedREPL) closeSpentOrphan(tab *replTab) bool {
	if tab.parentName == "" || tab.parent != nil || tab.viewed || tab.report != nil || tab.turnDone != nil || tab.model == r.model {
		return false
	}
	i := r.tabIndexOfModel(tab.model)
	if i < 0 {
		return false
	}
	r.removeTab(i)
	_ = tab.state.Close()
	return true
}

// orphanChild drops the waiter of a child whose blocking call stopped
// waiting. A report already handed to that waiter is posted instead. Runs
// on the event loop with no model lock held.
func (r *managedREPL) orphanChild(tab *replTab) {
	if tab.waiter == nil {
		return
	}
	select {
	case rep := <-tab.waiter:
		tab.waiter = nil
		r.postChildReport(r.runCtx, tab, rep.result, rep.err, r.runTurn)
	default:
		tab.waiter = nil
	}
}

// requestSpawnLocked starts a background child of the visible tab on brief;
// the event loop applies it. Caller must hold r.model.mu.
func (r *managedREPL) requestSpawnLocked(brief string) {
	if r.opener == nil || r.opener.spawn == nil {
		r.model.appendNoticeLine("spawning agents is unavailable")
		return
	}
	if r.visibleTabIndex() < 0 || r.state == nil {
		r.model.appendNoticeLine("no session to spawn from")
		return
	}
	r.spawnRequests = append(r.spawnRequests, spawnRequest{parent: r.visibleTab(), req: subagent.Request{Task: brief, Background: true}})
}

// applySpawnRequests performs the /spawn requests handlers recorded. Runs on
// the event loop with no model lock held.
func (r *managedREPL) applySpawnRequests() {
	requests := r.spawnRequests
	r.spawnRequests = nil
	for _, sr := range requests {
		child, err := r.opener.spawn(r.runCtx, sr.parent.state, sr.req)
		if err == nil {
			_, err = r.spawnChildTab(sr.parent.model, child, sr.req, nil, "")
		}
		if err != nil {
			r.model.mu.Lock()
			r.model.appendErrorLine("could not spawn an agent: " + err.Error())
			r.model.mu.Unlock()
		}
	}
}

// attachChildToTool points the running disclosure row for callID at the
// child's screen model, so the row can show what the child is doing.
// Caller must hold m.mu.
func (m *replModel) attachChildToTool(callID string, child *replModel, name string) {
	for i := range m.activeTools {
		if m.activeTools[i].id == callID {
			m.activeTools[i].child = child
			m.activeTools[i].childName = name
			return
		}
	}
}

// childActivity describes what a child tab is doing, for its parent's tool
// row: what its turn is up to and how many tools it has called. The caller
// holds the parent's lock and the child is locked here; the parent is the
// visible tab when its rows repaint, so this is the visible-over-hidden
// order the tab code keeps.
func childActivity(child *replModel) string {
	child.mu.Lock()
	defer child.mu.Unlock()
	var parts []string
	switch {
	case child.approval != nil:
		parts = append(parts, "approval needed")
	case child.busy:
		parts = append(parts, child.busyLabel())
	default:
		switch child.lastOutcome {
		case turnOutcomeDone:
			parts = append(parts, "done")
		case turnOutcomeFailed:
			parts = append(parts, "failed")
		case turnOutcomeCanceled:
			parts = append(parts, "canceled")
		}
	}
	switch n := child.turnToolCallCount(); n {
	case 0:
	case 1:
		parts = append(parts, "1 tool")
	default:
		parts = append(parts, fmt.Sprintf("%d tools", n))
	}
	return strings.Join(parts, " · ")
}

// turnToolCallCount counts the tool calls the current turn has made. Caller
// must hold m.mu.
func (m *replModel) turnToolCallCount() int {
	n := 0
	for _, id := range m.turnToolDisclosureIDs {
		if record := m.toolDisclosures[id]; record != nil {
			n += len(record.rows)
		}
	}
	return n
}

// tabShortcut maps Alt-1 to Alt-9 to a tab position and Alt-] and Alt-[ to
// the next and previous tab, wrapping around.
func tabShortcut(id string, visible, count int) (int, bool) {
	if count == 0 {
		return 0, false
	}
	switch id {
	case "<M-]>":
		return (visible + 1) % count, true
	case "<M-[>":
		return ((visible-1)%count + count) % count, true
	}
	if len(id) == 5 && strings.HasPrefix(id, "<M-") && id[3] >= '1' && id[3] <= '9' && id[4] == '>' {
		if n := int(id[3] - '1'); n < count {
			return n, true
		}
	}
	return 0, false
}
