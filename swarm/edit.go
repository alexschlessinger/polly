package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// editBase marks a State handed to an edit: its maps are the callback's own,
// its records still belong to the shared state they came from.
type editBase struct {
	shared *State
	raw    map[string]map[string]json.RawMessage
}

// recordKind is one kind of record an edit can change, apart from workflow
// reports, which span several records.
type recordKind struct {
	kind string
	// changes adds to out the records work holds that shared does not, and
	// names in edited every shared record whose content no longer matches raw.
	changes func(shared, work *State, raw, out map[string]map[string]json.RawMessage, edited *[]string, confirm bool) error
	// adopt replaces work's record with a decode of data, or drops it.
	adopt func(work *State, id string, data json.RawMessage) error
}

func kindOf[T any](kind string, table func(*State) map[string]*T) recordKind {
	return recordKind{
		kind: kind,
		changes: func(shared, work *State, raw, out map[string]map[string]json.RawMessage, edited *[]string, confirm bool) error {
			before, after := table(shared), table(work)
			for id, value := range after {
				if previous, ok := before[id]; ok && previous == value {
					if !confirm {
						continue
					}
					var stored T
					if err := json.Unmarshal(raw[kind][id], &stored); err == nil && reflect.DeepEqual(&stored, value) {
						continue
					}
					*edited = append(*edited, kind+"/"+id)
				}
				data, err := json.Marshal(value)
				if err != nil {
					return err
				}
				if !bytes.Equal(data, raw[kind][id]) {
					setChange(out, kind, id, data)
				}
			}
			for id := range before {
				if _, ok := after[id]; !ok {
					setChange(out, kind, id, nil)
				}
			}
			return nil
		},
		adopt: func(work *State, id string, data json.RawMessage) error {
			if data == nil {
				delete(table(work), id)
				return nil
			}
			value := new(T)
			if err := json.Unmarshal(data, value); err != nil {
				return fmt.Errorf("read swarm %s: %w", kind, err)
			}
			table(work)[id] = value
			return nil
		},
	}
}

func setChange(out map[string]map[string]json.RawMessage, kind, id string, data json.RawMessage) {
	if out[kind] == nil {
		out[kind] = map[string]json.RawMessage{}
	}
	out[kind][id] = data
}

var recordKinds = []recordKind{
	kindOf("followup", func(s *State) map[string]*FollowupCall { return s.Followups }),
	kindOf("integration", func(s *State) map[string]*IntegrationCandidate { return s.Integrations }),
	kindOf("apply", func(s *State) map[string]*ApplyRecord { return s.Applies }),
	kindOf("parent_turn", func(s *State) map[string]*ParentTurn { return s.ParentTurns }),
	kindOf("parent", func(s *State) map[string]*ParentRecord { return s.Parents }),
	kindOf("run", func(s *State) map[string]*Run { return s.Runs }),
	kindOf("member", func(s *State) map[string]*Member { return s.Members }),
	kindOf("task", func(s *State) map[string]*Task { return s.Tasks }),
	kindOf("mail", func(s *State) map[string]*Mail { return s.Messages }),
	kindOf("publication", func(s *State) map[string]*Publication { return s.Publications }),
	kindOf("execution", func(s *State) map[string]*Execution { return s.Executions }),
	kindOf("context", func(s *State) map[string]*ExecutionContext { return s.Contexts }),
	kindOf("snapshot", func(s *State) map[string]*worktree.Snapshot { return s.Snapshots }),
	kindOf("preview", func(s *State) map[string]*worktree.Preview { return s.Previews }),
}

// editable returns a state whose maps the caller may add to and delete from.
// A record already in them is changed only through its own* method.
func (s *State) editable(raw map[string]map[string]json.RawMessage) *State {
	return &State{
		Followups: maps.Clone(s.Followups), Integrations: maps.Clone(s.Integrations), Applies: maps.Clone(s.Applies),
		ParentTurns: maps.Clone(s.ParentTurns), Parents: maps.Clone(s.Parents), Runs: maps.Clone(s.Runs),
		Members: maps.Clone(s.Members), Tasks: maps.Clone(s.Tasks), Messages: maps.Clone(s.Messages),
		Publications: maps.Clone(s.Publications), Executions: maps.Clone(s.Executions), Contexts: maps.Clone(s.Contexts),
		Snapshots: maps.Clone(s.Snapshots), Previews: maps.Clone(s.Previews), Workflows: maps.Clone(s.Workflows),
		Format: s.Format, editing: &editBase{shared: s, raw: raw},
	}
}

// own returns record id for a callback that is about to change it. Inside an
// edit that is a copy of the caller's own, put in the record's place; a state
// that is already private returns the record itself.
func own[T any](s *State, kind string, table func(*State) map[string]*T, id string) *T {
	records := table(s)
	value := records[id]
	if value == nil || s.editing == nil || table(s.editing.shared)[id] != value {
		return value
	}
	mine := new(T)
	if err := json.Unmarshal(s.editing.raw[kind][id], mine); err != nil {
		panic(fmt.Sprintf("swarm: stored %s %s no longer decodes: %v", kind, id, err))
	}
	records[id] = mine
	return mine
}

