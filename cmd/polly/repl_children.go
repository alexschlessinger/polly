package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// spawnRequest is a /spawn typed on a tab, applied by the event loop.
type spawnRequest struct {
	parent *replTab
	req    subagent.Request
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
					r.model.appendNoticeLine("Agent reports for " + tab.name + " unavailable · " + err.Error())
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
	turn := managedTurnInput{
		displayText: display,
		userMessage: messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Join(bodies, "\n\n"), Metadata: map[string]any{messages.MetadataKeyAgentReport: true}},
		reportIDs:   ids,
		notice:      true,
	}
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
	case sessions.ReportPaused:
		return fmt.Sprintf("agent %s paused at its iteration limit: %s", rep.Child, rep.Error)
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
	if tab.parentName == "" || !tab.delivered || tab.keepOpen || tab.turnDone != nil || tab.viewOpening || tab.model == r.model || r.runningDescendants(tab) > 0 {
		return false
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

// requestSpawnLocked starts a background child of the visible tab on brief;
// the event loop applies it. Caller must hold r.model.mu.
func (r *managedREPL) requestSpawnLocked(req subagent.Request) {
	if r.visibleTabIndex() < 0 || r.state == nil || r.state.swarm == nil {
		r.model.appendNoticeLine("Spawn from a parent session; inspected agents cannot start agents")
		return
	}
	req.Background = true
	r.spawnRequests = append(r.spawnRequests, spawnRequest{parent: r.visibleTab(), req: req})
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
		parent := sr.parent
		if r.tabIndexOfModel(parent.model) < 0 || parent.state == nil || parent.state.swarm == nil {
			continue
		}
		runtime := parent.state.swarm
		probe := parent.state.sandboxProbe
		parent.model.mu.Lock()
		settings := parent.state.settings.clone()
		req := createCompletionRequest(r.config, &settings, nil, parent.state.toolRegistry, nil, nil)
		updateSwarmDefaults(parent.state, req, settings)
		parent.model.mu.Unlock()
		// Worktree capture and tool binding can take time. The runtime owns the
		// member from launch onward, independently of the visible tab.
		r.background(func() {
			var res subagent.Result
			err := probe.wait(r.work.ctx)
			if err == nil {
				res, err = runtime.Spawn(r.work.ctx, sr.req)
			}
			r.postUI(r.work.ctx, func() {
				if r.tabIndexOfModel(parent.model) < 0 || parent.state.swarm != runtime {
					return
				}
				parent.model.mu.Lock()
				defer parent.model.mu.Unlock()
				if err != nil {
					parent.model.appendErrorLine("could not spawn an agent: " + err.Error())
				} else {
					parent.swarmActive = true
					parent.model.appendNoticeLine("Agent " + res.Session + " started · /sessions to inspect · /swarm to coordinate")
				}
			})
		})
	}
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
