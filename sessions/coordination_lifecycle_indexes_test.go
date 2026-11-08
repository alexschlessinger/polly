package sessions

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func lifecycleAccess(t *testing.T, native bool) CoordinationAccess {
	t.Helper()
	if !native {
		return recordAccess(t, newRecordLegacyFixture())
	}
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	access, err := OpenCoordinationAccess(nativeHandle(t, store, "root"))
	nativeMust(t, err)
	_, err = access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		return tx.Put(RecordKey{"format", "swarm"}, json.RawMessage(`{"version":2}`))
	})
	nativeMust(t, err)
	return access
}

func lifecycleKeys(page RecordPage) []string {
	var ids []string
	for _, record := range page.Records {
		ids = append(ids, record.Key.ID)
	}
	return ids
}

func TestLifecycleSelectorsNativeLegacyParity(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			records := map[RecordKey]string{
				{"execution", "a"}:           `{"run":"r/\u0000☃","request":{"callID":"c:'[]"}}`,
				{"execution", "z"}:           `{"run":"r/\u0000☃","request":{"callID":"c:'[]"}}`,
				{"execution", "\xff"}:        `{"run":"r/\u0000☃","request":{"callID":"c:'[]"}}`,
				{"execution", "split-a"}:     `{"run":"a","request":{"callID":"bc"}}`,
				{"execution", "split-b"}:     `{"run":"ab","request":{"callID":"c"}}`,
				{"execution", "empty"}:       `{}`,
				{"mail", "z"}:                `{"from":"sender/\u0000☃","callID":"c:'[]","to":"recipient' OR 1=1 --","start":true,"posted":"2026-01-01T01:00:00+01:00"}`,
				{"mail", "a"}:                `{"from":"sender/\u0000☃","callID":"c:'[]","to":"recipient' OR 1=1 --","start":true,"posted":"2026-01-01T00:00:00Z"}`,
				{"mail", "b"}:                `{"from":"sender/\u0000☃","callID":"c:'[]","to":"recipient' OR 1=1 --","start":true,"posted":"2025-01-01T00:00:00Z"}`,
				{"mail", "ordinary"}:         `{"from":"other","to":"recipient' OR 1=1 --"}`,
				{"mail", "delivered"}:        `{"from":"other","to":"recipient' OR 1=1 --","start":true,"delivered":true}`,
				{"mail", "elsewhere"}:        `{"to":"elsewhere","start":true}`,
				{"mail", "split-a"}:          `{"from":"a","callID":"bc"}`,
				{"mail", "split-b"}:          `{"from":"ab","callID":"c"}`,
				{"member", "z"}:              `{"agentName":"/root/Team"}`,
				{"member", "a"}:              `{"agentName":"/root/Team"}`,
				{"member", "child"}:          `{"agentName":"/root/Team/child"}`,
				{"member", "lower"}:          `{"agentName":"/root/team"}`,
				{"member", "slash"}:          `{"agentName":"/root/Team/"}`,
				{"member", "fallback"}:       `{"id":"opaque/\u0000☃"}`,
				{"task", "a"}:                `{"follows":"t/\u0000☃","followupCallID":"c:'[]"}`,
				{"task", "z"}:                `{"follows":"t/\u0000☃","followupCallID":"c:'[]"}`,
				{"task", "split-a"}:          `{"follows":"a","followupCallID":"bc"}`,
				{"task", "split-b"}:          `{"follows":"ab","followupCallID":"c"}`,
				{"snapshot", "a"}:            `{"source":"/source//☃\u0000/../a"}`,
				{"snapshot", "z"}:            `{"source":"/source//☃\u0000/../a"}`,
				{"snapshot", "cleaned"}:      `{"source":"/source/a"}`,
				{"workflow_step", "w/10"}:    `{"id":"stable/☃\u0000"}`,
				{"workflow_step", "w/2"}:     `{"id":"stable/☃\u0000"}`,
				{"workflow_step", "w/1"}:     `{"id":"other"}`,
				{"workflow_step", "other/2"}: `{"id":"stable/☃\u0000"}`,
				{"workflow_step", "a/0"}:     `{"id":"bc"}`,
				{"workflow_step", "ab/0"}:    `{"id":"c"}`,
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
			queries := []struct {
				index CoordinationIndex
				key   string
				ids   []string
			}{
				{CoordinationIndexExecutionRunCall, CoordinationIndexKey("r/\x00☃", "c:'[]"), []string{"a", "z", "\xff"}},
				{CoordinationIndexExecutionRunCall, CoordinationIndexKey("a", "bc"), []string{"split-a"}},
				{CoordinationIndexExecutionRunCall, CoordinationIndexKey("ab", "c"), []string{"split-b"}},
				{CoordinationIndexExecutionRunCall, CoordinationIndexKey("", ""), []string{"empty"}},
				{CoordinationIndexMailFromCall, CoordinationIndexKey("sender/\x00☃", "c:'[]"), []string{"b", "a", "z"}},
				{CoordinationIndexMailFromCall, CoordinationIndexKey("a", "bc"), []string{"split-a"}},
				{CoordinationIndexMailFromCall, CoordinationIndexKey("ab", "c"), []string{"split-b"}},
				{CoordinationIndexMailPendingStartRecipient, "recipient' OR 1=1 --", []string{"b", "a", "z"}},
				{CoordinationIndexMemberName, "/root/Team", []string{"a", "z"}},
				{CoordinationIndexMemberName, "/root/Team/", []string{"slash"}},
				{CoordinationIndexMemberName, "/root/team", []string{"lower"}},
				{CoordinationIndexMemberName, "/root/agent_opaque/\x00☃", []string{"fallback"}},
				{CoordinationIndexMemberName, "/root", nil},
				{CoordinationIndexTaskFollowupCall, CoordinationIndexKey("t/\x00☃", "c:'[]"), []string{"a", "z"}},
				{CoordinationIndexTaskFollowupCall, CoordinationIndexKey("a", "bc"), []string{"split-a"}},
				{CoordinationIndexTaskFollowupCall, CoordinationIndexKey("ab", "c"), []string{"split-b"}},
				{CoordinationIndexSnapshotSource, "/source//☃\x00/../a", []string{"a", "z"}},
				{CoordinationIndexSnapshotSource, "/source/a", []string{"cleaned"}},
				{CoordinationIndexExecutionCallID, "c:'[]", []string{"a", "z", "\xff"}},
				{CoordinationIndexExecutionCallID, "", []string{"empty"}},
				{CoordinationIndexWorkflowStepID, CoordinationIndexKey("w", "stable/☃\x00"), []string{"w/2", "w/10"}},
				{CoordinationIndexWorkflowStepID, CoordinationIndexKey("other", "stable/☃\x00"), []string{"other/2"}},
				{CoordinationIndexWorkflowStepID, CoordinationIndexKey("a", "bc"), []string{"a/0"}},
				{CoordinationIndexWorkflowStepID, CoordinationIndexKey("ab", "c"), []string{"ab/0"}},
			}
			for _, test := range queries {
				nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
					query := RecordQuery{Index: test.index, Key: test.key, Limit: 1, IncludeTotal: true}
					var ids []string
					for pages := 0; ; pages++ {
						if pages > len(test.ids) {
							t.Fatal("cursor failed to advance", test.index, test.key)
						}
						page, err := r.Query(query)
						if err != nil {
							return err
						}
						if page.Total != len(test.ids) {
							t.Fatalf("%s %q total %d want %d", test.index, test.key, page.Total, len(test.ids))
						}
						ids = append(ids, lifecycleKeys(page)...)
						if page.Next == "" {
							break
						}
						wrong := query
						wrong.After = page.Next
						if test.index == CoordinationIndexExecutionRunCall || test.index == CoordinationIndexMailFromCall || test.index == CoordinationIndexTaskFollowupCall || test.index == CoordinationIndexWorkflowStepID {
							wrong.Key = CoordinationIndexKey("wrong", "partition")
						} else {
							wrong.Key += "/wrong"
						}
						if _, err := r.Query(wrong); !errors.Is(err, ErrCoordinationCursor) {
							t.Fatalf("cursor not bound to partition: %v", err)
						}
						wrong = query
						wrong.After, wrong.Kind = page.Next, page.Records[0].Key.Kind
						if _, err := r.Query(wrong); !errors.Is(err, ErrCoordinationCursor) {
							t.Fatalf("cursor not bound to kind: %v", err)
						}
						query.After = page.Next
					}
					if !slices.Equal(ids, test.ids) {
						t.Fatalf("%s %q IDs %q want %q", test.index, test.key, ids, test.ids)
					}
					query.After, query.Kind = "", "custom"
					if _, err := r.Query(query); !errors.Is(err, ErrCoordinationSelector) {
						t.Fatalf("accepted conflicting kind: %v", err)
					}
					return nil
				}))
			}
		})
	}
}

