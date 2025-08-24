package sessions

import (
	"slices"
	"time"

	"dario.cat/mergo"
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
