package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// ViewTarget resolves an existing session. ID is an opaque, stable identity
// returned by ReadView. Otherwise use Name, or Parent and SpawnCallID together.
// A target never creates a session; ambiguous child links fail closed.
type ViewTarget struct {
	ID, Name, Parent, SpawnCallID string
}

// SessionView is a consistent, read-only snapshot. Reading it neither acquires
// a lease nor updates last-used time. Artifacts remains usable after a writer
// closes, but cannot write and never resolves a deleted/reused session name.
type SessionView struct {
	ID, Revision string
	// ParentID is the linked parent's stable identity. Metadata.Parent may
	// retain a display name after deletion and must not be used as identity.
	ParentID         string
	Metadata         *Metadata
	History          []messages.ChatMessage
	Artifacts        artifacts.Store
	InUse, Unchanged bool
}

// ViewStore is optional so existing SessionStore implementations keep working.
// Matching knownRevision omits History; metadata and identity are still checked.
type ViewStore interface {
	ReadView(context.Context, ViewTarget, string) (*SessionView, error)
}

// ViewIdentity is implemented by sessions that can be addressed without a
// lease. The identity remains safe to read after Close; it grants no writes.
type ViewIdentity interface{ ViewID() string }

func (s *sqliteSession) ViewID() string { return hex.EncodeToString(s.id) }

var ErrReadOnlyView = errors.New("session view is read-only")

func (s *sqliteArtifactStore) requireReadAccess(ctx context.Context) error {
	if !s.readOnly {
		return s.session.requireLease(ctx, s.session.store.db)
	}
	return s.session.store.ensureOpen()
}

func (s *SQLiteStore) ReadView(ctx context.Context, target ViewTarget, knownRevision string) (*SessionView, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	view := &SessionView{}
	var id []byte
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		var err error
		switch {
		case target.ID != "":
			id, err = decodeSessionID(target.ID)
		case target.Parent != "" && target.SpawnCallID != "":
			rows, err := conn.QueryContext(ctx, `SELECT c.id FROM sessions c JOIN sessions p ON p.id=c.parent_id WHERE p.name=? AND json_extract(c.settings_json, '$.spawnCallID')=? LIMIT 2`, target.Parent, target.SpawnCallID)
			if err != nil {
				return err
			}
			err = eachRow(rows, func() error {
				if id != nil {
					return fmt.Errorf("agent session is ambiguous")
				}
				return rows.Scan(&id)
			})
			if err != nil {
				return err
			}
			if id == nil {
				return ErrSessionNotFound
			}
		case target.Name != "":
			id, err = sessionIDByName(ctx, conn, target.Name)
		default:
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		snap, err := scanSnapshot(ctx, conn, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		// A swarm parent or member outlives its TTL until the family is
		// cleaned up, so it stays visible too.
		now := time.Now().UnixNano()
		var pinned bool
		if err := conn.QueryRowContext(ctx, "SELECT "+leaseActiveSQL("sessions.id")+", "+swarmPinnedSQL+" FROM sessions WHERE id = ?", now, id).Scan(&view.InUse, &pinned); err != nil {
			return err
		}
		if !view.InUse && !pinned && expiredAt(snap.updatedNS, snap.ttlNS, now) {
			return ErrSessionNotFound
		}
		view.Metadata, err = metadataFromSnapshot(snap)
		if err != nil {
			return err
		}
		view.ID = hex.EncodeToString(id)
		view.ParentID = hex.EncodeToString(snap.parentID)
		// Includes metadata and parent/name changes, as well as history changes
		// that preserve the message count (Clear/Reset followed by appends).
		// Writers advance updated_ns monotonically, even within one clock tick.
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", snap.name, snap.parent.String, snap.updatedNS, snap.nextSeq, snap.settings)))
		view.Revision = hex.EncodeToString(digest[:])
		view.Unchanged = view.Revision == knownRevision
		if !view.Unchanged {
			view.History, err = readHistory(ctx, conn, id, snap.nextSeq)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// This read handle owns no lease, heartbeat, or runtime. Store shutdown
	// cancels readers; the artifact membership query is still scoped by id.
	view.Artifacts = &sqliteArtifactStore{session: &sqliteSession{store: s, id: id, ctx: s.ctx}, readOnly: true}
	return view, nil
}

func readHistory(ctx context.Context, conn *sql.Conn, id []byte, next int64) ([]messages.ChatMessage, error) {
	rows, err := conn.QueryContext(ctx, "SELECT sequence,payload_json FROM messages WHERE session_id=? ORDER BY sequence", id)
	if err != nil {
		return nil, err
	}
	var history []messages.ChatMessage
	err = eachRow(rows, func() error {
		var sequence int64
		var payload []byte
		if err := rows.Scan(&sequence, &payload); err != nil {
			return err
		}
		if sequence != int64(len(history)) {
			return fmt.Errorf("session message sequence is corrupt: got %d, want %d", sequence, len(history))
		}
		var msg messages.ChatMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("decode session message %d: %w", sequence, err)
		}
		history = append(history, msg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if int64(len(history)) != next {
		return nil, fmt.Errorf("session message sequence is corrupt: read %d messages, expected %d", len(history), next)
	}
	return history, nil
}
