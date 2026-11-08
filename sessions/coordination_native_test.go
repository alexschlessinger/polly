package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
)

func nativeMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func nativeHandle(t *testing.T, store *SQLiteStore, name string) *sqliteSession {
	t.Helper()
	return acquireNamed(t, store, name).(*sqliteSession)
}
func nativePut(t *testing.T, s *sqliteSession, kind, id, payload string) CoordinationCommit {
	t.Helper()
	c, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return tx.Put(RecordKey{kind, id}, json.RawMessage(payload)) })
	nativeMust(t, err)
	return c
}
func nativeRevisions(t *testing.T, s *sqliteSession, scopes ...RevisionScope) []Revision {
	t.Helper()
	var out []Revision
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error { var err error; out, err = r.Revisions(scopes); return err }))
	return out
}

func TestNativeRecordsAtomicEffectsAndOwnership(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	parent := nativeHandle(t, store, "root")
	childSession, err := store.Acquire(t.Context(), "child", AcquireOptions{Parent: "root"})
	nativeMust(t, err)
	defer childSession.Close()
	child := childSession.(*sqliteSession)
	ref, err := child.ArtifactStore().Put(t.Context(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("evidence")})
	nativeMust(t, err)
	a, err := OpenCoordinationAccess(parent)
	nativeMust(t, err)
	if a.Capabilities() != (CoordinationCapabilities{true, true}) {
		t.Fatal(a.Capabilities())
	}
	zero := int64(0)
	calls := 0
	veto := errors.New("veto")
	var retained CoordinationTx
	payload := json.RawMessage(`{"to":"child","delivered":true}`)
	apply := func(tx CoordinationTx) error {
		calls++
		retained = tx
		nativeMust(t, tx.Put(RecordKey{"format", "swarm"}, json.RawMessage(`{"version":2}`)))
		nativeMust(t, tx.Put(RecordKey{"mail", "receipt"}, payload))
		got, found, err := tx.Get(RecordKey{"mail", "receipt"})
		nativeMust(t, err)
		if !found || !bytes.Equal(got.Payload, payload) {
			t.Fatal("read-your-writes")
		}
		got.Payload[0] = 'x'
		got, _, err = tx.Get(RecordKey{"mail", "receipt"})
		nativeMust(t, err)
		if !json.Valid(got.Payload) {
			t.Fatal("aliased output")
		}
		msgs := messages.User("checkpoint")
		msgs[0].Metadata = map[string]any{"nested": map[string]any{"owned": true}}
		nativeMust(t, tx.Effects(CoordinationEffects{Append: msgs, ExpectedSequence: &zero, Transcript: &TranscriptIdentity{Execution: "e", Generation: 2}, Pins: []string{ref.ID}, PinOwner: child.ViewID(), Members: []string{child.ViewID()}}))
		msgs[0].Metadata["nested"].(map[string]any)["owned"] = false
		return nil
	}
	c, err := a.UpdateRecords(t.Context(), func(tx CoordinationTx) error { nativeMust(t, apply(tx)); return veto })
	if !errors.Is(err, veto) || calls != 1 || c.Sequence != 0 || len(c.Changed) != 0 {
		t.Fatalf("veto: %+v %v calls=%d", c, err, calls)
	}
	for _, table := range []string{"swarm_records", "swarm_relations", "swarm_revisions", "swarm_artifacts", "swarm_members", "messages"} {
		var n int
		nativeMust(t, store.db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != 0 {
			t.Fatalf("veto leaked %s", table)
		}
	}
	c, err = a.UpdateRecords(t.Context(), apply)
	nativeMust(t, err)
	if calls != 2 || c.Sequence != 1 || len(c.Changed) == 0 {
		t.Fatalf("commit %+v calls=%d", c, calls)
	}
	if _, err := retained.Identity(); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if err := retained.Put(RecordKey{"custom", "late"}, json.RawMessage(`{}`)); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	page, err := store.ReadTranscriptPage(t.Context(), ViewTarget{ID: parent.ViewID()}, TranscriptQuery{Limit: 10})
	nativeMust(t, err)
	if page.Transcript == nil || page.Transcript.Sequence != 1 || page.Transcript.Execution != "e" || page.Messages[0].Metadata["nested"].(map[string]any)["owned"] != true {
		t.Fatalf("effects %+v", page)
	}
	for _, table := range []string{"swarm_artifacts", "swarm_members"} {
		var n int
		nativeMust(t, store.db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != 1 {
			t.Fatalf("lost %s", table)
		}
	}
	c, err = parent.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		return tx.Effects(CoordinationEffects{Append: messages.User("stale"), ExpectedSequence: &zero})
	})
	if !errors.Is(err, ErrCoordinationRequest) || c.Sequence != 0 {
		t.Fatalf("stale: %+v %v", c, err)
	}
	c, err = parent.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		return tx.Effects(CoordinationEffects{Append: messages.User("effects only")})
	})
	nativeMust(t, err)
	if c.Sequence != 2 || len(c.Changed) != 0 {
		t.Fatalf("effects only %+v", c)
	}
	nativeMust(t, store.ReadRecordsView(t.Context(), parent.ViewID(), func(r CoordinationReader) error {
		i, err := r.Identity()
		if i.Sequence != 2 || i.ActorID != parent.ViewID() {
			t.Fatalf("identity %+v", i)
		}
		return err
	}))
	state, err := store.ReadCoordinationView(t.Context(), parent.ViewID())
	nativeMust(t, err)
	if state.Sequence != 2 {
		t.Fatal(state.Sequence)
	}
	if err := store.ReadRecordsView(t.Context(), child.ViewID(), func(CoordinationReader) error { t.Fatal("child view callback"); return nil }); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
	nativeMust(t, child.ReadRecords(t.Context(), func(r CoordinationReader) error {
		i, err := r.Identity()
		if i.Sequence != 0 || i.ActorID != child.ViewID() || i.ParentID != parent.ViewID() {
			t.Fatalf("child identity %+v", i)
		}
		return err
	}))
}

