package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// editBase marks a State handed to an edit. Its maps are the family's own:
// the callback's changes are made in them, and taken back if the edit fails.
// Its records are shared with every reader until the callback takes one
// through own.
type editBase struct {
	raw map[string]map[string]json.RawMessage
	// prior holds, by kind, what each record the callback took through own,
	// put or drop was when the edit began, nil for one it added, and undo
	// puts each back. Its keys name the only records an edit looks at when
	// it finishes.
	prior map[string]map[string]any
	undo  []func()
	// format is the format record the edit began with.
	format *FormatRecord
	// cloned names the kinds whose map no reader holds, so a write needs no
	// copy. It outlives the edit: see StateCache.lend.
	cloned map[string]bool
}

// has reports whether the edit already took record id of kind.
func (b *editBase) has(kind, id string) bool {
	_, ok := b.prior[kind][id]
	return ok
}

// writable makes table, s's map for kind, safe to write: a map some reader
// may still hold is copied before the first write to it.
func writable[T any](s *State, kind string, table *map[string]*T) {
	if s.editing == nil || s.editing.cloned[kind] {
		return
	}
	s.editing.cloned[kind] = true
	if *table == nil {
		*table = map[string]*T{}
		return
	}
	*table = maps.Clone(*table)
}

// remember notes what record id was before an edit first writes it.
func remember[T any](s *State, kind string, table *map[string]*T, id string) {
	b := s.editing
	if b == nil || b.has(kind, id) {
		return
	}
	old, had := (*table)[id]
	if b.prior == nil {
		b.prior = map[string]map[string]any{}
	}
	if b.prior[kind] == nil {
		b.prior[kind] = map[string]any{}
	}
	if had {
		b.prior[kind][id] = old
	} else {
		b.prior[kind][id] = nil
	}
	b.undo = append(b.undo, func() {
		if had {
			(*table)[id] = old
		} else {
			delete(*table, id)
		}
	})
}

// revert takes back everything an edit's callback did.
func (s *State) revert() {
	for i := len(s.editing.undo) - 1; i >= 0; i-- {
		s.editing.undo[i]()
	}
	s.Format = s.editing.format
}

// put adds or replaces a record. table is s's map for kind. Every write to a
// State's maps goes through put, drop or an own method: inside an edit the
// maps are the family's, and a write has to be noted and may need a copy.
func put[T any](s *State, kind string, table *map[string]*T, id string, value *T) {
	writable(s, kind, table)
	remember(s, kind, table, id)
	(*table)[id] = value
}

// drop deletes a record, as put adds one.
func drop[T any](s *State, kind string, table *map[string]*T, id string) {
	writable(s, kind, table)
	remember(s, kind, table, id)
	delete(*table, id)
}

// recordKind is one kind of record an edit can change, apart from workflow
// reports, which span several records.
type recordKind struct {
	kind string
	// touched adds to out what an edit did to the one record it says it touched.
	touched func(work *State, raw, out map[string]map[string]json.RawMessage, id string) error
	// adopt replaces work's record with a decode of data, or drops it.
	adopt func(work *State, id string, data json.RawMessage) error
}

