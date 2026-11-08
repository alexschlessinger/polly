package swarm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func savedWorkflowRecord(t *testing.T, session *workflowRecordSession) json.RawMessage {
	t.Helper()
	var payload json.RawMessage
	if err := session.access.ReadRecords(t.Context(), func(reader sessions.CoordinationReader) error {
		record, found, err := reader.Get(sessions.RecordKey{Kind: "workflow", ID: "w"})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("missing workflow header")
		}
		payload = record.Payload
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return payload
}

func saveWorkflowHeader(t *testing.T, r *Runtime, header workflow.ReportHeader, full bool) error {
	t.Helper()
	if !full {
		return r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: header})
	}
	return r.SaveWorkflow(t.Context(), workflow.Report{
		ID: header.ID, Run: header.Run, CallID: header.CallID, Acknowledged: header.Acknowledged,
		Name: header.Name, Status: header.Status, Source: header.Source,
		Input: header.Input, Output: header.Output, Error: header.Error,
		Started: header.Started, Finished: header.Finished,
	})
}

func TestWorkflowRecordsRejectPersistedGaps(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			for _, corrupt := range []string{"first", "middle", "high"} {
				t.Run(fmt.Sprintf("legacy=%t/full=%t/%s", legacy, full, corrupt), func(t *testing.T) {
					r, session := workflowRecordsRuntime(t, legacy)
					header := workflowRecordsHeader()
					steps := map[int]workflow.Step{0: workflowRecordsStep(0), 1: workflowRecordsStep(1), 2: workflowRecordsStep(2)}
					if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: header, TotalSteps: len(steps), Steps: steps}); err != nil {
						t.Fatal(err)
					}
					count := 2
					if _, err := session.access.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
						switch corrupt {
						case "first":
							return tx.Delete(sessions.RecordKey{Kind: "workflow_step", ID: "w/0"})
						case "middle":
							return tx.Delete(sessions.RecordKey{Kind: "workflow_step", ID: "w/1"})
						default:
							count = 4
							data, err := json.Marshal(workflowRecordsStep(100))
							if err != nil {
								return err
							}
							return tx.Put(sessions.RecordKey{Kind: "workflow_step", ID: "w/100"}, data)
						}
					}); err != nil {
						t.Fatal(err)
					}
					before, err := session.CoordinationSession.ReadCoordination(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := decodeState(before); err == nil {
						t.Fatal("gap fixture unexpectedly valid")
					}
					header.Status = "completed"
					session.resetCounts()
					if full {
						// A full empty report must reject corruption, not delete only
						// count keys and silently orphan the sparse high-index tail.
						err = saveWorkflowHeader(t, r, header, true)
					} else {
						err = r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: header, TotalSteps: count})
					}
					if !errors.Is(err, sessions.ErrCoordinationFormat) {
						t.Fatalf("gapped prior history accepted: %v", err)
					}
					if counts := session.snapshot(); counts.puts != 0 || counts.deletes != 0 || counts.gets != 2 || counts.rows != 2 || counts.queries != 1 {
						t.Fatalf("gap check wrote records or scanned old payloads: %+v", counts)
					}
					after, err := session.CoordinationSession.ReadCoordination(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before.Records, after.Records) {
						t.Fatal("rejected history changed header/steps or posted terminal mail")
					}
				})
			}
		}
	}
}

func TestWorkflowRecordsDeliveryPreservesOpaqueTaskNumbers(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, legacy)
			header := workflowRecordsHeader()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: header}); err != nil {
				t.Fatal(err)
			}
			_, step := seedWorkflowReceipt(t, session, "completed")
			key := sessions.RecordKey{Kind: "task", ID: "task"}
			result := json.RawMessage(`{"nested":[9007199254740993,18446744073709551615,1.234567890123456789e-100]}`)
			if _, err := session.access.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
				record, _, err := tx.Get(key)
				if err != nil {
					return err
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(record.Payload, &fields); err != nil {
					return err
				}
				fields["result"] = result
				data, err := json.Marshal(fields)
				if err != nil {
					return err
				}
				return tx.Put(key, data)
			}); err != nil {
				t.Fatal(err)
			}
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: header, TotalSteps: 1, Steps: map[int]workflow.Step{0: step}}); err != nil {
				t.Fatal(err)
			}
			if err := session.access.ReadRecords(t.Context(), func(reader sessions.CoordinationReader) error {
				record, _, err := reader.Get(key)
				if err != nil {
					return err
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(record.Payload, &fields); err != nil {
					return err
				}
				if !bytes.Equal(fields["result"], result) || string(fields["status"]) != `"done"` {
					t.Fatalf("receipt changed opaque result or lost status: %s", record.Payload)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkflowRecordsExactHeaderNumbers(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			for _, field := range []string{"input", "output", "error.result", "error.usage"} {
				t.Run(fmt.Sprintf("legacy=%t/full=%t/%s", legacy, full, field), func(t *testing.T) {
					r, session := workflowRecordsRuntime(t, legacy)
					header := workflowRecordsHeader()
					for _, number := range []string{"9007199254740993", "9007199254740992", "9007199254740993"} {
						value := map[string]any{
							"nested": []any{map[string]any{"n": json.Number(number)}, "opaque task/member"},
							"other":  json.Number("18446744073709551615"),
							"tiny":   json.Number("1.234567890123456789e-100"),
						}
						switch field {
						case "input":
							header.Input = value
						case "output":
							header.Output = value
						case "error.result":
							header.Error = &workflow.Error{Code: "test", Message: "kept", Result: value}
						case "error.usage":
							header.Error = &workflow.Error{Code: "test", Message: "kept", Usage: value}
						}
						ownedBefore, err := json.Marshal(header)
						if err != nil {
							t.Fatal(err)
						}
						session.resetCounts()
						if err := saveWorkflowHeader(t, r, header, full); err != nil {
							t.Fatal(err)
						}
						if counts := session.snapshot(); !reflect.DeepEqual(counts.written, []sessions.RecordKey{{Kind: "workflow", ID: "w"}}) {
							t.Fatalf("exact numeric replacement must write one header: %+v", counts)
						}
						if raw := savedWorkflowRecord(t, session); !bytes.Equal(raw, ownedBefore) {
							t.Fatalf("successful save lost exact nested numbers:\n got %s\nwant %s", raw, ownedBefore)
						}
						session.resetCounts()
						if err := saveWorkflowHeader(t, r, header, full); err != nil {
							t.Fatal(err)
						}
						if counts := session.snapshot(); counts.puts != 0 || counts.deletes != 0 {
							t.Fatalf("identical exact-number header rewritten: %+v", counts)
						}
						ownedAfter, err := json.Marshal(header)
						if err != nil || !bytes.Equal(ownedBefore, ownedAfter) {
							t.Fatalf("save mutated caller-owned values: %s (%v)", ownedAfter, err)
						}
					}
				})
			}
		}
	}
}
