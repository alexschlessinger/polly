package workflow

import (
	"context"
	"fmt"
	"os"
	"reflect"
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
	// A plan whose check runs a harness that the second wave's task creates.
	harnessPlan := map[string]any{
		"summary": "the plan", "checks": []any{"node tools/smoke.mjs"}, "finalChecks": []any{"slow-suite"},
		"tasks": []any{
			map[string]any{"id": "core", "title": "core", "brief": "do core",
				"paths": []any{"core.go"}, "dependsOn": []any{}, "acceptance": []any{"core works"}},
			map[string]any{"id": "cli", "title": "cli", "brief": "do cli",
				"paths": []any{"cli.go", "tools/smoke.mjs"}, "dependsOn": []any{"core"}, "acceptance": []any{"cli works"}},
		},
		"docsUpdates": []any{}, "risks": []any{}, "openQuestions": []any{},
	}
	for _, tc := range []struct {
		name          string
		plan          map[string]any
		inputChecks   []any
		hostNotes     []any
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
		// The check is pre-existing in every wave, and these are the
		// package-level names it hands back as unverified.
		wantPreexisting bool
		wantUnverified  []any
		// missing is what the probe for a check's harness prints; wantChecks
		// counts the candidate check runs and wantSkippedBy the task wave 1's
		// check waits for.
		missing       string
		wantChecks    int
		wantSkippedBy string
		// ignoreOnReReview makes the re-review drop the required change it
		// was given, which costs one continuation of its session.
		ignoreOnReReview  bool
		wantContinuations int
		ignoreOnRetry     bool
		secondFinding     bool
		wantUnaccounted   string
	}{
		{name: "clean", wantCheck: "plan-check"},
		{name: "failed check repaired", inputChecks: []any{"check"}, wantCheck: "check", rejectChecks: 1, repairs: 1, wantBaselines: 1},
		{name: "repair budget exhausted", inputChecks: []any{"check"}, wantCheck: "check", rejectReviews: 5, repairs: 2, blocked: true},
		// The wave's own baseline fails the same way, so the check is a limit
		// of the environment and the wave integrates without a repair.
		{name: "failure already at the baseline does not block", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "--- FAIL: TestEnvironment (0.01s)\nFAIL", baselineExit: 1,
			baselineText: "--- FAIL: TestEnvironment (0.01s)\nFAIL", wantBaselines: 2, wantPreexisting: true},
		// A package that cannot set up on either commit ran no test at all:
		// the wave is not blocked by the environment, but the check verified
		// nothing, so the result names it for the caller to run.
		{name: "setup failure at the baseline too is unverified", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "FAIL\texample.com/cli [setup failed]\nFAIL", baselineExit: 1,
			baselineText: "FAIL\texample.com/cli [setup failed]\nFAIL", wantBaselines: 2, wantPreexisting: true,
			wantUnverified: []any{"example.com/cli [setup failed]"}},
		// Beside a case that fails at the baseline, only the package that
		// did not build is unverified.
		{name: "build failure at the baseline too is unverified", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "--- FAIL: TestEnvironment (0.01s)\nFAIL\nFAIL\tpkg/a\t0.1s\n# pkg/b\nb.go:3:1: undefined: x\nFAIL\tpkg/b [build failed]\nFAIL",
			baselineExit: 1, baselineText: "--- FAIL: TestEnvironment (0.01s)\nFAIL\nFAIL\tpkg/a\t0.1s\nFAIL\tpkg/b [build failed]\nFAIL",
			wantBaselines: 2, wantPreexisting: true, wantUnverified: []any{"pkg/b [build failed]"}},
		// One new name on top of the pre-existing failure is a regression.
		{name: "new failure beside a baseline failure blocks", inputChecks: []any{"check"}, wantCheck: "check",
			rejectChecks: 99, candidateText: "--- FAIL: TestEnvironment (0.01s)\n--- FAIL: TestRegression (0.02s)\nFAIL",
			baselineExit: 1, baselineText: "--- FAIL: TestEnvironment (0.01s)\nFAIL", wantBaselines: 3, repairs: 2, blocked: true},
		// A compile error names only its package, so it must not hide behind
		// a baseline that fails for another reason.
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
		// The check names a path a later wave's task creates: the probe finds
		// it absent in wave 1, so the check is skipped there and runs from the
		// creating task's wave on.
		{name: "check created by a later task is skipped until its wave", plan: harnessPlan, wantCheck: "node tools/smoke.mjs",
			missing: "tools/smoke.mjs", wantChecks: 1, wantSkippedBy: "cli"},
		// The same task only edits an existing harness: the probe finds the
		// file, and the check runs on every wave like any other.
		{name: "task that only edits the harness runs the check every wave", plan: harnessPlan, wantCheck: "node tools/smoke.mjs", wantChecks: 2},
		// Facts about the host reach the editors, the reviewer and the repairer.
		{name: "host notes reach every agent", inputChecks: []any{"check"}, wantCheck: "check", rejectChecks: 1, repairs: 1, wantBaselines: 1,
			hostNotes: []any{"headless chrome never exits"}},
		// A re-review that neither closes nor carries a required change is
		// asked once more in its own session before the wave proceeds.
		{name: "re-review that ignores a required change is asked once more", inputChecks: []any{"check"}, wantCheck: "check",
			rejectReviews: 1, repairs: 1, ignoreOnReReview: true, wantContinuations: 1},
		{name: "review retry that still omits a finding blocks integration", inputChecks: []any{"check"}, wantCheck: "check",
			rejectReviews: 1, repairs: 1, ignoreOnReReview: true, wantContinuations: 1,
			ignoreOnRetry: true, blocked: true, wantUnaccounted: "R1"},
		{name: "review retry must preserve findings already accounted for", inputChecks: []any{"check"}, wantCheck: "check",
			rejectReviews: 1, repairs: 1, ignoreOnReReview: true, wantContinuations: 1,
			secondFinding: true, blocked: true, wantUnaccounted: "R2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			applies, repairs, reviews, checks, baselines, continuations, version := 0, 0, 0, 0, 0, 0, 0
			checkedSinceReview := false // a candidate check ran before the next review
			waveFor := map[string]int{"core": 1, "cli": 2}
			candidate := func() map[string]any {
				// Every candidate lists the paths that differ from the parent;
				// a revised one carries the repair's new blob.
				return map[string]any{"id": fmt.Sprint("candidate-", version),
					"merged":    map[string]any{"commit": fmt.Sprint("commit-", version)},
					"parent":    map[string]any{"commit": "baseline"},
					"conflicts": []any{}, "inputs": []any{}, "repairs": []any{},
					"plan": map[string]any{"paths": []any{map[string]any{"path": "core.go",
						"after": map[string]any{"exists": true, "kind": "file", "object": fmt.Sprint("obj-", version)}}}}}
			}
			wantNotes := func(input map[string]any, who string) {
				if tc.hostNotes == nil {
					if input["hostNotes"] != nil {
						t.Errorf("%s got host notes from nowhere: %#v", who, input["hostNotes"])
					}
				} else if !reflect.DeepEqual(input["hostNotes"], tc.hostNotes) {
					t.Errorf("%s host notes: %#v", who, input["hostNotes"])
				}
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
					if op.Args["session"] == "session-review" {
						// The reviewer's own session is continued for exactly
						// the required changes its re-review left unaccounted.
						continuations++
						asked, _ := op.Args["input"].(map[string]any)["unaccounted"].([]any)
						if len(asked) != 1 || asked[0].(map[string]any)["id"] != "R1" || op.Args["label"] != nil {
							t.Errorf("review continuation: %#v", op.Args)
						}
						closed := []any{map[string]any{"id": "R1", "evidence": "checked"}}
						if tc.ignoreOnRetry {
							closed = []any{}
						}
						return map[string]any{"task": "review-again", "context": "ctx-review-again", "session": "session-review",
							"value": map[string]any{"approved": true, "feedback": "reviewed", "requiredChanges": []any{},
								"closed": closed}}, nil
					}
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
						wantNotes(input, "editor "+id)
						return map[string]any{"task": "task-" + id, "context": "ctx-" + id,
							"value": map[string]any{"summary": "did " + id, "filesChanged": []any{id + ".go"}, "notes": []any{}}}, nil
					case label == "wave reviewer":
						reviews++
						input := op.Args["input"].(map[string]any)
						wantNotes(input, "reviewer")
						// The reviewer reads the checks already run on this
						// commit, and is told which commits to diff.
						cand, _ := input["candidate"].(map[string]any)
						if cand["baseCommit"] != "baseline" || cand["commit"] == nil || cand["note"] == nil {
							t.Errorf("reviewer candidate: %#v", input["candidate"])
						}
						got, _ := input["checks"].([]any)
						if len(got) != 1 || got[0].(map[string]any)["command"] != tc.wantCheck {
							t.Errorf("reviewer checks: %#v", input["checks"])
						} else if got[0].(map[string]any)["skipped"] == nil && !checkedSinceReview {
							t.Errorf("reviewer %d ran before the wave's check", reviews)
						}
						checkedSinceReview = false
						rejected := reviews <= tc.rejectReviews
						value := map[string]any{"approved": !rejected, "feedback": "reviewed", "requiredChanges": []any{}, "closed": []any{}}
						if rejected {
							value["requiredChanges"] = []any{map[string]any{"id": "R1", "summary": "fix it", "paths": []any{"core.go"}}}
							if tc.secondFinding {
								value["requiredChanges"] = append(value["requiredChanges"].([]any), map[string]any{"id": "R2", "summary": "fix this too", "paths": []any{"core.go"}})
							}
						}
						if previous, ok := input["previousReview"].(map[string]any); ok {
							// A re-review sees the verdict, each repair's report
							// and the paths the repair changed.
							reps, _ := input["repairs"].([]any)
							if len(reps) == 0 || reps[len(reps)-1].(map[string]any)["summary"] != "fixed" {
								t.Errorf("re-review repairs: %#v", input["repairs"])
							}
							if !reflect.DeepEqual(input["changedSince"], []any{"core.go"}) {
								t.Errorf("re-review changedSince: %#v", input["changedSince"])
							}
							if open, _ := previous["requiredChanges"].([]any); len(open) > 0 && !rejected && !tc.ignoreOnReReview {
								value["closed"] = []any{map[string]any{"id": "R1", "evidence": "checked"}}
							}
							if tc.secondFinding {
								value["closed"] = []any{map[string]any{"id": "R2", "evidence": "checked"}}
							}
						} else if input["repairs"] != nil || input["changedSince"] != nil {
							t.Errorf("first review carries re-review fields: %#v", input)
						}
						return map[string]any{"task": fmt.Sprint("review-", reviews), "context": fmt.Sprint("ctx-review-", reviews),
							"session": "session-review", "value": value}, nil
					case strings.HasPrefix(label, "integration repair"):
						repairs++
						input := op.Args["input"].(map[string]any)
						wantNotes(input, "repairer")
						// The repairer gets the spec, the submissions and the
						// review it is repairing against.
						if input["spec"] != "the spec" || input["candidate"].(map[string]any)["commit"] == nil {
							t.Errorf("repair input: %#v", input)
						}
						if subs, _ := input["submissions"].([]any); len(subs) == 0 || subs[0].(map[string]any)["planTask"] == nil {
							t.Errorf("repair submissions: %#v", input["submissions"])
						}
						if review, ok := input["review"].(map[string]any); ok && review["requiredChanges"] == nil {
							t.Errorf("repair review: %#v", review)
						}
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
					if command, _ := op.Args["command"].(string); strings.HasPrefix(command, "for p in ") {
						// The probe for a check's harness prints the paths
						// that do not exist.
						if !strings.Contains(command, "'tools/smoke.mjs'") {
							t.Errorf("probe names the wrong path: %s", command)
						}
						return map[string]any{"exitCode": 0, "text": tc.missing}, nil
					}
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
					checkedSinceReview = true
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
			if tc.plan != nil {
				input["plan"] = tc.plan
			}
			if tc.inputChecks != nil {
				input["checks"] = tc.inputChecks
			}
			if tc.hostNotes != nil {
				input["hostNotes"] = tc.hostNotes
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
			if tc.wantChecks != 0 && checks != tc.wantChecks {
				t.Fatalf("candidate check runs=%d, want %d", checks, tc.wantChecks)
			}
			if continuations != tc.wantContinuations {
				t.Fatalf("review continuations=%d, want %d", continuations, tc.wantContinuations)
			}
			if tc.blocked {
				if applies != 0 {
					t.Fatal("blocked wave reached integration")
				}
				if tc.wantUnaccounted != "" && !strings.Contains(err.Error(), "still did not account for required changes: "+tc.wantUnaccounted) {
					t.Fatalf("missing review finding not reported: %v", err)
				}
				// A failed wave keeps its whole evidence: it is what the
				// caller must report.
				result, _ := report.Error.Result.(map[string]any)
				validations, _ := result["validations"].([]any)
				if len(validations) == 0 {
					t.Fatalf("blocked result has no validations: %#v", report.Error.Result)
				}
				checks := validations[len(validations)-1].(map[string]any)["checks"].([]any)
				if _, ok := checks[0].(map[string]any)["output"]; !ok {
					t.Fatalf("blocked result lost its check output: %#v", checks[0])
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
			landedRepairs := 0
			for i, wave := range waves {
				integration := wave.(map[string]any)["integration"].(map[string]any)
				if integration["status"] != "applied" {
					t.Fatalf("wave %d not applied: %#v", i+1, wave)
				}
				// A landed wave lists what each repair reported.
				for _, repair := range integration["repairs"].([]any) {
					if repair.(map[string]any)["summary"] != "fixed" || repair.(map[string]any)["reason"] == nil {
						t.Fatalf("wave %d repair: %#v", i+1, repair)
					}
					landedRepairs++
				}
				// A landed wave is summarized: the caller has the plan tasks,
				// and a passing check's output is not evidence of anything.
				for _, s := range wave.(map[string]any)["submissions"].([]any) {
					if _, ok := s.(map[string]any)["planTask"]; ok || s.(map[string]any)["report"] == nil {
						t.Fatalf("wave %d submission: %#v", i+1, s)
					}
				}
				checks := integration["checks"].([]any)
				if len(checks) != 1 || integration["review"] == nil || integration["validations"] != nil {
					t.Fatalf("wave %d integration: %#v", i+1, integration)
				}
				check := checks[0].(map[string]any)
				if _, ok := check["output"]; ok || check["command"] != tc.wantCheck {
					t.Fatalf("wave %d check: %#v", i+1, check)
				}
				// A check waiting on a file a later wave creates says so and
				// reports no exit; from that wave on it ran.
				if skipped, _ := check["skipped"].(map[string]any); (i == 0 && tc.wantSkippedBy != "") != (skipped != nil) ||
					skipped != nil && (skipped["createdBy"] != tc.wantSkippedBy || check["exitCode"] != nil || fmt.Sprint(skipped["paths"]) != "["+tc.missing+"]") {
					t.Fatalf("wave %d check: %#v", i+1, check)
				}
				if tc.wantPreexisting {
					if check["preexisting"] != true || fmt.Sprint(check["failures"]) == "0" || (tc.wantUnverified == nil) != (check["unverified"] == nil) ||
						tc.wantUnverified != nil && !reflect.DeepEqual(check["unverified"], tc.wantUnverified) {
						t.Fatalf("wave %d check: %#v", i+1, check)
					}
				}
			}
			if landedRepairs != tc.repairs {
				t.Fatalf("landed repairs=%d, want %d", landedRepairs, tc.repairs)
			}
			unverified, _ := output["unverified"].([]any)
			if tc.wantUnverified == nil {
				if len(unverified) != 0 {
					t.Fatalf("unverified: %#v", unverified)
				}
				return
			}
			if len(unverified) != len(waves) {
				t.Fatalf("unverified: %#v, want one entry per wave", unverified)
			}
			for i, entry := range unverified {
				e := entry.(map[string]any)
				if fmt.Sprint(e["wave"]) != fmt.Sprint(i+1) || e["command"] != tc.wantCheck || !reflect.DeepEqual(e["packages"], tc.wantUnverified) {
					t.Fatalf("unverified entry %d: %#v", i, e)
				}
			}
		})
	}
}