func kindOf[T any](kind string, table func(*State) map[string]*T) recordKind {
	return recordKind{
		kind: kind,
		touched: func(work *State, raw, out map[string]map[string]json.RawMessage, id string) error {
			value, ok := table(work)[id]
			if !ok {
				if _, stored := raw[kind][id]; stored {
					setChange(out, kind, id, nil)
				}
				return nil
			}
			_, err := encodeChange(raw, out, kind, id, value)
			return err
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

// encodeChange adds value to out as record kind/id unless raw already stores
// those bytes, and reports whether it did.
func encodeChange(raw, out map[string]map[string]json.RawMessage, kind, id string, value any) (bool, error) {
	data, err := json.Marshal(value)
	if err != nil || bytes.Equal(data, raw[kind][id]) {
		return false, err
	}
	setChange(out, kind, id, data)
	return true, nil
}

// workflowStepKey is the record ID of a report's step.
func workflowStepKey(id string, index int) string { return fmt.Sprintf("%s/%d", id, index) }

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

// editable returns a state for an edit's callback, which changes it through
// put, drop and the own methods. Those write s's own maps, copying one first
// unless cloned says no reader holds it.
func (s *State) editable(raw map[string]map[string]json.RawMessage, cloned map[string]bool) *State {
	if cloned == nil {
		cloned = map[string]bool{}
	}
	work := *s
	work.editing = &editBase{raw: raw, format: s.Format, cloned: cloned}
	return &work
}

// own returns record id for a callback that is about to change it. Inside an
// edit that is a copy of the caller's own, put in the record's place; a state
// that is already private returns the record itself.
func own[T any](s *State, kind string, table *map[string]*T, id string) *T {
	value := (*table)[id]
	if value == nil || s.editing == nil || s.editing.has(kind, id) {
		// A record the callback already owns or added may change again.
		return value
	}
	mine := new(T)
	if err := json.Unmarshal(s.editing.raw[kind][id], mine); err != nil {
		panic(fmt.Sprintf("swarm: stored %s %s no longer decodes: %v", kind, id, err))
	}
	put(s, kind, table, id, mine)
	return mine
}

func (s *State) ownExecution(id string) *Execution { return own(s, "execution", &s.Executions, id) }
func (s *State) ownMember(id string) *Member       { return own(s, "member", &s.Members, id) }
func (s *State) ownTask(id string) *Task           { return own(s, "task", &s.Tasks, id) }
func (s *State) ownMail(id string) *Mail           { return own(s, "mail", &s.Messages, id) }
func (s *State) ownRun(id string) *Run             { return own(s, "run", &s.Runs, id) }
func (s *State) ownContext(id string) *ExecutionContext {
	return own(s, "context", &s.Contexts, id)
}
func (s *State) ownIntegration(id string) *IntegrationCandidate {
	return own(s, "integration", &s.Integrations, id)
}
func (s *State) ownFollowup(id string) *FollowupCall { return own(s, "followup", &s.Followups, id) }
func (s *State) ownParent(id string) *ParentRecord   { return own(s, "parent", &s.Parents, id) }
func (s *State) ownPublication(id string) *Publication {
	return own(s, "publication", &s.Publications, id)
}

// ownWorkflow is own for a report, which is stored as one record and one per
// step: the copy comes from the report itself.
func (s *State) ownWorkflow(id string) *workflow.Report {
	value := s.Workflows[id]
	if value == nil || s.editing == nil || s.editing.has("workflow", id) {
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
	put(s, "workflow", &s.Workflows, id, mine)
	return mine
}

// workflowChanges adds to out what an edit did to one report, which is stored
// as a record of its own and one per step, and reports whether it differs
// from the stored one. previous is the report as the edit began, nil for one
// it added.
func (s *State) workflowChanges(out map[string]map[string]json.RawMessage, id string, previous *workflow.Report) (differs bool, err error) {
	raw, was := s.editing.raw, previous != nil
	report, ok := s.Workflows[id]
	if !ok {
		if was {
			setChange(out, "workflow", id, nil)
			for index := range previous.Steps {
				setChange(out, "workflow_step", workflowStepKey(id, index), nil)
			}
		}
		return was, nil
	}
	headers, steps := detachWorkflowSteps(map[string]*workflow.Report{id: report})
	if differs, err = encodeChange(raw, out, "workflow", id, headers[id]); err != nil {
		return false, err
	}
	for key, step := range steps {
		changed, err := encodeChange(raw, out, "workflow_step", key, step)
		if err != nil {
			return false, err
		}
		differs = differs || changed
	}
	if was {
		for index := len(report.Steps); index < len(previous.Steps); index++ {
			setChange(out, "workflow_step", workflowStepKey(id, index), nil)
		}
	}
	return differs, nil
}

// formatChanges adds the format record to out when an edit replaced it.
func (s *State) formatChanges(out map[string]map[string]json.RawMessage) error {
	if s.Format == s.editing.format {
		return nil
	}
	if s.Format == nil {
		setChange(out, formatKind, formatID, nil)
		return nil
	}
	_, err := encodeChange(s.editing.raw, out, formatKind, formatID, s.Format)
	return err
}

var kindsByName = func() map[string]recordKind {
	out := map[string]recordKind{}
	for _, kind := range recordKinds {
		out[kind.kind] = kind
	}
	return out
}()

// changes returns what an edit did to the shared state it started from, as
// records to write: those its callback took through own, put or drop.
func (s *State) changes() (out map[string]map[string]json.RawMessage, err error) {
	raw := s.editing.raw
	out = map[string]map[string]json.RawMessage{}
	for name, ids := range s.editing.prior {
		if name == "workflow" {
			for id, was := range ids {
				previous, _ := was.(*workflow.Report)
				if _, err := s.workflowChanges(out, id, previous); err != nil {
					return nil, err
				}
			}
			continue
		}
		kind, ok := kindsByName[name]
		if !ok {
			return nil, fmt.Errorf("swarm: no record kind %q", name)
		}
		for id := range ids {
			if err := kind.touched(s, raw, out, id); err != nil {
				return nil, err
			}
		}
	}
	if err := s.formatChanges(out); err != nil {
		return nil, err
	}
	return out, nil
}

// edit is update for a callback that takes every record it changes through
// its own* method, and so pays for those records, not for the family. Adding
// and deleting records needs nothing more. Against a store that cannot write
// a set of changes, or behind a wrapper that hides that, it is update. The
// state fn is handed becomes the family's own, which later edits write: fn
// must not keep it or a map of it, and a caller that wants the result reads.
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

// editLive is edit for a change to one execution's record that says what its
// member is doing at the moment, such as waiting for a slot. Such a change
// is for whoever watches this runtime. It is volatile: the store keeps it in
// memory and writes it with the record's next change, so the stored record
// says what the member was doing when it last saved something. The record
// gets status unless it has it already or allowed, when given, says no. That
// a member is running is said by its slice's first request, which is an edit
// already: see bindCheckpoint.
func (r *Runtime) editLive(ctx context.Context, execution, status string, allowed func(*Execution) bool) error {
	if err := r.prepare(ctx); err != nil {
		return err
	}
	err := r.editOn(ctx, r.parent, func(raw *sessions.CoordinationState, s *State) error {
		e := s.Executions[execution]
		if e == nil || e.Status == status || allowed != nil && !allowed(e) {
			return nil
		}
		s.ownExecution(execution).Status = status
		volatile(raw, "execution", execution)
		return nil
	})
	if err == nil {
		r.changed()
	}
	return err
}

// volatile marks the change an edit makes to record id of kind as one the
// store need not write: see sessions.CoordinationState.Volatile. The edit
// must change nothing in the record that has to outlive the process.
func volatile(raw *sessions.CoordinationState, kind, id string) {
	if raw.Volatile == nil {
		raw.Volatile = map[string]map[string]bool{}
	}
	if raw.Volatile[kind] == nil {
		raw.Volatile[kind] = map[string]bool{}
	}
	raw.Volatile[kind][id] = true
}

// editOn is edit through any session of the family, for a callback that also
// sets what the transaction appends, pins or fences on raw. raw carries no
// Records. The callback runs at most once.
//
// An edit that returns nil is in the database. The store may answer it from
// memory and write it a moment later, together with the edits made
// meanwhile; the caller is held until then, so what it does next never rests
// on a change that a crash of the process could take back. Readers and the
// members the edit pokes do not wait: the family in memory has the change as
// soon as the store accepts it.
func (r *Runtime) editOn(ctx context.Context, session sessions.CoordinationSession, fn func(raw *sessions.CoordinationState, s *State) error) error {
	written, err := r.editAhead(ctx, session, fn)
	if err != nil || written == nil {
		return err
	}
	// The wait is not the caller's to cancel: the edit has happened, and an
	// error here would tell the caller it had not.
	if err := written(context.WithoutCancel(ctx)); err != nil {
		// The store cannot write the edit. The family in memory has it all
		// the same, so whoever waits on a change still hears of this one.
		r.changed()
		return err
	}
	return nil
}

// editAhead is editOn without the wait: it returns once the store has
// accepted the edit, with the wait for its write, nil for an edit already
// written. The store writes edits in the order it accepted them, so a caller
// may go on without waiting when nothing it does rests on this edit before a
// later edit of its own is written.
func (r *Runtime) editAhead(ctx context.Context, session sessions.CoordinationSession, fn func(raw *sessions.CoordinationState, s *State) error) (written func(context.Context) error, err error) {
	poke, written, err := r.editFamily(ctx, session, fn)
	// The pokes an edit owes are sent after it lets go of the family's state.
	if poke != nil {
		poke()
	}
	return written, err
}

// editBatch is how many edits run under one hold of the family's state. It
// bounds how long a reader waits for the state, and how long the caller that
// runs the edits waits for its own to return.
const editBatch = 32

// editCall is one edit and what it came to.
type editCall struct {
	ctx     context.Context
	session sessions.CoordinationSession
	delta   sessions.CoordinationDeltaSession
	fn      func(raw *sessions.CoordinationState, s *State) error
	// site names the edit's caller for a test's diagnostics.
	site string

	poke    func()
	written func(context.Context) error
	err     error
	// panicked is what the edit panicked with when another caller ran it.
	panicked any
	// turn tells a caller that waited what became of its edit: true once
	// another caller has run it, false when this caller is to run it and the
	// edits behind it.
	turn chan bool
}

// editQueue is the edits waiting for the family's state. One caller at a
// time leads: it runs its own edit and then the ones that queued up behind
// it, so the state passes from edit to edit without waking a caller for each.
type editQueue struct {
	mu      sync.Mutex
	led     bool
	waiting []*editCall
}

// join makes call's caller the lead when nobody leads, and otherwise queues
// call behind the edits already waiting.
func (q *editQueue) join(call *editCall) (lead bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.led {
		q.led = true
		return true
	}
	call.turn = make(chan bool, 1)
	q.waiting = append(q.waiting, call)
	return false
}

// take removes up to n of the edits that have waited longest, for the lead.
func (q *editQueue) take(n int) []*editCall {
	q.mu.Lock()
	defer q.mu.Unlock()
	n = min(n, len(q.waiting))
	if n == 0 {
		return nil
	}
	taken := q.waiting[:n:n]
	if q.waiting = q.waiting[n:]; len(q.waiting) == 0 {
		q.waiting = nil
	}
	return taken
}

// pass ends a caller's lead: the caller whose edit has waited longest leads
// next, and nobody does when none waits.
func (q *editQueue) pass() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiting) == 0 {
		q.led = false
		return
	}
	heir := q.waiting[0]
	if q.waiting = q.waiting[1:]; len(q.waiting) == 0 {
		q.waiting = nil
	}
	heir.turn <- false
}

// editFamily is an edit from when it asks for the family's state until it has
// let go of it. It returns the pokes the edit owes and, when the store
// answered before writing the edit, the wait for that write: editAhead sends
// the pokes, and editOn waits. Edits run one at a time, in the order they
// asked.
func (r *Runtime) editFamily(ctx context.Context, session sessions.CoordinationSession, fn func(raw *sessions.CoordinationState, s *State) error) (poke func(), written func(context.Context) error, err error) {
	delta, ok := session.(sessions.CoordinationDeltaSession)
	if !ok {
		// The session cannot write a set of changes: the update reads and
		// writes the family inside one transaction of its own.
		if err := rewrite(ctx, session, fn); err != nil {
			return nil, nil, err
		}
		return r.pokeAll, nil, nil
	}
	call := &editCall{ctx: ctx, session: session, delta: delta, fn: fn}
	if failOnSharedChange {
		call.site = editSite()
	}
	if r.edits.join(call) || !<-call.turn {
		r.lead(call)
	}
	if call.panicked != nil {
		panic(call.panicked)
	}
	return call.poke, call.written, call.err
}

// lead runs call, its caller's own edit, and then up to a batch of the edits
// waiting behind it, under one hold of the family's state: no reader looks at
// the state meanwhile. Each waiting caller goes on as soon as its edit has
// run. The next caller in line leads after that.
func (r *Runtime) lead(call *editCall) {
	defer r.edits.pass()
	r.editing.Lock()
	defer r.editing.Unlock()
	call.poke, call.written, call.err = r.editHeld(call)
	for _, next := range r.edits.take(editBatch - 1) {
		r.editFor(next)
		next.turn <- true
	}
}

// editFor runs an edit for the caller that waits on it. A panic in the edit
// is that caller's: it is handed over with the result.
func (r *Runtime) editFor(call *editCall) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("swarm_edit_panicked", "site", call.site, "panic", p, "stack", string(debug.Stack()))
			call.panicked = p
		}
	}()
	call.poke, call.written, call.err = r.editHeld(call)
}

