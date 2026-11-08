package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func candidatePutAll(tx CoordinationTx, records map[RecordKey]string) error {
	for key, payload := range records {
		if err := tx.Put(key, json.RawMessage(payload)); err != nil {
			return err
		}
	}
	return nil
}

func candidateTestRecords() map[RecordKey]string {
	return map[RecordKey]string{
		{"integration", "candidate"}:      `{"status":"ready"}`,
		{"integration", "anomalous"}:      `{"status":"unrecognized"}`,
		{"integration", "missing-status"}: `{}`,
		{"integration", "applied"}:        `{"status":"applied"}`,
		{"integration", "superseded"}:     `{"status":"superseded"}`,
		{"integration", "successor"}:      `{"status":"ready","successor":"next"}`,
		{"workflow", "running"}:           `{"status":"running","acknowledged":true,"input":{"integer":9007199254740993}}`,
		{"workflow", "unacked"}:           `{"status":"completed"}`,
		{"workflow", "acked"}:             `{"status":"completed","acknowledged":true}`,
		{"workflow_step", "running/0"}:    `{"id":"payload-not-report","kind":"context","status":"running"}`,
		{"workflow_step", "running/1"}:    `{"kind":"agent","status":"running"}`,
		{"workflow_step", "running/2"}:    `{"kind":"followup","status":"running"}`,
		{"workflow_step", "running/3"}:    `{"kind":"context","status":"completed","value":"ctx"}`,
		{"workflow_step", "running/4"}:    `{"kind":"context","status":"completed","value":{"id":"ctx"}}`,
		{"workflow_step", "running/5"}:    `{"kind":"other","status":"completed","value":"ctx"}`,
		{"workflow_step", "runningx/0"}:   `{"kind":"context","status":"running"}`,
		{"mail", "request"}:               `{"to":"owner","kind":"request","delivered":true}`,
		{"mail", "answered"}:              `{"to":"owner","kind":"request","replyID":"reply"}`,
		{"mail", "reply"}:                 `{"to":"owner","kind":"reply"}`,
		{"mail", "notice"}:                `{"to":"owner","kind":"info","workflow":"unacked"}`,
		{"mail", "delivered"}:             `{"to":"owner","kind":"reply","workflow":"unacked","delivered":true}`,
		{"mail", "elsewhere"}:             `{"to":"other","kind":"request"}`,
		{"mail", "task-notice"}:           `{"task":"open","execution":"execution","revision":9007199254740993,"delivered":true,"kind":"info"}`,
		{"mail", "old-task-notice"}:       `{"task":"open","execution":"execution","revision":9007199254740992}`,
		{"task", "open"}:                  `{"owner":"owner","status":"running"}`,
		{"task", "other-open"}:            `{"owner":"other","status":"running"}`,
		{"task", "done"}:                  `{"owner":"owner","status":"done","snapshot":"snap"}`,
		{"task", "starting-only"}:         `{"owner":"owner","status":"done","startingSnapshot":"snap","snapshot":"other-snap"}`,
		{"task", "canceled"}:              `{"owner":"owner","status":"canceled"}`,
		{"task", "delivery"}:              `{"status":"done","delivery":{"via":"workflow_step","ref":"running/5"}}`,
		{"task", "delivery-other"}:        `{"status":"done","delivery":{"via":"workflow_step","ref":"runningx/5"}}`,
		{"task", "delivery-wrong-via"}:    `{"status":"done","delivery":{"via":"mail","ref":"running/5"}}`,
		{"snapshot", "snap"}:              `{"source":"source","tree":"tree"}`,
		{"snapshot", "other-snap"}:        `{"source":"source","tree":"other-tree"}`,
		{"apply", "uncertain"}:            `{"status":"applying","tasks":[{"task":"missing"},{"task":"open"}]}`,
		{"apply", "recover"}:              `{"status":"recovery_required","tasks":[{"task":"done"}]}`,
		{"apply", "applied"}:              `{"status":"applied","tasks":[{"task":"open"}]}`,
		{"apply", "other"}:                `{"status":"applying","tasks":[{"task":"other-open"}]}`,
		{"followup", "preparing"}:         `{"refresh":true,"phase":"preparing","heldScratch":"scratch"}`,
		{"followup", "launched-held"}:     `{"refresh":true,"phase":"launched","heldScratch":"held"}`,
		{"followup", "no-refresh"}:        `{"phase":"preparing"}`,
	}
}

