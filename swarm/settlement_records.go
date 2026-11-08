package swarm

import (
	"context"
	"fmt"
	"strconv"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// settlementRecords is an obligation closure, not a writable family snapshot.
// Every open task is selected, along with its original owner, execution, deferral
// workflow and captured provenance. Candidate validity adds only its exact task
// references and receipt. Only repair writes below may consume this closure;
// neither presentation nor inferred workflow ownership supplies authority.
type settlementRecords struct {
	rd      *recordReader
	s       *State
	scopes  []sessions.RevisionScope
	seen    map[sessions.RecordKey]bool
	running bool
}

// Record subscriptions include missing dependencies. The selector subscriptions
// cover new obligations, including moves into a previously empty partition.
type settlementScopeReader struct {
	sessions.CoordinationReader
	scopes *[]sessions.RevisionScope
}

func (r settlementScopeReader) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	*r.scopes = append(*r.scopes, waitRecordScope(key.Kind, key.ID))
	return r.CoordinationReader.Get(key)
}
func (r settlementScopeReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	*r.scopes = append(*r.scopes, sessions.RevisionScope{Index: q.Index, Key: q.Key})
	return r.CoordinationReader.Query(q)
}

func settlementLoad[T any](d *settlementRecords, kind, id string, values map[string]*T) error {
	key := sessions.RecordKey{Kind: kind, ID: id}
	if id == "" || d.seen[key] {
		return nil
	}
	d.seen[key] = true
	value, err := getRecord[T](d.rd, kind, id)
	if err == nil && value != nil {
		values[id] = value
	}
	return err
}

func settlementSelect[T any](d *settlementRecords, kind string, index sessions.CoordinationIndex, key string, values map[string]*T) error {
	q := sessions.RecordQuery{Index: index, Key: key, Limit: sessions.MaxCoordinationPageLimit}
	for {
		page, err := queryRecords[T](d.rd, kind, q)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			values[record.Key.ID] = record.Value
			d.seen[record.Key] = true
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == q.After {
			return fmt.Errorf("%w: nonadvancing settlement cursor", sessions.ErrCoordinationCursor)
		}
		q.After = page.Next
	}
}

// Candidate selection remains on the supplied callback-local reader. Its kind
// revision is conservative notification coverage, not a broad logical read.
func settlementCandidates[T any](d *settlementRecords, kind string, selector sessions.CoordinationCandidateSelector, key string, values map[string]*T) error {
	d.scopes = append(d.scopes, sessions.RevisionScope{Index: sessions.CoordinationIndexKind, Key: kind})
	q := sessions.CoordinationCandidateQuery{Selector: selector, Key: key, Limit: sessions.MaxCoordinationPageLimit}
	for {
		page, err := sessions.QueryCoordinationCandidates(d.rd.raw.(settlementScopeReader).CoordinationReader, q)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			runtimemetrics.Add(d.rd.ctx, runtimemetrics.RecordsDecoded, 1)
			value, err := decodeRecord[T](record, d.rd.forWrite)
			if err != nil {
				return err
			}
			values[record.Key.ID] = value
			d.seen[record.Key] = true
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == q.After {
			return fmt.Errorf("%w: nonadvancing settlement candidate cursor", sessions.ErrCoordinationCursor)
		}
		q.After = page.Next
	}
}

func (d *settlementRecords) taskBase(t *Task) error {
	if t == nil {
		return nil
	}
	if err := settlementLoad(d, "member", t.Owner, d.s.Members); err != nil {
		return err
	}
	if err := settlementLoad(d, "execution", t.Execution, d.s.Executions); err != nil {
		return err
	}
	if e := d.s.Executions[t.Execution]; e != nil {
		if err := settlementLoad(d, "workflow", e.Workflow, d.s.Workflows); err != nil {
			return err
		}
	}
	if t.Deferral != nil {
		if err := settlementLoad(d, "workflow", t.Deferral.Workflow, d.s.Workflows); err != nil {
			return err
		}
	}
	for _, id := range []string{t.StartingSnapshot, t.Snapshot} {
		if err := settlementLoad(d, "snapshot", id, d.s.Snapshots); err != nil {
			return err
		}
	}
	return nil
}

