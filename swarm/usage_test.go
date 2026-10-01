package swarm

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMemberAccountsForDroppedReply(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleInternal {
				t.Error("internal usage record sent to provider")
			}
		}
		reply := answer("done")
		if calls.Add(1) == 1 {
			reply.StopReason = messages.StopReasonToolUse
			for i := range 500 {
				reply.ToolCalls = append(reply.ToolCalls, messages.ChatMessageToolCall{
					ID: fmt.Sprint(i), Name: "swarm_publish", Arguments: `{"text":"` + strings.Repeat("x", 2_000) + `"}`,
				})
			}
		}
		reply.SetTokenUsage(1_000, 20)
		reply.SetPromptCacheUsage(600, 0)
		return reply
	}), 1, 1)
	request := r.config.Request
	request.MaxContextTokens = 50_000
	r.UpdateDefaults(request, r.config.Agent, nil)
	result, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "answer", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	usage := result.Usage
	if calls.Load() != 2 || usage.Samples != 2 || usage.InputTokens == nil || *usage.InputTokens != 2_000 || usage.OutputTokens == nil || *usage.OutputTokens != 40 || usage.CachedInputTokens == nil || *usage.CachedInputTokens != 1_200 {
		t.Fatalf("calls=%d usage=%+v", calls.Load(), usage)
	}
	s, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Publications) != 0 {
		t.Fatal("dropped tool batch executed")
	}
	for _, execution := range s.Executions {
		if execution.Usage.Samples != 2 || execution.Usage.OutputTokens == nil || *execution.Usage.OutputTokens != 40 {
			t.Fatalf("checkpoint lost dropped usage: %+v", execution.Usage)
		}
	}
}
