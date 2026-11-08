package swarm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func assertSemanticStatus(t *testing.T, r *Runtime, root sessions.CoordinationSession) {
	t.Helper()
	s, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	actors := []string{r.ID, "missing"}
	for id := range s.Members {
		actors = append(actors, id)
	}
	for _, actor := range actors {
		want := Present(s, actor, r.ID)
		err := root.(sessions.CoordinationRecords).ReadRecords(t.Context(), func(raw sessions.CoordinationReader) error {
			reader, ok := raw.(sessions.CoordinationStatusReader)
			if !ok {
				t.Fatal("missing native semantic status")
			}
			header, err := reader.SemanticStatus(actor, true)
			if err != nil {
				return err
			}
			if header.ColdCompatibility {
				t.Fatal("unexpected cold compatibility")
			}
			var budget *Budget
			if header.Budget != nil {
				v := Budget(*header.Budget)
				budget = &v
			}
			if Counts(header.Counts) != want.Counts || !reflect.DeepEqual(budget, want.Budget) || header.Next != want.Next || header.ParentDisposition != parentDisposition(s) {
				t.Fatalf("actor %s header=%+v budget=%+v\nwant=%+v budget=%+v disposition=%s", actor, header, budget, want, want.Budget, parentDisposition(s))
			}
			for _, section := range []string{"decisions", "working"} {
				page, err := reader.SemanticStatusPage(sessions.SemanticStatusQuery{Actor: actor, Section: section, Limit: 1000})
				if err != nil {
					return err
				}
				if section == "decisions" {
					var got []DecisionItem
					for _, v := range page.Decisions {
						got = append(got, DecisionItem(v))
					}
					if page.Total != len(want.Decisions) || !slices.Equal(got, want.Decisions) {
						t.Fatalf("actor %s decisions=%+v want=%+v", actor, got, want.Decisions)
					}
				} else {
					var got []WorkingItem
					for _, v := range page.Working {
						got = append(got, WorkingItem(v))
					}
					if page.Total != len(want.Working) || !slices.Equal(got, want.Working) {
						t.Fatalf("actor %s working=%+v want=%+v", actor, got, want.Working)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.status(t.Context(), actor, tools.Args{})
		if err != nil {
			t.Fatal(err)
		}
		expected, err := summarize(want)
		if err != nil {
			t.Fatal(err)
		}
		if tools.Result(got) != tools.Result(expected) {
			t.Fatalf("summary actor %s:\n%s\nwant:\n%s", actor, tools.Result(got), tools.Result(expected))
		}
		saved := r.parent
		r.parent = repositoryLegacyOnly{root}
		legacy, err := r.status(t.Context(), actor, tools.Args{})
		r.parent = saved
		if err != nil || tools.Result(legacy) != tools.Result(expected) {
			t.Fatalf("legacy status actor %s: %s err=%v", actor, tools.Result(legacy), err)
		}
	}
}
func TestSemanticStatusMutationParity(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-writes=%t", legacy), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, 0, false)
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
				assertSemanticStatus(t, r, root)
			}
			assertSemanticStatus(t, r, root)
			put("task", "pending", &Task{ID: "pending", Run: "run", Status: "pending", Dependencies: []string{"selected"}})
			put("execution", "selected", &Execution{ID: "selected", Run: "run", Member: "selected", Status: "waiting", Generation: 7})
			put("mail", "ask", &Mail{ID: "ask", From: "selected", To: r.ID, Kind: "request", Text: "Which option?", Delivered: true})
			put("mail", "ask", &Mail{ID: "ask", From: "selected", To: r.ID, Kind: "request", Text: "Which option?", Delivered: true, ReplyID: "reply"})
			put("mail", "reply", &Mail{ID: "reply", From: "selected", To: r.ID, Kind: "reply"})
			put("workflow", "w", &workflow.ReportHeader{ID: "w", Run: "run", Name: "workflow", Status: "running"})
			put("workflow_step", "w/0", workflow.Step{Operation: workflow.Operation{ID: "call", Kind: "agent"}, Status: "running"})
			put("execution", "selected", &Execution{ID: "selected", Run: "run", Member: "selected", Status: "waiting", Workflow: "w", Generation: 7})
			put("workflow_step", "w/1", workflow.Step{Operation: workflow.Operation{ID: "followup", Kind: "followup"}, Status: "running"})
			put("workflow", "w", &workflow.ReportHeader{ID: "w", Run: "run", Status: "completed"})
			put("mail", "workflow-notice", &Mail{ID: "workflow-notice", Workflow: "w", To: r.ID})
			put("run", "run", &Run{ID: "run", Status: "paused", Starts: 10, Limit: 10})
			put("task", "pending", &Task{ID: "pending", Run: "run", Status: "pending", Dependencies: []string{"pending", "selected"}})
			put("task", "selected", &Task{ID: "selected", Run: "run", Status: "blocked", Owner: "selected", Execution: "selected"})
			put("member", "selected", &Member{ID: "selected", Control: MemberControlStopped, Task: "selected", Execution: "selected"})
			put("apply", "a", &ApplyRecord{ID: "a", Status: "applying"})
			put("run", "run2", &Run{ID: "run2", Status: "running", Limit: 3})
			put("run", "run", &Run{ID: "run", Status: "completed", Starts: 10, Limit: 10})
		})
	}
}
func TestSemanticStatusUnchangedAndCandidates(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	s, task := unchangedTaskState()
	s.Format = &FormatRecord{Version: 2}
	if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error { return encodeState(raw, s) }); err != nil {
		t.Fatal(err)
	}
	assertSemanticStatus(t, r, root)
	for _, status := range []string{"applying", "not_applied"} {
		if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
			s.Applies = map[string]*ApplyRecord{"apply": {ID: "apply", Status: status}}
			return encodeState(raw, s)
		}); err != nil {
			t.Fatal(err)
		}
		assertSemanticStatus(t, r, root)
	}
	// Two raw editing submissions exercise exact, uncapped batch Next.
	task.AcceptedRevision = 0
	copy := *task
	copy.ID = "second"
	s.Tasks[copy.ID] = &copy
	if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error { return encodeState(raw, s) }); err != nil {
		t.Fatal(err)
	}
	assertSemanticStatus(t, r, root)
}
func TestSemanticStatusBoundedHistory(t *testing.T) {
	t.Parallel()
	var baseline [2]uint64
	var baselineWrite [3]uint64
	for _, n := range []int{16, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, n, false)
			r.parent = lifecycleNarrowOnly{root, root.(sessions.CoordinationRecords)}
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			if _, err := r.status(ctx, r.ID, tools.Args{}); err != nil {
				t.Fatal(err)
			}
			snapshot := recorder.Snapshot()
			work := [2]uint64{snapshot.Count(runtimemetrics.RowsRead), snapshot.Count(runtimemetrics.PayloadBytesRead)}
			if n == 16 {
				baseline = work
			} else if work != baseline {
				t.Fatalf("history scaled native status: %v versus %v", work, baseline)
			}
			t.Logf("history groups=%d status returned rows=%d payload bytes=%d", n, work[0], work[1])
			recorder = runtimemetrics.New()
			ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
			if err := r.updateRecords(ctx, func(tx *recordTx) error {
				e, err := tx.Execution("selected")
				if err != nil {
					return err
				}
				e.Iterations++
				return tx.PutExecution(e)
			}); err != nil {
				t.Fatal(err)
			}
			snapshot = recorder.Snapshot()
			write := [3]uint64{snapshot.Count(runtimemetrics.SQLStatements), snapshot.Count(runtimemetrics.RowsRead), snapshot.Count(runtimemetrics.PayloadBytesRead)}
			if n == 16 {
				baselineWrite = write
			} else if write != baselineWrite {
				t.Fatalf("history scaled incremental derivation: %v versus %v", write, baselineWrite)
			}
			t.Logf("history groups=%d execution mutation SQL=%d returned rows=%d payload bytes=%d (not VM visits)", n, write[0], write[1], write[2])
		})
	}
}
func TestSemanticCandidateSelectionAndDeduplication(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	s, task := unchangedTaskState()
	s.Format = &FormatRecord{Version: 2}
	task.AcceptedRevision = 0
	other := *task
	other.ID = "a-first"
	s.Tasks[other.ID] = &other
	if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error { return encodeState(raw, s) }); err != nil {
		t.Fatal(err)
	}
	put := func(kind, id string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		_, err = root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error { return tx.Put(sessions.RecordKey{Kind: kind, ID: id}, b) })
		if err != nil {
			t.Fatal(err)
		}
		assertSemanticStatus(t, r, root)
	}
	candidate := &IntegrationCandidate{ID: "a", Status: "ready", Created: time.Unix(100, 0), Inputs: []IntegrationInput{
		{TaskReference: TaskReference{Task: task.ID, Revision: task.Revision}, Submitted: *s.Snapshots[task.Snapshot]},
		{TaskReference: TaskReference{Task: other.ID, Revision: other.Revision}, Submitted: *s.Snapshots[other.Snapshot]},
	}}
	put("integration", candidate.ID, candidate)
	candidate.ID = "z" // equal timestamp: higher ID wins, anchored at a-first
	put("integration", candidate.ID, candidate)
	state, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p := Present(state, r.ID, r.ID)
	if len(p.Decisions) != 1 || p.Decisions[0].ID != "z" {
		t.Fatalf("candidate tie/dedup: %+v", p.Decisions)
	}
	other.Revision++ // invalidates both candidates, including the other task's row
	put("task", other.ID, &other)
	candidate.Inputs[1].Revision = other.Revision
	put("integration", candidate.ID, candidate)
	put("apply", "z", &ApplyRecord{ID: "z", Status: "applying"})
	put("apply", "z", &ApplyRecord{ID: "z", Status: "not_applied"})
	candidate.Status = "conflicted"
	put("integration", candidate.ID, candidate)
	// Dependency/deferral invalidation is not an active-run-only decision rule.
	w := &workflow.ReportHeader{ID: "w", Status: "failed", Acknowledged: true}
	put("workflow", "w", w)
	task.Status, task.Execution = "blocked", "e"
	task.Deferral = &TaskDeferral{Workflow: "w", Execution: "e", Owner: task.Owner, Revision: task.Revision, Status: "blocked", Generation: 3}
	put("execution", "e", &Execution{ID: "e", Member: task.Owner, Workflow: "w", Status: "failed", Generation: 3})
	put("task", task.ID, task)
	put("execution", "e", &Execution{ID: "e", Member: task.Owner, Workflow: "w", Status: "failed", Generation: 4})
}

