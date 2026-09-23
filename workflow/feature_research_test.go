package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestFeatureResearchRecipe drives the builtin feature-workflow skill's
// feature-research.js through a fake host: the source is captured once and
// every agent reads that commit, lens researchers return structured reports,
// a failed optional lens becomes a gap, the plan synthesizer merges the
// reports, and a plan that fails validation (unique task ids, known
// dependencies, acyclic, no shared paths inside a wave) goes back to the
// synthesizer's own session at most twice before the run fails with the
// research and the last plan attached. Every check is first run once on the
// pinned commit in a disposable copy: one that cannot run there or leaves files
// behind goes back to the synthesizer too, and one still unusable after the
// repairs is handed back beside the plan instead of failing the run.
func TestFeatureResearchRecipe(t *testing.T) {
	source, err := os.ReadFile("../skills/builtin/feature-workflow/feature-research.js")
	if err != nil {
		t.Fatal(err)
	}
	implement, err := os.ReadFile("../skills/builtin/feature-workflow/feature-implement.js")
	if err != nil {
		t.Fatal(err)
	}
	task := func(id, path string, deps ...string) map[string]any {
		d := []any{}
		for _, dep := range deps {
			d = append(d, dep)
		}
		return map[string]any{"id": id, "title": id, "brief": "do " + id,
			"paths": []any{path}, "dependsOn": d, "acceptance": []any{id + " works"}}
	}
	planWithTasks := func(tasks, checks []any) map[string]any {
		return map[string]any{"summary": "the plan", "checks": checks, "finalChecks": []any{"make ci"},
			"tasks": tasks, "docsUpdates": []any{}, "risks": []any{}, "openQuestions": []any{}, "environmentNotes": []any{}}
	}
	// What a check prints on the pinned commit, and what git status then
	// lists in its copy.
	type run struct {
		exit   int
		text   string
		status string
		err    *Error
	}
	runs := map[string]run{
		"make test":            {text: "ok"},
		"broken":               {exit: 127, text: "sh: broken: command not found"},
		"go test ./x":          {exit: 1, text: "FAIL\texample.com/x [setup failed]\nFAIL"},
		"go build ./cmd/tool":  {text: "", status: "?? tool.exe"},
		"go test ./...":        {exit: 1, text: "--- FAIL: TestEnvironment (0.01s)\nFAIL\nFAIL\texample.com/pkg\t0.1s\nFAIL"},
		"no shell":             {err: &Error{Code: "tool_denied", Message: "bash is not available"}},
		"node tools/smoke.mjs": {text: "ok"},
	}
	const status = "git status --porcelain --untracked-files=all"
	// An ordered pair may share a path: the second task starts from the
	// first one's integrated result.
	clean := []any{task("core", "pkg/"), task("cli", "pkg/cli.go", "core")}
	customLenses := []any{
		map[string]any{"id": "codebase", "focus": "f1", "required": true},
		map[string]any{"id": "external", "focus": "f2"},
	}
	for _, tc := range []struct {
		name string
		// plans holds the tasks the synthesizer returns first and then on
		// each repair; the last entry repeats once the list runs out.
		plans [][]any
		// checks holds the plan's checks per round the same way; empty means
		// the default ["make test"].
		checks         [][]any
		failLens       string
		noSnapshot     bool
		defaultLenses  bool
		wantErr        string
		wantRepairs    int
		wantNoSynth    bool
		wantGap        string
		wantResearched []string
		// wantRepairKind is a problem kind the first repair must name;
		// wantCheckProblem is the kind handed back beside the plan.
		wantRepairKind   string
		wantCheckProblem string
		// wantFailures counts the failing tests the preflight saw; noPreflight
		// means no check ran before planning.
		wantFailures int
		noPreflight  bool
		// hostNotes go to every agent; missing is what the probe for a
		// check's harness prints; wantCreatedBy names the task the preflight
		// credits with the harness; wantNotRun is a check that must not run.
		hostNotes     []any
		missing       string
		wantCreatedBy string
		wantNotRun    string
	}{
		{name: "clean", plans: [][]any{clean}, wantResearched: []string{"codebase", "external"}},
		{name: "default lenses", plans: [][]any{clean}, defaultLenses: true,
			wantResearched: []string{"codebase", "conventions", "verification", "external", "docs-config"}},
		{name: "source outside git is read live", plans: [][]any{clean}, noSnapshot: true, noPreflight: true,
			wantResearched: []string{"codebase", "external"}},
		{name: "duplicate ids repaired", plans: [][]any{{task("core", "a.go"), task("core", "b.go")}, clean},
			wantRepairs: 1, wantResearched: []string{"codebase", "external"}},
		{name: "repaired on the second attempt", plans: [][]any{{task("cli", "a.go", "nope")}, {task("a", "a.go", "b"), task("b", "b.go", "a")}, clean},
			wantRepairs: 2, wantResearched: []string{"codebase", "external"}},
		{name: "duplicate ids", plans: [][]any{{task("core", "a.go"), task("core", "b.go")}}, wantErr: "unique", wantRepairs: 2,
			wantResearched: []string{"codebase", "external"}},
		{name: "unknown dependency", plans: [][]any{{task("cli", "a.go", "nope")}}, wantErr: "unknown task", wantRepairs: 2,
			wantResearched: []string{"codebase", "external"}},
		{name: "self dependency", plans: [][]any{{task("cli", "a.go", "cli")}}, wantErr: "depends on itself", wantRepairs: 2,
			wantResearched: []string{"codebase", "external"}},
		{name: "cycle", plans: [][]any{{task("a", "a.go", "b"), task("b", "b.go", "a")}}, wantErr: "cycle", wantRepairs: 2,
			wantResearched: []string{"codebase", "external"}},
		{name: "concurrent tasks share a directory", plans: [][]any{{task("core", "./pkg/"), task("cli", "pkg/cli/main.go")}},
			wantErr: "run concurrently in wave 1", wantRepairs: 2, wantResearched: []string{"codebase", "external"}},
		{name: "optional lens fails", plans: [][]any{clean}, failLens: "external", wantGap: "external",
			wantResearched: []string{"codebase"}},
		{name: "required lens fails", plans: [][]any{clean}, failLens: "codebase", wantErr: "Required research failed: codebase",
			wantNoSynth: true, wantGap: "codebase", wantResearched: []string{"external"}},
		// A check that fails naming no test would block every wave.
		{name: "check naming no test is repaired", plans: [][]any{clean}, checks: [][]any{{"broken"}, {"make test"}},
			wantRepairs: 1, wantRepairKind: "check_cannot_run", wantResearched: []string{"codebase", "external"}},
		// A package that cannot set up runs none of its tests.
		{name: "check whose package cannot set up is repaired", plans: [][]any{clean}, checks: [][]any{{"go test ./x"}, {"make test"}},
			wantRepairs: 1, wantRepairKind: "check_cannot_run", wantResearched: []string{"codebase", "external"}},
		// The parent runs checks in the user's own tree.
		{name: "check leaving files is repaired", plans: [][]any{clean}, checks: [][]any{{"go build ./cmd/tool"}, {"make test"}},
			wantRepairs: 1, wantRepairKind: "check_leaves_files", wantResearched: []string{"codebase", "external"}},
		// A test that already fails on the unchanged code is only counted.
		{name: "failing test on the unchanged code is not a problem", plans: [][]any{clean}, checks: [][]any{{"go test ./..."}},
			wantFailures: 1, wantResearched: []string{"codebase", "external"}},
		{name: "unusable check is left for the gate", plans: [][]any{clean}, checks: [][]any{{"broken"}},
			wantRepairs: 2, wantCheckProblem: "check_cannot_run", wantResearched: []string{"codebase", "external"}},
		{name: "unusable check beside a structural problem fails", plans: [][]any{{task("core", "a.go"), task("core", "b.go")}},
			checks: [][]any{{"broken"}}, wantErr: "unique", wantRepairs: 2, wantResearched: []string{"codebase", "external"}},
		// Without a shell nothing can run a check; that is not the plan's fault.
		{name: "checks are not run without a shell", plans: [][]any{clean}, checks: [][]any{{"no shell"}},
			noPreflight: true, wantResearched: []string{"codebase", "external"}},
		// A check names a path a task creates: the harness does not exist on
		// the unchanged code, so the check is recognised instead of run.
		{name: "check whose harness a task creates is not a problem", plans: [][]any{{task("smoke-harness", "tools/smoke.mjs"), task("cli", "pkg/cli.go")}},
			checks: [][]any{{"node tools/smoke.mjs"}}, missing: "tools/smoke.mjs", wantCreatedBy: "smoke-harness", wantNotRun: "node tools/smoke.mjs",
			wantResearched: []string{"codebase", "external"}},
		// The task only edits the harness: the probe finds it, and the check
		// runs on the unchanged code like any other.
		{name: "check naming a path a task only edits runs on the baseline", plans: [][]any{{task("harness", "tools/"), task("cli", "pkg/cli.go")}},
			checks: [][]any{{"node tools/smoke.mjs"}}, wantResearched: []string{"codebase", "external"}},
		// A repair that drops the creating task turns the same check into a
		// problem: nothing lists the path any more.
		{name: "created-by check whose task a repair drops becomes a problem",
			plans:  [][]any{{task("smoke-harness", "tools/smoke.mjs"), task("smoke-harness", "x.go")}, clean},
			checks: [][]any{{"node tools/smoke.mjs"}}, missing: "tools/smoke.mjs", wantRepairs: 2, wantRepairKind: "duplicate_id",
			wantCheckProblem: "check_cannot_run", wantNotRun: "node tools/smoke.mjs", wantResearched: []string{"codebase", "external"}},
		{name: "host notes reach every agent", plans: [][]any{clean}, hostNotes: []any{"headless chrome never exits"},
			wantResearched: []string{"codebase", "external"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var synthInput map[string]any
			var released, repairKinds []string
			contexts, snapshots, synths, repairs, copies := 0, 0, 0, 0, 0
			ran := map[string]string{} // check copy -> the command run in it
			execs := map[string]int{}
			nextPlan := func() map[string]any {
				i := synths + repairs - 1
				checks := []any{"make test"}
				if len(tc.checks) > 0 {
					checks = tc.checks[min(i, len(tc.checks)-1)]
				}
				return planWithTasks(tc.plans[min(i, len(tc.plans)-1)], checks)
			}
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				switch op.Kind {
				case "log":
					return nil, nil
				case "context":
					if op.Args["disposable"] == true {
						// A check runs on the pinned commit in a copy that is
						// released whatever the check leaves in it.
						if op.Args["commit"] != "base-commit" || op.Args["source"] != nil || op.Args["readOnly"] != nil {
							t.Errorf("check copy args: %#v", op.Args)
						}
						copies++
						return fmt.Sprint("ctx-check-", copies), nil
					}
					contexts++
					if op.Args["source"] != "/repo" || op.Args["readOnly"] != true {
						t.Errorf("pin context args: %#v", op.Args)
					}
					return "ctx-pin", nil
				case "exec":
					copy, command := fmt.Sprint(op.Args["context"]), fmt.Sprint(op.Args["command"])
					if !strings.HasPrefix(copy, "ctx-check-") || op.Args["check"] != false {
						t.Errorf("exec args: %#v", op.Args)
					}
					if strings.HasPrefix(command, "for p in ") {
						// The probe for a check's harness prints the paths
						// that do not exist; it is not a check run.
						if !strings.Contains(command, "'tools/smoke.mjs'") {
							t.Errorf("probe names the wrong path: %s", command)
						}
						return map[string]any{"exitCode": 0, "text": tc.missing}, nil
					}
					if command == status {
						return map[string]any{"exitCode": 0, "text": runs[ran[copy]].status}, nil
					}
					r, ok := runs[command]
					if !ok {
						t.Errorf("unexpected check %q", command)
					}
					ran[copy] = command
					execs[command]++
					if r.err != nil {
						return nil, r.err
					}
					return map[string]any{"exitCode": r.exit, "text": r.text}, nil
				case "snapshot":
					snapshots++
					if op.Args["context"] != "ctx-pin" {
						t.Errorf("snapshot args: %#v", op.Args)
					}
					if tc.noSnapshot {
						return nil, errors.New("snapshot requires an isolated Git checkout")
					}
					return map[string]any{"commit": "base-commit", "tree": "tree", "source": "/repo"}, nil
				case "release":
					released = append(released, fmt.Sprint(op.Args["context"]))
					return map[string]any{"released": op.Args["context"]}, nil
				case "agent":
				default:
					return nil, fmt.Errorf("unexpected operation: %+v", op)
				}
				if op.Args["session"] == "session-synth" {
					// A repair continues the synthesizer's own session, which
					// inherits its context: naming a copy again is refused.
					if op.Args["commit"] != nil || op.Args["source"] != nil || op.Args["label"] != nil {
						t.Errorf("repair continuation names a context or label: %#v", op.Args)
					}
					problems := op.Args["input"].(map[string]any)["problems"].([]any)
					if len(problems) == 0 || problems[0].(map[string]any)["message"] == "" {
						t.Errorf("repair got no problems: %#v", op.Args["input"])
					}
					if repairs == 0 {
						for _, p := range problems {
							repairKinds = append(repairKinds, fmt.Sprint(p.(map[string]any)["kind"]))
							if p.(map[string]any)["kind"] == "check_leaves_files" && fmt.Sprint(p.(map[string]any)["paths"]) != "[tool.exe]" {
								t.Errorf("leftover problem does not name the file: %#v", p)
							}
						}
					}
					repairs++
					return map[string]any{"task": fmt.Sprint("task-repair-", repairs), "session": "session-synth", "value": nextPlan()}, nil
				}
				// Every new agent reads the one pinned capture; only a source
				// that cannot be captured is named directly.
				if tc.noSnapshot {
					if op.Args["source"] != "/repo" || op.Args["commit"] != nil {
						t.Errorf("agent does not read the live source: %#v", op.Args)
					}
				} else if op.Args["commit"] != "base-commit" || op.Args["source"] != nil {
					t.Errorf("agent does not read the pinned commit: %#v", op.Args)
				}
				if op.Args["readOnly"] != true {
					t.Errorf("agent is not read-only: %#v", op.Args)
				}
				label := op.Args["label"].(string)
				input := op.Args["input"].(map[string]any)
				if tc.hostNotes == nil {
					if input["hostNotes"] != nil {
						t.Errorf("%s got host notes from nowhere: %#v", label, input["hostNotes"])
					}
				} else if !reflect.DeepEqual(input["hostNotes"], tc.hostNotes) {
					t.Errorf("%s host notes: %#v", label, input["hostNotes"])
				}
				switch {
				case strings.HasSuffix(label, " researcher"):
					lens := strings.TrimSuffix(label, " researcher")
					if input["spec"] != "the spec" || input["focus"] == nil || input["name"] != "feat" {
						t.Errorf("researcher lost spec, focus, or name: %#v", input)
					}
					for _, other := range input["otherLenses"].([]any) {
						if other.(map[string]any)["id"] == lens {
							t.Errorf("%s researcher was told its own lens belongs to others", lens)
						}
					}
					if lens == tc.failLens {
						return nil, &Error{Code: "agent_failed", Message: "stream died", Session: "session-" + lens,
							Result: map[string]any{"task": "task-" + lens}}
					}
					return map[string]any{"task": "task-" + lens, "session": "session-" + lens, "value": map[string]any{
						"summary": lens + " summary",
						"findings": []any{map[string]any{
							"topic": "t", "detail": "d", "paths": []any{"x.go"}, "evidence": "e"}},
						"recommendations": []any{"r"}, "unknowns": []any{lens + " unknown"}}}, nil
				case label == "plan synthesizer":
					synths++
					synthInput = input
					return map[string]any{"task": "task-synth", "session": "session-synth", "value": nextPlan()}, nil
				}
				t.Errorf("unexpected label %s", label)
				return nil, errors.New("unexpected label")
			})
			r := Runner{Host: host}
			input := map[string]any{"name": "feat", "spec": "the spec", "source": "/repo"}
			if !tc.defaultLenses {
				input["lenses"] = customLenses
			}
			if tc.hostNotes != nil {
				input["hostNotes"] = tc.hostNotes
			}
			report, err := r.Run(context.Background(), string(source), input)
			if contexts != 1 || snapshots != 1 || len(released) != copies+1 || released[0] != "ctx-pin" {
				t.Fatalf("source pinned %d time(s), captured %d, released %v of %d check copies (err=%v)", contexts, snapshots, released, copies, err)
			}
			if repairs != tc.wantRepairs {
				t.Fatalf("repairs=%d, want %d (err=%v)", repairs, tc.wantRepairs, err)
			}
			if tc.wantRepairKind != "" && !slices.Contains(repairKinds, tc.wantRepairKind) {
				t.Fatalf("first repair problems %v, want %s", repairKinds, tc.wantRepairKind)
			}
			// A command is run once however many repairs keep it.
			for command, n := range execs {
				if n != 1 {
					t.Fatalf("check %q ran %d times", command, n)
				}
			}
			if tc.wantNotRun != "" && execs[tc.wantNotRun] != 0 {
				t.Fatalf("check %q ran although its harness does not exist yet", tc.wantNotRun)
			}
			if tc.wantNoSynth != (synths == 0) {
				t.Fatalf("synthesizer ran %d time(s)", synths)
			}
			var output map[string]any
			if tc.wantErr != "" {
				var failure *Error
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.As(err, &failure) {
					t.Fatalf("expected %q failure, got report=%+v err=%v", tc.wantErr, report, err)
				}
				// A failed run still hands back what it learned.
				output, _ = failure.Result.(map[string]any)
				if output == nil {
					t.Fatalf("failure carries no result: %#v", failure)
				}
				if !tc.wantNoSynth {
					if output["plan"] == nil || len(output["problems"].([]any)) == 0 || output["synthesizer"] == nil {
						t.Fatalf("validation failure lost the plan or its problems: %#v", output)
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				output = report.Output.(map[string]any)
			}
			if tc.noSnapshot {
				if _, ok := output["commit"]; ok {
					t.Fatalf("unpinned run reports a commit: %#v", output["commit"])
				}
			} else if output["commit"] != "base-commit" {
				t.Fatalf("commit: %#v", output["commit"])
			}
			// The output carries a digest per lens, never the full reports.
			research := output["research"].([]any)
			if len(research) != len(tc.wantResearched) {
				t.Fatalf("research: %#v", research)
			}
			for i, lens := range tc.wantResearched {
				row := research[i].(map[string]any)
				if row["lens"] != lens || row["task"] != "task-"+lens || row["summary"] != lens+" summary" ||
					len(row["unknowns"].([]any)) != 1 || row["report"] != nil {
					t.Fatalf("research row %d: %#v", i, row)
				}
			}
			gaps := output["gaps"].([]any)
			if tc.wantGap == "" {
				if len(gaps) != 0 {
					t.Fatalf("gaps: %#v", gaps)
				}
			} else if gap := gaps[0].(map[string]any); len(gaps) != 1 || gap["lens"] != tc.wantGap ||
				gap["reason"] != "stream died" || gap["task"] != "task-"+tc.wantGap {
				t.Fatalf("gaps: %#v", gaps)
			}
			if tc.wantErr != "" {
				return
			}
			// The synthesizer saw every full report tagged by lens, and was
			// told which lenses produced none.
			reports := synthInput["research"].([]any)
			if len(reports) != len(tc.wantResearched) {
				t.Fatalf("synthesizer research: %#v", reports)
			}
			for i, lens := range tc.wantResearched {
				row := reports[i].(map[string]any)
				if row["lens"] != lens || row["task"] != "task-"+lens || row["report"] == nil {
					t.Fatalf("synthesizer research row %d: %#v", i, row)
				}
			}
			if told := synthInput["gaps"].([]any); len(told) != len(gaps) ||
				(len(told) == 1 && told[0].(map[string]any)["lens"] != tc.wantGap) {
				t.Fatalf("synthesizer gaps: %#v", told)
			}
			plan := output["plan"].(map[string]any)
			preflight, _ := output["preflight"].([]any)
			if tc.noPreflight {
				if output["preflight"] != nil {
					t.Fatalf("checks reported as run: %#v", output["preflight"])
				}
			} else if tc.missing != "" {
				// A check whose harness is absent reports the task that
				// creates it, if any, instead of an exit code.
				row, _ := preflight[0].(map[string]any)
				createdBy, _ := row["createdBy"].(string)
				if len(preflight) != 1 || row["command"] != plan["checks"].([]any)[0] || fmt.Sprint(row["missing"]) != "["+tc.missing+"]" ||
					createdBy != tc.wantCreatedBy || row["exitCode"] != nil {
					t.Fatalf("preflight: %#v", output["preflight"])
				}
			} else if len(preflight) != 1 || preflight[0].(map[string]any)["command"] != plan["checks"].([]any)[0] ||
				fmt.Sprint(preflight[0].(map[string]any)["failures"]) != fmt.Sprint(tc.wantFailures) {
				t.Fatalf("preflight: %#v", output["preflight"])
			}
			left, _ := output["checkProblems"].([]any)
			if tc.wantCheckProblem == "" {
				if len(left) != 0 {
					t.Fatalf("checkProblems: %#v", left)
				}
			} else if len(left) != 1 || left[0].(map[string]any)["kind"] != tc.wantCheckProblem {
				t.Fatalf("checkProblems: %#v", left)
			}
			wantSynth := "task-synth"
			if tc.wantRepairs > 0 {
				wantSynth = fmt.Sprint("task-repair-", tc.wantRepairs)
			}
			if len(plan["tasks"].([]any)) != 2 || output["synthesizer"] != wantSynth || fmt.Sprint(output["repairs"]) != fmt.Sprint(tc.wantRepairs) {
				t.Fatalf("output: %#v", output)
			}
			// feature-implement.js keeps its own copy of the plan schema: the
			// plan returned here must clear its input validation and reach
			// the host.
			reached := errors.New("reached the host")
			_, err = (&Runner{Host: hostFunc(func(context.Context, Operation) (any, error) { return nil, reached })}).
				Run(context.Background(), string(implement), map[string]any{"name": "feat", "plan": plan})
			if err == nil || !strings.Contains(err.Error(), reached.Error()) {
				t.Fatalf("feature-implement.js refused the research plan: %v", err)
			}
		})
	}
}

