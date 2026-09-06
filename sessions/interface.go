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
	ID           string
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
	GetLastUsed(context.Context) (time.Time, error)
	CacheSessionID(context.Context) (string, error)
	ArtifactStore() artifacts.Store

	GetTotalTokens(context.Context) (int, error)
	GetCapacityPercentage(context.Context) (float64, error)
	GetTimeToExpiry(context.Context) (time.Duration, error)
	GetMessageCounts(context.Context) (map[string]int, error)
	GetToolCallCount(context.Context) (int, error)
	// Report posts a report to the session that spawned this one, naming
	// this session as its child. ErrNoParent when there is none.
	Report(context.Context, Report) error
	// TakeReports removes and returns the subagent reports addressed to
	// this session, oldest first. See Report.
	TakeReports(context.Context) ([]Report, error)
	// PeekReports reads pending reports without consuming them or taking a
	// database write lock. IDs identify reports for AddReportMessage.
	PeekReports(context.Context) ([]Report, error)
	// AddReportMessage atomically appends the parent input and consumes only
	// the specified reports addressed to this session.
	AddReportMessage(context.Context, messages.ChatMessage, []int64) error
}

// ReportStatus says how a subagent's run ended.
type ReportStatus string

const (
	ReportFinished ReportStatus = "finished"
	ReportCanceled ReportStatus = "canceled"
	ReportFailed   ReportStatus = "failed"
)

// Report is a subagent's reply addressed to the session whose agent spawned
// it. The store holds it until that session takes it, so the reply reaches
// a parent whose tab was closed, or whose polly has since exited, the next
// time the parent is open. A report is deleted with its addressee.
type Report struct {
	// ID identifies a stored report when read; it is ignored when posting.
	ID int64
	// Child names the session the subagent ran on, as it is named now.
	Child  string
	Status ReportStatus
	// Text is the child's final reply, or what it had said when canceled.
	Text string
	// Error says why a failed run failed.
	Error string
	// InputTokens and OutputTokens are the child's own usage.
	InputTokens  int
	OutputTokens int
	Posted       time.Time
}

// SessionStore manages sessions in one SQLite database.
type SessionStore interface {
	Acquire(context.Context, string, AcquireOptions) (Session, error)
	Delete(context.Context, string) error
	List(context.Context) ([]string, error)
	Exists(context.Context, string) (bool, error)
	GetAllMetadata(context.Context) (map[string]*Metadata, error)
	ListSummaries(context.Context) ([]SessionSummary, error)
	GetLast(context.Context) (string, error)
	// PostReport holds a subagent's report for the named session until that
	// session takes it; the session need not be open. ErrSessionNotFound
	// when no session has that name.
	PostReport(context.Context, string, Report) error
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

	// ContextWindows caches provider-advertised context windows per
	// provider-prefixed model, discovered once and reused to clamp the
	// projection budget. A stale entry only makes the clamp conservative.
	ContextWindows map[string]int `json:"contextWindows,omitempty"`
}
