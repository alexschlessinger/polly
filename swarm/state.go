// Package swarm provides durable coordination for one parent and its direct
// children. Workflow scripts and model tools share this runtime's scheduler.
package swarm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

const DefaultMaxConcurrent = 32
const DefaultMaxExecutions = 256

var ErrYielded = errors.New("agent yielded to await a swarm event")
var ErrBudget = errors.New("swarm execution budget exhausted; explicit resume is required")

type Run struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Starts int    `json:"starts"`
	Limit  int    `json:"limit"`
}
type Usage struct {
	Samples           int  `json:"samples,omitempty"`
	InputTokens       *int `json:"inputTokens"`
	OutputTokens      *int `json:"outputTokens"`
	CachedInputTokens *int `json:"cachedInputTokens"`
}
type Member struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Label      string   `json:"label"`
	Status     string   `json:"status"`
	Controller string   `json:"controller,omitempty"`
	Context    string   `json:"context"`
	Tools      []string `json:"tools"`
	Model      string   `json:"model"`
	Task       string   `json:"task,omitempty"`
	Execution  string   `json:"execution,omitempty"`
	ReadOnly   bool     `json:"readOnly"`
}
type Task struct {
	Execution        string   `json:"execution,omitempty"`
	ID               string   `json:"id"`
	Run              string   `json:"run"`
	Description      string   `json:"description"`
	Criteria         string   `json:"criteria"`
	Dependencies     []string `json:"dependencies"`
	Owner            string   `json:"owner,omitempty"`
	Status           string   `json:"status"`
	Revision         int      `json:"revision"`
	AcceptedRevision int      `json:"acceptedRevision,omitempty"`
	Result           any      `json:"result,omitempty"`
	Feedback         string   `json:"feedback,omitempty"`
	Snapshot         string   `json:"snapshot,omitempty"`
}
type Mail struct {
	ReplyID   string    `json:"replyID,omitempty"`
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Kind      string    `json:"kind"`
	ReplyTo   string    `json:"replyTo,omitempty"`
	Text      string    `json:"text"`
	Delivered bool      `json:"delivered"`
	Posted    time.Time `json:"posted"`
}
type Publication struct {
	ID         string          `json:"id"`
	Author     string          `json:"author"`
	Run        string          `json:"run"`
	Text       string          `json:"text"`
	Supersedes string          `json:"supersedes,omitempty"`
	Snapshot   string          `json:"snapshot,omitempty"`
	Artifacts  []artifacts.Ref `json:"artifacts,omitempty"`
	Sources    []string        `json:"sources,omitempty"`
	Posted     time.Time       `json:"posted"`
}
type Execution struct {
	InputSaved   bool                   `json:"inputSaved"`
	Intent       []messages.ChatMessage `json:"intent,omitempty"`
	Usage        Usage                  `json:"usage"`
	ID           string                 `json:"id"`
	Run          string                 `json:"run"`
	Member       string                 `json:"member"`
	Controller   string                 `json:"controller,omitempty"`
	Status       string                 `json:"status"`
	Request      AgentRequest           `json:"request"`
	Iterations   int                    `json:"iterations"`
	Generation   int                    `json:"generation"`
	PendingTools any                    `json:"pendingTools,omitempty"`
	Result       *AgentResult           `json:"result,omitempty"`
	Error        string                 `json:"error,omitempty"`
	StopReason   messages.StopReason    `json:"stopReason,omitempty"`
}
type ExecutionContext struct {
	ID       string             `json:"id"`
	Owner    string             `json:"owner"`
	Root     string             `json:"root"`
	ReadOnly bool               `json:"readOnly"`
	Checkout *worktree.Checkout `json:"checkout,omitempty"`
}
type ParentTurn struct {
	Intent []messages.ChatMessage `json:"intent,omitempty"`
}

type State struct {
	ParentTurns  map[string]*ParentTurn        `json:"parentTurns"`
	Runs         map[string]*Run               `json:"runs"`
	Members      map[string]*Member            `json:"members"`
	Tasks        map[string]*Task              `json:"tasks"`
	Messages     map[string]*Mail              `json:"messages"`
	Publications map[string]*Publication       `json:"publications"`
	Executions   map[string]*Execution         `json:"executions"`
	Contexts     map[string]*ExecutionContext  `json:"contexts"`
	Snapshots    map[string]*worktree.Snapshot `json:"snapshots"`
	Previews     map[string]*worktree.Preview  `json:"previews"`
	Workflows    map[string]*workflow.Report   `json:"workflows"`
}

