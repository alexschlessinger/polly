package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

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
	if !reflect.DeepEqual(shared, stored) {
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
		delete(s.Tasks, deleted)
		s.Tasks["added"] = &Task{ID: "added", Run: task.Run, Description: "added", Status: "pending", Revision: 1}
		added := s.ownTask("added")
		added.Criteria = "set after adding"
		s.Workflows["flow"] = &workflow.Report{ID: "flow", Run: task.Run, Status: "completed", Steps: []workflow.Step{{Status: "completed"}, {Status: "completed"}}}
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
	if err := r.edit(ctx, func(s *State) error { delete(s.Workflows, "flow"); return nil }); err != nil {
		t.Fatal(err)
	}
	if after = sameAsStored(t, r); after.Workflows["flow"] != nil {
		t.Fatal("deleted report survived")
	}
	// A refused edit changes nothing, in memory or stored.
	veto := errors.New("veto")
	if err := r.edit(ctx, func(s *State) error {
		s.ownTask(kept).Description = "refused"
		delete(s.Tasks, edited)
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
	r.parent = hiddenDelta{r.parent}
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
