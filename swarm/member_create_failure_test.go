package swarm

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

// memberRefusal is a root session whose store refuses, while refuse is set,
// the update that records a new member.
type memberRefusal struct {
	sessions.CoordinationSession
	refuse  atomic.Bool
	refused error
}

func (m *memberRefusal) CoordinationRevision(ctx context.Context) (uint64, error) {
	return m.CoordinationSession.(sessions.CoordinationRevisionSession).CoordinationRevision(ctx)
}

func (m *memberRefusal) UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*sessions.CoordinationState) error) error {
	return m.CoordinationSession.(sessions.CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(state *sessions.CoordinationState) error {
		if err := fn(state); err != nil {
			return err
		}
		if len(state.Changes["member"]) > 0 && m.refuse.Load() {
			return m.refused
		}
		return nil
	})
}

// The store keeps a member's session, from its creation, until the family's
// root is removed. A spawn that fails before a record names the member
// therefore deletes the session it created: nothing else would.
func TestSpawnThatCannotRecordItsMemberLeavesNoSession(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 2)
	ctx := context.Background()
	parent := &memberRefusal{CoordinationSession: r.parent, refused: errors.New("the store refused the member's record")}
	setParent(r, parent)
	parent.refuse.Store(true)
	if _, err := r.start(ctx, "", AgentRequest{Label: "refused", Task: "work", ReadOnly: true}); !errors.Is(err, parent.refused) {
		t.Fatalf("the spawn returned %v", err)
	}
	names, err := r.config.Store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"parent"}) {
		t.Fatalf("after the failed spawn the store holds %q, want the parent alone", names)
	}
	if s := sameAsStored(t, r); len(s.Members) != 0 {
		t.Fatalf("after the failed spawn the family has %d members", len(s.Members))
	}

	parent.refuse.Store(false)
	i, err := r.start(ctx, "", AgentRequest{Label: "recorded", Task: "work", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s := sameAsStored(t, r)
	if member := s.Members[i.member]; member == nil || member.Label != "recorded" {
		t.Fatalf("the next spawn recorded %+v", member)
	}
	if exists, err := r.config.Store.Exists(ctx, s.Members[i.member].Name); err != nil || !exists {
		t.Fatalf("the recorded member's session exists = %v, %v", exists, err)
	}
}
