// Package swarm provides durable coordination for one parent and its direct
// children. Workflow scripts and model tools share this runtime's scheduler.
package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/ids"
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

// Execution is a member's run of one assignment. Intent is the tool batch its
// slice last journaled: the generated messages the transcript did not yet
// hold, to be appended at transcript sequence IntentAt. A transcript that has
// moved past IntentAt saved the batch with its results, so the intent is
// spent; a record written before intents were stamped has no IntentAt. A
// batch whose tools all only read is not journaled.
// Iterations and Usage count the model calls that have been answered. They
// trail by at most the call whose tool batch is running, and a request in
// flight is not yet counted.
//
// What the record says of the moment is volatile: that its member is queued
// or running, and the count of a call whose tools only read. The runtime's
// own readers see those at once, and the store writes them with the record's
// next change. A stored record therefore says what the member was doing when
// it last saved something else, and counts the calls up to then; the member's
// transcript is saved with the counts at every boundary, and Uncounted marks
// a record a restart found live, whose next slice takes them from there.
type Execution struct {
	Workspace  string `json:"workspace,omitempty"`
	Base       string `json:"base,omitempty"`
	SourceRoot string `json:"sourceRoot,omitempty"`
	// Workflow is assigned by the host, not inferred from display call IDs.
	Workflow   string                 `json:"workflow,omitempty"`
	InputSaved bool                   `json:"inputSaved"`
	Intent     []messages.ChatMessage `json:"intent,omitempty"`
	IntentAt   int64                  `json:"intentAt,omitempty"`
	Uncounted  bool                   `json:"uncounted,omitempty"`
	Usage      Usage                  `json:"usage"`
	ID         string                 `json:"id"`
	Run        string                 `json:"run"`
	Member     string                 `json:"member"`
	Status     string                 `json:"status"`
	Request    AgentRequest           `json:"request"`
	Iterations int                    `json:"iterations"`
	Generation int                    `json:"generation"`
	ResumedAt  time.Time              `json:"resumedAt,omitzero"`
	ResumedBy  string                 `json:"resumedBy,omitempty"`
	Result     *AgentResult           `json:"result,omitempty"`
	Error      string                 `json:"error,omitempty"`
	StopReason messages.StopReason    `json:"stopReason,omitempty"`
	RetryPause *RetryPause            `json:"retryPause,omitempty"`

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

	// editing is set on the state an edit hands its callback.
	editing *editBase
	// mailTo names each member's mail, on the state a Runtime's edits keep
	// current; any other state finds mail by looking at all of it.
	mailTo mailIndex
	// rootUse counts the contexts on each root, kept the same way.
	rootUse map[string]int
	// mailOn names the mail about each task, kept the same way.
	mailOn mailIndex
	// lazy is set on a state a reader keeps, which never changes: it builds
	// mailOn for itself the first time it is asked.
	lazy *lazyIndex
}

type lazyIndex struct {
	once   sync.Once
	mailOn mailIndex
}

// mailIndex names mail IDs by one of a mail's fields: its recipient or its task.
type mailIndex map[string]map[string]struct{}

func (x mailIndex) add(key, id string) {
	if x[key] == nil {
		x[key] = map[string]struct{}{}
	}
	x[key][id] = struct{}{}
}

func (x mailIndex) remove(key, id string) {
	if delete(x[key], id); len(x[key]) == 0 {
		delete(x, key)
	}
}

func indexMail(s *State) mailIndex {
	index := mailIndex{}
	for id, mail := range s.Messages {
		index.add(mail.To, id)
	}
	return index
}

func indexTaskMail(s *State) mailIndex {
	index := mailIndex{}
	for id, mail := range s.Messages {
		if mail.Task != "" {
			index.add(mail.Task, id)
		}
	}
	return index
}

func indexRoots(s *State) map[string]int {
	index := map[string]int{}
	for _, c := range s.Contexts {
		index[c.Root]++
	}
	return index
}

