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

// dividerRow is the rule above the composer. It carries the caller link on
// the left and the compact checklist on the right when present. Hitboxes
// follow the rule through resizes and multiline input. Caller holds m.mu.
func (m *replModel) dividerRow(l frameLayout, inspectorFocused ...bool) string {
	m.parentLink, m.todoLink = image.Rectangle{}, image.Rectangle{}
	if l.dividerRows == 0 || l.width <= 0 {
		return ""
	}
	inactive := len(inspectorFocused) > 0 && inspectorFocused[0]
	rule := func(x, cols int) string { return m.composerBoundaryRule(x, cols, inactive) }
	y := l.composerRow(0) - 1
	row, x := "", 0
	if m.status.parentName != "" {
		// Inside a full-width frame the link sits past the corner glyphs.
		col := 0
		if l.chrome.joined && l.chrome.main.Empty() {
			col = min(2, l.width-1)
		}
		label := rw.Truncate("← Back to caller", l.width-col, "…")
		x = col + rw.StringWidth(label)
		m.parentLink = image.Rect(col, y, x, y+1)
		row = rule(0, col) + style.Styled(label, "accent", "")
		if x < l.width {
			row += " "
			x++
		}
	}
	// Keep the prompt-edge cue, a space on each side of the label, and
	// the rightmost rule cell (the frame corner with an open inspector).
	end := l.width - 2
	label := m.planProgress.dividerLabel(end - max(x, composerBoundaryAccentCols) - 1)
	if label == "" {
		return row + rule(x, l.width-x)
	}
	start := end - rw.StringWidth(label)
	m.todoLink = image.Rect(start, y, end, y+1)
	return row + rule(x, start-x-1) + " " + style.Styled(label, "muted", "") + " " + rule(end+1, 1)
}
