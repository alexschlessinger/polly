package swarm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// InspectorQuery selects a bounded display page. Revision is an equality hint
// for precisely this selector, not a fence or mutation capability.
type InspectorQuery struct {
	Section, After, Revision string
	Limit, MaxBytes          int
}

// InspectorItem is an owned selected record and its exact display dependencies.
// None of these observations confer acceptance, control, or filesystem authority.
type InspectorItem struct {
	Kind, ID        string
	Decision        *DecisionItem
	Working         *WorkingItem
	Total           int
	Counts          [4]int // members, tasks, publications, workflows
	Member          *Member
	Execution       *Execution
	Context         *ExecutionContext
	Display         string
	Run             *Run
	Active, Pending int
	Task            *TaskView
	Deferred        bool
	Mail            *Mail
	Publication     *Publication
	Commit          string
	Workflow        *workflow.Report
	DeferredItems   int
	Integration     *IntegrationView
	Apply           *ApplyView
	Preview         *worktree.Preview
}
type InspectorPage struct {
	Section, Revision, Next string
	Unchanged, Oversized    bool
	Total                   int // record-section total before the cursor
	Items                   []InspectorItem
	Status                  DisplayStatus
	Raw                     *State // only for the explicit raw export
}
type inspectorCursor struct {
	Root, Section string
	Phase         int
	After         string
	Offset        int
	StatusLegacy  bool
}