// TestFeatureWorkflowScriptsShareHelperBlocks keeps the two copies of each
// shared helper block identical: a script has no imports, and research must
// judge a check, and recognise a check's harness, the way implementation
// will.
func TestFeatureWorkflowScriptsShareHelperBlocks(t *testing.T) {
	for _, block := range []string{"failure names", "check origins"} {
		var blocks []string
		for _, name := range []string{"feature-implement.js", "feature-research.js"} {
			source, err := os.ReadFile("../skills/builtin/feature-workflow/" + name)
			if err != nil {
				t.Fatal(err)
			}
			text := string(source)
			begin, end := strings.Index(text, "// "+block+": begin"), strings.Index(text, "// "+block+": end")
			if begin < 0 || end < begin || strings.Count(text, "// "+block+": begin") != 1 {
				t.Fatalf("%s: no single marked %s block", name, block)
			}
			blocks = append(blocks, text[begin:end])
		}
		if blocks[0] != blocks[1] {
			t.Fatalf("feature-implement.js and feature-research.js carry different %s blocks", block)
		}
	}
}

// TestFeatureResearchRejectsBadInput pins the input rules that protect a run
// before any agent starts: the feature name becomes a file name, and lens ids
// tag every report.
func TestFeatureResearchRejectsBadInput(t *testing.T) {
	source, err := os.ReadFile("../skills/builtin/feature-workflow/feature-research.js")
	if err != nil {
		t.Fatal(err)
	}
	lens := func(id string) map[string]any { return map[string]any{"id": id, "focus": "f"} }
	for _, tc := range []struct {
		name  string
		input map[string]any
	}{
		{name: "name with a path", input: map[string]any{"name": "../feat", "spec": "s"}},
		{name: "name not kebab-case", input: map[string]any{"name": "My Feature", "spec": "s"}},
		{name: "duplicate lens ids", input: map[string]any{"name": "feat", "spec": "s", "lenses": []any{lens("a"), lens("a")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				t.Errorf("host reached with bad input: %+v", op)
				return nil, errors.New("unexpected operation")
			})
			if report, err := (&Runner{Host: host}).Run(context.Background(), string(source), tc.input); err == nil {
				t.Fatalf("bad input accepted: %+v", report)
			}
		})
	}
}
