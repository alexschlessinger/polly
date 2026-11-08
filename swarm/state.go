// Package swarm provides durable coordination for one parent and its direct
// children. Workflow scripts and model tools share this runtime's scheduler.
package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
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
	AgentName  string        `json:"agentName,omitempty"`
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Label      string        `json:"label"`
	Control    MemberControl `json:"control,omitempty"`
	Controller string        `json:"controller,omitempty"`
	Context    string        `json:"context,omitempty"`
	Tools      []string      `json:"tools"`
	ModelHost  string        `json:"modelHost,omitempty"`
	Model      string        `json:"model"`
	Task       string        `json:"task,omitempty"`
	Execution  string        `json:"execution,omitempty"`
	ReadOnly   bool          `json:"readOnly"`
	// SandboxPresets retains restrictive floors and requests after workspace release.
	SandboxPresets []string `json:"sandboxPresets,omitempty"`
	// Publications is the admission high-water mark. Older promoted
	// confirmations carry their own exact receipts (see pendingPublications).
	Publications PublicationMark `json:"publications,omitzero"`
}

// PublicationMark is a position in posting order, the order inbox uses:
// posting time, then ID. The zero mark precedes every publication.
type PublicationMark struct {
	Posted time.Time `json:"posted"`
	ID     string    `json:"id,omitempty"`
}

func (m PublicationMark) IsZero() bool { return m.Posted.IsZero() && m.ID == "" }

// before reports whether p was posted after the mark.
func (m PublicationMark) before(p *Publication) bool {
	if m.Posted.Equal(p.Posted) {
		return m.ID < p.ID
	}
	return m.Posted.Before(p.Posted)
}