// index builds the indexes a Runtime's edits then keep current.
func (s *State) index() {
	s.mailTo, s.rootUse, s.mailOn = indexMail(s), indexRoots(s), indexTaskMail(s)
}

// mailFor returns the mail addressed to a member, in no order.
func (s *State) mailFor(to string) []*Mail { return s.mailWhere(s.mailTo, to, false) }

// mailAbout returns the mail that names a task, in no order.
func (s *State) mailAbout(task string) []*Mail {
	index := s.mailOn
	if index == nil && s.lazy != nil {
		s.lazy.once.Do(func() { s.lazy.mailOn = indexTaskMail(s) })
		index = s.lazy.mailOn
	}
	return s.mailWhere(index, task, true)
}

// mailWhere returns the mail whose task, or else recipient, is want. With no
// index it looks at all mail. An index is as the edit in progress found it,
// so the mail that edit touched is looked at directly.
func (s *State) mailWhere(index mailIndex, want string, byTask bool) []*Mail {
	matches := func(mail *Mail) bool {
		if byTask {
			return mail.Task == want
		}
		return mail.To == want
	}
	var out []*Mail
	if index == nil {
		for _, mail := range s.Messages {
			if matches(mail) {
				out = append(out, mail)
			}
		}
		return out
	}
	var touched map[string]any
	if s.editing != nil {
		touched = s.editing.prior["mail"]
	}
	for id := range index[want] {
		if _, ok := touched[id]; ok {
			continue
		}
		if mail := s.Messages[id]; mail != nil {
			out = append(out, mail)
		}
	}
	for id := range touched {
		if mail := s.Messages[id]; mail != nil && matches(mail) {
			out = append(out, mail)
		}
	}
	return out
}

// reindex brings the indexes up to date with the records an edit touched.
func (s *State) reindex() {
	if s.mailTo == nil || s.editing == nil {
		return
	}
	for id, was := range s.editing.prior["context"] {
		if old, _ := was.(*ExecutionContext); old != nil {
			if s.rootUse[old.Root]--; s.rootUse[old.Root] <= 0 {
				delete(s.rootUse, old.Root)
			}
		}
		if c := s.Contexts[id]; c != nil {
			s.rootUse[c.Root]++
		}
	}
	for id, was := range s.editing.prior["mail"] {
		if old, _ := was.(*Mail); old != nil {
			s.mailTo.remove(old.To, id)
			s.mailOn.remove(old.Task, id)
		}
		if mail := s.Messages[id]; mail != nil {
			s.mailTo.add(mail.To, id)
			if mail.Task != "" {
				s.mailOn.add(mail.Task, id)
			}
		}
	}
}

func decodeState(raw *sessions.CoordinationState) (*State, error) {
	return decodeStateFrom(raw, nil, &State{})
}

