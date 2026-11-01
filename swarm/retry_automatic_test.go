package swarm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
)

func TestMemberStallRecoversWithoutExplicitContinuation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			failure := answer("")
			failure.SetError(&streaming.StallError{Timeout: time.Second})
			return failure
		}
		return answer("recovered automatically")
	})
	r := runtimeTest(t, model, 1, 4)
	r.config.Request.RetryPolicy = &llm.RetryPolicy{MaxRetries: 2, InitialDelay: time.Microsecond, MaxDelay: time.Microsecond}
	r.UpdateDefaults(r.config.Request, r.config.Agent, r.config.Instructions)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Worker", Task: "work", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	execution := state.Executions[state.Members[result.Session].Execution]
	if calls.Load() != 2 || execution.Status != "completed" || execution.RetryPause != nil || execution.Iterations != 1 || execution.ResumedBy != "" {
		t.Fatalf("automatic recovery changed execution/allowance: calls=%d execution=%+v", calls.Load(), execution)
	}
}
