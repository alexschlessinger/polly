package sessions

import (
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// ArtifactSession is an optional extension implemented by Polly's built-in
// sessions. Keeping it separate preserves the Session interface for external
// stores that do not need model-context artifacts.
type ArtifactSession interface {
	ArtifactStore() (artifacts.Store, error)
}

// Session interface defines the contract for session implementations
type Session interface {
	GetHistory() []messages.ChatMessage
	AddMessage(messages.ChatMessage) error
	AddMessages([]messages.ChatMessage) error // Append a batch with a single persist
	Clear() error
	Close() // Clean up resources (file locks, etc.)

	// Session metadata
	GetName() string
	GetMetadata() *Metadata
	SetMetadata(*Metadata) error
	UpdateMetadata(*Metadata) error // Apply partial updates (only non-zero values)
	GetLastUsed() time.Time

	// Capacity tracking
	GetTotalTokens() int            // Sum of all message tokens in history
	GetCapacityPercentage() float64 // Durable estimate / model budget; may exceed 100, or 0 if unlimited

	// Session statistics
	GetTimeToExpiry() time.Duration   // Time until TTL expiry (0 if no TTL or expired)
	GetMessageCounts() map[string]int // Counts by role (user, assistant, tool, system)
	GetToolCallCount() int            // Total tool calls in session
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
	Model            string                 `json:"model,omitempty"`
	Temperature      float64                `json:"temperature,omitempty"`
	MaxTokens        int                    `json:"maxTokens,omitempty"`
	MaxHistoryTokens int                    `json:"maxHistoryTokens,omitempty"` // Provider-visible model projection budget
	ThinkingEffort   string                 `json:"thinkingEffort,omitempty"`
	SystemPrompt     string                 `json:"systemPrompt,omitempty"`
	ActiveTools      []tools.ToolLoaderInfo `json:"activeTools,omitempty"`
	ActiveSkills     []string               `json:"activeSkills,omitempty"`
	MaxIterations    int                    `json:"maxIterations,omitempty"`
	ToolTimeout      time.Duration          `json:"toolTimeout,omitempty"`
	SkillDirs        []string               `json:"skillDirs,omitempty"`
	SkillSources     []string               `json:"skillSources,omitempty"`
}
