package main

import (
	"context"
	rw "github.com/mattn/go-runewidth"
	"image"
	"strings"
)

// Parent navigation reuses the session opener, but must never create a new
// session when the saved parent has expired or been deleted.
type existingSessionTargetKey struct{}

// requestParentLocked handles the divider link and /parent. Caller holds the
// visible model's lock; the metadata read runs off the event loop.
func (r *managedREPL) requestParentLocked() {
	child := r.visibleTab()
	if parent := r.liveParent(child); parent != nil {
		r.requestShowTabLocked(r.tabIndexOfModel(parent.model))
		return
	}
	if child.parentName == "" || child.state == nil || child.state.session == nil {
		r.model.appendNoticeLine("this session has no parent")
		return
	}
	if r.opener == nil || !r.canOpenLocked() {
		return
	}
	session := child.state.session
	r.background(func() {
		md, err := session.GetMetadata(r.work.ctx)
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
			if md == nil || md.Parent == "" {
				m.appendNoticeLine("this session has no parent")
				return
			}
			child.parentName, m.status.parentName = md.Parent, md.Parent
			if i := r.tabIndexOf(md.Parent); i >= 0 {
				r.requestShowTabLocked(i)
				return
			}
			if r.canOpenLocked() {
				ctx := context.WithValue(r.runCtx, existingSessionTargetKey{}, true)
				r.beginOpenContextLocked(ctx, md.Parent, false)
			}
		})
	})
}

// dividerRow anchors parent navigation immediately above the composer. Its
// mouse target follows the layout, including multiline input and short screens.
// Caller holds m.mu.
func (m *replModel) dividerRow(l frameLayout) string {
	m.parentLink = image.Rectangle{}
	if l.dividerRows == 0 || l.width <= 0 {
		return ""
	}
	if m.status.parentName == "" {
		return styled(strings.Repeat("─", l.width), "muted", "")
	}
	label := rw.Truncate("← Back to caller", l.width, "…")
	cols := rw.StringWidth(label)
	y := l.composerRow(0) - 1
	m.parentLink = image.Rect(0, y, cols, y+1)
	rest := ""
	if cols < l.width {
		rest = " " + strings.Repeat("─", l.width-cols-1)
	}
	return styled(label, "accent", "") + styled(rest, "muted", "")
}
