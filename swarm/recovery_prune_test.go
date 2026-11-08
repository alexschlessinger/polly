package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Fail only the later apply-outcome transaction, after execution/workflow and
// followup recovery have committed. Both an actual rollback and an uncertain
// committed outcome must prevent destructive constructor scratch maintenance.
type recoveryApplyFailure struct {
	*recoveryProbe
	committed, failed bool
}

func (s *recoveryApplyFailure) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	apply := false
	commit, err := s.recoveryProbe.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		if err := fn(recoveryApplyFailureTx{tx, &apply}); err != nil {
			return err
		}
		if apply {
			s.failed = true
			if !s.committed {
				return errRecoveryCommit
			}
		}
		return nil
	})
	if apply && err == nil {
		return sessions.CoordinationCommit{}, errRecoveryCommit
	}
	return commit, err
}

type recoveryApplyFailureTx struct {
	sessions.CoordinationTx
	apply *bool
}

func (tx recoveryApplyFailureTx) Put(key sessions.RecordKey, value json.RawMessage) error {
	if key.Kind == "apply" {
		*tx.apply = true
	}
	return tx.CoordinationTx.Put(key, value)
}

func (tx recoveryApplyFailureTx) QueryCoordinationCandidates(q sessions.CoordinationCandidateQuery) (sessions.RecordPage, error) {
	return sessions.QueryCoordinationCandidates(tx.CoordinationTx, q)
}

func TestRecoveryApplyFailureDefersScratchPruningUntilSuccessfulReopen(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rolled_back", true: "committed_uncertain"}[committed], func(t *testing.T) {
			r, parent := recoveryFixture(t, 0)
			dir, err := r.runtimeDirectory()
			if err != nil {
				t.Fatal(err)
			}
			claim := func(slot string) string {
				t.Helper()
				path, err := scratch.Claim(filepath.Join(dir, slot))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { scratch.Release(path) })
				if err := os.WriteFile(filepath.Join(path, "keep.txt"), []byte(slot), 0600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			park := func(id string) string {
				t.Helper()
				path := claim("park-" + id)
				name := heldScratchName(r.ID, id)
				held, err := scratch.Park(path, name, filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { scratch.Release(held) })
				return held
			}
			stale, live := claim("live-0000"), claim("live-0001")
			held, uncertain, orphan := park("refresh"), park("uncertain"), park("orphan")
			if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				s, err := decodeState(raw)
				if err != nil {
					return err
				}
				s.Contexts["live"] = &ExecutionContext{ID: "live", Owner: "m", Root: r.config.Root, ReadOnly: true, Scratch: live}
				s.Members["m"].Context = "live"
				s.Executions["active"].Workspace = "live"
				s.Followups["refresh"].HeldScratch = held
				s.Followups["uncertain"] = &FollowupCall{Member: "m", Refresh: true, Phase: "preparing"}
				s.Applies["late-apply"] = &ApplyRecord{ID: "late-apply", Status: "applying"}
				return encodeState(raw, s)
			}); err != nil {
				t.Fatal(err)
			}
			assertKept := func(paths ...string) {
				t.Helper()
				for _, path := range paths {
					if _, err := os.ReadFile(filepath.Join(path, "keep.txt")); err != nil {
						t.Fatalf("scratch was pruned before successful recovery: %s: %v", path, err)
					}
				}
			}
			failure := &recoveryApplyFailure{recoveryProbe: probeRecovery(parent), committed: committed}
			config := r.config
			config.Parent = failure
			fresh, err := New(config)
			if fresh != nil || !errors.Is(err, errRecoveryCommit) || !failure.failed {
				t.Fatalf("expected late apply failure: runtime=%v err=%v failed=%v", fresh, err, failure.failed)
			}
			assertKept(stale, live, held, uncertain, orphan)
			// Earlier groups really ran, rather than failure short-circuiting
			// the constructor before it reached preparation.
			s, err := r.read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if s.Executions["active"].Status != "paused" || s.Workflows["w"].Status != "interrupted" || s.Followups["refresh"].Phase != "interrupted" {
				t.Fatal("apply failure did not follow completed preparation groups")
			}
			config.Parent = probeRecovery(parent)
			fresh, err = New(config)
			if err != nil {
				t.Fatal(err)
			}
			fresh.Close()
			assertKept(live, held, uncertain)
			for _, path := range []string{stale, orphan} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("safe unreferenced scratch survived successful recovery: %s: %v", path, err)
				}
			}
			assertRecoveryConverged(t, r, parent)
		})
	}
}

func TestRecoveryLazyPrepareStillPrunesScratch(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	dir, err := r.runtimeDirectory()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := scratch.Claim(filepath.Join(dir, "live-0000"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scratch.Release(stale) })
	if r.prepared || r.initializing.Load() {
		t.Fatal("empty runtime unexpectedly prepared")
	}
	if err := r.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary lazy preparation did not prune stale scratch: %v", err)
	}
}
