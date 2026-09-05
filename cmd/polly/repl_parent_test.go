package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func TestDeliveredVisibleChildClosesOnParentLink(t *testing.T) {
	for _, outcome := range []error{nil, errors.New("provider failed"), context.Canceled} {
		t.Run(string(storedChildReport(subagent.Result{}, outcome).Status), func(t *testing.T) {
			r, _ := newChildTestREPL(t)
			parent := r.visibleTab()
			r.runTurn = func(_ context.Context, _ string, turnUI TurnUI) error {
				turnUI.AppendAssistantText("agent result")
				return outcome
			}
			result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "inspect"}, "")
			runUITask(t, r)
			child := r.tabs[1]
			r.showTab(1)
			settleUntil(t, r, settled(child))
			awaitReport(t, result)
			settleUntil(t, r, func() bool { return child.delivered })
			if r.visibleTab() != child || len(r.tabs) != 2 {
				t.Fatal("visible child closed on delivery")
			}
			bar := plainStyledText(child.model.dividerRow(r.frameLayoutFor(80, 24)))
			if !strings.Contains(bar, "← Back to caller") {
				t.Fatalf("parent link missing: %q", bar)
			}
			target := child.model.parentLink
			r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: target.Min.X, Y: target.Min.Y}})
			r.applyTabRequests()
			if r.visibleTab() != parent || len(r.tabs) != 1 {
				t.Fatal("parent link did not close delivered child and return to parent")
			}
		})
	}
}

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
			r.runTabCommand("/parent")
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
	r.runTabCommand("/parent")
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
	r.runTabCommand("/parent")
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
			if rw.StringWidth(row) != width {
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
	row := plainStyledText(m.dividerRow(r.frameLayoutFor(80, 24)))
	if row != strings.Repeat("─", 80) || !m.parentLink.Empty() {
		t.Fatal("ordinary session has navigation")
	}
}

type failedAgentReportSession struct{ sessions.Session }

func (s *failedAgentReportSession) Report(context.Context, sessions.Report) error {
	return errors.New("report storage unavailable")
}

func TestFailedReportDeliveryKeepsAgentTab(t *testing.T) {
	r, _ := newChildTestREPL(t)
	parent := r.visibleTab()
	spawn := r.opener.spawn
	r.opener.spawn = func(ctx context.Context, parent *conversationState, req subagent.Request) (*conversationState, error) {
		child, err := spawn(ctx, parent, req)
		if err == nil {
			child.session = &failedAgentReportSession{child.session}
		}
		return child, err
	}
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "inspect", Background: true}, "")
	runUITask(t, r)
	child := r.tabs[1]
	awaitReport(t, result)
	settleUntil(t, r, func() bool { return child.turnDone == nil && child.report == nil && !child.reporting })
	r.showTab(r.tabIndexOfModel(child.model))
	r.runTabCommand("/parent")
	if child.delivered || len(r.tabs) != 2 {
		t.Fatal("undelivered report lost its tab")
	}
	if !strings.Contains(plainStyledText(parent.model.fullTranscript()), "could not be delivered") {
		t.Fatal("delivery error not shown")
	}
}