// editHeld is an edit while its runner holds the family's state.
func (r *Runtime) editHeld(call *editCall) (poke func(), written func(context.Context) error, err error) {
	ctx := call.ctx
	// A family that keeps changing under an edit falls back to the update
	// below, which reads and writes inside one transaction.
	for attempt := 0; attempt < 3; attempt++ {
		// The shared state is current unless something outside this runtime
		// wrote the family; the store says so, and then a read catches up.
		shared, raw, revision := r.view.snapshot()
		if shared == nil || revision == 0 || attempt > 0 {
			if _, err := r.current(ctx); err != nil {
				return nil, nil, err
			}
			if shared, raw, revision = r.view.snapshot(); shared == nil || revision == 0 {
				break
			}
		}
		var committed *sessions.CoordinationState
		var work *State
		var changed map[string]map[string]json.RawMessage
		err := call.delta.UpdateCoordinationFrom(ctx, revision, func(state *sessions.CoordinationState) error {
			work = shared.editable(raw, r.view.unheld())
			if err := call.fn(state, work); err != nil {
				return err
			}
			var err error
			if changed, err = work.changes(); err != nil {
				return err
			}
			committed, state.Changes = state, changed
			return nil
		})
		if err != nil && work != nil {
			// Nothing was written: put the records back as they were.
			work.revert()
			r.view.keep(shared, work)
		}
		if errors.Is(err, sessions.ErrCoordinationStale) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		prior := work.editing.prior
		work.reindex()
		r.view.advance(revision, committed.Revision, work, changed, call.site)
		return r.pokesFor(prior, work), committed.Written, nil
	}
	if err := rewrite(ctx, call.session, call.fn); err != nil {
		return nil, nil, err
	}
	return r.pokeAll, nil, nil
}