func (d *settlementRecords) candidates(t *Task) error {
	if t.Status != "awaiting_review" || !editingTask(d.s, t) {
		return nil
	}
	selected := map[string]*IntegrationCandidate{}
	if err := settlementSelect(d, "integration", sessions.CoordinationIndexOverviewDependency, sessions.CoordinationIndexKey("task", t.ID), selected); err != nil {
		return err
	}
	for id, c := range selected {
		d.s.Integrations[id] = c
		if c.Status != "ready" && c.Status != "conflicted" || c.Successor != "" {
			continue
		}
		if err := settlementLoad(d, "apply", c.ID, d.s.Applies); err != nil {
			return err
		}
		for _, ref := range c.references() {
			if err := settlementLoad(d, "task", ref.Task, d.s.Tasks); err != nil {
				return err
			}
			if err := d.taskBase(d.s.Tasks[ref.Task]); err != nil {
				return err
			}
		}
	}
	return nil
}

func newSettlementRecords(rd *recordReader) *settlementRecords {
	d := &settlementRecords{seen: map[sessions.RecordKey]bool{}, scopes: []sessions.RevisionScope{ActivityScope(), waitRecordScope(formatKind, formatID)}, s: &State{
		Runs: map[string]*Run{}, Tasks: map[string]*Task{}, Members: map[string]*Member{},
		Executions: map[string]*Execution{}, Messages: map[string]*Mail{}, Workflows: map[string]*workflow.Report{},
		Snapshots: map[string]*worktree.Snapshot{}, Integrations: map[string]*IntegrationCandidate{}, Applies: map[string]*ApplyRecord{},
	}}
	copy := *rd
	copy.raw = settlementScopeReader{rd.raw, &d.scopes}
	d.rd = &copy
	return d
}

func (d *settlementRecords) taskNotice(t *Task) error {
	if !deliveringTask(d.s, t) {
		return nil
	}
	d.scopes = append(d.scopes, sessions.RevisionScope{Index: sessions.CoordinationIndexKind, Key: "mail"})
	page, err := sessions.QueryCoordinationCandidates(d.rd.raw.(settlementScopeReader).CoordinationReader, sessions.CoordinationCandidateQuery{
		Selector: sessions.CandidateTaskNotice, Key: sessions.CoordinationIndexKey(t.ID, t.Execution, strconv.Itoa(t.Revision)), Limit: 1,
	})
	if err != nil {
		return err
	}
	for _, record := range page.Records {
		runtimemetrics.Add(d.rd.ctx, runtimemetrics.RecordsDecoded, 1)
		mail, err := decodeRecord[Mail](record, d.rd.forWrite)
		if err != nil {
			return err
		}
		d.s.Messages[record.Key.ID] = mail
	}
	return nil
}

// The prebootstrap format guard proves which records exist, not their payload
// shape. Preserve the full-state reader's typed journal validation without
// preparing a swarm or issuing format-dependent candidate queries.
func validateSettlementJournal(rd *recordReader) error {
	identity, err := rd.Identity()
	if err != nil {
		return err
	}
	_, err = rd.ParentTurn(identity.ParentID)
	return err
}

