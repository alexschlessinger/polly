package swarm

import (
	"fmt"
	"reflect"
	"testing"
)

func TestRecordReleaseSharedOwnerReceiptPreflightsWholeBatch(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"automatic", "cleanup"} {
		for _, currentFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/currentFirst=%v", mode, currentFirst), func(t *testing.T) {
				r, parent := releaseRecordsFixture(t, 0)
				currentID, oldID := "z-current", "a-old"
				if currentFirst {
					currentID, oldID = "a-current", "z-old"
				}
				seedReleaseRecords(t, r, parent, currentID)
				if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
					if err := tx.PutMember(&Member{ID: "unrelated", Context: "other-context"}); err != nil {
						return err
					}
					return tx.PutContext(&ExecutionContext{ID: oldID, Owner: currentID, Root: r.config.Root, Release: WorkspaceReleasing, SandboxPresets: []string{"old-floor"}})
				}); err != nil {
					t.Fatal(err)
				}
				if mode == "automatic" {
					if n, err := r.releaseWorkspaces(t.Context()); err != nil || n != 2 {
						t.Fatalf("shared-owner batch self-invalidated: n=%d err=%v", n, err)
					}
				} else if err := r.Cleanup(t.Context(), ""); err != nil {
					t.Fatal(err)
				}
				if err := r.readRecords(t.Context(), func(rd *recordReader) error {
					for _, id := range []string{currentID, oldID} {
						c, err := rd.Context(id)
						if err != nil {
							return err
						}
						if c != nil {
							t.Fatalf("removed context receipt remains: %+v", c)
						}
					}
					m, err := rd.Member(currentID)
					if err != nil {
						return err
					}
					if m.Context != "" || !reflect.DeepEqual(m.SandboxPresets, []string{"read-only"}) {
						t.Fatalf("wrong current-member unlink/floors: %+v", m)
					}
					other, err := rd.Member("unrelated")
					if err == nil && other.Context != "other-context" {
						t.Fatal("unrelated member unlinked")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
