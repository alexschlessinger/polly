// Package qwencloud talks to QwenCloud's OpenAI-compatible Chat Completions API.
package qwencloud

import (
	"context"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// DefaultBaseURL is QwenCloud's international OpenAI-compatible endpoint.
const DefaultBaseURL = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"

var _ contract.LLM = (*Provider)(nil)

// Provider serves QwenCloud chat with thinking controls and reasoning replay.
type Provider struct{ client *openai.Client }

// NewProvider uses DefaultBaseURL when baseURL is empty.
func NewProvider(apiKey, baseURL string) *Provider {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{client: openai.NewClient(apiKey, baseURL)}
}

// chatRequest keeps QwenCloud extensions at the top level of the wire body.
type chatRequest struct {
	openai.ChatCompletionRequest
	EnableThinking bool `json:"enable_thinking"`
	ThinkingBudget int  `json:"thinking_budget,omitempty"`
}

func (r *chatRequest) Streaming(on bool) openai.ChatBody {
	body := *r
	body.ChatCompletionRequest = *r.ChatCompletionRequest.Streaming(on).(*openai.ChatCompletionRequest)
	return &body
}

func (p Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, openai.NewChatAdapter(), func(ctx context.Context, core *streaming.StreamingCore) {
		params := buildRequest(req)
		var err error
		if req.IsStreaming() {
			err = openai.StreamChat(ctx, p.client, params, core)
		} else {
			err = openai.CompleteChat(ctx, p.client, params, core)
		}
		if err != nil {
			core.EmitError(err)
		}
	})
}

func buildRequest(req *contract.CompletionRequest) *chatRequest {
	params := &chatRequest{ChatCompletionRequest: *openai.BuildChatCompletionRequest(req), EnableThinking: req.ThinkingEffort.IsEnabled()}
	// Budgets work across Qwen generations; never send both controls because
	// Qwen3.8 rejects reasoning_effort combined with thinking_budget.
	params.ReasoningEffort = ""
	if budget, ok := req.ThinkingEffort.AsBudget(); ok {
		params.ThinkingBudget = budget
	}
	for i, msg := range req.Messages {
		if msg.Role == messages.MessageRoleAssistant {
			params.Messages[i].ReasoningContent = msg.Reasoning
		}
	}
	return params
}
