package swarm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

type taskToolView struct {
	*Task
	DisplayStatus string `json:"displayStatus"`
}

func TestFailedCoordinationMutationsDoNotReportSuccess(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"swarm_control", map[string]any{"action": "release", "id": "execution-not-context"}},
		{"swarm_control", map[string]any{"action": "stop", "id": "missing"}},
		{"swarm_control", map[string]any{"action": "cancel_task", "id": "missing"}},
		{"swarm_review", map[string]any{"task": "missing", "revision": 1, "accept": true}},
		{"swarm_control", map[string]any{"action": "cancel_workflow", "id": "missing"}},
		{"swarm_control", map[string]any{"action": "acknowledge_workflow", "id": "missing"}},
	} {
		t.Run(tc.name+":"+stringValue(tc.args["action"]), func(t *testing.T) {
			tool, _, _ := r.config.Registry.GetIfAllowed(tc.name)
			out, err := tool.Execute(context.Background(), tc.args)
			if err == nil || out != "" {
				t.Fatalf("failed mutation reported output %q, error %v", out, err)
			}
			if tc.args["action"] == "release" && (!strings.Contains(err.Error(), "execution-not-context") || !strings.Contains(err.Error(), "list_agents")) {
				t.Fatalf("cleanup error lacks the requested ID and recovery guidance: %v", err)
			}
		})
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func TestReviewToolRefusesEditingAndAcceptsResearch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	if err := r.update(ctx, func(s *State) error {
		base := worktree.Snapshot{ID: "base", Tree: "base-tree", Commit: "base-commit", Source: "parent"}
		s.Tasks["changed"] = &Task{ID: "changed", Owner: "editor", Status: "awaiting_review", Revision: 2, Snapshot: "candidate", StartingSnapshot: base.ID}
		s.Members["editor"] = &Member{ID: "editor", Context: "copy"}
		s.Contexts["copy"] = &ExecutionContext{ID: "copy", Owner: "editor", Root: "child", Checkout: &worktree.Checkout{Path: "child", Base: base}}
		s.Snapshots[base.ID] = &base
		s.Snapshots["candidate"] = &worktree.Snapshot{ID: "candidate", Tree: "changed-tree", Commit: "changed-commit", Source: "child"}
		s.Tasks["research"] = &Task{ID: "research", Status: "awaiting_review", Revision: 3}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	review, _, _ := r.config.Registry.GetIfAllowed("swarm_review")
	out, err := review.Execute(ctx, map[string]any{"task": "changed", "revision": 2, "accept": true})
	if err == nil || !strings.Contains(err.Error(), "swarm_integrate") || out != "" {
		t.Fatalf("editing acceptance: %q %v", out, err)
	}
	// Preserve the list's display coverage for a saved acceptance from before
	// this API change. New editing acceptances must go through Integrate.
	if err := r.update(ctx, func(s *State) error { s.Tasks["changed"].AcceptedRevision = 2; return nil }); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id, status, display string
		revision            int
	}{

		{"research", "done", "done", 3},
	} {
		out, err := review.Execute(ctx, map[string]any{"task": tc.id, "revision": tc.revision, "accept": true})
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if result["status"] != tc.status || result["displayStatus"] != tc.display || result["acceptedRevision"] != float64(tc.revision) {
			t.Fatalf("misleading review result: %s", out)
		}
		if tc.id == "changed" && !strings.Contains(stringValue(result["nextAction"]), "swarm_integrate") {
			t.Fatalf("missing integration recovery: %s", out)
		}
		if tc.id == "research" && result["nextAction"] != nil {
			t.Fatalf("completed review still asks for work: %s", out)
		}
	}
	list, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	out, err = list.Execute(ctx, map[string]any{"view": "tasks"})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []taskToolView `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]taskToolView{}
	for _, task := range page.Items {
		tasks[task.ID] = task
	}
	if tasks["changed"].Status != "awaiting_review" || tasks["changed"].DisplayStatus != "integration pending" || tasks["research"].DisplayStatus != "done" {
		t.Fatalf("task list lost machine or display status: %s", out)
	}
}

func TestReviewToolGuidanceForReleasedOrMissingProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	if err := r.update(ctx, func(s *State) error {
		s.Members["released"] = &Member{ID: "released", Control: MemberControlEnabled, Context: "removed"}
		s.Tasks["task"] = &Task{ID: "task", Owner: "released", Status: "awaiting_review", Revision: 2, Snapshot: "missing"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	review, _, _ := r.config.Registry.GetIfAllowed("swarm_review")
	out, err := review.Execute(ctx, map[string]any{"task": "task", "revision": 2, "accept": true})
	if err == nil || !strings.Contains(err.Error(), "swarm_integrate") || out != "" {
		t.Fatalf("editing acceptance should be refused: %q %v", out, err)
	}
	out, err = review.Execute(ctx, map[string]any{"task": "task", "revision": 2, "accept": false, "feedback": "revise the summary"})
	if err != nil || !strings.Contains(out, "followup_task") {
		t.Fatalf("released member cannot revise: %q %v", out, err)
	}
}

func TestReleaseToolRefusesWithoutCompletingTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "research", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control, _, _ := r.config.Registry.GetIfAllowed("swarm_control")
	out, err := control.Execute(ctx, map[string]any{"action": "release", "id": state.Members[result.Session].Context})
	if err != nil || !strings.Contains(out, `"status": "ineligible"`) {
		t.Fatalf("cleanup: %q %v", out, err)
	}
	state, err = r.State(ctx)
	if err != nil || state.Members[result.Session].Control != MemberControlEnabled {
		t.Fatalf("release refusal changed member control: %+v %v", state, err)
	}
}

func TestFailedWorkflowToolRetainsItsReport(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	out, err := tool.Execute(context.Background(), map[string]any{
		"source": `polly.defineWorkflow({name:"failure",inputSchema:polly.schema.object({}),async run(){throw new Error("inspect this report")}})`,
		"input":  "{}",
	})
	if err == nil || !strings.Contains(out, `"status": "failed"`) || !strings.Contains(out, `"id":`) {
		t.Fatalf("failed workflow lost its inspectable report: %q %v", out, err)
	}
}

// A file path submitted as source is a caller's mistake, not a run: the tool
// names it, and no workflow record is saved for something that never started.
// Left to the engine, a path parses as an expression and is reported as an
// undefined variable or a regular-expression flag naming the caller's home.
func TestWorkflowToolRejectsAPathAsSourceWithoutSavingAReport(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	ctx := context.Background()
	for _, source := range []string{"skills/builtin/feature-workflow/feature-implement.js", "/tmp/workflows/review.js"} {
		for _, background := range []bool{false, true} {
			out, err := tool.Execute(ctx, map[string]any{"source": source, "input": "{}", "background": background})
			if err == nil {
				t.Fatalf("workflow_run accepted the path %q (background %v): %q", source, background, out)
			}
			if !strings.Contains(err.Error(), "file path") || strings.Contains(err.Error(), "is not defined") {
				t.Fatalf("workflow_run error for %q does not name the mistake: %v", source, err)
			}
		}
	}
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Workflows) != 0 {
		t.Fatalf("rejected sources left workflow records: %+v", state.Workflows)
	}
}

// A model that copies a skill's script into source re-emits every byte and
// changes some. Named by skill and path, the file is read by the host: the
// text that runs, and that the report saves, is the file's.
func TestWorkflowToolRunsASkillScriptFromItsFile(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	root := t.TempDir()
	dir := filepath.Join(root, "demo-skill")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// The trailing newline and the quoting are what a retyped copy loses.
	script := "// shipped with the skill\npolly.defineWorkflow({name:\"from-file\",inputSchema:polly.schema.object({word:polly.schema.string()}),async run(input){return \"ran \" + input.word + \" and \\\"quoted\\\"\"}})\n"
	for name, text := range map[string]string{"SKILL.md": "---\nname: demo-skill\ndescription: test skill\n---\nRun workflow.js.\n", "workflow.js": script} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// A real script beside the skill directory: only containment refuses it.
	if err := os.WriteFile(filepath.Join(root, "outside.js"), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.NewSkillRuntime(catalog, r.config.Registry); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	ctx := context.Background()
	out, err := tool.Execute(ctx, map[string]any{"skill": "demo-skill", "path": "workflow.js", "input": `{"word":"it"}`})
	if err != nil || !strings.Contains(out, `"status": "completed"`) || !strings.Contains(out, `ran it and \"quoted\"`) {
		t.Fatalf("skill script did not run: %q %v", out, err)
	}
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Workflows) != 1 {
		t.Fatalf("workflows: %+v", state.Workflows)
	}
	for _, saved := range state.Workflows {
		if saved.Source != script {
			t.Fatalf("saved source is not the file's text: %q", saved.Source)
		}
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"both forms", map[string]any{"skill": "demo-skill", "path": "workflow.js", "source": script, "input": "{}"}, "not both"},
		{"skill without path", map[string]any{"skill": "demo-skill", "input": "{}"}, "together"},
		{"path without skill", map[string]any{"path": "workflow.js", "input": "{}"}, "together"},
		{"neither form", map[string]any{"input": "{}"}, "needs source"},
		{"unknown skill", map[string]any{"skill": "nope", "path": "workflow.js", "input": "{}"}, "not found"},
		{"missing file", map[string]any{"skill": "demo-skill", "path": "absent.js", "input": "{}"}, "absent.js"},
		{"path outside the skill", map[string]any{"skill": "demo-skill", "path": "../outside.js", "input": "{}"}, "cannot read"},
	} {
		if out, err := tool.Execute(ctx, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %q %v, want an error naming %q", tc.name, out, err, tc.want)
		}
	}
	if state, _ = r.State(ctx); len(state.Workflows) != 1 {
		t.Fatalf("refused calls left workflow records: %+v", state.Workflows)
	}
}

