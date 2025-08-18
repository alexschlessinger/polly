package sessions

import "time"

// SessionConfig holds configuration for session management
type SessionConfig struct {
	// MaxHistory is the maximum number of messages to keep in history
	// 0 means unlimited
	MaxHistory int

	// TTL is the duration after which inactive sessions expire
	// 0 means no expiration (infinite)
	TTL time.Duration

	// SystemPrompt is the system prompt to use when initializing new sessions
	SystemPrompt string
}

// DefaultConfig returns a SessionConfig with sensible defaults
func DefaultConfig() *SessionConfig {
	return &SessionConfig{
		MaxHistory:   0,  // Unlimited by default
		TTL:          0,  // No expiration by default (infinite)
		SystemPrompt: "", // No system prompt by default
	}
}