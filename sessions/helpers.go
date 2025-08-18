package sessions

import (
	"slices"

	"github.com/alexschlessinger/pollytool/messages"
)

// TrimHistory applies smart trimming to a message history slice
// It keeps the system prompt (first message) and the most recent MaxHistory messages
// It also removes orphaned tool responses that would violate API constraints
func TrimHistory(history []messages.ChatMessage, maxHistory int) []messages.ChatMessage {
	if maxHistory == 0 {
		return history // No limit
	}
	
	// Allow system prompt + MaxHistory messages
	if len(history) <= maxHistory+1 {
		return history
	}

	// Keep the first message (system prompt) and the most recent MaxHistory messages
	history = append(history[:1], history[len(history)-maxHistory:]...)

	// Handle the API constraint: tool responses must follow tool_calls
	// If the second message is a tool response, remove it
	if len(history) > 1 && history[1].Role == messages.MessageRoleTool {
		history = slices.Delete(history, 1, 2)
	}
	
	return history
}

// InitializeWithSystemPrompt initializes a history slice with the system prompt if configured
func InitializeWithSystemPrompt(history []messages.ChatMessage, config *SessionConfig) []messages.ChatMessage {
	history = history[:0]
	if config != nil && config.SystemPrompt != "" {
		history = append(history, messages.ChatMessage{
			Role:    messages.MessageRoleSystem,
			Content: config.SystemPrompt,
		})
	}
	return history
}

// CopyHistory creates a defensive copy of the history slice
func CopyHistory(history []messages.ChatMessage) []messages.ChatMessage {
	result := make([]messages.ChatMessage, len(history))
	copy(result, history)
	return result
}