func TestNativeRecordsRevisionsLegacyMovesAndTombstones(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	scopes := []RevisionScope{{CoordinationIndexRecord, CoordinationIndexKey("mail", "m")}, {CoordinationIndexMailRecipient, "a"}, {CoordinationIndexMailRecipient, "b"}, {CoordinationIndexMailPending, "a"}, {CoordinationIndexMailPending, "unrelated"}, {CoordinationIndexRecords, ""}}
	empty := nativeRevisions(t, s, scopes...)
	nativePut(t, s, "mail", "m", `{"to":"a"}`)
	first := nativeRevisions(t, s, scopes...)
	if first[0] == empty[0] || first[1] == empty[1] || first[3] == empty[3] || first[4] != empty[4] {
		t.Fatalf("initial scope revisions: %v %v", empty, first)
	}
	c := nativePut(t, s, "mail", "m", `{"to":"a"}`)
	if len(c.Changed) != 0 || !reflect.DeepEqual(first, nativeRevisions(t, s, scopes...)) {
		t.Fatal("no-op invalidation")
	}
	// The legacy write must invalidate precisely the same old/new partitions.
	nativeMust(t, s.UpdateCoordination(t.Context(), func(state *CoordinationState) error {
		state.Records["mail"]["m"] = json.RawMessage(`{"to":"b","delivered":true}`)
		return nil
	}))
	moved := nativeRevisions(t, s, scopes...)
	for _, i := range []int{0, 1, 2, 3, 5} {
		if moved[i] == first[i] {
			t.Fatalf("unchanged moved scope %v", scopes[i])
		}
	}
	if moved[4] != first[4] {
		t.Fatal("unrelated invalidation")
	}
	c, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return tx.Delete(RecordKey{"mail", "m"}) })
	nativeMust(t, err)
	if !slices.Contains(c.Changed, scopes[2]) {
		t.Fatal("missing deleted partition")
	}
	deleted := nativeRevisions(t, s, scopes...)
	if deleted[0] == empty[0] || deleted[0] == moved[0] {
		t.Fatal("deletion ABA")
	}
	nativePut(t, s, "mail", "m", `{"to":"a"}`)
	recreated := nativeRevisions(t, s, scopes...)
	if recreated[0] == first[0] || recreated[0] == deleted[0] {
		t.Fatal("recreation ABA")
	}
	var count int
	nativeMust(t, store.db.QueryRow(`SELECT record_count FROM swarm_revisions WHERE parent_id=? AND index_name=? AND index_key=?`, s.id, string(CoordinationIndexMailRecipient), "b").Scan(&count))
	if count != 0 {
		t.Fatal("old count", count)
	}
	var retained CoordinationReader
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		retained = r
		if _, ok := r.(CoordinationTx); ok {
			t.Fatal("read exposed mutation")
		}
		rs, err := r.Revisions(make([]RevisionScope, MaxCoordinationRevisionScopes))
		nativeMust(t, err)
		if len(rs) != MaxCoordinationRevisionScopes {
			t.Fatal(len(rs))
		}
		_, err = r.Revisions(make([]RevisionScope, MaxCoordinationRevisionScopes+1))
		if !errors.Is(err, ErrCoordinationRequest) {
			t.Fatal(err)
		}
		return nil
	}))
	if _, err := retained.Revisions(scopes); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	// In-place legacy edits must not mutate the before image used for comparison.
	nativePut(t, s, "custom", "x", `{"n":1}`)
	nativeMust(t, s.UpdateCoordination(t.Context(), func(state *CoordinationState) error { state.Records["custom"]["x"][5] = '2'; return nil }))
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		v, _, err := r.Get(RecordKey{"custom", "x"})
		if string(v.Payload) != `{"n":2}` {
			t.Fatal(string(v.Payload))
		}
		return err
	}))
}

