package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

func (m *replModel) setSessionTitle(md *sessions.Metadata) {
	if m.status.title == md.Title && m.status.titleSource == md.TitleSource {
		return
	}
	m.status.title, m.status.titleSource = md.Title, md.TitleSource
	m.visual.invalidate()
}

// SessionTitleChanged runs on a tool goroutine. The event reads current
// storage state, never a title captured before a concurrent manual edit.
func (t *gotuiTurnUI) SessionTitleChanged(session sessions.Session) {
	identity, ok := session.(sessions.ViewIdentity)
	if !ok || t.repl == nil {
		return
	}
	id := identity.ViewID()
	t.repl.postUI(t.repl.work.ctx, func() { t.repl.refreshSessionTitle(id, nil) })
}

// refreshSessionTitle runs on the event loop. locked is the model whose lock
// the calling command or modal already holds; queued tool events hold none.
func (r *managedREPL) refreshSessionTitle(id string, locked *replModel) {
	if id == "" || r.state == nil {
		return
	}
	summaries, err := r.state.sessionStore.ListSummaries(r.work.ctx)
	if err != nil {
		return
	}
	var md *sessions.Metadata
	for _, summary := range summaries {
		if summary.ID == id {
			md = summary.Metadata
			break
		}
	}
	if md == nil {
		return
	}
	for _, tab := range r.tabs {
		if tab.viewID() != id {
			continue
		}
		if tab.model != locked {
			tab.model.mu.Lock()
		}
		tab.model.setSessionTitle(md)
		if tab.model != locked {
			tab.model.mu.Unlock()
		}
		if tab.childView != nil {
			copy := *tab.childView
			copy.Metadata = md
			// Keep the previous revision so transcript refreshes are not skipped.
			tab.childView = &copy
		}
	}
	for _, w := range r.workspaceTabs() {
		if w.workspace == nil {
			continue
		}
		v := w.workspace.inspector.current
		if v != nil && v.info != nil && v.info.ID == id {
			copy := *v.info
			copy.Metadata = md
			v.info = &copy
		}
	}
	if r.model != locked {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
	}
	if p, m := r.sessionsPicker, r.model.modal; p != nil && m != nil && p.modal == m {
		selected := pickerSelection(m)
		p.merge(summaries, m.expanded)
		r.refreshSessionsPickerItems(p, m, selected)
	}

}

func (r *managedREPL) openSessionTitleInput(summary sessions.SessionSummary) {
	md := summary.Metadata
	latest, err := r.state.sessionStore.ListSummaries(r.work.ctx)
	if err != nil {
		r.model.appendNoticeLine("Title unavailable · " + err.Error())
		return
	}
	found := false
	for _, item := range latest {
		if item.ID == summary.ID && summary.ID != "" {
			summary, md, found = item, item.Metadata, true
			break
		}
	}
	if !found {
		r.model.appendNoticeLine("Title unavailable · session no longer exists")
		return
	}
	m := &replModal{
		title: "Edit title", inputMode: true, width: 72,
		helper:   "Enter save · Esc back",
		onCancel: func() { r.openSessionsPickerSelected(summary.ID) },
		onSubmit: func(title string) { r.editSessionTitle(summary, title) },
	}
	m.input.setText(sessions.DisplayLabel(md))
	r.openModal(m)
}

func (r *managedREPL) editSessionTitle(summary sessions.SessionSummary, title string) {
	ctx := r.work.ctx
	var target sessions.Session
	for _, tab := range r.tabs {
		if (summary.ID != "" && tab.viewID() == summary.ID) && tab.state != nil {
			target = tab.state.session
			if target != nil {
				break
			}
		}
	}
	closeTarget := target == nil
	var err error
	if target == nil {
		// Refresh lease status before acquiring; the picker may have been
		// open while another process took ownership or renamed the handle.
		fresh, listErr := r.state.sessionStore.ListSummaries(ctx)
		err = listErr
		found := false
		for _, item := range fresh {
			if item.ID == summary.ID && summary.ID != "" {
				summary = item
				found = true
				break
			}
		}
		if err == nil && !found {
			err = sessions.ErrSessionNotFound
		}
		if err == nil && summary.InUse {
			err = sessions.ErrSessionInUse
		} else if err == nil {
			// Bound the remaining ownership race instead of waiting ten
			// seconds on the UI goroutine for somebody else's lease.
			acquireCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			target, err = r.state.sessionStore.Acquire(acquireCtx, summary.Metadata.Name, sessions.AcquireOptions{ExistingOnly: true, ExpectedID: summary.ID})
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				err = sessions.ErrSessionInUse
			}
		}
	}
	if err == nil {
		if setter, ok := target.(sessions.TitleSession); ok {
			_, err = setter.SetTitle(ctx, title, sessions.TitleSourceUser)
		} else {
			err = fmt.Errorf("session titles are unavailable")
		}
		if closeTarget {
			if closeErr := target.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		r.model.appendNoticeLine("Title update failed · " + err.Error())
	}
	r.refreshSessionTitle(summary.ID, r.model)
	r.openSessionsPickerSelected(summary.ID)
}
