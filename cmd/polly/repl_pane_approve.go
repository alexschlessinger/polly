package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// What a turn waits on, in the host. An approval or a question from the model
// is asked in polly's own screen first. Where the host can ask too (see
// repl_pane_ask.go) it is put there as well, for a user who is not looking at
// polly: at once when the terminal has lost the keys, and otherwise after
// hostAskDelay with neither an answer nor a key. The host shows it on every app connected to it,
// so it can be answered from another device.
//
// Both stay up, and the first answer settles it: one given in polly takes the
// host's question down. The host's list opens with a row that decides
// nothing, so a key meant for something else cannot approve a tool.

// hostAskDelay is how long polly's own screen has a question to itself while
// the terminal has the keys: long enough to read a command through.
const hostAskDelay = 15 * time.Second

// askLaterBackoff bounds how soon syncHostAsks retries a host that reported
// errAskLater (busy or no app connected) so it does not spin every frame.
var askLaterBackoff = 2 * time.Second

// The rows of the host's questions that are not an option of the model's.
const (
	askLater    = "later"
	approveYes  = "allow"
	approveNo   = "deny"
	approveRest = "allow-all"
)

// approvalAsk identifies the host's question about one call of a batch.
type approvalAsk struct {
	model   *replModel
	request *approvalState
	call    int
}

// waits reports whether the call is still the one its model is asking about.
func (a approvalAsk) waits() bool {
	a.model.mu.Lock()
	defer a.model.mu.Unlock()
	return a.model.approval == a.request && a.request.index == a.call
}

// questionAsk identifies the host's copy of a question from the model.
type questionAsk struct {
	model  *replModel
	dialog *askDialog
}

func (a questionAsk) waits() bool {
	a.model.mu.Lock()
	defer a.model.mu.Unlock()
	return !a.dialog.answered
}

// syncHostAsks keeps the host's question in step with what the tabs wait on.
// Every render starts here: a question settled in polly is taken down, and
// the next one due is put up. Runs on the event loop with no lock held.
func (r *managedREPL) syncHostAsks() {
	h := &r.panes
	if _, ok := h.prompts(); !ok || r.work == nil {
		return
	}
	if h.asked != nil {
		if about, ok := h.asked.about.(interface{ waits() bool }); ok && !about.waits() {
			h.asked.cancel()
		}
		return
	}
	now := time.Now()
	retryAt := h.askRetryAt
	if h.askLaterUntil.After(retryAt) {
		retryAt = h.askLaterUntil
	}
	if now.Before(retryAt) {
		// The host has no list to ask in for now (see askHost): wake an idle
		// REPL when the backoff expires if a question is still waiting.
		if h.waking == nil && r.hasUnhostedAsk() {
			h.waking = time.AfterFunc(retryAt.Sub(now), func() { r.postUI(r.work.ctx, func() { h.waking = nil }) })
		}
		return
	}
	r.model.mu.Lock()
	away, typed, paneAt := r.model.focusKnown && !r.model.focused, r.keyAt, r.paneInputAt
	r.model.mu.Unlock()
	soonest := time.Duration(0)
	// Recent input in a detached pane (such as reading an agent pane in Rex)
	// shows the user is still actively working with polly.
	if !paneAt.IsZero() && now.Sub(paneAt) < hostAskDelay {
		away = false
		if paneAt.After(typed) {
			typed = paneAt
		}
	}
	// due reports whether a question waiting since then goes to the host now.
	// A key pressed since shows the user is here, reading it.
	due := func(since time.Time) bool {
		if typed.After(since) {
			since = typed
		}
		left := hostAskDelay - now.Sub(since)
		if away || left <= 0 {
			return true
		}
		if soonest == 0 || left < soonest {
			soonest = left
		}
		return false
	}
	for _, tab := range r.tabs {
		m := tab.model
		m.mu.Lock()
		var q paneQuestion
		var about any
		var answered func(paneAnswer, bool)
		var reset func()
		if a := m.approval; a != nil && a.index < len(a.calls) && a.hosted <= a.index && due(a.waiting) {
			a.hosted = a.index + 1
			ask := approvalAsk{model: m, request: a, call: a.index}
			reset = func() {
				m.mu.Lock()
				if a.hosted == ask.call+1 {
					a.hosted = ask.call
				}
				m.mu.Unlock()
			}
			q, about = approvalQuestion(tab.name, a), ask
			answered = func(answer paneAnswer, asked bool) {
				if !asked {
					reset()
					if !h.listless && !time.Now().Before(h.askRetryAt) {
						h.askLaterUntil = time.Now().Add(askLaterBackoff)
					}
					return
				}
				h.askLaterUntil = time.Time{}
				r.answerApprovalAsk(ask, answer)
			}
		} else if d := modelQuestion(m); d != nil && !d.hosted && due(d.shown) {
			d.hosted = true
			ask := questionAsk{model: m, dialog: d}
			reset = func() {
				m.mu.Lock()
				d.hosted = false
				m.mu.Unlock()
			}
			q, about = askQuestion(tab.name, d.req), ask
			answered = func(answer paneAnswer, asked bool) {
				if !asked {
					reset()
					if !h.listless && !time.Now().Before(h.askRetryAt) {
						h.askLaterUntil = time.Now().Add(askLaterBackoff)
					}
					return
				}
				h.askLaterUntil = time.Time{}
				r.answerQuestionAsk(ask, answer)
			}
		}
		m.mu.Unlock()
		if about != nil {
			if !r.askHost(q, about, answered) {
				reset()
			}
			return
		}
	}
	if soonest > 0 && h.waking == nil {
		// Nothing else may paint before the question is due.
		h.waking = time.AfterFunc(soonest, func() { r.postUI(r.work.ctx, func() { h.waking = nil }) })
	}
}

