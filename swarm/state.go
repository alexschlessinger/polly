// Package swarm provides durable coordination for one parent and its direct
// children. Workflow scripts and model tools share this runtime's scheduler.
package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/ids"
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
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Label      string        `json:"label"`
	Control    MemberControl `json:"control,omitempty"`
	Controller string        `json:"controller,omitempty"`
	Context    string        `json:"context,omitempty"`
	Tools      []string      `json:"tools"`
	Model      string        `json:"model"`
	Task       string        `json:"task,omitempty"`
	Execution  string        `json:"execution,omitempty"`
	ReadOnly   bool          `json:"readOnly"`
}
type Task struct {
	Requirement      string        `json:"requirement,omitempty"`
	Delivery         *TaskDelivery `json:"delivery,omitempty"`
	Follows          string        `json:"follows,omitempty"`
	SourceRoot       string        `json:"sourceRoot,omitempty"`
	Deferral         *TaskDeferral `json:"deferral,omitempty"`
	StartingSnapshot string        `json:"startingSnapshot,omitempty"`
	Execution        string        `json:"execution,omitempty"`
	ID               string        `json:"id"`
	Run              string        `json:"run"`
	Description      string        `json:"description"`
	Criteria         string        `json:"criteria"`
	Dependencies     []string      `json:"dependencies"`
	Owner            string        `json:"owner,omitempty"`
	Status           string        `json:"status"`
	Revision         int           `json:"revision"`
	AcceptedRevision int           `json:"acceptedRevision,omitempty"`
	Result           any           `json:"result,omitempty"`
	Feedback         string        `json:"feedback,omitempty"`
	Snapshot         string        `json:"snapshot,omitempty"`
}
type Mail struct {
	Task      string    `json:"task,omitempty"`
	Revision  int       `json:"revision,omitempty"`
	Execution string    `json:"execution,omitempty"`
	Workflow  string    `json:"workflow,omitempty"`
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
	Workspace  string `json:"workspace,omitempty"`
	Base       string `json:"base,omitempty"`
	SourceRoot string `json:"sourceRoot,omitempty"`
	// Workflow is assigned by the host, not inferred from display call IDs.
	Workflow   string                 `json:"workflow,omitempty"`
	InputSaved bool                   `json:"inputSaved"`
	Intent     []messages.ChatMessage `json:"intent,omitempty"`
	Usage      Usage                  `json:"usage"`
	ID         string                 `json:"id"`
	Run        string                 `json:"run"`
	Member     string                 `json:"member"`
	Status     string                 `json:"status"`
	Request    AgentRequest           `json:"request"`
	Iterations int                    `json:"iterations"`
	Generation int                    `json:"generation"`
	Result     *AgentResult           `json:"result,omitempty"`
	Error      string                 `json:"error,omitempty"`
	StopReason messages.StopReason    `json:"stopReason,omitempty"`

	// The final-answer retry belongs to the logical execution across restores.
	EmptyFinalRetried       bool                  `json:"emptyFinalRetried,omitempty"`
	ResultCorrections       int                   `json:"resultCorrections,omitempty"`
	PendingResultCorrection string                `json:"pendingResultCorrection,omitempty"`
	Completion              *StructuredCompletion `json:"completion,omitempty"`
}

// StructuredCompletion is committed with the successful receipt. A pointer
// distinguishes an accepted JSON null from an execution without a result.
type StructuredCompletion struct {
	Task   string `json:"task"`
	CallID string `json:"callID,omitempty"`
	Value  any    `json:"value"`
}
type ExecutionContext struct {
	Release  string             `json:"release,omitempty"`
	Reason   string             `json:"reason,omitempty"`
	ID       string             `json:"id"`
	Owner    string             `json:"owner"`
	Root     string             `json:"root"`
	ReadOnly bool               `json:"readOnly"`
	Checkout *worktree.Checkout `json:"checkout,omitempty"`
	// Scratch is the member's private writable directory: the slot's scratch
	// for checkouts, a runtime-directory entry for live trees. Empty on records
	// written before scratch directories existed; those keep denying all writes.
	Scratch string `json:"scratch,omitempty"`
}
type ParentTurn struct {
	Intent []messages.ChatMessage `json:"intent,omitempty"`
}

