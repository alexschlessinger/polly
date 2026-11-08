package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func swarmInspectorREPLFixture(t *testing.T) *managedREPL {
	t.Helper()
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), nil)
	base := worktree.Snapshot{ID: "base", Commit: "base-commit", Tree: "base-tree", Source: "/source"}
	result := worktree.Snapshot{ID: "result", Commit: "result-commit", Tree: "result-tree", Source: "/source"}
	records := map[sessions.RecordKey]any{
		{Kind: "format", ID: "swarm"}:  map[string]int{"version": 2},
		{Kind: "snapshot", ID: "base"}: base, {Kind: "snapshot", ID: "result"}: result,
		{Kind: "run", ID: "run"}:           &swarm.Run{ID: "run", Status: "running", Starts: 1, Limit: 10},
		{Kind: "member", ID: "a"}:          &swarm.Member{ID: "a", Label: "Edit", Name: "worker", Task: "a", Execution: "a", Context: "a"},
		{Kind: "execution", ID: "a"}:       &swarm.Execution{ID: "a", Member: "a", Run: "run", Status: "completed", Iterations: 2, Request: swarm.AgentRequest{MaxIterations: 8}},
		{Kind: "context", ID: "a"}:         &swarm.ExecutionContext{ID: "a", Owner: "a", Root: "/source", Scratch: "/scratch", Release: swarm.WorkspaceRetained, Reason: "review"},
		{Kind: "task", ID: "a"}:            &swarm.Task{ID: "a", Owner: "a", Execution: "a", Run: "run", Status: "awaiting_review", Revision: 2, AcceptedRevision: 2, Description: "edit files", Criteria: "tests", Requirement: swarm.RequirementApplied, StartingSnapshot: base.ID, Snapshot: result.ID, Result: map[string]any{"full": "result"}, Feedback: "feedback", Dependencies: []string{"other"}},
		{Kind: "task", ID: "other"}:        &swarm.Task{ID: "other", Status: "done"},
		{Kind: "mail", ID: "z"}:            &swarm.Mail{ID: "z", From: r.state.swarm.ID, To: "a", Kind: "note", Text: "Earlier post", Posted: time.Unix(1, 0)},
		{Kind: "mail", ID: "a"}:            &swarm.Mail{ID: "a", From: "a", To: r.state.swarm.ID, Kind: "request", Text: strings.Repeat("question", 100), Posted: time.Unix(2, 0), ReplyTo: "z"},
		{Kind: "publication", ID: "z"}:     &swarm.Publication{ID: "z", Author: "a", Text: "Earlier publication", Posted: time.Unix(1, 0)},
		{Kind: "publication", ID: "a"}:     &swarm.Publication{ID: "a", Author: "a", Text: "Full publication", Posted: time.Unix(2, 0), Supersedes: "z", Sources: []string{"source"}, Snapshot: result.ID},
		{Kind: "workflow", ID: "w"}:        &workflow.ReportHeader{ID: "w", Name: "workflow", Run: "run", Status: "failed", Acknowledged: true, Output: map[string]any{"result": "workflow"}, Error: &workflow.Error{Code: "failed", Message: "failure"}},
		{Kind: "workflow_step", ID: "w/0"}: &workflow.Step{Operation: workflow.Operation{ID: "step", Kind: "test"}, Status: "failed", Error: &workflow.Error{Message: "step failure"}},
		{Kind: "integration", ID: "a"}:     &swarm.IntegrationCandidate{ID: "a", Run: "run", Status: "conflicted", Drift: "drift", Parent: base, Merged: result, Inputs: []swarm.IntegrationInput{{TaskReference: swarm.TaskReference{Task: "a", Revision: 2}, Base: base, Submitted: result}}, Conflicts: []worktree.Conflict{{Type: "content", Paths: []string{"file"}, Base: base, Ours: base, Theirs: result}}, Predecessor: "older"},
		{Kind: "apply", ID: "a"}:           &swarm.ApplyRecord{ID: "a", Status: "not_applied", ObservedParent: base, Error: "not applied"},
		{Kind: "preview", ID: "a"}:         &worktree.Preview{ID: "a", Candidate: result, Conflicts: "conflicts"},
	}
	_, err := r.state.session.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		for key, value := range records {
			data, err := json.Marshal(value)
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
	return r
}