// editSite names the caller of the edit its goroutine is making, for the
// edit's diagnostics.
func editSite() string {
	for skip := 2; skip < 40; skip++ {
		pc, file, line, ok := runtime.Caller(skip)
		if !ok {
			break
		}
		name := runtime.FuncForPC(pc).Name()
		if !strings.Contains(name, "pollytool/swarm.") || strings.HasSuffix(name, ").edit") ||
			strings.HasSuffix(name, ").editOn") || strings.HasSuffix(name, ").editAhead") || strings.HasSuffix(name, ").editLive") {
			continue
		}
		return fmt.Sprintf("%s:%d", file[strings.LastIndex(file, "/")+1:], line)
	}
	return "an unknown caller"
}

// snapshot returns the family's state with the records and revision it was
// decoded from, for an edit.
func (c *StateCache) snapshot() (*State, map[string]map[string]json.RawMessage, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.raw, c.revision
}

func (c *StateCache) forget() {
	c.raw, c.revision, c.state, c.lent, c.cloned = nil, 0, nil, nil, nil
}

// unheld names the kinds whose map no reader holds, for an edit to add to.
func (c *StateCache) unheld() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cloned == nil {
		c.cloned = map[string]bool{}
	}
	return c.cloned
}

// lend returns s for a reader to keep. The family's own state changes under
// its edits, so a reader gets a state of its own that shares the maps; from
// then on an edit copies a map before it first writes it. Readers share one
// such state until an edit changes something.
func (c *StateCache) lend(s *State) *State {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s != c.state {
		// The cache has moved to a newer decode, so nothing writes s again.
		return s
	}
	if c.lent == nil {
		lent := *s
		lent.mailTo, lent.rootUse, lent.mailOn, lent.lazy = nil, nil, nil, &lazyIndex{}
		c.lent, c.cloned = &lent, nil
	}
	return c.lent
}

// keep makes work, a failed edit's state with its changes taken back, the
// family's state in place of shared: it holds the maps the edit copied.
func (c *StateCache) keep(shared, work *State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	work.editing = nil
	if c.state == shared {
		c.state = work
	}
}

// advance moves the family's state from revision known to committed by the
// changes an edit just wrote, so the next read needs no copy of the family.
// work is the edit's state, which becomes the family's, with each changed
// record decoded again from what was stored.
func (c *StateCache) advance(known, committed uint64, work *State, changed map[string]map[string]json.RawMessage, site string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fail := c.forget
	if c.state == nil || c.revision != known || committed == 0 {
		// The edit changed maps this cache no longer answers for.
		fail()
		return
	}
	work.editing = nil
	if committed == known {
		c.state = work
		c.check(site)
		return
	}
	for kind, group := range changed {
		for id, data := range group {
			if data == nil {
				delete(c.raw[kind], id)
			} else {
				setChange(c.raw, kind, id, data)
			}
		}
	}
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
	c.revision, c.state, c.lent = committed, work, nil
	c.check(site)
}
