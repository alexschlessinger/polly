package sessions

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

// CoordinationCandidateSelector names an observation-only runtime predicate.
// Candidates are not authorization, leases, or permission to mutate a record.
type CoordinationCandidateSelector string

const (
	CandidateIntegrationForget       CoordinationCandidateSelector = "integration.forget"         // empty key; no successor, neither applied nor superseded
	CandidateWorkflowUnsettled       CoordinationCandidateSelector = "workflow.unsettled"         // empty key; running or not acknowledged
	CandidateMailSettlement          CoordinationCandidateSelector = "mail.settlement"            // recipient; unanswered request, undelivered reply or workflow notice
	CandidateWorkflowHostRunningStep CoordinationCandidateSelector = "workflow.host-running-step" // workflow ID from canonical record key, not step payload ID
	CandidateTaskNotice              CoordinationCandidateSelector = "task.notice"                // tuple(task, execution, canonical decimal revision); any mail kind/delivery state
	CandidateTaskOpenOwner           CoordinationCandidateSelector = "task.open-owner"            // current owner; not done/canceled
	CandidateApplyUnresolvedOwner    CoordinationCandidateSelector = "apply.unresolved-owner"     // current owner of any referenced task; applying/recovery_required
	CandidateDoneTaskTree            CoordinationCandidateSelector = "task.done-tree"             // tuple(snapshot source, tree); exact task.snapshot, not startingSnapshot
	CandidateWorkflowContextStep     CoordinationCandidateSelector = "workflow.context-step"      // tuple(workflow, context); completed context step with exact string value
	CandidateWorkflowRunning         CoordinationCandidateSelector = "workflow.running"           // empty key
	CandidateFollowupPreparing       CoordinationCandidateSelector = "followup.preparing"         // empty key; refresh=true, phase=preparing
	CandidateFollowupHeldScratch     CoordinationCandidateSelector = "followup.held-scratch"      // empty key; nonempty heldScratch in any phase
	CandidateTaskWorkflowDelivery    CoordinationCandidateSelector = "task.workflow-delivery"     // workflow ID; delivery.via=workflow_step, ref prefix workflow+"/"
)

// CoordinationCandidateQuery selects candidates in bytewise record ID order.
// After is an opaque root/selector/key-bound cursor, with live semantics across
// callbacks: concurrent writes can move records across the cursor. Limit must be
// 1..MaxCoordinationPageLimit; MaxBytes is a nonnegative payload-byte budget (zero
// means unlimited). A selected oversized first item is returned intact, alone.
// Next exposes all continuation; results are never silently truncated. Total is
// -1: finding candidates does not require counting the whole partition.
// Kind revisions conservatively invalidate these observations; cross-record
// selectors also depend on their referenced task/snapshot kinds. Native simple
// selectors seek directly into source indexes. ApplyUnresolvedOwner examines
// unresolved applies and their exact task references; DoneTaskTree sorts matching
// snapshot/task candidates; TaskWorkflowDelivery sorts matching delivery refs.
// Limit bounds returned items, not those cross-record or sorting visits.
type CoordinationCandidateQuery struct {
	Selector        CoordinationCandidateSelector
	Key, After      string
	Limit, MaxBytes int
}

// CoordinationCandidateReader is optional and callback-local. It exposes no
// mutation authority. A wrapper must explicitly forward it to retain indexing.
type CoordinationCandidateReader interface {
	QueryCoordinationCandidates(CoordinationCandidateQuery) (RecordPage, error)
}

type candidateSpec struct {
	kind  string
	parts []string
}

