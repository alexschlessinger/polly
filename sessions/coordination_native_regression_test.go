package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestNativeRevisionMatchesReturnedPayload(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	var observed CoordinationRecord
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		if err := tx.Put(RecordKey{"custom", "x"}, json.RawMessage(`{"n":1}`)); err != nil {
			return err
		}
		var found bool
		var err error
		observed, found, err = tx.Get(RecordKey{"custom", "x"})
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("missing staged record")
		}
		return tx.Put(RecordKey{"custom", "x"}, json.RawMessage(`{"n":2}`))
	})
	nativeMust(t, err)
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		fresh, found, err := r.Get(observed.Key)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("missing committed record")
		}
		if string(observed.Payload) != `{"n":1}` || string(fresh.Payload) != `{"n":2}` {
			t.Fatalf("staged/committed payloads changed: %s / %s", observed.Payload, fresh.Payload)
		}
		if fresh.Revision == observed.Revision {
			t.Fatalf("same durable token for different returned payloads: staged=%s committed=%s revision=%+v", observed.Payload, fresh.Payload, fresh.Revision)
		}
		return nil
	}))
}

func TestNativeScopedCountsRepeatedWrites(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		for _, payload := range []string{`{"to":"a"}`, `{"to":"b","delivered":true}`, `{"to":"a"}`} {
			if err := tx.Put(RecordKey{"mail", "x"}, json.RawMessage(payload)); err != nil {
				return err
			}
		}
		if err := tx.Delete(RecordKey{"mail", "x"}); err != nil {
			return err
		}
		return tx.Put(RecordKey{"mail", "x"}, json.RawMessage(`{"to":"c"}`))
	})
	nativeMust(t, err)
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		for _, key := range []string{"a", "b", "c"} {
			p, err := r.Query(RecordQuery{Index: CoordinationIndexMailRecipient, Key: key, Limit: 10, IncludeTotal: true})
			if err != nil {
				return err
			}
			want := 0
			if key == "c" {
				want = 1
			}
			if p.Total != want || len(p.Records) != want {
				t.Fatalf("partition %q: %+v", key, p)
			}
		}
		return nil
	}))
}

func TestNativePartitionRevisionAfterSecondInsert(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	scope := RevisionScope{CoordinationIndexMailRecipient, "a"}
	var observed Revision
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		if err := tx.Put(RecordKey{"mail", "one"}, json.RawMessage(`{"to":"a"}`)); err != nil {
			return err
		}
		rev, err := tx.Revisions([]RevisionScope{scope})
		if err != nil {
			return err
		}
		observed = rev[0]
		return tx.Put(RecordKey{"mail", "two"}, json.RawMessage(`{"to":"a"}`))
	})
	nativeMust(t, err)
	fresh := nativeRevisions(t, s, scope)[0]
	if fresh == observed {
		t.Fatalf("recipient partition kept one-record revision after second insert: %+v", fresh)
	}
}