func (s *State) ownExecution(id string) *Execution {
	return own(s, "execution", func(s *State) map[string]*Execution { return s.Executions }, id)
}
func (s *State) ownMember(id string) *Member {
	return own(s, "member", func(s *State) map[string]*Member { return s.Members }, id)
}
func (s *State) ownTask(id string) *Task {
	return own(s, "task", func(s *State) map[string]*Task { return s.Tasks }, id)
}
func (s *State) ownMail(id string) *Mail {
	return own(s, "mail", func(s *State) map[string]*Mail { return s.Messages }, id)
}
func (s *State) ownRun(id string) *Run {
	return own(s, "run", func(s *State) map[string]*Run { return s.Runs }, id)
}
func (s *State) ownContext(id string) *ExecutionContext {
	return own(s, "context", func(s *State) map[string]*ExecutionContext { return s.Contexts }, id)
}
func (s *State) ownParentTurn(id string) *ParentTurn {
	return own(s, "parent_turn", func(s *State) map[string]*ParentTurn { return s.ParentTurns }, id)
}
func (s *State) ownIntegration(id string) *IntegrationCandidate {
	return own(s, "integration", func(s *State) map[string]*IntegrationCandidate { return s.Integrations }, id)
}
func (s *State) ownFollowup(id string) *FollowupCall {
	return own(s, "followup", func(s *State) map[string]*FollowupCall { return s.Followups }, id)
}
func (s *State) ownParent(id string) *ParentRecord {
	return own(s, "parent", func(s *State) map[string]*ParentRecord { return s.Parents }, id)
}
func (s *State) ownPublication(id string) *Publication {
	return own(s, "publication", func(s *State) map[string]*Publication { return s.Publications }, id)
}

// ownWorkflow is own for a report, which is stored as one record and one per
// step: the copy comes from the report itself.
func (s *State) ownWorkflow(id string) *workflow.Report {
	value := s.Workflows[id]
	if value == nil || s.editing == nil || s.editing.shared.Workflows[id] != value {
		return value
	}
	data, err := json.Marshal(value)
	mine := &workflow.Report{}
	if err == nil {
		err = json.Unmarshal(data, mine)
	}
	if err != nil {
		panic(fmt.Sprintf("swarm: workflow %s no longer encodes: %v", id, err))
	}
	s.Workflows[id] = mine
	return mine
}

// changes returns what an edit did to the shared state it started from, as
// records to write, and names the shared records a callback changed without
// taking them through own. Those are written too, so no change is lost, but
// the shared state can no longer be trusted. confirm asks for that check,
// which decodes every untouched record.
func (s *State) changes(confirm bool) (out map[string]map[string]json.RawMessage, edited []string, err error) {
	shared, raw := s.editing.shared, s.editing.raw
	out = map[string]map[string]json.RawMessage{}
	for _, kind := range recordKinds {
		if err := kind.changes(shared, s, raw, out, &edited, confirm); err != nil {
			return nil, nil, err
		}
	}
	for id, report := range s.Workflows {
		previous, ok := shared.Workflows[id]
		if ok && previous == report && !confirm {
			continue
		}
		headers, steps := detachWorkflowSteps(map[string]*workflow.Report{id: report})
		data, err := json.Marshal(headers[id])
		if err != nil {
			return nil, nil, err
		}
		differs := !bytes.Equal(data, raw["workflow"][id])
		if differs {
			setChange(out, "workflow", id, data)
		}
		for key, step := range steps {
			data, err := json.Marshal(step)
			if err != nil {
				return nil, nil, err
			}
			if !bytes.Equal(data, raw["workflow_step"][key]) {
				setChange(out, "workflow_step", key, data)
				differs = true
			}
		}
		if ok {
			for index := len(report.Steps); index < len(previous.Steps); index++ {
				setChange(out, "workflow_step", fmt.Sprintf("%s/%d", id, index), nil)
			}
		}
		if ok && previous == report && differs {
			edited = append(edited, "workflow/"+id)
		}
	}
	for id, previous := range shared.Workflows {
		if _, ok := s.Workflows[id]; ok {
			continue
		}
		setChange(out, "workflow", id, nil)
		for index := range previous.Steps {
			setChange(out, "workflow_step", fmt.Sprintf("%s/%d", id, index), nil)
		}
	}
	if s.Format != shared.Format {
		if s.Format == nil {
			setChange(out, formatKind, formatID, nil)
		} else {
			data, err := json.Marshal(s.Format)
			if err != nil {
				return nil, nil, err
			}
			if !bytes.Equal(data, raw[formatKind][formatID]) {
				setChange(out, formatKind, formatID, data)
			}
		}
	}
	sort.Strings(edited)
	return out, edited, nil
}

// edit is update for a callback that takes every record it changes through
// its own* method, and so pays for those records, not for the family. Adding
// and deleting records needs nothing more. Against a store that cannot write
// a set of changes, or behind a wrapper that hides that, it is update.
func (r *Runtime) edit(ctx context.Context, fn func(*State) error) error {
	if err := r.prepare(ctx); err != nil {
		return err
	}
	err := r.editOn(ctx, r.parent, func(_ *sessions.CoordinationState, s *State) error { return fn(s) })
	if err == nil {
		r.changed()
	}
	return err
}

