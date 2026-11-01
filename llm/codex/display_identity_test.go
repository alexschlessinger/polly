package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestRequestIgnoresDisplayIdentity(t *testing.T) {
	req := &contract.CompletionRequest{Model: "gpt-5.4", Messages: []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "work"},
		{Role: messages.MessageRoleAssistant, TextBlocks: []messages.AssistantText{{ID: "provider-text", Phase: messages.PhaseCommentary, Text: "checking"}}, ToolCalls: []messages.ChatMessageToolCall{{ID: "call_protocol", Name: "work", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call_protocol", ToolName: "work", Content: "result"},
	}}
	before, err := json.Marshal(buildRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	for i := range req.Messages {
		req.Messages[i].Metadata = map[string]any{messages.MetadataKeyDisplayID: "host-message"}
	}
	req.Messages[1].ToolCalls[0].DisplayID = "host-call"
	req.Messages[2].Metadata[messages.MetadataKeyDisplayID] = "host-call"
	after, err := json.Marshal(buildRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || bytes.Contains(after, []byte("display_id")) || !bytes.Contains(after, []byte("call_protocol")) {
		t.Fatalf("host identity changed provider request:\nbefore: %s\nafter: %s", before, after)
	}
}
