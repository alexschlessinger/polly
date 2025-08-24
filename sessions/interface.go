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
