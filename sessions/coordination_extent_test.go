package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func extentQuery(q RecordQuery) RecordQuery {
	q.CountOnly, q.IncludeTotal, q.IncludeLast = true, true, true
	return q
}

func checkExtent(t *testing.T, r CoordinationReader, q RecordQuery, total int, last *RecordKey) RecordPage {
	t.Helper()
	page, err := r.Query(extentQuery(q))
	nativeMust(t, err)
	if page.Total != total || !reflect.DeepEqual(page.Last, last) || page.Records != nil || page.Next != "" || page.Oversized {
		t.Fatalf("query %+v: %+v last=%v, want total=%d last=%v", q, page, page.Last, total, last)
	}
	return page
}

func TestCoordinationExtentNativeLegacyOrderingAndSelectors(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			records := map[RecordKey]string{
				{"format", "swarm"}:       `{"version":2}`,
				{"mail", "a"}:             `{"from":"sender","callID":"c","to":"m","task":"t","start":true,"posted":"2030-01-01T00:00:00Z"}`,
				{"mail", "z"}:             `{"from":"sender","callID":"c","to":"m","task":"t","start":true,"posted":"2020-01-01T00:00:00Z"}`,
				{"mail", "elsewhere"}:     `{"to":"other","delivered":true,"posted":"2040-01-01T00:00:00Z"}`,
				{"member", "m"}:           `{"agentName":"/root/a","task":"t","execution":"e","context":"c"}`,
				{"member", "n"}:           `{"agentName":"/root/a/child","task":"t"}`,
				{"execution", "e"}:        `{"member":"m","run":"r","workflow":"w","status":"running","request":{"callID":"c"}}`,
				{"execution", "f"}:        `{"member":"m","run":"r","workflow":"w","status":"running","request":{"callID":"c"}}`,
				{"task", "t"}:             `{"owner":"m","execution":"e","dependencies":["old","old"],"status":"running","follows":"old","followupCallID":"c"}`,
				{"task", "u"}:             `{"owner":"m","execution":"e","dependencies":["old"],"status":"running","follows":"old","followupCallID":"c"}`,
				{"context", "c"}:          `{"owner":"m"}`,
				{"context", "d"}:          `{"owner":"m","release":"done"}`,
				{"snapshot", "s"}:         `{"source":"source"}`,
				{"workflow", "w"}:         `{"run":"r"}`,
				{"workflow_step", "w/2"}:  `{"id":"duplicate"}`,
				{"workflow_step", "w/9"}:  `{"id":"other"}`,
				{"workflow_step", "w/10"}: `{"id":"duplicate"}`,
				{"workflow_step", "x/0"}:  `{"id":"duplicate"}`,
				{"publication", "a"}:      `{"scope":"snapshot","run":"r","key":"fact","author":"m","snapshot":"s","admittedTo":["m"],"supersedes":"old","posted":"2030-01-01T00:00:00Z"}`,
				{"publication", "z"}:      `{"scope":"snapshot","run":"r","key":"fact","author":"m","snapshot":"s","admittedTo":["m"],"supersedes":"old","posted":"2020-01-01T00:00:00Z"}`,
				{"custom", "10"}:          `null`,
				{"custom", "9"}:           `[]`,
				{"opaque", "\x00"}:        `{}`,
				{"opaque", "\xff"}:        `{}`,
				{"opaque", "\xffz"}:       `{}`,
				{"\xffkind", "\x00\xff"}:  `{}`,
			}
			scopes := map[RevisionScope]bool{}
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for key, payload := range records {
					if err := tx.Put(key, json.RawMessage(payload)); err != nil {
						return err
					}
					relations, _, err := coordindex.Extract(key.Kind, key.ID, json.RawMessage(payload))
					if err != nil {
						return err
					}
					for _, relation := range relations {
						scopes[RevisionScope{CoordinationIndex(relation.Index), relation.Key}] = true
					}
				}
				return nil
			})
			nativeMust(t, err)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			nativeMust(t, access.ReadRecords(ctx, func(r CoordinationReader) error {
				// Every extracted supported partition, including tuples and exact
				// singleton selectors, must agree with the ordinary declared order.
				for scope := range scopes {
					q := RecordQuery{Index: scope.Index, Key: scope.Key, Limit: MaxCoordinationPageLimit, IncludeTotal: true}
					ordinary, err := r.Query(q)
					nativeMust(t, err)
					if ordinary.Last != nil || ordinary.Next != "" || len(ordinary.Records) != ordinary.Total {
						t.Fatalf("ordinary query changed: %+v", ordinary)
					}
					q.Limit = 0
					before := recorder.Snapshot()
					checkExtent(t, r, q, ordinary.Total, &ordinary.Records[len(ordinary.Records)-1].Key)
					if native {
						checkExtentNoPayload(t, before, recorder.Snapshot())
					}
					q.CountOnly = true
					count, err := r.Query(q)
					nativeMust(t, err)
					if count.Last != nil || count.Total != ordinary.Total {
						t.Fatalf("unrequested extent: %+v", count)
					}
				}
				// Explicit expectations distinguish numeric, time and bytewise order.
				for _, test := range []struct {
					q     RecordQuery
					total int
					last  RecordKey
				}{
					{RecordQuery{}, len(records), RecordKey{"\xffkind", "\x00\xff"}},
					{RecordQuery{Kind: "custom"}, 2, RecordKey{"custom", "9"}},
					{RecordQuery{Index: CoordinationIndexKind, Key: "opaque"}, 3, RecordKey{"opaque", "\xffz"}},
					{RecordQuery{Index: CoordinationIndexMailRecipient, Key: "m"}, 2, RecordKey{"mail", "a"}},
					{RecordQuery{Index: CoordinationIndexPublicationClaim, Key: CoordinationIndexKey("snapshot", "r", "fact")}, 2, RecordKey{"publication", "a"}},
					{RecordQuery{Index: CoordinationIndexWorkflowSteps, Key: "w"}, 3, RecordKey{"workflow_step", "w/10"}},
					{RecordQuery{Index: CoordinationIndexWorkflowStepID, Key: CoordinationIndexKey("w", "duplicate")}, 2, RecordKey{"workflow_step", "w/10"}},
					{RecordQuery{Kind: "workflow_step"}, 4, RecordKey{"workflow_step", "x/0"}},
					{RecordQuery{Index: CoordinationIndexOverviewDependency, Key: CoordinationIndexKey("task", "t")}, 4, RecordKey{"member", "n"}},
					{RecordQuery{Index: CoordinationIndexOverviewDependency, Key: CoordinationIndexKey("task", "t"), Kind: "mail"}, 2, RecordKey{"mail", "a"}},
				} {
					checkExtent(t, r, test.q, test.total, &test.last)
				}
				for _, q := range []RecordQuery{
					{Kind: "absent"},
					{Index: CoordinationIndexMailRecipient, Key: "absent' OR 1=1 --"},
					{Index: CoordinationIndexWorkflowSteps, Key: "absent"},
					{Index: CoordinationIndexOverviewDependency, Key: CoordinationIndexKey("task", "t"), Kind: "custom"},
				} {
					checkExtent(t, r, q, 0, nil)
				}
				return nil
			}))
		})
	}
}

