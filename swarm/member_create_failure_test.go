package swarm

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// memberRecord is a root session that can fail the update that records a new
// member: refused by the store, or accepted and then not written.
type memberRecord struct {
	sessions.CoordinationSession
	refuse, unwritten atomic.Bool
	failure           error
	// registered counts the updates that asked the store to register members.
	registered atomic.Int32
}

func (m *memberRecord) CoordinationRevision(ctx context.Context) (uint64, error) {
	return m.CoordinationSession.(sessions.CoordinationRevisionSession).CoordinationRevision(ctx)
}

func (m *memberRecord) UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*sessions.CoordinationState) error) error {
	var state *sessions.CoordinationState
	err := m.CoordinationSession.(sessions.CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(s *sessions.CoordinationState) error {
		state = s
		if err := fn(s); err != nil {
			return err
		}
		if len(s.Members) > 0 {
			m.registered.Add(1)
		}
		if len(s.Changes["member"]) > 0 && m.refuse.Load() {
			return m.failure
		}
		return nil
	})
	if err == nil && len(state.Changes["member"]) > 0 && m.unwritten.Load() {
		state.Written = func(context.Context) error { return m.failure }
	}
	return err
}

// The store keeps a member's session, from its creation, until the family's
// root is removed. A spawn that fails before a record names the member
// therefore deletes the session it created: nothing else would. A spawn that
// fails only in waiting for the record to be written has a member, whose
// session stays.
func TestSpawnThatCannotRecordItsMemberLeavesNoSession(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]sessions.StoreConfig{
		"memory": {Mode: sessions.ModeMemory},
		"disk":   {Mode: sessions.ModeDisk, Path: filepath.Join(t.TempDir(), "polly.db")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := runtimeTestOn(t, config, idleModel(), 1, 3, nil)
			// Releases of a failed spawn's workspace would otherwise edit the
			// family while the test compares it with the store.
			suspendAutoRelease(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			parent := &memberRecord{CoordinationSession: r.parent, failure: errors.New("the store failed the member's record")}
			setParent(r, parent)

			parent.refuse.Store(true)
			if _, err := r.start(ctx, "", AgentRequest{Label: "refused", Task: "work", ReadOnly: true}); !errors.Is(err, parent.failure) {
				t.Fatalf("the refused spawn returned %v", err)
			}
			names, err := r.config.Store.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(names, []string{"parent"}) {
				t.Fatalf("after the refused spawn the store holds %q, want the parent alone", names)
			}
			if s := sameAsStored(t, r); len(s.Members) != 0 {
				t.Fatalf("after the refused spawn the family has %d members", len(s.Members))
			}

			parent.refuse.Store(false)
			parent.unwritten.Store(true)
			if _, err := r.start(ctx, "", AgentRequest{Label: "unwritten", Task: "work", ReadOnly: true}); !errors.Is(err, parent.failure) {
				t.Fatalf("the spawn whose record was not written returned %v", err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Members) != 1 {
				t.Fatalf("the family has %d members, want the one whose record was accepted", len(s.Members))
			}
			for _, member := range s.Members {
				if exists, err := r.config.Store.Exists(ctx, member.Name); err != nil || !exists {
					t.Fatalf("the recorded member's session exists = %v, %v", exists, err)
				}
			}

			parent.unwritten.Store(false)
			i, err := r.start(ctx, "", AgentRequest{Label: "recorded", Task: "work", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			awaitIdle(t, r, ctx)
			s = sameAsStored(t, r)
			if member := s.Members[i.member]; member == nil || member.Label != "recorded" {
				t.Fatalf("the next spawn recorded %+v", member)
			}
			if exists, err := r.config.Store.Exists(ctx, s.Members[i.member].Name); err != nil || !exists {
				t.Fatalf("the recorded member's session exists = %v, %v", exists, err)
			}
			// The store made each session a member as it created it.
			if n := parent.registered.Load(); n != 0 {
				t.Fatalf("%d of the runtime's updates asked the store to register members", n)
			}
		})
	}
}
