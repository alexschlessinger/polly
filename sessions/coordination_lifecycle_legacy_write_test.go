package sessions

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLifecycleSelectorsNativeLegacyWrites(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	records := []struct {
		key           RecordKey
		before, after string
	}{
		{RecordKey{"execution", "e"}, `{"run":"a","request":{"callID":"c1"}}`, `{"run":"b","request":{"callID":"c2"}}`},
		{RecordKey{"mail", "m"}, `{"from":"a","callID":"c1","to":"a","start":true}`, `{"from":"b","callID":"c2","to":"b","start":true}`},
		{RecordKey{"member", "m"}, `{"agentName":"/root/a"}`, `{"agentName":"/root/b"}`},
		{RecordKey{"task", "t"}, `{"follows":"a","followupCallID":"c1"}`, `{"follows":"b","followupCallID":"c2"}`},
		{RecordKey{"snapshot", "s"}, `{"source":"/a"}`, `{"source":"/b"}`},
		{RecordKey{"workflow_step", "w/2"}, `{"id":"a"}`, `{"id":"b"}`},
	}
	pairs := [][2]RevisionScope{
		{{CoordinationIndexExecutionRunCall, CoordinationIndexKey("a", "c1")}, {CoordinationIndexExecutionRunCall, CoordinationIndexKey("b", "c2")}},
		{{CoordinationIndexExecutionCallID, "c1"}, {CoordinationIndexExecutionCallID, "c2"}},
		{{CoordinationIndexMailFromCall, CoordinationIndexKey("a", "c1")}, {CoordinationIndexMailFromCall, CoordinationIndexKey("b", "c2")}},
		{{CoordinationIndexMailPendingStartRecipient, "a"}, {CoordinationIndexMailPendingStartRecipient, "b"}},
		{{CoordinationIndexMemberName, "/root/a"}, {CoordinationIndexMemberName, "/root/b"}},
		{{CoordinationIndexTaskFollowupCall, CoordinationIndexKey("a", "c1")}, {CoordinationIndexTaskFollowupCall, CoordinationIndexKey("b", "c2")}},
		{{CoordinationIndexSnapshotSource, "/a"}, {CoordinationIndexSnapshotSource, "/b"}},
		{{CoordinationIndexWorkflowStepID, CoordinationIndexKey("w", "a")}, {CoordinationIndexWorkflowStepID, CoordinationIndexKey("w", "b")}},
	}
	var scopes []RevisionScope
	for _, pair := range pairs {
		scopes = append(scopes, pair[:]...)
	}
	write := func(after bool) {
		t.Helper()
		nativeMust(t, s.UpdateCoordination(t.Context(), func(state *CoordinationState) error {
			if state.Records == nil {
				state.Records = map[string]map[string]json.RawMessage{}
			}
			state.Records["format"] = map[string]json.RawMessage{"swarm": json.RawMessage(`{"version":2}`)}
			for _, record := range records {
				payload := record.before
				if after {
					payload = record.after
				}
				state.Records[record.key.Kind] = map[string]json.RawMessage{record.key.ID: json.RawMessage(payload)}
			}
			return nil
		}))
	}
	write(false)
	before := nativeRevisions(t, s, scopes...)
	write(false)
	if got := nativeRevisions(t, s, scopes...); !reflect.DeepEqual(got, before) {
		t.Fatalf("legacy no-op invalidated native revisions: %+v -> %+v", before, got)
	}
	write(true)
	after := nativeRevisions(t, s, scopes...)
	for i := range scopes {
		if before[i] == after[i] {
			t.Fatalf("legacy move did not invalidate native scope %+v", scopes[i])
		}
	}
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		for _, pair := range pairs {
			for i, scope := range pair {
				page, err := r.Query(RecordQuery{Index: scope.Index, Key: scope.Key, IncludeTotal: true, CountOnly: true})
				if err != nil {
					return err
				}
				if page.Total != i {
					t.Fatalf("legacy partition move %+v count=%d want=%d", scope, page.Total, i)
				}
			}
		}
		return nil
	}))
}
