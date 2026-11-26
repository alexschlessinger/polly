package main

import (
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
)

// The sessions list in the host. Where the host can ask (see
// repl_pane_ask.go), /sessions shows the sessions there instead of in the
// Sessions modal: the same rows in the same order, less the session on
// screen, which there is nothing to pick about. The choice opens as it does
// from the modal: a tab of its own in front, or the tab of the polly that
// already has it.

// pickTitleWidth is where a session's title is cut in the host's list.
const pickTitleWidth = 120

// askSessions names the sessions list among the host's questions.
const askSessions = "sessions"

// pickSessionInHost asks the host to show the sessions and opens the one
// chosen. False means the host is not asked, and the caller shows them
// itself. Caller holds the visible model's lock.
func (r *managedREPL) pickSessionInHost() bool {
	if r.hostAsking(askSessions) {
		return true // The list is up already.
	}
	if h := &r.panes; h.asked != nil || (!h.askRetryAt.IsZero() && time.Now().Before(h.askRetryAt)) {
		return false
	} else if _, ok := h.prompts(); !ok {
		return false
	}
	p, ok := r.listSessions()
	if !ok {
		return true
	}
	choices := r.sessionChoices(p)
	if len(choices) == 0 {
		r.model.appendNoticeLine("No other sessions")
		return true
	}
	return r.askHost(paneQuestion{title: "Sessions", hint: "Filter sessions", choices: choices}, askSessions, func(answer paneAnswer, asked bool) {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
		switch {
		case !asked:
			if r.model.modal == nil {
				r.openSessionsPickerSelected("")
			}
		case answer.given:
			r.openPickedSession(p, answer.id)
		}
	})
}

// sessionChoices is the rows of the sessions list for a host to show: root
// sessions other than the one on screen, each with its title, its name and
// length, where it is open, and how long ago it was used. Caller holds the
// visible model's lock.
func (r *managedREPL) sessionChoices(p *sessionsPicker) []paneChoice {
	var choices []paneChoice
	for _, row := range p.rows {
		summary := p.infos[row.id]
		md := summary.Metadata
		if summary.ID == "" || summary.ID == p.current || summary.ParentID != "" || md.Parent != "" {
			continue
		}
		title := sessions.DisplayLabel(md)
		var facts []string
		if md.Name != title {
			facts = append(facts, md.Name)
		}
		facts = append(facts, formatSessionMessageCount(summary.MessageCount))
		here, locked := r.summaryTab(summary) >= 0, r.sessionLocked(summary)
		switch {
		case locked:
			facts = append(facts, "open in another polly")
		case here:
			facts = append(facts, "open here")
		}
		choice := paneChoice{
			id:       summary.ID,
			title:    style.Truncate(title, pickTitleWidth),
			subtitle: strings.Join(facts, " · "),
			detail:   formatCompactDuration(time.Since(md.LastUsed)),
			icon:     paneIconPast,
		}
		if here || locked {
			choice.icon = paneIconLive
		}
		choices = append(choices, choice)
	}
	return choices
}

// openPickedSession opens the session chosen from the host's list, as the
// Sessions modal does: one another polly has brings that polly's tab to the
// front, and is refused where it cannot be found. Caller holds the visible
// model's lock.
func (r *managedREPL) openPickedSession(p *sessionsPicker, id string) {
	info, ok := p.infos[id]
	if !ok {
		return
	}
	target := sessions.ViewTarget{ID: info.ID, Name: info.Metadata.Name}
	if !r.sessionLocked(info) {
		r.requestOpenTargetLocked(target)
		return
	}
	refuse := func() { r.model.appendErrorLine(target.Name + " is open in another polly") }
	if !r.jumpToHolder(target, func(jumped bool) {
		if !jumped {
			r.model.mu.Lock()
			defer r.model.mu.Unlock()
			refuse()
		}
	}) {
		refuse()
	}
}
