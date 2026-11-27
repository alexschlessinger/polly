package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The control contract's values as their lines hold them. Members the
// contract leaves out at their defaults are omitempty here, and a reader
// passes over members it does not know.

// ID is an agent's id: its session's, 32 lowercase hex digits.
type ID = string

// IsID reports whether s is an id: 32 lowercase hex digits and nothing else.
func IsID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func jsonString(s string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// Hello is the first request on a connection.
type Hello struct {
	Protocol int    `json:"protocol"`
	Setup    string `json:"setup,omitempty"`
}

// Welcome is what an engine says of itself.
type Welcome struct {
	Protocol int             `json:"protocol"`
	Version  string          `json:"version"`
	Setup    string          `json:"setup,omitempty"`
	Instance string          `json:"instance,omitempty"`
	Servers  []WelcomeServer `json:"servers,omitempty"`
	Scripts  []WelcomeScript `json:"scripts,omitempty"`
}

// WelcomeServer is an MCP server the engine loaded.
type WelcomeServer struct {
	Name       string `json:"name"`
	ServerName string `json:"server_name,omitempty"`
	ToolCount  int    `json:"tool_count,omitempty"`
}

// WelcomeScript is a script tool the engine loaded.
type WelcomeScript struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
}

// Event kinds.
const (
	KindDelta      = "delta"
	KindMessage    = "message"
	KindTool       = "tool"
	KindNotice     = "notice"
	KindCheckpoint = "checkpoint"
	KindEnd        = "end"
)

// AllKinds are the five an agent tells, and the end of a root's turn.
var AllKinds = []string{KindDelta, KindMessage, KindTool, KindNotice, KindCheckpoint, KindEnd}

// Watch says what its sender is sent of an agent from now on.
type Watch struct {
	Agent    ID       `json:"agent"`
	Kinds    []string `json:"kinds,omitempty"`
	Texts    []string `json:"texts,omitempty"`
	Children bool     `json:"children,omitempty"`
	Reply    bool     `json:"reply,omitempty"`
	// Off ends the watch.
	Off bool `json:"-"`
}

// MarshalJSON writes {"agent":..,"on":false} for an ended watch.
func (w Watch) MarshalJSON() ([]byte, error) {
	if w.Off {
		return json.Marshal(struct {
			Agent ID   `json:"agent"`
			On    bool `json:"on"`
		}{w.Agent, false})
	}
	type plain Watch
	return json.Marshal(plain(w))
}

// Watched is what a watch finds of the agent when it takes hold.
type Watched struct {
	Row          *Row     `json:"row,omitempty"`
	Lines        uint64   `json:"lines,omitempty"`
	Reply        *Reply   `json:"reply,omitempty"`
	Refused      *Refusal `json:"refused,omitempty"`
	OutputUnkept bool     `json:"output_unkept,omitempty"`
}

// Session names the session a prompt is for: by ID or Name, the last root
// (Last), or a new one (New, with the id the client drew and a name).
type Session struct {
	ID      ID
	Name    string
	Last    bool
	New     bool
	NewID   ID
	NewName string
}

// MarshalJSON writes the session as the contract names it.
func (s Session) MarshalJSON() ([]byte, error) {
	switch {
	case s.New:
		if s.NewID == "" && s.NewName == "" {
			return []byte(`"new"`), nil
		}
		inner := map[string]string{}
		if s.NewID != "" {
			inner["id"] = s.NewID
		}
		if s.NewName != "" {
			inner["name"] = s.NewName
		}
		return json.Marshal(map[string]any{"new": inner})
	case s.Last:
		return []byte(`"continue"`), nil
	case s.ID != "":
		return json.Marshal(map[string]string{"id": s.ID})
	case s.Name != "":
		return json.Marshal(map[string]string{"name": s.Name})
	}
	return nil, fmt.Errorf("a prompt names no session")
}

