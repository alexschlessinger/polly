package workflow

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestFeatureResearchRecipe drives the builtin feature-workflow skill's
// feature-research.js through a fake host: lens researchers return structured
// reports, the plan synthesizer merges them, and the workflow validates the
// plan (unique task ids, known dependencies, acyclic) before returning it.
func TestFeatureResearchRecipe(t *testing.T) {
	source, err := os.ReadFile("../skills/builtin/feature-workflow/feature-research.js")
	if err != nil {
		t.Fatal(err)
	}
	task := func(id string, deps ...string) map[string]any {
		d := []any{}
		for _, dep := range deps {
			d = append(d, dep)
		}
		return map[string]any{"id": id, "title": id, "brief": "do " + id,
			"paths": []any{}, "dependsOn": d, "acceptance": []any{id + " works"}}
	}
	planWithTasks := func(tasks []any) map[string]any {
		return map[string]any{"summary": "the plan", "checks": []any{"make test"}, "tasks": tasks,
			"docsUpdates": []any{}, "risks": []any{}, "openQuestions": []any{}}
	}
	for _, tc := range []struct {
		name    string
		tasks   []any
		wantErr string
	}{
		{name: "clean", tasks: []any{task("core"), task("cli", "core")}},
		{name: "duplicate ids", tasks: []any{task("core"), task("core")}, wantErr: "unique"},
		{name: "unknown dependency", tasks: []any{task("cli", "nope")}, wantErr: "unknown task"},
		{name: "cycle", tasks: []any{task("a", "b"), task("b", "a")}, wantErr: "cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var synthInput map[string]any
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				if op.Kind != "agent" {
					t.Fatalf("unexpected operation %s", op.Kind)
				}
				label := op.Args["label"].(string)
				input := op.Args["input"].(map[string]any)
				switch {
				case strings.HasSuffix(label, " researcher"):
					if input["spec"] != "the spec" || input["focus"] == nil || input["name"] != "feat" {
						t.Fatalf("researcher lost spec, focus, or name: %#v", input)
					}
					return map[string]any{"task": "task-" + label, "session": "s", "value": map[string]any{
						"summary": label + " summary",
						"findings": []any{map[string]any{
							"topic": "t", "detail": "d", "paths": []any{"x.go"}, "evidence": "e"}},
						"recommendations": []any{"r"}, "unknowns": []any{}}}, nil
				case label == "plan synthesizer":
					synthInput = input
					return map[string]any{"task": "task-synth", "session": "s", "value": planWithTasks(tc.tasks)}, nil
				}
				t.Fatalf("unexpected label %s", label)
				return nil, nil
			})
			r := Runner{Host: host}
			report, err := r.Run(context.Background(), string(source), map[string]any{
				"name": "feat", "spec": "the spec",
				"lenses": []any{
					map[string]any{"id": "codebase", "focus": "f1"},
					map[string]any{"id": "testing", "focus": "f2"},
				}})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected %q failure, got report=%+v err=%v", tc.wantErr, report, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// The synthesizer saw every report tagged by lens.
			research := synthInput["research"].([]any)
			if len(research) != 2 {
				t.Fatalf("synthesizer research: %#v", research)
			}
			for i, lens := range []string{"codebase", "testing"} {
				row := research[i].(map[string]any)
				if row["lens"] != lens || row["task"] != "task-"+lens+" researcher" {
					t.Fatalf("research row %d: %#v", i, row)
				}
			}
			output := report.Output.(map[string]any)
			plan := output["plan"].(map[string]any)
			if len(plan["tasks"].([]any)) != 2 || output["synthesizer"] != "task-synth" {
				t.Fatalf("output: %#v", output)
			}
		})
	}
}