func candidateCases() []struct {
	selector CoordinationCandidateSelector
	key      string
	want     []string
} {
	return []struct {
		selector CoordinationCandidateSelector
		key      string
		want     []string
	}{
		{CandidateIntegrationForget, "", []string{"anomalous", "candidate", "missing-status"}},
		{CandidateWorkflowUnsettled, "", []string{"running", "unacked"}},
		{CandidateWorkflowRunning, "", []string{"running"}},
		{CandidateWorkflowHostRunningStep, "running", []string{"running/0"}},
		{CandidateWorkflowContextStep, CoordinationIndexKey("running", "ctx"), []string{"running/3"}},
		{CandidateMailSettlement, "owner", []string{"notice", "reply", "request"}},
		{CandidateTaskNotice, CoordinationIndexKey("open", "execution", "9007199254740993"), []string{"task-notice"}},
		{CandidateTaskOpenOwner, "owner", []string{"open"}},
		{CandidateApplyUnresolvedOwner, "owner", []string{"recover", "uncertain"}},
		{CandidateDoneTaskTree, CoordinationIndexKey("source", "tree"), []string{"done"}},
		{CandidateFollowupPreparing, "", []string{"preparing"}},
		{CandidateFollowupHeldScratch, "", []string{"launched-held", "preparing"}},
		{CandidateTaskWorkflowDelivery, "running", []string{"delivery"}},
	}
}

func TestCoordinationCandidatesParity(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			access := lifecycleAccess(t, native)
			records := candidateTestRecords()
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return candidatePutAll(tx, records) })
			nativeMust(t, err)
			for _, test := range candidateCases() {
				t.Run(string(test.selector), func(t *testing.T) {
					q := CoordinationCandidateQuery{Selector: test.selector, Key: test.key, Limit: 1}
					var got []string
					for {
						var page RecordPage
						nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
							if _, mutable := r.(CoordinationTx); mutable {
								t.Fatal("read callback acquired mutation authority")
							}
							if _, ok := r.(CoordinationCandidateReader); native && !ok {
								t.Fatal("native read-only wrapper dropped candidate capability")
							}
							var err error
							page, err = QueryCoordinationCandidates(r, q)
							return err
						}))
						if page.Total != -1 {
							t.Fatalf("counted candidate history: %+v", page)
						}
						for _, record := range page.Records {
							if !bytes.Equal(record.Payload, []byte(records[record.Key])) {
								t.Fatalf("source bytes changed: %s", record.Payload)
							}
							got = append(got, record.Key.ID)
						}
						if page.Next == "" {
							break
						}
						if page.Next == q.After {
							t.Fatal("nonadvancing page")
						}
						q.After = page.Next
					}
					if !reflect.DeepEqual(got, test.want) {
						t.Fatalf("got %v want %v", got, test.want)
					}
				})
			}
		})
	}
}

