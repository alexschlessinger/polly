package main

import "github.com/alexschlessinger/pollytool/sessions"

// The runtime keeps its source text and input state. An inactive root's
// expensive wrapped cells belong to the same bounded cache as inspector views.
// Called under the departing model's lock; measurement happens on a worker.
func (r *managedREPL) retireMainProjection(tab *replTab) {
	if tab == nil || !tab.workspaceRoot || !tab.model.visual.valid || tab.viewID() == "" {
		return
	}
	m := tab.model
	display := newReplModel()
	display.visual = m.visual
	m.visual = transcriptVisualCache{revision: m.visual.revision}
	for n := range m.transcript {
		m.transcript[n].codeCache = nil
	}
	entry := &cachedChildView{key: "main:" + tab.viewID(), info: &sessions.SessionView{ID: tab.viewID()}, model: display, used: tab.viewUsed}
	r.background(func() {
		entry.bytes = childViewSize(display)
		r.postUI(r.work.ctx, func() {
			if r.visibleTab() != tab && r.tabIndexOfModel(tab.model) >= 0 {
				r.childViews.put(entry)
			}
		})
	})
}

// Called under the arriving model's lock. Geometry validation remains in the
// conversation renderer; changed content cannot reuse an old projection.
func (r *managedREPL) restoreMainProjection(tab *replTab) {
	if !tab.workspaceRoot {
		return
	}
	if cached := r.childViews.take("main:" + tab.viewID()); cached != nil && cached.model.visual.revision == tab.model.visual.revision {
		tab.model.visual = cached.model.visual
	}
}
