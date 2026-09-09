package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
)

func savedAgentREPL(t *testing.T, parentOpen bool) *managedREPL {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	parent, err := store.Acquire(context.Background(), "parent-work", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(context.Background(), "saved-agent", sessions.AcquireOptions{Parent: "parent-work"})
	if err != nil {
		t.Fatal(err)
	}
	testAddMessages(t, parent, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "delegate"}})
	testAddMessages(t, child, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}, {Role: messages.MessageRoleAssistant, Content: "finished inspection"}})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	names := []string{"saved-agent"}
	if parentOpen {
		names = []string{"parent-work", "saved-agent"}
	}
	return newTabTestREPL(t, store, names...)
}

func TestReopenedAgentClosesOnLeavingButKeepsUserWork(t *testing.T) {
	for _, mode := range []string{"inspect", "draft", "followup"} {
		t.Run(mode, func(t *testing.T) {
			r := savedAgentREPL(t, true)
			child := r.visibleTab()
			if child.parentName != "parent-work" || child.model.status.parentName != "parent-work" {
				t.Fatal("reopened agent lost parent")
			}
			switch mode {
			case "draft":
				child.model.ed.setText("one more question")
			case "followup":
				child.model.beginTurn("one more question")
				r.startTurn(context.Background(), "one more question", func(context.Context, string, TurnUI) error { return nil })
				settleUntil(t, r, settled(child))
			}
			r.runParent()
			want := 1
			if mode != "inspect" {
				want = 2
			}
			if len(r.tabs) != want || r.visibleTab().name != "parent-work" {
				t.Fatalf("after leaving %s: %d tabs, visible %s", mode, len(r.tabs), r.visibleTab().name)
			}
			if mode == "draft" && child.model.ed.text() != "one more question" {
				t.Fatal("draft lost")
			}
		})
	}
}

func TestParentCommandReopensSavedParentAndFollowsRename(t *testing.T) {
	r := savedAgentREPL(t, false)
	parent, err := r.state.sessionStore.Acquire(context.Background(), "parent-work", sessions.AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Rename(context.Background(), "renamed-parent"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	r.runParent()
	runUITask(t, r)
	select {
	case result := <-r.openDone:
		r.finishOpen(result)
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not open")
	}
	if len(r.tabs) != 1 || r.visibleTab().name != "renamed-parent" {
		t.Fatalf("parent navigation: %d tabs, visible %s", len(r.tabs), r.visibleTab().name)
	}
}

func TestParentNavigationDoesNotCreateMissingSession(t *testing.T) {
	r := savedAgentREPL(t, false)
	if err := r.state.sessionStore.Delete(context.Background(), "parent-work"); err != nil {
		t.Fatal(err)
	}
	r.runParent()
	runUITask(t, r)
	select {
	case result := <-r.openDone:
		if result.err == nil {
			t.Fatal("missing parent was created")
		}
		r.finishOpen(result)
	case <-time.After(5 * time.Second):
		t.Fatal("parent open did not finish")
	}
	if r.visibleTab().name != "saved-agent" {
		t.Fatal("failed navigation left child")
	}
}

func TestParentDividerFollowsLayout(t *testing.T) {
	r := savedAgentREPL(t, true)
	m := r.model
	for _, input := range []string{"", "one\ntwo\nthree"} {
		m.ed.setText(input)
		for _, width := range []int{1, 4, 12, 24, 80, 120} {
			l := r.frameLayoutFor(width, 24)
			row := plainStyledText(m.dividerRow(l))
			if rw.StringWidth(row) != width || (width >= 24 && !strings.HasSuffix(row, "─")) {
				t.Fatalf("width %d divider: %q", width, row)
			}
			p := m.parentLink
			if p.Empty() || p.Min.X != 0 || p.Max.X > width || p.Min.Y != l.composerRow(0)-1 {
				t.Fatalf("invalid target: %+v, layout %+v", p, l)
			}
			if strings.Contains(plainStyledText(m.statusRow(width)), "Back to caller") {
				t.Fatal("navigation remains in status bar")
			}
		}
	}
	for _, quiet := range []bool{false, true} {
		m.quiet = quiet
		height := 24
		if !quiet {
			height = 2
		}
		if row := m.dividerRow(r.frameLayoutFor(80, height)); row != "" || !m.parentLink.Empty() {
			t.Fatal("hidden divider retained navigation")
		}
	}
	m.quiet = false
	m.status.parentName = ""
	// A root session keeps the rule but has nowhere to go back to.
	l := r.frameLayoutFor(80, 24)
	row := plainStyledText(m.dividerRow(l))
	if row != strings.Repeat("─", 80) || !m.parentLink.Empty() || l.dividerRows != 1 {
		t.Fatalf("ordinary session rule = %q link=%v layout=%+v", row, m.parentLink, l)
	}
}