type State struct {
	Integrations map[string]*IntegrationCandidate `json:"integrations"`
	Applies      map[string]*ApplyRecord          `json:"applies"`
	ParentTurns  map[string]*ParentTurn           `json:"parentTurns"`
	Runs         map[string]*Run                  `json:"runs"`
	Members      map[string]*Member               `json:"members"`
	Tasks        map[string]*Task                 `json:"tasks"`
	Messages     map[string]*Mail                 `json:"messages"`
	Publications map[string]*Publication          `json:"publications"`
	Executions   map[string]*Execution            `json:"executions"`
	Contexts     map[string]*ExecutionContext     `json:"contexts"`
	Snapshots    map[string]*worktree.Snapshot    `json:"snapshots"`
	Previews     map[string]*worktree.Preview     `json:"previews"`
	Workflows    map[string]*workflow.Report      `json:"workflows"`
	// Format is nil until the root's first coordination mutation records it.
	Format *FormatRecord `json:"format,omitempty"`
}

func decodeState(raw *sessions.CoordinationState) (*State, error) {
	s := &State{}
	format, err := decodeFormat(raw)
	if err != nil {
		return nil, err
	}
	if swarmRecordsPresent(raw) && (format == nil || format.Version != swarmFormatVersion) {
		return nil, unsupportedFormat(raw.ParentID, format)
	}
	s.Format = format
	// Keep each domain in separately keyed records. All affected records and
	// transcript receipts commit in the same SQLite transaction.
	steps := map[string]*workflow.Step{}
	if err := errors.Join(
		decodeRecords(raw, "integration", &s.Integrations),
		decodeRecords(raw, "apply", &s.Applies),
		decodeRecords(raw, "parent_turn", &s.ParentTurns),
		decodeRecords(raw, "run", &s.Runs),
		decodeRecords(raw, "member", &s.Members),
		decodeRecords(raw, "task", &s.Tasks),
		decodeRecords(raw, "mail", &s.Messages),
		decodeRecords(raw, "publication", &s.Publications),
		decodeRecords(raw, "execution", &s.Executions),
		decodeRecords(raw, "context", &s.Contexts),
		decodeRecords(raw, "snapshot", &s.Snapshots),
		decodeRecords(raw, "preview", &s.Previews),
		decodeRecords(raw, "workflow", &s.Workflows),
		decodeRecords(raw, "workflow_step", &steps),
	); err != nil {
		return nil, err
	}
	if err := attachWorkflowSteps(s.Workflows, steps); err != nil {
		return nil, err
	}
	return s, nil
}

// decodeRecords unmarshals one kind's records straight into their typed map.
// Each record is its own JSON document, so nothing is re-encoded to read it.
// A kind without records leaves an empty map so callers can index it.
func decodeRecords[T any](raw *sessions.CoordinationState, kind string, target *map[string]T) error {
	records := raw.Records[kind]
	out := make(map[string]T, len(records))
	for id, data := range records {
		var value T
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("read swarm %s: %w", kind, err)
		}
		out[id] = value
	}
	*target = out
	return nil
}
func encodeRecords[T any](raw *sessions.CoordinationState, kind string, values map[string]T) error {
	group := make(map[string]json.RawMessage, len(values))
	for id, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		group[id] = data
	}
	raw.Records[kind] = group
	return nil
}

func encodeState(raw *sessions.CoordinationState, s *State) error {
	// A workflow's steps are records of their own, so each checkpoint of a
	// long run rewrites one step instead of the whole report.
	reports, steps := detachWorkflowSteps(s.Workflows)
	if err := errors.Join(
		encodeRecords(raw, "integration", s.Integrations),
		encodeRecords(raw, "apply", s.Applies),
		encodeRecords(raw, "parent_turn", s.ParentTurns),
		encodeRecords(raw, "run", s.Runs),
		encodeRecords(raw, "member", s.Members),
		encodeRecords(raw, "task", s.Tasks),
		encodeRecords(raw, "mail", s.Messages),
		encodeRecords(raw, "publication", s.Publications),
		encodeRecords(raw, "execution", s.Executions),
		encodeRecords(raw, "context", s.Contexts),
		encodeRecords(raw, "snapshot", s.Snapshots),
		encodeRecords(raw, "preview", s.Previews),
		encodeRecords(raw, "workflow", reports),
		encodeRecords(raw, "workflow_step", steps),
	); err != nil {
		return err
	}
	format := map[string]json.RawMessage{}
	if s.Format != nil {
		data, err := json.Marshal(s.Format)
		if err != nil {
			return err
		}
		format[formatID] = data
	}
	raw.Records[formatKind] = format
	return nil
}

