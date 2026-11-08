package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func launchLinkModel() *replModel {
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "assign"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{agentCall("selected-call", `{"label":"selected"}`)}}}, "root")
	return m
}

func TestDisplayedAgentLinksSelectOnlyShownCalls(t *testing.T) {
	var rows uint64
	for _, n := range []int{16, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			root := testAcquireSession(t, store, "root")
			rootID := root.(sessions.ViewIdentity).ViewID()
			var selected string
			for i := -1; i < n; i++ {
				child, err := store.Acquire(t.Context(), fmt.Sprintf("child-%04d", i), sessions.AcquireOptions{Parent: "root"})
				if err != nil {
					t.Fatal(err)
				}
				md, err := child.GetMetadata(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				md.SpawnCallID = fmt.Sprintf("call-%d", i)
				if i < 0 {
					md.SpawnCallID = "selected-call"
					selected = child.(sessions.ViewIdentity).ViewID()
				}
				md.Description = strings.Repeat("unrelated", 128)
				if err = child.SetMetadata(t.Context(), md); err != nil {
					t.Fatal(err)
				}
				if err = child.AddMessage(t.Context(), messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: strings.Repeat("history", 128)}); err != nil {
					t.Fatal(err)
				}
				if err = child.Close(); err != nil {
					t.Fatal(err)
				}
			}
			m := launchLinkModel()
			recorder := runtimemetrics.New()
			if err := hydrateDisplayedAgentSessions(runtimemetrics.WithRecorder(t.Context(), recorder), store, rootID, "root", m); err != nil {
				t.Fatal(err)
			}
			linked := projectedAgentRows(m)
			if len(linked[selected]) != 1 {
				t.Fatalf("selected row not linked: %v", linked)
			}
			cost := recorder.Snapshot()
			if cost.Count(runtimemetrics.TranscriptMessagesRead) != 0 || cost.Count(runtimemetrics.WriteTransactions) != 0 {
				t.Fatal("link lookup read transcript or wrote lease", cost)
			}
			if n == 16 {
				rows = cost.Count(runtimemetrics.RowsRead)
			} else if rows != cost.Count(runtimemetrics.RowsRead) {
				t.Fatal("unrelated child catalogue scaled link lookup", cost)
			}
			t.Logf("children=%d shown launches=1 rows=%d", n, cost.Count(runtimemetrics.RowsRead))
			// A stale/reused parent name cannot bind a different durable family.
			wrong := launchLinkModel()
			if err := hydrateDisplayedAgentSessions(t.Context(), store, "foreign-id", "root", wrong); err != nil {
				t.Fatal(err)
			}
			if len(projectedAgentRows(wrong)) != 0 {
				t.Fatal("cross-family launch binding")
			}
			// Duplicate retained children fail closed, including a previously linked row.
			duplicate, err := store.Acquire(t.Context(), "duplicate", sessions.AcquireOptions{Parent: "root"})
			if err != nil {
				t.Fatal(err)
			}
			md, _ := duplicate.GetMetadata(t.Context())
			md.SpawnCallID = "selected-call"
			if err = duplicate.SetMetadata(t.Context(), md); err != nil {
				t.Fatal(err)
			}
			if err = duplicate.Close(); err != nil {
				t.Fatal(err)
			}
			if err = hydrateDisplayedAgentSessions(t.Context(), store, rootID, "root", m); err != nil {
				t.Fatal(err)
			}
			if len(projectedAgentRows(m)) != 0 {
				t.Fatal("ambiguous launch retained a guessed link")
			}
			if err = root.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDisplayedAgentLinksCancellation(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := hydrateDisplayedAgentSessions(ctx, store, "id", "root", launchLinkModel()); err == nil {
		t.Fatal("canceled lookup succeeded")
	}
}
