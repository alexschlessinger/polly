package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/sessions"
)

// The callback reports success in both cases. Only the backend knows whether
// its transaction committed, so the runtime must not infer rollback from err.
// The first transaction reserves the refresh; inject only its release mark.
type refreshUncertainSession struct {
	sessions.CoordinationSession
	sessions.CoordinationRecords
	rollback bool
	failure  error
	writes   int
}

func (s *refreshUncertainSession) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.writes++
	inject := s.writes == 2
	commit, err := s.CoordinationRecords.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if inject && s.rollback {
			return s.failure
		}
		return nil
	})
	if inject && err == nil {
		err = s.failure
	}
	return commit, err
}

func TestRecordRefreshUncertainMarkPreservesHeldScratchAcrossReopen(t *testing.T) {
	t.Parallel()
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%v", rollback), func(t *testing.T) {
			r, parent, _ := followupRecordsFixture(t, 0, false)
			r.parent = parent
			c, err := r.makeContext(t.Context(), "selected-member", AgentRequest{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c.Scratch, "keep.txt"), []byte("precious scratch"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
				m, err := tx.Member("selected-member")
				if err != nil {
					return err
				}
				m.Context = c.ID
				return tx.PutMember(m)
			}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("unknown refresh mark outcome")
			wrapped := &refreshUncertainSession{CoordinationSession: parent, CoordinationRecords: parent.(sessions.CoordinationRecords), rollback: rollback, failure: failure}
			r.parent = wrapped
			// Exercise the production caller, including its failure-cleanup tail.
			// A direct prepareRefreshWorkspace test would miss hold disposal there.
			_, err = r.refreshFollowup(t.Context(), "selected-member", "refresh", "selected-call")
			if !errors.Is(err, failure) || wrapped.writes != 2 || !r.releaseQuarantined(c.ID) {
				t.Fatalf("uncertain refresh replayed or failed its intent: %v writes=%d", err, wrapped.writes)
			}
			var mailID string
			var saved *FollowupCall
			if err := r.readRecords(t.Context(), func(rd *recordReader) error {
				page, err := queryRecords[FollowupCall](rd, "followup", sessions.RecordQuery{Index: sessions.CoordinationIndexFollowupMember, Key: "selected-member", Limit: 2})
				if err != nil {
					return err
				}
				if len(page.Records) != 1 {
					t.Fatalf("unexpected refresh intents: %+v", page)
				}
				mailID, saved = page.Records[0].Key.ID, page.Records[0].Value
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if saved.Phase != "preparing" {
				t.Fatalf("unknown outcome became failed intent: %+v", saved)
			}
			held := filepath.Join(scratch.Root(), heldScratchName(r.ID, mailID))
			t.Cleanup(func() { _ = scratch.Release(held) })
			assertHeld := func() {
				t.Helper()
				if data, err := os.ReadFile(filepath.Join(held, "keep.txt")); err != nil || string(data) != "precious scratch" {
					t.Fatalf("uncertain refresh lost its owned hold: %q %v", data, err)
				}
			}
			assertHeld()
			if n, err := r.releaseWorkspaces(t.Context()); err != nil || n != 0 || wrapped.writes != 2 {
				t.Fatalf("automatic release replayed uncertain refresh: %d %v", n, err)
			}
			if !rollback && saved.HeldScratch != held || rollback && saved.HeldScratch != "" {
				t.Fatalf("wrong durable mark outcome: %+v", saved)
			}
			r.parent = parent
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(r.config)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			assertHeld()
		})
	}
}
