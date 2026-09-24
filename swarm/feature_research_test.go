package swarm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// TestFeatureResearchRunsOnThePinnedCapture runs the builtin feature-workflow
// skill's feature-research.js on the real runtime. The script captures the
// source once, releases that context, and starts every agent from the
// captured commit, then repairs an invalid plan by continuing the
// synthesizer's finished read-only session with a schema. A researcher that
// answers with placeholder probes never delivers one as research: the report
// schema refuses them, its execution fails after the host's corrections, and
// its optional lens becomes a gap that names the paused session. Every check
// runs once in a disposable copy of the capture, and one that leaves a file
// behind goes back to the synthesizer with the plan's other problems. A source
// outside Git cannot be captured, and every agent reads it live instead.
func TestFeatureResearchRunsOnThePinnedCapture(t *testing.T) {
	source, err := os.ReadFile("../skills/builtin/feature-workflow/feature-research.js")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("evidence gathered from the code. ", 5)
	report := tools.Result(map[string]any{"summary": long,
		"findings":        []any{map[string]any{"topic": "t", "detail": long, "paths": []any{"a.txt"}, "evidence": "e"}},
		"recommendations": []any{"r"}, "unknowns": []any{}})
	// What a model sends to test whether its JSON parses: valid shape, no content.
	probe := tools.Result(map[string]any{"summary": "s",
		"findings":        []any{map[string]any{"topic": "t", "detail": "A", "paths": []any{"a"}, "evidence": "x"}},
		"recommendations": []any{"r"}, "unknowns": []any{"u"}})
	plan := func(second, check string) string {
		task := func(id string) map[string]any {
			return map[string]any{"id": id, "title": id, "brief": "do " + id + ": " + long, "paths": []any{id + ".txt"},
				"dependsOn": []any{}, "acceptance": []any{id + " works"}}
		}
		return tools.Result(map[string]any{"summary": long, "checks": []any{check}, "finalChecks": []any{},
			"tasks": []any{task("core"), task(second)}, "docsUpdates": []any{}, "risks": []any{}, "openQuestions": []any{}, "environmentNotes": []any{}})
	}
	for _, git := range []bool{true, false} {
		t.Run(map[bool]string{true: "checkout", false: "live"}[git], func(t *testing.T) {
			if git {
				skipIfWindows(t)
			}
			var researchers, probes, synths, repairs atomic.Int32
			var repairBrief atomic.Value
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var first, brief string
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleUser && !strings.HasPrefix(msg.Content, "<peer_messages>") {
						if first == "" {
							first = msg.Content
						}
						brief = msg.Content
					}
				}
				switch {
				case strings.HasPrefix(brief, "The plan you returned cannot be executed"):
					repairs.Add(1)
					repairBrief.Store(brief)
					return completion(plan("cli", "true"))
				case strings.HasPrefix(first, "You are the plan synthesizer"):
					synths.Add(1)
					// A duplicate id, and a check that leaves a file in the
					// copy: both are sent back in one repair.
					return completion(plan("core", "touch leftover.out"))
				case strings.HasPrefix(first, "You are the external researcher"):
					probes.Add(1) // the first answer and both corrections
					return completion(probe)
				}
				researchers.Add(1)
				return completion(report)
			})
			r := runtimeTest(t, model, 2, 8)
			if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
				t.Fatal(err)
			}
			// The assertions below read member contexts after the run.
			suspendAutoRelease(t, r)
			root := r.config.Root
			if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if git {
				for _, args := range [][]string{{"init", "-q"}, {"add", "."},
					{"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base"}} {
					cmd := exec.Command("git", args...)
					cmd.Dir = root
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git: %s %v", out, err)
					}
				}
				// The capture includes the parent's uncommitted work.
				if err := os.WriteFile(filepath.Join(root, "draft.txt"), []byte("draft\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result, err := r.RunWorkflow(ctx, string(source), map[string]any{"name": "feat", "spec": "the spec", "source": root,
				"lenses": []any{map[string]any{"id": "codebase", "focus": "f1", "required": true}, map[string]any{"id": "external", "focus": "f2"}}})
			if err != nil {
				t.Fatalf("workflow: %v %+v", err, result)
			}
			if researchers.Load() != 1 || probes.Load() != 3 || synths.Load() != 1 || repairs.Load() != 1 {
				t.Fatalf("researchers=%d probes=%d synths=%d repairs=%d", researchers.Load(), probes.Load(), synths.Load(), repairs.Load())
			}
			// The script logs a failed release instead of failing, so the
			// steps are where a leaked pin context would show.
			released, notes := 0, []string{}
			for _, step := range result.Steps {
				switch step.Kind {
				case "release":
					if step.Status != "completed" {
						t.Fatalf("pin context was not released: %+v", step)
					}
					released++
				case "log":
					notes = append(notes, step.Args["message"].(string))
				}
			}
			// The pin context, and in a checkout one copy per distinct check.
			if want := map[bool]int{true: 3, false: 1}[git]; released != want {
				t.Fatalf("released %d contexts, want %d", released, want)
			}
			if brief, _ := repairBrief.Load().(string); git != strings.Contains(brief, "check_leaves_files") ||
				git && !strings.Contains(brief, "leftover.out") {
				t.Fatalf("repair brief (git=%v): %s", git, brief)
			}
			wantNotes := []string{"continuing without external (typed result invalid after two corrections", "plan repair 1: task ids must be unique: core"}
			if !git {
				wantNotes = append([]string{"source is not pinned, agents read it as it is: snapshot requires an isolated Git checkout"}, wantNotes...)
			}
			if len(notes) != len(wantNotes) {
				t.Fatalf("log steps: %q", notes)
			}
			for i, want := range wantNotes {
				if !strings.HasPrefix(notes[i], want) {
					t.Fatalf("log step %d: %q, want prefix %q", i, notes[i], want)
				}
			}
			output := result.Output.(map[string]any)
			tasks := output["plan"].(map[string]any)["tasks"].([]any)
			if len(tasks) != 2 || tasks[1].(map[string]any)["id"] != "cli" {
				t.Fatalf("output: %#v", output)
			}
			// The probe never counts as research: the lens is a gap whose
			// reason is the refused value, and it names the session that
			// still holds the investigation.
			digest, gaps := output["research"].([]any), output["gaps"].([]any)
			if len(digest) != 1 || digest[0].(map[string]any)["lens"] != "codebase" || len(gaps) != 1 {
				t.Fatalf("research=%#v gaps=%#v", digest, gaps)
			}
			gap := gaps[0].(map[string]any)
			if gap["lens"] != "external" || !strings.Contains(gap["reason"].(string), "minLength") || gap["session"] == "" || gap["session"] == nil {
				t.Fatalf("gap: %#v", gap)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// The repair reused the synthesizer: three members, four executions.
			if len(s.Members) != 3 || len(s.Executions) != 4 {
				t.Fatalf("%d members %d executions", len(s.Members), len(s.Executions))
			}
			commit, _ := output["commit"].(string)
			if !git {
				if commit != "" {
					t.Fatalf("live source reported a commit: %q", commit)
				}
				return
			}
			if commit == "" {
				t.Fatalf("no pinned commit: %#v", output)
			}
			// Every member's checkout starts from the one pinned capture,
			// which carries the parent's uncommitted file.
			for _, m := range s.Members {
				c := s.Contexts[m.Context]
				if c == nil || c.Checkout == nil || c.Checkout.Base.Commit != commit {
					t.Fatalf("member %s does not read the pinned commit %s: %+v", m.Label, commit, c)
				}
			}
			show := exec.Command("git", "show", commit+":draft.txt")
			show.Dir = root
			if out, err := show.CombinedOutput(); err != nil || string(out) != "draft\n" {
				t.Fatalf("pinned capture lost the parent's draft: %q %v", out, err)
			}
		})
	}
}
