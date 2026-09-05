package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
// delivered child closes while hidden, unless the user has a draft or has
// continued its conversation. A visible child stays until the user leaves.

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
	t.model.mu.Lock()
	parent := t.state.childSnapshot()
	t.model.mu.Unlock()
	return r.runChild(ctx, t.model, parent, req, toolCallFrom(ctx).ID)
}

// runChild prepares the session and screen off the event loop. A launch has
// one owner: either the loop adopts it, or this goroutine closes it. The
// lifetime channel always follows the child, even when its caller cancels.
func (r *managedREPL) runChild(ctx context.Context, parentModel *replModel, parent *conversationState, req subagent.Request, callID string) (subagent.Result, error) {
	if !r.work.begin() {
		return subagent.Result{}, context.Canceled
	}
	defer r.work.wg.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.work.ctx, cancel)
	defer stop()
	defer cancel()
	child, err := r.opener.spawn(ctx, parent, req)
	if err != nil {
		return subagent.Result{}, err
	}
	if callID != "" {
		if err := updateMetadata(ctx, child.session, func(md *sessions.Metadata) { md.SpawnCallID = callID }); err != nil {
			_ = child.Close()
			return subagent.Result{}, err
		}
	}
	name, model, err := r.newTabModelContext(ctx, child)
	if err != nil {
		_ = child.Close()
		return subagent.Result{}, err
	}
	tab := &replTab{name: name, state: child, model: model, report: &childTurnUI{}, settled: make(chan struct{})}
	if !req.Background {
		tab.waiter = make(chan childReport, 1)
		tab.waitCtx = ctx
	}
	waiter := tab.waiter
	res := subagent.Result{Session: name, Done: tab.settled}
	var handoff sync.Mutex
	adopted, abandoned := false, false
	spawned := make(chan error, 1)
	launch := func() {
		handoff.Lock()
		defer handoff.Unlock()
		if abandoned {
			return
		}
		err := context.Cause(ctx)
		if err == nil {
			err = r.spawnChildTab(parentModel, tab, req, callID)
		}
		adopted = err == nil
		spawned <- err
	}
	cleanup := func() {
		handoff.Lock()
		abandoned = !adopted
		owned := adopted
		handoff.Unlock()
		if !owned {
			_ = child.Close()
			tab.markSettled()
		} else if waiter != nil {
			r.childDoneWaiting(tab, waiter, true)
		}
	}
	if !r.postUI(ctx, launch) {
		cleanup()
		err := context.Cause(ctx)
		if err == nil {
			err = context.Canceled
		}
		return res, err
	}
	select {
	case err := <-spawned:
		if err != nil {
			cleanup()
			return res, err
		}
	case <-ctx.Done():
		cleanup()
		return res, context.Cause(ctx)
	}
	if req.Background {
		res.Started = true
		return res, nil
	}
	select {
	case rep := <-waiter:
		r.childDoneWaiting(tab, waiter, false)
		return rep.result, rep.err
	case <-ctx.Done():
		cleanup()
		return res, context.Cause(ctx)
	}
}

// spawnChildTab opens child as a tab under the tab that owns parentModel,
// starts its first turn on the brief, and, for a blocking spawn, points the
// parent's tool row at it so the row can show the child's progress. Runs on
// the event loop with no model lock held.
func (r *managedREPL) spawnChildTab(parentModel *replModel, tab *replTab, req subagent.Request, callID string) error {
	if r.quitting {
		return errors.New("polly is quitting")
	}
	pi := r.tabIndexOfModel(parentModel)
	if pi < 0 || r.runTurn == nil {
		return errors.New("the parent tab is closed")
	}
	parent := r.tabs[pi]
	child, m, name := tab.state, tab.model, tab.name
	tab.spawnCallID = callID
	tab.parent, tab.parentName = parent, parent.name
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
		tab.agentActivity = &agentActivity{}
		tab.agentStatus, tab.agentActive = "working", true
		parentModel.mu.Lock()
		parentModel.attachChildToTool(callID, m, name)
		if record, row := parentModel.toolDisclosureRowForCall(callID); row != nil && row.agent != nil {
			tab.agentActivity = row.agent
			row.agent.session, row.agent.status, row.agent.attached = name, "working", true
			parentModel.refreshAgentRecord(record)
		}
		parentModel.mu.Unlock()
	}
	r.model.mu.Lock()
	if tab.waiter == nil {
		label := req.Label
		if label == "" {
			label = name
		}
		r.model.appendNoticeLine(fmt.Sprintf("agent %s started in tab %d · /tab %s", label, at+1, name))
	}
	r.model.mu.Unlock()
	return nil
}

