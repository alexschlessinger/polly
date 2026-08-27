package sessions

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// StoreMode selects only the SQLite database location. Memory and disk stores
// otherwise share the same schema and behavior.
type StoreMode uint8

const (
	ModeMemory StoreMode = iota + 1
	ModeDisk
)

// StoreConfig configures a unified SQLite session store.
type StoreConfig struct {
	Mode            StoreMode
	Path            string
	DefaultMetadata *Metadata
	AutoSessionTTL  time.Duration
}

// AcquireOptions describe a newly created session. They never alter the
// retention class of an existing session.
type AcquireOptions struct {
	Auto bool
}

// Session is an exclusively leased, database-backed conversation. Context is
// canceled if the lease is lost, the session is closed, or its store closes.
type Session interface {
	Context() context.Context

	GetHistory(context.Context) ([]messages.ChatMessage, error)
	AddMessage(context.Context, messages.ChatMessage) error
	AddMessages(context.Context, []messages.ChatMessage) error
	// Clear removes the transcript and artifacts while preserving metadata.
	Clear(context.Context) error
	// Reset atomically replaces metadata and clears the transcript and artifacts.
	Reset(context.Context, *Metadata) error
	Close() error

	GetName(context.Context) (string, error)
	Rename(context.Context, string) error
	GetMetadata(context.Context) (*Metadata, error)
	SetMetadata(context.Context, *Metadata) error
	GetLastUsed(context.Context) (time.Time, error)
	CacheSessionID(context.Context) (string, error)
	ArtifactStore() artifacts.Store

	GetTotalTokens(context.Context) (int, error)
	GetCapacityPercentage(context.Context) (float64, error)
	GetTimeToExpiry(context.Context) (time.Duration, error)
	GetMessageCounts(context.Context) (map[string]int, error)
	GetToolCallCount(context.Context) (int, error)
}

// SessionStore manages sessions in one SQLite database.
type SessionStore interface {
	Acquire(context.Context, string, AcquireOptions) (Session, error)
	Delete(context.Context, string) error
	List(context.Context) ([]string, error)
	Exists(context.Context, string) (bool, error)
	GetAllMetadata(context.Context) (map[string]*Metadata, error)
	GetLast(context.Context) (string, error)
	Expire(context.Context) error
	Close() error
}

// Metadata stores session metadata and persisted runtime settings. Name,
// Created, LastUsed, and TTL are canonicalized from indexed session columns.
type Metadata struct {
	Name        string        `json:"name"`
	Created     time.Time     `json:"created"`
	LastUsed    time.Time     `json:"lastUsed"`
	Description string        `json:"description,omitempty"`
	TTL         time.Duration `json:"ttl,omitempty"`

	Model            string                 `json:"model,omitempty"`
	Temperature      float64                `json:"temperature,omitempty"`
	MaxTokens        int                    `json:"maxTokens,omitempty"`
	MaxHistoryTokens int                    `json:"maxHistoryTokens,omitempty"`
	ThinkingEffort   string                 `json:"thinkingEffort,omitempty"`
	SystemPrompt     string                 `json:"systemPrompt,omitempty"`
	ActiveTools      []tools.ToolLoaderInfo `json:"activeTools,omitempty"`
	ActiveSkills     []string               `json:"activeSkills,omitempty"`
	MaxIterations    int                    `json:"maxIterations,omitempty"`
	ToolTimeout      time.Duration          `json:"toolTimeout,omitempty"`
	SkillDirs        []string               `json:"skillDirs,omitempty"`
	SkillSources     []string               `json:"skillSources,omitempty"`
}
