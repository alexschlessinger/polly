package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func inspectorFixture(t *testing.T) (*Runtime, sessions.CoordinationSession) {
	t.Helper()
	r, root := lifecycleRecordFixture(t, 0, false)
	_, err := root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		base := worktree.Snapshot{ID: "base", Commit: "base-commit", Tree: "base-tree", Source: "/source"}
		submitted := worktree.Snapshot{ID: "result", Commit: "result-commit", Tree: "result-tree", Source: "/source"}
		records := map[sessions.RecordKey]any{
			{Kind: "snapshot", ID: base.ID}: base, {Kind: "snapshot", ID: submitted.ID}: submitted,
			{Kind: "member", ID: "a"}:          &Member{ID: "a", Name: "worker", Label: "Edit", Execution: "a", Task: "a", Context: "a"},
			{Kind: "context", ID: "a"}:         &ExecutionContext{ID: "a", Owner: "a", Root: "/source", Scratch: "/scratch", Release: WorkspaceRetained, Reason: "review"},
			{Kind: "execution", ID: "a"}:       &Execution{ID: "a", Member: "a", Run: "run", Status: "completed", Iterations: 3, Request: AgentRequest{MaxIterations: 8}, Error: "reason"},
			{Kind: "task", ID: "a"}:            &Task{ID: "a", Owner: "a", Execution: "a", Run: "run", Description: "edit files", Criteria: "tests", Status: "awaiting_review", Revision: 2, AcceptedRevision: 2, Requirement: RequirementApplied, StartingSnapshot: base.ID, Snapshot: submitted.ID, Result: map[string]any{"result": "full"}, Feedback: "feedback", Dependencies: []string{"selected"}},
			{Kind: "mail", ID: "z"}:            &Mail{ID: "z", From: r.ID, To: "a", Kind: "note", Text: "earlier post, later ID", Posted: time.Unix(1, 0)},
			{Kind: "mail", ID: "a"}:            &Mail{ID: "a", From: "a", To: r.ID, Kind: "request", Text: "question", ReplyTo: "z", Posted: time.Unix(2, 0)},
			{Kind: "publication", ID: "z"}:     &Publication{ID: "z", Author: "a", Text: "earlier publication", Posted: time.Unix(1, 0)},
			{Kind: "publication", ID: "a"}:     &Publication{ID: "a", Author: "a", Text: "full publication", Snapshot: submitted.ID, Sources: []string{"source"}, Supersedes: "z", Posted: time.Unix(2, 0)},
			{Kind: "workflow", ID: "w"}:        &workflow.ReportHeader{ID: "w", Run: "run", Name: "workflow", Status: "completed", Output: map[string]any{"full": "output"}},
			{Kind: "workflow_step", ID: "w/0"}: &workflow.Step{Operation: workflow.Operation{Kind: "test", ID: "step"}, Status: "completed"},
			{Kind: "integration", ID: "a"}:     &IntegrationCandidate{ID: "a", Run: "run", Status: "conflicted", Parent: base, Merged: submitted, Inputs: []IntegrationInput{{TaskReference: TaskReference{Task: "a", Revision: 2}, Base: base, Submitted: submitted}}, Conflicts: []worktree.Conflict{{Type: "content", Paths: []string{"file"}, Base: base, Ours: base, Theirs: submitted}}},
			{Kind: "preview", ID: "a"}:         &worktree.Preview{ID: "a", Candidate: submitted, Conflicts: "conflict"},
		}
		keys := make([]sessions.RecordKey, 0, len(records))
		for key := range records {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Kind != keys[j].Kind {
				return keys[i].Kind < keys[j].Kind
			}
			return keys[i].ID < keys[j].ID
		})
		for _, key := range keys {
			data, err := json.Marshal(records[key])
			if err != nil {
				return err
			}
			if err = tx.Put(key, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, root
}

func readInspectorItems(t *testing.T, r *Runtime, section string, limit int) []InspectorItem {
	t.Helper()
	var items []InspectorItem
	q := InspectorQuery{Section: section, Limit: limit, MaxBytes: 2000}
	for n := 0; n < 100; n++ {
		page, err := r.ReadInspectorPage(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if page.Next == "" {
			return items
		}
		if page.Next == q.After {
			t.Fatal("nonadvancing continuation")
		}
		q.After = page.Next
	}
	t.Fatal("unterminated pages")
	return nil
}
func TestInspectorPagesNativeLegacyParity(t *testing.T) {
	t.Parallel()
	r, root := inspectorFixture(t)
	for _, section := range []string{"members", "tasks", "messages", "publications", "workflows", "integrations", "previews"} {
		t.Run(section, func(t *testing.T) {
			native := readInspectorItems(t, r, section, 2)
			r.parent = repositoryLegacyOnly{root}
			legacy := readInspectorItems(t, r, section, 2)
			r.parent = root
			if !reflect.DeepEqual(native, legacy) {
				a, _ := json.MarshalIndent(native, "", "  ")
				b, _ := json.MarshalIndent(legacy, "", "  ")
				t.Fatalf("native=%s\nlegacy=%s", a, b)
			}
			whole := readInspectorItems(t, r, section, 100)
			if !reflect.DeepEqual(native, whole) {
				t.Fatalf("continuations changed selected items for %s", section)
			}
		})
	}
}
func TestInspectorPagesNativeUnchangedAndOversized(t *testing.T) {
	t.Parallel()
	r, root := inspectorFixture(t)
	payload := strings.Repeat("large-result", 10000)
	_, err := root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		data, _ := json.Marshal(&Task{ID: "a", Description: payload, Result: payload, Status: "pending"})
		return tx.Put(sessions.RecordKey{Kind: "task", ID: "a"}, data)
	})
	if err != nil {
		t.Fatal(err)
	}
	q := InspectorQuery{Section: "tasks", Limit: 20, MaxBytes: 128}
	page, err := r.ReadInspectorPage(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Oversized || page.Next == "" || len(page.Items) != 1 || page.Items[0].Task.Description != payload || page.Items[0].Task.Result != payload {
		t.Fatal("oversized selected task was truncated or lost continuation")
	}
	for _, section := range []string{"members", "tasks", "messages", "publications", "workflows", "integrations", "previews", "raw"} {
		q = InspectorQuery{Section: section}
		first, err := r.ReadInspectorPage(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		q.Revision = first.Revision
		rec := runtimemetrics.New()
		unchanged, err := r.ReadInspectorPage(runtimemetrics.WithRecorder(t.Context(), rec), q)
		if err != nil || !unchanged.Unchanged || len(unchanged.Items) > 0 || unchanged.Raw != nil {
			t.Fatalf("%s unchanged=%+v err=%v", section, unchanged, err)
		}
		snap := rec.Snapshot()
		if snap.Count(runtimemetrics.PayloadBytesRead) != 0 || snap.Count(runtimemetrics.RecordsDecoded) != 0 {
			t.Fatalf("unchanged %s read payloads: %+v", section, snap)
		}
	}
}

func TestInspectorPagesUnrelatedFamilyWork(t *testing.T) {
	t.Parallel()
	baseline := map[string][3]uint64{}
	var seedSQLPerGroup uint64
	for _, count := range []int{16, 1024} {
		r, root := inspectorFixture(t)
		seed := runtimemetrics.New()
		_, err := root.(sessions.CoordinationRecords).UpdateRecords(runtimemetrics.WithRecorder(t.Context(), seed), func(tx sessions.CoordinationTx) error {
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("z-%06d", i)
				for _, kind := range []string{"task", "member", "mail", "publication", "workflow", "integration", "preview"} {
					value := map[string]any{"id": id, "status": "done", "text": strings.Repeat("x", 4096), "description": strings.Repeat("y", 4096)}
					if kind == "workflow" {
						value["status"] = "completed"
					}
					if kind == "publication" {
						// Empty-run members would otherwise receive every publication,
						// turning unrelated history setup into quadratic delivery fanout.
						value["run"] = "unrelated-publications"
					}
					data, _ := json.Marshal(value)
					if err := tx.Put(sessions.RecordKey{Kind: kind, ID: id}, data); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// Guard fixture growth with work counts, not a machine-dependent timeout.
		seedSQL := seed.Snapshot().Count(runtimemetrics.SQLStatements)
		if seedSQL == 0 {
			t.Fatal("fixture setup did not record SQL work")
		}
		if count == 16 {
			seedSQLPerGroup = (seedSQL + uint64(count) - 1) / uint64(count)
		} else if limit := seedSQLPerGroup * uint64(count); seedSQL > limit {
			t.Fatalf("unrelated fixture setup grew faster than linearly: %d SQL statements, want <= %d", seedSQL, limit)
		}
		t.Logf("fixture unrelated=%d setup SQL=%d", count, seedSQL)
		// A one-item members page selects only the first decision; the run and
		// member partitions remain reachable through explicit continuation.
		for _, section := range []string{"members", "tasks", "messages", "publications", "workflows", "integrations", "previews"} {
			rec := runtimemetrics.New()
			page, err := r.ReadInspectorPage(runtimemetrics.WithRecorder(t.Context(), rec), InspectorQuery{Section: section, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if page.Next == "" && section != "members" {
				t.Fatalf("%s missing continuation", section)
			}
			snap := rec.Snapshot()
			got := [3]uint64{snap.Count(runtimemetrics.RowsRead), snap.Count(runtimemetrics.PayloadBytesRead), snap.Count(runtimemetrics.SQLStatements)}
			if count == 16 {
				baseline[section] = got
			} else if baseline[section] != got {
				t.Fatalf("%s grew with unrelated family: %v vs %v", section, baseline[section], got)
			}
			t.Logf("%s unrelated=%d rows/bytes/sql=%v", section, count, got)
		}
	}
}

type coldInspectorSession struct {
	sessions.CoordinationSession
	sessions.CoordinationRecords
}
type coldInspectorReader struct {
	sessions.CoordinationReader
	sessions.CoordinationStatusReader
	sessions.CoordinationIDReader
	sessions.CoordinationInspectorReader
}

func (r coldInspectorReader) SemanticStatus(actor string, next bool) (sessions.SemanticStatusHeader, error) {
	h, e := r.CoordinationStatusReader.SemanticStatus(actor, next)
	h.ColdCompatibility = true
	return h, e
}
func (s coldInspectorSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	return s.CoordinationRecords.ReadRecords(ctx, func(r sessions.CoordinationReader) error {
		return fn(coldInspectorReader{r, r.(sessions.CoordinationStatusReader), r.(sessions.CoordinationIDReader), r.(sessions.CoordinationInspectorReader)})
	})
}
func TestInspectorColdCompatibilityAndNoPrepare(t *testing.T) {
	t.Parallel()
	r, root := inspectorFixture(t)
	want := readInspectorItems(t, r, "members", 1)
	r.parent = coldInspectorSession{root, root.(sessions.CoordinationRecords)}
	r.prepared = false // A display must not bootstrap or repair the supplied runtime.
	got := readInspectorItems(t, r, "members", 1)
	if r.prepared || !reflect.DeepEqual(want, got) {
		t.Fatal("cold compatibility changed presentation or prepared runtime")
	}
	first, err := r.ReadInspectorPage(t.Context(), InspectorQuery{Section: "members", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	r.parent = root
	next, err := r.ReadInspectorPage(t.Context(), InspectorQuery{Section: "members", Limit: 1, After: first.Next})
	if err != nil || len(next.Items) == 0 {
		t.Fatalf("cold-to-native continuation lost: %+v %v", next, err)
	}
}

func TestInspectorEmptyJournalAndCompleteWorkflow(t *testing.T) {
	t.Parallel()
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(t.Context(), "empty-inspector", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	r := &Runtime{ID: parent.(sessions.CoordinationSession).ViewID(), parent: parent.(sessions.CoordinationSession)}
	for _, section := range []string{"members", "tasks", "raw"} {
		page, err := r.ReadInspectorPage(t.Context(), InspectorQuery{Section: section})
		if err != nil {
			t.Fatal(err)
		}
		if section == "members" && len(page.Items) != 4 {
			t.Fatalf("missing empty members text: %+v", page)
		}
	}
	r, root := inspectorFixture(t)
	_, err = root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		for i := 1; i < 1100; i++ {
			step := workflow.Step{Operation: workflow.Operation{Kind: "step", ID: fmt.Sprint(i)}, Status: "completed", Value: strings.Repeat("output", 100)}
			data, _ := json.Marshal(step)
			if err := tx.Put(sessions.RecordKey{Kind: "workflow_step", ID: fmt.Sprintf("w/%d", i)}, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := r.ReadInspectorPage(t.Context(), InspectorQuery{Section: "workflows", Limit: 1, MaxBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || len(page.Items[0].Workflow.Steps) != 1100 || page.Items[0].Workflow.Steps[1099].ID != "1099" {
		t.Fatal("selected workflow step output truncated or lexically reordered")
	}
}