// deliverChildReport hands a settled child's first reply to whoever waits
// for it: the parent's blocking call, or else the store, for the parent
// session. Runs on the event loop with no model lock held.
func (r *managedREPL) deliverChildReport(ctx context.Context, tab *replTab, err error, runTurn turnRunner) {
	if tab.report == nil {
		return
	}
	r.finishChildAgent(tab, err)
	rec := tab.report
	tab.report = nil
	res := subagent.Result{Session: tab.name, Done: tab.settled}
	res.Text, res.InputTokens, res.OutputTokens = rec.result()
	if tab.waiter != nil && (tab.waitCtx == nil || tab.waitCtx.Err() == nil) {
		tab.deliveryPending = true
		select {
		case tab.waiter <- childReport{result: res, err: err}:
		default:
		}
		tab.waiter = nil
		return
	}
	tab.waiter = nil
	r.postChildReport(ctx, tab, res, err, runTurn)
}

// postChildReport posts a child's reply through its session to the store,
// for the parent session the store links it to, which takes it the next
// time it is open and idle, wherever that is. When the parent is an idle tab
// here, that is now. Runs on the event loop with no model lock held.
func (r *managedREPL) postChildReport(ctx context.Context, child *replTab, res subagent.Result, err error, runTurn turnRunner) {
	report := storedChildReport(res, err)
	if child.state == nil || child.state.session == nil {
		return
	}
	child.reporting = true
	done := make(chan struct{})
	child.reportWriteDone = done
	session := child.state.session
	if !r.background(func() {
		defer close(done)
		writeErr := writeChildReport(session, report)
		if writeErr != nil {
			r.work.recordError(fmt.Errorf("deliver agent report: %w", writeErr))
		}
		r.postUI(r.work.ctx, func() {
			child.reporting = false
			if writeErr != nil {
				r.model.mu.Lock()
				r.model.appendNoticeLine(fmt.Sprintf("agent %s's reply could not be delivered to %s: %s", child.name, child.parentName, writeErr))
				r.model.mu.Unlock()
				return
			}
			if parent := r.liveParent(child); parent != nil {
				r.pullReports(r.runCtx, parent)
			}
			child.delivered = true
			r.closeSpentChild(child)
		})
	}) {
		child.reporting = false
		close(done)
	}
}

func storedChildReport(res subagent.Result, err error) sessions.Report {
	report := sessions.Report{Status: sessions.ReportFinished, Text: res.Text, InputTokens: res.InputTokens, OutputTokens: res.OutputTokens}
	switch {
	case errors.Is(err, context.Canceled):
		report.Status = sessions.ReportCanceled
	case err != nil:
		report.Status = sessions.ReportFailed
		report.Error = err.Error()
	}
	return report
}

func writeChildReport(session sessions.Session, report sessions.Report) error {
	// A finished child's report survives cancellation of the parent turn.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return session.Report(ctx, report)
}

// liveParent is child's parent tab while it is open here, else nil.
func (r *managedREPL) liveParent(child *replTab) *replTab {
	if child.parent != nil && r.tabIndexOfModel(child.parent.model) >= 0 {
		return child.parent
	}
	return nil
}

// pullReports schedules an idle tab's inbox read. Its completion queues one
// parent input, echoed by its headers alone, before draining other queued
// inputs. Reports remain in the store until that input is persisted. Returns
// whether a read started; runs on the event loop with no model lock held.
func (r *managedREPL) pullReports(ctx context.Context, tab *replTab) bool {
	if r.quitting || tab.turnDone != nil || tab.state == nil || tab.state.session == nil {
		return false
	}
	if tab.reportsLoading {
		tab.reportsRepull = true
		return false
	}
	m := tab.model
	m.mu.Lock()
	busy := m.busy
	m.mu.Unlock()
	if busy {
		return false
	}
	tab.reportsLoading = true
	session := tab.state.session
	if !r.background(func() {
		readCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(r.work.ctx, cancel)
		defer stop()
		defer cancel()
		reports, err := session.PeekReports(readCtx)
		r.postUI(readCtx, func() {
			tab.reportsLoading = false
			if r.quitting || r.tabIndexOfModel(m) < 0 {
				return
			}
			if err != nil {
				if session.Context().Err() == nil {
					r.model.mu.Lock()
					r.model.appendNoticeLine("agent reports for " + tab.name + ": " + err.Error())
					r.model.mu.Unlock()
				}
			} else {
				m.queueReports(reports)
			}
			if tab.reportsRepull {
				tab.reportsRepull = false
				if r.pullReports(r.runCtx, tab) {
					return
				}
			}
			// Other queued inputs waited on this read, whatever it found.
			m.mu.Lock()
			busy := m.busy
			m.mu.Unlock()
			if !busy {
				r.startQueued(r.runCtx, tab, r.runTurn)
			}
		})
	}) {
		tab.reportsLoading = false
		return false
	}
	return true
}