func decodeState(raw *sessions.CoordinationState) (*State, error) {
	s := &State{}
	// Keep each domain in separately keyed records. All affected records and
	// transcript receipts commit in the same SQLite transaction.
	fields := map[string]any{"parent_turn": &s.ParentTurns, "run": &s.Runs, "member": &s.Members, "task": &s.Tasks, "mail": &s.Messages, "publication": &s.Publications, "execution": &s.Executions, "context": &s.Contexts, "snapshot": &s.Snapshots, "preview": &s.Previews, "workflow": &s.Workflows}
	for kind, target := range fields {
		records := raw.Records[kind]
		if records == nil {
			records = map[string]json.RawMessage{}
		}
		data, err := json.Marshal(records)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, target); err != nil {
			return nil, fmt.Errorf("read swarm %s: %w", kind, err)
		}
	}
	s.normalizeIterationLimits()
	return s, nil
}
func encodeState(raw *sessions.CoordinationState, s *State) error {
	fields := map[string]any{"parent_turn": s.ParentTurns, "run": s.Runs, "member": s.Members, "task": s.Tasks, "mail": s.Messages, "publication": s.Publications, "execution": s.Executions, "context": s.Contexts, "snapshot": s.Snapshots, "preview": s.Previews, "workflow": s.Workflows}
	for kind, value := range fields {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		var group map[string]json.RawMessage
		if err = json.Unmarshal(data, &group); err != nil {
			return err
		}
		raw.Records[kind] = group
	}
	return nil
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func fail(code, message string) error { return &workflow.Error{Code: code, Message: message} }

func (r *Runtime) read(ctx context.Context) (*State, error) {
	raw, err := r.parent.ReadCoordination(ctx)
	if err != nil {
		return nil, err
	}
	return decodeState(raw)
}
func (r *Runtime) update(ctx context.Context, fn func(*State) error) error {
	if err := r.prepare(ctx); err != nil {
		return err
	}
	err := r.parent.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		s, err := decodeState(raw)
		if err != nil {
			return err
		}
		if err = fn(s); err != nil {
			return err
		}
		return encodeState(raw, s)
	})
	if err == nil {
		r.changed()
	}
	return err
}
func (r *Runtime) currentRun(s *State) *Run {
	for _, run := range s.Runs {
		if run.Status == "running" || run.Status == "paused" {
			return run
		}
	}
	run := &Run{ID: newID(), Status: "running", Limit: r.config.MaxExecutions}
	s.Runs[run.ID] = run
	return run
}

func (r *Runtime) pauseBudget(ctx context.Context, id string) error {
	return r.update(ctx, func(s *State) error {
		if run := s.Runs[id]; run != nil && run.Status == "running" {
			run.Status = "paused"
		}
		return nil
	})
}
func member(s *State, parent, actor string) error {
	if actor == parent {
		return nil
	}
	if s.Members[actor] == nil {
		return fail("unknown_member", "caller is not a member of this swarm")
	}
	return nil
}

func compactRoster(s *State) string {
	ids := make([]string, 0, len(s.Members))
	for id := range s.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("Roster at assignment start (use list_agents to refresh):\n")
	for _, id := range ids[:min(32, len(ids))] {
		m := s.Members[id]
		fmt.Fprintf(&b, "%s · %s · %s · task %s\n", m.ID, m.Label, m.Status, m.Task)
	}
	if len(ids) > 32 {
		fmt.Fprintf(&b, "%d additional members available through list_agents.\n", len(ids)-32)
	}
	return b.String()
}

