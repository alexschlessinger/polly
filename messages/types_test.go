package messages

import (
	"encoding/json"
	"testing"
	"time"
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

// TestDurationAccessorsSurviveJSONRoundTrip verifies the display-only timing
// metadata reads back after persistence, and that sub-millisecond durations
// round up instead of vanishing.
func TestDurationAccessorsSurviveJSONRoundTrip(t *testing.T) {
	msg := ChatMessage{Role: MessageRoleAssistant, Reasoning: "hmm"}
	msg.SetThinkingDuration(2500 * time.Millisecond)
	result := ChatMessage{Role: MessageRoleTool, ToolCallID: "1"}
	result.SetToolDuration(1200 * time.Millisecond)

	for _, tc := range []struct {
		name string
		msg  ChatMessage
		get  func(*ChatMessage) time.Duration
		want time.Duration
	}{
		{"thinking", msg, (*ChatMessage).ThinkingDuration, 2500 * time.Millisecond},
		{"tool", result, (*ChatMessage).ToolDuration, 1200 * time.Millisecond},
	} {
		data, err := json.Marshal(tc.msg)
		if err != nil {
			t.Fatalf("%s marshal: %v", tc.name, err)
		}
		var loaded ChatMessage
		if err := json.Unmarshal(data, &loaded); err != nil {
			t.Fatalf("%s unmarshal: %v", tc.name, err)
		}
		if got := tc.get(&loaded); got != tc.want {
			t.Errorf("%s duration after round trip = %v, want %v", tc.name, got, tc.want)
		}
	}

	var tiny ChatMessage
	tiny.SetThinkingDuration(200 * time.Microsecond)
	if got := tiny.ThinkingDuration(); got != time.Millisecond {
		t.Errorf("sub-millisecond thinking duration = %v, want 1ms", got)
	}
	var none ChatMessage
	none.SetThinkingDuration(0)
	none.SetToolDuration(-time.Second)
	if none.Metadata != nil || none.ThinkingDuration() != 0 || none.ToolDuration() != 0 {
		t.Errorf("zero durations were recorded: %#v", none.Metadata)
	}
}
