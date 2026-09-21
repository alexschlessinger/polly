package sessions

import (
	"github.com/alexschlessinger/pollytool/artifacts"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input    messages.ChatMessage
		expected int
	}{
		{messages.ChatMessage{Content: ""}, 4},            // 0 content + 4 overhead
		{messages.ChatMessage{Content: "1234"}, 5},        // 1 content + 4 overhead
		{messages.ChatMessage{Content: "12345678"}, 6},    // 2 content + 4 overhead
		{messages.ChatMessage{Content: "hello world"}, 7}, // 3 content + 4 overhead: (11+3)/4 = 3
		{
			messages.ChatMessage{
				Role: "assistant",
				ToolCalls: []messages.ChatMessageToolCall{
					{Name: "test_tool", Arguments: `{"key": "value"}`},
				},
			},
			13, // Name(3: (9+3)/4) + Args(6: (16+2)/3) + Overhead(4) = 13
		},
	}

	for _, tt := range tests {
		got := EstimateTokens(tt.input)
		if got != tt.expected {
			t.Errorf("EstimateTokens(%q) = %d; want %d", tt.input.Content, got, tt.expected)
		}
	}
}

// TestEstimateTokensIgnoresProviderUsage pins EstimateTokens to the replay
// estimate: input_tokens is cumulative (whole request prompt) and
// output_tokens includes reasoning tokens that are never replayed, so neither
// measures the message's retained size.
func TestEstimateTokensIgnoresProviderUsage(t *testing.T) {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "hello world"}
	want := EstimateTokens(msg)
	msg.SetTokenUsage(50000, 12000) // reasoning-heavy turn: huge billed output, tiny answer
	if got := EstimateTokens(msg); got != want {
		t.Errorf("EstimateTokens() = %d, want replay estimate %d", got, want)
	}
}

// TestEstimateTokensCountsImages verifies image parts carry a real token cost
// so a budget cannot retain unbounded images at ~4 tokens each.
func TestEstimateTokensCountsImages(t *testing.T) {
	msg := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "look"},
			{Type: "image_base64", ImageData: "aGVsbG8=", MimeType: "image/png"},
		},
	}
	if got := EstimateTokens(msg); got < messages.EstimatedImageTokens {
		t.Fatalf("EstimateTokens(image msg) = %d, want >= %d", got, messages.EstimatedImageTokens)
	}
}

func TestValidateContextName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty", "", true},
		{"slash", "a/b", true},
		{"backslash", "a\\b", true},
		{"colon", "a:b", true},
		{"star", "a*b", true},
		{"question", "a?b", true},
		{"quote", `a"b`, true},
		{"lt", "a<b", true},
		{"gt", "a>b", true},
		{"pipe", "a|b", true},
		{"dot", ".", true},
		{"dotdot", "..", true},
		{"leading_space", " name", true},
		{"trailing_space", "name ", true},
		{"leading_dot", ".name", true},
		{"trailing_dot", "name.", true},
		{"control_null", "ab\x00c", true},
		{"control_x1f", "ab\x1fc", true},
		{"control_del", "ab\x7fc", true},
		{"valid_simple", "my-context", false},
		{"valid_underscores", "my_context_2", false},
		{"valid_spaces_middle", "my context", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSessionName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSessionName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

// TestEstimateTokensCountsTextArtifactsWhereTheyReplaceContent pins the
// durable estimate against double counting: a text artifact on a tool result
// stands in for content the receipt no longer carries, while the reference a
// projection records on an assistant reply for an older result that is still
// inline points at content already counted on that result.
func TestEstimateTokensCountsTextArtifactsWhereTheyReplaceContent(t *testing.T) {
	ref := artifacts.Ref{ID: "sha256:" + strings.Repeat("a", 64), Kind: artifacts.KindText, Bytes: 8_000, Lines: 1}
	part := messages.ContentPart{Type: "artifact", Artifact: &ref}
	receipt := messages.ChatMessage{Role: messages.MessageRoleTool, ToolName: "lookup", ToolCallID: "old", Content: "[tool output stored as artifact]", Parts: []messages.ContentPart{part}}
	if got := EstimateTokens(receipt); got < 2_000 {
		t.Fatalf("EstimateTokens(receipt) = %d, want the externalized content counted", got)
	}
	reply := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", Parts: []messages.ContentPart{part}}
	plain := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"}
	if got, want := EstimateTokens(reply), EstimateTokens(plain); got != want {
		t.Fatalf("EstimateTokens(reply with reference) = %d, want %d: the referenced content is counted on the inline result", got, want)
	}
}
