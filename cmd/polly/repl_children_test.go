package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// childTestRuns is a turn runner for saved-view and historical report tests. A prompt of "delegate"
// stands for a parent turn blocked in a tool call until release closes;
// "wait" blocks until the turn is canceled; "slow" waits for slow to close
// and then replies; a prompt in brackets is a report turn on the parent,
// whose message is recorded; anything else replies "found <prompt>".
type childTestRuns struct {
	release chan struct{}
	slow    chan struct{}
}

func (c *childTestRuns) run(ctx context.Context, prompt string, turnUI TurnUI) error {
	switch {
	case prompt == "delegate":
		<-c.release
		return nil
	case prompt == "wait":
		<-ctx.Done()
		return context.Cause(ctx)
	case prompt == "slow":
		<-c.slow
	}
	turnUI.AppendAssistantText("found " + prompt)
	turnUI.FinishTextTurn()
	turnUI.RecordTurnTokens(7, 3)
	return nil
}

func newChildTestREPL(t *testing.T) (*managedREPL, *childTestRuns) {
	t.Helper()
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	runs := &childTestRuns{release: make(chan struct{}), slow: make(chan struct{})}
	r.runTurn = runs.run
	return r, runs
}

func runUITask(t *testing.T, r *managedREPL) {
	t.Helper()
	select {
	case task := <-r.uiTasks:
		task()
	case <-time.After(5 * time.Second):
		t.Fatal("no UI task arrived")
	}
}

// settleUntil drives the loop's settle step on each wake until cond holds.
// Wakes coalesce, so a single wake may precede the goroutine it announces.
func settleUntil(t *testing.T, r *managedREPL, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-r.tabEvents:
		case task := <-r.uiTasks:
			task()
		case <-deadline:
			t.Fatal("the loop did not reach the expected state")
		}
		if err := r.settleTabs(context.Background(), r.runTurn); err != nil {
			t.Fatal(err)
		}
	}
}

func settled(tab *replTab) func() bool {
	return func() bool { return tab.turnDone == nil }
}

func TestAltKeysSwitchTabs(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "a-work", "b-work", "c-work")
	press := func(id string) {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
		r.applyTabRequests()
	}
	for _, step := range []struct {
		key  string
		want int
	}{{"<M-1>", 0}, {"<M-]>", 1}, {"<M-[>", 0}, {"<M-[>", 2}, {"<M-7>", 2}, {"<M-2>", 1}} {
		press(step.key)
		if got := r.visibleTabIndex(); got != step.want {
			t.Fatalf("after %s visible tab = %d, want %d", step.key, got, step.want)
		}
	}
	if _, ok := tabShortcut("<M-x>", 0, 3); ok {
		t.Fatal("an unrelated Alt key switched tabs")
	}
	if i, ok := tabShortcut("<M-[>", -1, 1); !ok || i != 0 {
		t.Fatalf("Alt-[ on a lone placeholder tab = %d %v", i, ok)
	}
}

// Reports the REPL composed for the parent read as notices, live and after a
// reload: the header line only, no gutter, and never the session trailer.
func TestHydratedAgentReportsReadAsNotices(t *testing.T) {
	marked := messages.ChatMessage{
		Role:     messages.MessageRoleUser,
		Content:  "agent helper finished\nfound it\n\n(agent session helper · 7 in / 3 out)",
		Metadata: map[string]any{messages.MetadataKeyAgentReport: true},
	}
	legacy := messages.ChatMessage{
		Role:    messages.MessageRoleUser,
		Content: "agent one canceled\nhalf done\n\n(agent session one)\n\nagent two failed: boom\n(the agent returned no reply)\n\n(agent session two)",
	}
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "delegate"},
		{Role: messages.MessageRoleAssistant, Content: "on it"},
		marked,
		{Role: messages.MessageRoleAssistant, Content: "thanks"},
		legacy,
		{Role: messages.MessageRoleAssistant, Content: "noted"},
	}, "parent")
	got := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	for _, want := range []string{"▎ delegate", "\nagent helper finished\n", "\n2 agent reports\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hydrated transcript %q missing %q", got, want)
		}
	}
	for _, leaked := range []string{"(agent session", "found it", "▎ agent", "▎ 2 agent", "half done"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("hydrated transcript leaked %q: %q", leaked, got)
		}
	}
	if m.restoredDraft != nil || !m.userPromptSeen {
		t.Fatalf("report notices changed draft or prompt state: draft=%#v seen=%v", m.restoredDraft, m.userPromptSeen)
	}
}
