package swarm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func assertDisplayParity(t *testing.T, c *DisplayCache, r *Runtime) *DisplaySnapshot {
	t.Helper()
	got, err := c.Read(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectDisplayState(state, r.ID)
	if !reflect.DeepEqual(got, want) {
		a, _ := json.MarshalIndent(got, "", "  ")
		b, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("display mismatch\ngot %s\nwant %s", a, b)
	}
	return got
}
func TestDisplaySnapshotNativeLegacyParity(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, 0, false)
			cache := new(DisplayCache)
			put := func(kind, id string, value any) {
				t.Helper()
				b, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if legacy {
					err = root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
						if raw.Records[kind] == nil {
							raw.Records[kind] = map[string]json.RawMessage{}
						}
						raw.Records[kind][id] = b
						return nil
					})
				} else {
					_, err = root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error { return tx.Put(sessions.RecordKey{Kind: kind, ID: id}, b) })
				}
				if err != nil {
					t.Fatal(err)
				}
				assertDisplayParity(t, cache, r)
			}
			first := assertDisplayParity(t, cache, r)
			if again := assertDisplayParity(t, cache, r); again != first {
				t.Fatal("unchanged snapshot pointer changed")
			}
			put("execution", "old", &Execution{ID: "old", Member: "selected", Status: "completed", Request: AgentRequest{CallID: "duplicate"}})
			put("execution", "older", &Execution{ID: "older", Member: "selected", Status: "completed", Request: AgentRequest{CallID: "duplicate"}})
			if len(cache.snapshot.Launches["duplicate"]) != 2 {
				t.Fatal("launch multiplicity lost")
			}
			put("task", "selected", &Task{ID: "selected", Run: "run", Owner: "selected", Execution: "selected", Status: "blocked", Revision: 4, Description: "title", Result: "\n# Summary\nFirst prose.\nDiscarded details"})
			put("member", "selected", &Member{ID: "selected", Name: "worker", Label: "Builder", Task: "selected", Execution: "selected", Control: MemberControlStopped, SandboxPresets: []string{"workspace+private-home", "readonly"}})
			if !reflect.DeepEqual(cache.snapshot.Members["selected"].SandboxPresets, []string{"workspace+private-home", "readonly"}) {
				t.Fatal("display projection lost retained sandbox restrictions")
			}
			put("execution", "selected", &Execution{ID: "selected", Member: "selected", Run: "run", Status: "waiting", Request: AgentRequest{CallID: "agent-call", MaxIterations: 22}, Iterations: 3})
			put("task", "dep", &Task{ID: "dep", Run: "run", Status: "pending"})
			put("task", "selected", &Task{ID: "selected", Run: "run", Owner: "selected", Execution: "selected", Status: "blocked", Revision: 5, Dependencies: []string{"dep"}})
			put("task", "dep", &Task{ID: "dep", Run: "run", Status: "done"})
			posted := time.Unix(5, 0).UTC()
			put("mail", "a", &Mail{ID: "a", From: "selected", To: r.ID, Kind: "request", Text: "\ue000\x01  " + strings.Repeat("λ", 200), Posted: posted})
			put("mail", "z", &Mail{ID: "z", From: r.ID, To: "selected", Kind: "reply", Text: "latest", Posted: posted})
			put("mail", "info", &Mail{ID: "info", From: "selected", To: r.ID, Kind: "info", Text: "not a preview", Posted: posted.Add(time.Hour)})
			if len(cache.snapshot.Messages) != 1 || cache.snapshot.Messages["z"] == nil {
				t.Fatal("newest non-info preview differs")
			}
			put("workflow", "w", &workflow.ReportHeader{ID: "w", Run: "run", Name: "flow", Status: "running", CallID: "workflow-call"})
			put("workflow_step", "w/0", workflow.Step{Operation: workflow.Operation{ID: "agent-call", Kind: "agent"}, Status: "running", Value: strings.Repeat("discard", 100)})
			put("execution", "selected", &Execution{ID: "selected", Member: "selected", Run: "run", Status: "paused", Workflow: "w", RetryPause: &RetryPause{Reason: "provider"}, Request: AgentRequest{CallID: "agent-call"}})
			put("workflow", "w", &workflow.ReportHeader{ID: "w", Run: "run", Name: "flow", Status: "failed"})
			put("parent_turn", r.ID, &ParentTurn{RetryPause: &RetryPause{Reason: "root"}})
			// Removing either source or dependency invalidates its current bundle without
			// mutating an already handed-out snapshot.
			old := cache.snapshot
			_, err := root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
				return tx.Delete(sessions.RecordKey{Kind: "execution", ID: "selected"})
			})
			if err != nil {
				t.Fatal(err)
			}
			after := assertDisplayParity(t, cache, r)
			if old.Executions["selected"] == nil || after.Executions["selected"] != nil {
				t.Fatal("snapshot mutation/deletion")
			}
			saved := r.parent
			r.parent = repositoryLegacyOnly{root}
			compat, err := new(DisplayCache).Read(t.Context(), r)
			r.parent = saved
			if err != nil || !reflect.DeepEqual(after, compat) {
				t.Fatalf("legacy reader differs: %v", err)
			}
		})
	}
}

func TestDisplaySnapshotBoundedUnrelatedHistory(t *testing.T) {
	t.Parallel()
	var baseline [3]uint64
	for _, n := range []int{16, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, 0, false)
			filler := strings.Repeat("unrelated payload ", 2048)
			_, err := root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
				for i := 0; i < n; i++ {
					id := fmt.Sprintf("history-%04d", i)
					for kind, value := range map[string]any{
						"task":      &Task{ID: id, Run: "history", Status: "done", Result: filler},
						"execution": &Execution{ID: id, Member: "old", Status: "completed", Result: &AgentResult{Value: filler}},
						"mail":      &Mail{ID: id, From: "old", To: "old-peer", Kind: "info", Text: filler, Delivered: true},
					} {
						b, _ := json.Marshal(value)
						if err := tx.Put(sessions.RecordKey{Kind: kind, ID: id}, b); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			cache := new(DisplayCache)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			first, err := cache.Read(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			stat := recorder.Snapshot()
			work := [3]uint64{stat.Count(runtimemetrics.SQLStatements), stat.Count(runtimemetrics.RowsRead), stat.Count(runtimemetrics.PayloadBytesRead)}
			if n == 16 {
				baseline = work
			} else if work != baseline {
				t.Fatalf("cold projection scaled with unrelated history: %v versus %v", work, baseline)
			}
			if stat.Count(runtimemetrics.CoordinationRecordsRead) != 0 {
				t.Fatal("display read source records")
			}
			recorder = runtimemetrics.New()
			ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
			again, err := cache.Read(ctx, r)
			if err != nil || again != first {
				t.Fatalf("no-op cache: %v", err)
			}
			if recorder.Snapshot().Count(runtimemetrics.PayloadBytesRead) != 0 {
				t.Fatal("no-op read hydrated values")
			}
			_, err = root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
				return tx.Put(sessions.RecordKey{Kind: "custom", ID: "unrelated"}, json.RawMessage(`42`))
			})
			if err != nil {
				t.Fatal(err)
			}
			again, err = cache.Read(ctx, r)
			if err != nil || again != first {
				t.Fatalf("unrelated write invalidated display: %v", err)
			}
			t.Logf("history=%d cold SQL/rows/bytes=%v; no-op payload=0", n, work)
		})
	}
}
