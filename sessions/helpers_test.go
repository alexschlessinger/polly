package sessions

import (
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
			name:      "Token limit - trim realigns to user boundary",
			history:   []messages.ChatMessage{systemMsg, msg1, msg2, msg3}, // Tokens: Sys(ignored), 5, 5, 6. Total non-sys: 16
			maxTokens: 12,                                                  // Budget keeps msg3(6)+msg2(5), then the suffix realigns to start at msg3 (user).
			wantLen:   2,                                                   // System + msg3
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

		// The whole history is one exchange (starts at the most recent user
		// message), which is always kept — even over budget — so the session
		// is never wiped down to just the system prompt.
		result := TrimHistory(history, 100)

		if len(result) != len(history) {
			t.Errorf("TrimHistory() got %d messages, want %d (current exchange must survive)", len(result), len(history))
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

// TestTrimHistoryIgnoresCumulativeInputTokens is a regression test: providers
// stamp assistant messages with input_tokens equal to the full prompt of the
// request that produced them. Counting that against the budget used to
// collapse whole sessions to the system prompt.
func TestTrimHistoryIgnoresCumulativeInputTokens(t *testing.T) {
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "sys"}}
	promptTokens := 500
	for turn := 0; turn < 10; turn++ {
		user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "a question of moderate length"}
		asst := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "an answer of moderate length"}
		asst.SetTokenUsage(promptTokens, 20) // input_tokens = full prompt at that turn
		promptTokens += 600
		history = append(history, user, asst)
	}

	// Real per-message size is ~10 turns * ~30 tokens, far under the budget.
	result := TrimHistory(history, 4000)
	if len(result) != len(history) {
		t.Errorf("TrimHistory() got %d messages, want %d (cumulative input_tokens must not count)", len(result), len(history))
	}
}

// TestGetMessageTokensIgnoresProviderUsage pins GetMessageTokens to the
// replay estimate: input_tokens is cumulative (whole request prompt) and
// output_tokens includes reasoning tokens that are never replayed, so neither
// measures the message's retained size.
func TestGetMessageTokensIgnoresProviderUsage(t *testing.T) {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "hello world"}
	msg.SetTokenUsage(50000, 12000) // reasoning-heavy turn: huge billed output, tiny answer
	if got, want := GetMessageTokens(msg), EstimateTokens(msg); got != want {
		t.Errorf("GetMessageTokens() = %d, want replay estimate %d", got, want)
	}
}

// TestTrimHistoryIgnoresReasoningHeavyOutput is a regression test: a short
// answer produced with heavy hidden reasoning (billed output_tokens far above
// the replayed size) must not evict the surrounding exchanges.
func TestTrimHistoryIgnoresReasoningHeavyOutput(t *testing.T) {
	heavy := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "short answer"}
	heavy.SetTokenUsage(4000, 3000) // billed output includes ~3k hidden reasoning tokens
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: "first question"},
		{Role: messages.MessageRoleAssistant, Content: "first answer"},
		{Role: messages.MessageRoleUser, Content: "second question"},
		heavy,
	}

	result := TrimHistory(history, 2000)
	if len(result) != len(history) {
		t.Errorf("TrimHistory() got %d messages, want %d (billed output tokens must not evict history)", len(result), len(history))
	}
}

// TestTrimHistoryCountsImages verifies image parts carry a real token cost so
// maxcontext cannot retain unbounded images at ~4 tokens each.
func TestTrimHistoryCountsImages(t *testing.T) {
	imageMsg := func(q string) messages.ChatMessage {
		return messages.ChatMessage{
			Role: messages.MessageRoleUser,
			Parts: []messages.ContentPart{
				{Type: "text", Text: q},
				{Type: "image_base64", ImageData: "aGVsbG8=", MimeType: "image/png"},
			},
		}
	}
	if got := EstimateTokens(imageMsg("look")); got < imageTokenEstimate {
		t.Fatalf("EstimateTokens(image msg) = %d, want >= %d", got, imageTokenEstimate)
	}

	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		imageMsg("image one"),
		{Role: messages.MessageRoleAssistant, Content: "about image one"},
		imageMsg("image two"),
		{Role: messages.MessageRoleAssistant, Content: "about image two"},
		{Role: messages.MessageRoleUser, Content: "final question"},
	}

	// Budget fits one image exchange, not two.
	result := TrimHistory(history, 2000)
	images := 0
	for _, m := range result {
		for _, p := range m.Parts {
			if p.Type == "image_base64" {
				images++
			}
		}
	}
	if images > 1 {
		t.Errorf("TrimHistory() kept %d images, want <= 1 within a 2000-token budget", images)
	}
}

// TestTrimHistoryRealignsToUserBoundary verifies a trim that cuts into the
// middle of an old exchange drops the exchange's tail instead of sending a
// history that opens with an unanchored assistant tool-call turn (rejected by
// Gemini and Anthropic).
func TestTrimHistoryRealignsToUserBoundary(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: string(make([]byte, 400))}, // ~104 tokens, gets trimmed
		assistantWithToolCalls("a"),
		toolResponse("a", "result"),
		{Role: messages.MessageRoleAssistant, Content: "answer1"},
		{Role: messages.MessageRoleUser, Content: "next question"},
		{Role: messages.MessageRoleAssistant, Content: "answer2"},
	}

	// Budget fits everything except the oldest user message, so the raw cut
	// lands on the assistant tool-call turn.
	result := TrimHistory(history, 40)

	if len(result) < 2 || result[0].Role != messages.MessageRoleSystem {
		t.Fatalf("TrimHistory() unexpected shape: %d messages", len(result))
	}
	if result[1].Role != messages.MessageRoleUser {
		t.Errorf("TrimHistory() first non-system role = %s, want user", result[1].Role)
	}
	for _, m := range result {
		if m.Role == messages.MessageRoleUser && m.Content == "next question" {
			return
		}
	}
	t.Errorf("TrimHistory() dropped the newest user message")
}

// TestTrimHistoryKeepsCurrentExchange verifies the suffix from the most
// recent user message survives even when it alone exceeds the budget.
func TestTrimHistoryKeepsCurrentExchange(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: "old exchange"},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current question"},
		assistantWithToolCalls("x"),
		toolResponse("x", string(make([]byte, 10000))), // ~2500 tokens, over budget alone
		{Role: messages.MessageRoleAssistant, Content: "final answer"},
	}

	result := TrimHistory(history, 100)

	var sawCurrent, sawFinal bool
	for _, m := range result {
		if m.Content == "current question" {
			sawCurrent = true
		}
		if m.Content == "final answer" {
			sawFinal = true
		}
		if m.Content == "old exchange" || m.Content == "old answer" {
			t.Errorf("TrimHistory() kept old exchange despite budget")
		}
	}
	if !sawCurrent || !sawFinal {
		t.Errorf("TrimHistory() dropped part of the current exchange (user=%v final=%v)", sawCurrent, sawFinal)
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
			err := validateContextName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateContextName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
