package workflow

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func reviewFinding(id, severity string) map[string]any {
	return map[string]any{
		"id": id, "title": id + " problem", "severity": severity,
		"location": "tools/registry.go:1", "evidence": "quoted code",
		"impact": "concrete failure scenario", "suggestion": "specific fix",
	}
}

// TestCodeReviewRecipe drives examples/workflows/code-review.js through a fake
// host: three reviewers, one judge, one final reviewer. It covers the
// consolidation path the zero-finding live run cannot reach, plus the
// fail-closed check that every reviewer finding is accounted for.
func TestCodeReviewRecipe(t *testing.T) {
	source, err := os.ReadFile("../examples/workflows/code-review.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"consolidate", "overrule", "empty", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			var judgeValue, judgeReviews any
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				if op.Kind != "agent" {
					t.Fatalf("unexpected operation %s", op.Kind)
				}
				label := op.Args["label"].(string)
				input := op.Args["input"].(map[string]any)
				var value any
				switch {
				case strings.HasSuffix(label, " reviewer") && !strings.HasPrefix(label, "final "):
					findings := []any{}
					if mode != "empty" {
						if strings.Contains(label, " correctness ") {
							findings = append(findings, reviewFinding("race", "major"))
						}
						if strings.Contains(label, " security ") {
							findings = append(findings, reviewFinding("injection", "major"))
						}
					}
					value = map[string]any{"summary": label + " summary", "findings": findings,
						"checked": []any{label + " evidence"}, "unknowns": []any{label + " uncertainty"}}
				case label == "review judge":
					judgeReviews = input["reviews"]
					var ids []string
					for _, review := range input["reviews"].([]any) {
						for _, finding := range review.(map[string]any)["findings"].([]any) {
							ids = append(ids, finding.(map[string]any)["id"].(string))
						}
					}
					issues, dispositions := []any{}, map[string]any{}
					for _, id := range ids {
						canonical := "issue-" + strconv.Itoa(len(issues)+1)
						issues = append(issues, map[string]any{
							"id": canonical, "title": canonical, "severity": "major",
							"location": "tools/registry.go:1", "evidence": "quoted code",
							"impact": "impact", "suggestion": "fix", "sources": []any{id},
						})
						dispositions[id] = map[string]any{"disposition": "confirmed", "issue": canonical, "reason": "reproduced"}
					}
					additional := []any{}
					if mode != "empty" {
						additional = append(additional, reviewFinding("missing-timeout", "minor"))
					}
					if mode == "incomplete" && len(ids) > 0 {
						delete(dispositions, ids[len(ids)-1])
					}
					value = map[string]any{"summary": "integrated judgement", "issues": issues,
						"dispositions": dispositions, "additionalFindings": additional,
						"disagreements": []any{map[string]any{"topic": "verification", "detail": "reviewers disagree about reproduction"}},
						"unknowns":      []any{"full suite unavailable"}}
					judgeValue = value
				case label == "final reviewer":
					// Preserve the entire judgment and the original review evidence,
					// including when there are no canonical issues to audit.
					if !reflect.DeepEqual(input["judge"], judgeValue) {
						return nil, fmt.Errorf("final reviewer lost judge context: got %#v, want %#v", input["judge"], judgeValue)
					}
					if !reflect.DeepEqual(input["reviews"], judgeReviews) {
						return nil, fmt.Errorf("final reviewer lost original review evidence: got %#v, want %#v", input["reviews"], judgeReviews)
					}
					for _, raw := range input["reviews"].([]any) {
						review := raw.(map[string]any)
						label := "pr-1 " + review["lens"].(string) + " reviewer"
						if !reflect.DeepEqual(review["checked"], []any{label + " evidence"}) ||
							!reflect.DeepEqual(review["unknowns"], []any{label + " uncertainty"}) {
							return nil, fmt.Errorf("original review evidence was dropped before judging: %#v", review)
						}
					}
					judgements := map[string]any{}
					for _, raw := range input["issues"].([]any) {
						id := raw.(map[string]any)["id"].(string)
						entry := map[string]any{"verdict": "upheld", "reason": "re-derived from the code", "severity": "minor"}
						if id == "issue-2" {
							if mode == "overrule" {
								entry = map[string]any{"verdict": "unsupported", "reason": "code does not support it", "severity": "minor"}
							} else {
								entry = map[string]any{"verdict": "revised", "reason": "understated impact", "severity": "critical"}
							}
						}
						judgements[id] = entry
					}
					missed := []any{}
					if mode == "consolidate" {
						missed = append(missed, reviewFinding("docs-drift", "minor"))
					}
					value = map[string]any{"assessment": "partly_sound", "summary": "audited the judgement",
						"judgements": judgements, "missed": missed, "notes": []any{}}
				default:
					t.Fatalf("unexpected label %s", label)
				}
				if err := validateAuditValue(op, value); err != nil {
					return nil, err
				}
				return map[string]any{"value": value, "session": "agent", "task": "task"}, nil
			})
			r := Runner{Host: host}
			report, err := r.Run(context.Background(), string(source),
				map[string]any{"intent": "keep behavior", "diffs": []any{map[string]any{"id": "pr-1", "diff": "@@ change @@"}}})
			if mode == "incomplete" {
				if err == nil || report.Status != "failed" {
					t.Fatalf("unaccounted finding succeeded: %+v %v", report, err)
				}
				if !strings.Contains(err.Error(), "dispositions") {
					t.Fatalf("failed for the wrong reason: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			output := report.Output.(map[string]any)
			final := output["finalIssues"].([]any)
			switch mode {
			case "consolidate":
				// The judge's canonical issue ids, the severity the final reviewer
				// corrected, and its own unadjudicated finding, worst first.
				want := []struct{ id, severity, audit string }{
					{"issue-2", "critical", "revised"},
					{"issue-1", "major", "upheld"},
					{"judge/missing-timeout", "minor", "upheld"},
					{"audit/docs-drift", "minor", "unadjudicated"},
				}
				if len(final) != len(want) {
					t.Fatalf("final issues: %#v", final)
				}
				for i, expected := range want {
					issue := final[i].(map[string]any)
					if issue["id"] != expected.id || issue["severity"] != expected.severity || issue["audit"] != expected.audit {
						t.Fatalf("final issue %d: %#v", i, issue)
					}
				}
				if sources := final[2].(map[string]any)["sources"].([]any); sources[0] != "judge" {
					t.Fatalf("judge finding lost its source: %#v", sources)
				}
				if sources := final[3].(map[string]any)["sources"].([]any); sources[0] != "audit" {
					t.Fatalf("audit finding lost its source: %#v", sources)
				}
				if output["clean"] != false {
					t.Fatal("issues reported as clean")
				}
			case "overrule":
				overruled := output["audit"].(map[string]any)["overruled"].([]any)
				if len(overruled) != 1 || overruled[0].(map[string]any)["id"] != "issue-2" {
					t.Fatalf("overruled: %#v", overruled)
				}
				if len(final) != 2 {
					t.Fatalf("unsupported issue survived: %#v", final)
				}
			case "empty":
				if len(final) != 0 || output["clean"] != true {
					t.Fatalf("empty reviews were not clean: %#v", output)
				}
				judgement := output["judgement"].(map[string]any)
				if judgement["issues"] != float64(0) {
					t.Fatalf("empty judgement: %#v", judgement)
				}
			}
		})
	}
}