// Without a skill catalog there is no read_skill_file to load a script with.
func TestWorkflowToolNamesMissingSkillsWhenAskedForAScript(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	if out, err := tool.Execute(context.Background(), map[string]any{"skill": "demo-skill", "path": "workflow.js", "input": "{}"}); err == nil || !strings.Contains(err.Error(), "no skills are available") {
		t.Fatalf("%q %v", out, err)
	}
}

// The builtin feature-workflow scripts are the largest the tool is asked to
// load; each must come through whole, under the skill file size limit.
func TestWorkflowToolLoadsTheBuiltinFeatureWorkflowScripts(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	catalog, err := skills.Discover([]string{"../skills/builtin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.NewSkillRuntime(catalog, r.config.Registry); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	ctx := context.Background()
	for _, name := range []string{"feature-research.js", "feature-implement.js"} {
		want, err := os.ReadFile(filepath.Join("../skills/builtin/feature-workflow", name))
		if err != nil {
			t.Fatal(err)
		}
		// Empty input stops the run at input validation, after the source
		// was loaded, compiled, and saved.
		out, err := tool.Execute(ctx, map[string]any{"skill": "feature-workflow", "path": name, "input": "{}"})
		if err == nil || !strings.Contains(out, "workflow input") {
			t.Fatalf("%s: %q %v", name, out, err)
		}
		state, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, saved := range state.Workflows {
			found = found || saved.Source == string(want)
		}
		if !found {
			t.Fatalf("%s did not run from its file's exact text", name)
		}
	}
}
