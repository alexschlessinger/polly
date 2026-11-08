package sessions

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// displayReadFence binds all selection queries to the supplied stable identity,
// replacement epoch and captured exclusive end. Append cannot move that end.
func displayReadFence(ctx context.Context, conn *sql.Conn, target ViewTarget, epoch string, through int64) ([]byte, string, error) {
	id, err := resolveViewTarget(ctx, conn, target)
	if err != nil {
		return nil, "", err
	}
	var current, next int64
	now := time.Now().UnixNano()
	nativeSQL(ctx)
	err = conn.QueryRowContext(ctx, `SELECT s.transcript_epoch,s.next_sequence FROM sessions s WHERE s.id=? AND `+pageVisibleSQL, id, now, now, now).Scan(&current, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrSessionNotFound
	}
	if err != nil {
		return nil, "", err
	}
	nativeRows(ctx, 1)
	actual := transcriptEpoch(hex.EncodeToString(id), current)
	if epoch == "" || epoch != actual {
		return nil, "", ErrStaleTranscriptEpoch
	}
	if through < 0 || through > next {
		return nil, "", ErrInvalidTranscriptRange
	}
	if through > 0 {
		var found int
		nativeSQL(ctx)
		if err := conn.QueryRowContext(ctx, `SELECT 1 FROM transcript_display WHERE session_id=? AND sequence=?`, id, through-1).Scan(&found); err != nil {
			return nil, "", fmt.Errorf("incomplete transcript display index: %w", err)
		}
		nativeRows(ctx, 1)
	}
	return id, actual, nil
}

func (s *SQLiteStore) ReadTranscriptWindow(ctx context.Context, target ViewTarget, q TranscriptWindowQuery) (TranscriptWindow, error) {
	if err := s.ensureOpen(); err != nil {
		return TranscriptWindow{}, err
	}
	if err := validatePageLimit(q.Turns); err != nil {
		return TranscriptWindow{}, err
	}
	var out TranscriptWindow
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		id, epoch, err := displayReadFence(ctx, conn, target, q.Epoch, q.Through)
		if err != nil {
			return err
		}
		out = TranscriptWindow{ID: hex.EncodeToString(id), Epoch: epoch, Start: q.Through, Through: q.Through, PreviousTurn: -1}
		var starts []int64
		nativeSQL(ctx)
		if err := queryEach(ctx, conn, `SELECT sequence FROM transcript_display WHERE session_id=? AND real_user=1 AND sequence<? ORDER BY sequence DESC LIMIT ?`, []any{id, q.Through, q.Turns + 1}, func(rows *sql.Rows) error {
			var seq int64
			if err := rows.Scan(&seq); err != nil {
				return err
			}
			nativeRows(ctx, 1)
			starts = append(starts, seq)
			return nil
		}); err != nil {
			return err
		}
		if len(starts) > q.Turns {
			out.PreviousTurn = starts[q.Turns]
			starts = starts[:q.Turns]
		}
		if len(starts) > 0 {
			out.Start = starts[len(starts)-1]
		}
		return nil
	})
	return out, err
}

func (s *SQLiteStore) ReadTranscriptPreviousUser(ctx context.Context, target ViewTarget, epoch string, before int64) (*TranscriptDisplayUser, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	var out *TranscriptDisplayUser
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		id, _, err := displayReadFence(ctx, conn, target, epoch, before)
		if err != nil {
			return err
		}
		var seq int64
		var payload []byte
		nativeSQL(ctx)
		err = conn.QueryRowContext(ctx, `SELECT sequence,user_json FROM transcript_display WHERE session_id=? AND real_user=1 AND sequence<? ORDER BY sequence DESC LIMIT 1`, id, before).Scan(&seq, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		nativeRows(ctx, 1)
		nativePayload(ctx, payload)
		out = &TranscriptDisplayUser{Sequence: seq}
		if err := json.Unmarshal(payload, &out.Message); err != nil {
			return fmt.Errorf("decode transcript display user: %w", err)
		}
		return nil
	})
	return out, err
}

func (s *SQLiteStore) ReadTranscriptDisplayPage(ctx context.Context, target ViewTarget, q TranscriptQuery) (TranscriptDisplayPage, error) {
	if err := s.ensureOpen(); err != nil {
		return TranscriptDisplayPage{}, err
	}
	if err := validatePageLimit(q.Limit); err != nil {
		return TranscriptDisplayPage{}, err
	}
	if q.MaxBytes < 0 || q.MaxBytes > MaxTranscriptPageBytes {
		return TranscriptDisplayPage{}, ErrInvalidPageRequest
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = DefaultTranscriptPageBytes
	}
	if q.After < 0 || q.Through < q.After {
		return TranscriptDisplayPage{}, ErrInvalidTranscriptRange
	}
	var out TranscriptDisplayPage
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		id, epoch, err := displayReadFence(ctx, conn, target, q.Epoch, q.Through)
		if err != nil {
			return err
		}
		out = TranscriptDisplayPage{ID: hex.EncodeToString(id), Epoch: epoch, First: q.After, Next: q.After, Through: q.Through}
		type selected struct {
			seq  int64
			size int
		}
		var selectedRows []selected
		nativeSQL(ctx)
		if err := queryEach(ctx, conn, `SELECT sequence,length(fact_json) FROM transcript_display WHERE session_id=? AND relevant=1 AND sequence>=? AND sequence<? ORDER BY sequence LIMIT ?`, []any{id, q.After, q.Through, q.Limit}, func(rows *sql.Rows) error {
			var row selected
			if err := rows.Scan(&row.seq, &row.size); err != nil {
				return err
			}
			nativeRows(ctx, 1)
			selectedRows = append(selectedRows, row)
			return nil
		}); err != nil {
			return err
		}
		used := 0
		for _, row := range selectedRows {
			if row.size > q.MaxBytes-used && len(out.Facts) > 0 {
				return nil
			}
			var payload []byte
			nativeSQL(ctx)
			if err := conn.QueryRowContext(ctx, `SELECT fact_json FROM transcript_display WHERE session_id=? AND sequence=?`, id, row.seq).Scan(&payload); err != nil {
				return err
			}
			nativeRows(ctx, 1)
			nativePayload(ctx, payload)
			var fact TranscriptDisplayFact
			if err := json.Unmarshal(payload, &fact); err != nil {
				return fmt.Errorf("decode transcript display fact: %w", err)
			}
			if fact.Sequence != row.seq || fact.Turn > row.seq || fact.Barrier >= row.seq || fact.AnswerBarrier >= row.seq {
				return fmt.Errorf("invalid transcript display fact")
			}
			out.Facts = append(out.Facts, fact)
			out.Next = row.seq + 1
			used += len(payload)
			if row.size > q.MaxBytes {
				out.Oversized = true
				return nil
			}
		}
		if len(selectedRows) < q.Limit {
			out.Next = q.Through
		}
		return nil
	})
	return out, err
}
