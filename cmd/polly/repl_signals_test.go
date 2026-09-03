package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// waitTabEvent waits for something to wake the event loop.
func waitTabEvent(t *testing.T, r *managedREPL) {
	t.Helper()
	select {
	case <-r.tabEvents:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop was not woken")
	}
}

// startHiddenTurn starts runTurn on the visible tab and switches to tab 1,
// leaving the turn running out of sight on the tab it started on.
func startHiddenTurn(t *testing.T, r *managedREPL, runTurn turnRunner) *replTab {
	t.Helper()
	tab := r.visibleTab()
	r.model.mu.Lock()
	r.model.beginTurn("keep going")
	r.model.mu.Unlock()
	r.startTurn(context.Background(), "keep going", runTurn)
	r.runTabCommand("/tab 1")
	if r.visibleTab() == tab {
		t.Fatal("/tab 1 did not leave the busy tab")
	}
	return tab
}

// signal runs what the event loop does before a render: relay hidden tabs'
// signals.
func (r *managedREPL) signal() {
	r.relayTabSignals()
}

func TestHiddenTurnOutcomeSignalsTheVisibleTab(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	release := make(chan struct{})
	runTurn := func(context.Context, string, TurnUI) error {
		<-release
		return nil
	}
	busy := startHiddenTurn(t, r, runTurn)
	r.signal()

	close(release)
	waitTabEvent(t, r)
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "current-work done · ") {
		t.Fatalf("visible transcript lacks the settle notice: %q", got)
	}
	if got := strings.Join(r.tabLines(), "\n"); !strings.Contains(got, "2  current-work  done") {
		t.Fatalf("tab list does not show the unseen outcome: %q", got)
	}
	if got := r.model.frameTitle(); got != "polly · first-work" {
		t.Fatalf("title left the visible tab: %q", got)
	}
	if strings.Contains(plainStyledText(busy.model.fullTranscript()), "current-work done") {
		t.Fatal("the settled tab got a notice about itself")
	}

	// Showing the tab is seeing the news: its listed activity clears.
	r.runTabCommand("/tab 2")
	r.signal()
	if got := strings.Join(r.tabLines(), "\n"); !strings.Contains(got, "2  current-work  current") || strings.Contains(got, "done") {
		t.Fatalf("tab list after showing the tab: %q", got)
	}
	r.runTabCommand("/tab 1")
	r.signal()
	if got := strings.Count(plainStyledText(r.model.fullTranscript()), "current-work done"); got != 1 {
		t.Fatalf("settle notice appeared %d times, want once", got)
	}
}

func TestHiddenTurnFailureSignalsTheVisibleTab(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	runTurn := func(context.Context, string, TurnUI) error {
		return errors.New("boom")
	}
	startHiddenTurn(t, r, runTurn)
	waitTabEvent(t, r)
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "current-work failed · boom") {
		t.Fatalf("visible transcript lacks the failure notice: %q", got)
	}
	if got := strings.Join(r.tabLines(), "\n"); !strings.Contains(got, "2  current-work  failed") {
		t.Fatalf("tab list does not show the failure: %q", got)
	}
}

