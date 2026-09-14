package main

import (
	"context"
	"image"
	"strings"

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
	// A saved child view reads its stored parent; a live session asks its own.
	var parentName func(context.Context) (string, error)
	if view := child.childView; view != nil {
		store := child.state.sessionStore.(sessions.ViewStore)
		id, revision := view.ID, view.Revision
		parentName = func(ctx context.Context) (string, error) {
			view, err := store.ReadView(ctx, sessions.ViewTarget{ID: id}, revision)
			if err != nil {
				return "", err
			}
			return view.Metadata.Parent, nil
		}
	} else {
		session := child.state.session
		parentName = func(ctx context.Context) (string, error) {
			md, err := session.GetMetadata(ctx)
			if err != nil || md == nil {
				return "", err
			}
			return md.Parent, nil
		}
	}
	r.background(func() {
		name, err := parentName(r.work.ctx)
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
			if i := r.tabIndexOf(name); i >= 0 {
				r.requestShowTabLocked(i)
				return
			}
			if r.canOpenLocked() {
				ctx := context.WithValue(r.runCtx, existingSessionTargetKey{}, true)
				r.beginOpenContextLocked(ctx, name, false)
			}
		})
	})
}

// dividerRow is the rule above the composer. In an agent tab it carries the
// link back to the caller, whose mouse target follows the layout, including
// multiline input and short screens. Caller holds m.mu.
func (m *replModel) dividerRow(l frameLayout) string {
	m.parentLink = image.Rectangle{}
	if l.dividerRows == 0 || l.width <= 0 {
		return ""
	}
	rule := func(n int) string { return style.Styled(strings.Repeat("─", max(0, n)), "muted", "") }
	if m.status.parentName == "" {
		return rule(l.width)
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
	return rule(col) + style.Styled(label, "accent", "") + rule(tail)
}