// UnmarshalJSON reads a session as the contract names it.
func (s *Session) UnmarshalJSON(data []byte) error {
	var word string
	if json.Unmarshal(data, &word) == nil {
		switch word {
		case "new":
			*s = Session{New: true}
		case "continue":
			*s = Session{Last: true}
		default:
			return fmt.Errorf("a session is named %q", word)
		}
		return nil
	}
	var named struct {
		ID   ID     `json:"id"`
		Name string `json:"name"`
		New  *struct {
			ID   ID     `json:"id"`
			Name string `json:"name"`
		} `json:"new"`
	}
	if err := json.Unmarshal(data, &named); err != nil {
		return err
	}
	switch {
	case named.New != nil:
		*s = Session{New: true, NewID: named.New.ID, NewName: named.New.Name}
	case named.ID != "":
		*s = Session{ID: named.ID}
	case named.Name != "":
		*s = Session{Name: named.Name}
	default:
		return fmt.Errorf("a prompt names no session: %s", data)
	}
	return nil
}

// Prompt is one turn asked of a session.
type Prompt struct {
	Session        Session         `json:"session"`
	Cwd            string          `json:"cwd"`
	Model          string          `json:"model"`
	System         string          `json:"system,omitempty"`
	MaxIterations  uint32          `json:"max_iterations"`
	MaxTokens      uint32          `json:"max_tokens"`
	MaxContext     uint32          `json:"max_context"`
	MaxAgents      uint32          `json:"max_agents,omitempty"`
	Timeout        Nanos           `json:"timeout"`
	ToolTimeout    Nanos           `json:"tool_timeout"`
	Temperature    *float32        `json:"temperature,omitempty"`
	Effort         string          `json:"effort"`
	Allow          []string        `json:"allow,omitempty"`
	Deny           []string        `json:"deny,omitempty"`
	None           bool            `json:"none,omitempty"`
	ResponseSchema json.RawMessage `json:"response_schema,omitempty"`
	Env            []EnvOver       `json:"env,omitempty"`
	TracePath      string          `json:"trace_path,omitempty"`
	Content        json.RawMessage `json:"content"`
}

// Nanos is a duration as the contract writes one: nanoseconds.
type Nanos time.Duration

// MarshalJSON writes the duration's nanoseconds.
func (n Nanos) MarshalJSON() ([]byte, error) { return []byte(fmt.Sprint(int64(n))), nil }

// UnmarshalJSON reads nanoseconds.
func (n *Nanos) UnmarshalJSON(data []byte) error {
	var v int64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*n = Nanos(v)
	return nil
}

// Effort is the word the contract takes for a reasoning effort.
func Effort(effort string) string {
	switch e := strings.ToLower(strings.TrimSpace(effort)); e {
	case "", "off", "none":
		return "off"
	case "auto", "dynamic":
		return "dynamic"
	default:
		return e
	}
}

// Spent is a provider's usage summed over a turn's model calls.
type Spent struct {
	InputTokens      uint64 `json:"input_tokens"`
	OutputTokens     uint64 `json:"output_tokens"`
	CacheReadTokens  uint64 `json:"cache_read_tokens"`
	CacheWriteTokens uint64 `json:"cache_write_tokens"`
}

// Tokens is a count of input and output tokens.
type Tokens struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

// ContextSize is the size of a request against its budget (0: none).
type ContextSize struct {
	Used uint64 `json:"used"`
	Size uint64 `json:"size"`
}

