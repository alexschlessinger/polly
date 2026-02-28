package sessions

import (
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func newTestSession(meta *Metadata) *LocalSession {
	if meta == nil {
		meta = &Metadata{}
	}
	s := &LocalSession{
		name:     "test",
		last:     time.Now(),
		metadata: meta,
	}
	return s
}

func TestGetTotalTokens_Empty(t *testing.T) {
	s := newTestSession(nil)
	if got := s.GetTotalTokens(); got != 0 {
		t.Errorf("GetTotalTokens() = %d, want 0", got)
	}
}

func TestGetTotalTokens_SumsMessages(t *testing.T) {
	s := newTestSession(nil)
	msg1 := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello world"}
	msg2 := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "hi there"}
	s.history = []messages.ChatMessage{msg1, msg2}

	want := EstimateTokens(msg1) + EstimateTokens(msg2)
	got := s.GetTotalTokens()
	if got != want {
		t.Errorf("GetTotalTokens() = %d, want %d", got, want)
	}
}

func TestGetCapacityPercentage_NoLimit(t *testing.T) {
	s := newTestSession(&Metadata{MaxHistoryTokens: 0})
	if got := s.GetCapacityPercentage(); got != 0 {
		t.Errorf("GetCapacityPercentage() = %f, want 0", got)
	}
}

func TestGetCapacityPercentage_HalfFull(t *testing.T) {
	// Each message with 4-char content = 1 token content + 4 overhead = 5 tokens
	s := newTestSession(&Metadata{MaxHistoryTokens: 20})
	s.history = []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "aaaa"},      // 5 tokens
		{Role: messages.MessageRoleAssistant, Content: "bbbb"}, // 5 tokens
	}

	got := s.GetCapacityPercentage()
	// 10 tokens / 20 max = 50%
	if got < 45 || got > 55 {
		t.Errorf("GetCapacityPercentage() = %f, want ~50", got)
	}
}

func TestGetTimeToExpiry_NoTTL(t *testing.T) {
	s := newTestSession(&Metadata{TTL: 0})
	if got := s.GetTimeToExpiry(); got != 0 {
		t.Errorf("GetTimeToExpiry() = %v, want 0", got)
	}
}

func TestGetTimeToExpiry_WithTTL(t *testing.T) {
	s := newTestSession(&Metadata{TTL: time.Hour})
	s.last = time.Now()

	got := s.GetTimeToExpiry()
	if got <= 0 {
		t.Errorf("GetTimeToExpiry() = %v, want positive duration", got)
	}
	if got > time.Hour {
		t.Errorf("GetTimeToExpiry() = %v, want <= 1h", got)
	}
}

func TestGetMessageCounts(t *testing.T) {
	s := newTestSession(nil)
	s.history = []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: "hi"},
		{Role: messages.MessageRoleAssistant, Content: "hello"},
		{Role: messages.MessageRoleUser, Content: "bye"},
		{Role: messages.MessageRoleTool, ToolCallID: "1", Content: "result"},
	}

	counts := s.GetMessageCounts()
	if counts["system"] != 1 {
		t.Errorf("system count = %d, want 1", counts["system"])
	}
	if counts["user"] != 2 {
		t.Errorf("user count = %d, want 2", counts["user"])
	}
	if counts["assistant"] != 1 {
		t.Errorf("assistant count = %d, want 1", counts["assistant"])
	}
	if counts["tool"] != 1 {
		t.Errorf("tool count = %d, want 1", counts["tool"])
	}
}

func TestGetToolCallCount(t *testing.T) {
	s := newTestSession(nil)
	s.history = []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "do stuff"},
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "1", Name: "tool_a", Arguments: "{}"},
				{ID: "2", Name: "tool_b", Arguments: "{}"},
			},
		},
		{Role: messages.MessageRoleTool, ToolCallID: "1", Content: "result_a"},
		{Role: messages.MessageRoleTool, ToolCallID: "2", Content: "result_b"},
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "3", Name: "tool_c", Arguments: "{}"},
			},
		},
	}

	got := s.GetToolCallCount()
	if got != 3 {
		t.Errorf("GetToolCallCount() = %d, want 3", got)
	}
}