func TestHiddenApprovalSignalsAndWakesTheLoop(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	r.config.Confirm = true
	calls := []messages.ChatMessageToolCall{{ID: "1", Name: "bash", Arguments: `{"command":"ls -la"}`}}
	release := make(chan struct{})
	answered := make(chan []bool, 1)
	runTurn := func(_ context.Context, _ string, turnUI TurnUI) error {
		<-release
		answered <- turnUI.ApproveToolCalls(calls)
		return nil
	}
	busy := startHiddenTurn(t, r, runTurn)

	// Asking for approval out of sight wakes the loop, which tells the
	// visible tab where to answer.
	close(release)
	waitTabEvent(t, r)
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	r.signal()
	transcript := plainStyledText(r.model.fullTranscript())
	if !strings.Contains(transcript, "current-work needs approval: bash") || !strings.Contains(transcript, "· /tab current-work") {
		t.Fatalf("visible transcript lacks the approval notice: %q", transcript)
	}
	if got := strings.Join(r.tabLines(), "\n"); !strings.Contains(got, "2  current-work  approval needed") {
		t.Fatalf("tab list does not show the approval: %q", got)
	}
	if got := r.model.frameTitle(); got != "polly · first-work" {
		t.Fatalf("title left the visible tab: %q", got)
	}

	// The prompt waits on the tab that asked; showing it is how to answer.
	r.runTabCommand("/tab 2")
	r.signal()
	if r.model != busy.model || r.model.approval == nil {
		t.Fatal("the approval is not waiting on its tab")
	}
	if got := plainStyledText(r.model.inputDisplay()); !strings.Contains(got, "allow bash ls -la") {
		t.Fatalf("approval prompt not shown: %q", got)
	}
	r.model.mu.Lock()
	r.model.denyApprovalLocked()
	r.model.mu.Unlock()
	select {
	case got := <-answered:
		if len(got) != 1 || got[0] {
			t.Fatalf("answer = %v, want denied", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not get its answer")
	}
	waitTabEvent(t, r)
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	if busy.turnDone != nil || r.model.busy {
		t.Fatal("the answered turn did not settle")
	}
}

func TestTabSignalsWaitForTheVisibleTurn(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	release := make(chan struct{})
	runTurn := func(context.Context, string, TurnUI) error {
		<-release
		return nil
	}
	startHiddenTurn(t, r, runTurn)
	done := startBlockedTurn(t, r)

	close(release)
	waitTabEvent(t, r)
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); strings.Contains(got, "current-work done") {
		t.Fatalf("notice landed inside the visible turn: %q", got)
	}

	settleBlockedTurn(t, r, done)
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "current-work done · ") {
		t.Fatalf("notice did not follow the visible turn: %q", got)
	}
}

func TestShowingATabDropsItsPendingSignals(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	release := make(chan struct{})
	runTurn := func(context.Context, string, TurnUI) error {
		<-release
		return nil
	}
	startHiddenTurn(t, r, runTurn)
	done := startBlockedTurn(t, r)
	close(release)
	waitTabEvent(t, r)
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	r.signal()

	// Seen before the visible turn let the notice through: nothing to say.
	r.runTabCommand("/tab 2")
	r.runTabCommand("/tab 1")
	settleBlockedTurn(t, r, done)
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); strings.Contains(got, "current-work done") {
		t.Fatalf("a seen outcome was still announced: %q", got)
	}
}

func TestApprovalSignalsDoNotWaitForTheVisibleTurn(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	r.config.Confirm = true
	calls := []messages.ChatMessageToolCall{{ID: "1", Name: "bash", Arguments: `{"command":"ls"}`}}
	release := make(chan struct{})
	answered := make(chan []bool, 1)
	runTurn := func(_ context.Context, _ string, turnUI TurnUI) error {
		<-release
		answered <- turnUI.ApproveToolCalls(calls)
		return nil
	}
	busy := startHiddenTurn(t, r, runTurn)
	done := startBlockedTurn(t, r)

	// The visible tab is mid-turn, yet a stalled hidden turn must be heard.
	close(release)
	waitTabEvent(t, r)
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "current-work needs approval: bash") {
		t.Fatalf("approval notice waited for the visible turn: %q", got)
	}
	busy.model.mu.Lock()
	busy.model.denyApprovalLocked()
	busy.model.mu.Unlock()
	<-answered
	waitTabEvent(t, r)
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); strings.Contains(got, "current-work done") {
		t.Fatalf("a settle notice landed inside the visible turn: %q", got)
	}
	settleBlockedTurn(t, r, done)
	r.signal()
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "current-work done · ") {
		t.Fatalf("the settle notice did not follow the visible turn: %q", got)
	}
}