func TestSemanticBatchNextOutputCost(t *testing.T) {
	t.Parallel()
	for _, n := range []int{16, 128} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, 0, false)
			s, task := unchangedTaskState()
			s.Format = &FormatRecord{Version: 2}
			s.Tasks = map[string]*Task{}
			task.AcceptedRevision = 0
			for i := 0; i < n; i++ {
				copy := *task
				copy.ID = fmt.Sprintf("t%04d", i)
				s.Tasks[copy.ID] = &copy
			}
			if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error { return encodeState(raw, s) }); err != nil {
				t.Fatal(err)
			}
			want := Present(s, r.ID, r.ID).Next
			recorder := runtimemetrics.New()
			if err := root.(sessions.CoordinationRecords).ReadRecords(runtimemetrics.WithRecorder(t.Context(), recorder), func(rd sessions.CoordinationReader) error {
				h, err := rd.(sessions.CoordinationStatusReader).SemanticStatus(r.ID, true)
				if err == nil && (h.Next != want || strings.Count(h.Next, `"revision":`) != n) {
					t.Fatalf("batch refs were capped: %s", h.Next)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			cost := recorder.Snapshot()
			t.Logf("exact batch Next refs=%d output bytes=%d returned rows=%d payload bytes=%d (intentional output-proportional work)", n, len(want), cost.Count(runtimemetrics.RowsRead), cost.Count(runtimemetrics.PayloadBytesRead))
		})
	}
}

func TestSemanticTimeoutUsesTotal(t *testing.T) {
	t.Parallel()
	p := Presentation{Counts: Counts{Working: 100}, Working: []WorkingItem{{Label: "first", State: "running"}}}
	if text := waitTextTotal(p, 100); !strings.Contains(text, "and 99 more") {
		t.Fatal(text)
	}
}