func TestNativeRecordsSelectorParityAndOpaqueCursors(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	records := map[RecordKey]string{
		{"format", "swarm"}:       `{"version":2}`,
		{"mail", "z"}:             `{"to":"a","from":"b","task":"t","posted":"2026-01-01T01:00:00+01:00"}`,
		{"mail", "a"}:             `{"to":"a","posted":"2026-01-01T00:00:00Z","delivered":true}`,
		{"mail", "b"}:             `{"to":"a","posted":"0001-01-01T00:00:00Z"}`,
		{"member", "m"}:           `{"agentName":"/root/team/child","task":"t","context":"c","execution":"e"}`,
		{"execution", "e"}:        `{"member":"m","run":"r","workflow":"w","status":"running","workspace":"c","base":"s"}`,
		{"task", "t"}:             `{"owner":"m","execution":"e","run":"r","dependencies":["d","d"],"status":"working","snapshot":"s"}`,
		{"context", "c"}:          `{"owner":"m"}`,
		{"workflow", "w"}:         `{"run":"r"}`,
		{"workflow_step", "w/10"}: `{}`, {"workflow_step", "w/2"}: `{}`, {"workflow_step", "w/0"}: `{}`,
		{"publication", "p"}:            `{"scope":"snapshot","run":"r","key":"k","value":"v","snapshot":"s","admittedTo":["m","n"],"posted":"2026-01-01T00:00:00Z"}`,
		{"publication", "duplicate"}:    `{"scope":"snapshot","run":"r","key":"k","value":"v","snapshot":"historical-same-commit","duplicateOf":"p","admittedTo":["n"]}`,
		{"publication", "superseding"}:  `{"scope":"snapshot","run":"r","key":"k","value":"v2","supersedes":"p"}`,
		{"publication", "host"}:         `{"kind":"host","key":"host-key"}`,
		{"integration", "i"}:            `{"run":"r","inputs":[{"task":"t","base":{"id":"s"}}]}`,
		{"custom", string([]byte{255})}: `null`, {"custom", string([]byte{255, 'z'})}: `[]`,
		{string([]byte{'x', 255}), "one"}: `{}`, {string([]byte{'x', 255}), "two"}: `{}`,
	}
	scopes := map[RevisionScope]bool{}
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		for key, payload := range records {
			nativeMust(t, tx.Put(key, json.RawMessage(payload)))
			rs, _, err := coordindex.Extract(key.Kind, key.ID, json.RawMessage(payload))
			nativeMust(t, err)
			for _, r := range rs {
				scopes[RevisionScope{CoordinationIndex(r.Index), r.Key}] = true
			}
		}
		return nil
	})
	nativeMust(t, err)
	legacy, err := OpenCoordinationAccess(struct{ CoordinationSession }{s})
	nativeMust(t, err)
	for scope := range scopes {
		q := RecordQuery{Index: scope.Index, Key: scope.Key, Limit: 1000, IncludeTotal: true}
		var want RecordPage
		nativeMust(t, legacy.ReadRecords(t.Context(), func(r CoordinationReader) error { var err error; want, err = r.Query(q); return err }))
		var got []CoordinationRecord
		q.Limit = 1
		for pages := 0; ; pages++ {
			if pages > len(records) {
				t.Fatal("cursor loop", scope)
			}
			var page RecordPage
			nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error { var err error; page, err = r.Query(q); return err }))
			if page.Total != want.Total {
				t.Fatalf("scope %v count=%d want=%d", scope, page.Total, want.Total)
			}
			got = append(got, page.Records...)
			if page.Next == "" {
				break
			}
			q.After = page.Next
		}
		if len(got) != len(want.Records) {
			t.Fatalf("scope %v length %d want %d", scope, len(got), len(want.Records))
		}
		for i, v := range got {
			if v.Key != want.Records[i].Key || !bytes.Equal(v.Payload, want.Records[i].Payload) {
				t.Fatalf("scope %v position %d: %v want %v", scope, i, v.Key, want.Records[i].Key)
			}
		}
	}
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		p, err := r.Query(RecordQuery{Kind: "mail", Limit: 100, MaxBytes: 1})
		nativeMust(t, err)
		if !p.Oversized || len(p.Records) != 1 || p.Next == "" {
			t.Fatal(p)
		}
		_, err = r.Query(RecordQuery{Kind: "publication", After: p.Next, Limit: 1})
		if !errors.Is(err, ErrCoordinationCursor) {
			t.Fatal(err)
		}
		p, err = r.Query(RecordQuery{Index: CoordinationIndexPublicationClaim, Key: CoordinationIndexKey("snapshot", "r", "k"), Limit: 10})
		nativeMust(t, err)
		if len(p.Records) != 3 {
			t.Fatalf("claim must preserve duplicates/superseded candidates: %v", p)
		}
		p, err = r.Query(RecordQuery{Index: CoordinationIndexPublicationExactReceipt, Key: CoordinationIndexKey("m", "duplicate"), Limit: 10})
		nativeMust(t, err)
		if len(p.Records) != 0 {
			t.Fatal("receipt inferred from candidate equivalence")
		}
		return nil
	}))
	nativePut(t, s, "format", "swarm", `{"version":99}`)
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		if _, _, err := r.Get(RecordKey{"member", "m"}); !errors.Is(err, ErrCoordinationFormat) {
			t.Fatal(err)
		}
		if _, err := r.Query(RecordQuery{Index: CoordinationIndexMailRecipient, Key: "absent", Limit: 1}); !errors.Is(err, ErrCoordinationFormat) {
			t.Fatal(err)
		}
		if _, err := r.Revisions([]RevisionScope{{CoordinationIndexOverviewDependency, CoordinationIndexKey("task", "t")}}); !errors.Is(err, ErrCoordinationFormat) {
			t.Fatal(err)
		}
		_, found, err := r.Get(RecordKey{"custom", string([]byte{255})})
		if !found {
			t.Fatal("unknown kind lost")
		}
		return err
	}))
}

