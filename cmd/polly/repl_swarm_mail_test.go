package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestHydrateSwarmMailDoesNotBecomeUserTurns(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "memory"
		if saved {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "summarize the changes"},
				{Role: messages.MessageRoleAssistant, Content: "Reviewers are working."},
			}
			// Legacy mailbox metadata must work both before and after JSON storage.
			for range resumedTurnLimit + 2 {
				history = append(history,
					messages.ChatMessage{Role: messages.MessageRoleUser, Content: "<peer_messages>private mailbox envelope</peer_messages>", Metadata: map[string]any{"swarm_messages": []string{"mail-id"}}},
					messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Reviewed another result."})
			}
			history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "<peer_messages>trailing envelope</peer_messages>", Metadata: map[string]any{"swarm_messages": []string{"last-mail"}}})
			if saved {
				data, err := json.Marshal(history)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &history); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := json.Marshal(history)
			if start, total, shown := resumedHistoryWindow(history); start != 0 || total != 1 || shown != 1 {
				t.Fatalf("mail changed the user window: start=%d total=%d shown=%d", start, total, shown)
			}
			m := newReplModel()
			m.hydrateHistory(history, "swarm")
			plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
			if strings.Contains(plain, "peer_messages") || !strings.Contains(plain, "summarize the changes") || !strings.Contains(plain, "Reviewed another result.") {
				t.Fatalf("hydration lost visible work or exposed mail: %q", plain)
			}
			if !m.ed.empty() || m.restoredDraft != nil {
				t.Fatalf("mail restored to composer: %q", m.ed.text())
			}
			after, _ := json.Marshal(history)
			if string(before) != string(after) {
				t.Fatal("display filtering changed durable model history")
			}
		})
	}
}

func TestHydrateDoesNotHideUserPastedPeerEnvelope(t *testing.T) {
	text := "<peer_messages>please explain this</peer_messages>"
	for _, metadata := range []map[string]any{nil, {"swarm_messages": "user text"}, {"swarm_messages": []any{17}}, {"swarm_messages": []string{}}} {
		m := newReplModel()
		m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: text, Metadata: metadata}}, "ctx")
		if m.ed.text() != text {
			t.Fatalf("real prompt hidden for metadata %#v", metadata)
		}
	}
}