func TestNativeEffectsFailureRollsBackTouchedScopes(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	child, err := store.Acquire(t.Context(), "child", AcquireOptions{Parent: "root"})
	nativeMust(t, err)
	defer child.Close()
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	scope := RevisionScope{CoordinationIndexMailRecipient, "a"}
	before := nativeRevisions(t, s, scope)[0]
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		if err := tx.Put(RecordKey{"mail", "one"}, json.RawMessage(`{"to":"a"}`)); err != nil {
			return err
		}
		return tx.Effects(CoordinationEffects{Members: []string{child.(ViewIdentity).ViewID()}, Pins: []string{"invalid-artifact-id"}})
	})
	if err == nil {
		t.Fatal("invalid pin accepted")
	}
	if nativeRevisions(t, s, scope)[0] != before {
		t.Fatal("failed effect changed revision")
	}
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		_, found, err := r.Get(RecordKey{"mail", "one"})
		if found {
			t.Fatal("failed effect persisted mail")
		}
		return err
	}))
	var count int
	nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM swarm_members`).Scan(&count))
	if count != 0 {
		t.Fatal("failed effect retained member")
	}
}

func TestNativeRepeatedMutationsRevisionStabilityAndDurability(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	nativePut(t, s, "mail", "untouched", `{"to":"unrelated"}`)
	scopes := []RevisionScope{
		{CoordinationIndexRecord, CoordinationIndexKey("mail", "one")},
		{CoordinationIndexMailRecipient, "a"},
		{CoordinationIndexMailRecipient, "b"},
		{CoordinationIndexMailRecipient, "unrelated"},
	}
	before := nativeRevisions(t, s, scopes...)
	var committed []Revision
	commit, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		revisions := func() []Revision {
			r, err := tx.Revisions(scopes)
			nativeMust(t, err)
			return r
		}
		var previous []Revision
		for i, payload := range []string{`{"to":"a"}`, `{"to":"b"}`, "", `{"to":"a"}`} {
			key := RecordKey{"mail", "one"}
			mutate := func() error {
				if payload == "" {
					return tx.Delete(key)
				}
				return tx.Put(key, json.RawMessage(payload))
			}
			nativeMust(t, mutate())
			current := revisions()
			if current[3] != before[3] {
				t.Fatal("unrelated partition invalidated")
			}
			if i > 0 && current[0] == previous[0] {
				t.Fatalf("record mutation %d reused revision", i)
			}
			if i == 1 && (current[1] == previous[1] || current[2] == previous[2]) {
				t.Fatal("move reused an old/new partition revision")
			}
			// Repeating each put/delete is an actual no-op, even after an earlier
			// mutation. It must not allocate another stamp or touch partitions.
			nativeMust(t, mutate())
			if !reflect.DeepEqual(current, revisions()) {
				t.Fatal("staged no-op changed revisions")
			}
			previous = current
		}
		committed = previous
		return nil
	})
	nativeMust(t, err)
	for _, scope := range scopes[:3] {
		found := false
		for _, changed := range commit.Changed {
			found = found || changed == scope
		}
		if !found {
			t.Fatalf("lost earlier changed scope %v", scope)
		}
	}
	check := func() {
		t.Helper()
		if got := nativeRevisions(t, s, scopes...); !reflect.DeepEqual(got, committed) {
			t.Fatalf("committed revisions changed: got %v want %v", got, committed)
		}
	}
	check()
	veto := errors.New("veto repeated mutation")
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		nativeMust(t, tx.Put(RecordKey{"mail", "one"}, json.RawMessage(`{"to":"b"}`)))
		nativeMust(t, tx.Delete(RecordKey{"mail", "one"}))
		return veto
	})
	if !errors.Is(err, veto) {
		t.Fatal(err)
	}
	check()
	path := filepath.Join(t.TempDir(), "promoted.db")
	nativeMust(t, store.Promote(t.Context(), path))
	check()
	nativeMust(t, s.Close())
	nativeMust(t, store.Close())
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer reopened.Close()
	s = nativeHandle(t, reopened, "root")
	check()
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		got, found, err := r.Get(RecordKey{"mail", "one"})
		if !found || string(got.Payload) != `{"to":"a"}` || got.Revision != committed[0] {
			t.Fatalf("reopened record changed: %+v", got)
		}
		return err
	}))
}

func TestRuntimeStorageScaleNativeRepeatedMutationWork(t *testing.T) {
	var baseline runtimemetrics.Snapshot
	for _, size := range []int{16, 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativeMust(t, s.UpdateCoordination(t.Context(), func(state *CoordinationState) error {
				state.Records["custom"] = map[string]json.RawMessage{"selected": json.RawMessage(`0`)}
				for i := range size {
					state.Records["custom"][fmt.Sprint(i)] = json.RawMessage(`{"unrelated":true}`)
				}
				return nil
			}))
			recorder := runtimemetrics.New()
			_, err := s.UpdateRecords(runtimemetrics.WithRecorder(t.Context(), recorder), func(tx CoordinationTx) error {
				for _, payload := range []string{"1", "2", "3"} {
					for range 2 { // change followed by no-op
						if err := tx.Put(RecordKey{"custom", "selected"}, json.RawMessage(payload)); err != nil {
							return err
						}
					}
				}
				return nil
			})
			nativeMust(t, err)
			counts := recorder.Snapshot()
			if counts.Count(runtimemetrics.CoordinationRecordsRead) != 6 || counts.Count(runtimemetrics.RecordsDecoded) != 3 || counts.Count(runtimemetrics.RecordsEncoded) != 0 {
				t.Fatalf("repeated mutation touched unrelated records: %+v", counts)
			}
			if size == 16 {
				baseline = counts
			} else {
				for _, counter := range []runtimemetrics.Counter{runtimemetrics.SQLStatements, runtimemetrics.RowsRead, runtimemetrics.PayloadBytesRead, runtimemetrics.RecordsDecoded} {
					if counts.Count(counter) != baseline.Count(counter) {
						t.Fatalf("repeated mutation work grew: counter=%d small=%d large=%d", counter, baseline.Count(counter), counts.Count(counter))
					}
				}
			}
			t.Logf("native dispatched SQL/returned rows (not VM visits): unrelated=%d statements=%d rows=%d payload_bytes=%d decoded=%d", size, counts.Count(runtimemetrics.SQLStatements), counts.Count(runtimemetrics.RowsRead), counts.Count(runtimemetrics.PayloadBytesRead), counts.Count(runtimemetrics.RecordsDecoded))
		})
	}
}

func TestNativePutOwnsInput(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	payload := json.RawMessage(`{"n":1}`)
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		if err := tx.Put(RecordKey{"custom", "owned"}, payload); err != nil {
			return err
		}
		payload[5] = '2'
		got, found, err := tx.Get(RecordKey{"custom", "owned"})
		if err != nil {
			return err
		}
		if !found || string(got.Payload) != `{"n":1}` {
			t.Fatalf("Put input alias: %s", got.Payload)
		}
		return nil
	})
	nativeMust(t, err)
}

func TestNativeReadCancellationAndVeto(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	err := s.ReadRecords(ctx, func(r CoordinationReader) error {
		calls++
		cancel()
		_, err := r.Identity()
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		return nil
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	veto := errors.New("veto")
	if err := s.ReadRecords(t.Context(), func(CoordinationReader) error { calls++; return veto }); !errors.Is(err, veto) || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
