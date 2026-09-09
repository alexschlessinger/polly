package main

import (
	"context"
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
			r.runTabCommand("/spawn --read-only inspect")
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
			var acknowledgement func()
			select {
			case acknowledgement = <-r.uiTasks:
			case <-time.After(3 * time.Second):
				t.Fatal("send acknowledgement did not arrive")
			}
			// The user can reopen the composer and start the next draft before the
			// queued acknowledgement is processed on the UI loop.
			if next != "" {
				w.setAgentDraft(target.key(), "an edit")
				w.setAgentDraft(target.key(), next)
			}
			acknowledgement()
			if got := w.agentDrafts[target.key()]; got != next {
				t.Fatalf("send acknowledgement erased newer draft: got %q", got)
			}
		})
	}
}
