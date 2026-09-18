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
// synthesizer's finished read-only session with a schema. A source outside
// Git cannot be captured, and every agent reads it live instead.
func TestFeatureResearchRunsOnThePinnedCapture(t *testing.T) {
	source, err := os.ReadFile("../skills/builtin/feature-workflow/feature-research.js")
	if err != nil {
		t.Fatal(err)
	}
	report := tools.Result(map[string]any{"summary": "s",
		"findings":        []any{map[string]any{"topic": "t", "detail": "d", "paths": []any{"a.txt"}, "evidence": "e"}},
		"recommendations": []any{"r"}, "unknowns": []any{}})
	plan := func(second string) string {
		task := func(id string) map[string]any {
			return map[string]any{"id": id, "title": id, "brief": "do " + id, "paths": []any{id + ".txt"},
				"dependsOn": []any{}, "acceptance": []any{id + " works"}}
		}
		return tools.Result(map[string]any{"summary": "p", "checks": []any{"true"}, "finalChecks": []any{},
			"tasks": []any{task("core"), task(second)}, "docsUpdates": []any{}, "risks": []any{}, "openQuestions": []any{}})
	}
	for _, git := range []bool{true, false} {
		t.Run(map[bool]string{true: "checkout", false: "live"}[git], func(t *testing.T) {
			if git {
				skipIfWindows(t)
			}
			var researchers, synths, repairs atomic.Int32
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var brief string
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleUser && !strings.HasPrefix(msg.Content, "<peer_messages>") {
						brief = msg.Content
					}
				}
				switch {
				case strings.HasPrefix(brief, "The plan you returned cannot be executed"):
					repairs.Add(1)
					return completion(plan("cli"))
				case strings.HasPrefix(brief, "You are the plan synthesizer"):
					synths.Add(1)
					return completion(plan("core")) // duplicate id: sent back once
				}
				researchers.Add(1)
				return completion(report)
			})
			r := runtimeTest(t, model, 2, 8)
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
			if researchers.Load() != 2 || synths.Load() != 1 || repairs.Load() != 1 {
				t.Fatalf("researchers=%d synths=%d repairs=%d", researchers.Load(), synths.Load(), repairs.Load())
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
			if released != 1 {
				t.Fatalf("released %d contexts, want the pin context alone", released)
			}
			wantNotes := []string{"plan repair 1: task ids must be unique: core"}
			if !git {
				wantNotes = append([]string{"source is not pinned, agents read it as it is: snapshot requires an isolated Git checkout"}, wantNotes...)
			}
			if strings.Join(notes, "\n") != strings.Join(wantNotes, "\n") {
				t.Fatalf("log steps: %q", notes)
			}
			output := result.Output.(map[string]any)
			tasks := output["plan"].(map[string]any)["tasks"].([]any)
			if len(tasks) != 2 || tasks[1].(map[string]any)["id"] != "cli" || len(output["gaps"].([]any)) != 0 {
				t.Fatalf("output: %#v", output)
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
