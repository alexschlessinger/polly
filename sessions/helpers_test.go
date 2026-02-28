package sessions

import (
	"testing"
	"time"

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
		{messages.ChatMessage{Content: "hello world"}, 6}, // 2 content + 4 overhead
		{
			messages.ChatMessage{
				Role: "assistant",
				ToolCalls: []messages.ChatMessageToolCall{
					{Name: "test_tool", Arguments: `{"key": "value"}`},
				},
			},
			10, // Name(2) + Args(4) + Overhead(4) = 10
		},
	}

	for _, tt := range tests {
		got := EstimateTokens(tt.input)
		if got != tt.expected {
			t.Errorf("EstimateTokens(%q) = %d; want %d", tt.input.Content, got, tt.expected)
		}
	}
}

// Helper to create an assistant message with tool calls
func assistantWithToolCalls(ids ...string) messages.ChatMessage {
	calls := make([]messages.ChatMessageToolCall, len(ids))
	for i, id := range ids {
		calls[i] = messages.ChatMessageToolCall{ID: id, Name: "tool_" + id, Arguments: "{}"}
	}
	return messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		Content:   "",
		ToolCalls: calls,
	}
}

// Helper to create a tool response
func toolResponse(id string, content string) messages.ChatMessage {
	return messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		ToolCallID: id,
		Content:    content,
	}
}

func TestTrimHistory(t *testing.T) {
	systemMsg := messages.ChatMessage{Role: messages.MessageRoleSystem, Content: "System"}
	msg1 := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "1234"}      // ~5 tokens (1 + 4 overhead)
	msg2 := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "1234"} // ~5 tokens
	msg3 := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "12345678"}  // ~6 tokens (2 + 4 overhead)
	msgToolResp := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "1", Content: "result"}
	tests := []struct {
		name      string
		history   []messages.ChatMessage
		maxTokens int
		wantLen   int
		wantFirst string
	}{
		{
			name:      "No limits",
			history:   []messages.ChatMessage{systemMsg, msg1, msg2},
			maxTokens: 0,
			wantLen:   3,
			wantFirst: messages.MessageRoleSystem,
		},
		{
			name:      "Token limit - keep all",
			history:   []messages.ChatMessage{systemMsg, msg1, msg2},
			maxTokens: 100,
			wantLen:   3,
			wantFirst: messages.MessageRoleSystem,
		},
		{
			name:      "Token limit - trim oldest user msg",
			history:   []messages.ChatMessage{systemMsg, msg1, msg2, msg3}, // Tokens: Sys(ignored), 5, 5, 6. Total non-sys: 16
			maxTokens: 12,                                                  // Should keep msg3(6) and msg2(5) -> total 11. msg1 dropped.
			wantLen:   3,                                                   // System + msg2 + msg3
			wantFirst: messages.MessageRoleSystem,
		},
		{
			name:      "Token limit - trim multiple",
			history:   []messages.ChatMessage{systemMsg, msg1, msg2, msg3},
			maxTokens: 8, // Should keep msg3(6). msg2(5) dropped.
			wantLen:   2, // System + msg3
			wantFirst: messages.MessageRoleSystem,
		},
		{
			name:      "Orphaned tool response removal",
			history:   []messages.ChatMessage{systemMsg, msgToolResp, msg1},
			maxTokens: 0,
			wantLen:   2, // System + msg1 (tool resp removed)
			wantFirst: messages.MessageRoleSystem,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimHistory(tt.history, tt.maxTokens)
			if len(got) != tt.wantLen {
				t.Errorf("TrimHistory() length = %d, want %d", len(got), tt.wantLen)
			}
			if len(got) > 0 && got[0].Role != tt.wantFirst {
				t.Errorf("TrimHistory() first role = %v, want %v", got[0].Role, tt.wantFirst)
			}
			// Additional check for orphaned tool response logic
			if len(got) > 1 && got[0].Role == messages.MessageRoleSystem && got[1].Role == messages.MessageRoleTool {
				t.Errorf("TrimHistory() failed to remove orphaned tool response at index 1")
			}
			if len(got) > 0 && got[0].Role == messages.MessageRoleTool {
				t.Errorf("TrimHistory() failed to remove orphaned tool response at index 0")
			}
		})
	}
}