// AgentsSpent is what a turn's child agents used, counted apart.
type AgentsSpent struct {
	Spawned      uint32 `json:"spawned"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

// Stop reasons a turn ends with.
const (
	StopEndTurn       = "end_turn"
	StopMaxTokens     = "max_tokens"
	StopMaxIterations = "max_iterations"
	StopContentFilter = "content_filter"
	StopCancelled     = "cancelled"
	StopError         = "error"
)

// Reply is how a turn ended and what it used.
type Reply struct {
	Session    ID          `json:"session"`
	Name       string      `json:"name,omitempty"`
	Stop       string      `json:"stop"`
	Usage      Spent       `json:"usage"`
	ModelCalls uint32      `json:"model_calls"`
	ToolCalls  uint32      `json:"tool_calls"`
	ToolFails  uint32      `json:"tool_failures"`
	Summaries  Tokens      `json:"summaries,omitempty"`
	Context    ContextSize `json:"context"`
	Agents     AgentsSpent `json:"agents,omitempty"`
	Ns         uint64      `json:"ns"`
	Output     *string     `json:"output,omitempty"`
	Failure    string      `json:"failure,omitempty"`
	Unkept     string      `json:"unkept,omitempty"`
}

// StopReq has an agent end its turn; From names the turn by Row.From.
type StopReq struct {
	Agent ID      `json:"agent"`
	From  *uint64 `json:"from,omitempty"`
}

// Stopped says whether the agent had a turn running.
type Stopped struct {
	Running bool `json:"running"`
}

// AgentsReq asks for rows: by IDs, a window of Parent's children, or a
// window of the roots the engine has.
type AgentsReq struct {
	IDs    []ID   `json:"ids,omitempty"`
	Parent ID     `json:"parent,omitempty"`
	Offset *int64 `json:"offset,omitempty"`
	Limit  uint32 `json:"limit,omitempty"`
	Since  uint64 `json:"since,omitempty"`
}

// MarshalJSON writes offset and limit only for a window, as the contract
// takes them.
func (a AgentsReq) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	if len(a.IDs) > 0 {
		out["ids"] = a.IDs
	} else {
		if a.Parent != "" {
			out["parent"] = a.Parent
		}
		offset := int64(0)
		if a.Offset != nil {
			offset = *a.Offset
		}
		limit := a.Limit
		if limit == 0 {
			limit = AgentsMost
		}
		out["offset"] = offset
		out["limit"] = limit
	}
	if a.Since > 0 {
		out["since"] = a.Since
	}
	return json.Marshal(out)
}

// AgentsMost is the most ids a request names, and the most a window holds.
const AgentsMost = 256

// Rows is what the agents asked for are now.
type Rows struct {
	Ver     uint64 `json:"ver"`
	Total   uint64 `json:"total,omitempty"`
	Offset  uint64 `json:"offset,omitempty"`
	IDs     []ID   `json:"ids,omitempty"`
	Rows    []Row  `json:"rows"`
	Over    []ID   `json:"over,omitempty"`
	Unknown []ID   `json:"unknown,omitempty"`
}

// Row states.
const (
	StateRunning   = "running"
	StateIdle      = "idle"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateSaved     = "saved"
)

// RowChildren counts an agent's descendants.
type RowChildren struct {
	Total   uint32 `json:"total"`
	Running uint32 `json:"running"`
	Failed  uint32 `json:"failed"`
}

// Row is what an agent is, at the moment somebody asked.
type Row struct {
	ID           ID           `json:"id"`
	Parent       ID           `json:"parent,omitempty"`
	Index        *uint64      `json:"index,omitempty"`
	Label        string       `json:"label,omitempty"`
	Model        string       `json:"model,omitempty"`
	Structured   bool         `json:"structured,omitempty"`
	State        string       `json:"state"`
	Phase        string       `json:"phase,omitempty"`
	Stop         string       `json:"stop,omitempty"`
	Activity     string       `json:"activity,omitempty"`
	ModelCalls   uint32       `json:"modelCalls"`
	ToolCalls    uint32       `json:"toolCalls"`
	ToolFailures uint32       `json:"toolFailures"`
	TokensIn     uint64       `json:"tokensIn"`
	TokensOut    uint64       `json:"tokensOut"`
	Context      ContextSize  `json:"context"`
	Started      int64        `json:"started"`
	Updated      int64        `json:"updated"`
	From         *uint64      `json:"from,omitempty"`
	At           uint64       `json:"at"`
	Children     *RowChildren `json:"children,omitempty"`
	Changes      string       `json:"changes,omitempty"`
	Failure      string       `json:"failure,omitempty"`
	Ver          uint64       `json:"ver"`
}

// Running reports a row of an agent with a turn in progress.
func (r Row) Running() bool { return r.State == StateRunning }

// Refusal kinds.
const (
	RefusedBusy     = "busy"
	RefusedNotFound = "not_found"
	RefusedExists   = "exists"
	RefusedInvalid  = "invalid"
	RefusedFailed   = "failed"
	RefusedMismatch = "mismatch"
)

// Refusal says why a request was refused.
type Refusal struct {
	Kind   string `json:"kind"`
	Text   string `json:"text,omitempty"`
	Member string `json:"member,omitempty"`
}

// JoinReq is the first line of a connection that joins a daemon.
type JoinReq struct {
	Protocol     int    `json:"protocol"`
	Setup        string `json:"setup,omitempty"`
	Session      string `json:"session,omitempty"`
	ExistingOnly bool   `json:"existing_only,omitempty"`
}

// Made is how a start or a join is answered.
type Made struct {
	Made      bool        `json:"made,omitempty"`
	NeedStart bool        `json:"need_start,omitempty"`
	NotFound  bool        `json:"not_found,omitempty"`
	Failed    *MadeFailed `json:"failed,omitempty"`
	Again     bool        `json:"again,omitempty"`
	Setup     string      `json:"setup,omitempty"`
}

// MadeFailed is a setup that could not be made.
type MadeFailed struct {
	Text    string          `json:"text"`
	Servers []WelcomeServer `json:"servers,omitempty"`
	Scripts []WelcomeScript `json:"scripts,omitempty"`
}

// ProviderKey is one provider's key as the start line carries it.
type ProviderKey struct {
	Provider string `json:"provider"`
	Key      string `json:"key"`
}

// Setup is what an engine is made with, as the start line carries it.
type Setup struct {
	Key       string   `json:"key"`
	Provider  string   `json:"provider"`
	BaseURL   string   `json:"base_url"`
	Dir       string   `json:"dir"`
	Home      string   `json:"home"`
	Natives   bool     `json:"natives"`
	Servers   []string `json:"servers"`
	Scripts   []string `json:"scripts"`
	Skills    []Place  `json:"skills"`
	Trees     string   `json:"trees"`
	Workspace string   `json:"workspace,omitempty"`
	SkillDirs []string `json:"skill_dirs,omitempty"`
	Outputs   string   `json:"outputs,omitempty"`
}

// Start is the start line: who speaks, the keys, the setup, where the store
// is, and which program's file the setup's key was made with (empty for a
// client that reached the daemon by its socket alone).
type Start struct {
	Protocol   int           `json:"protocol"`
	Keys       []ProviderKey `json:"keys"`
	Setup      Setup         `json:"setup"`
	Runtime    string        `json:"runtime"`
	Executable Program       `json:"executable"`
}

// --------------------------------------------------------------- events

// Event is one event of an agent, as a watcher is told it.
type Event struct {
	Method string
	Agent  ID
	// Parent and Label are a child's; a root's events have neither.
	Parent ID
	Label  string

	Delta      *Delta
	Message    *MessageEvent
	Tool       *ToolEvent
	Notice     *Notice
	Checkpoint *Checkpoint
	End        *End
}

// Delta is a piece of a reply, as the model produces it.
type Delta struct {
	Kind string `json:"kind"` // answer, commentary, reasoning
	At   uint64 `json:"at"`
	Text string `json:"text"`
	Last bool   `json:"last,omitempty"`
}

// Delta text kinds.
const (
	TextAnswer     = "answer"
	TextCommentary = "commentary"
	TextReasoning  = "reasoning"
)

// MessageEvent is a transcript line as it is produced.
type MessageEvent struct {
	Line      json.RawMessage `json:"line"`
	Iteration *uint32         `json:"iteration,omitempty"`
	Usage     *LineUsage      `json:"usage,omitempty"`
}

// LineUsage is what a provider said a model call spent.
type LineUsage struct {
	InputTokens      uint64 `json:"input_tokens,omitempty"`
	OutputTokens     uint64 `json:"output_tokens,omitempty"`
	TokensReported   bool   `json:"tokens_reported,omitempty"`
	CacheReadTokens  uint64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens uint64 `json:"cache_write_tokens,omitempty"`
	CacheReported    bool   `json:"cache_reported,omitempty"`
}

// ToolEvent is a tool call starting or ending.
type ToolEvent struct {
	ID      string `json:"id"`
	Display string `json:"display,omitempty"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	State   string `json:"state"` // start, end
	Index   uint32 `json:"index"`
	Of      uint32 `json:"of"`
	Ns      uint64 `json:"ns,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
}

// Notice is something said beside the conversation: a note, one end of a
// retry's wait, or what a compaction summary spent.
type Notice struct {
	Kind      string     `json:"kind"` // note, retry, summary
	Feature   string     `json:"feature,omitempty"`
	Count     uint32     `json:"count,omitempty"`
	Text      string     `json:"text,omitempty"`
	Waiting   bool       `json:"waiting,omitempty"`
	Attempt   uint32     `json:"attempt,omitempty"`
	Cause     string     `json:"cause,omitempty"`
	DelayNs   uint64     `json:"delay_ns,omitempty"`
	WaitError string     `json:"wait_error,omitempty"`
	Cancelled bool       `json:"cancelled,omitempty"`
	Usage     *LineUsage `json:"usage,omitempty"`
}

// Checkpoint says the transcript is durable up to Lines, and that the save
// added Count of them.
type Checkpoint struct {
	Lines *uint64 `json:"lines,omitempty"`
	Count uint32  `json:"count"`
}

// End says a turn of a session's root has ended.
type End struct {
	From         *uint64  `json:"from,omitempty"`
	At           *uint64  `json:"at,omitempty"`
	Reply        *Reply   `json:"reply,omitempty"`
	Refused      *Refusal `json:"refused,omitempty"`
	OutputUnkept bool     `json:"output_unkept,omitempty"`
}

// ParseEvent reads an event's params by its method. An event of a kind this
// client does not know is returned with none of its bodies set.
func ParseEvent(method string, params json.RawMessage) (Event, error) {
	var head struct {
		Agent  ID     `json:"agent"`
		Parent ID     `json:"parent"`
		Label  string `json:"label"`
	}
	if err := json.Unmarshal(params, &head); err != nil {
		return Event{}, fmt.Errorf("read %s event: %w", method, err)
	}
	ev := Event{Method: method, Agent: head.Agent, Parent: head.Parent, Label: head.Label}
	var body any
	switch method {
	case KindDelta:
		ev.Delta = &Delta{}
		body = ev.Delta
	case KindMessage:
		ev.Message = &MessageEvent{}
		body = ev.Message
	case KindTool:
		ev.Tool = &ToolEvent{}
		body = ev.Tool
	case KindNotice:
		ev.Notice = &Notice{}
		body = ev.Notice
	case KindCheckpoint:
		ev.Checkpoint = &Checkpoint{}
		body = ev.Checkpoint
	case KindEnd:
		ev.End = &End{}
		body = ev.End
	default:
		return ev, nil
	}
	if err := json.Unmarshal(params, body); err != nil {
		return Event{}, fmt.Errorf("read %s event: %w", method, err)
	}
	return ev, nil
}

// ----------------------------------------------------------- store reads

// SessionEntry is one session as `sessions` lists it, and as `info`
// describes its record.
type SessionEntry struct {
	ID           ID       `json:"id"`
	ParentID     ID       `json:"parent_id,omitempty"`
	Name         string   `json:"name,omitempty"`
	LastUsedNs   int64    `json:"last_used_ns,omitempty"`
	Description  string   `json:"description,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Model        string   `json:"model,omitempty"`
	ActiveSkills []string `json:"active_skills,omitempty"`
	Daemon       string   `json:"daemon,omitempty"`
	ResumeFrom   *uint64  `json:"resume_from,omitempty"`
	Lines        uint64   `json:"lines"`
	InUse        bool     `json:"in_use,omitempty"`
}

