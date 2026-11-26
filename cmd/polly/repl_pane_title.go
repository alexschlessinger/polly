package main

import (
	"context"
	"time"
)

// The tab polly runs in is named after its session, where a pane host has
// tabs to name: a row of pollys is told apart by what each is working on. A
// name someone else gave the tab is left alone (see paneOpener.title), and
// the host's own name comes back when polly leaves.

// titlePane names polly's tab label, the visible session's title, once per
// change. name is the session's name, which a tab polly was launched into
// already carries.
func (r *managedREPL) titlePane(label, name string) {
	h := &r.panes
	if label == "-" {
		label = ""
	}
	if h.opener == nil || h.titling || label == "" || label == h.titled {
		return
	}
	was := h.titled
	if was == "" {
		was = name
	}
	opener := h.opener
	h.titling = true
	if !r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		// A host that cannot name the tab is not asked again for this title.
		_ = opener.title(ctx, was, label)
		r.postUI(r.work.ctx, func() { h.titling, h.titled = false, label })
	}) {
		h.titling = false
	}
}

// untitlePane gives the tab back its own name as the run ends. The loop has
// stopped by now, so this waits for the host, briefly.
func (r *managedREPL) untitlePane() {
	h := &r.panes
	if h.opener == nil || h.titled == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.opener.title(ctx, h.titled, "")
	h.titled = ""
}
