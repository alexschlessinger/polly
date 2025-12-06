package sessions

import (
	"slices"
	"time"

	"dario.cat/mergo"
	"github.com/alexschlessinger/pollytool/messages"
)

// TrimHistory applies smart trimming to a message history slice.
// It keeps the system prompt (first message) and the most recent messages that fit within the token limit.
// maxTokens: maximum tokens to keep (0 = unlimited).
func TrimHistory(history []messages.ChatMessage, maxTokens int) []messages.ChatMessage {
	if len(history) == 0 {
		return history
	}

	// Always keep the system prompt if it exists
	var systemPrompt *messages.ChatMessage
	startIdx := 0
	if history[0].Role == messages.MessageRoleSystem {
		systemPrompt = &history[0]
		startIdx = 1
	}

	// If we only have system prompt or empty history, return as is
	if len(history) <= startIdx {
		return history
	}

	// Work with the rest of the messages
	msgs := history[startIdx:]

	// Apply token limit (if set)
	if maxTokens > 0 {
		currentTokens := 0
		// Calculate tokens from newest to oldest
		keepCount := 0
		for i := len(msgs) - 1; i >= 0; i-- {
			tokens := GetMessageTokens(msgs[i])
			if currentTokens+tokens > maxTokens {
				break
			}
			currentTokens += tokens
			keepCount++
		}
		if keepCount < len(msgs) {
			msgs = msgs[len(msgs)-keepCount:]
		}
	}

	// Reconstruct history
	result := make([]messages.ChatMessage, 0, len(msgs)+1)
	if systemPrompt != nil {
		result = append(result, *systemPrompt)
	}
	result = append(result, msgs...)

	// Handle the API constraint: tool responses must follow tool_calls
	// Remove all orphaned tool responses at the start (after system prompt)
	checkIdx := 0
	if systemPrompt != nil {
		checkIdx = 1
	}

	for len(result) > checkIdx && result[checkIdx].Role == messages.MessageRoleTool {
		result = slices.Delete(result, checkIdx, checkIdx+1)
	}

	return result
}

// GetMessageTokens returns the token count for a message.
// It prefers actual token counts from provider metadata if available,
// otherwise falls back to estimation.
func GetMessageTokens(msg messages.ChatMessage) int {
	// Prefer actual tokens from metadata
	if input := msg.GetInputTokens(); input > 0 {
		return input
	}
	if output := msg.GetOutputTokens(); output > 0 {
		return output
	}
	// Fall back to estimate
	return EstimateTokens(msg)
}

// EstimateTokens provides a rough estimate of tokens in a message.
// It uses a simple heuristic: 1 token ≈ 4 characters.
func EstimateTokens(msg messages.ChatMessage) int {
	count := 0

	// Content
	count += len(msg.Content) / 4

	// Multimodal parts
	for _, part := range msg.Parts {
		if part.Type == "text" {
			count += len(part.Text) / 4
		}
		// TODO: Add estimation for images if needed
	}

	// Tool calls
	for _, tc := range msg.ToolCalls {
		count += len(tc.Name) / 4
		count += len(tc.Arguments) / 4
	}

	// Reasoning
	count += len(msg.Reasoning) / 4

	// Tool Call ID
	count += len(msg.ToolCallID) / 4

	// Base overhead per message
	count += 4

	return count
}

// CopyHistory creates a defensive copy of the history slice
func CopyHistory(history []messages.ChatMessage) []messages.ChatMessage {
	result := make([]messages.ChatMessage, len(history))
	copy(result, history)
	return result
}

// MergeMetadata merges non-zero fields from 'update' into 'existing'.
// Zero values (empty strings, 0 numbers, nil slices) in 'update' do not overwrite existing values.
func MergeMetadata(existing *Metadata, update *Metadata) *Metadata {
	if existing == nil {
		existing = &Metadata{}
	}
	if update == nil {
		out := *existing
		return &out
	}

	// Create a copy to avoid modifying the original
	out := *existing

	// Use mergo with WithOverride to merge non-zero values from 'update' into 'out'
	if err := mergo.Merge(&out, update, mergo.WithOverride); err != nil {
		// If merge fails for some reason, fall back to the original
		return existing
	}

	// Handle special case: set LastUsed to now if it's still zero
	if out.LastUsed.IsZero() {
		out.LastUsed = time.Now()
	}

	return &out
}