func TestRuntimeStorageScaleNativeBoundedRecords(t *testing.T) {
	var baseline runtimemetrics.Snapshot
	var baselinePollAllocs, baselinePointAllocs float64
	for _, size := range []int{16, 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativeMust(t, s.UpdateCoordination(t.Context(), func(state *CoordinationState) error {
				state.Records["format"] = map[string]json.RawMessage{"swarm": json.RawMessage(`{"version":2}`)}
				state.Records["mail"] = map[string]json.RawMessage{"selected": json.RawMessage(`{"to":"selected"}`)}
				state.Records["custom"] = map[string]json.RawMessage{}
				for i := 0; i < size; i++ {
					state.Records["custom"][fmt.Sprint(i)] = json.RawMessage(`{"unrelated":"` + strings.Repeat("x", 512) + `"}`)
				}
				return nil
			}))
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			nativeMust(t, s.ReadRecords(ctx, func(r CoordinationReader) error {
				_, err := r.Identity()
				nativeMust(t, err)
				_, err = r.Revisions([]RevisionScope{{CoordinationIndexMailPending, "selected"}, {CoordinationIndexTaskDependency, "unrelated"}})
				return err
			}))
			counts := recorder.Snapshot()
			if counts.Count(runtimemetrics.PayloadBytesRead) != 0 || counts.Count(runtimemetrics.RecordsDecoded) != 0 || counts.Count(runtimemetrics.SQLStatements) != 2 || counts.Count(runtimemetrics.RowsRead) != 3 {
				t.Fatalf("identity/revisions fetched payloads: %+v", counts)
			}
			recorder = runtimemetrics.New()
			ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
			_, err := s.UpdateRecords(ctx, func(tx CoordinationTx) error {
				return tx.Put(RecordKey{"mail", "selected"}, json.RawMessage(`{"to":"selected","delivered":true}`))
			})
			nativeMust(t, err)
			point := recorder.Snapshot()
			if point.Count(runtimemetrics.CoordinationRecordsRead) != 1 || point.Count(runtimemetrics.RecordsDecoded) != 1 || point.Count(runtimemetrics.RecordsEncoded) != 0 {
				t.Fatalf("point copied unrelated records: %+v", point)
			}
			if size == 16 {
				baseline = point
			} else {
				for _, c := range []runtimemetrics.Counter{runtimemetrics.SQLStatements, runtimemetrics.RowsRead, runtimemetrics.PayloadBytesRead, runtimemetrics.RecordsDecoded} {
					if point.Count(c) != baseline.Count(c) {
						t.Fatalf("point work grew counter=%d: %d -> %d", c, baseline.Count(c), point.Count(c))
					}
				}
			}
			recorder = runtimemetrics.New()
			ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
			nativeMust(t, s.ReadRecords(ctx, func(r CoordinationReader) error {
				page, err := r.Query(RecordQuery{Index: CoordinationIndexMailRecipient, Key: "selected", Limit: 1, IncludeTotal: true})
				if len(page.Records) != 1 || page.Total != 1 {
					t.Fatal(page)
				}
				return err
			}))
			query := recorder.Snapshot()
			pollAllocs := testing.AllocsPerRun(10, func() {
				nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
					_, err := r.Revisions([]RevisionScope{{CoordinationIndexMailPending, "selected"}})
					return err
				}))
			})
			toggle := false
			pointAllocs := testing.AllocsPerRun(10, func() {
				toggle = !toggle
				payload := json.RawMessage(`{"to":"selected","delivered":true}`)
				if toggle {
					payload = json.RawMessage(`{"to":"selected","delivered":false}`)
				}
				_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return tx.Put(RecordKey{"mail", "selected"}, payload) })
				nativeMust(t, err)
			})
			if size == 16 {
				baselinePollAllocs, baselinePointAllocs = pollAllocs, pointAllocs
			} else if pollAllocs > baselinePollAllocs*1.25+20 || pointAllocs > baselinePointAllocs*1.25+20 {
				t.Fatalf("unrelated records grew allocations: poll %.0f -> %.0f point %.0f -> %.0f", baselinePollAllocs, pollAllocs, baselinePointAllocs, pointAllocs)
			}
			t.Logf("allocation coverage=native operation including SQLite driver, recorder disabled: unrelated=%d revision_poll_allocs=%.0f point_change_allocs=%.0f", size, pollAllocs, pointAllocs)
			if query.Count(runtimemetrics.CoordinationRecordsRead) != 1 || query.Count(runtimemetrics.RecordsDecoded) != 1 {
				t.Fatal("query decoded unrelated rows")
			}
			t.Logf("coverage=native-dispatched-SQL/returned-rows/payload-bytes (excludes transaction/lease SQL); unrelated=%d identity_revision statements=%d rows=%d payload=0; point statements=%d rows=%d payload=%d decoded=%d encoded=%d; query statements=%d rows=%d payload=%d decoded=%d", size, counts.Count(runtimemetrics.SQLStatements), counts.Count(runtimemetrics.RowsRead), point.Count(runtimemetrics.SQLStatements), point.Count(runtimemetrics.RowsRead), point.Count(runtimemetrics.PayloadBytesRead), point.Count(runtimemetrics.RecordsDecoded), point.Count(runtimemetrics.RecordsEncoded), query.Count(runtimemetrics.SQLStatements), query.Count(runtimemetrics.RowsRead), query.Count(runtimemetrics.PayloadBytesRead), query.Count(runtimemetrics.RecordsDecoded))
		})
	}
}

