package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func notificationFact(kind, id string) RevisionScope {
	return RevisionScope{CoordinationIndexDisplayFact, CoordinationIndexKey(kind, id)}
}

func notificationScopes() []RevisionScope {
	return []RevisionScope{
		{CoordinationIndexSemanticNews, ""}, {CoordinationIndexDisplay, ""},
		notificationFact("member", "m"), notificationFact("member", "other"),
		notificationFact("workflow", "w"), notificationFact("status", "parent"), notificationFact("launch", "e"),
		{CoordinationIndexSemanticMail, "a"}, {CoordinationIndexSemanticMail, "b"},
	}
}

func notificationWrite(t *testing.T, s *sqliteSession, legacy bool, key RecordKey, payload string) CoordinationCommit {
	t.Helper()
	if legacy {
		nativeMust(t, s.UpdateCoordination(t.Context(), func(state *CoordinationState) error {
			if payload == "" {
				delete(state.Records[key.Kind], key.ID)
			} else {
				if state.Records[key.Kind] == nil {
					state.Records[key.Kind] = map[string]json.RawMessage{}
				}
				state.Records[key.Kind][key.ID] = json.RawMessage(payload)
			}
			return nil
		}))
		return CoordinationCommit{}
	}
	commit, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		if payload == "" {
			return tx.Delete(key)
		}
		return tx.Put(key, json.RawMessage(payload))
	})
	nativeMust(t, err)
	return commit
}