func (r *Runtime) State(ctx context.Context) (*State, error) { return r.read(ctx) }
func (r *Runtime) CreateTask(ctx context.Context, description, criteria string, deps []string, owner string) (*Task, error) {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	if description == "" {
		return nil, errors.New("task description is required")
	}
	var task *Task
	err := r.update(ctx, func(s *State) error {
		run := r.currentRun(s)
		for _, dep := range deps {
			if s.Tasks[dep] == nil || s.Tasks[dep].Run != run.ID {
				return fmt.Errorf("unknown dependency %s", dep)
			}
		}
		if owner != "" && s.Members[owner] == nil {
			return errors.New("unknown task owner")
		}
		task = &Task{ID: newID(), Run: r.currentRun(s).ID, Description: description, Criteria: criteria, Dependencies: deps, Owner: owner, Status: "pending", Revision: 1}
		s.Tasks[task.ID] = task
		return nil
	})
	return task, err
}
func depsDone(s *State, t *Task) bool {
	for _, id := range t.Dependencies {
		dep := s.Tasks[id]
		if dep == nil || dep.Status != "done" {
			return false
		}
	}
	return true
}
func (r *Runtime) Claim(ctx context.Context, actor, taskID string, revision int) error {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.update(ctx, func(s *State) error {
		if err := member(s, r.ID, actor); err != nil {
			return err
		}
		t := s.Tasks[taskID]
		if t == nil || t.Run != r.currentRun(s).ID || t.Revision != revision || t.Owner != "" || t.Status != "pending" || !depsDone(s, t) {
			return fail("not_claimable", "task is no longer claimable")
		}
		t.Owner = actor
		t.Status = "running"
		m := s.Members[actor]
		if m == nil {
			return errors.New("only members can claim tasks")
		}
		if old := s.Tasks[m.Task]; old != nil && old.ID != t.ID && old.Status == "running" {
			return errors.New("submit or block the current assignment before claiming another")
		}
		m.Task, t.Execution = t.ID, m.Execution
		t.Revision++
		return nil
	})
}
func (r *Runtime) Submit(ctx context.Context, actor, taskID string, revision int, result any, snapshot string) error {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.update(ctx, func(s *State) error {
		t := s.Tasks[taskID]
		if t == nil || t.Owner != actor || t.Revision != revision || t.Status == "canceled" || t.Status == "done" {
			return fail("stale_task", "task owner or revision changed")
		}
		if snapshot != "" && s.Snapshots[snapshot] == nil {
			return errors.New("unknown snapshot")
		}
		if owner := s.Members[actor]; owner != nil {
			c := s.Contexts[owner.Context]
			if !owner.ReadOnly && c != nil && c.Checkout != nil && snapshot == "" {
				return errors.New("editing results require an immutable candidate from swarm_snapshot")
			}
			if snapshot != "" && (c == nil || s.Snapshots[snapshot].Source != c.Root) {
				return errors.New("task snapshot must come from its owner's execution context")
			}
		}
		t.Result = result
		t.AcceptedRevision = 0
		t.Snapshot = snapshot
		t.Status = "awaiting_review"
		t.Revision++
		return nil
	})
}
func (r *Runtime) Review(ctx context.Context, taskID string, revision int, accept bool, feedback string) error {
	r.parentTools.Lock()
	var wake string
	err := r.update(ctx, func(s *State) error {
		t := s.Tasks[taskID]
		if t == nil || t.Status != "awaiting_review" || t.Revision != revision {
			return fail("stale_task", "review must name the current submitted revision")
		}
		if accept {
			if owner := s.Members[t.Owner]; owner != nil && !owner.ReadOnly && t.Snapshot == "" {
				return errors.New("editing task has no integration candidate")
			}
			t.AcceptedRevision = t.Revision
			if t.Snapshot == "" {
				t.Status = "done"
			}
		} else {
			if strings.TrimSpace(feedback) == "" {
				return errors.New("changes requested requires feedback")
			}
			t.AcceptedRevision = 0
			t.Status = "changes_requested"
			t.Feedback = feedback
			t.Revision++
			mail := &Mail{ID: newID(), From: r.ID, To: t.Owner, Kind: "request", Text: "Changes requested for task " + t.ID + ": " + feedback, Posted: time.Now().UTC()}
			s.Messages[mail.ID] = mail
			wake = t.Owner
		}
		return nil
	})
	r.parentTools.Unlock()
	if err == nil && wake != "" {
		r.wake(wake)
	}
	return err
}
func (r *Runtime) CancelTask(ctx context.Context, taskID string) error {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.update(ctx, func(s *State) error {
		t := s.Tasks[taskID]
		if t == nil {
			return errors.New("unknown task")
		}
		t.Status = "canceled"
		t.AcceptedRevision = 0
		t.Revision++
		for _, other := range s.Tasks {
			for _, dep := range other.Dependencies {
				if dep == taskID && other.Status != "done" && other.Status != "canceled" {
					other.Status = "blocked"
					other.Feedback = "dependency canceled: " + taskID
					other.Revision++
				}
			}
		}
		return nil
	})
}
func (r *Runtime) Send(ctx context.Context, actor, to, kind, replyTo, text string) (*Mail, error) {
	var mail *Mail
	err := r.update(ctx, func(s *State) error {
		if err := member(s, r.ID, actor); err != nil {
			return err
		}
		if err := member(s, r.ID, to); err != nil {
			return err
		}
		if text == "" {
			return errors.New("message text is required")
		}
		if kind != "info" && kind != "request" && kind != "reply" {
			return errors.New("message kind must be info, request or reply")
		}
		if kind == "reply" {
			prior := s.Messages[replyTo]
			if prior == nil || prior.Kind != "request" || prior.From != to || prior.To != actor || prior.ReplyID != "" {
				return errors.New("reply does not match an addressed request")
			}
		}
		mail = &Mail{ID: newID(), From: actor, To: to, Kind: kind, ReplyTo: replyTo, Text: text, Posted: time.Now().UTC()}
		if kind == "reply" {
			s.Messages[replyTo].ReplyID = mail.ID
		}
		s.Messages[mail.ID] = mail
		return nil
	})
	if err == nil && kind != "info" {
		r.wake(to)
	}
	return mail, err
}

