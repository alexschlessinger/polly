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
	// CleanupInterval sets how often the background sweep deletes expired
	// sessions; zero keeps the one-hour default. The sweep only bounds how
	// long expired rows linger unobserved — Acquire retires an expired,
	// unleased session immediately regardless of this interval.
	CleanupInterval time.Duration
}

// AcquireOptions describe a newly created session. They never alter the
// retention class or parent of an existing session.
type AcquireOptions struct {
	Auto bool
	// ExpectedID, when set, requires the identity returned by ReadView. It
	// refuses a deleted/reused name atomically and implies ExistingOnly.
	ExpectedID string
	// ExistingOnly refuses a missing or expired session instead of creating it.
	ExistingOnly bool
	// Parent names the session whose agent spawns this one. The link is by
	// id, so Metadata.Parent follows the parent's renames; ErrSessionNotFound
	// when no session has the name.
	Parent string
}

// SessionSummary combines persisted metadata with lightweight transcript
// aggregates suitable for session pickers. MessageCount is the number of
// durable messages currently stored for the session. InUse reports a live,
// unexpired lease on the session, whether this process or another one holds
// it; a picker can mark or refuse such sessions instead of waiting on Acquire.
type SessionSummary struct {
	// ID is the stable ViewTarget identity, independent of the session name.
	ID string
	// ParentID is the linked parent's stable identity; empty after deletion.
	ParentID     string
	Metadata     *Metadata
	MessageCount int
	InUse        bool
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
	CacheSessionID(context.Context) (string, error)
	ArtifactStore() artifacts.Store
}

// ReportStatus says how a child agent's delegated run ended; see
// Metadata.SpawnOutcome.
type ReportStatus string

const (
	ReportFinished ReportStatus = "finished"
	ReportCanceled ReportStatus = "canceled"
	ReportFailed   ReportStatus = "failed"
	ReportPaused   ReportStatus = "paused"
)

// SessionStore manages sessions in one SQLite database.
type SessionStore interface {
	Acquire(context.Context, string, AcquireOptions) (Session, error)
	Delete(context.Context, string) error
	List(context.Context) ([]string, error)
	Exists(context.Context, string) (bool, error)
	// GetMetadata reads one session's metadata by name without opening it.
	// ErrSessionNotFound when no session has that name.
	GetMetadata(context.Context, string) (*Metadata, error)
	GetAllMetadata(context.Context) (map[string]*Metadata, error)
	ListSummaries(context.Context) ([]SessionSummary, error)
	GetLast(context.Context) (string, error)
	Expire(context.Context) error
	Close() error
}

// WorkspaceBaseline references a complete baseline owned by this session.
type WorkspaceBaseline struct {
	Root string        `json:"root"`
	Tree string        `json:"tree"`
	Pack artifacts.Ref `json:"pack"`
}

// Metadata stores session metadata and persisted runtime settings. Name,
// Created, LastUsed, and TTL are canonicalized from indexed session columns.
type Metadata struct {
	ChangeBaseline   *WorkspaceBaseline `json:"changeBaseline,omitempty"`
	WorkspaceChanges *artifacts.Ref     `json:"workspaceChanges,omitempty"`

	// SwarmID and ExecutionContext bind managed members to their parent's
	// runtime. A UI may inspect them lease-free; execution resumes via parent.
	SwarmID          string        `json:"swarmID,omitempty"`
	ExecutionContext string        `json:"executionContext,omitempty"`
	Name             string        `json:"name"`
	Title            string        `json:"title,omitempty"`
	TitleSource      TitleSource   `json:"titleSource,omitempty"`
	Created          time.Time     `json:"created"`
	LastUsed         time.Time     `json:"lastUsed"`
	Description      string        `json:"description,omitempty"`
	TTL              time.Duration `json:"ttl,omitempty"`
	// Parent names the session whose agent spawned this one as a subagent,
	// as that session is called now; empty for a session a person started.
	// Canonical from the store's parent link (see AcquireOptions.Parent):
	// SetMetadata and Reset ignore it. A session whose parent was deleted
	// keeps the last name it knew.
	Parent string `json:"parent,omitempty"`
	// SpawnCallID identifies the parent's spawn_agent call. SpawnOutcome
	// records only the initial delegated run; later child turns leave it alone.
	// Once set, SetMetadata and Reset preserve these fields.
	SpawnCallID  string       `json:"spawnCallID,omitempty"`
	SpawnOutcome ReportStatus `json:"spawnOutcome,omitempty"`

	ModelHost        string                 `json:"modelHost,omitempty"`
	Model            string                 `json:"model,omitempty"`
	Temperature      float64                `json:"temperature,omitempty"`
	MaxTokens        int                    `json:"maxTokens,omitempty"`
	MaxHistoryTokens int                    `json:"maxHistoryTokens,omitempty"`
	AutoMaxContext   bool                   `json:"autoMaxContext,omitempty"`
	ThinkingEffort   string                 `json:"thinkingEffort,omitempty"`
	Fast             bool                   `json:"fast,omitempty"`
	SystemPrompt     string                 `json:"systemPrompt,omitempty"`
	ActiveTools      []tools.ToolLoaderInfo `json:"activeTools,omitempty"`
	ActiveSkills     []string               `json:"activeSkills,omitempty"`
	MaxIterations    int                    `json:"maxIterations,omitempty"`
	ToolTimeout      time.Duration          `json:"toolTimeout,omitempty"`
	SkillDirs        []string               `json:"skillDirs,omitempty"`
	SkillSources     []string               `json:"skillSources,omitempty"`
	// ExtraReadDirs lists the canonical absolute real paths of extra
	// read-only directories added with --add-dir / the /add-dir REPL
	// command. The session record is the source of truth: paths are
	// resolved and validated at add time, restored on resume, and kept by
	// SetMetadata, Clear, and Reset (their call sites pass read-modify-write
	// metadata back).
	ExtraReadDirs []string `json:"extraReadDirs,omitempty"`
}