// editOn is edit through any session of the family, for a callback that also
// sets what the transaction appends, pins or fences on raw. raw carries no
// Records. The callback runs at most once.
func (r *Runtime) editOn(ctx context.Context, session sessions.CoordinationSession, fn func(raw *sessions.CoordinationState, s *State) error) error {
	delta, ok := session.(sessions.CoordinationDeltaSession)
	// A family that keeps changing under an edit falls back to the update
	// below, which reads and writes inside one transaction.
	for attempt := 0; ok && attempt < 3; attempt++ {
		if _, err := r.State(ctx); err != nil {
			return err
		}
		shared, raw, revision := r.view.snapshot()
		if shared == nil || revision == 0 {
			break
		}
		var committed *sessions.CoordinationState
		var work *State
		var changed map[string]map[string]json.RawMessage
		var edited []string
		err := delta.UpdateCoordinationFrom(ctx, revision, func(state *sessions.CoordinationState) error {
			work = shared.editable(raw)
			if err := fn(state, work); err != nil {
				return err
			}
			var err error
			changed, edited, err = work.changes(confirmEdits(raw))
			if err != nil {
				return err
			}
			if len(edited) > 0 && failOnSharedChange {
				panic("swarm: the edit at " + editSite() + " changed records without own: " + strings.Join(edited, ", "))
			}
			committed, state.Changes = state, changed
			return nil
		})
		if errors.Is(err, sessions.ErrCoordinationStale) {
			continue
		}
		if err != nil {
			return err
		}
		if len(edited) > 0 {
			slog.Warn("swarm_edit_without_own", "site", editSite(), "records", strings.Join(edited, ", "))
			r.view.drop()
		} else {
			r.view.advance(revision, committed.Revision, work, changed)
		}
		return nil
	}
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

// editSite names the caller of the edit in progress, for its diagnostics.
func editSite() string {
	for skip := 2; skip < 40; skip++ {
		pc, file, line, ok := runtime.Caller(skip)
		if !ok {
			break
		}
		name := runtime.FuncForPC(pc).Name()
		if !strings.Contains(name, "pollytool/swarm.") || strings.Contains(name, ").editOn") || strings.HasSuffix(name, ").edit") {
			continue
		}
		return fmt.Sprintf("%s:%d", file[strings.LastIndex(file, "/")+1:], line)
	}
	return "an unknown caller"
}

// confirmEdits reports whether an edit of this family checks every record it
// left alone. Tests always do; a running process does while the family is
// small enough for that to cost what update would.
func confirmEdits(raw map[string]map[string]json.RawMessage) bool {
	if failOnSharedChange {
		return true
	}
	records := 0
	for _, group := range raw {
		records += len(group)
	}
	return records <= sharedCheckRecords
}

// snapshot returns the shared state with the records and revision it was
// decoded from.
func (c *StateCache) snapshot() (*State, map[string]map[string]json.RawMessage, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.raw, c.revision
}

// drop forgets the shared state, so the next read decodes the stored records.
func (c *StateCache) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw, c.revision, c.state = nil, 0, nil
}

// advance moves the shared state from revision known to committed by the
// changes an edit just wrote, so the next read needs no copy of the family.
// work is the edit's state: its maps become the shared ones, with each
// changed record decoded again from what was stored. If another reader moved
// the cache meanwhile, it already holds something newer.
func (c *StateCache) advance(known, committed uint64, work *State, changed map[string]map[string]json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil || c.revision != known || committed == 0 {
		return
	}
	if committed == known {
		return
	}
	fail := func() { c.raw, c.revision, c.state = nil, 0, nil }
	for kind, group := range changed {
		for id, data := range group {
			if data == nil {
				delete(c.raw[kind], id)
				continue
			}
			if c.raw[kind] == nil {
				c.raw[kind] = map[string]json.RawMessage{}
			}
			c.raw[kind][id] = data
		}
	}
	work.editing = nil
	for _, kind := range recordKinds {
		for id, data := range changed[kind.kind] {
			if err := kind.adopt(work, id, data); err != nil {
				fail()
				return
			}
		}
	}
	if data, ok := changed[formatKind][formatID]; ok {
		work.Format = nil
		if data != nil {
			work.Format = &FormatRecord{}
			if err := json.Unmarshal(data, work.Format); err != nil {
				fail()
				return
			}
		}
	}
	if len(changed["workflow"]) > 0 || len(changed["workflow_step"]) > 0 {
		raw := &sessions.CoordinationState{Records: c.raw}
		steps := map[string]*workflow.Step{}
		if err := errors.Join(
			decodeRecords(raw, "workflow", &work.Workflows, nil, nil),
			decodeRecords(raw, "workflow_step", &steps, nil, nil),
		); err != nil {
			fail()
			return
		}
		if err := attachWorkflowSteps(work.Workflows, steps); err != nil {
			fail()
			return
		}
	}
	c.revision, c.state = committed, work
}
