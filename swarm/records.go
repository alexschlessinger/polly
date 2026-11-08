package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// recordRepository keeps the supplied handle authoritative. A legacy handle is
// functional but materializes the family on every operation; this facade does
// not turn that fallback into indexed storage. It performs no preparation,
// promotion, notification, domain transition or automatic callback retry.
// Callers must prepare format 2 before using swarm operations. The sole
// exception is the root's own pre-swarm parent journal, explicitly opted in.
type recordRepository struct {
	access        sessions.CoordinationAccess
	parentJournal bool // only the format-less root journal, never a partial swarm
}

func openRecordRepository(handle any) (*recordRepository, error) {
	access, err := sessions.OpenCoordinationAccess(handle)
	if err != nil {
		return nil, err
	}
	return &recordRepository{access: access}, nil
}

func (r *recordRepository) Capabilities() sessions.CoordinationCapabilities {
	return r.access.Capabilities()
}

// Read and Update never retry callbacks, including after an uncertain commit.
// The reader/transaction must not escape the callback or be used concurrently.
// Owned decoded values may escape; changing them alone never stages a write.
// Nested calls into the supplied store are prohibited by the storage contract.
func (r *recordRepository) Read(ctx context.Context, fn func(*recordReader) error) error {
	if fn == nil {
		return fmt.Errorf("%w: callback required", sessions.ErrCoordinationRequest)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	called := false
	err := r.access.ReadRecords(ctx, func(raw sessions.CoordinationReader) error {
		if called {
			return fmt.Errorf("%w: backend retried record callback", sessions.ErrCoordinationRequest)
		}
		called = true
		reader := &recordReader{raw: raw, ctx: ctx, active: true}
		defer func() { reader.active = false }()
		if err := reader.checkFormatFor(r.parentJournal); err != nil {
			return err
		}
		if err := fn(reader); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err == nil && !called {
		return fmt.Errorf("%w: backend omitted record callback", sessions.ErrCoordinationRequest)
	}
	return err
}

func (r *recordRepository) Update(ctx context.Context, fn func(*recordTx) error) (sessions.CoordinationCommit, error) {
	if fn == nil {
		return sessions.CoordinationCommit{}, fmt.Errorf("%w: callback required", sessions.ErrCoordinationRequest)
	}
	if err := ctx.Err(); err != nil {
		return sessions.CoordinationCommit{}, err
	}
	called := false
	commit, err := r.access.UpdateRecords(ctx, func(raw sessions.CoordinationTx) error {
		if called {
			return fmt.Errorf("%w: backend retried record callback", sessions.ErrCoordinationRequest)
		}
		called = true
		reader := &recordReader{raw: raw, ctx: ctx, active: true, forWrite: true}
		defer func() { reader.active = false }()
		if err := reader.checkFormatFor(r.parentJournal); err != nil {
			return err
		}
		if err := fn(&recordTx{recordReader: reader, tx: raw}); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		// An uncertain result is not a receipt and must never advance a cursor.
		return sessions.CoordinationCommit{}, err
	}
	if !called {
		return sessions.CoordinationCommit{}, fmt.Errorf("%w: backend omitted record callback", sessions.ErrCoordinationRequest)
	}
	commit.Changed = append([]sessions.RevisionScope(nil), commit.Changed...)
	return commit, nil
}

type recordReader struct {
	raw         sessions.CoordinationReader
	ctx         context.Context
	active      bool
	journalOnly bool
	forWrite    bool // retain JSON number tokens when decoded values may be rewritten
}

type recordTx struct {
	*recordReader
	tx sessions.CoordinationTx
}

func (r *recordReader) ready() error {
	if r == nil || !r.active {
		return sessions.ErrCoordinationClosed
	}
	return r.ctx.Err()
}

// One guard covers the consistent callback view, including all query/loaders.
// Format cannot be changed through recordTx, so read-your-writes cannot bypass it.
func (r *recordReader) checkFormatFor(parentJournal bool) error {
	identity, err := r.Identity()
	if err != nil {
		return err
	}
	format, err := getRecord[FormatRecord](r, formatKind, formatID)
	if err != nil {
		return err
	}
	if format == nil && parentJournal && identity.ActorID == identity.ParentID {
		// A root's own turn predates swarm bootstrap. Only that journal is
		// allowed without format; this is not permission to read a malformed swarm.
		page, err := r.raw.Query(sessions.RecordQuery{Limit: 2})
		if err != nil {
			return err
		}
		if page.Next != "" {
			return unsupportedFormat(identity.ParentID, nil)
		}
		for _, record := range page.Records {
			if record.Key.Kind != "parent_turn" || record.Key.ID != identity.ActorID {
				return unsupportedFormat(identity.ParentID, nil)
			}
		}
		r.journalOnly = true
		return nil
	}
	if format == nil || format.Version != swarmFormatVersion {
		return unsupportedFormat(identity.ParentID, format)
	}
	return nil
}

func (r *recordReader) Identity() (sessions.CoordinationIdentity, error) {
	if err := r.ready(); err != nil {
		return sessions.CoordinationIdentity{}, err
	}
	return r.raw.Identity()
}

func (r *recordReader) Revisions(scopes []sessions.RevisionScope) ([]sessions.Revision, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	revisions, err := r.raw.Revisions(append([]sessions.RevisionScope(nil), scopes...))
	if err != nil {
		return nil, err
	}
	return append([]sessions.Revision(nil), revisions...), nil
}

// Missing exact records return nil, nil. Every decode returns independent owned
// pointers, slices and maps, even for repeated reads of the same record.
func getRecord[T any](r *recordReader, kind, id string) (*T, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("%w: empty %s ID", sessions.ErrCoordinationRequest, kind)
	}
	record, found, err := r.raw.Get(sessions.RecordKey{Kind: kind, ID: id})
	if err != nil || !found {
		return nil, err
	}
	runtimemetrics.Add(r.ctx, runtimemetrics.RecordsDecoded, 1)
	return decodeRecord[T](record, r.forWrite)
}

func decodeRecord[T any](record sessions.CoordinationRecord, forWrite bool) (*T, error) {
	var value *T
	var err error
	if forWrite {
		// Control/status updates must not round opaque task results, workflow
		// inputs or saved completion values. Read-only projections retain their
		// existing float64 representation; only transaction values use Number.
		decoder := json.NewDecoder(bytes.NewReader(record.Payload))
		decoder.UseNumber()
		err = decoder.Decode(&value)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = fmt.Errorf("trailing JSON: %w", sessions.ErrCoordinationFormat)
		}
	} else {
		err = json.Unmarshal(record.Payload, &value)
	}
	if err != nil {
		return nil, fmt.Errorf("read swarm %s %q: %w", record.Key.Kind, record.Key.ID, err)
	}
	if value == nil {
		return nil, fmt.Errorf("read swarm %s %q: null record: %w", record.Key.Kind, record.Key.ID, sessions.ErrCoordinationFormat)
	}
	if record.Key.Kind == "workflow" {
		var embedded struct {
			Steps []json.RawMessage `json:"steps"`
		}
		if err := json.Unmarshal(record.Payload, &embedded); err != nil {
			return nil, fmt.Errorf("read swarm workflow %q: %w", record.Key.ID, err)
		}
		if len(embedded.Steps) != 0 {
			return nil, fmt.Errorf("read swarm workflow %q: embedded steps: %w", record.Key.ID, ErrUnsupportedFormat)
		}
	}
	return value, nil
}

type recordValue[T any] struct {
	Key      sessions.RecordKey
	Value    *T
	Revision sessions.Revision
}

type recordPage[T any] struct {
	Records   []recordValue[T]
	Next      string
	Total     int
	Oversized bool
}

// Query helpers fix the kind before selection, not after decoding. Index keys
// and continuations retain sessions' selector semantics. Publication queries
// return candidates only: they never decide audience, equivalence or receipts.
func queryRecords[T any](r *recordReader, kind string, query sessions.RecordQuery) (recordPage[T], error) {
	if err := r.ready(); err != nil {
		return recordPage[T]{}, err
	}
	if query.Kind != "" && query.Kind != kind {
		return recordPage[T]{}, fmt.Errorf("%w: query kind %q, want %q", sessions.ErrCoordinationRequest, query.Kind, kind)
	}
	query.Kind = kind
	page, err := r.raw.Query(query)
	if err != nil {
		return recordPage[T]{}, err
	}
	out := recordPage[T]{Next: page.Next, Total: page.Total, Oversized: page.Oversized, Records: make([]recordValue[T], 0, len(page.Records))}
	for _, record := range page.Records {
		if record.Key.Kind != kind {
			return recordPage[T]{}, fmt.Errorf("%w: backend returned kind %q for %q", sessions.ErrCoordinationRequest, record.Key.Kind, kind)
		}
		runtimemetrics.Add(r.ctx, runtimemetrics.RecordsDecoded, 1)
		value, err := decodeRecord[T](record, r.forWrite)
		if err != nil {
			return recordPage[T]{}, err
		}
		out.Records = append(out.Records, recordValue[T]{Key: record.Key, Value: value, Revision: record.Revision})
	}
	return out, nil
}

func putRecord[T any](tx *recordTx, kind, id string, value *T) error {
	if err := tx.ready(); err != nil {
		return err
	}
	if id == "" || value == nil {
		return fmt.Errorf("%w: %s ID and value required", sessions.ErrCoordinationRequest, kind)
	}
	if err := tx.journalKey(sessions.RecordKey{Kind: kind, ID: id}); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("write swarm %s %q: %w", kind, id, err)
	}
	runtimemetrics.Add(tx.ctx, runtimemetrics.RecordsEncoded, 1)
	return tx.tx.Put(sessions.RecordKey{Kind: kind, ID: id}, data)
}

func (tx *recordTx) journalKey(key sessions.RecordKey) error {
	if !tx.journalOnly {
		return nil
	}
	identity, err := tx.Identity()
	if err != nil {
		return err
	}
	if key.Kind != "parent_turn" || key.ID != identity.ActorID {
		return fmt.Errorf("%w: format-less root may write only its own parent journal", ErrUnsupportedFormat)
	}
	return nil
}

// Delete does not cascade domain relations. The operation must explicitly load
// its dependencies and decide whether deletion is authorized. Format is sealed.
func (tx *recordTx) Delete(key sessions.RecordKey) error {
	if err := tx.ready(); err != nil {
		return err
	}
	if err := tx.journalKey(key); err != nil {
		return err
	}
	switch key.Kind {
	case "member", "execution", "context", "task", "run", "mail", "publication", "parent", "parent_turn", "followup", "snapshot", "workflow", "workflow_step":
	default:
		return fmt.Errorf("%w: unsupported typed deletion %q", sessions.ErrCoordinationRequest, key.Kind)
	}
	return tx.tx.Delete(key)
}

func (tx *recordTx) Effects(effects sessions.CoordinationEffects) error {
	if err := tx.ready(); err != nil {
		return err
	}
	if tx.journalOnly && (len(effects.Pins) != 0 || len(effects.Members) != 0) {
		return fmt.Errorf("%w: format-less parent journal cannot retain swarm members or artifacts", ErrUnsupportedFormat)
	}
	return tx.tx.Effects(effects)
}

func (r *recordReader) Member(id string) (*Member, error) { return getRecord[Member](r, "member", id) }
func (r *recordReader) Execution(id string) (*Execution, error) {
	return getRecord[Execution](r, "execution", id)
}
func (r *recordReader) Context(id string) (*ExecutionContext, error) {
	return getRecord[ExecutionContext](r, "context", id)
}
func (r *recordReader) Task(id string) (*Task, error) { return getRecord[Task](r, "task", id) }
func (r *recordReader) Run(id string) (*Run, error)   { return getRecord[Run](r, "run", id) }
func (r *recordReader) Mail(id string) (*Mail, error) { return getRecord[Mail](r, "mail", id) }
func (r *recordReader) Publication(id string) (*Publication, error) {
	return getRecord[Publication](r, "publication", id)
}
func (r *recordReader) Parent(id string) (*ParentRecord, error) {
	return getRecord[ParentRecord](r, "parent", id)
}
func (r *recordReader) ParentTurn(id string) (*ParentTurn, error) {
	return getRecord[ParentTurn](r, "parent_turn", id)
}
func (r *recordReader) Followup(id string) (*FollowupCall, error) {
	return getRecord[FollowupCall](r, "followup", id)
}
func (r *recordReader) Snapshot(id string) (*worktree.Snapshot, error) {
	return getRecord[worktree.Snapshot](r, "snapshot", id)
}
func (r *recordReader) WorkflowHeader(id string) (*workflow.ReportHeader, error) {
	return getRecord[workflow.ReportHeader](r, "workflow", id)
}

func workflowStepRecordID(report string, index int) (string, error) {
	if report == "" || strings.Contains(report, "/") || index < 0 {
		return "", fmt.Errorf("%w: workflow report ID and nonnegative step required", sessions.ErrCoordinationRequest)
	}
	return report + "/" + strconv.Itoa(index), nil
}

func (r *recordReader) WorkflowStep(report string, index int) (*workflow.Step, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	id, err := workflowStepRecordID(report, index)
	if err != nil {
		return nil, err
	}
	return getRecord[workflow.Step](r, "workflow_step", id)
}

func (tx *recordTx) PutMember(value *Member) error {
	return putIdentifiedRecord(tx, "member", value, func(v *Member) string { return v.ID })
}
func (tx *recordTx) PutExecution(value *Execution) error {
	return putIdentifiedRecord(tx, "execution", value, func(v *Execution) string { return v.ID })
}
func (tx *recordTx) PutContext(value *ExecutionContext) error {
	return putIdentifiedRecord(tx, "context", value, func(v *ExecutionContext) string { return v.ID })
}
func (tx *recordTx) PutTask(value *Task) error {
	return putIdentifiedRecord(tx, "task", value, func(v *Task) string { return v.ID })
}
func (tx *recordTx) PutRun(value *Run) error {
	return putIdentifiedRecord(tx, "run", value, func(v *Run) string { return v.ID })
}
func (tx *recordTx) PutMail(value *Mail) error {
	return putIdentifiedRecord(tx, "mail", value, func(v *Mail) string { return v.ID })
}
func (tx *recordTx) PutPublication(value *Publication) error {
	return putIdentifiedRecord(tx, "publication", value, func(v *Publication) string { return v.ID })
}
func (tx *recordTx) PutParent(id string, value *ParentRecord) error {
	return putRecord(tx, "parent", id, value)
}
func (tx *recordTx) PutParentTurn(id string, value *ParentTurn) error {
	return putRecord(tx, "parent_turn", id, value)
}
func (tx *recordTx) PutFollowup(id string, value *FollowupCall) error {
	return putRecord(tx, "followup", id, value)
}
func (tx *recordTx) PutSnapshot(value *worktree.Snapshot) error {
	return putIdentifiedRecord(tx, "snapshot", value, func(v *worktree.Snapshot) string { return v.ID })
}
func (tx *recordTx) PutWorkflowHeader(value *workflow.ReportHeader) error {
	return putIdentifiedRecord(tx, "workflow", value, func(v *workflow.ReportHeader) string { return v.ID })
}
func (tx *recordTx) PutWorkflowStep(report string, index int, value *workflow.Step) error {
	if err := tx.ready(); err != nil {
		return err
	}
	id, err := workflowStepRecordID(report, index)
	if err != nil {
		return err
	}
	return putRecord(tx, "workflow_step", id, value)
}

func putIdentifiedRecord[T any](tx *recordTx, kind string, value *T, id func(*T) string) error {
	if value == nil {
		return putRecord(tx, kind, "", value)
	}
	return putRecord(tx, kind, id(value), value)
}

func (r *recordReader) Members(q sessions.RecordQuery) (recordPage[Member], error) {
	return queryRecords[Member](r, "member", q)
}
func (r *recordReader) Executions(q sessions.RecordQuery) (recordPage[Execution], error) {
	return queryRecords[Execution](r, "execution", q)
}
func (r *recordReader) Contexts(q sessions.RecordQuery) (recordPage[ExecutionContext], error) {
	return queryRecords[ExecutionContext](r, "context", q)
}
func (r *recordReader) Tasks(q sessions.RecordQuery) (recordPage[Task], error) {
	return queryRecords[Task](r, "task", q)
}
func (r *recordReader) Mails(q sessions.RecordQuery) (recordPage[Mail], error) {
	return queryRecords[Mail](r, "mail", q)
}
func (r *recordReader) Publications(q sessions.RecordQuery) (recordPage[Publication], error) {
	return queryRecords[Publication](r, "publication", q)
}
func (r *recordReader) Runs(q sessions.RecordQuery) (recordPage[Run], error) {
	return queryRecords[Run](r, "run", q)
}
func (r *recordReader) WorkflowHeaders(q sessions.RecordQuery) (recordPage[workflow.ReportHeader], error) {
	return queryRecords[workflow.ReportHeader](r, "workflow", q)
}
func (r *recordReader) WorkflowSteps(q sessions.RecordQuery) (recordPage[workflow.Step], error) {
	return queryRecords[workflow.Step](r, "workflow_step", q)
}

// SlowSnapshot is an explicitly unbounded bootstrap/cold-path escape hatch. It
// reads every record in the SAME callback view before calling whole-State
// helpers. Never assemble a partial State from the narrow getters for such a
// helper: many interpret an absent record as authority or completion.
func (r *recordReader) SlowSnapshot() (*State, error) {
	identity, err := r.Identity()
	if err != nil {
		return nil, err
	}
	raw := &sessions.CoordinationState{ActorID: identity.ActorID, ParentID: identity.ParentID, Sequence: identity.Sequence, Records: map[string]map[string]json.RawMessage{}}
	query := sessions.RecordQuery{Limit: sessions.MaxCoordinationPageLimit}
	for {
		page, err := r.raw.Query(query)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			if raw.Records[record.Key.Kind] == nil {
				raw.Records[record.Key.Kind] = map[string]json.RawMessage{}
			}
			raw.Records[record.Key.Kind][record.Key.ID] = append(json.RawMessage(nil), record.Payload...)
		}
		if page.Next == "" {
			break
		}
		if page.Next == query.After {
			return nil, fmt.Errorf("%w: nonadvancing snapshot cursor", sessions.ErrCoordinationCursor)
		}
		query.After = page.Next
	}
	return decodeState(raw)
}

// committedRecords publishes only the scopes of a known successful commit.
// Empty/effects-only writes do not invalidate coordination readers. This must
// never receive tentative callback changes or an uncertain commit's receipt.
func (r *Runtime) committedRecords(commit sessions.CoordinationCommit) {
	if len(commit.Changed) == 0 {
		return
	}
	r.signalCompatibilityChange()
	r.publishChanges(commit.Changed)
}

func (r *Runtime) updateRecords(ctx context.Context, fn func(*recordTx) error) error {
	if err := r.prepare(ctx); err != nil {
		return err
	}
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	commit, err := repo.Update(ctx, fn)
	if err == nil {
		r.committedRecords(commit)
	}
	return err
}

func (r *Runtime) readRecords(ctx context.Context, fn func(*recordReader) error) error {
	if err := r.prepare(ctx); err != nil {
		return err
	}
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	return repo.Read(ctx, fn)
}

func (r *recordReader) requireMember(parent, actor string) error {
	if actor == parent {
		return nil
	}
	m, err := r.Member(actor)
	if err != nil {
		return err
	}
	if m == nil {
		return fail("unknown_member", "caller is not a member of this swarm")
	}
	return nil
}
