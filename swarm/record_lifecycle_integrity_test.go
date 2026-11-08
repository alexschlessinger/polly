package swarm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestRecordLifecycleControlPreservesOpaqueNumbers(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		for _, operation := range []string{"acknowledge", "defer", "stop"} {
			t.Run(fmt.Sprintf("legacy=%t/%s", legacy, operation), func(t *testing.T) {
				r, root := lifecycleRecordFixture(t, 1, legacy)
				value := map[string]any{"integer": json.Number("9007199254740993"), "unsigned": json.Number("18446744073709551615"), "exponent": json.Number("1.234567890123456789e+40")}
				if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
					if err := tx.PutWorkflowHeader(&workflow.ReportHeader{ID: "w", Run: "run", Status: "failed", Input: value, Output: value}); err != nil {
						return err
					}
					e, err := tx.Execution("selected")
					if err != nil {
						return err
					}
					e.Status, e.Workflow, e.Request.Input = "failed", "w", value
					e.Completion = &StructuredCompletion{Task: "selected", Value: value}
					e.RetryPause = &RetryPause{}
					if err := tx.PutExecution(e); err != nil {
						return err
					}
					task, err := tx.Task("selected")
					if err != nil {
						return err
					}
					task.Result = value
					return tx.PutTask(task)
				}); err != nil {
					t.Fatal(err)
				}
				var err error
				switch operation {
				case "acknowledge":
					err = r.AcknowledgeWorkflow(t.Context(), "w")
				case "defer":
					err = r.DeferWorkflow(t.Context(), "w", "retain")
				case "stop":
					err = r.StopMember(t.Context(), "selected")
				}
				if err != nil {
					t.Fatal(err)
				}
				access, err := sessions.OpenCoordinationAccess(root)
				if err != nil {
					t.Fatal(err)
				}
				err = access.ReadRecords(t.Context(), func(rd sessions.CoordinationReader) error {
					for _, key := range []sessions.RecordKey{{Kind: "workflow", ID: "w"}, {Kind: "task", ID: "selected"}, {Kind: "execution", ID: "selected"}} {
						record, found, err := rd.Get(key)
						if err != nil {
							return err
						}
						for _, token := range value {
							if !found || !strings.Contains(string(record.Payload), token.(json.Number).String()) {
								t.Fatalf("%s rounded stored %s: %s", operation, key.Kind, record.Payload)
							}
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
