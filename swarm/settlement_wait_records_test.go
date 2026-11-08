package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestSettlementRecordsDiscoversMissingDependencyBeforeReturn(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	seedSettlement(t, r, func(s *State) { settlementNoop(s); delete(s.Snapshots, "base") })
	hub := localWaitNotifications(t, r)
	b := startWaitReadBarrier(t, r, root, r.Settle)
	b.read(t, 1)
	// This exact dependency was absent from the initial subscription. Its
	// creation after the read must be observed by Replace's mandatory reread.
	commitWaitRecord(t, r, root, "snapshot", "base", &worktree.Snapshot{ID: "base", Tree: "tree", Commit: "base-commit", Source: "/source"})
	hub.mu.Lock()
	for sub := range hub.subscriptions {
		if _, ok := sub.scopes[sessions.RevisionScope{Index: sessions.CoordinationIndexRecords}]; ok {
			t.Error("native settlement subscribed to the whole family")
		}
		if _, ok := sub.scopes[waitRecordScope("snapshot", "base")]; ok || len(sub.changes) != 0 {
			t.Error("test did not exercise a newly discovered, unhinted dependency")
		}
	}
	hub.mu.Unlock()
	b.advance(t)
	b.read(t, 2)
	b.advance(t)
	b.result(t, nil)
}

func TestSettlementRecordsWaitBarrierAndScopeIsolation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"activity", "mail", "cancel", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 0, false)
			hub := localWaitNotifications(t, r)
			r.active["selected"] = &invocation{}
			b := startWaitReadBarrier(t, r, root, r.Settle)
			b.read(t, 1)
			b.advance(t)
			b.read(t, 2)
			hub.Publish([]sessions.RevisionScope{{Index: sessions.CoordinationIndexRecords}, waitRecordScope("member", "unrelated"), {Index: sessions.CoordinationIndexKind, Key: "publication"}})
			hub.mu.Lock()
			for sub := range hub.subscriptions {
				if len(sub.changes) != 0 {
					t.Error("unrelated notification woke settlement")
				}
			}
			hub.mu.Unlock()
			switch mode {
			case "activity":
				r.mu.Lock()
				delete(r.active, "selected")
				r.mu.Unlock()
				r.publishActivity()
			case "mail":
				commitWaitRecord(t, r, root, "mail", "request", &Mail{ID: "request", To: r.ID, Kind: "request", Delivered: true})
			case "cancel":
				b.cancel()
			case "shutdown":
				hub.Close()
			}
			if mode == "cancel" {
				b.result(t, context.Canceled)
				return
			}
			b.advance(t)
			if mode == "shutdown" {
				b.result(t, context.Canceled)
				return
			}
			b.read(t, 3)
			b.advance(t)
			select {
			case err := <-b.done:
				want := "task selected revision"
				if mode == "mail" {
					want = "a member is waiting for a parent reply"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("settlement = %v, want %q", err, want)
				}
			case <-b.ctx.Done():
				t.Fatal("settlement lost its before-block notification")
			}
		})
	}
}

func TestSettlementRecordsLegacyCompatibility(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"delivery", "no-op", "deferral", "candidate"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 0, false)
			seedSettlement(t, r, func(s *State) {
				switch mode {
				case "delivery":
					settlementDelivery(s)
				case "no-op":
					settlementNoop(s)
				case "deferral":
					settlementDeferred(s)
				case "candidate":
					settlementCandidate(s)
				}
			})
			r.parent = repositoryLegacyOnly{root}
			localWaitNotifications(t, r)
			err := r.Settle(t.Context())
			if mode == "no-op" || mode == "deferral" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || errors.Is(err, errHotLegacy) {
				t.Fatalf("legacy result = %v", err)
			}
		})
	}
}

func TestSettlementParentWaitReturnRepairsWithoutSnapshot(t *testing.T) {
	t.Parallel()
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprint(repair), func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 16, false)
			if repair {
				seedSettlement(t, r, settlementDelivery)
			}
			guard := guardSettlement(t, r, root)
			wake, err := r.waitParent(t.Context())
			if err != nil || !wake.Idle {
				t.Fatalf("wait = %+v, %v", wake, err)
			}
			wantWrites := int64(0)
			if repair {
				wantWrites = 1
			}
			if guard.writes.Load() != wantWrites {
				t.Fatalf("repair writes = %d, want %d", guard.writes.Load(), wantWrites)
			}
			if _, err := r.waitParent(t.Context()); err != nil {
				t.Fatal(err)
			}
			if guard.writes.Load() != wantWrites {
				t.Fatal("wait reposted an existing notice")
			}
			if repair {
				raw, err := root.ReadCoordination(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				s, err := decodeState(raw)
				if err != nil {
					t.Fatal(err)
				}
				if resultNotice(s, s.Tasks["selected"]) == nil {
					t.Fatal("successful wait omitted delivery repair")
				}
			}
		})
	}
}

func TestSettlementParentWaitReturnBoundedHistory(t *testing.T) {
	t.Parallel()
	var baseline [4]uint64
	for _, count := range []int{16, 512} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, count, false)
			guardSettlement(t, r, root)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			if wake, err := r.waitParent(ctx); err != nil || !wake.Idle {
				t.Fatalf("wait = %+v, %v", wake, err)
			}
			snapshot := recorder.Snapshot()
			work := [4]uint64{snapshot.Count(runtimemetrics.SQLStatements), snapshot.Count(runtimemetrics.RowsRead), snapshot.Count(runtimemetrics.PayloadBytesRead), snapshot.Count(runtimemetrics.RecordsDecoded)}
			if count == 16 {
				baseline = work
			} else if work != baseline {
				t.Fatalf("successful wait scaled with completed history: %v vs %v", work, baseline)
			}
			t.Logf("completed history groups=%d: successful wait SQL=%d rows=%d bytes=%d decoded=%d", count, work[0], work[1], work[2], work[3])
		})
	}
}
