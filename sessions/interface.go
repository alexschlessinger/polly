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
	GetMetadata() *Metadata
	SetMetadata(*Metadata)
	UpdateMetadata(*Metadata) error // Apply partial updates (only non-zero values)
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
	GetAllMetadata() map[string]*Metadata // Read-only bulk operation
	GetLast() string                      // Returns name of most recently used session
}

// Metadata stores metadata about a context
type Metadata struct {
	// Persistence-specific fields
	Name        string        `json:"name"`
	Created     time.Time     `json:"created"`
	LastUsed    time.Time     `json:"lastUsed"`
	Description string        `json:"description,omitempty"`
	TTL         time.Duration `json:"ttl,omitempty"` // Time before context expires (0 = never)

	// Settings that can be persisted
	Model          string        `json:"model,omitempty"`
	Temperature    float64       `json:"temperature,omitempty"`
	MaxTokens      int           `json:"maxTokens,omitempty"`
	MaxHistory     int           `json:"maxHistory,omitempty"`
	ThinkingEffort string        `json:"thinkingEffort,omitempty"`
	SystemPrompt   string        `json:"systemPrompt,omitempty"`
	ToolPaths      []string      `json:"toolPaths,omitempty"`
	MCPServers     []string      `json:"mcpServers,omitempty"`
	ToolTimeout    time.Duration `json:"toolTimeout,omitempty"`
}
