package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckSourceRejectsPathsAndAcceptsScripts(t *testing.T) {
	paths := []string{
		"skills/builtin/feature-workflow/feature-implement.js",
		"feature-implement.js",
		"/Users/someone/workspace/project/workflow.js",
		"./feature-implement.js",
		"../workflows/review.mjs",
		"~/.pollytool/builtin-skills/feature-workflow/feature-plan.js",
		"  skills/builtin/feature-workflow/feature-review.js  ",
		"/opt/workflows/nightly",
	}
	for _, source := range paths {
		err := CheckSource(source)
		if err == nil {
			t.Fatalf("CheckSource(%q) accepted a file path", source)
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != "invalid_source" {
			t.Fatalf("CheckSource(%q) = %v, want an invalid_source workflow error", source, err)
		}
		if !strings.Contains(e.Message, "file path") || !strings.Contains(e.Message, "contents as source") {
			t.Fatalf("CheckSource(%q) message does not say what to do: %s", source, e.Message)
		}
	}
	scripts := []string{
		`polly.defineWorkflow({name:"x",inputSchema:polly.schema.object({}),async run(){return 1}})`,
		"polly.workflow('x', polly.schema.object({}), async function (input) { return input })",
		"// a comment mentioning skills/builtin/thing.js\npolly.defineWorkflow({name:\"x\",inputSchema:polly.schema.object({}),async run(){}})",
		"",
		"   ",
		"const path = 'a/b.js'; polly.defineWorkflow({name:\"x\",inputSchema:polly.schema.object({}),async run(){}})",
	}
	for _, source := range scripts {
		if err := CheckSource(source); err != nil {
			t.Fatalf("CheckSource(%q) rejected script source: %v", source, err)
		}
	}
}

// The guard runs before anything else, so a path never reaches the engine and
// is never reported as an undefined variable or a regular-expression flag.
func TestRunRejectsAPathBeforeEvaluatingIt(t *testing.T) {
	r := &Runner{Host: struct{ Host }{}}
	_, err := r.Run(context.Background(), "skills/builtin/feature-workflow/feature-implement.js", map[string]any{})
	if err == nil {
		t.Fatal("Run accepted a file path as source")
	}
	if strings.Contains(err.Error(), "is not defined") {
		t.Fatalf("path reached the engine: %v", err)
	}
	if !strings.Contains(err.Error(), "file path") {
		t.Fatalf("Run error does not name the mistake: %v", err)
	}
}