// queueReports puts one input for the reports not already running, queued,
// or held as a restored draft ahead of the other queued inputs. Runs with
// no model lock held.
func (m *replModel) queueReports(reports []sessions.Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[int64]bool)
	remember := func(turn managedTurnInput) {
		for _, id := range turn.reportIDs {
			seen[id] = true
		}
	}
	if m.busy {
		remember(m.currentTurn)
	}
	if m.restoredDraft != nil {
		remember(*m.restoredDraft)
	}
	for _, item := range m.queue {
		if item.turn != nil {
			remember(*item.turn)
		}
	}
	var bodies []string
	var ids []int64
	display := ""
	for _, rep := range reports {
		if seen[rep.ID] {
			continue
		}
		bodies = append(bodies, reportBody(rep))
		ids = append(ids, rep.ID)
		display = reportHeader(rep)
	}
	if len(ids) == 0 {
		return
	}
	if len(ids) > 1 {
		display = fmt.Sprintf("%d agent reports", len(ids))
	}
	turn := managedTurnInput{displayText: display, userMessage: messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Join(bodies, "\n\n")}, reportIDs: ids}
	m.queue = slices.Insert(m.queue, 0, queuedREPLInput{text: display, turn: &turn})
}

// pullAllReports schedules a read for every idle tab. Results return through
// uiTasks; a report stays durable until its parent input is persisted.
func (r *managedREPL) pullAllReports(ctx context.Context, runTurn turnRunner) bool {
	started := false
	for _, tab := range r.tabs {
		if r.pullReports(ctx, tab) {
			started = true
		}
	}
	return started
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

// closeSpentChild releases a delivered agent's hidden tab. Drafts, queued
// input, and follow-up conversations belong to the user and keep it open.
// Runs on the event loop with no model lock held.
func (r *managedREPL) closeSpentChild(tab *replTab) bool {
	if tab.parentName == "" || !tab.delivered || tab.keepOpen || tab.report != nil || tab.reporting || tab.deliveryPending || tab.turnDone != nil || tab.model == r.model || r.runningChildren(tab) > 0 {
		return false
	}
	if tab.settled != nil {
		select {
		case <-tab.settled:
		default:
			return false
		}
	}
	m := tab.model
	m.mu.Lock()
	// An untouched retry draft restored after a failed initial run is not
	// user input. Editing it makes it an ordinary draft.
	draft := !m.ed.empty() && (m.restoredDraft == nil || m.ed.text() != m.restoredDraft.displayText)
	keep := m.busy || draft || len(m.queue) > 0 || m.pasting || m.clipboardCapture
	m.mu.Unlock()
	if keep {
		return false
	}
	i := r.tabIndexOfModel(tab.model)
	if i < 0 {
		return false
	}
	r.removeTab(i)
	r.closeTabState(tab)
	return true
}

func (r *managedREPL) closeSpentTabs() {
	for i := len(r.tabs) - 1; i >= 0; i-- {
		r.closeSpentChild(r.tabs[i])
	}
}

// orphanChild drops the waiter of a child whose blocking call stopped
// waiting. A report already handed to that waiter is posted instead. Runs
// on the event loop with no model lock held.
func (r *managedREPL) orphanChild(tab *replTab, waiter chan childReport) {
	tab.waiter = nil
	select {
	case rep := <-waiter:
		r.postChildReport(r.runCtx, tab, rep.result, rep.err, r.runTurn)
	default:
	}
}

// A buffered reply is not spent until its caller accepts it or gives it back
// for durable delivery. Acknowledgement is asynchronous, including when the
// UI queue is full; shutdown drains a returned reply before closing sessions.
func (r *managedREPL) childDoneWaiting(tab *replTab, waiter chan childReport, abandoned bool) {
	finish := func() {
		ack := make(chan struct{})
		if r.postUI(r.work.ctx, func() {
			if abandoned {
				r.orphanChild(tab, waiter)
			}
			tab.deliveryPending = false
			if !abandoned {
				tab.delivered = true
			}
			r.closeSpentChild(tab)
			close(ack)
		}) {
			select {
			case <-ack:
				return
			case <-r.work.ctx.Done():
			}
		}
		if abandoned {
			select {
			case rep := <-waiter:
				r.work.recordError(writeChildReport(tab.state.session, storedChildReport(rep.result, rep.err)))
			default:
			}
		}
	}
	if !r.background(finish) {
		finish()
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
	if r.quitting {
		return
	}
	for _, sr := range requests {
		sr.parent.model.mu.Lock()
		parent := sr.parent.state.childSnapshot()
		sr.parent.model.mu.Unlock()
		ctx := r.runCtx
		r.background(func() {
			_, err := r.runChild(ctx, sr.parent.model, parent, sr.req, "")
			if err != nil {
				r.postUI(r.work.ctx, func() {
					r.model.mu.Lock()
					r.model.appendErrorLine("could not spawn an agent: " + err.Error())
					r.model.mu.Unlock()
				})
			}
		})
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

// childActivity samples a child's state for its parent's tool row. Painting
// never waits for the hidden model; a busy lock retains the previous label.
func childActivity(child *replModel) (string, bool) {
	if !child.mu.TryLock() {
		return "", false
	}
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
	return strings.Join(parts, " · "), true
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
