package messages

import (
	"encoding/json"
	"testing"
)

// TestTokenAccessorsSurviveJSONRoundTrip verifies token metadata still reads
// back after session persistence, where JSON turns ints into float64.
func TestTokenAccessorsSurviveJSONRoundTrip(t *testing.T) {
	msg := ChatMessage{Role: MessageRoleAssistant, Content: "hi"}
	msg.SetTokenUsage(1234, 56)
	msg.SetPromptCacheUsage(789, 321)

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var loaded ChatMessage
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := loaded.GetInputTokens(); got != 1234 {
		t.Errorf("GetInputTokens() after round trip = %d, want 1234", got)
	}
	if got := loaded.GetOutputTokens(); got != 56 {
		t.Errorf("GetOutputTokens() after round trip = %d, want 56", got)
	}
	if got := loaded.GetCacheReadInputTokens(); got != 789 {
		t.Errorf("GetCacheReadInputTokens() after round trip = %d, want 789", got)
	}
	if got := loaded.GetCacheWriteInputTokens(); got != 321 {
		t.Errorf("GetCacheWriteInputTokens() after round trip = %d, want 321", got)
	}
}