type Task struct {
	FollowupCallID   string        `json:"followupCallID,omitempty"`
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
	// Outputs are the files the completing execution left in its output
	// directory, captured before its workspace could be released.
	Outputs  []artifacts.Ref `json:"outputs,omitempty"`
	Feedback string          `json:"feedback,omitempty"`
	Snapshot string          `json:"snapshot,omitempty"`
}
type Mail struct {
	// Start authorizes execution independently of message delivery. Ordinary
	// mail, including review feedback, never carries this authority.
	Start           bool      `json:"start,omitempty"`
	CallID          string    `json:"callID,omitempty"`
	ResumeExecution string    `json:"resumeExecution,omitempty"`
	StartError      string    `json:"startError,omitempty"`
	Task            string    `json:"task,omitempty"`
	Revision        int       `json:"revision,omitempty"`
	Execution       string    `json:"execution,omitempty"`
	Workflow        string    `json:"workflow,omitempty"`
	ReplyID         string    `json:"replyID,omitempty"`
	ID              string    `json:"id"`
	From            string    `json:"from"`
	To              string    `json:"to"`
	Kind            string    `json:"kind"`
	ReplyTo         string    `json:"replyTo,omitempty"`
	Text            string    `json:"text"`
	Delivered       bool      `json:"delivered"`
	Posted          time.Time `json:"posted"`
}
type Publication struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Run    string `json:"run"`
	// Kind is "" for a finding, read by the members of its run, or host for a
	// fact about this machine, read by the parent and later runs as well.
	Kind        string          `json:"kind,omitempty"`
	Key         string          `json:"key,omitempty"`
	Value       string          `json:"value,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	DuplicateOf string          `json:"duplicateOf,omitempty"`
	Text        string          `json:"text"`
	Supersedes  string          `json:"supersedes,omitempty"`
	Snapshot    string          `json:"snapshot,omitempty"`
	Artifacts   []artifacts.Ref `json:"artifacts,omitempty"`
	Sources     []string        `json:"sources,omitempty"`
	Posted      time.Time       `json:"posted"`
	// AdmittedTo records actual delivery of a historical duplicate that later
	// represents a surviving claim. Its original timestamp cannot prove reads.
	AdmittedTo []string `json:"admittedTo,omitempty"`
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
	// Started and Ended bound the execution's wall time: Started is set when
	// it first runs, Ended when it reaches a terminal status and cleared
	// while it runs again.
	Started    time.Time           `json:"started,omitzero"`
	Ended      time.Time           `json:"ended,omitzero"`
	ResumedAt  time.Time           `json:"resumedAt,omitzero"`
	ResumedBy  string              `json:"resumedBy,omitempty"`
	Result     *AgentResult        `json:"result,omitempty"`
	Error      string              `json:"error,omitempty"`
	StopReason messages.StopReason `json:"stopReason,omitempty"`
	RetryPause *RetryPause         `json:"retryPause,omitempty"`

	// The final-answer retry belongs to the logical execution across restores.
	EmptyFinalRetried       bool                  `json:"emptyFinalRetried,omitempty"`
	ResultCorrections       int                   `json:"resultCorrections,omitempty"`
	PendingResultCorrection string                `json:"pendingResultCorrection,omitempty"`
	Completion              *StructuredCompletion `json:"completion,omitempty"`
}

func (e *Execution) noteResume(user bool) {
	e.ResumedAt, e.ResumedBy = time.Now().UTC(), "parent"
	if user {
		e.ResumedBy = "user"
	}
}

// StructuredCompletion is committed with the successful receipt. A pointer
// distinguishes an accepted JSON null from an execution without a result.
type StructuredCompletion struct {
	Task   string `json:"task"`
	CallID string `json:"callID,omitempty"`
	Value  any    `json:"value"`
}
type ExecutionContext struct {
	Release  string `json:"release,omitempty"`
	Reason   string `json:"reason,omitempty"`
	ID       string `json:"id"`
	Owner    string `json:"owner"`
	Root     string `json:"root"`
	ReadOnly bool   `json:"readOnly"`
	// SandboxPresets are intersected restrictions, not replacement policies.
	SandboxPresets []string `json:"sandboxPresets,omitempty"`
	// Disposable marks a workflow's own copy whose contents are never work:
	// it is declared when the copy is created, only exec and tool calls run
	// in it, it can be neither captured nor used to seed another copy, and
	// its release therefore needs no proof about what it holds. The only
	// other writer is the user's /swarm discard, recorded with the release
	// of a copy whose contents they chose to lose.
	Disposable bool               `json:"disposable,omitempty"`
	Checkout   *worktree.Checkout `json:"checkout,omitempty"`
	// Scratch is the member's private writable directory: the slot's scratch
	// for checkouts, a runtime-directory entry for live trees. Empty on records
	// written before scratch directories existed; those keep denying all writes.
	Scratch string `json:"scratch,omitempty"`
}
type ParentTurn struct {
	Intent     []messages.ChatMessage `json:"intent,omitempty"`
	RetryPause *RetryPause            `json:"retryPause,omitempty"`
}

// ParentRecord is what persists about the parent between its turns: the
// receipt of the last host fact it was shown. Members keep theirs on their
// own records.
type ParentRecord struct {
	Publications PublicationMark `json:"publications,omitzero"`
}

type State struct {
	Followups    map[string]*FollowupCall         `json:"followups,omitempty"`
	Integrations map[string]*IntegrationCandidate `json:"integrations"`
	Applies      map[string]*ApplyRecord          `json:"applies"`
	ParentTurns  map[string]*ParentTurn           `json:"parentTurns"`
	Parents      map[string]*ParentRecord         `json:"parents,omitempty"`
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
	return decodeStateContext(context.Background(), raw)
}

// Only callers with an observation-bearing context collect codec work. Count
// successful individual record conversions, including work before a later error.
func decodeStateContext(ctx context.Context, raw *sessions.CoordinationState) (*State, error) {
	defer runtimemetrics.Start(ctx, runtimemetrics.StateDecode).Stop()
	s := &State{}
	format, err := decodeFormat(raw)
	if err != nil {
		return nil, err
	}
	if format != nil {
		runtimemetrics.Add(ctx, runtimemetrics.RecordsDecoded, 1)
	}
	if swarmRecordsPresent(raw) && (format == nil || format.Version != swarmFormatVersion) {
		return nil, unsupportedFormat(raw.ParentID, format)
	}
	s.Format = format
	// Keep each domain in separately keyed records. All affected records and
	// transcript receipts commit in the same SQLite transaction.
	steps := map[string]*workflow.Step{}
	if err := errors.Join(
		decodeRecords(ctx, raw, "followup", &s.Followups),
		decodeRecords(ctx, raw, "integration", &s.Integrations),
		decodeRecords(ctx, raw, "apply", &s.Applies),
		decodeRecords(ctx, raw, "parent_turn", &s.ParentTurns),
		decodeRecords(ctx, raw, "parent", &s.Parents),
		decodeRecords(ctx, raw, "run", &s.Runs),
		decodeRecords(ctx, raw, "member", &s.Members),
		decodeRecords(ctx, raw, "task", &s.Tasks),
		decodeRecords(ctx, raw, "mail", &s.Messages),
		decodeRecords(ctx, raw, "publication", &s.Publications),
		decodeRecords(ctx, raw, "execution", &s.Executions),
		decodeRecords(ctx, raw, "context", &s.Contexts),
		decodeRecords(ctx, raw, "snapshot", &s.Snapshots),
		decodeRecords(ctx, raw, "preview", &s.Previews),
		decodeRecords(ctx, raw, "workflow", &s.Workflows),
		decodeRecords(ctx, raw, "workflow_step", &steps),
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
func decodeRecords[T any](ctx context.Context, raw *sessions.CoordinationState, kind string, target *map[string]T) error {
	records := raw.Records[kind]
	out := make(map[string]T, len(records))
	for id, data := range records {
		var value T
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("read swarm %s: %w", kind, err)
		}
		runtimemetrics.Add(ctx, runtimemetrics.RecordsDecoded, 1)
		out[id] = value
	}
	*target = out
	return nil
}
func encodeRecords[T any](ctx context.Context, raw *sessions.CoordinationState, kind string, values map[string]T) error {
	group := make(map[string]json.RawMessage, len(values))
	for id, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		runtimemetrics.Add(ctx, runtimemetrics.RecordsEncoded, 1)
		group[id] = data
	}
	raw.Records[kind] = group
	return nil
}

func encodeState(raw *sessions.CoordinationState, s *State) error {
	return encodeStateContext(context.Background(), raw, s)
}

func encodeStateContext(ctx context.Context, raw *sessions.CoordinationState, s *State) error {
	defer runtimemetrics.Start(ctx, runtimemetrics.StateEncode).Stop()
	// A workflow's steps are records of their own, so each checkpoint of a
	// long run rewrites one step instead of the whole report.
	reports, steps := detachWorkflowSteps(s.Workflows)
	if err := errors.Join(
		encodeRecords(ctx, raw, "followup", s.Followups),
		encodeRecords(ctx, raw, "integration", s.Integrations),
		encodeRecords(ctx, raw, "apply", s.Applies),
		encodeRecords(ctx, raw, "parent_turn", s.ParentTurns),
		encodeRecords(ctx, raw, "parent", s.Parents),
		encodeRecords(ctx, raw, "run", s.Runs),
		encodeRecords(ctx, raw, "member", s.Members),
		encodeRecords(ctx, raw, "task", s.Tasks),
		encodeRecords(ctx, raw, "mail", s.Messages),
		encodeRecords(ctx, raw, "publication", s.Publications),
		encodeRecords(ctx, raw, "execution", s.Executions),
		encodeRecords(ctx, raw, "context", s.Contexts),
		encodeRecords(ctx, raw, "snapshot", s.Snapshots),
		encodeRecords(ctx, raw, "preview", s.Previews),
		encodeRecords(ctx, raw, "workflow", reports),
		encodeRecords(ctx, raw, "workflow_step", steps),
	); err != nil {
		return err
	}
	format := map[string]json.RawMessage{}
	if s.Format != nil {
		data, err := json.Marshal(s.Format)
		if err != nil {
			return err
		}
		runtimemetrics.Add(ctx, runtimemetrics.RecordsEncoded, 1)
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
	return decodeStateContext(ctx, raw)
}
func (r *Runtime) update(ctx context.Context, fn func(*State) error) error {
	if err := r.prepare(ctx); err != nil {
		return err
	}
	return r.updateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		s, err := decodeStateContext(ctx, raw)
		if err != nil {
			return err
		}
		if err = fn(s); err != nil {
			return err
		}
		return encodeStateContext(ctx, raw, s)
	})
}

// Whole-state operations retain their existing transaction/authority. Native
// hosts can still return precise committed scopes; legacy hosts conservatively
// invalidate live subscriptions without treating a hint as a commit receipt.
func (r *Runtime) updateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	if source, ok := r.parent.(sessions.CoordinationCommitSession); ok {
		commit, err := source.UpdateCoordinationCommit(ctx, fn)
		if err == nil {
			r.committedRecords(commit)
		}
		return err
	}
	err := r.parent.UpdateCoordination(ctx, fn)
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
	return r.updateRecords(ctx, func(tx *recordTx) error {
		run, err := tx.Run(id)
		if err != nil {
			return err
		}
		if run != nil && run.Status == "running" {
			run.Status = "paused"
			return tx.PutRun(run)
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
// swarm_read shows agent history and status on demand.
func compactRoster(s *State) string {
	working, idle, paused := 0, 0, 0
	var lines []string
	for _, id := range sortedInspectionIDs(s.Members) {
		m := s.Members[id]
		p := MemberState(s, m)
		switch {
		case p.Busy:
			working++
			if len(lines) < 32 {
				lines = append(lines, fmt.Sprintf("%s · %s · %s · task %s", agentName(m), m.Label, p.Display, m.Task))
			}
		case p.Lifecycle == LifecyclePaused:
			paused++
		default:
			idle++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Roster at assignment start (use list_agents to refresh): %d members: %d working, %d idle, %d paused", working+idle+paused, working, idle, paused)
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

// State is the display's read, and parked members'. Successive calls share
// one decode until a record changes, so treat the result as read-only.
func (r *Runtime) State(ctx context.Context) (*State, error) {
	access, err := sessions.OpenCoordinationAccess(r.parent)
	if err != nil {
		return nil, err
	}
	if !access.Capabilities().DurableRevisions {
		raw, err := r.parent.ReadCoordination(ctx)
		if err != nil {
			return nil, err
		}
		return r.view.decodeContext(ctx, raw)
	}
	// This is still an explicit full snapshot on change. Only unchanged
	// requests are cheap; token and payloads always share one read transaction.
	r.view.mu.Lock()
	defer r.view.mu.Unlock()
	var state *State
	err = access.ReadRecords(ctx, func(reader sessions.CoordinationReader) error {
		revisions, err := reader.Revisions([]sessions.RevisionScope{{Index: sessions.CoordinationIndexRecords}})
		if err != nil {
			return err
		}
		if len(revisions) != 1 {
			return sessions.ErrCoordinationRequest
		}
		if r.view.state != nil && r.view.revision == revisions[0].Token {
			state = r.view.state
			return nil
		}
		identity, err := reader.Identity()
		if err != nil {
			return err
		}
		raw := &sessions.CoordinationState{ActorID: identity.ActorID, ParentID: identity.ParentID, Sequence: identity.Sequence, Records: map[string]map[string]json.RawMessage{}}
		q := sessions.RecordQuery{Limit: sessions.MaxCoordinationPageLimit}
		for {
			page, err := reader.Query(q)
			if err != nil {
				return err
			}
			for _, record := range page.Records {
				if raw.Records[record.Key.Kind] == nil {
					raw.Records[record.Key.Kind] = map[string]json.RawMessage{}
				}
				raw.Records[record.Key.Kind][record.Key.ID] = record.Payload
			}
			if page.Next == "" {
				break
			}
			q.After = page.Next
		}
		state, err = decodeStateContext(ctx, raw)
		if err != nil {
			return err
		}
		r.view.raw, r.view.state, r.view.revision = raw.Records, state, revisions[0].Token
		return nil
	})
	return state, err
}

// StateCache hands back the previous decode whenever a fresh read finds every
// record byte-identical. A display polling a swarm between checkpoints pays
// for the row scan, not for decoding megabytes of unchanged records.
type StateCache struct {
	mu       sync.Mutex
	raw      map[string]map[string]json.RawMessage
	state    *State
	revision string // durable native whole-family equality token
}

func (c *StateCache) decode(raw *sessions.CoordinationState) (*State, error) {
	return c.decodeContext(context.Background(), raw)
}

func (c *StateCache) decodeContext(ctx context.Context, raw *sessions.CoordinationState) (*State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != nil && sameRecords(c.raw, raw.Records) {
		return c.state, nil
	}
	s, err := decodeStateContext(ctx, raw)
	if err != nil {
		return nil, err
	}
	c.raw, c.state, c.revision = raw.Records, s, ""
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
func (r *recordReader) activeRuns() ([]*Run, error) {
	var runs []*Run
	for _, status := range []string{"running", "paused"} {
		page, err := r.Runs(sessions.RecordQuery{Index: sessions.CoordinationIndexRunStatus, Key: status, Limit: 2})
		if err != nil {
			return nil, err
		}
		for _, entry := range page.Records {
			runs = append(runs, entry.Value)
		}
	}
	return runs, nil
}

func (r *Runtime) currentRunRecords(tx *recordTx) (*Run, error) {
	runs, err := tx.activeRuns()
	if err != nil {
		return nil, err
	}
	if len(runs) != 0 {
		return runs[0], nil
	}
	run := &Run{ID: ids.New(), Status: "running", Limit: r.config.MaxExecutions}
	return run, tx.PutRun(run)
}

func (tx *recordTx) reactivateTask(t *Task) error {
	if t == nil || t.Deferral == nil {
		return nil
	}
	runs, err := tx.activeRuns()
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.ID != t.Run {
			return fail("session_busy", "deferred work belongs to an earlier run; wait for the current run to settle before recovering it")
		}
	}
	run, err := tx.Run(t.Run)
	if err != nil {
		return err
	}
	if run == nil {
		return errors.New("deferred task's original run is unavailable")
	}
	if run.Status == "completed" {
		run.Status = "running"
		if err := tx.PutRun(run); err != nil {
			return err
		}
	}
	t.Deferral = nil
	return nil
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
	err := r.updateRecords(ctx, func(tx *recordTx) error {
		run, err := r.currentRunRecords(tx)
		if err != nil {
			return err
		}
		for _, dep := range deps {
			var task *Task
			if dep != "" {
				task, err = tx.Task(dep)
			}
			if err != nil {
				return err
			}
			if task == nil || task.Run != run.ID {
				return fmt.Errorf("unknown dependency %s", dep)
			}
		}
		var member *Member
		if owner != "" {
			member, err = tx.Member(owner)
			if err != nil {
				return err
			}
			if member == nil {
				return errors.New("unknown task owner")
			}
		}
		requirement, err := creationRequirement(option, member)
		if err != nil {
			return err
		}
		if criteria == "" {
			criteria = criteriaFor(requirement)
		}
		task = &Task{ID: ids.New(), Run: run.ID, Requirement: requirement, Description: description, Criteria: criteria, Dependencies: deps, Owner: owner, Status: "pending", Revision: 1}
		return tx.PutTask(task)
	})
	if err != nil {
		return nil, err
	}
	return task, nil
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
	return r.updateRecords(ctx, func(tx *recordTx) error {
		if err := tx.requireMember(r.ID, actor); err != nil {
			return err
		}
		d, err := tx.TaskDependencies(taskID)
		if err != nil {
			return err
		}
		run, err := r.currentRunRecords(tx)
		if err != nil {
			return err
		}
		if d == nil || d.Task.Run != run.ID || d.Task.Revision != revision || d.Task.Owner != "" || d.Task.Status != "pending" || !d.dependenciesDone() {
			return fail("not_claimable", "task is no longer claimable")
		}
		m, err := tx.Member(actor)
		if err != nil {
			return err
		}
		if m == nil {
			return errors.New("only members can claim tasks")
		}
		if m.Task != "" {
			old, err := tx.TaskDependencies(m.Task)
			if err != nil {
				return err
			}
			if old != nil && old.Task.ID != d.Task.ID && old.Task.Status == "running" && !old.delivering() {
				return errors.New("submit or block the current assignment before claiming another")
			}
		}
		return tx.assignTask(d.Task, m, m.Execution)
	})
}
func (r *Runtime) Submit(ctx context.Context, actor, taskID string, revision int, result any, snapshot string) error {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.updateRecords(ctx, func(tx *recordTx) error {
		d, err := tx.TaskDependencies(taskID)
		if err != nil {
			return err
		}
		if d != nil && d.requirement() == RequirementDelivered {
			return fail("invalid_args", "task completes on delivery; end your turn with the result")
		}
		if d == nil || d.Task.Owner != actor || d.Task.Revision != revision || d.Task.Status == "canceled" || d.Task.Status == "done" {
			return fail("stale_task", "task owner or revision changed")
		}
		t := d.Task
		var capture *worktree.Snapshot
		if snapshot != "" {
			capture, err = tx.Snapshot(snapshot)
			if err != nil {
				return err
			}
			if capture == nil {
				return unavailableWorkspace()
			}
		}
		if owner := d.Owner; owner != nil {
			var c *ExecutionContext
			if owner.Context != "" {
				c, err = tx.Context(owner.Context)
			}
			if err != nil {
				return err
			}
			if !owner.ReadOnly && c != nil && c.Checkout != nil && snapshot == "" {
				return errors.New("editing results require an immutable candidate captured from the assigned editing workspace")
			}
			if snapshot != "" && (c == nil || capture.Source != c.Root) {
				return errors.New("submitted capture must come from its owner's execution context")
			}
		}
		t.Result, t.AcceptedRevision, t.Snapshot = result, 0, snapshot
		t.Status = "awaiting_review"
		t.Revision++
		return tx.PutTask(t)
	})
}
func (r *Runtime) Review(ctx context.Context, taskID string, revision int, accept bool, feedback string) error {
	defer r.scheduleRelease()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.updateRecords(ctx, func(tx *recordTx) error {
		d, err := tx.TaskDependencies(taskID)
		if err != nil {
			return err
		}
		if d != nil && d.requirement() == RequirementDelivered {
			return fail("invalid_args", "task "+taskID+" completes on delivery; ask a follow-up with followup_task")
		}
		if d == nil || revision <= 0 || d.Task.Status != "awaiting_review" || d.Task.Revision != revision {
			return fail("stale_task", "review must name the current submitted revision")
		}
		t := d.Task
		if accept && (d.requirement() == RequirementApplied || d.Owner != nil && !d.Owner.ReadOnly) {
			return fail("invalid_args", fmt.Sprintf("task %s is editing work; integrate it with %s or request changes with feedback", t.ID, integrateTasksAction([]TaskReference{{Task: t.ID, Revision: t.Revision}})))
		}
		if err := tx.reactivateTask(t); err != nil {
			return err
		}
		if accept {
			t.AcceptedRevision = t.Revision
			// Editing owners were rejected above; only research may finish here.
			if t.Requirement == RequirementReviewed || t.Snapshot == "" {
				t.Status = "done"
			}
		} else {
			if strings.TrimSpace(feedback) == "" {
				return errors.New("changes requested requires feedback")
			}
			t.AcceptedRevision, t.Status, t.Feedback = 0, "changes_requested", feedback
			t.Revision++
			mail := &Mail{ID: ids.New(), From: r.ID, To: t.Owner, Kind: "info", Task: t.ID, Revision: t.Revision, Execution: t.Execution, Text: "Changes requested for task " + t.ID + ": " + feedback, Posted: time.Now().UTC()}
			if err := tx.PutMail(mail); err != nil {
				return err
			}
		}
		return tx.PutTask(t)
	})
}
func (r *Runtime) CancelTask(ctx context.Context, taskID string) error {
	defer r.scheduleRelease()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.updateRecords(ctx, func(tx *recordTx) error {
		t, err := tx.Task(taskID)
		if err != nil {
			return err
		}
		if t == nil {
			return errors.New("unknown task")
		}
		if err := tx.reactivateTask(t); err != nil {
			return err
		}
		t.Status, t.AcceptedRevision = "canceled", 0
		t.Revision++
		if err := tx.PutTask(t); err != nil {
			return err
		}
		q := sessions.RecordQuery{Index: sessions.CoordinationIndexTaskDependency, Key: taskID, Limit: 64}
		for {
			page, err := tx.Tasks(q)
			if err != nil {
				return err
			}
			for _, entry := range page.Records {
				other := entry.Value
				for _, dep := range other.Dependencies {
					if dep == taskID && other.Status != "done" && other.Status != "canceled" {
						other.Status, other.Feedback = "blocked", "dependency canceled: "+taskID
						other.Revision++
					}
				}
				if err := tx.PutTask(other); err != nil {
					return err
				}
			}
			if page.Next == "" {
				return nil
			}
			q.After = page.Next
		}
	})
}
func (r *Runtime) Send(ctx context.Context, actor, to, kind, replyTo, text string) (*Mail, error) {
	var mail *Mail
	err := r.updateRecords(ctx, func(tx *recordTx) error {
		if err := tx.requireMember(r.ID, actor); err != nil {
			return err
		}
		if err := tx.requireMember(r.ID, to); err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return errors.New("message text is required")
		}
		if kind != "info" && kind != "request" && kind != "reply" {
			return errors.New("message kind must be info, request or reply")
		}
		var prior *Mail
		if kind == "reply" {
			var err error
			if replyTo != "" {
				prior, err = tx.Mail(replyTo)
			}
			if err != nil {
				return err
			}
			if prior == nil || prior.Kind != "request" || prior.From != to || prior.To != actor || prior.ReplyID != "" {
				return errors.New("reply does not match an addressed request")
			}
		}
		mail = &Mail{ID: ids.New(), From: actor, To: to, Kind: kind, ReplyTo: replyTo, Text: text, Posted: time.Now().UTC()}
		if prior != nil {
			prior.ReplyID = mail.ID
			if err := tx.PutMail(prior); err != nil {
				return err
			}
		}
		return tx.PutMail(mail)
	})
	if err != nil {
		return nil, err
	}
	return mail, nil
}

// UpdateTask changes assignment and dependencies under parent authority.
// A running owner must first be stopped; published candidates remain in history.
func (r *Runtime) UpdateTask(ctx context.Context, taskID string, revision int, owner string, deps []string) error {
	defer r.scheduleRelease()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.updateRecords(ctx, func(tx *recordTx) error {
		d, err := tx.TaskDependencies(taskID)
		if err != nil {
			return err
		}
		if d.delivering() {
			return fail("blocked", "its result awaits delivery; cancel it or wait")
		}
		if d == nil || d.Task.Revision != revision || d.Task.Status == "done" || d.Task.Status == "canceled" {
			return fail("stale_task", "task is not editable at this revision")
		}
		t := d.Task
		if err := tx.reactivateTask(t); err != nil {
			return err
		}
		if owner != "" {
			m, err := tx.Member(owner)
			if err != nil {
				return err
			}
			if m == nil {
				return errors.New("unknown task owner")
			}
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
		// The reachable dependency graph is a real cost; unrelated tasks are
		// never loaded. A missing referenced dependency cannot authorize a cycle.
		seen := map[string]bool{}
		var visit func(string) error
		visit = func(id string) error {
			if id == taskID {
				return errors.New("task dependency cycle")
			}
			if seen[id] {
				return nil
			}
			seen[id] = true
			var dep *Task
			var err error
			if id != "" {
				dep, err = tx.Task(id)
			}
			if err != nil {
				return err
			}
			if dep == nil || dep.Run != t.Run {
				return errors.New("dependency must belong to the same run")
			}
			for _, next := range dep.Dependencies {
				if err := visit(next); err != nil {
					return err
				}
			}
			return nil
		}
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		t.Owner, t.Dependencies, t.Status = owner, deps, "pending"
		t.Execution, t.Snapshot, t.StartingSnapshot, t.SourceRoot = "", "", "", ""
		t.Delivery = nil
		t.Result, t.AcceptedRevision = nil, 0
		t.Revision++
		return tx.PutTask(t)
	})
}

// BlockTask records an owner's blocker. A workflow cannot integrate or refresh
// its workers while awaiting them, so an explicit blocker interrupts that
// attempt and returns control to the parent. The parent explicitly unblocks it.
func (r *Runtime) BlockTask(ctx context.Context, actor, taskID string, revision int, reason string) error {
	r.parentTools.Lock()
	var controller, execution string
	var blocker *workflow.Error
	err := r.updateRecords(ctx, func(tx *recordTx) error {
		d, err := tx.TaskDependencies(taskID)
		if err != nil {
			return err
		}
		if d.delivering() {
			return fail("blocked", "result awaits delivery; use a follow-up for new work")
		}
		if d == nil || d.Task.Revision != revision || d.Task.Owner != actor || d.Task.Status == "done" || d.Task.Status == "canceled" {
			return fail("stale_task", "task owner or revision changed")
		}
		t := d.Task
		if err := tx.reactivateTask(t); err != nil {
			return err
		}
		if strings.TrimSpace(reason) == "" {
			return errors.New("blocker reason is required")
		}
		t.Status, t.Feedback = "blocked", reason
		t.AcceptedRevision = 0
		t.Revision++
		m, e := d.Owner, d.Execution
		if m != nil && e != nil && m.Task == t.ID && m.Execution == e.ID && e.Member == actor && m.Controller == e.Workflow && e.Workflow != "" && (e.Status == "running" || e.Status == "waiting" || e.Status == "queued") {
			w, err := tx.WorkflowHeader(e.Workflow)
			if err != nil {
				return err
			}
			if w != nil && w.Status == "running" {
				controller, execution = w.ID, e.ID
				blocker = &workflow.Error{
					Code: "workflow_blocked", Session: actor,
					Message: fmt.Sprintf("Worker %s blocked task %s: %s", agentName(m), t.ID, reason),
					Result:  map[string]any{"task": t.ID, "execution": e.ID, "revision": t.Revision, "reason": reason},
				}
			}
		}
		return tx.PutTask(t)
	})
	committed := err == nil
	if err != nil && blocker != nil {
		// An update error can lose a successful commit's reply. Resolve only
		// this blocker, under the same task-edit exclusion, without replaying
		// the write or letting caller cancellation hide the durable receipt.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		readErr := r.readRecords(readCtx, func(reader *recordReader) error {
			d, err := reader.TaskDependencies(taskID)
			if err != nil || d == nil {
				return err
			}
			w, err := reader.WorkflowHeader(controller)
			if err != nil {
				return err
			}
			t, m, e := d.Task, d.Owner, d.Execution
			committed = t.Owner == actor && t.Execution == execution && t.Revision == revision+1 && t.Status == "blocked" && t.Feedback == reason && t.AcceptedRevision == 0 &&
				m != nil && m.Task == taskID && m.Execution == execution && m.Controller == controller &&
				e != nil && e.ID == execution && e.Member == actor && e.Workflow == controller && w != nil && w.Status == "running"
			return nil
		})
		cancel()
		if readErr != nil {
			committed = false
		}
		// A positively inspected receipt is a durable hint, never success for
		// the original uncertain API call. Do not replay its callback.
		if committed {
			r.changed()
		}
		// Preserve the original write error even when its effect is confirmed.
	}
	r.parentTools.Unlock()
	if committed && blocker != nil {
		// Commit the blocker before signaling, and never join workers from
		// their own tool call. Cancellation's first cause wins across races.
		r.mu.Lock()
		cancel := r.workflowCancels[controller]
		r.mu.Unlock()
		if cancel != nil {
			cancel(blocker)
		}
	}
	return err
}
func inbox(s *State, to string, pending bool) []*Mail {
	out := []*Mail{}
	for _, m := range s.Messages {
		if m.To == to && (!pending || !m.Delivered) {
			if f := s.Followups[m.ID]; pending && f != nil && f.Refresh && f.Phase != "launched" {
				continue // preparation is not an assignment or an input intent
			}
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
	err := r.updateRecords(ctx, func(tx *recordTx) error {
		if err := tx.requireMember(r.ID, actor); err != nil {
			return err
		}
		switch p.Kind {
		case "", PublicationKindFinding, PublicationKindHost:
		default:
			return fail("invalid_args", "kind must be finding or host")
		}
		if p.Supersedes != "" {
			old, err := tx.Publication(p.Supersedes)
			if err != nil {
				return err
			}
			if old == nil || old.Author != actor {
				return errors.New("only the author may supersede a publication")
			}
			// A correction keeps its predecessor's readers unless it says otherwise.
			if p.Kind == "" {
				p.Kind = old.Kind
			}
			if p.Scope == "" {
				p.Scope = old.Scope
			}
			if p.Key == "" {
				p.Key = old.Key
			}
			if p.Snapshot == "" && publicationScope(old) == PublicationScopeSnapshot {
				p.Snapshot = old.Snapshot
			}
		}
		if p.Kind == PublicationKindFinding {
			p.Kind = ""
		}
		run, err := r.currentRunRecords(tx)
		if err != nil {
			return err
		}
		p.ID, p.Author, p.Run = ids.New(), actor, run.ID
		m, err := tx.Member(actor)
		if err != nil {
			return err
		}
		var execution *Execution
		if m != nil && m.Execution != "" {
			execution, err = tx.Execution(m.Execution)
			if err != nil {
				return err
			}
			if execution != nil && execution.Run != "" {
				p.Run = execution.Run
			}
		}
		if err := validatePublicationScope(&p); err != nil {
			return err
		}
		if publicationScope(&p) == PublicationScopeSnapshot && p.Snapshot == "" {
			if execution != nil {
				p.Snapshot = execution.Base
			}
			if p.Snapshot == "" {
				return fail("invalid_args", "snapshot scope requires a captured baseline; supply commit or use workspace scope for a live workspace observation")
			}
		}
		if p.Snapshot != "" {
			snapshot, err := tx.Snapshot(p.Snapshot)
			if err != nil {
				return err
			}
			if snapshot == nil || publicationScope(&p) == PublicationScopeSnapshot && !validTaskSnapshot(snapshot, p.Snapshot) {
				return unavailableWorkspace()
			}
		}
		p.Posted = time.Now().UTC()
		p.DuplicateOf = ""
		p.AdmittedTo = nil
		if p.Key != "" && p.Supersedes == "" {
			claim, err := tx.currentClaim(&p)
			if err != nil {
				return err
			}
			if len(claim) > 0 {
				p.DuplicateOf = claim[0].ID
			}
		}
		result = &p
		if err := tx.PutPublication(&p); err != nil {
			return err
		}
		effects := sessions.CoordinationEffects{PinOwner: actor}
		for _, ref := range p.Artifacts {
			effects.Pins = append(effects.Pins, ref.ID)
		}
		return tx.Effects(effects)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// contextScope is the workspace and authority a context's tools are opened
// for. It hides the source checkout, the runtime directory and every other
// context's root as private roots; the tool implementation re-grants only
// the member's own root and scratch inside them, so siblings are invisible
// structurally rather than by name. The common Git object store and the
// user's Git configuration stay readable; filesystem isolation is not
// source-code secrecy.
func (r *Runtime) contextScope(ctx context.Context, s *State, c *ExecutionContext) (tools.ToolScope, error) {
	roots := make([]string, 0, len(s.Contexts))
	for _, other := range s.Contexts {
		roots = append(roots, other.Root)
	}
	return r.contextScopeRoots(ctx, c, roots)
}

// roots is the complete security dependency set, never a partial State.
func (r *Runtime) contextScopeRoots(ctx context.Context, c *ExecutionContext, roots []string) (tools.ToolScope, error) {
	if c == nil || c.Release != "" {
		return tools.ToolScope{}, fail("context_denied", "execution context is releasing")
	}
	var manager *worktree.Manager
	if c.Checkout != nil {
		var err error
		if manager, err = r.manager(ctx); err != nil {
			return tools.ToolScope{}, err
		}
	}
	dir, err := r.runtimeDirectory()
	if err != nil {
		return tools.ToolScope{}, err
	}
	denied := append([]string(nil), r.config.PrivatePaths...)
	// The runtime directory holds every slot and manifest. It is a no-op under
	// the private home directory and hides them all when the runtime directory
	// lives elsewhere; the member's own root is granted back inside it.
	denied = append(denied, dir)
	for _, root := range roots {
		if root != c.Root {
			denied = append(denied, root)
		}
	}
	if c.Checkout != nil {
		denied = append(denied, r.config.Root)
	}
	sort.Strings(denied)
	writes := []string{}
	if c.Checkout != nil {
		writes = append(writes, manager.GitDir, c.Root+"/.git")
	}
	scope := tools.ToolScope{
		Root:  c.Root,
		Grant: tools.ExecutionGrant{ReadOnly: c.ReadOnly, DeniedReads: denied, DeniedWrites: writes, Scratch: c.Scratch, SourceRoot: r.config.Root, SandboxPresets: sandboxPresets(c.SandboxPresets)},
	}
	if c.Checkout != nil {
		base, _, err := r.config.Registry.BaseSandboxPolicy()
		if err != nil {
			return tools.ToolScope{}, err
		}
		grants, err := checkoutReadGrants(base, r.config.PrivatePaths, manager.GitDir, manager.UserConfigPaths())
		if err != nil {
			return tools.ToolScope{}, err
		}
		scope.ReadPaths = grants
	}
	return scope, nil
}

// checkoutReadGrants lists what a checkout member reads beyond the policy's
// inheritance: the repository's shared Git directory, inside the denied
// source root by design, and the user's Git configuration. They are added
// after ExecutionPolicy filtered the inherited grants, so they are checked
// here against the operator's explicit denials (the base policy's denied
// paths and the swarm's private paths), which win over any grant: a masked
// configuration file is dropped and a masked Git directory refuses the
// member, since a checkout cannot work without it.
func checkoutReadGrants(base sandbox.Config, privatePaths []string, gitDir string, configPaths []string) ([]string, error) {
	masks := sandbox.Config{DenyPaths: append(append([]string(nil), base.DenyPaths...), privatePaths...)}
	if err := sandbox.ReadMasked(masks, gitDir); err != nil {
		return nil, fmt.Errorf("checkout member cannot read the repository's Git directory: %w", err)
	}
	grants := []string{gitDir}
	for _, path := range configPaths {
		if sandbox.ReadMasked(masks, path) == nil {
			grants = append(grants, path)
		}
	}
	return grants, nil
}