// LastUsed is when the session was last used.
func (e SessionEntry) LastUsed() time.Time {
	if e.LastUsedNs == 0 {
		return time.Time{}
	}
	return time.Unix(0, e.LastUsedNs)
}

// SessionList is the answer of `sessions`.
type SessionList struct {
	Sessions []SessionEntry `json:"sessions"`
	Last     ID             `json:"last,omitempty"`
}

// SessionsReq lists every session, or the roots alone.
type SessionsReq struct {
	Filter string `json:"filter,omitempty"` // all, roots
}

// InfoReq names one session's record: by ID, by Name, or the last root.
type InfoReq struct {
	ID   ID     `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Last bool   `json:"last,omitempty"`
}

// RecordMetadata is what a record says beside its identity.
type RecordMetadata struct {
	Name         string   `json:"name,omitempty"`
	LastUsedNs   int64    `json:"last_used_ns,omitempty"`
	Description  string   `json:"description,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Model        string   `json:"model,omitempty"`
	ActiveSkills []string `json:"active_skills,omitempty"`
	Daemon       string   `json:"daemon,omitempty"`
	ResumeFrom   *uint64  `json:"resume_from,omitempty"`
}

// Final is the row a finished child's record keeps.
type Final struct {
	State        string      `json:"state"`
	Stop         string      `json:"stop"`
	Index        *uint64     `json:"index,omitempty"`
	ModelCalls   uint32      `json:"modelCalls"`
	ToolCalls    uint32      `json:"toolCalls"`
	ToolFailures uint32      `json:"toolFailures"`
	TokensIn     uint64      `json:"tokensIn"`
	TokensOut    uint64      `json:"tokensOut"`
	Context      ContextSize `json:"context"`
	Started      int64       `json:"started"`
	Updated      int64       `json:"updated"`
	At           uint64      `json:"at"`
	Changes      string      `json:"changes,omitempty"`
}

