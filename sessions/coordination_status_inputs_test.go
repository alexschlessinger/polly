package sessions

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestStatusInputsMemberWorkspaceChanges(t *testing.T) {
	before := []byte(`{"id":"m","context":"old","sandboxPresets":["workspace"],"control":"","readOnly":true}`)
	for _, tc := range []struct {
		name, after string
		changed     bool
	}{
		{"release", `{"id":"m","context":"","sandboxPresets":["workspace","private-home"],"control":"","readOnly":true}`, false},
		{"restore", `{"id":"m","context":"new","control":"","readOnly":true}`, false},
		{"control", `{"id":"m","control":"stopped","readOnly":true}`, true},
		{"read-only", `{"id":"m","control":"","readOnly":false}`, true},
		{"task", `{"id":"m","control":"","readOnly":true,"task":"next"}`, true},
		{"execution", `{"id":"m","control":"","readOnly":true,"execution":"next"}`, true},
		{"controller", `{"id":"m","control":"","readOnly":true,"controller":"w"}`, true},
		{"name", `{"id":"m","control":"","readOnly":true,"agentName":"/root/new"}`, true},
		{"label", `{"id":"m","control":"","readOnly":true,"label":"new"}`, true},
		{"unknown", `{"id":"m","control":"","readOnly":true,"future":9007199254740993}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := statusInputsChanged(RecordKey{"member", "m"}, before, []byte(tc.after))
			if err != nil || changed != tc.changed {
				t.Fatalf("changed=%t, want %t: %v", changed, tc.changed, err)
			}
		})
	}
	for _, pair := range [][2][]byte{{nil, before}, {before, nil}} {
		if changed, err := statusInputsChanged(RecordKey{"member", "m"}, pair[0], pair[1]); err != nil || !changed {
			t.Fatalf("creation/deletion lost invalidation: %t %v", changed, err)
		}
	}
	if _, err := statusInputsChanged(RecordKey{"member", "m"}, before, []byte(`{"id":`)); err == nil {
		t.Fatal("malformed changed payload was accepted")
	}
}

func TestRuntimeStorageScaleMemberReleaseStatusInputs(t *testing.T) {
	var small [4]uint64
	for _, legacy := range []bool{false, true} {
		for _, history := range []int{4, 64} {
			t.Run(fmt.Sprintf("legacy=%t/history=%d", legacy, history), func(t *testing.T) {
				store, _ := openTestStore(t, ModeMemory, nil, 0)
				s := nativeHandle(t, store, "root")
				nativePut(t, s, "format", "swarm", `{"version":2}`)
				nativePut(t, s, "member", "m", `{"id":"m","readOnly":true,"context":"old"}`)
				_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
					for i := 0; i < history; i++ {
						id := fmt.Sprintf("old-%04d", i)
						payload, _ := json.Marshal(map[string]any{"id": id, "owner": "m", "status": "done", "feedback": strings.Repeat("history", 100)})
						if err := tx.Put(RecordKey{"task", id}, payload); err != nil {
							return err
						}
					}
					return nil
				})
				nativeMust(t, err)
				before := nativeRevisions(t, s, RevisionScope{Index: CoordinationIndexSemanticNews})
				display := displayReadFor(t, s, "")
				var statusBefore SemanticStatusHeader
				nativeMust(t, s.ReadRecords(t.Context(), func(rd CoordinationReader) error {
					id, err := rd.Identity()
					if err != nil {
						return err
					}
					statusBefore, err = rd.(CoordinationStatusReader).SemanticStatus(id.ParentID, true)
					return err
				}))
				recorder := runtimemetrics.New()
				ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
				after := json.RawMessage(`{"id":"m","readOnly":true,"context":"","sandboxPresets":["workspace"]}`)
				if legacy {
					err = s.UpdateCoordination(ctx, func(raw *CoordinationState) error {
						raw.Records["member"]["m"] = after
						return nil
					})
				} else {
					_, err = s.UpdateRecords(ctx, func(tx CoordinationTx) error { return tx.Put(RecordKey{"member", "m"}, after) })
				}
				nativeMust(t, err)
				metrics := recorder.Snapshot()
				cost := [4]uint64{metrics.Count(runtimemetrics.SQLStatements), metrics.Count(runtimemetrics.RowsRead), metrics.Count(runtimemetrics.PayloadBytesRead), metrics.Count(runtimemetrics.RecordsDecoded)}
				if !legacy {
					if history == 4 {
						small = cost
					} else if cost != small {
						t.Fatalf("workspace-only member write followed closed history: %v -> %v", small, cost)
					}
				}
				t.Logf("SQL/rows/payload bytes/decodes: %v", cost)
				if !reflect.DeepEqual(before, nativeRevisions(t, s, RevisionScope{Index: CoordinationIndexSemanticNews})) {
					t.Fatal("workspace-only change altered semantic news")
				}
				nativeMust(t, s.ReadRecords(t.Context(), func(rd CoordinationReader) error {
					id, err := rd.Identity()
					if err != nil {
						return err
					}
					status, err := rd.(CoordinationStatusReader).SemanticStatus(id.ParentID, true)
					if err == nil && !reflect.DeepEqual(statusBefore, status) {
						t.Fatalf("workspace-only change altered status: before=%+v after=%+v", statusBefore, status)
					}
					return err
				}))
				updated := false
				for _, record := range displayReadFor(t, s, display.Mark).Records {
					if record.Kind != "member" || record.ID != "m" {
						continue
					}
					var member DisplayMember
					nativeMust(t, json.Unmarshal(record.Value, &member))
					var value struct {
						Context        string
						SandboxPresets []string
					}
					nativeMust(t, json.Unmarshal(member.Member, &value))
					if value.Context != "" || !reflect.DeepEqual(value.SandboxPresets, []string{"workspace"}) {
						t.Fatalf("display retained stale workspace/floor: %+v", value)
					}
					updated = true
				}
				if !updated {
					t.Fatal("workspace-only write failed to update compact display")
				}
			})
		}
	}
}