func candidateQuerySpec(q CoordinationCandidateQuery) (candidateSpec, error) {
	var spec candidateSpec
	tuple := 0
	empty := false
	switch q.Selector {
	case CandidateIntegrationForget:
		spec.kind, empty = "integration", true
	case CandidateWorkflowUnsettled, CandidateWorkflowRunning:
		spec.kind, empty = "workflow", true
	case CandidateMailSettlement:
		spec.kind = "mail"
	case CandidateTaskNotice:
		spec.kind, tuple = "mail", 3
	case CandidateWorkflowHostRunningStep:
		spec.kind = "workflow_step"
	case CandidateWorkflowContextStep:
		spec.kind, tuple = "workflow_step", 2
	case CandidateTaskOpenOwner, CandidateTaskWorkflowDelivery:
		spec.kind = "task"
	case CandidateDoneTaskTree:
		spec.kind, tuple = "task", 2
	case CandidateApplyUnresolvedOwner:
		spec.kind = "apply"
	case CandidateFollowupPreparing, CandidateFollowupHeldScratch:
		spec.kind, empty = "followup", true
	default:
		return spec, fmt.Errorf("%w: candidate selector %q", ErrCoordinationSelector, q.Selector)
	}
	if q.Limit < 1 || q.Limit > MaxCoordinationPageLimit || q.MaxBytes < 0 || empty && q.Key != "" {
		return spec, fmt.Errorf("%w: candidate key or page bounds", ErrCoordinationRequest)
	}
	spec.parts = []string{q.Key}
	if tuple != 0 {
		parts, err := coordindex.SplitKey(q.Key)
		if err != nil || len(parts) != tuple {
			return spec, fmt.Errorf("%w: candidate tuple key", ErrCoordinationRequest)
		}
		spec.parts = parts
	}
	if q.Selector == CandidateTaskNotice {
		n, err := strconv.ParseInt(spec.parts[2], 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != spec.parts[2] {
			return spec, fmt.Errorf("%w: candidate revision", ErrCoordinationRequest)
		}
	}
	return spec, nil
}

type candidateCursor struct {
	Version                 int
	Root, Selector, Key, ID []byte
}

func candidatePageCursor(r CoordinationReader, q CoordinationCandidateQuery) (candidateCursor, candidateSpec, error) {
	spec, err := candidateQuerySpec(q)
	if err != nil {
		return candidateCursor{}, spec, err
	}
	identity, err := r.Identity()
	if err != nil {
		return candidateCursor{}, spec, err
	}
	cursor := candidateCursor{Version: 1, Root: []byte(identity.ParentID), Selector: []byte(q.Selector), Key: []byte(q.Key)}
	if q.After != "" {
		data, err := base64.RawURLEncoding.DecodeString(q.After)
		var c candidateCursor
		if err != nil || json.Unmarshal(data, &c) != nil || c.Version != cursor.Version || !bytes.Equal(c.Root, cursor.Root) || !bytes.Equal(c.Selector, cursor.Selector) || !bytes.Equal(c.Key, cursor.Key) || len(c.ID) == 0 {
			return cursor, spec, ErrCoordinationCursor
		}
		cursor.ID = c.ID
	}
	return cursor, spec, nil
}

func candidatePageNext(c candidateCursor, id string) string {
	c.ID = []byte(id)
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

// QueryCoordinationCandidates uses ONLY the supplied callback-local capability.
// Without it, compatibility deliberately enumerates and filters the kind through
// that same reader, never reopening a store or acquiring a different snapshot.
func QueryCoordinationCandidates(r CoordinationReader, q CoordinationCandidateQuery) (RecordPage, error) {
	if indexed, ok := r.(CoordinationCandidateReader); ok {
		return indexed.QueryCoordinationCandidates(q)
	}
	cursor, spec, err := candidatePageCursor(r, q)
	if err != nil {
		return RecordPage{}, err
	}
	var all []CoordinationRecord
	query := RecordQuery{Index: CoordinationIndexKind, Key: spec.kind, Kind: spec.kind, Limit: MaxCoordinationPageLimit}
	for {
		page, err := r.Query(query)
		if err != nil {
			return RecordPage{}, err
		}
		all = append(all, page.Records...)
		if page.Next == "" {
			break
		}
		if page.Next == query.After {
			return RecordPage{}, ErrCoordinationCursor
		}
		query.After = page.Next
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key.ID < all[j].Key.ID })
	page := RecordPage{Total: -1}
	used := 0
	for _, record := range all {
		if len(cursor.ID) > 0 && record.Key.ID <= string(cursor.ID) {
			continue
		}
		selected, err := candidateMatches(r, q, spec, record)
		if err != nil {
			return RecordPage{}, err
		}
		if !selected {
			continue
		}
		if len(page.Records) == q.Limit || q.MaxBytes > 0 && len(page.Records) > 0 && len(record.Payload) > q.MaxBytes-used {
			page.Next = candidatePageNext(cursor, page.Records[len(page.Records)-1].Key.ID)
			break
		}
		if q.MaxBytes > 0 && len(record.Payload) > q.MaxBytes-used {
			page.Oversized = true
		}
		record.Payload = bytes.Clone(record.Payload)
		page.Records = append(page.Records, record)
		used += len(record.Payload)
	}
	return page, nil
}

// Decode only the selector's compact typed subset. Never round-trip a source
// payload through interface{} (which would coerce large integers to float64).
func candidateDecode(record CoordinationRecord, value any) error {
	if err := json.Unmarshal(record.Payload, value); err != nil {
		return fmt.Errorf("%w: candidate %s/%s: %v", ErrCoordinationFormat, record.Key.Kind, record.Key.ID, err)
	}
	return nil
}

func candidateMatches(r CoordinationReader, q CoordinationCandidateQuery, spec candidateSpec, record CoordinationRecord) (bool, error) {
	switch q.Selector {
	case CandidateIntegrationForget:
		var v struct{ Status, Successor string }
		err := candidateDecode(record, &v)
		return v.Successor == "" && v.Status != "applied" && v.Status != "superseded", err
	case CandidateWorkflowUnsettled:
		var v struct {
			Status       string
			Acknowledged bool
		}
		err := candidateDecode(record, &v)
		return v.Status == "running" || !v.Acknowledged, err
	case CandidateWorkflowRunning:
		var v struct{ Status string }
		err := candidateDecode(record, &v)
		return v.Status == "running", err
	case CandidateMailSettlement:
		var v struct {
			To, Kind, ReplyID, Workflow string
			Delivered                   bool
		}
		err := candidateDecode(record, &v)
		return v.To == q.Key && (v.Kind == "request" && v.ReplyID == "" || !v.Delivered && (v.Kind == "reply" || v.Workflow != "")), err
	case CandidateTaskNotice:
		var v struct {
			Task, Execution string
			Revision        int64
		}
		err := candidateDecode(record, &v)
		return v.Task == spec.parts[0] && v.Execution == spec.parts[1] && strconv.FormatInt(v.Revision, 10) == spec.parts[2], err
	case CandidateWorkflowHostRunningStep:
		var v struct{ Status, Kind string }
		err := candidateDecode(record, &v)
		workflow, _, _ := strings.Cut(record.Key.ID, "/")
		return workflow == q.Key && coordsem.WorkStepRunning(v.Kind, v.Status), err
	case CandidateWorkflowContextStep:
		var v struct {
			Status, Kind string
			Value        json.RawMessage
		}
		if err := candidateDecode(record, &v); err != nil {
			return false, err
		}
		workflow, _, _ := strings.Cut(record.Key.ID, "/")
		if workflow != spec.parts[0] || v.Status != "completed" || v.Kind != "context" {
			return false, nil
		}
		// Step.Value is any: a non-string is valid, but not a context identity.
		var value string
		return len(v.Value) > 0 && v.Value[0] == '"' && json.Unmarshal(v.Value, &value) == nil && value == spec.parts[1], nil
	case CandidateTaskOpenOwner:
		var v struct{ Status, Owner string }
		err := candidateDecode(record, &v)
		return v.Owner == q.Key && v.Status != "done" && v.Status != "canceled", err
	case CandidateApplyUnresolvedOwner:
		var v struct {
			Status string
			Tasks  []struct{ Task string }
		}
		if err := candidateDecode(record, &v); err != nil {
			return false, err
		}
		if !coordsem.UncertainApply(v.Status) {
			return false, nil
		}
		for _, ref := range v.Tasks {
			if ref.Task == "" {
				continue
			}
			task, found, err := r.Get(RecordKey{"task", ref.Task})
			if err != nil {
				return false, err
			}
			if !found {
				continue
			}
			var t struct{ Owner string }
			if err := candidateDecode(task, &t); err != nil {
				return false, err
			}
			if t.Owner == q.Key {
				return true, nil
			}
		}
		return false, nil
	case CandidateDoneTaskTree:
		var v struct{ Status, Snapshot string }
		if err := candidateDecode(record, &v); err != nil {
			return false, err
		}
		if v.Status != "done" || v.Snapshot == "" {
			return false, nil
		}
		snapshot, found, err := r.Get(RecordKey{"snapshot", v.Snapshot})
		if err != nil || !found {
			return false, err
		}
		var s struct{ Source, Tree string }
		err = candidateDecode(snapshot, &s)
		return s.Source == spec.parts[0] && s.Tree == spec.parts[1], err
	case CandidateFollowupPreparing:
		var v struct {
			Refresh bool
			Phase   string
		}
		err := candidateDecode(record, &v)
		return v.Refresh && v.Phase == "preparing", err
	case CandidateFollowupHeldScratch:
		var v struct{ HeldScratch string }
		err := candidateDecode(record, &v)
		return v.HeldScratch != "", err
	case CandidateTaskWorkflowDelivery:
		var v struct{ Delivery *struct{ Via, Ref string } }
		err := candidateDecode(record, &v)
		return v.Delivery != nil && v.Delivery.Via == "workflow_step" && strings.HasPrefix(v.Delivery.Ref, q.Key+"/"), err
	}
	return false, ErrCoordinationSelector
}
