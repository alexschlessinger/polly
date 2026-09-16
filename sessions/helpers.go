package sessions

import (
	"slices"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// TrimHistory applies smart trimming to a message history slice.
// It keeps the system prompt (first message) plus the newest messages that
// fit within the token limit, with two guarantees:
//   - the suffix starting at the most recent user message always survives,
//     so a single oversized exchange can never wipe the whole session
//   - when a trim cuts mid-exchange, the kept suffix is realigned to start
//     at a user message, since providers reject histories that open with
//     tool responses or unanchored assistant tool-call turns
//
// maxTokens: maximum tokens to keep (0 = unlimited).
func TrimHistory(history []messages.ChatMessage, maxTokens int) []messages.ChatMessage {
	if len(history) == 0 {
		return history
	}

	// Always keep the system prompt if it exists
	startIdx := 0
	if history[0].Role == messages.MessageRoleSystem {
		startIdx = 1
	}

	// If we only have system prompt or empty history, return as is
	if len(history) <= startIdx {
		return history
	}

	// Work with the rest of the messages
	msgs := history[startIdx:]

	// Apply token limit (if set)
	trimmed := false
	if maxTokens > 0 {
		currentTokens := 0
		// Calculate tokens from newest to oldest
		keepCount := 0
		for i := len(msgs) - 1; i >= 0; i-- {
			tokens := EstimateTokens(msgs[i])
			if currentTokens+tokens > maxTokens {
				break
			}
			currentTokens += tokens
			keepCount++
		}
		// The current exchange is never trimmed: everything from the most
		// recent user message on is kept even when it exceeds the budget.
		lastUser := -1
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == messages.MessageRoleUser {
				lastUser = i
				break
			}
		}
		if lastUser >= 0 && keepCount < len(msgs)-lastUser {
			keepCount = len(msgs) - lastUser
		}
		if keepCount < len(msgs) {
			msgs = msgs[len(msgs)-keepCount:]
			trimmed = true
		}
	}

	// Reconstruct history
	result := make([]messages.ChatMessage, 0, startIdx+len(msgs))
	result = append(result, history[:startIdx]...)
	result = append(result, msgs...)

	// A trim can cut mid-exchange, leaving the suffix opening with tool
	// responses or an assistant turn; realign it to the next user message.
	if trimmed {
		firstUser := slices.IndexFunc(result[startIdx:], func(m messages.ChatMessage) bool {
			return m.Role == messages.MessageRoleUser
		})
		if firstUser > 0 {
			result = slices.Delete(result, startIdx, startIdx+firstUser)
		}
	}

	// Handle the API constraint: tool responses must follow tool_calls
	// Remove all orphaned tool responses at the start (after system prompt)
	for len(result) > startIdx && result[startIdx].Role == messages.MessageRoleTool {
		result = slices.Delete(result, startIdx, startIdx+1)
	}

	return result
}

// EstimateTokens returns the token count of a single message as it would be
// replayed to a provider, using the shared heuristics defined in the messages
// package: 1 token ≈ 4 characters of prose, 3 bytes per token of dense JSON
// tool arguments, and a flat per-image cost.
// Provider-reported counts are deliberately not used: input_tokens is
// cumulative (the entire request prompt), and output_tokens includes
// reasoning tokens that are not replayed from history, so both misstate the
// message's retained size.
func EstimateTokens(msg messages.ChatMessage) int {
	count := 0

	// Content
	count += messages.EstimatedStringTokens(msg.Content)

	// Multimodal parts
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			count += messages.EstimatedStringTokens(part.Text)
		case "image_base64", "image_url":
			count += messages.EstimatedImageTokens
		}
		if part.Artifact != nil {
			switch part.Artifact.Kind {
			case artifacts.KindImage:
				count += messages.EstimatedImageTokens
			case artifacts.KindText:
				// A text artifact replaces a tool result's externalized
				// content. On any other role it only references stored
				// content that is counted where it lives (the projection
				// records the artifacts it mints for older inline results on
				// the assistant reply), so it adds nothing here.
				if msg.Role == messages.MessageRoleTool {
					count += int(part.Artifact.Bytes / 4)
				}
			}
		}
	}

	// Tool calls
	for _, tc := range msg.ToolCalls {
		count += messages.EstimatedStringTokens(tc.Name)
		count += messages.EstimatedJSONTokens(tc.Arguments)
	}

	// Reasoning
	count += messages.EstimatedStringTokens(msg.Reasoning)

	// Tool Call ID
	count += messages.EstimatedStringTokens(msg.ToolCallID)

	// Base overhead per message
	count += 4

	return count
}

// CopyHistory creates a defensive copy of the history slice
func CopyHistory(history []messages.ChatMessage) []messages.ChatMessage {
	result := make([]messages.ChatMessage, len(history))
	for i, msg := range history {
		result[i] = msg.Clone()
	}
	return result
}
