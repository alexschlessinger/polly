package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
)

type displayRecordView struct{ root sessions.CoordinationSession }

func (v displayRecordView) ReadRecordsView(ctx context.Context, root string, fn func(sessions.CoordinationReader) error) error {
	if root != v.root.ViewID() {
		return sessions.ErrSessionNotFound
	}
	return v.root.(sessions.CoordinationRecords).ReadRecords(ctx, fn)
}

func TestDisplayStatusAndWorkspaceBoundedReads(t *testing.T) {
	t.Parallel()
	var baseline [2]uint64
	for _, n := range []int{16, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, n, false)
			if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
				m, err := tx.Member("selected")
				if err != nil {
					return err
				}
				m.Context = "workspace"
				if err = tx.PutMember(m); err != nil {
					return err
				}
				return tx.PutContext(&ExecutionContext{ID: "workspace", Owner: m.ID, Root: "/selected-worktree"})
			}); err != nil {
				t.Fatal(err)
			}
			r.parent = lifecycleNarrowOnly{root, root.(sessions.CoordinationRecords)}
			metric := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), metric)
			status, err := r.ReadDisplayStatus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			view := displayRecordView{root}
			path, err := ReadMemberWorkspaceView(ctx, view, r.ID, "selected", "workspace")
			if err != nil || path != "/selected-worktree" {
				t.Fatalf("path=%q err=%v", path, err)
			}
			work := metric.Snapshot()
			cost := [2]uint64{work.Count(runtimemetrics.RowsRead), work.Count(runtimemetrics.PayloadBytesRead)}
			if n == 16 {
				baseline = cost
			} else if baseline != cost {
				t.Fatalf("unrelated history scaled display lookups: %v vs %v", baseline, cost)
			}
			t.Logf("history groups=%d status/workspace rows=%d bytes=%d", n, cost[0], cost[1])
			raw, err := root.ReadCoordination(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			s, err := decodeState(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := legacyDisplayStatus(s, r.ID)
			if !reflect.DeepEqual(status, want) || !reflect.DeepEqual(r.ParentDisplayState(status), r.ParentState(s)) {
				t.Fatalf("narrow status drift: %+v want %+v", status, want)
			}
			_, err = root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
				return tx.Put(sessions.RecordKey{Kind: "context", ID: "workspace"}, json.RawMessage(`{"id":"workspace","owner":"other","root":"/wrong"}`))
			})
			if err != nil {
				t.Fatal(err)
			}
			path, err = ReadMemberWorkspaceView(t.Context(), view, r.ID, "selected", "workspace")
			if err != nil || path != "" {
				t.Fatalf("foreign workspace: %q %v", path, err)
			}
		})
	}
}

func TestAllMembersStoppedReadsSourceControls(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 16, false)
	r.parent = lifecycleNarrowOnly{root, root.(sessions.CoordinationRecords)}
	if stopped, err := r.AllMembersStopped(t.Context()); err != nil || stopped {
		t.Fatalf("empty/active control: %v %v", stopped, err)
	}
	if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
		page, err := tx.Members(sessions.RecordQuery{Limit: 64})
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			m := record.Value
			m.Control = MemberControlStopped
			if err = tx.PutMember(m); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stopped, err := r.AllMembersStopped(t.Context()); err != nil || !stopped {
		t.Fatalf("stopped controls with historical running execution: %v %v", stopped, err)
	}
}