// Info is the answer of `info`.
type Info struct {
	ID       ID             `json:"id"`
	ParentID ID             `json:"parent_id,omitempty"`
	Metadata RecordMetadata `json:"metadata"`
	Final    *Final         `json:"final,omitempty"`
	Lines    uint64         `json:"lines"`
	InUse    bool           `json:"in_use,omitempty"`
}

// HistoryReq reads a transcript's lines from From, none at or past Upto.
type HistoryReq struct {
	Agent    ID      `json:"agent"`
	From     uint64  `json:"from"`
	At       *uint64 `json:"at,omitempty"`
	Upto     *uint64 `json:"upto,omitempty"`
	MaxBytes uint64  `json:"max_bytes,omitempty"`
}

// History is the answer of `history`: the lines as the store keeps them.
type History struct {
	First uint64            `json:"first"`
	Lines []json.RawMessage `json:"lines"`
	EOF   bool              `json:"eof"`
	End   uint64            `json:"end"`
}

// OutputReq reads a byte range of a saved output.
type OutputReq struct {
	Agent  ID     `json:"agent"`
	Name   string `json:"name"`
	Offset uint64 `json:"offset"`
	Limit  uint64 `json:"limit,omitempty"`
}

// Output is the answer of `output`.
type Output struct {
	Data   []byte `json:"data"` // base64 on the line
	Size   uint64 `json:"size"`
	Offset uint64 `json:"offset"`
	EOF    bool   `json:"eof"`
}