func checkExtentNoPayload(t *testing.T, before, after runtimemetrics.Snapshot) {
	t.Helper()
	for _, counter := range []runtimemetrics.Counter{runtimemetrics.PayloadBytesRead, runtimemetrics.RecordsDecoded, runtimemetrics.RecordsEncoded, runtimemetrics.CoordinationRecordsRead, runtimemetrics.CoordinationBytesRead} {
		if got := after.Count(counter) - before.Count(counter); got != 0 {
			t.Fatalf("extent used payload: counter %v=%d", counter, got)
		}
	}
}

func TestCoordinationExtentReadYourWritesOwnershipAndNoop(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			q := RecordQuery{Index: CoordinationIndexWorkflowSteps, Key: "w"}
			key := RecordKey{"workflow_step", "w/10"}
			payload := json.RawMessage(`{"id":"s"}`)
			var retained RecordPage
			var reader CoordinationReader
			_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				reader = tx
				checkExtent(t, tx, q, 0, nil)
				nativeMust(t, tx.Put(key, payload))
				retained = checkExtent(t, tx, q, 1, &key)
				owned := checkExtent(t, tx, q, 1, &key)
				owned.Last.Kind, owned.Last.ID = "changed", "changed"
				checkExtent(t, tx, q, 1, &key)
				nativeMust(t, tx.Delete(key))
				checkExtent(t, tx, q, 0, nil)
				nativeMust(t, tx.Put(key, payload))
				checkExtent(t, tx, q, 1, &key)
				return nil
			})
			nativeMust(t, err)
			if retained.Last == nil || *retained.Last != key {
				t.Fatalf("retained extent changed: %+v", retained)
			}
			if _, err := reader.Query(extentQuery(q)); !errors.Is(err, ErrCoordinationClosed) {
				t.Fatalf("retained reader: %v", err)
			}
			commit, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				before, err := tx.Revisions([]RevisionScope{{q.Index, q.Key}})
				nativeMust(t, err)
				nativeMust(t, tx.Put(key, payload))
				nativeMust(t, tx.Delete(RecordKey{"workflow_step", "w/9"}))
				checkExtent(t, tx, q, 1, &key)
				after, err := tx.Revisions([]RevisionScope{{q.Index, q.Key}})
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("extent/no-op invalidated revisions: %v -> %v", before, after)
				}
				return err
			})
			nativeMust(t, err)
			if len(commit.Changed) != 0 {
				t.Fatalf("extent/no-op changed scopes: %+v", commit)
			}
			nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
				checkExtent(t, r, q, 1, &key)
				return nil
			}))
		})
	}
}

