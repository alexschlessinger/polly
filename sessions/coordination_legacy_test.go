package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// A deliberately independent, counted legacy authority. Embedding supplies
// unused artifact methods; the record adapter must never call or unwrap them.
type recordLegacyFixture struct {
	CoordinationSession
	state                     CoordinationState
	reads, updates, callbacks int
	retry                     bool
	failure                   error
}

func newRecordLegacyFixture() *recordLegacyFixture {
	return &recordLegacyFixture{state: CoordinationState{ActorID: "root", ParentID: "root", Records: map[string]map[string]json.RawMessage{"format": {"swarm": json.RawMessage(`{"version":2}`)}}}}
}
func (f *recordLegacyFixture) ReadCoordination(context.Context) (*CoordinationState, error) {
	f.reads++
	copy := f.state
	copy.Records = copyCoordinationRecords(f.state.Records)
	return &copy, nil
}
func (f *recordLegacyFixture) UpdateCoordination(_ context.Context, fn func(*CoordinationState) error) error {
	f.updates++
	copy := f.state
	copy.Records = copyCoordinationRecords(f.state.Records)
	copy.Append, copy.Pins, copy.Members = nil, nil, nil
	copy.Transcript, copy.ExpectedSequence = nil, nil
	f.callbacks++
	if err := fn(&copy); err != nil {
		return err
	}
	if f.retry {
		f.callbacks++
		if err := fn(&copy); err != nil {
			return err
		}
	}
	if f.failure != nil {
		return f.failure
	}
	copy.Sequence += int64(len(copy.Append))
	if copy.Transcript != nil {
		copy.Transcript.Sequence = copy.Sequence
	}
	f.state = copy
	return nil
}
func (f *recordLegacyFixture) ReadCoordinationView(ctx context.Context, _ string) (*CoordinationState, error) {
	return f.ReadCoordination(ctx)
}
func recordAccess(t *testing.T, f *recordLegacyFixture) CoordinationAccess {
	t.Helper()
	access, err := OpenCoordinationAccess(f)
	if err != nil {
		t.Fatal(err)
	}
	return access
}
func putRecord(t *testing.T, tx CoordinationTx, kind, id, payload string) {
	t.Helper()
	if err := tx.Put(RecordKey{kind, id}, json.RawMessage(payload)); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinationRecordsLegacyCallbackAtomicityAndOwnership(t *testing.T) {
	ctx := context.Background()
	f := newRecordLegacyFixture()
	access := recordAccess(t, f)
	if access.Capabilities() != (CoordinationCapabilities{}) {
		t.Fatal("legacy advertised fast/durable access")
	}
	veto := errors.New("veto")
	calls := 0
	commit, err := access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		calls++
		putRecord(t, tx, "mail", "m", `{"to":"a","delivered":false}`)
		if err := tx.Effects(CoordinationEffects{Append: messages.User("veto"), Pins: []string{"pin"}, Members: []string{"child"}}); err != nil {
			t.Fatal(err)
		}
		return veto
	})
	if !errors.Is(err, veto) || calls != 1 || f.updates != 1 || f.callbacks != 1 || commit.Sequence != 0 || len(commit.Changed) != 0 {
		t.Fatalf("veto/callback: %+v %v calls=%d backend=%+v", commit, err, calls, f)
	}
	if f.state.Records["mail"] != nil || f.state.Sequence != 0 || len(f.state.Pins) != 0 || len(f.state.Members) != 0 {
		t.Fatal("veto leaked effects")
	}
	payload := json.RawMessage(`{"to":"a","delivered":false}`)
	seq := int64(0)
	transcript := &TranscriptIdentity{Execution: "e", Generation: 3}
	metadata := map[string]any{"nested": map[string]any{"text": "owned"}}
	appendMessages := []messages.ChatMessage{{Role: "user", Content: "one", Metadata: metadata}}
	pins, members := []string{"pin"}, []string{"child"}
	var retained CoordinationTx
	commit, err = access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		retained = tx
		if err := tx.Put(RecordKey{"mail", "m"}, payload); err != nil {
			return err
		}
		payload[2] = 'X'
		record, found, err := tx.Get(RecordKey{"mail", "m"})
		if err != nil || !found || !strings.Contains(string(record.Payload), `"to"`) {
			t.Fatalf("put alias: %s %v", record.Payload, err)
		}
		record.Payload[2] = 'Y'
		record, _, err = tx.Get(RecordKey{"mail", "m"})
		if err != nil || !strings.Contains(string(record.Payload), `"to"`) {
			t.Fatal("get aliases storage")
		}
		page, err := tx.Query(RecordQuery{Index: CoordinationIndexMailPending, Key: "a", Limit: 5})
		if err != nil || len(page.Records) != 1 {
			t.Fatalf("read own writes: %+v %v", page, err)
		}
		page.Records[0].Payload[2] = 'Z'
		if err := tx.Effects(CoordinationEffects{Append: appendMessages, ExpectedSequence: &seq, Transcript: transcript, Pins: pins, PinOwner: "child", Members: members}); err != nil {
			return err
		}
		seq = 9
		transcript.Execution = "mutated"
		metadata["nested"].(map[string]any)["text"] = "mutated"
		appendMessages[0].Content = "mutated"
		pins[0] = "mutated"
		members[0] = "mutated"
		return tx.Effects(CoordinationEffects{Append: messages.User("two")})
	})
	if err != nil || commit.Sequence != 2 || f.reads != 0 || f.updates != 2 {
		t.Fatalf("commit=%+v err=%v reads=%d updates=%d", commit, err, f.reads, f.updates)
	}
	if f.state.Transcript.Execution != "e" || f.state.Transcript.Sequence != 2 || f.state.Transcript.Generation != 3 || *f.state.ExpectedSequence != 0 || f.state.PinOwner != "child" || f.state.Pins[0] != "pin" || f.state.Members[0] != "child" || f.state.Append[0].Content != "one" || f.state.Append[0].Metadata["nested"].(map[string]any)["text"] != "owned" {
		t.Fatalf("effects lost/aliased: %+v", f.state)
	}
	if _, err := retained.Identity(); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatalf("retained identity: %v", err)
	}
	if err := retained.Put(RecordKey{"unknown", "x"}, json.RawMessage(`{}`)); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if err := retained.Delete(RecordKey{"mail", "m"}); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if err := retained.Effects(CoordinationEffects{}); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	var reader CoordinationReader
	if err := access.ReadRecords(ctx, func(r CoordinationReader) error {
		reader = r
		if _, ok := r.(CoordinationTx); ok {
			t.Fatal("read callback exposed mutation methods")
		}
		identity, err := r.Identity()
		if err != nil || identity.Sequence != 2 || identity.ActorID != "root" {
			t.Fatalf("identity: %+v %v", identity, err)
		}
		record, _, err := r.Get(RecordKey{"mail", "m"})
		if err != nil {
			return err
		}
		record.Payload[0] = 'x'
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if f.reads != 1 || !json.Valid(f.state.Records["mail"]["m"]) {
		t.Fatal("read count or payload ownership")
	}
	if _, _, err := reader.Get(RecordKey{"mail", "m"}); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if _, err := reader.Query(RecordQuery{Limit: 1}); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	if _, err := reader.Revisions(nil); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
}

func TestCoordinationRecordsLegacyNoRetryOrAuthoritativeErrorCommit(t *testing.T) {
	ctx := context.Background()
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "retry"}[retry], func(t *testing.T) {
			f := newRecordLegacyFixture()
			f.retry = retry
			failure := errors.New("uncertain commit")
			if !retry {
				f.failure = failure
			}
			access := recordAccess(t, f)
			calls := 0
			commit, err := access.UpdateRecords(ctx, func(tx CoordinationTx) error {
				calls++
				putRecord(t, tx, "custom", "x", `{}`)
				return tx.Effects(CoordinationEffects{Append: messages.User("hello")})
			})
			if err == nil || calls != 1 || f.updates != 1 || !reflect.DeepEqual(commit, CoordinationCommit{}) {
				t.Fatalf("%+v %v calls=%d", commit, err, calls)
			}
			if !retry && !errors.Is(err, failure) {
				t.Fatalf("lost error identity: %v", err)
			}
		})
	}
}

