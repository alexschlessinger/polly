package swarm

import (
	"fmt"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestRecordRosterParityAndBoundedHistory(t *testing.T) {
	t.Parallel()
	var baseline [3]int64
	for _, legacy := range []bool{false, true} {
		for _, unrelated := range []int{4, 512} {
			t.Run(fmt.Sprintf("legacy=%t/unrelated=%d", legacy, unrelated), func(t *testing.T) {
				r, root := lifecycleRecordFixture(t, unrelated, legacy)
				if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
					for n := range 90 {
						id := fmt.Sprintf("roster-%03d", n)
						m := &Member{ID: id, Execution: id, Task: id, Label: id, AgentName: "/root/" + id}
						e := &Execution{ID: id, Member: id, Run: "run", Status: []string{"running", "waiting", "queued", "completed", "paused"}[n%5]}
						if n%13 == 0 {
							m.Control = MemberControlStopped
						}
						task := &Task{ID: id, Owner: id, Execution: id, Run: "run", Status: "running", Requirement: RequirementReviewed}
						// An unusual but representable historical task binding must
						// preserve the full helper's deferral label even for a busy member.
						if n == 1 {
							task.Execution = "old"
							task.Deferral = &TaskDeferral{Workflow: "old", Execution: "old", Owner: id, Status: "running"}
							if err := tx.PutExecution(&Execution{ID: "old", Member: id, Run: "run", Workflow: "old", Status: "failed"}); err != nil {
								return err
							}
							if err := tx.PutWorkflowHeader(&workflow.ReportHeader{ID: "old", Run: "run", Status: "failed", Acknowledged: true}); err != nil {
								return err
							}
						}
						for _, err := range []error{tx.PutMember(m), tx.PutExecution(e), tx.PutTask(task)} {
							if err != nil {
								return err
							}
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				s, err := r.read(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				counts := new(followupAccessCounts)
				if !legacy {
					r.parent = &followupCountedSession{CoordinationSession: root, records: root.(sessions.CoordinationRecords), counts: counts}
				}
				got, err := r.compactRosterRecords(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if want := compactRoster(s); got != want {
					t.Fatalf("roster differs\ngot: %s\nwant: %s", got, want)
				}
				if !legacy {
					work := [3]int64{counts.gets.Load(), counts.queries.Load(), counts.rows.Load()}
					if unrelated == 4 {
						baseline = work
					} else if work != baseline {
						t.Fatalf("inactive history amplified roster: %v -> %v", baseline, work)
					}
					t.Logf("inactive history=%d gets/queries/rows=%v", unrelated, work)
				}
			})
		}
	}
}
