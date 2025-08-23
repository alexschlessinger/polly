package sessions

import (
    "slices"
    "time"

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

// MergeContextInfo merges non-zero fields from 'in' into 'existing' and returns a new value.
// Zero values (empty strings, 0 numbers, nil slices) in 'in' do not overwrite existing values.
func MergeContextInfo(existing *ContextInfo, in *ContextInfo) *ContextInfo {
    if existing == nil {
        existing = &ContextInfo{}
    }
    if in == nil {
        // Return a copy of existing
        out := *existing
        return &out
    }
    out := *existing
    if in.Name != "" {
        out.Name = in.Name
    }
    if !in.Created.IsZero() {
        out.Created = in.Created
    }
    if !in.LastUsed.IsZero() {
        out.LastUsed = in.LastUsed
    }
    if in.Model != "" {
        out.Model = in.Model
    }
    if in.Temperature != 0 {
        out.Temperature = in.Temperature
    }
    if in.SystemPrompt != "" {
        out.SystemPrompt = in.SystemPrompt
    }
    if in.Description != "" {
        out.Description = in.Description
    }
    if len(in.ToolPaths) > 0 {
        out.ToolPaths = in.ToolPaths
    }
    if len(in.MCPServers) > 0 {
        out.MCPServers = in.MCPServers
    }
    if in.MaxTokens != 0 {
        out.MaxTokens = in.MaxTokens
    }
    if in.MaxHistory != 0 {
        out.MaxHistory = in.MaxHistory
    }
    if in.TTL != 0 {
        out.TTL = in.TTL
    }
    return &out
}

// ApplyContextUpdate applies a partial ContextUpdate onto existing ContextInfo and returns a new value.
func ApplyContextUpdate(existing *ContextInfo, upd *ContextUpdate) *ContextInfo {
    if existing == nil {
        existing = &ContextInfo{}
    }
    if upd == nil {
        out := *existing
        return &out
    }
    out := *existing
    if upd.Name != "" {
        out.Name = upd.Name
    }
    if upd.Model != nil {
        out.Model = *upd.Model
    }
    if upd.Temperature != nil {
        out.Temperature = *upd.Temperature
    }
    if upd.SystemPrompt != nil {
        out.SystemPrompt = *upd.SystemPrompt
    }
    if upd.Description != nil {
        out.Description = *upd.Description
    }
    if upd.ToolPaths != nil {
        out.ToolPaths = *upd.ToolPaths
    }
    if upd.MCPServers != nil {
        out.MCPServers = *upd.MCPServers
    }
    if upd.MaxTokens != nil {
        out.MaxTokens = *upd.MaxTokens
    }
    if upd.MaxHistory != nil {
        out.MaxHistory = *upd.MaxHistory
    }
    if upd.LastUsed != nil {
        out.LastUsed = *upd.LastUsed
    } else if out.LastUsed.IsZero() {
        out.LastUsed = time.Now()
    }
    return &out
}
