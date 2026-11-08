package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// ErrAgentSessionAmbiguous means more than one retained child has the same
// parent/spawn-call binding. Display callers must not guess which one to link.
var ErrAgentSessionAmbiguous = errors.New("agent session is ambiguous")

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
	// Sequence is the atomic next-message cursor, including on Unchanged views.
	// A same-process live tail may be appended only at this exact boundary.
	Sequence int64
	// ParentID is the linked parent's stable identity. Metadata.Parent may
	// retain a display name after deletion and must not be used as identity.
	ParentID string
	Metadata *Metadata
	History  []messages.ChatMessage
	// Display explicitly identifies a selected window and its display-only prefix.
	// Ordinary ReadView always leaves this nil and returns complete History.
	Display          *TranscriptDisplay
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
		id, err = resolveViewTarget(ctx, conn, target)
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
		view.InUse, err = visibleSession(ctx, conn, id, snap.updatedNS, snap.ttlNS, time.Now().UnixNano())
		if err != nil {
			return err
		}
		view.Metadata, err = metadataFromSnapshot(snap)
		if err != nil {
			return err
		}
		view.ID = hex.EncodeToString(id)
		view.Sequence = snap.nextSeq
		view.ParentID = hex.EncodeToString(snap.parentID)
		view.Revision = sessionViewRevision(snap)
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

// sessionViewRevision is shared by full views and bounded headers. It includes
// metadata/parent/name changes and same-length transcript replacements.
func sessionViewRevision(snap sessionSnapshot) string {
	hasher := sha256.New()
	for _, part := range [][]byte{[]byte(snap.name), []byte(snap.parent.String), strconv.AppendInt(nil, snap.updatedNS, 10), strconv.AppendInt(nil, snap.nextSeq, 10)} {
		hasher.Write(part)
		hasher.Write([]byte{0})
	}
	hasher.Write(snap.settings)
	return hex.EncodeToString(hasher.Sum(nil))
}

func (s *SQLiteStore) ReadViewHeader(ctx context.Context, target ViewTarget, knownRevision string) (*SessionViewHeader, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	header := &SessionViewHeader{}
	var id []byte
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		var err error
		id, err = resolveViewTarget(ctx, conn, target)
		if err != nil {
			return err
		}
		var snap sessionSnapshot
		var epoch int64
		nativeSQL(ctx)
		err = conn.QueryRowContext(ctx, "SELECT "+snapshotColumns+",s.transcript_epoch"+snapshotFrom+" WHERE s.id=?", id).Scan(append(snap.scanTargets(), &epoch)...)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		nativeRows(ctx, 1)
		nativePayload(ctx, snap.settings)
		header.InUse, err = visibleSession(ctx, conn, id, snap.updatedNS, snap.ttlNS, time.Now().UnixNano())
		if err != nil {
			return err
		}
		header.Metadata, err = metadataFromSnapshot(snap)
		if err != nil {
			return err
		}
		header.ID = hex.EncodeToString(id)
		header.ParentID = hex.EncodeToString(snap.parentID)
		header.Sequence = snap.nextSeq
		header.Epoch = transcriptEpoch(header.ID, epoch)
		header.Revision = sessionViewRevision(snap)
		header.Unchanged = header.Revision == knownRevision
		return nil
	})
	if err != nil {
		return nil, err
	}
	header.Artifacts = &sqliteArtifactStore{session: &sqliteSession{store: s, id: id, ctx: s.ctx}, readOnly: true}
	return header, nil
}

// childIDBySpawn resolves the session that the spawn_agent call spawnCallID
// of the session named parent created. None is ErrSessionNotFound; more than
// one fails closed.
func childIDBySpawn(ctx context.Context, conn *sql.Conn, parent, spawnCallID string) ([]byte, error) {
	var id []byte
	err := queryEach(ctx, conn, `SELECT c.id FROM sessions c JOIN sessions p ON p.id=c.parent_id WHERE p.name=? AND json_extract(c.settings_json, '$.spawnCallID')=? LIMIT 2`, []any{parent, spawnCallID}, func(rows *sql.Rows) error {
		if id != nil {
			return ErrAgentSessionAmbiguous
		}
		return rows.Scan(&id)
	})
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, ErrSessionNotFound
	}
	return id, nil
}

func readHistory(ctx context.Context, conn *sql.Conn, id []byte, next int64) ([]messages.ChatMessage, error) {
	var history []messages.ChatMessage
	err := queryEach(ctx, conn, "SELECT sequence,payload_json FROM messages WHERE session_id=? ORDER BY sequence", []any{id}, func(rows *sql.Rows) error {
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
