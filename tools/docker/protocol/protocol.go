// Package protocol is the wire format between polly on the host and the
// polly helper running inside a container. Frames are JSON objects, one per
// line, over the helper's stdin and stdout. The host numbers requests; the
// helper answers each with exactly one terminal reply carrying the same ID,
// after any number of progress frames.
package protocol

import "encoding/json"

// Version is the protocol both sides must speak. A mismatch fails the hello
// exchange; there is no negotiation across versions.
const Version = 1

// MaxFrameBytes bounds one frame, including a tool result's media.
const MaxFrameBytes = 64 << 20

// Frame is one line on the wire.
type Frame struct {
	ID   uint64          `json:"id"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Request types, host to helper.
const (
	TypeHello     = "hello"
	TypeLoad      = "load"
	TypeList      = "list"
	TypeExecute   = "execute"
	TypeCancel    = "cancel"
	TypeSync      = "sync"
	TypeHeartbeat = "heartbeat"
)

// Reply types, helper to host. Progress may precede a result; every other
// reply is terminal for its request.
const (
	TypeWelcome  = "welcome"
	TypeLoaded   = "loaded"
	TypeListed   = "listed"
	TypeProgress = "progress"
	TypeResult   = "result"
	TypeOK       = "ok"
	TypePong     = "pong"
	TypeError    = "error"
)

// Error codes carried by TypeError replies.
const (
	CodeProtocol    = "E_PROTOCOL"
	CodeUnsupported = "E_UNSUPPORTED"
	CodeNotLoaded   = "E_NOT_LOADED"
	CodeNotFound    = "E_NOT_FOUND"
	CodeFrame       = "E_FRAME"
	CodeInternal    = "E_INTERNAL"
	// CodeHelperLost is raised on the host when the stream ends while a
	// request is in flight.
	CodeHelperLost = "E_HELPER_LOST"
)

// NetworkPolicy is the container's network posture, mirrored into the
// helper's in-process policy.
type NetworkPolicy struct {
	Allow   bool `json:"allow"`
	DenyDNS bool `json:"denyDNS,omitempty"`
}

// GitIdentity is written into the helper's Git configuration from values;
// the host's configuration files never travel.
type GitIdentity struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// Hello opens a session. Env is the sealed environment: the host-selected
// NAME=VALUE entries a tool process receives, delivered only here, never in
// the container's own environment or any argv.
type Hello struct {
	Protocol     int           `json:"protocol"`
	Polly        string        `json:"polly,omitempty"`
	Session      string        `json:"session,omitempty"`
	Mode         string        `json:"mode"`
	Root         string        `json:"root"`
	SourceRoot   string        `json:"sourceRoot,omitempty"`
	Scratch      string        `json:"scratch,omitempty"`
	ReadOnly     bool          `json:"readOnly,omitempty"`
	DeniedReads  []string      `json:"deniedReads,omitempty"`
	DeniedWrites []string      `json:"deniedWrites,omitempty"`
	Network      NetworkPolicy `json:"network"`
	AllowEnv     []string      `json:"allowEnv,omitempty"`
	PassEnv      []string      `json:"passEnv,omitempty"`
	Env          []string      `json:"env,omitempty"`
	Home         string        `json:"home,omitempty"`
	GitIdent     GitIdentity   `json:"gitIdent"`
}

// Welcome answers a compatible hello.
type Welcome struct {
	Protocol int    `json:"protocol"`
	Polly    string `json:"polly,omitempty"`
	Platform string `json:"platform"`
	UID      int    `json:"uid"`
	GID      int    `json:"gid"`
}

// ToolSpec names a tool to load inside the container, in the form sessions
// persist: a native tool by name, a shell tool by path, an MCP server by
// spec with the tool's name.
type ToolSpec struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

// Load asks the helper to build its registry.
type Load struct {
	Tools []ToolSpec `json:"tools,omitempty"`
	// Sources are raw tool sources as a command line names them (a native
	// tool name, a shell tool path, an MCP config), resolved inside the
	// container the way the host resolves --tool.
	Sources      []string `json:"sources,omitempty"`
	SkillRoots   []string `json:"skillRoots,omitempty"`
	ActiveSkills []string `json:"activeSkills,omitempty"`
	AutoActivate []string `json:"autoActivate,omitempty"`
	// AllowedTools is the scope's selection: nil inherits, empty disables,
	// patterns select. The helper's native binding applies it.
	AllowedTools []string `json:"allowedTools"`
}

// SandboxInfo mirrors tools.SandboxInfo for display on the host.
type SandboxInfo struct {
	Capable       bool     `json:"capable"`
	Active        bool     `json:"active"`
	OptedOut      bool     `json:"optedOut,omitempty"`
	WritablePaths []string `json:"writablePaths,omitempty"`
}

// ToolInfo describes one tool the helper serves: its schema and the optional
// interfaces the host proxy must present.
type ToolInfo struct {
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	Source        string          `json:"source"`
	Schema        json.RawMessage `json:"schema"`
	Strict        bool            `json:"strict,omitempty"`
	Output        bool            `json:"output,omitempty"`
	Untimed       bool            `json:"untimed,omitempty"`
	Exclusive     bool            `json:"exclusive,omitempty"`
	RecallStub    string          `json:"recallStub,omitempty"`
	Coordinates   bool            `json:"coordinates,omitempty"`
	AlwaysAllowed bool            `json:"alwaysAllowed,omitempty"`
	Builtin       bool            `json:"builtin,omitempty"`
	Sandbox       *SandboxInfo    `json:"sandbox,omitempty"`
}

// Listed answers a list request.
type Listed struct {
	Tools []ToolInfo `json:"tools"`
}

// Loaded answers a load request.
type Loaded struct {
	Listed
	Omitted          []string `json:"omitted,omitempty"`
	Instructions     string   `json:"instructions,omitempty"`
	ToolInstructions string   `json:"toolInstructions,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

// Execute runs one tool. TimeoutMillis is the host's remaining deadline,
// relative because the clocks may differ and rounded up so the helper's
// deadline never precedes the host's.
type Execute struct {
	Tool          string         `json:"tool"`
	Args          map[string]any `json:"args"`
	TimeoutMillis int64          `json:"timeoutMillis,omitempty"`
	Pipefail      bool           `json:"pipefail,omitempty"`
}

// Progress is streamed output before a result. The first version emits none.
type Progress struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// Media is one tools.ToolMedia; Data travels base64-encoded.
type Media struct {
	Data      []byte `json:"data"`
	MIMEType  string `json:"mimeType"`
	Name      string `json:"name,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// Error kinds a tool failure is reported as.
const (
	ErrorKindTool    = "tool"
	ErrorKindCommand = "command"
	ErrorKindPlain   = "plain"
)

// ToolError carries a tool's failure so the host can rebuild the same
// error type.
type ToolError struct {
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Code     string `json:"code,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
}

// Context outcomes of an execution.
const (
	ContextCanceled = "canceled"
	ContextDeadline = "deadline"
)

// Allowance is the skill allow-list a skill activation staged.
type Allowance struct {
	Patterns    []string `json:"patterns,omitempty"`
	AutoAllowed []string `json:"autoAllowed,omitempty"`
}

// Result answers an execute request.
type Result struct {
	Invoked    bool            `json:"invoked"`
	Text       string          `json:"text,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Media      []Media         `json:"media,omitempty"`
	Error      *ToolError      `json:"error,omitempty"`
	ContextErr string          `json:"contextErr,omitempty"`
	// Wrote reports that a file tool changed the workspace; bash, shell and
	// MCP tools are assumed to have written.
	Wrote bool `json:"wrote,omitempty"`
	// Staged lists tools a skill activation registered helper-side; the
	// host mirrors them and its own commit publishes them.
	Staged    []ToolInfo `json:"staged,omitempty"`
	Allowance *Allowance `json:"allowance,omitempty"`
}

// Cancel asks the helper to cancel an in-flight execute request.
type Cancel struct {
	ID uint64 `json:"id"`
}

// Error is a terminal failure reply.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Sync operations, host to helper, for a copy-mode container.
const (
	// SyncBootstrap initialises the copy: a repository at Root fetched from
	// Bundle and checked out at Commit. The host put the bundle inside Root.
	SyncBootstrap = "bootstrap"
	// SyncApply announces host-side changes: Deleted paths are removed before
	// the host puts Changed files, which the helper then counts as known.
	SyncApply = "apply"
	// SyncCollect asks for the copy's changes since the last collect: the
	// helper stages them for the host to fetch and lists deletions.
	SyncCollect = "collect"
	// SyncCollected tells the helper the host fetched a staging directory.
	SyncCollected = "collected"
	// SyncReset moves the copy to Commit (fetched from Bundle when set) and
	// discards every change but ignored files.
	SyncReset = "reset"
)

// Sync is a sync request.
type Sync struct {
	Op      string   `json:"op"`
	Root    string   `json:"root,omitempty"`
	Commit  string   `json:"commit,omitempty"`
	Bundle  string   `json:"bundle,omitempty"`
	Changed []string `json:"changed,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
	Seq     int      `json:"seq,omitempty"`
}

// Synced answers a sync request. A collect names the staging directory
// holding the changed files, empty when nothing changed.
type Synced struct {
	Seq     int      `json:"seq,omitempty"`
	Staging string   `json:"staging,omitempty"`
	Changed []string `json:"changed,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
}

// TypeSynced answers a sync request.
const TypeSynced = "synced"

// BundleRef is the reference a bundle carries the base commit under: a
// bundle needs a reference, and a copy fetches it by that name.
func BundleRef(commit string) string { return "refs/polly/copy-base/" + commit }
