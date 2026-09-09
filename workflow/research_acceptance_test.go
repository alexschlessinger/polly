package workflow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestFixReviewConsumesResearchDespiteParallelFailure(t *testing.T) {
	source, err := os.ReadFile("../examples/workflows/fix-review-findings.js")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	accepted := []string{}
	checksFailed := make(chan struct{})
	host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
		switch op.Kind {
		case "agent":
			label := op.Args["label"].(string)
			value := map[string]any{"a": map[string]any{"status": "fixed", "what": "patched"}}
			if strings.Contains(label, "reviewer") {
				// Ensure the unrelated branch has already failed when the
				// reviewer finishes, exercising consumption within the branch.
				select {
				case <-checksFailed:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				value = map[string]any{"a": map[string]any{"verdict": "regression", "reasoning": "test fails", "requiredChange": "fix regression"}}
			}
			if err := validateAuditValue(op, value); err != nil {
				return nil, err
			}
			return map[string]any{"task": label, "session": label, "context": "editor-context", "value": value}, nil
		case "snapshot":
			return map[string]any{"id": "candidate"}, nil
		case "context":
			return "check-context", nil
		case "exec":
			close(checksFailed)
			return nil, &Error{Code: "tool_denied", Message: "fixture check denied"}
		case "task":
			if op.Args["op"] == "review" {
				if op.Args["accept"] != true {
					t.Error("research was not accepted")
				}
				mu.Lock()
				accepted = append(accepted, op.Args["task"].(string))
				mu.Unlock()
			}
			return map[string]any{"id": op.Args["task"], "revision": 2}, nil
		default:
			return nil, fmt.Errorf("unexpected operation %s", op.Kind)
		}
	})
	r := Runner{Host: host}
	report, err := r.Run(context.Background(), string(source), map[string]any{"groups": []any{map[string]any{
		"label": "fixture", "source": "/repo", "evidence": "review.md", "findings": []any{map[string]any{"id": "a", "summary": "a bug"}}, "checks": []any{"check"},
	}}})
	if err == nil || report.Status != "failed" {
		t.Fatalf("denied check did not fail workflow: %+v, %v", report, err)
	}
	if len(accepted) != 1 || accepted[0] != "fixture reviewer" {
		t.Fatalf("research acceptance lost, or editing task accepted: %v", accepted)
	}
}