func TestCoordinationRecordsLegacyRevisionsAndPartitions(t *testing.T) {
	ctx := context.Background()
	f := newRecordLegacyFixture()
	access := recordAccess(t, f)
	scope := RevisionScope{CoordinationIndexMailPending, "a"}
	var previous string
	for i := 0; i < 2; i++ {
		if err := access.ReadRecords(ctx, func(r CoordinationReader) error {
			a, err := r.Revisions([]RevisionScope{scope})
			if err != nil {
				return err
			}
			b, err := r.Revisions([]RevisionScope{scope})
			if err != nil {
				return err
			}
			if a[0] != b[0] || a[0].Token == previous || a[0].Token == "" {
				t.Fatal("revision stability/invalidation")
			}
			previous = a[0].Token
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		before, _ := tx.Revisions([]RevisionScope{scope})
		putRecord(t, tx, "mail", "m", `{"to":"a","delivered":false}`)
		after, _ := tx.Revisions([]RevisionScope{scope})
		if before[0].Token == after[0].Token {
			t.Fatal("staged write kept token")
		}
		record, _, _ := tx.Get(RecordKey{"mail", "m"})
		rev, _ := tx.Revisions([]RevisionScope{record.Revision.Scope})
		if rev[0] != record.Revision {
			t.Fatal("record revision differs")
		}
		putRecord(t, tx, "mail", "m", `{"to":"a","delivered":false}`)
		same, _ := tx.Revisions([]RevisionScope{scope})
		if same[0] != after[0] {
			t.Fatal("no-op invalidated callback revision")
		}
		return nil
	})
	if err != nil || len(commit.Changed) == 0 {
		t.Fatalf("%+v %v", commit, err)
	}
	commit, err = access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		putRecord(t, tx, "mail", "m", `{"to":"b","delivered":true}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []RevisionScope{scope, {CoordinationIndexMailRecipient, "a"}, {CoordinationIndexMailRecipient, "b"}, {CoordinationIndexRecord, CoordinationIndexKey("mail", "m")}} {
		if !slices.Contains(commit.Changed, want) {
			t.Fatalf("missing old/new partition %+v: %+v", want, commit)
		}
	}
	commit, err = access.UpdateRecords(ctx, func(tx CoordinationTx) error { return tx.Delete(RecordKey{"mail", "m"}) })
	if err != nil || !slices.Contains(commit.Changed, RevisionScope{CoordinationIndexMailRecipient, "b"}) {
		t.Fatalf("delete: %+v %v", commit, err)
	}
	commit, err = access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		return tx.Effects(CoordinationEffects{Append: messages.User("effects only")})
	})
	if err != nil || commit.Sequence != 1 || len(commit.Changed) != 0 {
		t.Fatalf("effects-only: %+v %v", commit, err)
	}
}

func TestCoordinationRecordsLegacyQueries(t *testing.T) {
	ctx := context.Background()
	f := newRecordLegacyFixture()
	access := recordAccess(t, f)
	_, err := access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		putRecord(t, tx, "mail", "z", `{"to":"a","posted":"2026-01-01T01:00:00+01:00"}`)
		putRecord(t, tx, "mail", "a", `{"to":"a","posted":"2026-01-01T00:00:00Z"}`)
		putRecord(t, tx, "mail", "b", `{"to":"a","posted":"2025-01-01T00:00:00Z"}`)
		putRecord(t, tx, "mail", "other", `{"to":"b"}`)
		for _, id := range []string{"w/10", "w/2", "w/0", "other/1"} {
			putRecord(t, tx, "workflow_step", id, `{}`)
		}
		for _, id := range []string{"z", "A", "a"} {
			putRecord(t, tx, "custom", id, `{}`)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	query := RecordQuery{Index: CoordinationIndexMailRecipient, Key: "a", Limit: 2, IncludeTotal: true}
	var cursor string
	if err := access.ReadRecords(ctx, func(r CoordinationReader) error {
		page, err := r.Query(query)
		if err != nil {
			return err
		}
		if page.Total != 3 || len(page.Records) != 2 || page.Records[0].Key.ID != "b" || page.Records[1].Key.ID != "a" || page.Next == "" {
			t.Fatalf("ordering: %+v", page)
		}
		cursor = page.Next
		for _, test := range []struct {
			q   RecordQuery
			ids []string
		}{
			{RecordQuery{Index: CoordinationIndexWorkflowSteps, Key: "w", Limit: 10}, []string{"w/0", "w/2", "w/10"}},
			{RecordQuery{Kind: "custom", Limit: 10}, []string{"A", "a", "z"}},
			{RecordQuery{Index: CoordinationIndexMailRecipient, Key: "a", Offset: 1, Limit: 10}, []string{"a", "z"}},
		} {
			page, err := r.Query(test.q)
			if err != nil {
				return err
			}
			var ids []string
			for _, record := range page.Records {
				ids = append(ids, record.Key.ID)
			}
			if !reflect.DeepEqual(ids, test.ids) || page.Total != -1 {
				t.Fatalf("query %+v got %+v", test.q, page)
			}
		}
		oversize, err := r.Query(RecordQuery{Index: query.Index, Key: query.Key, Limit: 10, MaxBytes: 1})
		if err != nil {
			return err
		}
		if !oversize.Oversized || len(oversize.Records) != 1 || oversize.Next == "" || len(oversize.Records[0].Payload) <= 1 {
			t.Fatalf("oversize %+v", oversize)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	query.After = cursor
	if err := access.ReadRecords(ctx, func(r CoordinationReader) error {
		page, err := r.Query(query)
		if err != nil {
			return err
		}
		if len(page.Records) != 1 || page.Records[0].Key.ID != "z" || page.Next != "" || page.Total != 3 {
			t.Fatalf("next %+v", page)
		}
		query.Key = "b"
		_, err = r.Query(query)
		if !errors.Is(err, ErrCoordinationCursor) {
			t.Fatalf("selector cursor: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinationRecordsLegacyValidationAndCancellation(t *testing.T) {
	ctx := context.Background()
	f := newRecordLegacyFixture()
	access := recordAccess(t, f)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := access.ReadRecords(canceled, func(CoordinationReader) error { t.Fatal("canceled callback"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := access.UpdateRecords(canceled, func(CoordinationTx) error { t.Fatal("canceled callback"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.reads != 0 || f.updates != 0 {
		t.Fatal("canceled request reached backend")
	}
	if err := access.ReadRecords(ctx, nil); !errors.Is(err, ErrCoordinationRequest) {
		t.Fatal(err)
	}
	if _, err := access.UpdateRecords(ctx, nil); !errors.Is(err, ErrCoordinationRequest) {
		t.Fatal(err)
	}
	if err := access.ReadRecords(ctx, func(r CoordinationReader) error {
		for _, query := range []RecordQuery{{Limit: 0}, {Limit: MaxCoordinationPageLimit + 1}, {Limit: 1, Offset: -1}, {Limit: 1, MaxBytes: -1}, {Limit: 1, After: "!"}} {
			if _, err := r.Query(query); err == nil {
				t.Fatalf("accepted %+v", query)
			}
		}
		for _, scope := range []RevisionScope{{CoordinationIndex("unknown"), ""}, {CoordinationIndex("mail.recipient WHERE 1=1"), "a"}, {CoordinationIndexPublicationAudience, "run:a"}, {CoordinationIndexMemberNamePrefix, "relative"}, {CoordinationIndexRecords, "unexpected"}} {
			if _, err := r.Query(RecordQuery{Index: scope.Index, Key: scope.Key, Limit: 1}); !errors.Is(err, ErrCoordinationSelector) {
				t.Fatalf("query selector %+v: %v", scope, err)
			}
			if _, err := r.Revisions([]RevisionScope{scope}); !errors.Is(err, ErrCoordinationSelector) {
				t.Fatalf("revision selector %+v: %v", scope, err)
			}
		}
		if _, err := r.Revisions(make([]RevisionScope, MaxCoordinationRevisionScopes+1)); !errors.Is(err, ErrCoordinationRequest) {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.state.Records["format"]["swarm"] = json.RawMessage(`{"version":3}`)
	f.state.Records["member"] = map[string]json.RawMessage{"a": json.RawMessage(`{}`)}
	f.state.Records["custom"] = map[string]json.RawMessage{"a": json.RawMessage(`null`)}
	if err := access.ReadRecords(ctx, func(r CoordinationReader) error {
		if _, _, err := r.Get(RecordKey{"member", "a"}); !errors.Is(err, ErrCoordinationFormat) {
			t.Fatal(err)
		}
		if _, err := r.Query(RecordQuery{Kind: "member", Limit: 1}); !errors.Is(err, ErrCoordinationFormat) {
			t.Fatal(err)
		}
		if _, err := r.Revisions([]RevisionScope{{CoordinationIndexKind, "member"}}); !errors.Is(err, ErrCoordinationFormat) {
			t.Fatal(err)
		}
		if _, ok, err := r.Get(RecordKey{"custom", "a"}); err != nil || !ok {
			t.Fatalf("unknown roundtrip: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	active, stop := context.WithCancel(ctx)
	if _, err := access.UpdateRecords(active, func(tx CoordinationTx) error {
		putRecord(t, tx, "custom", "cancel", `{}`)
		stop()
		if _, err := tx.Identity(); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.state.Records["custom"]["cancel"] != nil {
		t.Fatal("canceled write committed")
	}
}

func TestCoordinationRecordsLegacyEffectConflicts(t *testing.T) {
	f := newRecordLegacyFixture()
	access := recordAccess(t, f)
	ctx := context.Background()
	zero, one := int64(0), int64(1)
	_, err := access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		if err := tx.Effects(CoordinationEffects{ExpectedSequence: &zero, PinOwner: "a", Transcript: &TranscriptIdentity{Execution: "e", Generation: 1}}); err != nil {
			return err
		}
		for _, effect := range []CoordinationEffects{{ExpectedSequence: &one}, {PinOwner: "b"}, {Transcript: &TranscriptIdentity{Execution: "other", Generation: 1}}, {Transcript: &TranscriptIdentity{}}} {
			if err := tx.Effects(effect); !errors.Is(err, ErrCoordinationRequest) {
				t.Fatalf("accepted conflicting/invalid effects: %+v %v", effect, err)
			}
		}
		return tx.Effects(CoordinationEffects{Append: messages.User("ok")})
	})
	if err != nil || f.state.Sequence != 1 {
		t.Fatalf("effects: %+v %v", f.state, err)
	}
	_, err = access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		return tx.Effects(CoordinationEffects{ExpectedSequence: &zero, Append: messages.User("stale")})
	})
	if !errors.Is(err, ErrCoordinationRequest) || f.state.Sequence != 1 {
		t.Fatalf("stale sequence: %v", err)
	}
}

func TestCoordinationRecordsLegacySQLiteAtomicEffects(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(StoreConfig{Mode: ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "record-parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	child, err := store.Acquire(ctx, "record-child", AcquireOptions{Parent: "record-parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	childID := child.(ViewIdentity).ViewID()
	parentID := parent.(ViewIdentity).ViewID()
	ref, err := child.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	// Select the legacy adapter deliberately even when SQLite offers native records.
	access, err := OpenCoordinationAccess(struct{ CoordinationSession }{parent.(CoordinationSession)})
	if err != nil {
		t.Fatal(err)
	}
	if access.Capabilities() != (CoordinationCapabilities{}) {
		t.Fatal("SQLite prematurely advertised native records")
	}
	veto := errors.New("veto")
	zero := int64(0)
	apply := func(tx CoordinationTx) error {
		putRecord(t, tx, "format", "swarm", `{"version":2}`)
		putRecord(t, tx, "mail", "m", `{"to":"a","delivered":true}`)
		return tx.Effects(CoordinationEffects{Append: messages.User("checkpoint"), ExpectedSequence: &zero, Transcript: &TranscriptIdentity{Execution: "e", Generation: 2}, Pins: []string{ref.ID}, PinOwner: childID, Members: []string{childID}})
	}
	if _, err := access.UpdateRecords(ctx, func(tx CoordinationTx) error {
		if err := apply(tx); err != nil {
			return err
		}
		return veto
	}); !errors.Is(err, veto) {
		t.Fatal(err)
	}
	for _, table := range []string{"swarm_records", "swarm_artifacts", "swarm_members"} {
		var n int
		if err := store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("veto leaked %s: %d %v", table, n, err)
		}
	}
	commit, err := access.UpdateRecords(ctx, apply)
	if err != nil || commit.Sequence != 1 {
		t.Fatalf("commit %+v %v", commit, err)
	}
	view, err := store.ReadView(ctx, ViewTarget{ID: parentID}, "")
	if err != nil || view.Sequence != 1 || len(view.History) != 1 || view.Metadata.Transcript == nil || view.Metadata.Transcript.Execution != "e" || view.Metadata.Transcript.Generation != 2 || view.Metadata.Transcript.Sequence != 1 {
		t.Fatalf("view %+v %v", view, err)
	}
	for _, table := range []string{"swarm_artifacts", "swarm_members"} {
		var n int
		if err := store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("lost %s: %d %v", table, n, err)
		}
	}
	reader, err := OpenCoordinationViewAccess(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ReadRecordsView(ctx, parentID, func(r CoordinationReader) error {
		record, ok, err := r.Get(RecordKey{"mail", "m"})
		if err != nil || !ok || !strings.Contains(string(record.Payload), "true") {
			t.Fatalf("view record %+v %v", record, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reader.ReadRecordsView(ctx, childID, func(CoordinationReader) error { t.Fatal("child view callback"); return nil }); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
}
