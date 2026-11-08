package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func TestRecoveryRejectsOrphanAndGappedSteps(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"orphan/0", "w/2"} {
		t.Run(key, func(t *testing.T) {
			r, parent := recoveryFixture(t, 0)
			_, err := parent.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
				return tx.Put(sessions.RecordKey{Kind: "workflow_step", ID: key}, json.RawMessage(`{"id":"saved"}`))
			})
			if err != nil {
				t.Fatal(err)
			}
			probe := probeRecovery(parent)
			config := r.config
			config.Parent = probe
			fresh, err := New(config)
			if err == nil {
				fresh.Close()
				t.Fatal("accepted invalid step topology")
			}
			if probe.writes != 0 {
				t.Fatal("mutated before validating step topology")
			}
		})
	}
}

func TestRecoveryOversizedRecordIsValidatedIntact(t *testing.T) {
	t.Parallel()
	r, parent := recoveryFixture(t, 0)
	data := make([]byte, recoveryPageBytes+1024)
	for i := range data {
		data[i] = 'x'
	}
	payload, err := json.Marshal(&Execution{ID: "oversized", Status: "completed", Request: AgentRequest{Input: string(data)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		return tx.Put(sessions.RecordKey{Kind: "execution", ID: "oversized"}, payload)
	}); err != nil {
		t.Fatal(err)
	}
	probe := probeRecovery(parent)
	config := r.config
	config.Parent = probe
	fresh, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Close()
	if probe.maxPage != len(payload) {
		t.Fatalf("oversized record was truncated or combined: %d != %d", probe.maxPage, len(payload))
	}
	assertRecoveryConverged(t, r, parent)
}

func TestRecoveryNativeApplyOutcomeAndAcceptance(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "stale"}[stale], func(t *testing.T) {
			r, plan := applyFixture(t, false)
			if err := r.worktrees.WriteApply(t.Context(), plan); err != nil {
				t.Fatal(err)
			}
			if err := r.update(t.Context(), func(s *State) error {
				s.Applies[plan.ID] = &ApplyRecord{ID: plan.ID, Plan: plan, Tasks: []TaskReference{{Task: "task", Revision: 1}}, Status: "applying"}
				if stale {
					s.Tasks["task"].AcceptedRevision = 0
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(r.config.Root, "a.txt"))
			if err != nil {
				t.Fatal(err)
			}
			parent := r.parent
			config := r.config
			config.Parent = probeRecovery(parent)
			r.Close()
			fresh, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			fresh.Close()
			s, err := r.read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := "applied"
			if stale {
				want = "recovery_required"
			}
			if s.Applies[plan.ID].Status != want {
				t.Fatalf("receipt: %+v", s.Applies[plan.ID])
			}
			if stale && s.Tasks["task"].Status == "done" {
				t.Fatal("stale acceptance completed task")
			}
			after, err := os.ReadFile(filepath.Join(r.config.Root, "a.txt"))
			if err != nil || string(after) != string(before) {
				t.Fatalf("recovery changed parent files: %v", err)
			}
		})
	}
}

type recoveryLegacyParent struct {
	sessions.Session
	sessions.CoordinationSession
}

func TestRecoveryLegacyAndExactNoticeParity(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "legacy"}[legacy], func(t *testing.T) {
			r, parent := recoveryFixture(t, 8)
			if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				data, err := json.Marshal(&Mail{ID: "already", Kind: "unusual-kind", Delivered: true, To: "other", Task: "delivery", Execution: "finished", Revision: 1})
				if err == nil {
					raw.Records["mail"]["already"] = data
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			config := r.config
			if legacy {
				config.Parent = recoveryLegacyParent{parent.(sessions.Session), parent}
			} else {
				config.Parent = probeRecovery(parent)
			}
			fresh, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			fresh.Close()
			assertRecoveryConverged(t, r, parent)
		})
	}
}

func TestRecoverySandboxFloorsAvoidNoopRewrites(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	if err := r.update(t.Context(), func(s *State) error {
		s.Members["m"] = &Member{ID: "m", AgentName: "/root/m", Context: "c", SandboxPresets: []string{"readonly"}}
		s.Contexts["c"] = &ExecutionContext{ID: "c", Owner: "m", Root: r.config.Root, ReadOnly: true, SandboxPresets: []string{"workspace"}}
		s.Contexts["sibling"] = &ExecutionContext{ID: "sibling", Root: filepath.Join(r.config.Root, "private-sibling"), SandboxPresets: []string{"workspace+git"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	for i := range 2 {
		probe := probeRecovery(r.parent)
		config := r.config
		config.Parent, config.AgentSandbox = probe, "private-home"
		// Retain contexts deliberately; production constructor scheduling is
		// exercised by the broad-API test and release's constructor tests.
		fresh, err := initializeRuntime(config)
		if err != nil {
			t.Fatal(err)
		}
		fresh.Close()
		if i == 0 && probe.puts != 2 || i == 1 && probe.puts != 0 {
			t.Fatalf("open %d rewrote %d records", i, probe.puts)
		}
		s, err := r.read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"readonly", "workspace", "private-home"}
		if !reflect.DeepEqual(s.Members["m"].SandboxPresets, want) || !reflect.DeepEqual(s.Contexts["c"].SandboxPresets, want) {
			t.Fatal("lost restrictive floors")
		}
		if s.Contexts["sibling"].Root != filepath.Join(r.config.Root, "private-sibling") || !reflect.DeepEqual(s.Contexts["sibling"].SandboxPresets, []string{"workspace+git"}) {
			t.Fatal("changed sibling masking authority")
		}
	}
}

func TestRecoveryApplyInitializationBarrier(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	if err := r.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.initializing.Store(true)
	r.prepared = false
	r.config.Promote = func(context.Context) error { return errors.New("recursive prepare during initialization") }
	if err := r.saveApplyOutcome(t.Context(), ApplyRecord{ID: "outcome", Status: "not_applied"}); err != nil {
		t.Fatal(err)
	}
	r.releaseMu.Lock()
	pending, running := r.releasePending, r.releaseRunning
	r.releaseMu.Unlock()
	if pending || running {
		t.Fatal("apply recovery scheduled release before initialization completed")
	}
	if !r.initializing.Load() || r.prepared {
		t.Fatal("outcome bypassed initialization barrier")
	}
}