func loadSettlementRecords(rd *recordReader, actor string) (*settlementRecords, error) {
	d := newSettlementRecords(rd)
	// The callback-local reader never escapes with the owned decision inputs.
	defer func() { d.rd = nil }()
	if rd.journalOnly {
		return d, validateSettlementJournal(rd)
	}
	for _, status := range []string{"running", "paused"} {
		if err := settlementSelect(d, "run", sessions.CoordinationIndexRunStatus, status, d.s.Runs); err != nil {
			return nil, err
		}
	}
	if err := settlementSelect(d, "task", sessions.CoordinationIndexTaskOpen, "", d.s.Tasks); err != nil {
		return nil, err
	}
	if err := settlementSelect(d, "apply", sessions.CoordinationIndexApplyUncertain, "", d.s.Applies); err != nil {
		return nil, err
	}
	if err := settlementCandidates(d, "workflow", sessions.CandidateWorkflowUnsettled, "", d.s.Workflows); err != nil {
		return nil, err
	}
	if err := settlementCandidates(d, "mail", sessions.CandidateMailSettlement, actor, d.s.Messages); err != nil {
		return nil, err
	}
	for _, status := range []string{"queued", "running"} {
		page, err := d.rd.raw.Query(sessions.RecordQuery{Kind: "execution", Index: sessions.CoordinationIndexExecutionStatus, Key: status, CountOnly: true, IncludeTotal: true})
		if err != nil {
			return nil, err
		}
		d.running = d.running || page.Total != 0
	}
	for _, w := range d.s.Workflows {
		if w.Status != "running" {
			continue
		}
		page, err := d.rd.raw.Query(sessions.RecordQuery{Kind: "workflow_step", Index: sessions.CoordinationIndexWorkflowSteps, Key: w.ID, CountOnly: true, IncludeTotal: true})
		if err != nil {
			return nil, err
		}
		steps := map[string]*workflow.Step{}
		if err := settlementCandidates(d, "workflow_step", sessions.CandidateWorkflowHostRunningStep, w.ID, steps); err != nil {
			return nil, err
		}
		d.running = d.running || page.Total == 0 || len(steps) != 0
	}
	// Freeze the selected open-task IDs before candidate validity adds completed
	// reference tasks. Those references cannot become repair candidates.
	for _, id := range sortedInspectionIDs(d.s.Tasks) {
		t := d.s.Tasks[id]
		if err := d.taskBase(t); err != nil {
			return nil, err
		}
		if err := d.taskNotice(t); err != nil {
			return nil, err
		}
		if err := d.candidates(t); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (d *settlementRecords) facts(actor string) *coordinationFacts {
	f := deriveFacts(d.s, actor)
	// Workflow headers deliberately contain no steps. Running is observed from
	// the step selectors, not from treating those headers as empty reports.
	f.running = d.running
	return f
}

// ensureTaskDeliveryNoticeRecords is also used by constructor recovery. Its
// exact task and dependencies are reloaded inside the caller's transaction;
// caller observations and display provenance cannot authorize the notice.
func (r *Runtime) ensureTaskDeliveryNoticeRecords(tx *recordTx, taskID string) error {
	d := newSettlementRecords(tx.recordReader)
	if err := settlementLoad(d, "task", taskID, d.s.Tasks); err != nil {
		return err
	}
	t := d.s.Tasks[taskID]
	if err := d.taskBase(t); err != nil {
		return err
	}
	if err := d.taskNotice(t); err != nil {
		return err
	}
	return r.putSettlementDeliveryNotice(tx, d.s, t)
}

func (r *Runtime) putSettlementDeliveryNotice(tx *recordTx, s *State, t *Task) error {
	if !needsDeliveryNotice(s, t) {
		return nil
	}
	notice := r.completionNotice(s, s.Members[t.Owner], s.Executions[t.Execution], t)
	if err := tx.PutMail(notice); err != nil {
		return err
	}
	s.Messages[notice.ID] = notice
	return nil
}

func loadDeliveryNoticeRecords(rd *recordReader) (*settlementRecords, error) {
	d := newSettlementRecords(rd)
	defer func() { d.rd = nil }()
	if rd.journalOnly {
		return d, validateSettlementJournal(rd)
	}
	if err := settlementSelect(d, "task", sessions.CoordinationIndexTaskOpen, "", d.s.Tasks); err != nil {
		return nil, err
	}
	for _, t := range d.s.Tasks {
		if err := d.taskBase(t); err != nil {
			return nil, err
		}
		if err := d.taskNotice(t); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (r *Runtime) ensureDeliveryNoticesRecords(tx *recordTx) error {
	d, err := loadDeliveryNoticeRecords(tx.recordReader)
	if err != nil {
		return err
	}
	for _, id := range sortedInspectionIDs(d.s.Tasks) {
		if err := r.putSettlementDeliveryNotice(tx, d.s, d.s.Tasks[id]); err != nil {
			return err
		}
	}
	return nil
}

// Returning from waitParent repairs delivery notices without converting a
// successful native wait into a family snapshot. The readiness observation and
// its activity reread remain the caller's; this is a separate repair operation.
func (r *Runtime) repairDeliveryNoticesOnReturn(ctx context.Context, legacy *State) error {
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	if !repo.Capabilities().NativeIndexed {
		if legacy == nil {
			legacy, err = r.read(ctx)
		}
		if err == nil {
			_, err = r.repairDeliveryNotices(ctx, legacy)
		}
		return err
	}
	ctx = context.WithValue(ctx, notificationWaitReadKey{}, false)
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	needed := false
	// Observing an empty root or its own parent journal must not prepare or
	// promote a swarm. The repository still rejects every other format-less root.
	repo.parentJournal = true
	err = repo.Read(ctx, func(rd *recordReader) error {
		d, err := loadDeliveryNoticeRecords(rd)
		if err != nil {
			return err
		}
		for _, t := range d.s.Tasks {
			needed = needed || needsDeliveryNotice(d.s, t)
		}
		return nil
	})
	if err != nil || !needed {
		return err
	}
	return r.updateRecords(ctx, r.ensureDeliveryNoticesRecords)
}

func (d *settlementRecords) needsRepair() bool {
	uncertain := false
	for _, a := range d.s.Applies {
		uncertain = uncertain || coordsem.UncertainApply(a.Status)
	}
	for _, t := range d.s.Tasks {
		if needsDeliveryNotice(d.s, t) || !uncertain && acceptedUnchangedTask(d.s, t) {
			return true
		}
	}
	return false
}

func (r *Runtime) subscribeSettlementFacts(ctx context.Context) (*ChangeSubscription, error) {
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return nil, err
	}
	if !repo.Capabilities().NativeIndexed {
		return r.subscribeSettlement(ctx)
	}
	return r.Subscribe(ctx, []sessions.RevisionScope{
		ActivityScope(), waitRecordScope(formatKind, formatID),
		{Index: sessions.CoordinationIndexTaskOpen},
		{Index: sessions.CoordinationIndexApplyUncertain},
		{Index: sessions.CoordinationIndexRunStatus, Key: "running"},
		{Index: sessions.CoordinationIndexRunStatus, Key: "paused"},
		{Index: sessions.CoordinationIndexExecutionStatus, Key: "queued"},
		{Index: sessions.CoordinationIndexExecutionStatus, Key: "running"},
		{Index: sessions.CoordinationIndexKind, Key: "workflow"},
		{Index: sessions.CoordinationIndexKind, Key: "mail"},
	})
}

// settlementFacts preserves the original compatibility path. Native repair is
// lease-fenced, reselects every authority input in the write transaction, and
// writes only newly required notices and accepted no-op tasks. Ordinary reads
// never open an update transaction or publish their own wakeup.
func (r *Runtime) settlementFacts(ctx context.Context) (*coordinationFacts, []sessions.RevisionScope, error) {
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return nil, nil, err
	}
	if !repo.Capabilities().NativeIndexed {
		s, err := r.settlementState(ctx)
		if err != nil {
			return nil, nil, err
		}
		return deriveFacts(s, r.ID), []sessions.RevisionScope{{Index: sessions.CoordinationIndexRecords}, ActivityScope()}, nil
	}
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	var d *settlementRecords
	// Settle is also called before the first swarm mutation. Read through the
	// explicit prebootstrap contract instead of readRecords, which calls prepare.
	repo.parentJournal = true
	err = repo.Read(ctx, func(rd *recordReader) error {
		d, err = loadSettlementRecords(rd, r.ID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if d.needsRepair() {
		err = r.updateRecords(ctx, func(tx *recordTx) error {
			current, err := loadSettlementRecords(tx.recordReader, r.ID)
			if err != nil {
				return err
			}
			uncertain := false
			for _, a := range current.s.Applies {
				uncertain = uncertain || coordsem.UncertainApply(a.Status)
			}
			for _, id := range sortedInspectionIDs(current.s.Tasks) {
				t := current.s.Tasks[id]
				if err := r.putSettlementDeliveryNotice(tx, current.s, t); err != nil {
					return err
				}
				if !uncertain && acceptedUnchangedTask(current.s, t) {
					t.Status = "done"
					if err := tx.PutTask(t); err != nil {
						return err
					}
				}
			}
			d = current
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return d.facts(r.ID), d.scopes, nil
}
