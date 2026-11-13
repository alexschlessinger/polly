package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

// sameAsStored fails unless the shared state matches a decode of what the
// store holds, which is what an edit's bookkeeping has to preserve.
func sameAsStored(t *testing.T, r *Runtime) *State {
	t.Helper()
	ctx := context.Background()
	shared, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := r.readScratch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A kept state carries its own lazily built index; the records are what
	// has to match.
	plain := *shared
	plain.lazy = nil
	if !reflect.DeepEqual(&plain, stored) {
		t.Fatalf("shared state left the stored records: %s", differingRecords(stored, shared))
	}
	return shared
}

func TestEditWritesTheRecordsItTouches(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	var ids []string
	for _, name := range []string{"kept", "edited", "deleted"} {
		task, err := r.CreateTask(ctx, name, "criteria", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	kept, edited, deleted := ids[0], ids[1], ids[2]
	before := sameAsStored(t, r)
	if err := r.edit(ctx, func(s *State) error {
		if s.Tasks[edited] != before.Tasks[edited] {
			t.Error("an edit copied a record before it was asked to")
		}
		task := s.ownTask(edited)
		if task == before.Tasks[edited] || s.ownTask(edited) != task {
			t.Error("own did not hand out one private copy")
		}
		task.Description = "changed"
		drop(s, "task", &s.Tasks, deleted)
		put(s, "task", &s.Tasks, "added", &Task{ID: "added", Run: task.Run, Description: "added", Status: "pending", Revision: 1})
		added := s.ownTask("added")
		added.Criteria = "set after adding"
		put(s, "workflow", &s.Workflows, "flow", &workflow.Report{ID: "flow", Run: task.Run, Status: "completed", Steps: []workflow.Step{{Status: "completed"}, {Status: "completed"}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after := sameAsStored(t, r)
	if before.Tasks[edited].Description != "edited" || before.Tasks[deleted] == nil || before.Tasks["added"] != nil {
		t.Fatal("an edit reached a state read before it")
	}
	if after.Tasks[edited].Description != "changed" || after.Tasks[deleted] != nil || after.Tasks["added"].Criteria != "set after adding" || len(after.Workflows["flow"].Steps) != 2 {
		t.Fatalf("edit result: %+v", after.Tasks)
	}
	if after.Tasks[kept] != before.Tasks[kept] {
		t.Fatal("an edit decoded a record it did not touch")
	}
	// A report loses a step and changes another; a later edit removes it.
	if err := r.edit(ctx, func(s *State) error {
		report := s.ownWorkflow("flow")
		report.Steps = report.Steps[:1]
		report.Steps[0].Status = "failed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after = sameAsStored(t, r); len(after.Workflows["flow"].Steps) != 1 || after.Workflows["flow"].Steps[0].Status != "failed" {
		t.Fatalf("report after edit: %+v", after.Workflows["flow"])
	}
	if err := r.edit(ctx, func(s *State) error { drop(s, "workflow", &s.Workflows, "flow"); return nil }); err != nil {
		t.Fatal(err)
	}
	if after = sameAsStored(t, r); after.Workflows["flow"] != nil {
		t.Fatal("deleted report survived")
	}
	// A refused edit changes nothing, in memory or stored.
	veto := errors.New("veto")
	if err := r.edit(ctx, func(s *State) error {
		s.ownTask(kept).Description = "refused"
		drop(s, "task", &s.Tasks, edited)
		return veto
	}); !errors.Is(err, veto) {
		t.Fatalf("refused edit: %v", err)
	}
	if after = sameAsStored(t, r); after.Tasks[kept].Description != "kept" || after.Tasks[edited] == nil {
		t.Fatal("a refused edit left a change behind")
	}
}

func TestEditWithoutOwnFailsTheTest(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an edit changed a shared record unnoticed")
		}
	}()
	_ = r.edit(ctx, func(s *State) error { s.Tasks[task.ID].Description = "changed in place"; return nil })
}

// hiddenDelta is a session wrapper of the kind tests and hosts put around the
// parent: it forwards coordination and nothing optional.
type hiddenDelta struct{ sessions.CoordinationSession }

func TestEditFallsBackToUpdate(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	setParent(r, hiddenDelta{r.parent})
	if err := r.edit(ctx, func(s *State) error {
		if s.editing != nil {
			t.Error("fallback handed out a shared state")
		}
		s.ownTask(task.ID).Description = "changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if s := sameAsStored(t, r); s.Tasks[task.ID].Description != "changed" {
		t.Fatal("fallback edit was lost")
	}
}

func BenchmarkOneRecordChange(b *testing.B) {
	for _, records := range []int{2000, 20000} {
		for _, name := range []string{"update", "edit"} {
			b.Run(fmt.Sprintf("%s/%d", name, records), func(b *testing.B) {
				measureSharedReads(b)
				r := runtimeTest(b, idleModel(), 1, 1)
				defer r.Close()
				ctx := context.Background()
				if err := r.update(ctx, func(s *State) error {
					for i := range records {
						id := fmt.Sprintf("%08d", i)
						s.Tasks[id] = &Task{ID: id, Description: "a task with a description of ordinary length", Criteria: "and criteria", Status: "pending", Revision: 1}
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				change := r.update
				if name == "edit" {
					change = r.edit
				}
				n := 0
				for b.Loop() {
					n++
					if err := change(ctx, func(s *State) error { s.ownTask("00000000").Revision = n; return nil }); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// An edit's maps may be the shared state's, which other goroutines read, so
// nothing but put, drop and the own methods may write a State's maps.
func TestStateMapsAreWrittenThroughPutAndDrop(t *testing.T) {
	t.Parallel()
	kinds := "Followups|Integrations|Applies|ParentTurns|Parents|Runs|Members|Tasks|Messages|Publications|Executions|Contexts|Snapshots|Previews|Workflows"
	direct := regexp.MustCompile(`\b\w+\.(` + kinds + `)\[[^\]]+\](, [^=\n]+)? = |\b(delete|clear)\(\w+\.(` + kinds + `)\b`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(data), "\n") {
			if direct.MatchString(line) {
				t.Errorf("%s:%d writes a State map directly: %s", file, number+1, strings.TrimSpace(line))
			}
		}
	}
}

// An edit writes the family's own maps, and copies one that a reader may
// still hold before it first writes it, so no reader's state changes.
func TestEditCopiesAMapWhenItWritesIt(t *testing.T) {
	t.Parallel()
	shared := &State{
		Tasks:    map[string]*Task{"kept": {ID: "kept", Status: "pending"}, "gone": {ID: "gone"}, "edited": {ID: "edited", Status: "pending"}},
		Messages: map[string]*Mail{"mail": {ID: "mail", To: "member"}},
	}
	raw := &sessions.CoordinationState{Records: map[string]map[string]json.RawMessage{}}
	if err := encodeState(raw, shared); err != nil {
		t.Fatal(err)
	}
	work := shared.editable(raw.Records, nil)
	if reflect.ValueOf(work.Tasks).Pointer() != reflect.ValueOf(shared.Tasks).Pointer() {
		t.Fatal("an edit copied a map before writing it")
	}
	work.ownTask("edited").Status = "done"
	drop(work, "task", &work.Tasks, "gone")
	put(work, "task", &work.Tasks, "added", &Task{ID: "added"})
	if shared.Tasks["edited"].Status != "pending" || shared.Tasks["gone"] == nil || shared.Tasks["added"] != nil || len(shared.Tasks) != 3 {
		t.Fatalf("the edit changed the shared tasks: %+v", shared.Tasks)
	}
	if reflect.ValueOf(work.Messages).Pointer() != reflect.ValueOf(shared.Messages).Pointer() {
		t.Fatal("an edit copied a map it never wrote")
	}
	changed, err := work.changes()
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	tasks := changed["task"]
	if len(changed) != 1 || len(tasks) != 3 || tasks["gone"] != nil || !strings.Contains(string(tasks["edited"]), `"done"`) || tasks["added"] == nil {
		t.Fatalf("changes: %s", changed)
	}
}

// A map no reader holds is written where it is, and a failed edit puts every
// record back.
func TestEditWritesInPlaceAndTakesBackAFailure(t *testing.T) {
	t.Parallel()
	family := &State{
		Tasks:    map[string]*Task{"kept": {ID: "kept", Status: "pending"}, "gone": {ID: "gone"}, "edited": {ID: "edited", Status: "pending"}},
		Messages: map[string]*Mail{},
	}
	raw := &sessions.CoordinationState{Records: map[string]map[string]json.RawMessage{}}
	if err := encodeState(raw, family); err != nil {
		t.Fatal(err)
	}
	edited, gone := family.Tasks["edited"], family.Tasks["gone"]
	work := family.editable(raw.Records, map[string]bool{"task": true})
	work.ownTask("edited").Status = "done"
	drop(work, "task", &work.Tasks, "gone")
	put(work, "task", &work.Tasks, "added", &Task{ID: "added"})
	put(work, "mail", &work.Messages, "mail", &Mail{ID: "mail"})
	if reflect.ValueOf(work.Tasks).Pointer() != reflect.ValueOf(family.Tasks).Pointer() {
		t.Fatal("an edit copied a map no reader holds")
	}
	if reflect.ValueOf(work.Messages).Pointer() == reflect.ValueOf(family.Messages).Pointer() {
		t.Fatal("an edit wrote a map a reader may hold")
	}
	if family.Tasks["edited"].Status != "done" || family.Tasks["gone"] != nil || family.Tasks["added"] == nil {
		t.Fatalf("the edit did not write the family's tasks: %+v", family.Tasks)
	}
	work.revert()
	if family.Tasks["edited"] != edited || edited.Status != "pending" || family.Tasks["gone"] != gone || family.Tasks["added"] != nil || len(family.Tasks) != 3 {
		t.Fatalf("a failed edit left its changes: %+v", family.Tasks)
	}
	if len(work.Messages) != 0 || len(family.Messages) != 0 {
		t.Fatalf("a failed edit left its mail: %+v", work.Messages)
	}
}

// A failed edit through the runtime leaves the family as stored, and the next
// edit still works from it.
func TestFailedEditLeavesTheFamilyAsStored(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// A first edit takes the maps for the family, so the failing one writes
	// them in place.
	if err := r.edit(ctx, func(s *State) error { s.ownTask(task.ID).Feedback = "first"; return nil }); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("refused")
	if err := r.edit(ctx, func(s *State) error {
		s.ownTask(task.ID).Description = "changed"
		put(s, "task", &s.Tasks, "added", &Task{ID: "added"})
		put(s, "mail", &s.Messages, "mail", &Mail{ID: "mail", To: "nobody"})
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("edit: %v", err)
	}
	if s := sameAsStored(t, r); s.Tasks[task.ID].Description != "task" || s.Tasks["added"] != nil || s.Messages["mail"] != nil {
		t.Fatalf("a failed edit changed the family: %+v", s.Tasks)
	}
	if err := r.edit(ctx, func(s *State) error { s.ownTask(task.ID).Description = "later"; return nil }); err != nil {
		t.Fatal(err)
	}
	if s := sameAsStored(t, r); s.Tasks[task.ID].Description != "later" || s.Tasks[task.ID].Feedback != "first" {
		t.Fatalf("an edit after a failed one: %+v", s.Tasks[task.ID])
	}
}

// A state a reader keeps does not change under later edits, and readers share
// one until an edit changes something.
func TestKeptStateDoesNotChangeUnderLaterEdits(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := r.State(ctx); again != kept {
		t.Fatal("two reads of an unchanged family did not share a state")
	}
	for round := range 3 {
		if _, err := r.CreateTask(ctx, fmt.Sprintf("more %d", round), "criteria", nil, ""); err != nil {
			t.Fatal(err)
		}
		if err := r.edit(ctx, func(s *State) error { s.ownTask(task.ID).Description = fmt.Sprintf("round %d", round); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(kept.Tasks) != 1 || kept.Tasks[task.ID].Description != "task" {
		t.Fatalf("a kept state changed under later edits: %+v", kept.Tasks)
	}
	if s := sameAsStored(t, r); len(s.Tasks) != 4 || s.Tasks[task.ID].Description != "round 2" || s == kept {
		t.Fatalf("a read after edits: %+v", s.Tasks)
	}
}

// look sees the family as the last edit left it, and finds a member's mail
// through the index the edits keep.
func TestLookFindsMailThroughTheIndex(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	for _, text := range []string{"one", "two"} {
		if _, err := r.Send(ctx, r.ID, r.ID, "info", "", text); err != nil {
			t.Fatal(err)
		}
	}
	var first string
	if err := r.look(ctx, func(s *State) {
		if s.mailTo == nil {
			t.Error("the family's state has no mail index")
		}
		if mail := inbox(s, r.ID, true); len(mail) != 2 || len(inbox(s, "nobody", true)) != 0 {
			t.Errorf("inbox through the index: %d", len(mail))
		} else {
			first = mail[0].ID
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.edit(ctx, func(s *State) error {
		drop(s, "mail", &s.Messages, first)
		// The edit's own view already leaves the dropped mail out.
		if len(inbox(s, r.ID, true)) != 1 {
			t.Error("an edit did not see its own change to the mail")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.look(ctx, func(s *State) {
		if mail := inbox(s, r.ID, true); len(mail) != 1 || mail[0].ID == first {
			t.Errorf("inbox after a drop: %d", len(mail))
		}
	}); err != nil {
		t.Fatal(err)
	}
	sameAsStored(t, r)
}

// heldWrite is the parent's session with the write of its first edit held
// back until release is closed. The store still answers that edit from memory.
type heldWrite struct {
	sessions.CoordinationSession
	holding atomic.Bool
	waiting chan struct{} // closed when the edit starts to wait for its write
	release chan struct{}
	fail    error // what the held write then reports
	// cancelled reports whether the context the edit waited under had ended.
	cancelled atomic.Bool
}

func (h *heldWrite) CoordinationRevision(ctx context.Context) (uint64, error) {
	return h.CoordinationSession.(sessions.CoordinationRevisionSession).CoordinationRevision(ctx)
}

func (h *heldWrite) UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*sessions.CoordinationState) error) error {
	var state *sessions.CoordinationState
	err := h.CoordinationSession.(sessions.CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(s *sessions.CoordinationState) error {
		state = s
		return fn(s)
	})
	if err != nil || !h.holding.CompareAndSwap(false, true) {
		return err
	}
	written := state.Written
	state.Written = func(ctx context.Context) error {
		close(h.waiting)
		<-h.release
		h.cancelled.Store(ctx.Err() != nil)
		if h.fail != nil || written == nil {
			return h.fail
		}
		return written(ctx)
	}
	return nil
}

// An edit is in the family's memory, and its pokes are sent, as soon as the
// store accepts it. The caller is held until the store has written it, and
// cancelling the caller does not let it go early.
func TestEditReturnsOnceItsChangeIsWritten(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]error{"written": nil, "not written": errors.New("disk gone")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := runtimeTest(t, idleModel(), 1, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			watcher := &invocation{poke: make(chan struct{}, 1)}
			r.watchTasks(watcher, []string{task.ID})
			held := &heldWrite{CoordinationSession: r.parent, waiting: make(chan struct{}), release: make(chan struct{}), fail: fail}
			setParent(r, held)
			r.mu.Lock()
			notify := r.notify
			r.mu.Unlock()
			done := make(chan error, 1)
			go func() {
				done <- r.edit(ctx, func(s *State) error {
					s.ownTask(task.ID).Description = "changed"
					return nil
				})
			}()
			select {
			case <-held.waiting:
			case err := <-done:
				t.Fatalf("edit returned without waiting for its write: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("edit never waited for its write")
			}
			var seen string
			if err := r.look(context.Background(), func(s *State) { seen = s.Tasks[task.ID].Description }); err != nil || seen != "changed" {
				t.Fatalf("a reader during the wait saw %q, %v", seen, err)
			}
			select {
			case <-watcher.poke:
			default:
				t.Fatal("the edit's poke waited for its write")
			}
			cancel()
			select {
			case err := <-done:
				t.Fatalf("edit returned before its change was written: %v", err)
			default:
			}
			close(held.release)
			if err := <-done; !errors.Is(err, fail) {
				t.Fatalf("edit = %v, want %v", err, fail)
			}
			if held.cancelled.Load() {
				t.Fatal("the caller's cancellation reached the wait for the write")
			}
			select {
			case <-notify:
			default:
				t.Fatal("waiters on a change did not hear of the edit")
			}
		})
	}
}

// A task change pokes the parked members whose wake depends on that task, and
// no others.
func TestTaskChangePokesOnlyItsWatchers(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	owner, other := &invocation{poke: make(chan struct{}, 1)}, &invocation{poke: make(chan struct{}, 1)}
	s := &State{
		Members: map[string]*Member{"owner": {ID: "owner", Task: "task"}, "other": {ID: "other", Task: "elsewhere"}},
		Tasks:   map[string]*Task{"task": {ID: "task", Dependencies: []string{"dependency"}}},
	}
	r.watchTasks(owner, waitTasks(s, "owner"))
	r.watchTasks(other, waitTasks(s, "other"))
	poked := func(i *invocation) bool {
		select {
		case <-i.poke:
			return true
		default:
			return false
		}
	}
	for _, id := range []string{"task", "dependency"} {
		r.pokesFor(map[string]map[string]any{"task": {id: nil}}, s)()
		if !poked(owner) || poked(other) {
			t.Fatalf("a change to %s poked the wrong members", id)
		}
	}
	if poke := r.pokesFor(map[string]map[string]any{"task": {"unwatched": nil}}, s); poke != nil {
		poke()
	}
	if poked(owner) || poked(other) {
		t.Fatal("a task nobody waits on poked a member")
	}
	r.watchTasks(owner, nil)
	r.watchTasks(other, nil)
	r.mu.Lock()
	left := len(r.watchers)
	r.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d tasks still watched after their members woke", left)
	}
}

func TestOtherRootsFollowsTheContextsAnEditTouched(t *testing.T) {
	t.Parallel()
	s := &State{Contexts: map[string]*ExecutionContext{
		"a": {ID: "a", Root: "/x"}, "b": {ID: "b", Root: "/y"}, "c": {ID: "c", Root: "/y"},
	}}
	s.mailTo, s.rootUse = indexMail(s), indexRoots(s)
	if got := otherRoots(s, s.Contexts["a"]); len(got) != 1 || got[0] != "/y" {
		t.Fatalf("otherRoots = %v, want [/y]", got)
	}
	// An edit drops b and moves c to a new root.
	old := s.Contexts["c"]
	s.editing = &editBase{prior: map[string]map[string]any{"context": {"b": s.Contexts["b"], "c": old}}}
	delete(s.Contexts, "b")
	s.Contexts["c"] = &ExecutionContext{ID: "c", Root: "/z"}
	s.reindex()
	s.editing = nil
	if got := otherRoots(s, s.Contexts["a"]); len(got) != 1 || got[0] != "/z" {
		t.Fatalf("otherRoots after the edit = %v, want [/z]", got)
	}
	if want := indexRoots(s); !reflect.DeepEqual(s.rootUse, want) {
		t.Fatalf("rootUse = %v, want %v", s.rootUse, want)
	}
}

func TestResultNoticeIsFoundThroughEveryKindOfState(t *testing.T) {
	t.Parallel()
	build := func() *State {
		return &State{
			Tasks: map[string]*Task{"t": {ID: "t", Revision: 2, Execution: "e"}},
			Messages: map[string]*Mail{
				"m1": {ID: "m1", Task: "t", Revision: 1, Execution: "e"},
				"m3": {ID: "m3", Task: "t", Revision: 2, Execution: "e"},
				"m2": {ID: "m2", Task: "t", Revision: 2, Execution: "e"},
				"m0": {ID: "m0", Task: "other", Revision: 2, Execution: "e"},
				"m4": {ID: "m4", To: "someone"},
			},
		}
	}
	scanned := build()
	indexed := build()
	indexed.mailTo, indexed.mailOn = indexMail(indexed), indexTaskMail(indexed)
	kept := build()
	kept.lazy = &lazyIndex{}
	for name, s := range map[string]*State{"scanned": scanned, "indexed": indexed, "kept": kept} {
		if notice := resultNotice(s, s.Tasks["t"]); notice == nil || notice.ID != "m2" {
			t.Fatalf("%s: resultNotice = %v, want m2", name, notice)
		}
		if notices := resultNotices(s); len(notices) != 1 || notices["t"].ID != "m2" {
			t.Fatalf("%s: resultNotices = %v, want t: m2", name, notices)
		}
	}
	// An edit drops the notice; the index follows.
	indexed.editing = &editBase{prior: map[string]map[string]any{"mail": {"m2": indexed.Messages["m2"]}}}
	delete(indexed.Messages, "m2")
	if notice := resultNotice(indexed, indexed.Tasks["t"]); notice == nil || notice.ID != "m3" {
		t.Fatalf("during the edit: resultNotice = %v, want m3", notice)
	}
	indexed.reindex()
	indexed.editing = nil
	if want := indexTaskMail(indexed); !reflect.DeepEqual(indexed.mailOn, want) {
		t.Fatalf("mailOn = %v, want %v", indexed.mailOn, want)
	}
}
