package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestFeatureScenarioReceivesIntegratedPrerequisite(t *testing.T) {
	t.Parallel()
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		var prompt strings.Builder
		read, wrote := false, false
		for _, m := range req.Messages {
			if m.Role == messages.MessageRoleUser {
				prompt.WriteString(m.Content)
			}
			if m.ToolName == "read_file" {
				read = true
				if !strings.Contains(m.Content, "working component") {
					t.Errorf("scenario did not receive integrated code: %s", m.Content)
				}
			}
			wrote = wrote || m.ToolName == "write_file"
		}
		if strings.Contains(prompt.String(), "Independently review this merged implementation candidate") {
			return completion(`{"approved":true,"feedback":"fixture reviewed","requiredChanges":[],"closed":[]}`)
		}
		if strings.Contains(prompt.String(), "exercise the integrated component") {
			if !read {
				return iterationTool("read", "read_file", `{"path":"component.txt"}`)
			}
			if !wrote {
				return iterationTool("write", "write_file", `{"path":"scenario.txt","content":"verified integrated component"}`)
			}
			return completion(`{"summary":"scenario added","filesChanged":["scenario.txt"],"notes":[]}`)
		}
		if !wrote {
			return iterationTool("write", "write_file", `{"path":"component.txt","content":"working component"}`)
		}
		return completion(`{"summary":"component implemented","filesChanged":["component.txt"],"notes":[]}`)
	})
	r := scratchRuntime(t, model, true)
	for _, tool := range []string{"read_file", "write_file"} {
		if _, err := r.config.Registry.LoadToolAuto(tool); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile("../skills/builtin/feature-workflow/feature-implement.js")
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"name": "dependency-scenario", "spec": "component with an integration scenario", "source": r.config.Root,
		"plan": map[string]any{
			"summary": "implement the component before adding its integration scenario",
			"tasks": []any{
				map[string]any{"id": "component", "title": "Component", "brief": "implement a component", "paths": []string{"component.txt"}, "dependsOn": []string{}, "acceptance": []string{"component works"}},
				map[string]any{"id": "scenario", "title": "Scenario", "brief": "exercise the integrated component", "paths": []string{"scenario.txt"}, "dependsOn": []string{"component"}, "acceptance": []string{"scenario uses working component"}},
			},
			"docsUpdates": []any{}, "risks": []any{}, "openQuestions": []any{},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := r.RunWorkflow(ctx, string(source), input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Output.(map[string]any)["status"] != "applied" {
		t.Fatalf("dependency waves failed: %+v", report.Output)
	}
	for file, want := range map[string]string{"component.txt": "working component", "scenario.txt": "verified integrated component"} {
		if content, err := os.ReadFile(filepath.Join(r.config.Root, file)); err != nil || string(content) != want {
			t.Fatalf("integrated %s = %q, %v", file, content, err)
		}
	}
}