func TestCoordinationCandidatesStagedMovesAndRollback(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			access := lifecycleAccess(t, native)
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return candidatePutAll(tx, candidateTestRecords()) })
			nativeMust(t, err)
			check := func(r CoordinationReader, selector CoordinationCandidateSelector, key string, want []string) {
				t.Helper()
				page, err := QueryCoordinationCandidates(r, CoordinationCandidateQuery{Selector: selector, Key: key, Limit: 100})
				nativeMust(t, err)
				if got := lifecycleKeys(page); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s got %v want %v", selector, got, want)
				}
			}
			veto := errors.New("rollback")
			var retained CoordinationReader
			_, err = access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				retained = tx
				nativeMust(t, tx.Put(RecordKey{"task", "open"}, json.RawMessage(`{"owner":"other","status":"done"}`)))
				check(tx, CandidateTaskOpenOwner, "owner", nil)
				check(tx, CandidateApplyUnresolvedOwner, "owner", []string{"recover"})
				nativeMust(t, tx.Put(RecordKey{"snapshot", "snap"}, json.RawMessage(`{"source":"source","tree":"new-tree"}`)))
				check(tx, CandidateDoneTaskTree, CoordinationIndexKey("source", "tree"), nil)
				check(tx, CandidateDoneTaskTree, CoordinationIndexKey("source", "new-tree"), []string{"done"})
				nativeMust(t, tx.Delete(RecordKey{"followup", "preparing"}))
				check(tx, CandidateFollowupPreparing, "", nil)
				return veto
			})
			if !errors.Is(err, veto) {
				t.Fatal(err)
			}
			if _, err := QueryCoordinationCandidates(retained, CoordinationCandidateQuery{Selector: CandidateWorkflowRunning, Limit: 1}); !errors.Is(err, ErrCoordinationClosed) {
				t.Fatalf("retained reader: %v", err)
			}
			nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
				check(r, CandidateTaskOpenOwner, "owner", []string{"open"})
				check(r, CandidateApplyUnresolvedOwner, "owner", []string{"recover", "uncertain"})
				check(r, CandidateDoneTaskTree, CoordinationIndexKey("source", "tree"), []string{"done"})
				check(r, CandidateFollowupPreparing, "", []string{"preparing"})
				return nil
			}))
		})
	}
}

func TestCoordinationCandidatesOpaqueCursorAndOversize(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			access := lifecycleAccess(t, native)
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for _, id := range []string{"z", "\xff", "a\x00x"} {
					data, _ := json.Marshal(map[string]string{"heldScratch": "held", "text": strings.Repeat("x", 1024)})
					if err := tx.Put(RecordKey{"followup", id}, data); err != nil {
						return err
					}
				}
				return nil
			})
			nativeMust(t, err)
			q := CoordinationCandidateQuery{Selector: CandidateFollowupHeldScratch, Limit: 3, MaxBytes: 10}
			var got []string
			for {
				var page RecordPage
				nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
					var err error
					page, err = QueryCoordinationCandidates(r, q)
					return err
				}))
				if len(page.Records) != 1 || !page.Oversized || len(page.Records[0].Payload) < 1024 {
					t.Fatalf("oversize truncated: %+v", page)
				}
				got = append(got, page.Records[0].Key.ID)
				if page.Next == "" {
					break
				}
				q.After = page.Next
			}
			if !reflect.DeepEqual(got, []string{"a\x00x", "z", "\xff"}) {
				t.Fatal(got)
			}
			nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
				other := q
				other.Selector = CandidateFollowupPreparing
				if _, err := QueryCoordinationCandidates(r, other); !errors.Is(err, ErrCoordinationCursor) {
					t.Fatalf("foreign selector cursor: %v", err)
				}
				other = q
				other.After = "not-a-cursor"
				if _, err := QueryCoordinationCandidates(r, other); !errors.Is(err, ErrCoordinationCursor) {
					t.Fatalf("invalid cursor: %v", err)
				}
				return nil
			}))
		})
	}
}

