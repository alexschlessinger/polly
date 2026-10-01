package subagent

import (
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// A reply the run dropped, kept only as its usage record, still counts
// toward the turn's tokens.
func TestTurnTokensCountDroppedReplies(t *testing.T) {
	reply := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "kept", Metadata: map[string]any{messages.MetadataKeyInputTokens: 100, messages.MetadataKeyOutputTokens: 5}}
	dropped := messages.ChatMessage{Role: messages.MessageRoleAssistant, Metadata: map[string]any{messages.MetadataKeyInputTokens: 300, messages.MetadataKeyOutputTokens: 7}}
	in, out := turnTokens([]messages.ChatMessage{reply, dropped.UsageRecord()})
	if in != 300 || out != 12 {
		t.Fatalf("turnTokens = %d, %d; want 300, 12", in, out)
	}
}