func TestNativeRecordLeaseCancellationAndFailedMutation(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.UpdateRecords(ctx, func(CoordinationTx) error { t.Fatal("canceled callback"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	_, err := s.UpdateRecords(ctx, func(tx CoordinationTx) error {
		nativeMust(t, tx.Put(RecordKey{"custom", "cancel"}, json.RawMessage(`{}`)))
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// A trigger error after deleting old relations must poison the transaction,
	// even when the callback mistakenly ignores the method's error.
	nativePut(t, s, "custom", "old", `{"v":1}`)
	_, err = store.db.Exec(`CREATE TEMP TRIGGER fail_native_update BEFORE UPDATE ON swarm_records BEGIN SELECT RAISE(ABORT,'injected write failure'); END`)
	nativeMust(t, err)
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		_ = tx.Put(RecordKey{"custom", "old"}, json.RawMessage(`{"v":2}`))
		return nil
	})
	if err == nil {
		t.Fatal("swallowed failure committed")
	}
	_, err = store.db.Exec(`DROP TRIGGER fail_native_update`)
	nativeMust(t, err)
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		p, err := r.Query(RecordQuery{Kind: "custom", Limit: 10})
		if len(p.Records) != 1 || string(p.Records[0].Payload) != `{"v":1}` {
			t.Fatal(p)
		}
		return err
	}))
	_, err = store.db.Exec(`UPDATE session_leases SET owner_token=randomblob(16) WHERE session_id=?`, s.id)
	nativeMust(t, err)
	if _, err = s.UpdateRecords(t.Context(), func(CoordinationTx) error { t.Fatal("lost-lease callback"); return nil }); !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatal(err)
	}
}

