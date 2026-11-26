package main

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// Questions in the host. Where a pane host has a list of its own (Rex's
// command palette), polly asks there what it would otherwise ask in a modal
// or a prompt of its own: which session, which command, a session's title,
// whether a tool may run. The host shows one question at a time, to every
// app connected to it, so the answer may come from another device.
//
// Polly stays usable while a question is up, and the answer lands whenever
// the user gives one. A host that cannot ask hands the job back: quietly when
// it is busy or was never set up for questions, and otherwise for good, with
// its reason.

// hostAsk is a question the host is showing: about says what it is for, and
// cancel takes it down.
type hostAsk struct {
	about  any
	cancel context.CancelFunc
}

// askHost puts q to the user in the host and hands the outcome to answered,
// on the event loop with no lock held. asked is false when the host could
// not ask, and whoever wanted the answer has to ask in polly's own screen;
// an answer that is not given is the user declining to. about identifies the
// question, for hostAsking and withdrawHostAsk.
//
// False means the host is not asked and answered never runs: there is none,
// it failed before, or it is showing another question.
func (r *managedREPL) askHost(q paneQuestion, about any, answered func(answer paneAnswer, asked bool)) bool {
	h := &r.panes
	prompts, ok := h.prompts()
	if !ok || h.asked != nil || r.work == nil || (!h.askRetryAt.IsZero() && time.Now().Before(h.askRetryAt)) {
		return false
	}
	ctx, cancel := context.WithTimeout(r.work.ctx, cmp.Or(q.wait, paneAskTime)+paneOpenTimeout)
	ask := &hostAsk{about: about, cancel: cancel}
	h.asked = ask
	name := h.opener.words().name
	if !r.background(func() {
		defer cancel()
		answer, err := prompts.ask(ctx, q)
		if ctx.Err() != nil {
			// Withdrawn, or up for as long as a question can be: no answer.
			answer, err = paneAnswer{}, nil
		}
		r.postUI(r.work.ctx, func() {
			if h.asked == ask {
				h.asked = nil
			}
			if errors.Is(err, errAskUnconfigured) {
				h.askRetryAt = time.Now().Add(askUnconfiguredBackoff)
			} else if err == nil {
				h.askRetryAt = time.Time{}
			} else if !errors.Is(err, errAskLater) {
				h.listless = true
				r.model.mu.Lock()
				r.model.appendNoticeLine(name + " could not ask, asking here from now on · " + err.Error())
				r.model.mu.Unlock()
			}
			answered(answer, err == nil)
		})
	}) {
		cancel()
		h.asked = nil
		return false
	}
	return true
}

// hostAsking reports whether the host is showing the question about.
func (r *managedREPL) hostAsking(about any) bool {
	return r.panes.asked != nil && r.panes.asked.about == about
}

// withdrawHostAsk takes the question about down, answered somewhere else.
// Its answered func still runs, with no answer given.
func (r *managedREPL) withdrawHostAsk(about any) {
	if r.hostAsking(about) {
		r.panes.asked.cancel()
	}
}

// ownListWidth is how wide polly draws a list of its own.
const ownListWidth = 76

// ask puts q to the user wherever polly runs: in the host's list, and with
// no host that can show one in a list polly draws itself (askOwn), which is
// the same question in polly's own cells. answered hears the outcome on the
// event loop with no lock held; an answer that is not given is the user
// declining to. False means nobody could ask: a dialog is open. Caller holds
// the visible model's lock.
func (r *managedREPL) ask(q paneQuestion, about any, answered func(paneAnswer)) bool {
	if r.askHost(q, about, func(answer paneAnswer, asked bool) {
		if asked {
			answered(answer)
			return
		}
		r.model.mu.Lock()
		own := r.askOwn(q, answered)
		r.model.mu.Unlock()
		if !own {
			answered(paneAnswer{})
		}
	}) {
		return true
	}
	return r.askOwn(q, answered)
}

// askOwn asks q in a dialog on polly's own screen: a list to filter and
// choose from, or a line to type. False means a dialog is already open.
// Caller holds the visible model's lock.
func (r *managedREPL) askOwn(q paneQuestion, answered func(paneAnswer)) bool {
	if r.dialogOpen() {
		return false
	}
	m := r.model
	// The dialog's keys run with the model locked, and whoever hears the
	// answer takes the lock it needs itself.
	reply := func(answer paneAnswer) {
		m.mu.Unlock()
		defer m.mu.Lock()
		answered(answer)
	}
	d := &replModal{title: q.title, width: ownListWidth, onCancel: func() { reply(paneAnswer{}) }}
	if q.text {
		d.inputMode = true
		d.helper = "Enter " + strings.ToLower(cmp.Or(q.confirm, "save")) + " · Esc cancel"
		d.input.setText(q.value)
		d.onSubmit = func(text string) { reply(paneAnswer{given: true, text: text}) }
	} else {
		for _, choice := range q.choices {
			d.items = append(d.items, ownListRow(choice))
		}
		d.onSubmit = func(id string) { reply(paneAnswer{given: true, id: id}) }
	}
	r.openModal(d)
	return true
}

// ownListRow is a choice as a row of polly's own list: its title, what it is
// in muted words after it, and its detail last.
func ownListRow(choice paneChoice) replModalItem {
	const room = ownListWidth - 6 // The border, the selection mark, and a margin.
	title := style.Truncate(choice.title, room)
	rest := ""
	for _, part := range []string{choice.subtitle, choice.detail} {
		if part != "" {
			rest += "  " + part
		}
	}
	rest = style.Truncate(rest, max(0, room-len([]rune(title))))
	muted := ""
	if strings.TrimSpace(rest) != "" {
		muted = style.Styled(rest, "muted", "")
	}
	return replModalItem{
		label:           choice.title,
		value:           choice.id,
		searchText:      choice.title + " " + choice.subtitle + " " + choice.detail,
		display:         style.Escape(title) + muted,
		selectedDisplay: style.Styled(title, "accent", "bold") + muted,
	}
}
