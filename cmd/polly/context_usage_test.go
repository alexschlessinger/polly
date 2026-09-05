package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

type contextUsageRecorder struct {
	TurnUI
	used, peakInput, output int
	estimated               bool
}

func (u *contextUsageRecorder) RecordContextUsage(used, limit int, estimated bool) {
	u.used, u.estimated = used, estimated
}

func (u *contextUsageRecorder) RecordTurnTokens(input, output int) {
	u.peakInput, u.output = input, output
}

type compactingContextLLM struct {
	inputs         []int
	omitFinalUsage bool
}

func (m *compactingContextLLM) ChatCompletionStream(_ context.Context, req *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	inputTokens := 0
	for _, msg := range req.Messages {
		inputTokens += llm.EstimateMessageTokens(msg)
	}
	response := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"}
	if len(m.inputs) == 0 {
		response.StopReason = messages.StopReasonToolUse
		response.Content = ""
		response.ToolCalls = []messages.ChatMessageToolCall{{ID: "now", Name: "noop", Arguments: `{}`}}
	}
	if len(m.inputs) == 0 || !m.omitFinalUsage {
		response.SetTokenUsage(inputTokens, 1)
	}
	m.inputs = append(m.inputs, inputTokens)
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

func TestContextMeterUsesLatestRequestAfterCompaction(t *testing.T) {
	for _, omitFinalUsage := range []bool{false, true} {
		name := "provider usage"
		if omitFinalUsage {
			name = "estimated final usage"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "context-meter")
			if err := session.AddMessages(ctx, []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "old request"},
				{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "noop", Arguments: `{}`}}},
				{Role: messages.MessageRoleTool, ToolName: "noop", ToolCallID: "old", Content: strings.Repeat("x", 26_000)},
				{Role: messages.MessageRoleAssistant, Content: "old answer"},
			}); err != nil {
				t.Fatal(err)
			}
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "noop", Run: func(context.Context, tools.Args) (string, error) {
				return strings.Repeat("y", 6_000), nil
			}}})
			artifactStore := session.ArtifactStore()
			model := &compactingContextLLM{omitFinalUsage: omitFinalUsage}
			state := &conversationState{
				session: session, artifactStore: artifactStore, toolRegistry: registry,
				agent:          llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
				settings:       Settings{Model: "test/model", MaxTokens: 128, MaxHistoryTokens: 8_000},
				contextWindows: map[string]int{"test/model": 0},
			}
			config := &Config{}
			var stdout, stderr bytes.Buffer
			baseUI := newLineTurnUI(config, nil)
			baseUI.writer, baseUI.errWriter = &stdout, &stderr
			ui := &contextUsageRecorder{TurnUI: baseUI}
			code, err := executeTurnWithUserMessage(ctx, config, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "continue"}, nil, nil, ui, false)
			if code != 0 || err != nil {
				t.Fatalf("turn failed: code=%d err=%v", code, err)
			}
			if len(model.inputs) != 2 || model.inputs[1] >= model.inputs[0] {
				t.Fatalf("fixture did not compact: inputs=%v", model.inputs)
			}
			if ui.estimated != omitFinalUsage {
				t.Fatalf("estimated=%v, want %v", ui.estimated, omitFinalUsage)
			}
			if !omitFinalUsage && ui.used != model.inputs[1] {
				t.Fatalf("context meter=%d, final input=%d", ui.used, model.inputs[1])
			}
			if omitFinalUsage && (ui.used < model.inputs[1] || ui.used >= model.inputs[0]) {
				t.Fatalf("estimate=%d, want final request with schemas rather than peak input %d", ui.used, model.inputs[0])
			}
			if ui.peakInput != model.inputs[0] {
				t.Fatalf("turn trailer lost peak input: %d, want %d", ui.peakInput, model.inputs[0])
			}
		})
	}
}