// TestTrimHistoryPathological tests edge cases with tool calls that expose bugs
func TestTrimHistoryPathological(t *testing.T) {
	systemMsg := messages.ChatMessage{Role: messages.MessageRoleSystem, Content: "System"}
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}
	assistantMsg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "hi there"}

	// Helper to check for orphaned tool responses anywhere in history
	hasOrphanedToolResponse := func(history []messages.ChatMessage) bool {
		for i, msg := range history {
			if msg.Role != messages.MessageRoleTool {
				continue
			}
			// Check if there's a preceding assistant message with matching tool call
			found := false
			for j := i - 1; j >= 0; j-- {
				if history[j].Role == messages.MessageRoleAssistant {
					for _, tc := range history[j].ToolCalls {
						if tc.ID == msg.ToolCallID {
							found = true
							break
						}
					}
					break // Stop at first assistant message going backwards
				}
			}
			if !found {
				return true
			}
		}
		return false
	}

	t.Run("Multiple consecutive orphaned tool responses", func(t *testing.T) {
		history := []messages.ChatMessage{
			systemMsg,
			toolResponse("1", "result1"),
			toolResponse("2", "result2"),
			toolResponse("3", "result3"),
			userMsg,
		}

		result := TrimHistory(history, 0)

		// All orphaned tool responses should be removed, leaving only system + user
		if len(result) != 2 {
			t.Errorf("TrimHistory() got %d messages, want 2 (system + user)", len(result))
		}
		if hasOrphanedToolResponse(result) {
			t.Errorf("TrimHistory() left orphaned tool responses")
			for i, m := range result {
				t.Logf("  [%d] Role=%s ToolCallID=%s", i, m.Role, m.ToolCallID)
			}
		}
	})

	t.Run("Tool call trimmed but responses remain orphaned", func(t *testing.T) {
		// Simulate: assistant calls 2 tools, then conversation continues
		// When we trim by maxHistory, the assistant+toolcalls gets removed but responses may remain
		history := []messages.ChatMessage{
			systemMsg,
			userMsg,
			assistantWithToolCalls("a", "b"),
			toolResponse("a", "result_a"),
			toolResponse("b", "result_b"),
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: "thanks"},
			assistantMsg,
		}

		// When tokens are limited, earlier messages get trimmed
		// This tests that orphaned tool responses are cleaned up
		result := TrimHistory(history, 100) // token limit that keeps most messages

		if hasOrphanedToolResponse(result) {
			t.Errorf("TrimHistory() left orphaned tool response after maxHistory trim")
			for i, m := range result {
				t.Logf("  [%d] Role=%s Content=%q ToolCallID=%s", i, m.Role, m.Content, m.ToolCallID)
			}
		}
	})

	t.Run("Massive tool result exceeds token budget", func(t *testing.T) {
		// A single tool result so large it exceeds the entire token budget
		giantContent := string(make([]byte, 10000)) // ~2500 tokens
		history := []messages.ChatMessage{
			systemMsg,
			userMsg,
			assistantWithToolCalls("big"),
			toolResponse("big", giantContent),
		}

		// Token budget of 100 can't fit the giant response
		result := TrimHistory(history, 100)

		// Should only have system prompt when everything is too big
		if len(result) != 1 {
			t.Errorf("TrimHistory() got %d messages, want 1 (only system)", len(result))
		}
		if hasOrphanedToolResponse(result) {
			t.Errorf("TrimHistory() left orphaned tool response")
		}
	})

	t.Run("Parallel tool calls partially trimmed by tokens", func(t *testing.T) {
		// 3 parallel tool calls, token limit only fits some responses
		history := []messages.ChatMessage{
			systemMsg,
			userMsg,
			assistantWithToolCalls("1", "2", "3"),
			toolResponse("1", "small"),
			toolResponse("2", "small"),
			toolResponse("3", string(make([]byte, 200))), // ~50 tokens + overhead
		}

		// Token limit that only fits the large response
		result := TrimHistory(history, 60)

		if hasOrphanedToolResponse(result) {
			t.Errorf("TrimHistory() left orphaned tool response after token trim")
			for i, m := range result {
				t.Logf("  [%d] Role=%s ToolCallID=%s", i, m.Role, m.ToolCallID)
			}
		}
	})

	t.Run("Multi-turn tool chain trimmed mid-sequence", func(t *testing.T) {
		// Multiple turns of tool calling
		history := []messages.ChatMessage{
			systemMsg,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: "turn1"},
			assistantWithToolCalls("t1"),
			toolResponse("t1", "r1"),
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: "turn2"},
			assistantWithToolCalls("t2"),
			toolResponse("t2", "r2"),
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: "turn3"},
			assistantWithToolCalls("t3"),
			toolResponse("t3", "r3"),
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: "final"},
		}

		// Use a small token limit to trim older messages
		result := TrimHistory(history, 50)

		if hasOrphanedToolResponse(result) {
			t.Errorf("TrimHistory() left orphaned tool responses after multi-turn trim")
			for i, m := range result {
				t.Logf("  [%d] Role=%s Content=%q ToolCallID=%s", i, m.Role, m.Content, m.ToolCallID)
			}
		}
	})
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
			err := validateContextName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateContextName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestMergeMetadata(t *testing.T) {
	t.Run("nil existing", func(t *testing.T) {
		result := MergeMetadata(nil, &Metadata{Name: "new"})
		if result.Name != "new" {
			t.Errorf("Name = %q, want %q", result.Name, "new")
		}
	})

	t.Run("nil update", func(t *testing.T) {
		result := MergeMetadata(&Metadata{Name: "original"}, nil)
		if result.Name != "original" {
			t.Errorf("Name = %q, want %q", result.Name, "original")
		}
	})

	t.Run("non-zero overrides", func(t *testing.T) {
		existing := &Metadata{Name: "old", MaxTokens: 100}
		update := &Metadata{Name: "new", Temperature: 0.7}
		result := MergeMetadata(existing, update)
		if result.Name != "new" {
			t.Errorf("Name = %q, want %q", result.Name, "new")
		}
		if result.Temperature != 0.7 {
			t.Errorf("Temperature = %f, want 0.7", result.Temperature)
		}
	})

	t.Run("zero value does not override", func(t *testing.T) {
		existing := &Metadata{MaxTokens: 8192}
		update := &Metadata{MaxTokens: 0} // zero should not override
		result := MergeMetadata(existing, update)
		if result.MaxTokens != 8192 {
			t.Errorf("MaxTokens = %d, want 8192", result.MaxTokens)
		}
	})

	t.Run("zero LastUsed gets backfilled", func(t *testing.T) {
		existing := &Metadata{Name: "test"}
		update := &Metadata{}
		result := MergeMetadata(existing, update)
		if result.LastUsed.IsZero() {
			t.Error("LastUsed should be backfilled when zero")
		}
		if time.Since(result.LastUsed) > time.Second {
			t.Error("LastUsed should be approximately now")
		}
	})
}
