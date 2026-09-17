package workflow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestFeatureImplementRecipe drives the builtin feature-workflow skill's
// feature-implement.js through a fake host: plan tasks run as editors in
// dependency waves, each wave is reviewed and checked on the merged
// candidate, repaired within bounds, and integrated before the next wave's
// editors start.
func TestFeatureImplementRecipe(t *testing.T) {
	source, err := os.ReadFile("../skills/builtin/feature-workflow/feature-implement.js")
	if err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{
		"summary": "the plan",
		"checks":  []any{"plan-check"},
		"tasks": []any{
			map[string]any{"id": "core", "title": "core", "brief": "do core",
				"paths": []any{"core.go"}, "dependsOn": []any{}, "acceptance": []any{"core works"}},
			map[string]any{"id": "cli", "title": "cli", "brief": "do cli",
				"paths": []any{"cli.go"}, "dependsOn": []any{"core"}, "acceptance": []any{"cli works"}},
		},
		"docsUpdates": []any{}, "risks": []any{}, "openQuestions": []any{},
	}
	for _, tc := range []struct {
		name          string
		inputChecks   []any
		wantCheck     string
		rejectChecks  int
		rejectReviews int
		repairs       int
		blocked       bool
	}{
		{name: "clean", wantCheck: "plan-check"},
		{name: "failed check repaired", inputChecks: []any{"check"}, wantCheck: "check", rejectChecks: 1, repairs: 1},
		{name: "repair budget exhausted", inputChecks: []any{"check"}, wantCheck: "check", rejectReviews: 5, repairs: 2, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			applies, repairs, reviews, checks, version := 0, 0, 0, 0, 0
			waveFor := map[string]int{"core": 1, "cli": 2}
			candidate := func() map[string]any {
				return map[string]any{"id": fmt.Sprint("candidate-", version),
					"merged":    map[string]any{"commit": fmt.Sprint("commit-", version)},
					"conflicts": []any{}, "inputs": []any{}, "repairs": []any{}}
			}
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				switch op.Kind {
				case "log":
					return nil, nil
				case "integration":
					switch op.Args["op"] {
					case "prepare", "read":
						return candidate(), nil
					case "revise":
						version++
						return candidate(), nil
					case "refresh":
						return candidate(), nil
					}
				case "integrate":
					applies++
					return map[string]any{"status": "applied", "receipt": map[string]any{"status": "applied"}}, nil
				case "agent":
					label := op.Args["label"].(string)
					switch {
					case strings.HasPrefix(label, "implement "):
						id := strings.TrimPrefix(label, "implement ")
						if waveFor[id] == 2 && applies == 0 {
							t.Errorf("dependent editor %s started before wave 1 integrated", id)
						}
						input := op.Args["input"].(map[string]any)
						if input["task"].(map[string]any)["id"] != id || input["spec"] != "the spec" {
							t.Errorf("editor %s got wrong input: %#v", id, input)
						}
						return map[string]any{"task": "task-" + id, "context": "ctx-" + id,
							"value": map[string]any{"summary": "did " + id, "filesChanged": []any{id + ".go"}, "notes": []any{}}}, nil
					case label == "wave reviewer":
						reviews++
						return map[string]any{"task": fmt.Sprint("review-", reviews), "context": fmt.Sprint("ctx-review-", reviews),
							"value": map[string]any{"approved": reviews > tc.rejectReviews, "feedback": "reviewed"}}, nil
					case strings.HasPrefix(label, "integration repair"):
						repairs++
						return map[string]any{"task": fmt.Sprint("repair-", repairs), "context": fmt.Sprint("ctx-repair-", repairs),
							"value": map[string]any{"summary": "fixed"}}, nil
					}
					t.Fatalf("unexpected agent label %s", label)
				case "task":
					return map[string]any{"id": op.Args["task"], "revision": 1}, nil
				case "context":
					return fmt.Sprint("ctx-check-", op.Args["commit"]), nil
				case "exec":
					checks++
					if op.Args["command"] != tc.wantCheck {
						t.Errorf("check command = %v, want %s", op.Args["command"], tc.wantCheck)
					}
					if checks <= tc.rejectChecks {
						return map[string]any{"exitCode": 1, "text": "CHECK_FAIL"}, nil
					}
					return map[string]any{"exitCode": 0, "text": "ok"}, nil
				case "release":
					return map[string]any{"released": op.Args["context"]}, nil
				}
				return nil, fmt.Errorf("unexpected operation: %+v", op)
			})
			r := Runner{Host: host}
			input := map[string]any{"name": "feat", "spec": "the spec", "plan": plan}
			if tc.inputChecks != nil {
				input["checks"] = tc.inputChecks
			}
			report, err := r.Run(context.Background(), string(source), input)
			if (err != nil) != tc.blocked {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if repairs != tc.repairs {
				t.Fatalf("repairs=%d, want %d", repairs, tc.repairs)
			}
			if tc.blocked {
				if applies != 0 {
					t.Fatal("blocked wave reached integration")
				}
				return
			}
			if applies != 2 {
				t.Fatalf("applies=%d, want one per wave", applies)
			}
			output := report.Output.(map[string]any)
			waves := output["waves"].([]any)
			if len(waves) != 2 {
				t.Fatalf("waves: %#v", waves)
			}
			for i, wave := range waves {
				if wave.(map[string]any)["integration"].(map[string]any)["status"] != "applied" {
					t.Fatalf("wave %d not applied: %#v", i+1, wave)
				}
			}
		})
	}
}
