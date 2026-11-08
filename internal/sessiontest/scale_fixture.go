package sessiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// ArchiveSpec describes a small deterministic fixture, not a stress workload.
// Counts/content/order are deterministic; stores still assign real identities
// and timestamps. Limits guard ordinary tests against accidental heavy seeding.
type ArchiveSpec struct {
	Sessions, Messages, PayloadBytes int
}

type ArchiveEntry struct{ ID, Name string }

// SeedArchive creates ordinary persisted conversations through the selected
// backend, then closes their leases. The caller owns the store and its lifetime.
func SeedArchive(t *testing.T, store sessions.SessionStore, spec ArchiveSpec) []ArchiveEntry {
	t.Helper()
	if spec.Sessions < 0 || spec.Sessions > 256 || spec.Messages < 0 || spec.Messages > 32 || spec.PayloadBytes < 0 || spec.PayloadBytes > 16384 || spec.Sessions*spec.Messages*spec.PayloadBytes > 8<<20 {
		t.Fatal("small archive fixture exceeds limits")
	}
	ctx := t.Context()
	entries := make([]ArchiveEntry, 0, spec.Sessions)
	for i := range spec.Sessions {
		name := fmt.Sprintf("scale-archive-%06d", i)
		handle, err := store.Acquire(ctx, name, sessions.AcquireOptions{})
		must(t, err)
		t.Cleanup(func() { handle.Close() })
		identity, ok := handle.(sessions.ViewIdentity)
		if !ok {
			t.Fatal("archive fixture requires stable session identities")
		}
		history := make([]messages.ChatMessage, spec.Messages)
		for j := range history {
			role := messages.MessageRoleUser
			if j%2 != 0 {
				role = messages.MessageRoleAssistant
			}
			history[j] = messages.ChatMessage{Role: role, Content: strings.Repeat("x", spec.PayloadBytes)}
		}
		must(t, handle.AddMessages(ctx, history))
		entries = append(entries, ArchiveEntry{ID: identity.ViewID(), Name: name})
		must(t, handle.Close())
	}
	return entries
}

// FamilyFixture is a retained idle family: members are actual direct child
// sessions, pinned by the root, each with one pending addressed mail. There are
// no invented active executions, workspaces, or capacity bypasses. Shape validity
// is checked by the swarm tests using its own format decoder and inbox rules.
type FamilyFixture struct {
	Parent  sessions.Session
	Members []ArchiveEntry
}

// SeedFamily writes format-2 member/mail records through one coordination
// transaction. It uses only storage types, avoiding a sessions/swarm import cycle.
// The parent lease is kept until test cleanup; child leases are closed promptly.
func SeedFamily(t *testing.T, store sessions.SessionStore, members, payloadBytes int) FamilyFixture {
	t.Helper()
	if members < 0 || members > 256 || payloadBytes < 0 || payloadBytes > 16384 {
		t.Fatal("small family fixture exceeds limits")
	}
	ctx := t.Context()
	parent, err := store.Acquire(ctx, "scale-family", sessions.AcquireOptions{})
	must(t, err)
	t.Cleanup(func() { parent.Close() })
	coord, ok := parent.(sessions.CoordinationSession)
	if !ok {
		t.Fatal("family fixture requires coordination")
	}
	fixture := FamilyFixture{Parent: parent}
	for i := range members {
		name := fmt.Sprintf("scale-member-%06d", i)
		child, err := store.Acquire(ctx, name, sessions.AcquireOptions{Parent: "scale-family"})
		must(t, err)
		t.Cleanup(func() { child.Close() })
		identity, ok := child.(sessions.ViewIdentity)
		if !ok {
			t.Fatal("family fixture requires stable child identities")
		}
		metadata, err := child.GetMetadata(ctx)
		must(t, err)
		metadata.SwarmID = coord.ViewID()
		must(t, child.SetMetadata(ctx, metadata))
		fixture.Members = append(fixture.Members, ArchiveEntry{ID: identity.ViewID(), Name: name})
		must(t, child.Close())
	}
	must(t, coord.UpdateCoordination(ctx, func(state *sessions.CoordinationState) error {
		if state.Records == nil {
			state.Records = map[string]map[string]json.RawMessage{}
		}
		put := func(kind, id string, value any) error {
			payload, err := json.Marshal(value)
			if err != nil {
				return err
			}
			if state.Records[kind] == nil {
				state.Records[kind] = map[string]json.RawMessage{}
			}
			state.Records[kind][id] = payload
			return nil
		}
		if err := put("format", "swarm", struct {
			Version int `json:"version"`
		}{2}); err != nil {
			return err
		}
		for i, member := range fixture.Members {
			// A retained member without an execution is idle; its empty context
			// does not claim a retained checkout or an execution slot.
			if err := put("member", member.ID, map[string]any{
				"id": member.ID, "name": member.Name,
				"agentName": fmt.Sprintf("/root/member-%06d", i),
				"label":     fmt.Sprintf("Fixture member %d", i), "readOnly": true,
			}); err != nil {
				return err
			}
			mailID := fmt.Sprintf("mail-%06d", i)
			if err := put("mail", mailID, map[string]any{
				"id": mailID, "from": state.ParentID, "to": member.ID,
				"kind": "info", "text": strings.Repeat("m", payloadBytes),
				"posted": time.Date(2025, 1, 1, 0, 0, 0, i, time.UTC), "delivered": false,
			}); err != nil {
				return err
			}
			state.Members = append(state.Members, member.ID)
		}
		return nil
	}))
	return fixture
}

// RecordSize reports the materialized payload volume, not SQLite page I/O or
// allocations. No payload or record identifier is retained in metrics.
func RecordSize(state *sessions.CoordinationState) (records, bytes uint64) {
	for _, group := range state.Records {
		records += uint64(len(group))
		for _, payload := range group {
			bytes += uint64(len(payload))
		}
	}
	return records, bytes
}

// ReadFamily is a convenience for callers that want the persisted fixture, not
// a separately constructed expectation with potentially different identities.
func (f FamilyFixture) ReadFamily(ctx context.Context) (*sessions.CoordinationState, error) {
	return f.Parent.(sessions.CoordinationSession).ReadCoordination(ctx)
}
