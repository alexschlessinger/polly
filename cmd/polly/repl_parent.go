package main

import (
	"context"
	"image"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
)

// Parent navigation reuses the session opener, but must never create a new
// session when the saved parent has expired or been deleted.
type existingSessionTargetKey struct{}

// requestParentLocked handles the divider link and /parent. Caller holds the
// visible model's lock; the metadata read runs off the event loop.
func (r *managedREPL) requestParentLocked() {
	if r.workspace().inspector.open {
		r.inspectorAction("parent")
		return
	}
	child := r.visibleTab()
	if parent := r.liveParent(child); parent != nil {
		r.requestShowTabLocked(r.tabIndexOfModel(parent.model))
		return
	}
	if child.parentName == "" || child.childView == nil && (child.state == nil || child.state.session == nil) {
		r.model.appendNoticeLine("This session has no parent")
		return
	}
	if r.opener == nil || !r.canOpenLocked() {
		return
	}
	// Resolve both ends by stable identity. A deleted parent's display handle
	// may have been reused; it is never a navigation fallback.
	store, session, id := child.state.sessionStore, child.state.session, child.viewID()
	parentTarget := func(ctx context.Context) (sessions.ViewTarget, error) {
		if id == "" {
			// Old host sessions without stable identities retain their legacy
			// existing-only navigation behavior.
			md, err := session.GetMetadata(ctx)
			if err != nil || md == nil {
				return sessions.ViewTarget{}, err
			}
			return sessions.ViewTarget{Name: md.Parent}, nil
		}
		summary, err := sessionSummary(ctx, store, id)
		if err != nil {
			return sessions.ViewTarget{}, err
		}
		if summary.ParentID == "" {
			return sessions.ViewTarget{}, sessions.ErrSessionNotFound
		}
		parent, err := sessionSummary(ctx, store, summary.ParentID)
		if err != nil {
			return sessions.ViewTarget{}, err
		}
		return sessions.ViewTarget{ID: parent.ID, Name: parent.Metadata.Name}, nil
	}
	r.background(func() {
		target, err := parentTarget(r.work.ctx)
		name := target.Name
		r.postUI(r.work.ctx, func() {
			if r.model != child.model || r.quitting {
				return
			}
			m := child.model
			m.mu.Lock()
			defer m.mu.Unlock()
			if err != nil {
				m.appendErrorLine("could not find parent: " + err.Error())
				return
			}
			if name == "" {
				m.appendNoticeLine("This session has no parent")
				return
			}
			child.parentName, m.status.parentName = name, name
			for i, tab := range r.tabs {
				if target.ID != "" && tab.viewID() == target.ID || target.ID == "" && tab.name == name {
					r.requestShowTabLocked(i)
					return
				}
			}
			if r.canOpenLocked() {
				ctx := context.WithValue(r.runCtx, existingSessionTargetKey{}, true)
				if target.ID != "" {
					ctx = context.WithValue(ctx, childViewIdentityKey{}, target.ID)
				}
				r.beginOpenContextLocked(ctx, name, false)
			}
		})
	})
}

// dividerRow is the rule above the composer. In an agent tab it carries the
// link back to the caller, whose mouse target follows the layout, including
// multiline input and short screens. Caller holds m.mu.
func (m *replModel) dividerRow(l frameLayout, inspectorFocused ...bool) string {
	m.parentLink = image.Rectangle{}
	if l.dividerRows == 0 || l.width <= 0 {
		return ""
	}
	inactive := len(inspectorFocused) > 0 && inspectorFocused[0]
	rule := func(x, cols int) string { return m.composerBoundaryRule(x, cols, inactive) }
	if m.status.parentName == "" {
		return rule(0, l.width)
	}
	// Inside a full-width frame the link sits past the corner glyphs.
	col := 0
	if l.chrome.joined && l.chrome.main.Empty() {
		col = min(2, l.width-1)
	}
	label := rw.Truncate("← Back to caller", l.width-col, "…")
	cols := rw.StringWidth(label)
	y := l.composerRow(0) - 1
	m.parentLink = image.Rect(col, y, col+cols, y+1)
	tail := l.width - col - cols
	if tail > 0 {
		label += " "
		tail--
	}
	return rule(0, col) + style.Styled(label, "accent", "") + rule(l.width-tail, tail)
}