func TestLifecycleSelectorPartitionMovesAndNoOps(t *testing.T) {
	cases := []struct {
		kind, before, after string
		index               CoordinationIndex
		oldKey, newKey      string
	}{
		{"execution", `{"run":"a","request":{"callID":"bc"}}`, `{"run":"ab","request":{"callID":"c"}}`, CoordinationIndexExecutionRunCall, CoordinationIndexKey("a", "bc"), CoordinationIndexKey("ab", "c")},
		{"mail", `{"from":"a","callID":"bc"}`, `{"from":"ab","callID":"c"}`, CoordinationIndexMailFromCall, CoordinationIndexKey("a", "bc"), CoordinationIndexKey("ab", "c")},
		{"mail", `{"to":"a","start":true}`, `{"to":"b","start":true}`, CoordinationIndexMailPendingStartRecipient, "a", "b"},
		{"member", `{"agentName":"/root/a"}`, `{"agentName":"/root/b"}`, CoordinationIndexMemberName, "/root/a", "/root/b"},
		{"task", `{"follows":"a","followupCallID":"bc"}`, `{"follows":"ab","followupCallID":"c"}`, CoordinationIndexTaskFollowupCall, CoordinationIndexKey("a", "bc"), CoordinationIndexKey("ab", "c")},
		{"snapshot", `{"source":"/a/../b"}`, `{"source":"/b"}`, CoordinationIndexSnapshotSource, "/a/../b", "/b"},
		{"execution", `{"request":{"callID":"old"}}`, `{"request":{"callID":"new"}}`, CoordinationIndexExecutionCallID, "old", "new"},
		{"workflow_step", `{"id":"old"}`, `{"id":"new"}`, CoordinationIndexWorkflowStepID, CoordinationIndexKey("w", "old"), CoordinationIndexKey("w", "new")},
	}
	for _, native := range []bool{false, true} {
		for _, test := range cases {
			t.Run(map[bool]string{false: "legacy/", true: "native/"}[native]+string(test.index), func(t *testing.T) {
				access := lifecycleAccess(t, native)
				key := RecordKey{test.kind, "id"}
				if test.kind == "workflow_step" {
					key.ID = "w/2"
				}
				put := func(payload string) CoordinationCommit {
					commit, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
						return tx.Put(key, json.RawMessage(payload))
					})
					nativeMust(t, err)
					return commit
				}
				scopes := []RevisionScope{{test.index, test.oldKey}, {test.index, test.newKey}}
				put(test.before)
				commit, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
					before, err := tx.Revisions(scopes)
					if err != nil {
						return err
					}
					if err := tx.Put(key, json.RawMessage(test.before)); err != nil {
						return err
					}
					after, err := tx.Revisions(scopes)
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("no-op invalidated scopes: %v -> %v", before, after)
					}
					return err
				})
				nativeMust(t, err)
				if len(commit.Changed) != 0 {
					t.Fatal("no-op changed", commit.Changed)
				}
				commit = put(test.after)
				for _, scope := range scopes {
					if !slices.Contains(commit.Changed, scope) {
						t.Fatalf("move lost scope %+v in %+v", scope, commit.Changed)
					}
				}
				nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
					for i, scope := range scopes {
						page, err := r.Query(RecordQuery{Index: scope.Index, Key: scope.Key, Limit: 2, IncludeTotal: true})
						if err != nil {
							return err
						}
						if page.Total != i || len(page.Records) != i {
							t.Fatalf("move partition %+v: %+v", scope, page)
						}
					}
					return nil
				}))
				commit, err = access.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return tx.Delete(key) })
				nativeMust(t, err)
				if !slices.Contains(commit.Changed, scopes[1]) || slices.Contains(commit.Changed, scopes[0]) {
					t.Fatalf("delete invalidation %+v", commit.Changed)
				}
			})
		}
	}
}