func TestCoordinationExtentValidationAndCancellation(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			nativeMust(t, access.ReadRecords(ctx, func(r CoordinationReader) error {
				before := recorder.Snapshot()
				for _, q := range []RecordQuery{
					{IncludeLast: true},
					{IncludeLast: true, IncludeTotal: true, Limit: 1},
					{IncludeLast: true, CountOnly: true},
					{IncludeLast: true, CountOnly: true, IncludeTotal: true, Limit: 1},
					{IncludeLast: true, CountOnly: true, IncludeTotal: true, Limit: -1},
					{IncludeLast: true, CountOnly: true, IncludeTotal: true, Offset: 1},
					{IncludeLast: true, CountOnly: true, IncludeTotal: true, Offset: -1},
					{IncludeLast: true, CountOnly: true, IncludeTotal: true, MaxBytes: 1},
					{IncludeLast: true, CountOnly: true, IncludeTotal: true, MaxBytes: -1},
					{IncludeLast: true, CountOnly: true, IncludeTotal: true, After: "invalid"},
				} {
					if _, err := r.Query(q); !errors.Is(err, ErrCoordinationRequest) {
						t.Fatalf("accepted invalid extent %+v: %v", q, err)
					}
				}
				for _, q := range []RecordQuery{
					{Index: "unsupported"},
					{Index: CoordinationIndexRecords, Key: "not-empty"},
					{Index: CoordinationIndexWorkflowStepID, Key: "malformed"},
					{Index: CoordinationIndexWorkflowSteps, Kind: "mail"},
				} {
					if _, err := r.Query(extentQuery(q)); !errors.Is(err, ErrCoordinationSelector) {
						t.Fatalf("accepted unsupported extent %+v: %v", q, err)
					}
				}
				if got := recorder.Snapshot().Count(runtimemetrics.SQLStatements) - before.Count(runtimemetrics.SQLStatements); got != 0 {
					t.Fatalf("malformed query dispatched %d SQL statements", got)
				}
				return nil
			}))
			ctx, cancel := context.WithCancel(ctx)
			err := access.ReadRecords(ctx, func(r CoordinationReader) error {
				cancel()
				_, err := r.Query(extentQuery(RecordQuery{Kind: "custom"}))
				return err
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("extent cancellation: %v", err)
			}
			_, err = access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				nativeMust(t, tx.Put(RecordKey{"workflow_step", "w/0"}, json.RawMessage(`{}`)))
				return tx.Put(RecordKey{"format", "swarm"}, json.RawMessage(`{"version":99}`))
			})
			nativeMust(t, err)
			err = access.ReadRecords(t.Context(), func(r CoordinationReader) error {
				_, err := r.Query(extentQuery(RecordQuery{Index: CoordinationIndexWorkflowSteps, Key: "w"}))
				return err
			})
			if !errors.Is(err, ErrCoordinationFormat) {
				t.Fatalf("extent bypassed explicit format error: %v", err)
			}
		})
	}
	// Validation precedes even legacy payload interpretation.
	fixture := newRecordLegacyFixture()
	fixture.state.Records["workflow_step"] = map[string]json.RawMessage{"w/01": json.RawMessage(`{}`)}
	access := recordAccess(t, fixture)
	nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
		if _, err := r.Query(RecordQuery{IncludeLast: true, Kind: "workflow_step", Limit: 1}); !errors.Is(err, ErrCoordinationRequest) {
			t.Fatalf("legacy interpreted malformed data before validation: %v", err)
		}
		if _, err := r.Query(extentQuery(RecordQuery{Index: CoordinationIndexWorkflowSteps, Key: "w"})); !errors.Is(err, ErrCoordinationFormat) {
			t.Fatalf("legacy extent omitted extraction: %v", err)
		}
		return nil
	}))
}