func TestCoordinationCandidatesAllPartitionsSelectedWork(t *testing.T) {
	baseline := map[CoordinationCandidateSelector][3]uint64{}
	for _, count := range []int{4, 128} {
		access := lifecycleAccess(t, true)
		_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
			if err := candidatePutAll(tx, candidateTestRecords()); err != nil {
				return err
			}
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("history-%06d", i)
				for kind, fields := range map[string]map[string]any{
					"workflow":      {"status": "completed", "acknowledged": true},
					"workflow_step": {"kind": "context", "status": "completed", "value": "elsewhere"},
					"mail":          {"to": "owner", "delivered": true, "task": id, "execution": "old", "revision": 1},
					"task":          {"owner": "owner", "status": "done", "snapshot": id, "delivery": map[string]string{"via": "workflow_step", "ref": "other/0"}},
					"snapshot":      {"source": "source", "tree": "other-tree"},
					"apply":         {"status": "applied", "tasks": []map[string]string{{"task": "open"}}},
					"followup":      {"refresh": false, "phase": "launched"},
					"integration":   {"status": "applied"},
				} {
					fields["text"] = strings.Repeat("x", 4096)
					payload, _ := json.Marshal(fields)
					key := RecordKey{kind, id}
					if kind == "workflow_step" {
						key.ID = id + "/0"
					}
					if err := tx.Put(key, payload); err != nil {
						return err
					}
				}
			}
			return nil
		})
		nativeMust(t, err)
		for _, test := range candidateCases() {
			recorder := runtimemetrics.New()
			nativeMust(t, access.ReadRecords(runtimemetrics.WithRecorder(t.Context(), recorder), func(r CoordinationReader) error {
				page, err := QueryCoordinationCandidates(r, CoordinationCandidateQuery{Selector: test.selector, Key: test.key, Limit: 1, MaxBytes: 1024})
				if err == nil && (len(page.Records) != 1 || page.Records[0].Key.ID != test.want[0]) {
					t.Fatalf("%s wrong selection: %+v", test.selector, page)
				}
				return err
			}))
			snapshot := recorder.Snapshot()
			observed := [3]uint64{snapshot.Count(runtimemetrics.RowsRead), snapshot.Count(runtimemetrics.PayloadBytesRead), snapshot.Count(runtimemetrics.SQLStatements)}
			if count == 4 {
				baseline[test.selector] = observed
			} else if observed != baseline[test.selector] {
				t.Fatalf("%s history grew returned work: %v vs %v", test.selector, baseline[test.selector], observed)
			}
			t.Logf("%s with %d historical rows per kind: rows/bytes/sql=%v", test.selector, count, observed)
		}
	}
}

func TestCoordinationCandidatesNativeSelectedWork(t *testing.T) {
	var baseline [3]uint64
	for _, count := range []int{8, 512} {
		access := lifecycleAccess(t, true)
		_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("%06d", i)
				data, _ := json.Marshal(map[string]any{"status": "completed", "acknowledged": true, "output": strings.Repeat("x", 4096)})
				if err := tx.Put(RecordKey{"workflow", id}, data); err != nil {
					return err
				}
			}
			for _, id := range []string{"selected-a", "selected-b", "selected-c"} {
				data, _ := json.Marshal(map[string]any{"status": "running", "output": strings.Repeat("x", 4096)})
				if err := tx.Put(RecordKey{"workflow", id}, data); err != nil {
					return err
				}
			}
			return nil
		})
		nativeMust(t, err)
		recorder := runtimemetrics.New()
		nativeMust(t, access.ReadRecords(runtimemetrics.WithRecorder(t.Context(), recorder), func(r CoordinationReader) error {
			page, err := QueryCoordinationCandidates(r, CoordinationCandidateQuery{Selector: CandidateWorkflowUnsettled, Limit: 2, MaxBytes: 5000})
			if err == nil && (len(page.Records) != 1 || page.Next == "" || page.Total != -1) {
				t.Fatalf("page %+v", page)
			}
			return err
		}))
		snapshot := recorder.Snapshot()
		observed := [3]uint64{snapshot.Count(runtimemetrics.RowsRead), snapshot.Count(runtimemetrics.PayloadBytesRead), snapshot.Count(runtimemetrics.SQLStatements)}
		if count == 8 {
			baseline = observed
		} else if observed != baseline {
			t.Fatalf("history grew selected work: %v vs %v", baseline, observed)
		}
		t.Logf("%d historical workflows: rows/bytes/sql=%v", count, observed)
	}
}
