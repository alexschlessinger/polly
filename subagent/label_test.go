package subagent

import (
	"context"
	"strings"
	"testing"
)

func TestToolRequiresLabelOnlyForNewAgent(t *testing.T) {
	calls := 0
	tool := NewTool(func(_ context.Context, req Request) (Result, error) { calls++; return Result{Text: "done"}, nil })
	for _, label := range []string{"", " \n", strings.Repeat("x", 81), "bad\x00label"} {
		if _, err := tool.Execute(context.Background(), map[string]any{"task": "Inspect code", "label": label}); err == nil {
			t.Fatalf("accepted label %q", label)
		}
	}
	if calls != 0 {
		t.Fatal("invalid label invoked runner")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"task": "Continue inspection", "session": "existing"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("continuation did not run")
	}
}
