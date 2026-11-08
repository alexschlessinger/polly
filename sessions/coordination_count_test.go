package sessions

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestCoordinationCountOnlyNativeLegacyParity(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			records := map[RecordKey]string{
				{"mail", "large"}: `{"to":"m","from":"sender","callID":"c","task":"t","start":true,"text":"` + strings.Repeat("x", 1<<20) + `"}`,
				{"mail", "read"}:  `{"to":"m","from":"sender","callID":"c","delivered":true}`,
				{"member", "m"}:   `{"agentName":"/root/a","task":"t"}`,
				{"task", "t"}:     `{"follows":"old","followupCallID":"c"}`,
				{"custom", "x"}:   `null`,
			}
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for key, payload := range records {
					if err := tx.Put(key, json.RawMessage(payload)); err != nil {
						return err
					}
				}
				return nil
			})
			nativeMust(t, err)
			for _, test := range []struct {
				query RecordQuery
				total int
			}{
				{RecordQuery{}, 6},
				{RecordQuery{Kind: "mail"}, 2},
				{RecordQuery{Index: CoordinationIndexKind, Key: "mail"}, 2},
				{RecordQuery{Index: CoordinationIndexRecord, Key: CoordinationIndexKey("mail", "large")}, 1},
				{RecordQuery{Index: CoordinationIndexMailPendingStartRecipient, Key: "m"}, 1},
				{RecordQuery{Index: CoordinationIndexMailFromCall, Key: CoordinationIndexKey("sender", "c")}, 2},
				{RecordQuery{Index: CoordinationIndexMemberName, Key: "/root/a"}, 1},
				{RecordQuery{Index: CoordinationIndexTaskFollowupCall, Key: CoordinationIndexKey("old", "c")}, 1},
				{RecordQuery{Index: CoordinationIndexOverviewDependency, Key: CoordinationIndexKey("task", "t")}, 2},
				{RecordQuery{Index: CoordinationIndexOverviewDependency, Key: CoordinationIndexKey("task", "t"), Kind: "mail"}, 1},
				{RecordQuery{Index: CoordinationIndexOverviewDependency, Key: CoordinationIndexKey("task", "t"), Kind: "custom"}, 0},
				{RecordQuery{Index: CoordinationIndexMailRecipient, Key: "absent' OR 1=1 --"}, 0},
				{RecordQuery{Kind: "absent"}, 0},
			} {
				recorder := runtimemetrics.New()
				ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
				query := test.query
				query.IncludeTotal, query.CountOnly = true, true
				nativeMust(t, access.ReadRecords(ctx, func(r CoordinationReader) error {
					page, err := r.Query(query)
					if err != nil {
						return err
					}
					if page.Total != test.total || page.Records != nil || page.Next != "" || page.Oversized {
						t.Fatalf("query %+v: %+v want only total %d", query, page, test.total)
					}
					return nil
				}))
				if native {
					snapshot := recorder.Snapshot()
					for _, counter := range []runtimemetrics.Counter{runtimemetrics.PayloadBytesRead, runtimemetrics.RecordsDecoded, runtimemetrics.CoordinationRecordsRead, runtimemetrics.CoordinationBytesRead} {
						if got := snapshot.Count(counter); got != 0 {
							t.Fatalf("count-only read payload: query %+v counter %v=%d", query, counter, got)
						}
					}
					if snapshot.Count(runtimemetrics.SQLStatements) == 0 {
						t.Fatal("count metric test did not observe native SQL")
					}
				}
			}
		})
	}
}

func TestCoordinationCountOnlyValidation(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
				for _, query := range []RecordQuery{
					{CountOnly: true},
					{CountOnly: true, IncludeTotal: true, Limit: 1},
					{CountOnly: true, IncludeTotal: true, Limit: -1},
					{CountOnly: true, IncludeTotal: true, Offset: 1},
					{CountOnly: true, IncludeTotal: true, Offset: -1},
					{CountOnly: true, IncludeTotal: true, After: "cursor"},
					{CountOnly: true, IncludeTotal: true, MaxBytes: 1},
					{CountOnly: true, IncludeTotal: true, MaxBytes: -1},
					{IncludeTotal: true}, // Limit=0 remains invalid for a record page.
				} {
					if _, err := r.Query(query); !errors.Is(err, ErrCoordinationRequest) {
						t.Fatalf("accepted %+v: %v", query, err)
					}
				}
				for _, query := range []RecordQuery{
					{Index: "not-an-index"},
					{Index: CoordinationIndexMailFromCall, Key: "!"},
					{Index: CoordinationIndexMailRecipient, Key: "m", Kind: "task"},
				} {
					query.IncludeTotal, query.CountOnly = true, true
					if _, err := r.Query(query); !errors.Is(err, ErrCoordinationSelector) {
						t.Fatalf("accepted selector %+v: %v", query, err)
					}
				}
				return nil
			}))
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				if err := tx.Put(RecordKey{"format", "swarm"}, json.RawMessage(`{"version":99}`)); err != nil {
					return err
				}
				return tx.Put(RecordKey{"mail", "m"}, json.RawMessage(`{"to":"m"}`))
			})
			nativeMust(t, err)
			err = access.ReadRecords(t.Context(), func(r CoordinationReader) error {
				_, err := r.Query(RecordQuery{Index: CoordinationIndexMailRecipient, Key: "m", IncludeTotal: true, CountOnly: true})
				return err
			})
			if !errors.Is(err, ErrCoordinationFormat) {
				t.Fatalf("count-only bypassed format check: %v", err)
			}
		})
	}
	fixture := newRecordLegacyFixture()
	fixture.state.Records["mail"] = map[string]json.RawMessage{"invalid": json.RawMessage(`{"start":"true"}`)}
	access := recordAccess(t, fixture)
	err := access.ReadRecords(t.Context(), func(r CoordinationReader) error {
		_, err := r.Query(RecordQuery{Index: CoordinationIndexMailRecipient, Key: "m", IncludeTotal: true, CountOnly: true})
		return err
	})
	if !errors.Is(err, ErrCoordinationFormat) || fixture.reads != 1 {
		t.Fatalf("legacy must extract supplied family, not infer fast counts: %v reads=%d", err, fixture.reads)
	}
}