// decodeStateFrom decodes raw, taking from previous, the decode of was, every
// record whose bytes did not change. The result shares those records with
// previous, so both are read-only. Workflow reports are always decoded: a
// report is assembled from its own record and one per step.
func decodeStateFrom(raw *sessions.CoordinationState, was map[string]map[string]json.RawMessage, previous *State) (*State, error) {
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
		decodeRecords(raw, "followup", &s.Followups, was, previous.Followups),
		decodeRecords(raw, "integration", &s.Integrations, was, previous.Integrations),
		decodeRecords(raw, "apply", &s.Applies, was, previous.Applies),
		decodeRecords(raw, "parent_turn", &s.ParentTurns, was, previous.ParentTurns),
		decodeRecords(raw, "parent", &s.Parents, was, previous.Parents),
		decodeRecords(raw, "run", &s.Runs, was, previous.Runs),
		decodeRecords(raw, "member", &s.Members, was, previous.Members),
		decodeRecords(raw, "task", &s.Tasks, was, previous.Tasks),
		decodeRecords(raw, "mail", &s.Messages, was, previous.Messages),
		decodeRecords(raw, "publication", &s.Publications, was, previous.Publications),
		decodeRecords(raw, "execution", &s.Executions, was, previous.Executions),
		decodeRecords(raw, "context", &s.Contexts, was, previous.Contexts),
		decodeRecords(raw, "snapshot", &s.Snapshots, was, previous.Snapshots),
		decodeRecords(raw, "preview", &s.Previews, was, previous.Previews),
		decodeRecords(raw, "workflow", &s.Workflows, nil, nil),
		decodeRecords(raw, "workflow_step", &steps, nil, nil),
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
// A record whose bytes match was is taken from previous instead. A kind
// without records leaves an empty map so callers can index it.
func decodeRecords[T any](raw *sessions.CoordinationState, kind string, target *map[string]T, was map[string]map[string]json.RawMessage, previous map[string]T) error {
	records := raw.Records[kind]
	out := make(map[string]T, len(records))
	for id, data := range records {
		if old, ok := was[kind][id]; ok && bytes.Equal(old, data) {
			if value, ok := previous[id]; ok {
				out[id] = value
				continue
			}
		}
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
		encodeRecords(raw, "followup", s.Followups),
		encodeRecords(raw, "integration", s.Integrations),
		encodeRecords(raw, "apply", s.Applies),
		encodeRecords(raw, "parent_turn", s.ParentTurns),
		encodeRecords(raw, "parent", s.Parents),
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
			steps[workflowStepKey(id, index)] = step
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

// read returns the family's state for a caller that only looks at it. The
// result is shared with every other reader: change records inside an edit.
func (r *Runtime) read(ctx context.Context) (*State, error) { return r.State(ctx) }

// readScratch returns a decode of the caller's own, for a check that works by
// applying a change it then throws away.
func (r *Runtime) readScratch(ctx context.Context) (*State, error) {
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
	err := rewrite(ctx, r.parent, func(_ *sessions.CoordinationState, s *State) error { return fn(s) })
	if err == nil {
		r.pokeAll()
		r.changed()
	}
	return err
}

// rewrite runs fn on a decode of the whole family inside one transaction of
// session's and writes back whatever differs.
func rewrite(ctx context.Context, session sessions.CoordinationSession, fn func(raw *sessions.CoordinationState, s *State) error) error {
	return session.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		s, err := decodeState(raw)
		if err != nil {
			return err
		}
		if err = fn(raw, s); err != nil {
			return err
		}
		return encodeState(raw, s)
	})
}
func (r *Runtime) currentRun(s *State) *Run {
	for _, run := range s.Runs {
		if run.Status == "running" || run.Status == "paused" {
			return run
		}
	}
	run := &Run{ID: ids.New(), Status: "running", Limit: r.config.MaxExecutions}
	put(s, "run", &s.Runs, run.ID, run)
	return run
}

func (r *Runtime) pauseBudget(ctx context.Context, id string) error {
	return r.edit(ctx, func(s *State) error {
		if run := s.Runs[id]; run != nil && run.Status == "running" {
			s.ownRun(id).Status = "paused"
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
	// Every member is counted from its own record and execution; only the
	// listed ones, the first by id, get the full presentation.
	const listed = 32
	working, idle, paused := 0, 0, 0
	var first []string
	for id, m := range s.Members {
		switch lifecycle, _ := memberLifecycle(m, s.Executions[m.Execution]); lifecycle {
		case LifecycleActive, LifecycleWaiting:
			working++
			if at, _ := slices.BinarySearch(first, id); at < listed {
				if first = slices.Insert(first, at, id); len(first) > listed {
					first = first[:listed]
				}
			}
		case LifecyclePaused:
			paused++
		default:
			idle++
		}
	}
	var lines []string
	for _, id := range first {
		m := s.Members[id]
		lines = append(lines, fmt.Sprintf("%s · %s · %s · task %s", agentName(m), m.Label, MemberState(s, m).Display, m.Task))
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

// State is the family's state for a caller that keeps it, such as a display.
// Successive calls share one state until a record changes, and a changed
// family shares every record that did not, so treat the result as read-only.
func (r *Runtime) State(ctx context.Context) (*State, error) {
	r.editing.RLock()
	defer r.editing.RUnlock()
	s, err := r.current(ctx)
	if err != nil {
		return nil, err
	}
	return r.view.lend(s), nil
}

// look runs fn on the family's state while no edit can change it. It costs
// later edits nothing, where State makes the next one copy the maps it
// writes. fn must not keep s or a map of it, and must not read or edit the
// family again; the records it finds are its to keep, read-only.
func (r *Runtime) look(ctx context.Context, fn func(s *State)) error {
	r.editing.RLock()
	defer r.editing.RUnlock()
	s, err := r.current(ctx)
	if err != nil {
		return err
	}
	fn(s)
	return nil
}

// current returns the family's own state, which an edit changes in place: the
// caller holds r.editing, and lends the state before letting go of it.
func (r *Runtime) current(ctx context.Context) (*State, error) {
	parent := r.parent
	if revisions, ok := parent.(sessions.CoordinationRevisionSession); ok {
		revision, err := revisions.CoordinationRevision(ctx)
		if err != nil {
			return nil, err
		}
		if s := r.view.at(revision); s != nil {
			return s, nil
		}
	}
	raw, err := parent.ReadCoordination(ctx)
	if err != nil {
		return nil, err
	}
	return r.view.decode(raw)
}

// lookThrough is look for a caller holding any session of the family: the
// session's lease is checked, and the family's state answers when the store
// can say so without handing over the records. raw carries the session's
// identity and no Records then.
func (r *Runtime) lookThrough(ctx context.Context, session sessions.CoordinationSession, fn func(s *State, raw *sessions.CoordinationState)) error {
	revisions, ok := session.(sessions.CoordinationRevisionSession)
	if _, shared := r.parent.(sessions.CoordinationRevisionSession); ok && shared {
		if _, err := revisions.CoordinationRevision(ctx); err != nil {
			return err
		}
		raw := &sessions.CoordinationState{ActorID: session.ViewID(), ParentID: r.ID}
		return r.look(ctx, func(s *State) { fn(s, raw) })
	}
	raw, err := session.ReadCoordination(ctx)
	if err != nil {
		return err
	}
	s, err := decodeState(raw)
	if err != nil {
		return err
	}
	fn(s, raw)
	return nil
}

// StateCache hands back the previous decode whenever a fresh read finds every
// record unchanged, and decodes only the records that did change. A display
// polling a swarm between checkpoints pays for the comparison, not for
// decoding megabytes of unchanged records.
type StateCache struct {
	mu       sync.Mutex
	raw      map[string]map[string]json.RawMessage
	revision uint64
	// state is the family's own: a Runtime's edits change its maps in place.
	// lent is the state readers were handed, which shares those maps, and
	// cloned names the kinds whose map has been copied since, so that no
	// reader holds it.
	state  *State
	lent   *State
	cloned map[string]bool
}

// at returns the cached state when it was decoded from the records the store
// calls revision.
func (c *StateCache) at(revision uint64) *State {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil || revision == 0 || revision != c.revision {
		return nil
	}
	c.check("")
	return c.state
}

func (c *StateCache) decode(raw *sessions.CoordinationState) (*State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != nil && (raw.Revision != 0 && raw.Revision == c.revision || sameRecords(c.raw, raw.Records)) {
		c.check("")
		c.revision = raw.Revision
		return c.state, nil
	}
	previous := c.state
	if previous == nil {
		previous = &State{}
	} else {
		c.check("")
	}
	s, err := decodeStateFrom(raw, c.raw, previous)
	if err != nil {
		return nil, err
	}
	s.index()
	c.raw, c.revision, c.state, c.lent, c.cloned = raw.Records, raw.Revision, s, nil, nil
	return s, nil
}

// failOnSharedChange makes a test confirm the shared state on every read of
// it and after every edit.
var failOnSharedChange = testing.Testing()

// check confirms, in a test, that the shared state is what its records
// decode to: nothing changed a record but an edit through own, put or drop.
// site names the edit that just ran, empty for a read. The caller holds c.mu.
func (c *StateCache) check(site string) {
	if !failOnSharedChange {
		return
	}
	fresh, err := decodeState(&sessions.CoordinationState{Records: c.raw})
	if err == nil {
		fresh.index()
	}
	if err != nil || reflect.DeepEqual(fresh, c.state) {
		return
	}
	if site != "" {
		panic("swarm: the edit at " + site + " changed records without own, put or drop: " + differingRecords(fresh, c.state))
	}
	panic("swarm: a State returned by a read was modified; change records inside an edit: " + differingRecords(fresh, c.state))
}

// differingRecords names the records two states disagree on, for check.
func differingRecords(a, b *State) string {
	var out []string
	left, right := reflect.ValueOf(a).Elem(), reflect.ValueOf(b).Elem()
	for i := range left.NumField() {
		if left.Field(i).Kind() != reflect.Map || !left.Type().Field(i).IsExported() {
			continue
		}
		name := left.Type().Field(i).Name
		for _, key := range left.Field(i).MapKeys() {
			other := right.Field(i).MapIndex(key)
			if !other.IsValid() || !reflect.DeepEqual(left.Field(i).MapIndex(key).Interface(), other.Interface()) {
				out = append(out, name+"/"+key.String())
			}
		}
		for _, key := range right.Field(i).MapKeys() {
			if !left.Field(i).MapIndex(key).IsValid() {
				out = append(out, name+"/"+key.String())
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "none; the index of mail by recipient is out of date"
	}
	return strings.Join(out, ", ")
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
	err := r.edit(ctx, func(s *State) error {
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
		task = &Task{ID: ids.New(), Run: run.ID, Requirement: requirement, Description: description, Criteria: criteria, Dependencies: deps, Owner: owner, Status: "pending", Revision: 1}
		put(s, "task", &s.Tasks, task.ID, task)
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
	return r.edit(ctx, func(s *State) error {
		if err := member(s, r.ID, actor); err != nil {
			return err
		}
		t := s.ownTask(taskID)
		if t == nil || t.Run != r.currentRun(s).ID || t.Revision != revision || t.Owner != "" || t.Status != "pending" || !depsDone(s, t) {
			return fail("not_claimable", "task is no longer claimable")
		}
		m := s.ownMember(actor)
		if m == nil {
			return errors.New("only members can claim tasks")
		}
		if old := s.Tasks[m.Task]; old != nil && old.ID != t.ID && old.Status == "running" && !deliveringTask(s, old) {
			return errors.New("submit or block the current assignment before claiming another")
		}
		return assignTask(s, t, m, s.Contexts[m.Context], m.Execution)
	})
}
func (r *Runtime) Submit(ctx context.Context, actor, taskID string, revision int, result any, snapshot string) error {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.edit(ctx, func(s *State) error {
		t := s.ownTask(taskID)
		if t != nil && requirementOf(s, t) == RequirementDelivered {
			return fail("invalid_args", "task completes on delivery; end your turn with the result")
		}
		if t == nil || t.Owner != actor || t.Revision != revision || t.Status == "canceled" || t.Status == "done" {
			return fail("stale_task", "task owner or revision changed")
		}
		if snapshot != "" && s.Snapshots[snapshot] == nil {
			return unavailableWorkspace()
		}
		if owner := s.Members[actor]; owner != nil {
			c := s.Contexts[owner.Context]
			if !owner.ReadOnly && c != nil && c.Checkout != nil && snapshot == "" {
				return errors.New("editing results require an immutable candidate captured from the assigned editing workspace")
			}
			if snapshot != "" && (c == nil || s.Snapshots[snapshot].Source != c.Root) {
				return errors.New("submitted capture must come from its owner's execution context")
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
	defer r.scheduleRelease()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.edit(ctx, func(s *State) error {
		t := s.ownTask(taskID)
		if t != nil && requirementOf(s, t) == RequirementDelivered {
			return fail("invalid_args", "task "+taskID+" completes on delivery; ask a follow-up with followup_task")
		}
		if t == nil || revision <= 0 || t.Status != "awaiting_review" || t.Revision != revision {
			return fail("stale_task", "review must name the current submitted revision")
		}
		if accept && editingTask(s, t) {
			return fail("invalid_args", fmt.Sprintf("task %s is editing work; integrate it with %s or request changes with feedback", t.ID, integrateTasksAction([]TaskReference{{Task: t.ID, Revision: t.Revision}})))
		}
		if err := reactivateTask(s, t); err != nil {
			return err
		}
		if accept {
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
			mail := &Mail{ID: ids.New(), From: r.ID, To: t.Owner, Kind: "info", Task: t.ID, Revision: t.Revision, Execution: t.Execution, Text: "Changes requested for task " + t.ID + ": " + feedback, Posted: time.Now().UTC()}
			put(s, "mail", &s.Messages, mail.ID, mail)
		}
		return nil
	})
}
func (r *Runtime) CancelTask(ctx context.Context, taskID string) error {
	defer r.scheduleRelease()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.edit(ctx, func(s *State) error {
		t := s.ownTask(taskID)
		if t == nil {
			return errors.New("unknown task")
		}
		if err := reactivateTask(s, t); err != nil {
			return err
		}
		t.Status = "canceled"
		t.AcceptedRevision = 0
		t.Revision++
		for key, other := range s.Tasks {
			for _, dep := range other.Dependencies {
				if dep == taskID && other.Status != "done" && other.Status != "canceled" {
					other = s.ownTask(key)
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
	err := r.edit(ctx, func(s *State) error {
		if err := member(s, r.ID, actor); err != nil {
			return err
		}
		if err := member(s, r.ID, to); err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
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
			s.ownMail(replyTo).ReplyID = mail.ID
		}
		put(s, "mail", &s.Messages, mail.ID, mail)
		return nil
	})
	return mail, err
}

// UpdateTask changes assignment and dependencies under parent authority.
// A running owner must first be stopped; published candidates remain in history.
func (r *Runtime) UpdateTask(ctx context.Context, taskID string, revision int, owner string, deps []string) error {
	defer r.scheduleRelease()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.edit(ctx, func(s *State) error {
		t := s.ownTask(taskID)
		if deliveringTask(s, t) {
			return fail("blocked", "its result awaits delivery; cancel it or wait")
		}
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

// BlockTask records an owner's blocker. A workflow cannot integrate or refresh
// its workers while awaiting them, so an explicit blocker interrupts that
// attempt and returns control to the parent. The parent explicitly unblocks it.
func (r *Runtime) BlockTask(ctx context.Context, actor, taskID string, revision int, reason string) error {
	r.parentTools.Lock()
	var controller, execution string
	var blocker *workflow.Error
	err := r.edit(ctx, func(s *State) error {
		t := s.ownTask(taskID)
		if deliveringTask(s, t) {
			return fail("blocked", "result awaits delivery; use a follow-up for new work")
		}
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
		m, e := s.Members[actor], s.Executions[t.Execution]
		if m != nil && e != nil && m.Task == t.ID && m.Execution == e.ID && e.Member == actor && m.Controller == e.Workflow && (e.Status == "running" || e.Status == "waiting" || e.Status == "queued") {
			if w := s.Workflows[e.Workflow]; w != nil && w.Status == "running" {
				controller, execution = w.ID, e.ID
				blocker = &workflow.Error{
					Code: "workflow_blocked", Session: actor,
					Message: fmt.Sprintf("Worker %s blocked task %s: %s", agentName(m), t.ID, reason),
					Result:  map[string]any{"task": t.ID, "execution": e.ID, "revision": t.Revision, "reason": reason},
				}
			}
		}
		return nil
	})
	committed := err == nil
	if err != nil && blocker != nil {
		// An update error can lose a successful commit's reply. Resolve only
		// this blocker, under the same task-edit exclusion, without replaying
		// the write or letting caller cancellation hide the durable receipt.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		s, readErr := r.read(readCtx)
		cancel()
		if readErr == nil {
			t, m := s.Tasks[taskID], s.Members[actor]
			e, w := s.Executions[execution], s.Workflows[controller]
			committed = t != nil && t.Owner == actor && t.Execution == execution && t.Revision == revision+1 && t.Status == "blocked" && t.Feedback == reason && t.AcceptedRevision == 0 &&
				m != nil && m.Task == taskID && m.Execution == execution && m.Controller == controller &&
				e != nil && e.Member == actor && e.Workflow == controller && w != nil && w.Status == "running"
			if committed {
				r.changed()
			}
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
	for _, m := range s.mailFor(to) {
		if !pending || !m.Delivered {
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
	if err := r.prepare(ctx); err != nil {
		return nil, err
	}
	err := r.editOn(ctx, r.parent, func(raw *sessions.CoordinationState, s *State) error {
		if err := member(s, r.ID, actor); err != nil {
			return err
		}
		switch p.Kind {
		case "", PublicationKindFinding, PublicationKindHost:
		default:
			return fail("invalid_args", "kind must be finding or host")
		}
		if p.Supersedes != "" {
			old := s.Publications[p.Supersedes]
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
		if p.Snapshot != "" && s.Snapshots[p.Snapshot] == nil {
			return unavailableWorkspace()
		}
		p.ID = ids.New()
		p.Author = actor
		p.Run = r.currentRun(s).ID
		// A resumed member may still belong to an earlier run.
		if m := s.Members[actor]; m != nil && memberRun(s, m) != "" {
			p.Run = memberRun(s, m)
		}
		if err := preparePublicationScope(s, actor, &p); err != nil {
			return err
		}
		p.Posted = time.Now().UTC()
		p.DuplicateOf = ""
		p.AdmittedTo = nil
		if p.Key != "" && p.Supersedes == "" {
			for _, old := range currentPublications(s) {
				if samePublicationFact(s, old, &p) {
					p.DuplicateOf = old.ID
					break
				}
			}
		}
		put(s, "publication", &s.Publications, p.ID, &p)
		result = &p
		raw.PinOwner = actor
		for _, ref := range p.Artifacts {
			raw.Pins = append(raw.Pins, ref.ID)
		}
		return nil
	})
	if err == nil {
		r.changed()
	}
	return result, err
}

// contextScope is the workspace and authority a context's tools are opened
// for. It hides the source checkout, the runtime directory and every other
// context's root as private roots; the tool implementation re-grants only
// the member's own root and scratch inside them, so siblings are invisible
// structurally rather than by name. The common Git object store and the
// user's Git configuration stay readable; filesystem isolation is not
// source-code secrecy.
func (r *Runtime) contextScope(ctx context.Context, s *State, c *ExecutionContext) (tools.ToolScope, error) {
	return r.scopeOf(ctx, c, otherRoots(s, c))
}

// otherRoots lists the root of every context that does not share c's.
func otherRoots(s *State, c *ExecutionContext) []string {
	if c == nil {
		return nil
	}
	var roots []string
	if s.rootUse != nil && s.editing == nil {
		for root := range s.rootUse {
			if root != c.Root {
				roots = append(roots, root)
			}
		}
		return roots
	}
	for _, other := range s.Contexts {
		if other.Root != c.Root {
			roots = append(roots, other.Root)
		}
	}
	return roots
}

// scopeOf is contextScope for a caller that already holds the other
// contexts' roots.
func (r *Runtime) scopeOf(ctx context.Context, c *ExecutionContext, others []string) (tools.ToolScope, error) {
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
	denied = append(denied, others...)
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
