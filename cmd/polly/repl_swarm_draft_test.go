package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestSuccessfulSendPreservesNewInspectorDraft(t *testing.T) {
	for _, next := range []string{"new unsent draft", "first request", ""} {
		t.Run(next, func(t *testing.T) {
			r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), nil)
			r.runTabCommand("/spawn --read-only --review inspect")
			runUITask(t, r)
			s := waitSwarmIdle(t, r.state.swarm)
			var member *swarm.Member
			for _, m := range s.Members {
				member = m
				break
			}
			target := viewTarget{session: sessions.ViewTarget{ID: member.ID, Name: member.Name}}
			r.inspect(target)
			waitInspector(t, r, 140)
			w := r.workspace()
			w.agentDrafts = map[string]string{target.key(): "first request"}
			r.sendInspectorMessage(w, target, "first request", "")
			// The user can reopen the composer and start the next draft before the
			// queued acknowledgement is processed on the UI loop.
			if next != "" {
				w.setAgentDraft(target.key(), "an edit")
				w.setAgentDraft(target.key(), next)
			}
			deadline := time.After(3 * time.Second)
			for !strings.Contains(strings.Join(transcriptTexts(r.model), "\n"), "follow-up sent") {
				select {
				case fn := <-r.uiTasks:
					fn()
				case <-deadline:
					t.Fatalf("send acknowledgement did not arrive: %v", transcriptTexts(r.model))
				}
			}
			if got := w.agentDrafts[target.key()]; got != next {
				t.Fatalf("send acknowledgement erased newer draft: got %q", got)
			}
			s = waitSwarmIdle(t, r.state.swarm)
			if len(s.Executions) != 2 {
				t.Fatal("inspector message did not restart the idle member")
			}
			for _, mail := range s.Messages {
				if mail.Start && !mail.Delivered {
					t.Fatal("inspector follow-up was not delivered")
				}
			}
		})
	}
}
