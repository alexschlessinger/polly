package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestBeforeFirstRequestFollowsTheOnlyProjection(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore()
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "now", Name: "noop", Arguments: `{}`}}},
	}}
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "noop", Run: func(context.Context, tools.Args) (string, error) { return "ok", nil }}})
	defer registry.Close()
	agent := NewAgent(model, registry, AgentConfig{ArtifactStore: store})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "lookup", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolName: "lookup", ToolCallID: "old", Content: strings.Repeat("x", 8_000)},
		{Role: messages.MessageRoleAssistant, Content: "done"},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("q", 2_000)},
	}
	calls := 0
	var seen ProjectionStats
	response, err := agent.Run(ctx, &CompletionRequest{Messages: history, MaxContextTokens: 3_000}, &AgentCallbacks{
		BeforeFirstRequest: func(stats ProjectionStats) error {
			calls++
			if len(model.requests) != 0 {
				t.Fatal("the provider was called before the hook")
			}
			seen = stats
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The hook fires once per run, not once per iteration, and sees the
	// projection that the first request was built from: the older result was
	// demoted, its artifact stored, and the whole request priced with schemas.
	if calls != 1 || len(model.requests) != 2 || len(response.AllMessages) != 3 {
		t.Fatalf("hook calls=%d requests=%d generated=%d", calls, len(model.requests), len(response.AllMessages))
	}
	if seen.CompactedToolResults == 0 || len(store.blobs) == 0 {
		t.Fatalf("hook ran without the reduction the request needed: %+v blobs=%d", seen, len(store.blobs))
	}
	if seen.RequestEstimatedTokens <= seen.EstimatedTokens || seen.RequestEstimatedTokens > 3_000 {
		t.Fatalf("request estimate %d (messages %d) is not the priced request under the 3000 budget", seen.RequestEstimatedTokens, seen.EstimatedTokens)
	}
}

func TestBeforeFirstRequestVetoAbortsBeforeAnyProviderCall(t *testing.T) {
	model := &recordingSequentialLLM{}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	veto := errors.New("session store is read-only")
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("hi")}, &AgentCallbacks{
		BeforeFirstRequest: func(ProjectionStats) error { return veto },
		OnError:            func(error) { t.Fatal("OnError reported the caller's own veto") },
	})
	if !errors.Is(err, veto) || len(model.requests) != 0 || len(response.AllMessages) != 0 || response.IterationCount != 0 {
		t.Fatalf("veto: err=%v requests=%d response=%+v", err, len(model.requests), response)
	}
}

func TestRunRejectsUnsendableInputBeforeTheHook(t *testing.T) {
	missing := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: []byte("missing")})
	for _, tc := range []struct {
		name string
		req  *CompletionRequest
		want string
	}{
		{"missing selected image", &CompletionRequest{Messages: []messages.ChatMessage{{
			Role: messages.MessageRoleUser, Parts: []messages.ContentPart{imageArtifactPart(missing)},
		}}}, "read image artifact"},
		{"prompt over budget", &CompletionRequest{Messages: messages.User(strings.Repeat("x", 40_000)), MaxContextTokens: 2_000}, "context budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &recordingSequentialLLM{}
			agent := NewAgent(model, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
			response, err := agent.Run(context.Background(), tc.req, &AgentCallbacks{
				BeforeFirstRequest: func(ProjectionStats) error {
					t.Fatal("the hook ran for a request that cannot be sent")
					return nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(model.requests) != 0 || len(response.AllMessages) != 0 {
				t.Fatalf("err=%v requests=%d generated=%d", err, len(model.requests), len(response.AllMessages))
			}
		})
	}
}

func TestRunRespectsCancellationBeforeTheHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	model := &recordingSequentialLLM{}
	agent := NewAgent(model, nil, AgentConfig{})
	_, err := agent.Run(ctx, &CompletionRequest{Messages: messages.User("hi")}, &AgentCallbacks{
		BeforeFirstRequest: func(ProjectionStats) error {
			t.Fatal("the hook ran on a canceled run")
			return nil
		},
	})
	if !errors.Is(err, context.Canceled) || len(model.requests) != 0 {
		t.Fatalf("err=%v requests=%d, want context.Canceled and no provider call", err, len(model.requests))
	}
}