func TestNotificationRevisionScopesAndReadLifetime(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	scopes := notificationScopes()
	initial := nativeRevisions(t, s, scopes...)
	if !reflect.DeepEqual(initial, nativeRevisions(t, s, scopes...)) {
		t.Fatal("empty scope tokens changed")
	}
	other := nativeHandle(t, store, "other")
	for i, rev := range nativeRevisions(t, other, scopes...) {
		if rev == initial[i] {
			t.Fatal("foreign family token alias")
		}
	}
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	for _, legacy := range []bool{false, true} {
		var access CoordinationRecords = s
		if legacy {
			a, err := OpenCoordinationAccess(struct{ CoordinationSession }{s})
			nativeMust(t, err)
			access = a
		}
		var escaped CoordinationReader
		nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
			escaped = r
			if _, writable := r.(CoordinationTx); writable {
				t.Fatal("read-only callback exposes mutations")
			}
			if _, err := r.Revisions(scopes); err != nil {
				return err
			}
			for _, scope := range scopes {
				for _, q := range []RecordQuery{{Index: scope.Index, Key: scope.Key, Limit: 1}, {Index: scope.Index, Key: scope.Key, CountOnly: true, IncludeTotal: true}} {
					if _, err := r.Query(q); !errors.Is(err, ErrCoordinationSelector) {
						t.Fatalf("legacy=%t accepted revision-only Query %+v: %v", legacy, q, err)
					}
				}
			}
			for _, bad := range []RevisionScope{{CoordinationIndexSemanticNews, "key"}, {CoordinationIndexDisplay, "key"}, {CoordinationIndexDisplayFact, "!"}, {CoordinationIndexDisplayFact, CoordinationIndexKey("m")}} {
				if _, err := r.Revisions([]RevisionScope{bad}); !errors.Is(err, ErrCoordinationSelector) {
					t.Fatalf("bad scope %+v: %v", bad, err)
				}
			}
			return nil
		}))
		if _, err := escaped.Revisions(scopes); !errors.Is(err, ErrCoordinationClosed) {
			t.Fatalf("escaped revisions: %v", err)
		}
	}
	before := nativeRevisions(t, s, scopes...)
	var updated, stamp int64
	nativeMust(t, store.db.QueryRow(`SELECT updated_ns,coordination_stamp FROM sessions WHERE id=?`, s.id).Scan(&updated, &stamp))
	var escaped CoordinationReader
	nativeMust(t, store.ReadRecordsView(t.Context(), s.ViewID(), func(r CoordinationReader) error {
		escaped = r
		got, err := r.Revisions(scopes)
		if err == nil && !reflect.DeepEqual(before, got) {
			t.Fatal("view revisions differ")
		}
		return err
	}))
	if _, err := escaped.Revisions(scopes); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	var afterUpdated, afterStamp int64
	nativeMust(t, store.db.QueryRow(`SELECT updated_ns,coordination_stamp FROM sessions WHERE id=?`, s.id).Scan(&afterUpdated, &afterStamp))
	if updated != afterUpdated || stamp != afterStamp {
		t.Fatal("revision-only view mutated session")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.ReadRecords(ctx, func(CoordinationReader) error { t.Fatal("cancelled callback invoked"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNotificationSemanticNewsExactChanges(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativePut(t, s, "format", "swarm", `{"version":2}`)
			nativePut(t, s, "task", "t", `{"id":"t","status":"blocked"}`)
			baseline := semanticMarkFor(t, s)
			scope := RevisionScope{CoordinationIndexSemanticNews, ""}
			for _, tc := range []struct {
				name, payload  string
				changed, ready bool
			}{
				{"identical", `{"id":"t","status":"blocked"}`, false, false},
				{"label", `{"id":"t","status":"blocked","description":"new label"}`, false, false},
				{"canonical", `{"id":"t","status":"pending"}`, true, true},
				{"ABA", `{"id":"t","status":"blocked"}`, true, false},
				{"delete", "", true, false},
				{"missing delete", "", false, false},
				{"recreate", `{"id":"t","status":"blocked"}`, true, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := nativeRevisions(t, s, scope)[0]
					commit := notificationWrite(t, s, legacy, RecordKey{"task", "t"}, tc.payload)
					if changed := nativeRevisions(t, s, scope)[0] != before; changed != tc.changed {
						t.Fatalf("revision changed=%t want %t", changed, tc.changed)
					}
					if !legacy && slices.Contains(commit.Changed, scope) != tc.changed {
						t.Fatalf("Changed=%+v", commit.Changed)
					}
					if got := semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}); got.Different != tc.ready {
						t.Fatalf("news readiness=%+v want %t", got, tc.ready)
					}
				})
			}
			nativePut(t, s, "workflow", "w", `{"id":"w","status":"running"}`)
			nativePut(t, s, "execution", "e", `{"id":"e","member":"m","status":"queued","workflow":"w","generation":1}`)
			nativePut(t, s, "member", "m", `{"id":"m","execution":"e"}`)
			nativePut(t, s, "task", "t", `{"id":"t","execution":"e","status":"blocked"}`)
			baseline = semanticMarkFor(t, s)
			before := nativeRevisions(t, s, scope)[0]
			for _, payload := range []string{
				`{"id":"e","member":"m","status":"running","workflow":"w","generation":1}`,
				`{"id":"e","member":"m","status":"running","workflow":"w","generation":1,"iterations":4,"request":{"label":"rename"},"result":{"value":"opaque"}}`,
			} {
				commit := notificationWrite(t, s, legacy, RecordKey{"execution", "e"}, payload)
				if nativeRevisions(t, s, scope)[0] != before || slices.Contains(commit.Changed, scope) {
					t.Fatal("execution progress invalidated news")
				}
			}
			// A controlled canonical change invalidates the dependency but cannot
			// itself wake the parent. Releasing control must reconsider the head.
			notificationWrite(t, s, legacy, RecordKey{"task", "t"}, `{"id":"t","execution":"e","status":"pending"}`)
			controlled := nativeRevisions(t, s, scope)[0]
			if controlled == before || semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}).Different {
				t.Fatal("controlled change lost revision or leaked readiness")
			}
			commit := notificationWrite(t, s, legacy, RecordKey{"execution", "e"}, `{"id":"e","member":"m","status":"running","generation":1}`)
			if nativeRevisions(t, s, scope)[0] == controlled || !legacy && !slices.Contains(commit.Changed, scope) || !semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}).Different {
				t.Fatal("control-only head change did not invalidate/reconsider news")
			}
		})
	}
}