func TestLifecycleSelectorsValidationAndNarrowKind(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "native"}[native], func(t *testing.T) {
			access := lifecycleAccess(t, native)
			for _, test := range []struct{ kind, payload string }{
				{"execution", `{"request":[]}`}, {"execution", `{"request":{"callID":1}}`},
				{"mail", `{"start":"true"}`}, {"mail", `{"callID":[]}`}, {"member", `{"agentName":[]}`},
				{"task", `{"followupCallID":{}}`}, {"snapshot", `{"source":[]}`},
			} {
				commit, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
					return tx.Put(RecordKey{test.kind, "invalid"}, json.RawMessage(test.payload))
				})
				if !errors.Is(err, ErrCoordinationFormat) || len(commit.Changed) != 0 {
					t.Fatalf("malformed %s %s: %+v %v", test.kind, test.payload, commit, err)
				}
			}
			nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
				for _, index := range []CoordinationIndex{CoordinationIndexExecutionRunCall, CoordinationIndexMailFromCall, CoordinationIndexTaskFollowupCall, CoordinationIndexWorkflowStepID} {
					for _, key := range []string{"!", "", CoordinationIndexKey("one"), CoordinationIndexKey("a", "b", "c")} {
						if _, err := r.Query(RecordQuery{Index: index, Key: key, Limit: 1}); !errors.Is(err, ErrCoordinationSelector) {
							t.Fatalf("accepted query %s %q: %v", index, key, err)
						}
						if _, err := r.Revisions([]RevisionScope{{index, key}}); !errors.Is(err, ErrCoordinationSelector) {
							t.Fatalf("accepted scope %s %q: %v", index, key, err)
						}
					}
				}
				return nil
			}))
		})
	}
	// The slow adapter may scan the selected kind, but must not turn an exact
	// lifecycle selector into an unrestricted family scan/validation fallback.
	fixture := newRecordLegacyFixture()
	fixture.state.Records["context"] = map[string]json.RawMessage{"malformed": json.RawMessage(`[]`)}
	access := recordAccess(t, fixture)
	nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
		for _, scope := range []RevisionScope{
			{CoordinationIndexExecutionRunCall, CoordinationIndexKey("r", "c")},
			{CoordinationIndexMailFromCall, CoordinationIndexKey("m", "c")},
			{CoordinationIndexMailPendingStartRecipient, "m"},
			{CoordinationIndexMemberName, "/root/a"},
			{CoordinationIndexTaskFollowupCall, CoordinationIndexKey("t", "c")},
			{CoordinationIndexSnapshotSource, "/source"},
			{CoordinationIndexExecutionCallID, "c"},
			{CoordinationIndexWorkflowStepID, CoordinationIndexKey("w", "s")},
		} {
			page, err := r.Query(RecordQuery{Index: scope.Index, Key: scope.Key, Limit: 1})
			if err != nil || len(page.Records) != 0 {
				t.Fatalf("unrelated malformed kind leaked into %+v: %+v %v", scope, page, err)
			}
		}
		return nil
	}))
}
