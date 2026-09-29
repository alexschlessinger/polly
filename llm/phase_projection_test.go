package llm

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestCapabilityProjectionCannotRestorePhasedToolCalls(t *testing.T) {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant,
		TextBlocks: []messages.AssistantText{{ID: "output_0", Phase: messages.PhaseCommentary, Text: "Checking."}},
		ToolCalls:  []messages.ChatMessageToolCall{{ID: "call", Name: "read", Arguments: "{}"}},
		Metadata:   map[string]any{openai.ResponsesOutputOrderKey: []string{"message:output_0", "reasoning:rs", "function_call:call"}, openai.ResponsesReasoningModelKey: "test", openai.ResponsesReasoningItemsKey: []map[string]any{{"id": "rs", "encrypted_content": "opaque"}}}}
	req := &CompletionRequest{Model: "other", Messages: []messages.ChatMessage{msg}}
	prepared, _, err := PrepareCapabilities(req, ModelCapabilities{Tools: truth(false)}, false)
	if err != nil {
		t.Fatal(err)
	}
	flat := prepared.Messages[0]
	if len(flat.TextBlocks) != 0 || len(flat.ToolCalls) != 0 {
		t.Fatalf("tool-free history=%#v", flat)
	}
	input := openai.BuildResponsesRequest(prepared).Input
	if len(input) != 1 || input[0].Type != "message" || input[0].Phase != "" {
		t.Fatalf("reintroduced output=%#v", input)
	}
	if !strings.Contains(flat.GetContent(), "Checking.") || !strings.Contains(flat.GetContent(), "Tool call call") {
		t.Fatalf("lost projected text=%q", flat.GetContent())
	}
	if !msg.HasTextBlocks() || len(msg.ToolCalls) != 1 {
		t.Fatal("projection changed source history")
	}
	if messages.EstimateMessageTokens(msg) <= messages.EstimateMessageTokens(messages.ChatMessage{Role: messages.MessageRoleAssistant}) {
		t.Fatal("commentary not budgeted")
	}
}
