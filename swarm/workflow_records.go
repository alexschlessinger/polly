package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

var (
	_ workflow.Recorder      = (*Runtime)(nil)
	_ workflow.DeltaRecorder = (*Runtime)(nil)
	_ workflow.DeltaRecorder = (*workflowHost)(nil)
)

// SaveWorkflow retains the full-report API, including truncation. Comparison is
// proportional to this report, not to the family; only changed records are put.
// Runner uses SaveWorkflowDelta to avoid even that history comparison.
func (r *Runtime) SaveWorkflow(ctx context.Context, report workflow.Report) error {
	delta := workflow.ReportDelta{Header: workflow.ReportHeader{
		CallID: report.CallID, Run: report.Run, Acknowledged: report.Acknowledged,
		ID: report.ID, Name: report.Name, Source: report.Source, Input: report.Input,
		Output: report.Output, Status: report.Status, Error: report.Error,
		Started: report.Started, Finished: report.Finished,
	}, TotalSteps: len(report.Steps), Steps: make(map[int]workflow.Step, len(report.Steps))}
	for index, step := range report.Steps {
		delta.Steps[index] = step
	}
	return r.saveWorkflowDelta(ctx, delta, true)
}

// SaveWorkflowDelta atomically records intent/receipts before the runner proceeds.
// A storage error, including an uncertain commit, is returned without replay.
func (r *Runtime) SaveWorkflowDelta(ctx context.Context, delta workflow.ReportDelta) error {
	return r.saveWorkflowDelta(ctx, delta, false)
}

func (r *Runtime) saveWorkflowDelta(ctx context.Context, delta workflow.ReportDelta, full bool) error {
	defer r.scheduleRelease()
	if _, err := workflowStepRecordID(delta.Header.ID, 0); err != nil {
		return err
	}
	if delta.TotalSteps < 0 {
		return fmt.Errorf("%w: negative workflow step count", sessions.ErrCoordinationRequest)
	}
	for index := range delta.Steps {
		if index < 0 || index >= delta.TotalSteps {
			return fmt.Errorf("%w: workflow step %d outside count %d", sessions.ErrCoordinationRequest, index, delta.TotalSteps)
		}
	}
	return r.updateRecords(ctx, func(tx *recordTx) error {
		return r.applyWorkflowDelta(tx, delta, full)
	})
}

