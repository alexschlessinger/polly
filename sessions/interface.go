package sessions

import (
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// Session interface defines the contract for session implementations
type Session interface {
    GetHistory() []messages.ChatMessage
    AddMessage(messages.ChatMessage)
    Clear()
    Close() // Clean up resources (file locks, etc.)

    // Session metadata
    GetName() string
    GetContextInfo() *ContextInfo
    SetContextInfo(*ContextInfo)
    GetLastUsed() time.Time
}

// SessionStore manages multiple sessions
type SessionStore interface {
    Get(string) (Session, error)
    Delete(string)
    Range(func(key, value any) bool)
    Expire()

    // Session discovery and metadata
    List() ([]string, error)
    Exists(string) bool
    GetAllContextInfo() map[string]*ContextInfo
    SaveContextInfo(*ContextInfo) error
    GetLastContext() string // Returns name of most recently used session

    // Partial updates to context metadata (only non-nil fields are applied)
    SaveContextUpdate(*ContextUpdate) error
}

// ContextUpdate represents a partial update to context metadata.
// Only fields set to non-nil values will be applied.
type ContextUpdate struct {
    Name         string
    Model        *string
    Temperature  *float64
    SystemPrompt *string
    Description  *string
    ToolPaths    *[]string
    MCPServers   *[]string
    MaxTokens    *int
    MaxHistory   *int
    LastUsed     *time.Time
}
