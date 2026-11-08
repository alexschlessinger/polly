package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestCoordinationIDPagesParityAndContinuations(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			access := lifecycleAccess(t, native)
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for _, kind := range []string{"mail", "publication"} {
					for i, id := range []string{"z", "a", "b"} {
						data, _ := json.Marshal(map[string]any{"id": id, "posted": fmt.Sprintf("2025-01-0%dT00:00:00Z", i+1), "text": strings.Repeat(id, 1000)})
						if err := tx.Put(RecordKey{kind, id}, data); err != nil {
							return err
						}
					}
				}
				return nil
			})
			nativeMust(t, err)
			for _, kind := range []string{"mail", "publication"} {
				query := CoordinationIDQuery{Kind: kind, Limit: 2, MaxBytes: 100}
				var ids []string
				for {
					var page RecordPage
					nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
						var err error
						page, err = QueryRecordsByID(r, query)
						return err
					}))
					if page.Total != 3 || len(page.Records) != 1 || !page.Oversized || len(page.Records[0].Payload) < 1000 {
						t.Fatalf("not intact/oversized: %+v", page)
					}
					ids = append(ids, page.Records[0].Key.ID)
					if page.Next == "" {
						break
					}
					if page.Next == query.After {
						t.Fatal("nonadvancing page")
					}
					query.After = page.Next
				}
				if !reflect.DeepEqual(ids, []string{"a", "b", "z"}) {
					t.Fatalf("ID order changed: %v", ids)
				}
				nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
					query.Kind = "task"
					if _, err := QueryRecordsByID(r, query); !errors.Is(err, ErrCoordinationCursor) {
						t.Fatalf("foreign cursor accepted: %v", err)
					}
					return nil
				}))
			}
		})
	}
}

func TestCoordinationIDPageNativeSelectedWork(t *testing.T) {
	var baseline [3]uint64
	for _, count := range []int{16, 1024} {
		access := lifecycleAccess(t, true)
		_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("%06d", i)
				data, _ := json.Marshal(map[string]string{"id": id, "text": strings.Repeat("x", 4096)})
				if err := tx.Put(RecordKey{"mail", id}, data); err != nil {
					return err
				}
			}
			return nil
		})
		nativeMust(t, err)
		recorder := runtimemetrics.New()
		nativeMust(t, access.ReadRecords(runtimemetrics.WithRecorder(t.Context(), recorder), func(r CoordinationReader) error {
			page, err := QueryRecordsByID(r, CoordinationIDQuery{Kind: "mail", Limit: 2, MaxBytes: 5000})
			if err == nil && (len(page.Records) != 1 || page.Next == "" || page.Total != count) {
				t.Fatalf("page %+v", page)
			}
			return err
		}))
		snap := recorder.Snapshot()
		observed := [3]uint64{snap.Count(runtimemetrics.RowsRead), snap.Count(runtimemetrics.PayloadBytesRead), snap.Count(runtimemetrics.SQLStatements)}
		if count == 16 {
			baseline = observed
		} else if observed != baseline {
			t.Fatalf("unrelated rows changed work: %v vs %v", baseline, observed)
		}
		t.Logf("%d records: rows/bytes/sql=%v", count, observed)
	}
}