// applyWorkflowDelta consumes a consistent record view, never a partial State.
// Count and the last numeric key prove existing history is contiguous without
// fetching unchanged steps. Then every new position must be supplied. Full saves
// may shrink history; deltas may only replace or append, as ReportDelta requires.
func (r *Runtime) applyWorkflowDelta(tx *recordTx, delta workflow.ReportDelta, full bool) error {
	header := delta.Header
	prior, err := savedWorkflowHeader(tx, header.ID)
	if err != nil {
		return err
	}
	page, err := tx.raw.Query(sessions.RecordQuery{Index: sessions.CoordinationIndexWorkflowSteps, Key: header.ID, CountOnly: true, IncludeTotal: true, IncludeLast: true})
	if err != nil {
		return err
	}
	count := page.Total
	if count < 0 || prior == nil && count != 0 {
		return fmt.Errorf("%w: workflow step history has no header/count", sessions.ErrCoordinationFormat)
	}
	// The codec accepts only unique canonical nonnegative numeric step keys.
	// Such a set has no gaps exactly when its maximum index is count-1.
	if count == 0 {
		if page.Last != nil {
			return fmt.Errorf("%w: empty workflow step history has a last key", sessions.ErrCoordinationFormat)
		}
	} else {
		last, err := workflowStepRecordID(header.ID, count-1)
		if err != nil {
			return err
		}
		if page.Last == nil || *page.Last != (sessions.RecordKey{Kind: "workflow_step", ID: last}) {
			return fmt.Errorf("%w: workflow step history is not contiguous", sessions.ErrCoordinationFormat)
		}
	}
	if !full && delta.TotalSteps < count {
		return fmt.Errorf("%w: workflow delta cannot shrink from %d to %d steps", sessions.ErrCoordinationRequest, count, delta.TotalSteps)
	}
	// Check the number of supplied new positions before iterating the range, so
	// a malformed enormous TotalSteps cannot cause unbounded validation work.
	if delta.TotalSteps > count && delta.TotalSteps-count > len(delta.Steps) {
		return fmt.Errorf("%w: workflow delta leaves a step gap", sessions.ErrCoordinationRequest)
	}
	for index := count; index < delta.TotalSteps; index++ {
		if _, ok := delta.Steps[index]; !ok {
			return fmt.Errorf("%w: workflow delta omits new step %d", sessions.ErrCoordinationRequest, index)
		}
	}
	if prior != nil {
		header.Run, header.Acknowledged, header.CallID = prior.Run, prior.Acknowledged, prior.CallID
	}
	if header.Run == "" {
		run, err := r.currentRunRecords(tx)
		if err != nil {
			return err
		}
		header.Run = run.ID
	}
	// Header comparison normalizes the old full-report representation's
	// steps:null field without rewriting an otherwise identical header.
	data, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("write workflow header: %w", err)
	}
	previous, err := json.Marshal(prior)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, previous) {
		if err := tx.tx.Put(sessions.RecordKey{Kind: "workflow", ID: header.ID}, data); err != nil {
			return err
		}
	}
	indexes := make([]int, 0, len(delta.Steps))
	for index := range delta.Steps {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		step := delta.Steps[index]
		id, _ := workflowStepRecordID(header.ID, index)
		key := sessions.RecordKey{Kind: "workflow_step", ID: id}
		previous, found, err := tx.raw.Get(key)
		if err != nil {
			return err
		}
		if index < count && !found {
			return fmt.Errorf("%w: workflow history is missing step %d", sessions.ErrCoordinationFormat, index)
		}
		data, err := json.Marshal(step)
		if err != nil {
			return fmt.Errorf("write workflow step %d: %w", index, err)
		}
		if !found || !bytes.Equal(data, previous.Payload) {
			if err := tx.tx.Put(key, data); err != nil {
				return err
			}
		}
		// Recheck even an identical supplied step: another transaction may have
		// just completed its execution or delivered the task through parent mail.
		if err := recordWorkflowStepDelivery(tx, header.ID, step); err != nil {
			return err
		}
	}
	for index := delta.TotalSteps; index < count; index++ {
		id, _ := workflowStepRecordID(header.ID, index)
		if err := tx.Delete(sessions.RecordKey{Kind: "workflow_step", ID: id}); err != nil {
			return err
		}
	}
	if header.Status != "running" && (prior == nil || prior.Status == "running") {
		return r.recordWorkflowTerminal(tx, &header)
	}
	return nil
}

// workflowRecordForWrite retains number tokens in arbitrary values when a
// header is compared or a task is rewritten to add its delivery receipt. Display
// projections decode any fields as float64 and cannot be losslessly re-encoded.
func workflowRecordForWrite[T any](tx *recordTx, key sessions.RecordKey) (*T, error) {
	record, found, err := tx.raw.Get(key)
	if err != nil || !found {
		return nil, err
	}
	return decodeRecord[T](record, true)
}

func savedWorkflowHeader(tx *recordTx, id string) (*workflow.ReportHeader, error) {
	type storedHeader struct {
		workflow.ReportHeader
		Steps []json.RawMessage `json:"steps"`
	}
	saved, err := workflowRecordForWrite[storedHeader](tx, sessions.RecordKey{Kind: "workflow", ID: id})
	if err != nil || saved == nil {
		return nil, err
	}
	if len(saved.Steps) != 0 {
		return nil, fmt.Errorf("read swarm workflow %q: embedded steps: %w", id, ErrUnsupportedFormat)
	}
	return &saved.ReportHeader, nil
}

// The fallback requirement of a fixture without Requirement is reviewed or
// applied, never delivered; no owner lookup is needed for this predicate.
func workflowTaskDelivering(t *Task, e *Execution) bool {
	return t != nil && t.Status == "running" && t.Requirement == RequirementDelivered && !deliveredTask(t) &&
		e != nil && e.Status == "completed" && e.Member == t.Owner && e.Result != nil &&
		e.Result.Task == t.ID && e.Result.Execution == e.ID && e.Result.Revision == t.Revision
}