func TestNativeSelectionQueryPlans(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	tests := []struct {
		name, query, want string
		args              []any
	}{
		{"recipient", `SELECT r.kind,r.id,r.sort_key,length(s.payload_json) FROM swarm_relations r JOIN swarm_records s ON s.parent_id=r.parent_id AND s.kind=r.kind AND s.id=r.id WHERE r.parent_id=? AND r.index_name=? AND r.index_key=? ORDER BY r.sort_key LIMIT 2`, "SEARCH r USING PRIMARY KEY", []any{[]byte{}, "mail.recipient", "a"}},
		{"dependency", `SELECT kind,id FROM swarm_relations WHERE parent_id=? AND index_name=? AND index_key=? ORDER BY sort_key LIMIT 2`, "SEARCH swarm_relations USING PRIMARY KEY", []any{[]byte{}, "task.dependency", "a"}},
		{"summary ID", snapshotSelect + ` WHERE s.id=?`, "SEARCH s USING INDEX", []any{[]byte{}}},
		{"parent summary", `SELECT s.id FROM sessions s WHERE s.parent_id=? ORDER BY s.updated_ns DESC,s.name,s.id LIMIT 2`, "sessions_parent_summary_idx", []any{[]byte{}}},
		{"transcript", `SELECT sequence,length(payload_json) FROM messages WHERE session_id=? AND sequence>=? AND sequence<? ORDER BY sequence LIMIT 2`, "SEARCH messages USING PRIMARY KEY", []any{[]byte{}, 0, 10}},
		{"spawn", `SELECT c.id FROM sessions c JOIN sessions p ON p.id=c.parent_id WHERE p.name=? AND json_extract(c.settings_json,'$.spawnCallID')=? LIMIT 2`, "sessions_spawn_idx", []any{"root", "spawn"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var plan []string
			nativeMust(t, queryEach(t.Context(), store.db, "EXPLAIN QUERY PLAN "+test.query, test.args, func(rows *sql.Rows) error {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					return err
				}
				plan = append(plan, detail)
				return nil
			}))
			joined := strings.Join(plan, "; ")
			if !strings.Contains(joined, test.want) || strings.Contains(joined, "TEMP B-TREE") {
				t.Fatalf("plan %s", joined)
			}
			t.Log(joined)
		})
	}
}