func inspectorCursorText(c inspectorCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

// ReadInspectorPage observes the already supplied session. Unlike runtime
// operations, it never calls prepare, acquires a lease, or repairs durable state.
func (r *Runtime) ReadInspectorPage(ctx context.Context, q InspectorQuery) (InspectorPage, error) {
	access, err := sessions.OpenCoordinationAccess(r.parent)
	if err != nil {
		return InspectorPage{}, err
	}
	var out InspectorPage
	err = access.ReadRecords(ctx, func(raw sessions.CoordinationReader) error {
		rd := &recordReader{raw: raw, ctx: ctx, active: true}
		defer func() { rd.active = false }()
		var err error
		out, err = readInspectorPage(rd, r.ID, q, access.Capabilities().DurableRevisions)
		return err
	})
	return out, err
}

func readInspectorPage(rd *recordReader, actor string, q InspectorQuery, durable bool) (out InspectorPage, err error) {
	if q.Section == "" {
		q.Section = "members"
	}
	kind := map[string]string{"tasks": "task", "messages": "mail", "publications": "publication", "workflows": "workflow", "integrations": "integration", "previews": "preview"}[q.Section]
	if kind == "" && q.Section != "members" && q.Section != "raw" {
		return out, fmt.Errorf("unknown swarm section %q", q.Section)
	}
	if q.Limit == 0 {
		q.Limit = 32
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = 64 << 10
	}
	if q.Limit < 1 || q.Limit > sessions.MaxCoordinationPageLimit || q.MaxBytes < 0 {
		return out, sessions.ErrCoordinationRequest
	}
	identity, err := rd.Identity()
	if err != nil {
		return out, err
	}
	cursor := inspectorCursor{Root: identity.ParentID, Section: q.Section}
	if q.After != "" {
		data, e := base64.RawURLEncoding.DecodeString(q.After)
		var c inspectorCursor
		if e != nil || json.Unmarshal(data, &c) != nil || c.Root != cursor.Root || c.Section != cursor.Section || c.Phase < 0 || c.Phase > 3 || c.Offset < 0 {
			return out, sessions.ErrCoordinationCursor
		}
		cursor = c
	}
	out.Section = q.Section
	if durable {
		revisions, e := rd.Revisions([]sessions.RevisionScope{{Index: sessions.CoordinationIndexRecords}})
		if e != nil {
			return out, e
		}
		if len(revisions) != 1 {
			return out, sessions.ErrCoordinationRequest
		}
		out.Revision = fmt.Sprintf("%s/%s/%s/%d/%d", revisions[0].Token, q.Section, q.After, q.Limit, q.MaxBytes)
		if out.Revision == q.Revision {
			out.Unchanged = true
			return out, nil
		}
	}
	if err = rd.checkFormatFor(true); err != nil {
		return out, err
	}
	if rd.journalOnly {
		full, e := rd.SlowSnapshot()
		if e != nil {
			return out, e
		}
		if q.Section == "raw" {
			out.Raw = full
		}
		if q.Section == "members" {
			out.Status = legacyDisplayStatus(full, actor)
			out.Items = []InspectorItem{{Kind: "parent"}, {Kind: "counts"}, {Kind: "no_members"}, {Kind: "footer"}}
		}
		return out, nil
	}
	statusReader, native := rd.raw.(sessions.CoordinationStatusReader)
	factsReader, hasFacts := rd.raw.(sessions.CoordinationInspectorReader)
	_, hasIDs := rd.raw.(sessions.CoordinationIDReader)
	var full *State
	var header sessions.SemanticStatusHeader
	if native && hasFacts && hasIDs {
		if q.Section == "members" {
			header, err = statusReader.SemanticStatus(actor, false)
			if err != nil {
				return out, err
			}
			native = !header.ColdCompatibility
		}
	} else {
		native = false
	}
	if !native || q.Section == "raw" {
		full, err = rd.SlowSnapshot()
		if err != nil {
			return out, err
		}
	}
	if q.Section == "raw" {
		out.Raw = full
		return out, nil
	}
	source := inspectorSource{rd: rd, full: full, facts: factsReader}
	if q.Section != "members" {
		page, e := sessions.QueryRecordsByID(rd.raw, sessions.CoordinationIDQuery{Kind: kind, After: cursor.After, Limit: q.Limit, MaxBytes: q.MaxBytes})
		if e != nil {
			return out, e
		}
		for _, record := range page.Records {
			item, e := source.item(record)
			if e != nil {
				return out, e
			}
			out.Items = append(out.Items, item)
		}
		out.Oversized, out.Total = page.Oversized, page.Total
		if page.Next != "" {
			cursor.After = page.Next
			out.Next = inspectorCursorText(cursor)
		}
		return out, nil
	}
	var presentation Presentation
	if full != nil {
		presentation = Present(full, actor, actor)
		out.Status = legacyDisplayStatus(full, actor)
	} else {
		out.Status = DisplayStatus{Counts: Counts(header.Counts), HasRuns: header.HasRuns, ParentDisposition: header.ParentDisposition}
		turn, e := getInspectorRecord[ParentTurn](rd, "parent_turn", actor)
		if e != nil {
			return out, e
		}
		if turn != nil {
			out.Status.RetryPause = turn.RetryPause
		}
	}
	if cursor.Phase == 0 && cursor.After == "" && cursor.Offset == 0 {
		out.Items = append(out.Items, InspectorItem{Kind: "parent"})
	}
	remaining, room := q.Limit, q.MaxBytes
	for cursor.Phase < 4 {
		used, bytes, next := 0, 0, ""
		switch cursor.Phase {
		case 0, 1:
			section := "decisions"
			if cursor.Phase == 1 {
				section = "working"
			}
			var page sessions.SemanticStatusPage
			if full == nil {
				query := sessions.SemanticStatusQuery{Actor: actor, Section: section, After: cursor.After, Limit: remaining, MaxBytes: room}
				if cursor.StatusLegacy {
					query.After = ""
				}
				if query.After == "" {
					query.Offset = cursor.Offset
				}
				page, err = statusReader.SemanticStatusPage(query)
				if err != nil {
					return out, err
				}
			} else {
				if cursor.Phase == 0 {
					page.Total = len(presentation.Decisions)
					for _, d := range presentation.Decisions[min(cursor.Offset, page.Total):] {
						data, _ := json.Marshal(d)
						if used == remaining || used > 0 && len(data) > room-bytes {
							break
						}
						page.Decisions = append(page.Decisions, sessions.SemanticDecision(d))
						used++
						bytes += len(data)
					}
				} else {
					page.Total = len(presentation.Working)
					for _, w := range presentation.Working[min(cursor.Offset, page.Total):] {
						data, _ := json.Marshal(w)
						if used == remaining || used > 0 && len(data) > room-bytes {
							break
						}
						page.Working = append(page.Working, sessions.SemanticWorking(w))
						used++
						bytes += len(data)
					}
				}
				if cursor.Offset+used < page.Total {
					page.Next = "legacy"
				}
			}
			if cursor.Offset == 0 && page.Total > 0 {
				out.Items = append(out.Items, InspectorItem{Kind: section + "_heading", Total: page.Total})
			}
			used, bytes = 0, 0
			for _, d := range page.Decisions {
				v := DecisionItem(d)
				out.Items = append(out.Items, InspectorItem{Kind: "decision", Decision: &v})
				data, _ := json.Marshal(d)
				used++
				bytes += len(data)
			}
			for _, w := range page.Working {
				v := WorkingItem(w)
				out.Items = append(out.Items, InspectorItem{Kind: "working", Working: &v})
				data, _ := json.Marshal(w)
				used++
				bytes += len(data)
			}
			next = page.Next
			cursor.StatusLegacy = full != nil
			if next == "" && page.Total > 0 {
				out.Items = append(out.Items, InspectorItem{Kind: "space"})
			}
			out.Oversized = out.Oversized || page.Oversized || bytes > room
		case 2, 3:
			kind := "run"
			if cursor.Phase == 3 {
				kind = "member"
			}
			if cursor.Phase == 2 && cursor.After == "" {
				item := InspectorItem{Kind: "counts"}
				for i, k := range []string{"member", "task", "publication", "workflow"} {
					p, e := rd.raw.Query(sessions.RecordQuery{Index: sessions.CoordinationIndexKind, Key: k, Kind: k, CountOnly: true, IncludeTotal: true})
					if e != nil {
						return out, e
					}
					item.Counts[i] = p.Total
				}
				out.Items = append(out.Items, item)
			}
			page, e := sessions.QueryRecordsByID(rd.raw, sessions.CoordinationIDQuery{Kind: kind, After: cursor.After, Limit: remaining, MaxBytes: room})
			if e != nil {
				return out, e
			}
			for _, record := range page.Records {
				item, e := source.item(record)
				if e != nil {
					return out, e
				}
				out.Items = append(out.Items, item)
				used++
				bytes += len(record.Payload)
			}
			if cursor.Phase == 3 && cursor.After == "" && page.Total == 0 {
				out.Items = append(out.Items, InspectorItem{Kind: "no_members"})
			}
			next = page.Next
			out.Oversized = out.Oversized || page.Oversized
		}
		remaining -= used
		room -= bytes
		if next != "" {
			cursor.After = next
			cursor.Offset += used
			out.Next = inspectorCursorText(cursor)
			return out, nil
		}
		cursor.Phase++
		cursor.After = ""
		cursor.Offset = 0
		cursor.StatusLegacy = false
		if cursor.Phase < 4 && (remaining <= 0 || room <= 0) {
			out.Next = inspectorCursorText(cursor)
			return out, nil
		}
	}
	out.Items = append(out.Items, InspectorItem{Kind: "footer"})
	return out, nil
}

type inspectorSource struct {
	rd    *recordReader
	full  *State
	facts sessions.CoordinationInspectorReader
}

func getInspectorRecord[T any](rd *recordReader, kind, id string) (*T, error) {
	if id == "" {
		return nil, nil
	}
	return getRecord[T](rd, kind, id)
}
func (s inspectorSource) snapshot(id string) (*worktree.Snapshot, error) {
	return getInspectorRecord[worktree.Snapshot](s.rd, "snapshot", id)
}
func (s inspectorSource) commit(id string) (string, error) {
	return s.rd.lifecycleSnapshotCommit(id)
}
func (s inspectorSource) factsFor(key sessions.RecordKey) (sessions.CoordinationInspectorFacts, error) {
	if s.full == nil {
		return s.facts.InspectorFacts(key)
	}
	var out sessions.CoordinationInspectorFacts
	switch key.Kind {
	case "task":
		t := s.full.Tasks[key.ID]
		out.Display = TaskStatusIn(s.full, t)
		out.Deferred = TaskDeferred(s.full, t)
	case "member":
		out.Display = MemberState(s.full, s.full.Members[key.ID]).Display
	case "workflow":
		out.DeferredItems = DeferredCount(s.full, key.ID)
	case "run":
		for _, e := range s.full.Executions {
			if e.Run == key.ID && (e.Status == "queued" || e.Status == "running" || e.Status == "waiting") {
				out.Active++
			}
		}
		for _, t := range s.full.Tasks {
			if t.Run == key.ID && t.Status != "done" && t.Status != "canceled" {
				out.Pending++
			}
		}
	}
	return out, nil
}
func (s inspectorSource) item(record sessions.CoordinationRecord) (out InspectorItem, err error) {
	out.Kind, out.ID = record.Key.Kind, record.Key.ID
	runtimemetrics.Add(s.rd.ctx, runtimemetrics.RecordsDecoded, 1)
	switch record.Key.Kind {
	case "task":
		t, e := decodeRecord[Task](record, false)
		if e != nil {
			return out, e
		}
		facts, e := s.factsFor(record.Key)
		if e != nil {
			return out, e
		}
		// Resolve only the captures printed by this section. Do not invoke a
		// whole-State semantic helper with a partial map.
		base, e := s.commit(t.StartingSnapshot)
		if e != nil {
			return out, e
		}
		result, e := s.commit(t.Snapshot)
		if e != nil {
			return out, e
		}
		out.Task = &TaskView{ID: t.ID, Description: t.Description, Criteria: t.Criteria, Owner: t.Owner, Revision: t.Revision, Requirement: t.Requirement, Delivery: t.Delivery, Deferral: t.Deferral, AcceptedRevision: t.AcceptedRevision, Dependencies: t.Dependencies, Feedback: t.Feedback, BaseCommit: base, ResultCommit: result, Result: t.Result}
		out.Display, out.Deferred = facts.Display, facts.Deferred
	case "mail":
		out.Mail, err = decodeRecord[Mail](record, false)
	case "publication":
		out.Publication, err = decodeRecord[Publication](record, false)
		if err == nil {
			out.Commit, err = s.commit(out.Publication.Snapshot)
		}
	case "preview":
		out.Preview, err = decodeRecord[worktree.Preview](record, false)
	case "member":
		out.Member, err = decodeRecord[Member](record, false)
		if err != nil {
			return out, err
		}
		out.Execution, err = getInspectorRecord[Execution](s.rd, "execution", out.Member.Execution)
		if err != nil {
			return out, err
		}
		out.Context, err = getInspectorRecord[ExecutionContext](s.rd, "context", out.Member.Context)
		if err != nil {
			return out, err
		}
		facts, e := s.factsFor(record.Key)
		out.Display = facts.Display
		err = e
	case "run":
		out.Run, err = decodeRecord[Run](record, false)
		if err != nil {
			return out, err
		}
		facts, e := s.factsFor(record.Key)
		out.Active, out.Pending = facts.Active, facts.Pending
		err = e
	case "workflow":
		header, e := decodeRecord[workflow.ReportHeader](record, false)
		if e != nil {
			return out, e
		}
		out.Workflow = &workflow.Report{ID: header.ID, Name: header.Name, Run: header.Run, CallID: header.CallID, Acknowledged: header.Acknowledged, Source: header.Source, Input: header.Input, Output: header.Output, Status: header.Status, Error: header.Error, Started: header.Started, Finished: header.Finished}
		query := sessions.RecordQuery{Index: sessions.CoordinationIndexWorkflowSteps, Key: header.ID, Limit: sessions.MaxCoordinationPageLimit}
		for {
			page, e := s.rd.WorkflowSteps(query)
			if e != nil {
				return out, e
			}
			for _, step := range page.Records {
				expected, e := workflowStepRecordID(header.ID, len(out.Workflow.Steps))
				if e != nil {
					return out, e
				}
				if step.Key.ID != expected {
					return out, fmt.Errorf("%w: workflow step history is not contiguous", sessions.ErrCoordinationFormat)
				}
				out.Workflow.Steps = append(out.Workflow.Steps, *step.Value)
			}
			if page.Next == "" {
				break
			}
			if page.Next == query.After {
				return out, sessions.ErrCoordinationCursor
			}
			query.After = page.Next
		}
		if header.Acknowledged {
			facts, e := s.factsFor(record.Key)
			out.DeferredItems = facts.DeferredItems
			err = e
		}
	case "integration":
		c, e := decodeRecord[IntegrationCandidate](record, false)
		if e != nil {
			return out, e
		}
		out.Integration, out.Apply, err = s.integration(c)
	default:
		err = fmt.Errorf("unknown inspector record %q", record.Key.Kind)
	}
	return out, err
}

// Captured commit validation is intentionally shared with the public views,
// while each lookup is explicit and observation-only.
func (s inspectorSource) captured(c worktree.Snapshot) (CommitView, error) {
	saved, err := s.snapshot(c.ID)
	if err != nil {
		return CommitView{}, err
	}
	if !validTaskSnapshot(saved, c.ID) || !sameCapturedCommit(saved, &c) {
		return CommitView{}, nil
	}
	return CommitView{Commit: c.Commit, Tree: c.Tree, Source: c.Source}, nil
}
func (s inspectorSource) integration(c *IntegrationCandidate) (*IntegrationView, *ApplyView, error) {
	out := &IntegrationView{ID: c.ID, Status: c.Status, Drift: c.Drift, Accepted: c.Accepted, Predecessor: c.Predecessor, Successor: c.Successor}
	var err error
	out.Merged, err = s.captured(c.Merged)
	if err != nil {
		return nil, nil, err
	}
	for i, inputs := range [][]IntegrationInput{c.Inputs, c.Repairs, c.Pending} {
		var values []IntegrationInputView
		if inputs != nil {
			values = make([]IntegrationInputView, len(inputs))
		}
		for j, input := range inputs {
			v := IntegrationInputView{TaskReference: input.TaskReference}
			v.Base, err = s.captured(input.Base)
			if err != nil {
				return nil, nil, err
			}
			v.Submitted, err = s.captured(input.Submitted)
			if err != nil {
				return nil, nil, err
			}
			values[j] = v
		}
		switch i {
		case 0:
			out.Inputs = values
		case 1:
			out.Repairs = values
		case 2:
			out.Pending = values
		}
	}
	if c.Conflicts != nil {
		out.Conflicts = make([]ConflictView, len(c.Conflicts))
	}
	for i, f := range c.Conflicts {
		v := ConflictView{Type: f.Type, Message: f.Message, Paths: slices.Clone(f.Paths), Entries: slices.Clone(f.Entries)}
		v.Base, err = s.captured(f.Base)
		if err != nil {
			return nil, nil, err
		}
		v.Ours, err = s.captured(f.Ours)
		if err != nil {
			return nil, nil, err
		}
		v.Theirs, err = s.captured(f.Theirs)
		if err != nil {
			return nil, nil, err
		}
		out.Conflicts[i] = v
	}
	a, err := getInspectorRecord[ApplyRecord](s.rd, "apply", c.ID)
	if err != nil || a == nil {
		return out, nil, err
	}
	receipt := &ApplyView{Status: a.Status, Error: a.Error}
	receipt.ObservedParent, err = s.captured(a.ObservedParent)
	return out, receipt, err
}