func TestSwarmInspectorPageWordingAndOrder(t *testing.T) {
	r := swarmInspectorREPLFixture(t)
	state, err := r.state.swarm.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parent := r.state.swarm.ParentState(state)
	for _, section := range swarmInspectorSections {
		t.Run(section, func(t *testing.T) {
			want := swarmInspectorTextFor(state, &parent, section, r.state.swarm.ID)
			for _, limit := range []int{1, 100} {
				var text strings.Builder
				q := swarm.InspectorQuery{Section: section, Limit: limit}
				for n := 0; n < 100; n++ {
					page, err := r.state.swarm.ReadInspectorPage(t.Context(), q)
					if err != nil {
						t.Fatal(err)
					}
					text.WriteString(swarmInspectorPageText(page, &parent))
					if page.Next == "" {
						break
					}
					q.After = page.Next
				}
				if text.String() != want {
					t.Fatalf("section %s limit %d\ngot:\n%s\nwant:\n%s", section, limit, text.String(), want)
				}
			}
		})
	}
}
func TestSwarmInspectorHelperUnchangedAndEviction(t *testing.T) {
	r := swarmInspectorREPLFixture(t)
	target := tabViewTarget(r.visibleTab())
	target.kind = swarmViewKind
	target.item = "members"
	source, page, err := readSwarmInspector(t.Context(), r.state.swarm, target, viewSource{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, nextPage, err := readSwarmInspector(t.Context(), r.state.swarm, target, source, page)
	if err != nil || next.model != source.model || next.revision != source.revision || nextPage.revision != page.revision {
		t.Fatal("unchanged inspector rerendered")
	}
	reloaded, _, err := readSwarmInspector(t.Context(), r.state.swarm, target, viewSource{}, page)
	if err != nil || reloaded.model == nil || reloaded.model == source.model {
		t.Fatal("eviction did not reload selected page")
	}
}
func TestSwarmInspectorAsyncPageNavigation(t *testing.T) {
	r := swarmInspectorREPLFixture(t)
	_, err := r.state.session.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		for i := 0; i < 70; i++ {
			id := fmt.Sprintf("item-%03d", i)
			data, _ := json.Marshal(&swarm.Task{ID: id, Description: "TASK-" + id, Status: "done"})
			if err := tx.Put(sessions.RecordKey{Kind: "task", ID: id}, data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	target := tabViewTarget(r.visibleTab())
	target.kind = swarmViewKind
	target.item = "tasks"
	r.inspect(target)
	v := waitInspector(t, r, 140)
	state := r.workspace().viewState(target)
	if state.swarm == nil || state.swarm.next == "" || !strings.Contains(inspectorText(v), "Ctrl-Right: next page") {
		t.Fatal("first page has no discoverable continuation")
	}
	state.top, state.follow = 4, false
	r.navigateSwarmInspector("<C-Right>")
	v = waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), "Page 2") || state.swarm.after == "" {
		t.Fatal("asynchronous next page did not land")
	}
	r.navigateSwarmInspector("<C-Left>")
	v = waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), "Page 1") || state.swarm.after != "" || state.top != 4 || state.follow {
		t.Fatalf("asynchronous previous page lost position: top=%d follow=%v", state.top, state.follow)
	}
	r.navigateSwarmInspector("<C-Right>")
	state.anchor = viewAnchor{block: "destination-page", units: 7}
	r.inspectorAction("swarm_messages") // retire before the next read has landed
	if state.anchor != (viewAnchor{block: "destination-page", units: 7}) {
		t.Fatal("retiring the old page overwrote destination scroll")
	}
}

func TestSwarmInspectorPageNavigationRetainsSectionAndScroll(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	target := tabViewTarget(r.visibleTab())
	target.kind = swarmViewKind
	target.item = "tasks"
	r.inspect(target)
	i := &r.workspace().inspector
	s := r.workspace().viewState(target)
	s.swarm = &swarmInspectorPageState{next: "second"}
	s.top, s.follow, s.lastRows = 7, false, 50
	if !r.navigateSwarmInspector("<C-Right>") || s.swarm.after != "second" || len(s.swarm.previous) != 1 || s.top != 0 || s.follow {
		t.Fatal("next page lost cursor or initial position")
	}
	if rememberSwarmInspectorPosition(*s) {
		t.Fatal("old page can overwrite destination anchor")
	}
	s.swarm.renderedAfter = "second"
	s.top, s.lastRows = 13, 100
	if !r.navigateSwarmInspector("<C-Left>") || s.swarm.after != "" || s.top != 7 || s.lastRows != 50 || s.follow {
		t.Fatal("previous page did not restore scroll")
	}
	if i.target != target {
		t.Fatal("page navigation changed selected section")
	}
	r.inspectorAction("swarm_messages")
	r.inspectorAction("swarm_tasks")
	if r.workspace().viewState(i.target) != s || s.top != 7 {
		t.Fatal("section navigation lost page state")
	}
}
