package sessions

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestInspectorFactsMaintainedCounts(t *testing.T) {
	var baseline [3]uint64
	for _, count := range []int{16, 1024} {
		access := lifecycleAccess(t, true)
		_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
			put := func(kind, id string, v any) error {
				b, e := json.Marshal(v)
				if e != nil {
					return e
				}
				return tx.Put(RecordKey{kind, id}, b)
			}
			if err := put("run", "r", map[string]any{"id": "r", "status": "running", "limit": 2048}); err != nil {
				return err
			}
			if err := put("workflow", "w", map[string]any{"id": "w", "run": "r", "status": "failed", "acknowledged": true}); err != nil {
				return err
			}
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("%06d", i)
				if err := put("execution", "active-"+id, map[string]any{"id": "active-" + id, "run": "r", "status": "waiting"}); err != nil {
					return err
				}
				if err := put("execution", id, map[string]any{"id": id, "run": "r", "member": id, "workflow": "w", "status": "completed", "generation": 1}); err != nil {
					return err
				}
				if err := put("task", id, map[string]any{"id": id, "run": "r", "owner": id, "execution": id, "status": "blocked", "revision": 2, "deferral": map[string]any{"workflow": "w", "execution": id, "owner": id, "generation": 1, "revision": 2, "status": "blocked"}}); err != nil {
					return err
				}
			}
			return nil
		})
		nativeMust(t, err)
		rec := runtimemetrics.New()
		nativeMust(t, access.ReadRecords(runtimemetrics.WithRecorder(t.Context(), rec), func(raw CoordinationReader) error {
			reader := raw.(CoordinationInspectorReader)
			run, err := reader.InspectorFacts(RecordKey{"run", "r"})
			if err != nil {
				return err
			}
			workflow, err := reader.InspectorFacts(RecordKey{"workflow", "w"})
			if err != nil {
				return err
			}
			if run.Active != count || run.Pending != count || workflow.DeferredItems != count {
				t.Fatalf("counts lost: run=%+v workflow=%+v want=%d", run, workflow, count)
			}
			return nil
		}))
		snap := rec.Snapshot()
		got := [3]uint64{snap.Count(runtimemetrics.RowsRead), snap.Count(runtimemetrics.PayloadBytesRead), snap.Count(runtimemetrics.SQLStatements)}
		if count == 16 {
			baseline = got
		} else if got != baseline {
			t.Fatalf("counts scanned dependencies: %v vs %v", baseline, got)
		}
		if got[1] != 0 {
			t.Fatalf("counts fetched payloads: %v", got)
		}
	}
}