func recordWorkflowStepDelivery(tx *recordTx, workflowID string, step workflow.Step) error {
	result, ok := agentStepReceipt(step)
	if !ok {
		return nil
	}
	e, err := tx.Execution(result.Execution)
	if err != nil {
		return err
	}
	if e == nil || e.Status != "completed" || e.Workflow != workflowID || e.Member != result.Session || e.Request.CallID != step.ID || e.Result == nil {
		return nil
	}
	saved := e.Result
	if saved.Task != result.Task || saved.Session != result.Session || saved.Execution != result.Execution || saved.Revision != result.Revision {
		return nil
	}
	t, err := workflowRecordForWrite[Task](tx, sessions.RecordKey{Kind: "task", ID: result.Task})
	if err != nil {
		return err
	}
	if !workflowTaskDelivering(t, e) || t.Owner != result.Session || t.Execution != result.Execution || t.Revision != result.Revision {
		return nil
	}
	t.Delivery = &TaskDelivery{Via: "workflow_step", Ref: step.ID, Execution: result.Execution, Revision: result.Revision, Inline: true, At: time.Now().UTC()}
	t.Status = "done"
	return tx.PutTask(t)
}

// Only this workflow's execution/task dependencies can lose workflow control in
// this transition. Repair their suppressed notices, not unrelated task history.
// Work here is proportional to this workflow's agents, not its logged steps.
func (r *Runtime) recordWorkflowTerminal(tx *recordTx, header *workflow.ReportHeader) error {
	agents, unsettled := 0, 0
	delivered := make(map[string]bool)
	query := sessions.RecordQuery{Index: sessions.CoordinationIndexExecutionWorkflow, Key: header.ID, Limit: 100}
	for {
		page, err := tx.Executions(query)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			e := record.Value
			agents++
			if e.Status != "completed" {
				unsettled++
			}
			tasks := sessions.RecordQuery{Index: sessions.CoordinationIndexTaskExecution, Key: e.ID, Limit: 100}
			for {
				page, err := tx.Tasks(tasks)
				if err != nil {
					return err
				}
				for _, record := range page.Records {
					t := record.Value
					if t.Delivery != nil && t.Delivery.Via == "workflow_step" && strings.HasPrefix(t.Delivery.Ref, header.ID+"/") {
						delivered[t.ID] = true
					}
					if err := r.repairWorkflowTaskNotice(tx, header, t, e); err != nil {
						return err
					}
				}
				if page.Next == "" {
					break
				}
				tasks.After = page.Next
			}
		}
		if page.Next == "" {
			break
		}
		query.After = page.Next
	}
	name := clipInspection(header.Name, 512)
	if name == "" {
		name = header.ID
	}
	text := fmt.Sprintf("Workflow %s %s: %d agents", name+" ("+header.ID+")", header.Status, agents)
	if unsettled > 0 {
		text += fmt.Sprintf(", %d failed or paused", unsettled)
	}
	if header.Status == "completed" {
		text += ". Completed; the output accompanies this result. Inspect: swarm_read({view: \"workflows\", id: \"" + header.ID + "\"})."
	} else {
		text += ". Inspect: swarm_read({view: \"workflows\", id: \"" + header.ID + "\"}), then recover its unresolved work or report the failure and swarm_control({action: \"acknowledge_workflow\", id: \"" + header.ID + "\", defer: true, note: \"...\"}) to retain it."
		if len(delivered) > 0 {
			ids := make([]string, 0, len(delivered))
			for id := range delivered {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			text += ". Delivered to the script only: tasks " + strings.Join(ids, ", ")
		}
		if header.Error != nil {
			text += " Reason: " + clipInspection(header.Error.Code+": "+header.Error.Message, 1024)
		}
	}
	return tx.PutMail(&Mail{ID: ids.New(), From: header.ID, To: r.ID, Workflow: header.ID, Kind: "info", Text: text, Posted: time.Now().UTC()})
}

func (r *Runtime) repairWorkflowTaskNotice(tx *recordTx, header *workflow.ReportHeader, t *Task, e *Execution) error {
	if !workflowTaskDelivering(t, e) {
		return nil
	}
	if d := t.Deferral; d != nil && header.Acknowledged && (header.Status == "failed" || header.Status == "canceled" || header.Status == "interrupted") &&
		d.Workflow == header.ID && e.Run == header.Run && e.Run == t.Run && e.ID == d.Execution && e.Generation == d.Generation &&
		t.Owner == d.Owner && t.Revision == d.Revision && t.Status == d.Status && t.AcceptedRevision == d.AcceptedRevision {
		return nil
	}
	if exists, err := tx.lifecycleResultNotice(t); err != nil || exists {
		return err
	}
	owner, err := tx.Member(t.Owner)
	if err != nil || owner == nil {
		return err
	}
	// Deliver-only tasks do not need an editing snapshot's commit description.
	return tx.PutMail(r.completionNoticeValues(owner, e, t, ""))
}
