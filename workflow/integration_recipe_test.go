package workflow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntegrationRecipeDecisionsAndBudgets(t *testing.T) {
	source, err := os.ReadFile("../examples/workflows/integrate-results.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                                                                                string
		conflict                                                                            bool
		rejectReviews                                                                       int
		drift, unchanged, repeatDrift, sandboxFailure, uncertain, dirty, rejectAfterRefresh bool
		repairs, reviews, refreshes                                                         int
		blocked                                                                             bool
	}{
		{name: "clean", reviews: 1},
		{name: "conflict and repair", conflict: true, rejectReviews: 1, repairs: 2, reviews: 2},
		{name: "repair limit", rejectReviews: 3, repairs: 2, reviews: 3, blocked: true},
		{name: "changed refresh", drift: true, refreshes: 1, reviews: 2},
		{name: "repair budget survives refresh", conflict: true, rejectReviews: 1, drift: true, rejectAfterRefresh: true, repairs: 2, reviews: 3, refreshes: 1, blocked: true},
		{name: "unchanged refresh", drift: true, unchanged: true, refreshes: 1, reviews: 1},
		{name: "second drift blocks", drift: true, repeatDrift: true, refreshes: 1, reviews: 2, blocked: true},
		{name: "sandbox blocks", sandboxFailure: true, reviews: 1, blocked: true},
		{name: "uncertain apply blocks", uncertain: true, reviews: 1, blocked: true},
		{name: "dirty check retained", dirty: true, reviews: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			repairs, reviews, refreshes, checks, applies, version, contexts := 0, 0, 0, 0, 0, 0, 0
			candidate := func() map[string]any {
				conflicts := []any{}
				if tc.conflict && version == 0 {
					conflicts = append(conflicts, map[string]any{"type": "CONFLICT (binary)", "base": "base", "ours": "ours", "theirs": "theirs"})
				}
				return map[string]any{"id": fmt.Sprint("candidate-", version), "merged": map[string]any{"id": fmt.Sprint("snapshot-", version)}, "conflicts": conflicts, "inputs": []any{}, "repairs": []any{}}
			}
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				switch op.Kind {
				case "integration":
					switch op.Args["op"] {
					case "prepare", "accept", "read":
						return candidate(), nil
					case "revise":
						version++
						return candidate(), nil
					case "refresh":
						refreshes++
						if !tc.unchanged {
							version++
						}
						c := candidate()
						c["changed"] = !tc.unchanged
						return c, nil
					case "apply":
						applies++
						if tc.uncertain {
							return nil, &Error{Code: "recovery_required", Message: "inspect write"}
						}
						if tc.drift && (applies == 1 || tc.repeatDrift) {
							return nil, &Error{Code: "parent_changed", Message: "parent changed"}
						}
						return map[string]any{"status": "applied"}, nil
					}
				case "agent":
					if _, ok := op.Args["maxIterations"]; ok {
						t.Error("recipe overrides normal allowance")
					}
					label, _ := op.Args["label"].(string)
					value := map[string]any{"summary": "fixed"}
					if strings.Contains(label, "reviewer") {
						reviews++
						value = map[string]any{"approved": reviews > tc.rejectReviews && !(tc.rejectAfterRefresh && refreshes > 0), "feedback": "reviewed"}
					} else {
						repairs++
					}
					contexts++
					return map[string]any{"task": fmt.Sprint("task-", contexts), "context": fmt.Sprint("context-", contexts), "value": value}, nil
				case "task":
					return map[string]any{"id": op.Args["task"], "revision": 2}, nil
				case "context":
					contexts++
					return fmt.Sprint("context-", contexts), nil
				case "exec":
					checks++
					if tc.sandboxFailure {
						return nil, &Error{Code: "sandbox_setup", Message: "sandbox refused"}
					}
					return map[string]any{"exitCode": 0, "text": "passed"}, nil
				case "release":
					if tc.dirty {
						return nil, &Error{Code: "unintegrated_changes", Message: "retained dirty context"}
					}
					return map[string]any{"released": op.Args["context"]}, nil
				}
				return nil, fmt.Errorf("unexpected operation: %+v", op)
			})
			r := Runner{Host: host}
			report, err := r.Run(context.Background(), string(source), map[string]any{"tasks": []any{map[string]any{"task": "worker", "revision": 2}}, "checks": []any{"check"}})
			if (err != nil) != tc.blocked {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if repairs != tc.repairs || reviews != tc.reviews || refreshes != tc.refreshes || checks != reviews {
				t.Fatalf("repairs/reviews/refreshes/checks=%d/%d/%d/%d", repairs, reviews, refreshes, checks)
			}
			if tc.dirty && len(report.Output.(map[string]any)["retained"].([]any)) == 0 {
				t.Fatal("dirty contexts not reported")
			}
		})
	}
}

func TestWorkflowRecordsApplyThatFinishesAfterCancellation(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := hostFunc(func(context.Context, Operation) (any, error) {
		close(started)
		<-finish
		return map[string]any{"status": "applied", "id": "receipt"}, nil
	})
	done := make(chan *Report, 1)
	go func() {
		r := Runner{Host: host}
		report, _ := r.Run(ctx, script(`return await polly.integration.apply("candidate")`), map[string]any{})
		done <- report
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("returned before apply receipt")
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	report := <-done
	if report.Status != "interrupted" || len(report.Steps) != 1 || report.Steps[0].Status != "completed" || report.Steps[0].Value.(map[string]any)["id"] != "receipt" {
		t.Fatalf("receipt lost: %+v", report)
	}
}