func TestNotificationDisplayFactChanges(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			for _, v := range []struct{ kind, id, value string }{
				{"format", "swarm", `{"version":2}`},
				{"member", "m", `{"id":"m","name":"A","execution":"e"}`},
				{"member", "other", `{"id":"other"}`},
				{"execution", "e", `{"id":"e","member":"m","status":"running","request":{"callID":"call"},"result":{"value":"old"}}`},
				{"workflow", "w", `{"id":"w","name":"flow","status":"running"}`},
			} {
				nativePut(t, s, v.kind, v.id, v.value)
			}
			scopes := notificationScopes()
			for _, tc := range []struct {
				name, kind, id, value string
				facts                 []RevisionScope
			}{
				{"label", "member", "m", `{"id":"m","name":"B","execution":"e"}`, []RevisionScope{notificationFact("member", "m")}},
				{"identical", "member", "m", `{"id":"m","name":"B","execution":"e"}`, nil},
				{"ABA", "member", "m", `{"id":"m","name":"A","execution":"e"}`, []RevisionScope{notificationFact("member", "m")}},
				{"rename again", "member", "m", `{"id":"m","name":"B","execution":"e"}`, []RevisionScope{notificationFact("member", "m")}},
				{"source only", "member", "m", `{"id":"m","name":"B","execution":"e","opaque":"ignored"}`, nil},
				{"output only", "execution", "e", `{"id":"e","member":"m","status":"running","request":{"callID":"call"},"result":{"value":"new"}}`, nil},
				{"progress", "execution", "e", `{"id":"e","member":"m","status":"running","iterations":1,"request":{"callID":"call"},"result":{"value":"new"}}`, []RevisionScope{notificationFact("member", "m")}},
				{"launch", "execution", "e", `{"id":"e","member":"m","status":"running","iterations":1,"request":{"callID":"next"},"result":{"value":"new"}}`, []RevisionScope{notificationFact("member", "m"), notificationFact("launch", "e")}},
				{"workflow payload", "workflow", "w", `{"id":"w","name":"flow","status":"running","input":"opaque"}`, nil},
				{"workflow label", "workflow", "w", `{"id":"w","name":"renamed","status":"running","input":"opaque"}`, []RevisionScope{notificationFact("workflow", "w")}},
				{"step", "workflow_step", "w/0", `{"id":"step","kind":"agent","status":"running"}`, []RevisionScope{notificationFact("workflow", "w")}},
				{"step output", "workflow_step", "w/0", `{"id":"step","kind":"agent","status":"running","output":"opaque"}`, nil},
				{"step status", "workflow_step", "w/0", `{"id":"step","kind":"agent","status":"completed"}`, []RevisionScope{notificationFact("workflow", "w")}},
				{"step delete", "workflow_step", "w/0", "", []RevisionScope{notificationFact("workflow", "w")}},
				{"workflow delete", "workflow", "w", "", []RevisionScope{notificationFact("workflow", "w"), notificationFact("status", "parent")}},
				{"header", "parent_turn", s.ViewID(), `{"retryPause":{"reason":"retry"}}`, []RevisionScope{notificationFact("status", "parent")}},
				{"unrelated", "custom", "opaque", `{"data":"ignored"}`, nil},
				{"member delete", "member", "m", "", []RevisionScope{notificationFact("member", "m"), notificationFact("status", "parent")}},
				{"launch delete", "execution", "e", "", []RevisionScope{notificationFact("launch", "e")}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := nativeRevisions(t, s, scopes...)
					mark := displayReadFor(t, s, "").Mark
					commit := notificationWrite(t, s, legacy, RecordKey{tc.kind, tc.id}, tc.value)
					delta := displayReadFor(t, s, mark)
					after := nativeRevisions(t, s, scopes...)
					var facts []RevisionScope
					for _, fact := range delta.Records {
						facts = append(facts, notificationFact(fact.Kind, fact.ID))
						if tc.value == "" && fact.Kind == tc.kind && fact.ID == tc.id && fact.Value != nil {
							t.Fatal("deleted fact has nonnil value")
						}
					}
					if !slices.Equal(sortedScopes(scopeSet(facts)), sortedScopes(scopeSet(tc.facts))) {
						t.Fatalf("facts=%+v want %+v", facts, tc.facts)
					}
					for i, scope := range scopes {
						if scope.Index != CoordinationIndexDisplay && scope.Index != CoordinationIndexDisplayFact {
							continue
						}
						want := slices.Contains(facts, scope) || scope.Index == CoordinationIndexDisplay && len(facts) > 0
						if (before[i] != after[i]) != want || !legacy && slices.Contains(commit.Changed, scope) != want {
							t.Fatalf("scope %+v changed=%t want=%t commit=%+v", scope, before[i] != after[i], want, commit.Changed)
						}
					}
					if !legacy {
						for _, scope := range commit.Changed {
							if scope.Index == CoordinationIndexDisplayFact && !slices.Contains(facts, scope) {
								t.Fatalf("extra fact invalidation %+v", scope)
							}
						}
					}
				})
			}
		})
	}
}

