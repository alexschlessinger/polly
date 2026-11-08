package sessions

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestCoordinationWholeStateCommitReceipt(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	root := nativeHandle(t, store, "root")
	nativePut(t, root, "format", "swarm", `{"version":2}`)
	scopes := []RevisionScope{
		{Index: CoordinationIndexRecords},
		{Index: CoordinationIndexRecord, Key: CoordinationIndexKey("task", "selected")},
		{Index: CoordinationIndexSemanticNews},
		{Index: CoordinationIndexDisplay},
	}
	before := nativeRevisions(t, root, scopes...)
	calls := 0
	update := func(raw *CoordinationState) error {
		calls++
		if raw.Records["task"] == nil {
			raw.Records["task"] = map[string]json.RawMessage{}
		}
		raw.Records["task"]["selected"] = json.RawMessage(`{"id":"selected","status":"blocked"}`)
		raw.Append = messages.User("checkpoint")
		return nil
	}
	veto := errors.New("veto")
	commit, err := root.UpdateCoordinationCommit(t.Context(), func(raw *CoordinationState) error {
		_ = update(raw)
		return veto
	})
	if !errors.Is(err, veto) || !reflect.DeepEqual(commit, CoordinationCommit{}) || calls != 1 {
		t.Fatalf("failed callback returned receipt: %+v %v calls=%d", commit, err, calls)
	}
	if after := nativeRevisions(t, root, scopes...); !reflect.DeepEqual(before, after) {
		t.Fatal("rollback advanced revision scopes")
	}
	commit, err = root.UpdateCoordinationCommit(t.Context(), update)
	if err != nil || commit.Sequence != 1 || calls != 2 {
		t.Fatalf("commit %+v %v calls=%d", commit, err, calls)
	}
	for _, scope := range scopes {
		if !slices.Contains(commit.Changed, scope) {
			t.Fatalf("receipt omitted scope %+v: %+v", scope, commit)
		}
	}
	before = nativeRevisions(t, root, scopes...)
	commit, err = root.UpdateCoordinationCommit(t.Context(), func(*CoordinationState) error { return nil })
	if err != nil || commit.Sequence != 1 || len(commit.Changed) != 0 {
		t.Fatalf("no-op receipt %+v %v", commit, err)
	}
	if after := nativeRevisions(t, root, scopes...); !reflect.DeepEqual(before, after) {
		t.Fatal("no-op advanced revision scopes")
	}
	commit, err = root.UpdateCoordinationCommit(t.Context(), func(raw *CoordinationState) error {
		raw.Append = messages.User("effects only")
		return nil
	})
	if err != nil || commit.Sequence != 2 || len(commit.Changed) != 0 {
		t.Fatalf("effects-only receipt %+v %v", commit, err)
	}
	// Effects validation occurs after record staging; no scopes escape when
	// the append sequence fence rolls the entire transaction back.
	commit, err = root.UpdateCoordinationCommit(t.Context(), func(raw *CoordinationState) error {
		raw.Records["task"]["selected"] = json.RawMessage(`{"id":"selected","status":"done"}`)
		wrong := int64(0)
		raw.ExpectedSequence = &wrong
		raw.Append = messages.User("must not append")
		return nil
	})
	if err == nil || !reflect.DeepEqual(commit, CoordinationCommit{}) {
		t.Fatalf("failed effects returned receipt %+v %v", commit, err)
	}
	if after := nativeRevisions(t, root, scopes...); !reflect.DeepEqual(before, after) {
		t.Fatal("failed effects advanced revision scopes")
	}
	if _, err := root.UpdateCoordinationCommit(t.Context(), nil); err == nil {
		t.Fatal("nil update accepted")
	}
}