// UpdateTask changes assignment and dependencies under parent authority.
// A running owner must first be stopped; published candidates remain in history.
func (r *Runtime) UpdateTask(ctx context.Context, taskID string, revision int, owner string, deps []string) error {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.update(ctx, func(s *State) error {
		t := s.Tasks[taskID]
		if t == nil || t.Revision != revision || t.Status == "done" || t.Status == "canceled" {
			return fail("stale_task", "task is not editable at this revision")
		}
		if owner != "" && s.Members[owner] == nil {
			return errors.New("unknown task owner")
		}
		if t.Owner != "" {
			r.mu.Lock()
			busy := r.active[t.Owner] != nil
			r.mu.Unlock()
			if busy {
				return errors.New("stop the current owner before changing assignment")
			}
		}
		for _, dep := range deps {
			if s.Tasks[dep] == nil || s.Tasks[dep].Run != t.Run {
				return errors.New("dependency must belong to the same run")
			}
			seen := map[string]bool{}
			var reaches func(string) bool
			reaches = func(id string) bool {
				if id == taskID {
					return true
				}
				if seen[id] {
					return false
				}
				seen[id] = true
				for _, next := range s.Tasks[id].Dependencies {
					if reaches(next) {
						return true
					}
				}
				return false
			}
			if reaches(dep) {
				return errors.New("task dependency cycle")
			}
		}
		t.Owner, t.Dependencies, t.Status = owner, deps, "pending"
		t.Execution, t.Snapshot = "", ""
		t.Result, t.AcceptedRevision = nil, 0
		t.Revision++
		return nil
	})
}

// BlockTask records an owner's blocker. The parent explicitly unblocks it.
func (r *Runtime) BlockTask(ctx context.Context, actor, taskID string, revision int, reason string) error {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.update(ctx, func(s *State) error {
		t := s.Tasks[taskID]
		if t == nil || t.Revision != revision || t.Owner != actor || t.Status == "done" || t.Status == "canceled" {
			return fail("stale_task", "task owner or revision changed")
		}
		if strings.TrimSpace(reason) == "" {
			return errors.New("blocker reason is required")
		}
		t.Status, t.Feedback = "blocked", reason
		t.AcceptedRevision = 0
		t.Revision++
		return nil
	})
}
func inbox(s *State, to string, pending bool) []*Mail {
	out := []*Mail{}
	for _, m := range s.Messages {
		if m.To == to && (!pending || !m.Delivered) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Posted.Equal(out[j].Posted) {
			return out[i].ID < out[j].ID
		}
		return out[i].Posted.Before(out[j].Posted)
	})
	return out
}
func (r *Runtime) Publish(ctx context.Context, actor string, p Publication) (*Publication, error) {
	var result *Publication
	if err := r.prepare(ctx); err != nil {
		return nil, err
	}
	err := r.parent.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		s, err := decodeState(raw)
		if err != nil {
			return err
		}
		if err := member(s, r.ID, actor); err != nil {
			return err
		}
		if p.Supersedes != "" {
			old := s.Publications[p.Supersedes]
			if old == nil || old.Author != actor {
				return errors.New("only the author may supersede a publication")
			}
		}
		if p.Snapshot != "" && s.Snapshots[p.Snapshot] == nil {
			return errors.New("unknown snapshot")
		}
		p.ID = newID()
		p.Author = actor
		p.Run = r.currentRun(s).ID
		p.Posted = time.Now().UTC()
		s.Publications[p.ID] = &p
		result = &p
		raw.PinOwner = actor
		for _, ref := range p.Artifacts {
			raw.Pins = append(raw.Pins, ref.ID)
		}
		return encodeState(raw, s)
	})
	return result, err
}

// ContextPolicy denies live siblings and parent files. The common Git object
// store stays readable; filesystem isolation is not source-code secrecy.
func (r *Runtime) contextPolicy(s *State, c *ExecutionContext) (tools.ExecutionContext, error) {
	denied := append([]string(nil), r.config.PrivatePaths...)
	for _, other := range s.Contexts {
		if other.Root != c.Root {
			denied = append(denied, other.Root)
		}
	}
	if c.Checkout != nil {
		denied = append(denied, r.config.Root)
		for _, slot := range r.worktrees.Slots {
			if slot != filepath.Dir(c.Root) {
				denied = append(denied, slot)
			}
		}
	}
	writes := []string{}
	if c.Checkout != nil {
		writes = append(writes, r.worktrees.GitDir, c.Root+"/.git")
	}
	ec, err := r.config.Registry.ExecutionPolicy(c.Root, c.ReadOnly, denied, writes)
	ec.SourceRoot = r.config.Root
	ec.BuiltinTools = llm.BuiltinToolNames()
	if err == nil && c.Checkout != nil {
		ec.Sandbox.ReadPaths = append(ec.Sandbox.ReadPaths, r.worktrees.GitDir)
	}
	return ec, err
}