// Step records are keyed "<report>/<index>". A report that still carries
// embedded steps predates the split and is unsupported.
func detachWorkflowSteps(reports map[string]*workflow.Report) (map[string]*workflow.Report, map[string]workflow.Step) {
	headers := make(map[string]*workflow.Report, len(reports))
	steps := map[string]workflow.Step{}
	for id, report := range reports {
		header := *report
		header.Steps = nil
		headers[id] = &header
		for index, step := range report.Steps {
			steps[fmt.Sprintf("%s/%d", id, index)] = step
		}
	}
	return headers, steps
}
func attachWorkflowSteps(reports map[string]*workflow.Report, steps map[string]*workflow.Step) error {
	indexed := map[string]map[int]workflow.Step{}
	for key, step := range steps {
		id, number, ok := strings.Cut(key, "/")
		index, err := strconv.Atoi(number)
		if !ok || err != nil || index < 0 || reports[id] == nil {
			return fmt.Errorf("read swarm workflow_step: unexpected record %q", key)
		}
		if indexed[id] == nil {
			indexed[id] = map[int]workflow.Step{}
		}
		indexed[id][index] = *step
	}
	for id, report := range reports {
		if len(report.Steps) > 0 {
			return fmt.Errorf("read swarm workflow %s: embedded steps: %w", id, ErrUnsupportedFormat)
		}
		report.Steps = make([]workflow.Step, len(indexed[id]))
		for index, step := range indexed[id] {
			if index >= len(report.Steps) {
				return fmt.Errorf("read swarm workflow_step: %s is missing a step before %d", id, index)
			}
			report.Steps[index] = step
		}
	}
	return nil
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
	run := &Run{ID: ids.New(), Status: "running", Limit: r.config.MaxExecutions}
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

// compactRoster is the roster a member's first prompt carries: counts, then
// the members working right now. Finished members are counted, not listed;
// list_agents and swarm_status show the rest on demand.
func compactRoster(s *State) string {
	working, idle, paused, retired := 0, 0, 0, 0
	var lines []string
	for _, id := range sortedInspectionIDs(s.Members) {
		m := s.Members[id]
		if m.Control == MemberControlRetired {
			retired++
			continue
		}
		p := MemberState(s, m)
		switch {
		case p.Busy:
			working++
			if len(lines) < 32 {
				lines = append(lines, fmt.Sprintf("%s · %s · %s · task %s", m.ID, m.Label, p.Display, m.Task))
			}
		case p.Lifecycle == LifecyclePaused:
			paused++
		default:
			idle++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Roster at assignment start (use list_agents to refresh): %d members: %d working, %d idle, %d paused", working+idle+paused, working, idle, paused)
	if retired > 0 {
		fmt.Fprintf(&b, "; %s omitted", countNoun(retired, "retired member"))
	}
	b.WriteString(".\n")
	if working == 0 {
		b.WriteString("Working now: none.\n")
		return b.String()
	}
	b.WriteString("Working now:\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	if working > len(lines) {
		fmt.Fprintf(&b, "%d additional working members available through list_agents.\n", working-len(lines))
	}
	return b.String()
}

// State is the display's read. Successive calls share one decode until a
// record changes, so treat the result as read-only.
func (r *Runtime) State(ctx context.Context) (*State, error) {
	raw, err := r.parent.ReadCoordination(ctx)
	if err != nil {
		return nil, err
	}
	return r.view.decode(raw)
}

// StateCache hands back the previous decode whenever a fresh read finds every
// record byte-identical. A display polling a swarm between checkpoints pays
// for the row scan, not for decoding megabytes of unchanged records.
type StateCache struct {
	mu    sync.Mutex
	raw   map[string]map[string]json.RawMessage
	state *State
}

func (c *StateCache) decode(raw *sessions.CoordinationState) (*State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != nil && sameRecords(c.raw, raw.Records) {
		return c.state, nil
	}
	s, err := decodeState(raw)
	if err != nil {
		return nil, err
	}
	c.raw, c.state = raw.Records, s
	return s, nil
}
func sameRecords(a, b map[string]map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for kind, group := range a {
		other, ok := b[kind]
		if !ok || len(other) != len(group) {
			return false
		}
		for id, value := range group {
			if previous, ok := other[id]; !ok || !bytes.Equal(previous, value) {
				return false
			}
		}
	}
	return true
}
func (r *Runtime) CreateTask(ctx context.Context, description, criteria string, deps []string, owner string, options ...CreateTaskOptions) (*Task, error) {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	if description == "" {
		return nil, errors.New("task description is required")
	}
	var task *Task
	var option CreateTaskOptions
	if len(options) > 1 {
		return nil, errors.New("only one task creation option is allowed")
	}
	if len(options) == 1 {
		option = options[0]
	}
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
		requirement, err := creationRequirement(option, s.Members[owner])
		if err != nil {
			return err
		}
		if criteria == "" {
			criteria = criteriaFor(requirement)
		}
		task = &Task{ID: ids.New(), Run: r.currentRun(s).ID, Requirement: requirement, Description: description, Criteria: criteria, Dependencies: deps, Owner: owner, Status: "pending", Revision: 1}
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
		m := s.Members[actor]
		if m == nil {
			return errors.New("only members can claim tasks")
		}
		if old := s.Tasks[m.Task]; old != nil && old.ID != t.ID && old.Status == "running" {
			return errors.New("submit or block the current assignment before claiming another")
		}
		return assignTask(s, t, m, s.Contexts[m.Context], m.Execution)
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
		if t == nil || revision <= 0 || t.Status != "awaiting_review" || t.Revision != revision {
			return fail("stale_task", "review must name the current submitted revision")
		}
		if err := reactivateTask(s, t); err != nil {
			return err
		}
		if accept {
			if owner := s.Members[t.Owner]; owner != nil && !owner.ReadOnly && t.Snapshot == "" {
				return errors.New("editing task has no integration candidate")
			}
			if err := acceptTask(s, t); err != nil {
				return err
			}
		} else {
			if strings.TrimSpace(feedback) == "" {
				return errors.New("changes requested requires feedback")
			}
			t.AcceptedRevision = 0
			t.Status = "changes_requested"
			t.Feedback = feedback
			t.Revision++
			mail := &Mail{ID: ids.New(), From: r.ID, To: t.Owner, Kind: "request", Text: "Changes requested for task " + t.ID + ": " + feedback, Posted: time.Now().UTC()}
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
		if err := reactivateTask(s, t); err != nil {
			return err
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
		mail = &Mail{ID: ids.New(), From: actor, To: to, Kind: kind, ReplyTo: replyTo, Text: text, Posted: time.Now().UTC()}
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
		if err := reactivateTask(s, t); err != nil {
			return err
		}
		if owner != "" && s.Members[owner] == nil {
			return errors.New("unknown task owner")
		}
		if m := s.Members[owner]; m != nil {
			if err := validateRequirement(t, m.ReadOnly); err != nil {
				return err
			}
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
		t.Execution, t.Snapshot, t.StartingSnapshot, t.SourceRoot = "", "", "", ""
		t.Delivery = nil
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
		if err := reactivateTask(s, t); err != nil {
			return err
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
		p.ID = ids.New()
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

// ContextPolicy denies sibling checkouts, every reserved scratch slot but the
// member's own, and parent files, and grants the member's own scratch. Slots
// are denied by name, existing or not, so a sibling started later is covered.
// The common Git object store stays readable; filesystem isolation is not
// source-code secrecy.
func (r *Runtime) contextPolicy(ctx context.Context, s *State, c *ExecutionContext) (tools.ExecutionContext, error) {
	if c == nil || c.Release != "" {
		return tools.ExecutionContext{}, fail("context_denied", "execution context is retiring")
	}
	var manager *worktree.Manager
	if c.Checkout != nil {
		var err error
		if manager, err = r.manager(ctx); err != nil {
			return tools.ExecutionContext{}, err
		}
	}
	dir, err := r.runtimeDirectory()
	if err != nil {
		return tools.ExecutionContext{}, err
	}
	denied := append([]string(nil), r.config.PrivatePaths...)
	for _, other := range s.Contexts {
		if other.Root != c.Root {
			denied = append(denied, other.Root)
		}
	}
	if c.Checkout != nil {
		// Every reserved slot is denied individually: sandboxes refuse writes
		// inside a denied tree even where reads are exempted, so the runtime
		// directory itself cannot be denied around the member's own checkout.
		// Live scratches all sit under one directory this member never needs.
		denied = append(denied, r.config.Root, filepath.Join(dir, "scratch"))
		for _, slot := range manager.Slots {
			if slot != filepath.Dir(c.Root) {
				denied = append(denied, slot)
			}
		}
	} else {
		// Checkout scratches sit inside the slots; a live member's own
		// scratch is one reserved live slot, so the others are denied by name.
		denied = append(denied, worktree.SlotPaths(dir, r.config.MaxWorktrees)...)
		for _, slot := range r.liveScratchSlots(dir) {
			if slot != c.Scratch {
				denied = append(denied, slot)
			}
		}
	}
	sort.Strings(denied)
	writes := []string{}
	if c.Checkout != nil {
		writes = append(writes, manager.GitDir, c.Root+"/.git")
	}
	ec, err := r.config.Registry.ExecutionPolicy(c.Root, tools.ExecutionGrant{ReadOnly: c.ReadOnly, DeniedReads: denied, DeniedWrites: writes, Scratch: c.Scratch})
	if err != nil {
		return ec, err
	}
	ec.SourceRoot = r.config.Root
	ec.BuiltinTools = llm.BuiltinToolNames()
	if c.Checkout != nil {
		ec.Sandbox.ReadPaths = append(ec.Sandbox.ReadPaths, manager.GitDir)
	}
	return ec, nil
}