func scopeSet(scopes []RevisionScope) map[RevisionScope]bool {
	out := map[RevisionScope]bool{}
	for _, scope := range scopes {
		out[scope] = true
	}
	return out
}

func TestNotificationSemanticMailMembership(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativePut(t, s, "format", "swarm", `{"version":2}`)
			scopes := []RevisionScope{{CoordinationIndexSemanticMail, "a"}, {CoordinationIndexSemanticMail, "b"}, {CoordinationIndexSemanticMail, "unrelated"}}
			for _, tc := range []struct {
				kind, value      string
				changed, pending []bool
			}{
				{"mail", `{"id":"mail","to":"a","text":"one"}`, []bool{true, false, false}, []bool{true, false, false}},
				{"mail", `{"id":"mail","to":"a","text":"two","posted":"2020-01-01T00:00:00Z"}`, []bool{false, false, false}, []bool{true, false, false}},
				{"followup", `{"refresh":true,"phase":"preparing"}`, []bool{true, false, false}, []bool{false, false, false}},
				{"followup", `{"refresh":true,"phase":"preparing","opaque":1}`, []bool{false, false, false}, []bool{false, false, false}},
				{"followup", `{"refresh":true,"phase":"launched"}`, []bool{true, false, false}, []bool{true, false, false}},
				{"followup", "", []bool{false, false, false}, []bool{true, false, false}},
				{"mail", `{"id":"mail","to":"b"}`, []bool{true, true, false}, []bool{false, true, false}},
				{"mail", `{"id":"mail","to":"b","delivered":true}`, []bool{false, true, false}, []bool{false, false, false}},
				{"mail", `{"id":"mail","to":"b"}`, []bool{false, true, false}, []bool{false, true, false}},
				{"mail", "", []bool{false, true, false}, []bool{false, false, false}},
			} {
				before := nativeRevisions(t, s, scopes...)
				commit := notificationWrite(t, s, legacy, RecordKey{tc.kind, "mail"}, tc.value)
				after := nativeRevisions(t, s, scopes...)
				nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
					for i, scope := range scopes {
						if (after[i] != before[i]) != tc.changed[i] || !legacy && slices.Contains(commit.Changed, scope) != tc.changed[i] {
							t.Fatalf("%s/%s scope=%+v changed=%t want=%t", tc.kind, tc.value, scope, after[i] != before[i], tc.changed[i])
						}
						pending, err := SemanticReader(r).SemanticPendingMail(scope.Key)
						if err != nil {
							return err
						}
						if pending != tc.pending[i] {
							t.Fatalf("%s/%s reader=%s pending=%t", tc.kind, tc.value, scope.Key, pending)
						}
					}
					return nil
				}))
			}
		})
	}
}
