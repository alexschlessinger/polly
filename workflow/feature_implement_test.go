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
		// No wave runs a final check: the exec fake refuses any command
		// other than the wave check.
		"finalChecks": []any{"slow-suite"},
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
		candidateText string
		baselineExit  int
		baselineText  string
		wantBaselines int
		failEditor    string
		recoverWave2  bool
		wantStatus    string
		wantRemaining string
		wantApplies   int
		repairs       int
		blocked       bool
	}{
		{name: "clean", wantCheck: "plan-check"},
		{name: "failed check repaired", inputChecks: []any{"check"}, wantCheck: "check", rejectChecks: 1, repairs: 1, wantBaselines: 1},
		{name: "repair budget exhausted", inputChecks: []any{"check"}, wantCheck: "check", rejectReviews: 5, repairs: 2, blocked: true},
		// The wave's own baseline fails the same way, so the check is a limit
		// of the environment and the wave integrates without a repair.
		{name: "failure already at the baseline does not block", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "--- FAIL: TestEnvironment (0.01s)\nFAIL", baselineExit: 1,
			baselineText: "--- FAIL: TestEnvironment (0.01s)\nFAIL", wantBaselines: 2},
		// One new name on top of the pre-existing failure is a regression.
		{name: "new failure beside a baseline failure blocks", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "--- FAIL: TestEnvironment (0.01s)\n--- FAIL: TestRegression (0.02s)\nFAIL",
			baselineExit: 1, baselineText: "--- FAIL: TestEnvironment (0.01s)\nFAIL", wantBaselines: 3, repairs: 2, blocked: true},
		// A compile error names no test, so it must not hide behind a
		// baseline that fails for another reason.
		{name: "compile error beside a baseline failure blocks", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "# example.com/pkg\npkg.go:3:1: undefined: x\nFAIL\texample.com/pkg [build failed]\nFAIL",
			baselineExit: 1, baselineText: "--- FAIL: TestEnvironment (0.01s)\nFAIL", wantBaselines: 3, repairs: 2, blocked: true},
		// Output naming nothing on either side cannot be attributed to the
		// baseline, even when both exit codes match.
		{name: "unrecognised failure output blocks", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, baselineExit: 1, baselineText: "CHECK_FAIL", wantBaselines: 3, repairs: 2, blocked: true},
		// One name in two packages is two failures: Go's package line
		// qualifies the cases above it.
		{name: "same test name failing in a second package blocks", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tpkg/a\t0.1s\n--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tpkg/b\t0.1s",
			baselineExit: 1, baselineText: "--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tpkg/a\t0.1s", wantBaselines: 3, repairs: 2, blocked: true},
		// A package that fails without naming a case (a panic, a timeout)
		// names itself, which a baseline failing on a case does not match.
		{name: "package panic beside a baseline case failure blocks", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "panic: boom\nFAIL\tpkg/a\t0.1s",
			baselineExit: 1, baselineText: "--- FAIL: TestNet (0.01s)\nFAIL\nFAIL\tpkg/a\t0.1s", wantBaselines: 3, repairs: 2, blocked: true},
		// Jest and Vitest titles keep their spaces, so two titles that share
		// a first word stay distinct.
		{name: "new jest failure beside a baseline one blocks", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "  \u25cf suite \u203a renders the list\n  \u25cf suite \u203a renders the footer\n",
			baselineExit: 1, baselineText: "  \u25cf suite \u203a renders the list\n", wantBaselines: 3, repairs: 2, blocked: true},
		{name: "jest failure already at the baseline does not block", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "  \u2715 renders the list (12 ms)\n", baselineExit: 1,
			baselineText: "  \u2715 renders the list (9 ms)\n", wantBaselines: 2},
		// Names are read from the whole output, not the tail kept for readers.
		{name: "baseline failure named before the stored tail does not block", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "--- FAIL: TestEnvironment (0.01s)\n" + strings.Repeat("x", 7000) + "\nFAIL", baselineExit: 1,
			baselineText: "--- FAIL: TestEnvironment (0.01s)\n" + strings.Repeat("x", 7000) + "\nFAIL", wantBaselines: 2},
		// A transient failure in wave 2 must not discard wave 1, which is
		// already in the parent's files: the run reports what is left instead.
		{name: "later wave failure keeps the applied wave", wantCheck: "plan-check", failEditor: "cli",
			wantStatus: "incomplete", wantRemaining: "cli", wantApplies: 1},
		// An integrate that began without a confirmed outcome is neither
		// applied nor safe to implement again: the caller reconciles first.
		{name: "unconfirmed integrate of a later wave asks for recovery", wantCheck: "plan-check", recoverWave2: true,
			wantStatus: "recovery_required", wantApplies: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			applies, repairs, reviews, checks, baselines, version := 0, 0, 0, 0, 0, 0
			waveFor := map[string]int{"core": 1, "cli": 2}
			candidate := func() map[string]any {
				return map[string]any{"id": fmt.Sprint("candidate-", version),
					"merged":    map[string]any{"commit": fmt.Sprint("commit-", version)},
					"parent":    map[string]any{"commit": "baseline"},
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
					if tc.recoverWave2 && applies == 1 {
						return nil, &Error{Code: "recovery_required", Message: "inspect write"}
					}
					applies++
					return map[string]any{"status": "applied", "receipt": map[string]any{"status": "applied"}}, nil
				case "agent":
					label := op.Args["label"].(string)
					switch {
					case strings.HasPrefix(label, "implement "):
						id := strings.TrimPrefix(label, "implement ")
						if id == tc.failEditor {
							return nil, fmt.Errorf("stream died: connection reset by peer")
						}
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
					// Check copies are disposable, so an output a check leaves
					// behind cannot keep one from being released.
					if op.Args["disposable"] != true {
						t.Errorf("check copy is not disposable: %#v", op.Args)
					}
					return fmt.Sprint("ctx-check-", op.Args["commit"]), nil
				case "exec":
					if op.Args["command"] != tc.wantCheck {
						t.Errorf("check command = %v, want %s", op.Args["command"], tc.wantCheck)
					}
					// Contexts are named after their commit, so the wave's own
					// baseline re-run is distinguishable from the candidate's.
					if context, _ := op.Args["context"].(string); strings.Contains(context, "baseline") {
						baselines++
						return map[string]any{"exitCode": tc.baselineExit, "text": tc.baselineText}, nil
					}
					checks++
					if checks <= tc.rejectChecks {
						text := tc.candidateText
						if text == "" {
							text = "CHECK_FAIL"
						}
						return map[string]any{"exitCode": 1, "text": text}, nil
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
			if baselines != tc.wantBaselines {
				t.Fatalf("baseline re-runs=%d, want %d", baselines, tc.wantBaselines)
			}
			if tc.blocked {
				if applies != 0 {
					t.Fatal("blocked wave reached integration")
				}
				return
			}
			output := report.Output.(map[string]any)
			wantStatus := tc.wantStatus
			if wantStatus == "" {
				wantStatus = "applied"
			}
			if output["status"] != wantStatus {
				t.Fatalf("status=%v, want %s", output["status"], wantStatus)
			}
			wantApplies := tc.wantApplies
			if wantApplies == 0 {
				wantApplies = 2
			}
			if applies != wantApplies {
				t.Fatalf("applies=%d, want %d", applies, wantApplies)
			}
			waves := output["waves"].([]any)
			if len(waves) != wantApplies {
				t.Fatalf("waves: %#v", waves)
			}
			if tc.wantRemaining != "" {
				remaining := output["remaining"].([]any)
				if len(remaining) != 1 || remaining[0] != tc.wantRemaining {
					t.Fatalf("remaining=%#v, want [%s]", remaining, tc.wantRemaining)
				}
				if fmt.Sprint(output["stopped"].(map[string]any)["wave"]) != "2" {
					t.Fatalf("stopped: %#v", output["stopped"])
				}
				// The plan handed back relaunches as is: the remaining task
				// no longer depends on the applied one, which a relaunch
				// would refuse as unknown.
				relaunch := output["plan"].(map[string]any)
				left := relaunch["tasks"].([]any)
				if len(left) != 1 || left[0].(map[string]any)["id"] != tc.wantRemaining {
					t.Fatalf("relaunch plan tasks: %#v", left)
				}
				if deps, _ := left[0].(map[string]any)["dependsOn"].([]any); len(deps) != 0 {
					t.Fatalf("relaunch plan keeps dependencies on applied tasks: %#v", deps)
				}
				if relaunch["summary"] != plan["summary"] {
					t.Fatalf("relaunch plan summary: %#v", relaunch["summary"])
				}
			}
			if tc.recoverWave2 {
				if output["candidate"] == nil || output["remaining"] != nil {
					t.Fatalf("recovery result: %#v", output)
				}
				if fmt.Sprint(output["stopped"].(map[string]any)["wave"]) != "2" {
					t.Fatalf("stopped: %#v", output["stopped"])
				}
			}
			if wantStatus == "applied" {
				if final, _ := output["finalChecks"].([]any); len(final) != 1 || final[0] != "slow-suite" {
					t.Fatalf("applied result does not hand back the final checks: %#v", output["finalChecks"])
				}
			}
			for i, wave := range waves {
				if wave.(map[string]any)["integration"].(map[string]any)["status"] != "applied" {
					t.Fatalf("wave %d not applied: %#v", i+1, wave)
				}
			}
		})
	}
}
