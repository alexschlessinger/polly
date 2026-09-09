package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
)

func validateAuditValue(op Operation, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return (&schema.Schema{Raw: op.Args["schema"].(map[string]any)}).Validate(string(encoded))
}

func TestDocDriftRejectsIncompleteEvidence(t *testing.T) {
	source, err := os.ReadFile("../examples/workflows/doc-drift-audit.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"empty claims", "blank claim evidence", "blank evidence", "blank change", "stale blank change", "unverifiable"} {
		t.Run(kind, func(t *testing.T) {
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				if op.Kind != "agent" {
					t.Fatalf("unexpected operation %s", op.Kind)
				}
				label := op.Args["label"].(string)
				var value any
				if strings.Contains(label, "enumerator") {
					evidence := "README.md:10"
					if kind == "blank claim evidence" {
						evidence = " \t"
					}
					claims := []any{map[string]any{"id": "a", "claim": "a claim", "evidence": evidence}}
					if kind == "empty claims" {
						claims = []any{}
					}
					value = map[string]any{"claims": claims}
				} else if strings.Contains(label, "verifier") {
					verdict, evidence, change := "holds", "tools/registry.go:1", ""
					switch kind {
					case "blank evidence":
						evidence = " \t"
					case "blank change":
						verdict = "drifted"
						change = " "
					case "stale blank change":
						verdict = "stale"
					case "unverifiable":
						verdict = "unverifiable"
						evidence = "Read the relevant files but could not establish behavior"
					}
					value = map[string]any{"a": map[string]any{"verdict": verdict, "evidence": evidence, "suggestedChange": change}}
				} else {
					t.Fatalf("incomplete audit started an editor: %s", label)
				}
				if err := validateAuditValue(op, value); err != nil {
					return nil, err
				}
				return map[string]any{"value": value, "session": "agent", "task": "task"}, nil
			})
			r := Runner{Host: host}
			report, err := r.Run(context.Background(), string(source), map[string]any{"source": "/repo", "docs": []any{map[string]any{"path": "README.md"}}})
			if err == nil || report.Status != "failed" {
				t.Fatalf("incomplete audit succeeded: %+v %v", report, err)
			}
		})
	}
}

func TestDocDriftAuditWorkflowDecisions(t *testing.T) {
	source, err := os.ReadFile("../examples/workflows/doc-drift-audit.js")
	if err != nil {
		t.Fatal(err)
	}
	claims := []any{
		map[string]any{"id": "flag-a", "claim": "the -model flag accepts provider/model", "evidence": "README.md:10"},
		map[string]any{"id": "flag-b", "claim": "sandboxing is on by default", "evidence": "README.md:20"},
	}
	holds := func() map[string]any {
		value := map[string]any{}
		for _, c := range claims {
			id := c.(map[string]any)["id"].(string)
			value[id] = map[string]any{"verdict": "holds", "evidence": "tools/registry.go", "suggestedChange": ""}
		}
		return value
	}
	drifted := func() map[string]any {
		value := holds()
		value["flag-a"] = map[string]any{"verdict": "drifted", "evidence": "cmd/polly/config.go", "suggestedChange": "describe the real default"}
		return value
	}
	for _, tc := range []struct {
		name                                                           string
		drift, driftAfterRepair, repairDisabled, dupPaths, dupClaimIDs bool
		enumerators, verifiers, editors                                int
		blocked                                                        bool
		status                                                         string
	}{
		{name: "clean", enumerators: 1, verifiers: 1, status: "clean"},
		{name: "drift repaired", drift: true, enumerators: 1, verifiers: 2, editors: 1, status: "verified"},
		{name: "unresolved after repair", drift: true, driftAfterRepair: true, enumerators: 1, verifiers: 2, editors: 1, blocked: true},
		{name: "report only", drift: true, repairDisabled: true, enumerators: 1, verifiers: 1, status: "drifted"},
		{name: "duplicate doc paths", dupPaths: true, blocked: true},
		{name: "duplicate claim ids", dupClaimIDs: true, enumerators: 1, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var enumerators, verifiers, editors, verifyPass int
			docs := []any{map[string]any{"path": "README.md"}}
			if tc.dupPaths {
				docs = append(docs, docs[0])
			}
			input := map[string]any{"source": "/repo", "docs": docs}
			if tc.repairDisabled {
				input["repair"] = false
			}
			host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				switch op.Kind {
				case "log":
					return nil, nil
				case "snapshot":
					if op.Args["context"] != "context-README.md doc editor" {
						t.Errorf("wrong edited context: %v", op.Args)
					}
					return map[string]any{"id": "edited-candidate"}, nil
				case "agent":
					label, _ := op.Args["label"].(string)
					base := map[string]any{"task": "task-" + label, "context": "context-" + label}
					switch {
					case strings.Contains(label, "enumerator"):
						enumerators++
						value := map[string]any{"claims": claims}
						if tc.dupClaimIDs {
							value = map[string]any{"claims": []any{claims[0], claims[0]}}
						}
						base["value"] = value
					case strings.Contains(label, "verifier"):
						verifiers++
						verifyPass++
						if verifyPass > 1 && op.Args["snapshot"] != "edited-candidate" {
							t.Errorf("verifier did not receive edited candidate: %v", op.Args)
						}
						if tc.drift && (verifyPass == 1 || tc.driftAfterRepair) {
							base["value"] = drifted()
						} else {
							base["value"] = holds()
						}
					case strings.Contains(label, "editor"):
						editors++
						base["value"] = map[string]any{"flag-a": map[string]any{"status": "fixed", "what": "corrected the flag default"}}
					default:
						return nil, fmt.Errorf("unexpected agent label: %s", label)
					}
					if err := validateAuditValue(op, base["value"]); err != nil {
						return nil, err
					}
					return base, nil
				}
				return nil, fmt.Errorf("unexpected operation: %+v", op)
			})
			runner := Runner{Host: host}
			report, err := runner.Run(context.Background(), string(source), input)
			if (err != nil) != tc.blocked {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if enumerators != tc.enumerators || verifiers != tc.verifiers || editors != tc.editors {
				t.Fatalf("enumerators/verifiers/editors=%d/%d/%d, want %d/%d/%d",
					enumerators, verifiers, editors, tc.enumerators, tc.verifiers, tc.editors)
			}
			if tc.blocked {
				var failure *Error
				if !errors.As(err, &failure) {
					t.Fatalf("missing structured failure: %v", err)
				}
				return
			}
			rows, ok := report.Output.([]any)
			if !ok || len(rows) != len(docs) {
				t.Fatalf("output rows: %#v", report.Output)
			}
			for _, row := range rows {
				summary, ok := row.(map[string]any)["value"].(map[string]any)
				if !ok || summary["status"] != tc.status {
					t.Fatalf("summary: %#v", row)
				}
			}
		})
	}
}
