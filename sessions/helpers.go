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
    
    // Create a copy to avoid modifying the original
    out := *existing
    
    // Use mergo to merge non-zero values from 'in' into 'out'
    // By default, mergo only overwrites zero values
    if err := mergo.Merge(&out, in); err != nil {
        // If merge fails for some reason, fall back to the original
        return existing
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
    
    // Convert ContextUpdate pointers to a temporary ContextInfo with values
    // Only set fields that are non-nil in the update
    tempInfo := ContextInfo{}
    if upd.Name != "" {
        tempInfo.Name = upd.Name
    }
    if upd.Model != nil {
        tempInfo.Model = *upd.Model
    }
    if upd.Temperature != nil {
        tempInfo.Temperature = *upd.Temperature
    }
    if upd.SystemPrompt != nil {
        tempInfo.SystemPrompt = *upd.SystemPrompt
    }
    if upd.Description != nil {
        tempInfo.Description = *upd.Description
    }
    if upd.ToolPaths != nil {
        tempInfo.ToolPaths = *upd.ToolPaths
    }
    if upd.MCPServers != nil {
        tempInfo.MCPServers = *upd.MCPServers
    }
    if upd.MaxTokens != nil {
        tempInfo.MaxTokens = *upd.MaxTokens
    }
    if upd.MaxHistory != nil {
        tempInfo.MaxHistory = *upd.MaxHistory
    }
    if upd.LastUsed != nil {
        tempInfo.LastUsed = *upd.LastUsed
    }
    
    // Use mergo with WithOverride option to overwrite existing values
    // since we're explicitly setting values from the update
    if err := mergo.Merge(&out, tempInfo, mergo.WithOverride); err != nil {
        // If merge fails, return original
        return existing
    }
    
    // Handle special case: set LastUsed to now if it's still zero
    if out.LastUsed.IsZero() {
        out.LastUsed = time.Now()
    }
    
    return &out
}
