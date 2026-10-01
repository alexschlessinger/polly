package swarm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// A run the context budget could not reopen with more input still has its
// answer; a structured member's answer is its accepted completion, and a
// blank final is no answer at all.
func TestAnswerStandsWhenInputHasNoRoom(t *testing.T) {
	t.Parallel()
	accepted := &structuredResultState{accepted: &StructuredCompletion{Task: "t", Value: "v"}}
	answered := &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "the answer"}
	blank := &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: " "}
	for _, c := range []struct {
		name       string
		structured *structuredResultState
		final      *messages.ChatMessage
		delivered  bool
		stands     bool
	}{
		{"plain member", nil, answered, false, true},
		{"blank final", nil, blank, false, false},
		{"no final", nil, nil, false, false},
		{"delivered through the response tool", nil, blank, true, true},
		{"accepted completion", accepted, blank, false, true},
		{"pending correction", &structuredResultState{}, answered, true, false},
	} {
		if got := answerStands(c.structured, c.final, c.delivered); got != c.stands {
			t.Errorf("%s: answerStands = %v, want %v", c.name, got, c.stands)
		}
	}
}

// A blank final whose one retry the context budget turns away leaves the
// member incomplete, as when the retry is spent; it is not a completed result.
func TestBlankFinalWithoutRoomForItsRetryIsIncomplete(t *testing.T) {
	t.Parallel()
	// The budgets that admit the blank final but not its retry span about a
	// hundred tokens, wherever the member's system prompt puts them.
	for budget := 2_000; budget <= 12_000; budget += 50 {
		var exhausted atomic.Bool
		r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
			return answer("")
		}), 1, 1)
		req := r.config.Request
		req.MaxContextTokens = budget
		r.UpdateDefaults(req, r.config.Agent, nil)
		r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
			return &llm.AgentCallbacks{OnError: func(err error) {
				if errors.Is(err, llm.ErrContextExhausted) {
					exhausted.Store(true)
				}
			}}
		}
		result, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
		if !exhausted.Load() {
			continue
		}
		var empty *EmptyResultError
		if !errors.As(err, &empty) {
			t.Fatalf("budget %d: blank final completed as %q (err %v), want an incomplete member", budget, result.Value, err)
		}
		return
	}
	t.Fatal("no budget turned the blank final's retry away")
}

// The parent of a turn whose answer stood without room to reopen it is idle,
// as the host reports the turn completed.
func TestParentOutcomeOfAStandingAnswer(t *testing.T) {
	t.Parallel()
	if lifecycle, detail := parentOutcome(context.Background(), llm.ErrContextExhausted); lifecycle != LifecycleIdle || detail != "" {
		t.Fatalf("outcome = %v %q, want idle", lifecycle, detail)
	}
	joined := errors.Join(llm.ErrContextExhausted, errors.New("persisted"))
	if lifecycle, _ := parentOutcome(context.Background(), joined); lifecycle == LifecycleIdle {
		t.Fatal("a failed checkpoint joined to the standing answer left the parent idle")
	}
}