// hasUnhostedAsk reports whether any tab holds an approval or model question
// that has not been presented to the host yet.
func (r *managedREPL) hasUnhostedAsk() bool {
	for _, tab := range r.tabs {
		m := tab.model
		m.mu.Lock()
		pending := (m.approval != nil && m.approval.index < len(m.approval.calls) && m.approval.hosted <= m.approval.index) ||
			(modelQuestion(m) != nil && !modelQuestion(m).hosted)
		m.mu.Unlock()
		if pending {
			return true
		}
	}
	return false
}

// modelQuestion is the question m's turn waits on, shown or parked behind
// another dialog. Caller holds m.mu.
func modelQuestion(m *replModel) *askDialog {
	for _, modal := range []*replModal{m.modal, m.pendingModal} {
		if modal != nil && modal.ask != nil && !modal.ask.answered {
			return modal.ask
		}
	}
	return nil
}

// approvalQuestion asks about the call a is at. Caller holds its model's lock.
func approvalQuestion(session string, a *approvalState) paneQuestion {
	call := a.calls[a.index]
	label := toolLabel(call)
	if a.requester != "" {
		label = "agent " + a.requester + ": " + label
	}
	choices := []paneChoice{
		{id: askLater, title: "Decide in polly", subtitle: "leave this for the terminal"},
		{id: approveYes, title: "Allow", subtitle: rowText(expandToolCall(call))},
		{id: approveNo, title: "Deny"},
	}
	if left := len(a.calls) - a.index; left > 1 {
		choices = append(choices, paneChoice{id: approveRest, title: fmt.Sprintf("Allow all %d", left), subtitle: "this call and the ones waiting behind it"})
	}
	return paneQuestion{title: style.Truncate("Allow "+label+"?", pickTitleWidth), hint: "polly · " + session, choices: choices}
}

// askQuestion is the model's question as the host can show it: its first
// line, with the choices and their explanations as rows.
func askQuestion(session string, req askRequest) paneQuestion {
	title, _, _ := strings.Cut(strings.TrimSpace(req.Question), "\n")
	if req.Requester != "" {
		title = "agent " + req.Requester + " asks: " + title
	}
	q := paneQuestion{title: style.Truncate(title, pickTitleWidth), hint: "polly · " + session, typed: req.Other}
	if req.Other {
		q.hint = "Type another answer · " + q.hint
	}
	q.choices = append(q.choices, paneChoice{id: askLater, title: "Answer in polly", subtitle: "leave this for the terminal"})
	for i, option := range req.Options {
		q.choices = append(q.choices, paneChoice{id: strconv.Itoa(i), title: option.Label, subtitle: rowText(option.Description)})
	}
	return q
}

// rowText is text as a row can carry it.
func rowText(text string) string {
	return style.Truncate(strings.Join(strings.Fields(text), " "), 2*pickTitleWidth)
}

// answerApprovalAsk applies the host's answer to the call it asked about,
// unless polly's own screen settled it first. No lock is held.
func (r *managedREPL) answerApprovalAsk(ask approvalAsk, answer paneAnswer) {
	key, ok := map[string]byte{approveYes: 'y', approveNo: 'n', approveRest: 'a'}[answer.id]
	if !answer.given || !ok {
		return
	}
	m, a := ask.model, ask.request
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.approval == a && a.index == ask.call {
		m.answerApprovalRequest(a, key)
	}
}

// answerQuestionAsk gives the model the host's answer and takes its dialog
// down, unless polly's own screen answered first. No lock is held.
func (r *managedREPL) answerQuestionAsk(ask questionAsk, answer paneAnswer) {
	if !answer.given || answer.id == askLater {
		return
	}
	m, d := ask.model, ask.dialog
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.answered {
		return
	}
	reply := askAnswer{Outcome: askOutcomeAnswered, Index: len(d.req.Options), Choice: askOtherLabel, Text: strings.TrimSpace(answer.text)}
	if i, err := strconv.Atoi(answer.id); err == nil && i >= 0 && i < len(d.req.Options) {
		reply = askAnswer{Outcome: askOutcomeAnswered, Index: i, Choice: d.req.Options[i].Label}
	} else if reply.Text == "" || !d.req.Other {
		return
	}
	d.answer(reply)
	r.removeAskLocked(m, d)
}
