package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
)

func (*SQLiteStore) SummaryCapabilities() SummaryCapabilities {
	return SummaryCapabilities{NativeIndexed: true}
}
func (*SQLiteStore) TranscriptCapabilities() TranscriptCapabilities {
	return TranscriptCapabilities{NativeIndexed: true, StableEpoch: true, NativeDisplay: true}
}

// Visibility is logical, not a maintenance side effect. The new page APIs do
// not alter the older whole-result APIs or their startup/sweep policy.
var pageVisibleSQL = `(NOT (s.ttl_ns>0 AND s.updated_ns<=? AND s.ttl_ns<=?-s.updated_ns) OR ` + leaseActiveSQL("s.id") + ` OR EXISTS(SELECT 1 FROM swarm_members WHERE member_id=s.id) OR EXISTS(SELECT 1 FROM swarm_records WHERE parent_id=s.id))`

type nativeSummaryCursor struct {
	Version          int
	Filtered         bool
	Parent, Name, ID []byte
	Used             int64
}

func (s *SQLiteStore) ListSummaryPage(ctx context.Context, q SummaryQuery) (SummaryPage, error) {
	if err := s.ensureOpen(); err != nil {
		return SummaryPage{}, err
	}
	if err := validatePageLimit(q.Limit); err != nil {
		return SummaryPage{}, err
	}
	c := nativeSummaryCursor{Version: 1, Filtered: q.ParentID != nil}
	if q.ParentID != nil {
		c.Parent = []byte(*q.ParentID)
	}
	var parent []byte
	var err error
	if q.ParentID != nil && *q.ParentID != "" {
		parent, err = decodeSessionID(*q.ParentID)
		if err != nil {
			return SummaryPage{}, fmt.Errorf("%w: parent identity", ErrInvalidPageRequest)
		}
	}
	if q.After != "" {
		data, e := base64.RawURLEncoding.DecodeString(q.After)
		var found nativeSummaryCursor
		if e != nil || json.Unmarshal(data, &found) != nil || found.Version != c.Version || found.Filtered != c.Filtered || !bytes.Equal(found.Parent, c.Parent) || len(found.ID) != 16 {
			return SummaryPage{}, ErrInvalidSummaryCursor
		}
		c = found
	}
	runtimemetrics.Add(ctx, runtimemetrics.SummaryReads, 1)
	var page SummaryPage
	err = s.withRead(ctx, func(conn *sql.Conn) error {
		now := time.Now().UnixNano()
		var ids [][]byte
		for _, selection := range summaryPageQueries(q.ParentID != nil, parent, c, now) {
			remaining := q.Limit + 1 - len(ids)
			if remaining == 0 {
				break
			}
			nativeSQL(ctx)
			if err := queryEach(ctx, conn, selection.sql, append(selection.args, remaining), func(rows *sql.Rows) error {
				var id []byte
				if err := rows.Scan(&id); err != nil {
					return err
				}
				nativeRows(ctx, 1)
				ids = append(ids, id)
				return nil
			}); err != nil {
				return err
			}
		}
		for _, id := range ids[:min(q.Limit, len(ids))] {
			row, err := nativeSummary(ctx, conn, id, now)
			if err != nil {
				return err
			}
			page.Summaries = append(page.Summaries, *row)
		}
		if len(ids) > q.Limit {
			last := page.Summaries[len(page.Summaries)-1]
			c.ID = ids[q.Limit-1]
			c.Name = []byte(last.Metadata.Name)
			c.Used = last.Metadata.LastUsed.UnixNano()
			data, err := json.Marshal(c)
			if err != nil {
				return err
			}
			page.Next = base64.RawURLEncoding.EncodeToString(data)
		}
		return nil
	})
	if err != nil {
		return SummaryPage{}, err
	}
	return page, nil
}

type summaryPageQuery struct {
	sql  string
	args []any
}

func summaryPageQueries(filtered bool, parent []byte, cursor nativeSummaryCursor, now int64) []summaryPageQuery {
	where := pageVisibleSQL
	args := []any{now, now, now}
	if filtered {
		if len(parent) == 0 {
			where += ` AND s.parent_id IS NULL`
		} else {
			where += ` AND s.parent_id=?`
			args = append(args, parent)
		}
	}
	index := "sessions_summary_idx"
	if filtered {
		index = "sessions_parent_summary_idx"
	}
	var queries []summaryPageQuery
	add := func(bound string, values ...any) {
		queryArgs := append(append([]any(nil), args...), values...)
		queries = append(queries, summaryPageQuery{
			sql:  `SELECT s.id FROM sessions s INDEXED BY ` + index + ` WHERE ` + where + bound + ` ORDER BY s.updated_ns DESC,s.name,s.id LIMIT ?`,
			args: queryArgs,
		})
	}
	if len(cursor.ID) == 0 {
		add("")
	} else {
		// Mixed-direction tuple inequalities can seek only the timestamp, then
		// rescan every earlier name/ID tied at that time. Use disjoint ranges in
		// final page order instead: each range has an equality prefix and one
		// strict index bound, including the full timestamp/name/ID seek. Stop as
		// soon as the page plus its lookahead is full, with at most three seeks.
		add(` AND s.updated_ns=? AND s.name=? AND s.id>?`, cursor.Used, string(cursor.Name), cursor.ID)
		add(` AND s.updated_ns=? AND s.name>?`, cursor.Used, string(cursor.Name))
		add(` AND s.updated_ns<?`, cursor.Used)
	}
	return queries
}

func nativeSummary(ctx context.Context, conn *sql.Conn, id []byte, now int64) (*SessionSummary, error) {
	var snap sessionSnapshot
	var inUse bool
	nativeSQL(ctx)
	err := conn.QueryRowContext(ctx, "SELECT "+snapshotColumns+","+leaseActiveSQL("s.id")+snapshotFrom+` WHERE s.id=? AND `+pageVisibleSQL, now, id, now, now, now).Scan(append(snap.scanTargets(), &inUse)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	nativeRows(ctx, 1)
	nativePayload(ctx, snap.settings)
	metadata, err := metadataFromSnapshot(snap)
	if err != nil {
		return nil, err
	}
	runtimemetrics.Add(ctx, runtimemetrics.SummariesRead, 1)
	return &SessionSummary{ID: hex.EncodeToString(snap.id), ParentID: hex.EncodeToString(snap.parentID), Metadata: metadata, MessageCount: int(snap.nextSeq), InUse: inUse}, nil
}
func (s *SQLiteStore) GetSummary(ctx context.Context, id string) (*SessionSummary, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	raw, err := decodeSessionID(id)
	if err != nil {
		return nil, err
	}
	runtimemetrics.Add(ctx, runtimemetrics.SummaryReads, 1)
	var out *SessionSummary
	err = s.withRead(ctx, func(conn *sql.Conn) error {
		var err error
		out, err = nativeSummary(ctx, conn, raw, time.Now().UnixNano())
		return err
	})
	return out, err
}

func resolveViewTarget(ctx context.Context, conn *sql.Conn, target ViewTarget) ([]byte, error) {
	switch {
	case target.ID != "":
		return decodeSessionID(target.ID)
	case target.Parent != "" && target.SpawnCallID != "":
		return childIDBySpawn(ctx, conn, target.Parent, target.SpawnCallID)
	case target.Name != "":
		return sessionIDByName(ctx, conn, target.Name)
	default:
		return nil, ErrSessionNotFound
	}
}
func transcriptEpoch(id string, epoch int64) string { return id + ":" + strconv.FormatInt(epoch, 10) }

func (s *SQLiteStore) ReadTranscriptPage(ctx context.Context, target ViewTarget, q TranscriptQuery) (TranscriptPage, error) {
	if err := s.ensureOpen(); err != nil {
		return TranscriptPage{}, err
	}
	if err := validatePageLimit(q.Limit); err != nil {
		return TranscriptPage{}, err
	}
	if q.MaxBytes < 0 || q.MaxBytes > MaxTranscriptPageBytes {
		return TranscriptPage{}, fmt.Errorf("%w: transcript byte limit", ErrInvalidPageRequest)
	}
	if q.MaxBytes == 0 {
		q.MaxBytes = DefaultTranscriptPageBytes
	}
	if q.After < 0 || q.Through < 0 || q.Through != 0 && q.After > q.Through {
		return TranscriptPage{}, ErrInvalidTranscriptRange
	}
	runtimemetrics.Add(ctx, runtimemetrics.TranscriptReads, 1)
	var page TranscriptPage
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		id, err := resolveViewTarget(ctx, conn, target)
		if err != nil {
			return err
		}
		var epoch, next int64
		var identityJSON sql.NullString
		nativeSQL(ctx)
		now := time.Now().UnixNano()
		err = conn.QueryRowContext(ctx, `SELECT s.transcript_epoch,s.next_sequence,json_extract(s.settings_json,'$.transcript') FROM sessions s WHERE s.id=? AND `+pageVisibleSQL, id, now, now, now).Scan(&epoch, &next, &identityJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		nativeRows(ctx, 1)
		page.ID = hex.EncodeToString(id)
		page.Epoch = transcriptEpoch(page.ID, epoch)
		if q.Epoch != "" && q.Epoch != page.Epoch {
			return ErrStaleTranscriptEpoch
		}
		page.First = q.After
		page.Next = q.After
		page.Through = q.Through
		if page.Through == 0 {
			page.Through = next
		}
		if q.After > page.Through || page.Through > next {
			return ErrInvalidTranscriptRange
		}
		if identityJSON.Valid {
			nativePayload(ctx, []byte(identityJSON.String))
			if err := json.Unmarshal([]byte(identityJSON.String), &page.Transcript); err != nil {
				return fmt.Errorf("decode transcript identity: %w", err)
			}
		}
		type selected struct {
			seq  int64
			size int
		}
		var selectedRows []selected
		nativeSQL(ctx)
		if err := queryEach(ctx, conn, `SELECT sequence,length(payload_json) FROM messages WHERE session_id=? AND sequence>=? AND sequence<? ORDER BY sequence LIMIT ?`, []any{id, q.After, page.Through, q.Limit}, func(rows *sql.Rows) error {
			var v selected
			if err := rows.Scan(&v.seq, &v.size); err != nil {
				return err
			}
			nativeRows(ctx, 1)
			selectedRows = append(selectedRows, v)
			return nil
		}); err != nil {
			return err
		}
		used := 0
		stopped := false
		for _, row := range selectedRows {
			if row.seq != page.Next {
				return fmt.Errorf("session message sequence is corrupt: got %d, want %d", row.seq, page.Next)
			}
			if row.size > q.MaxBytes-used && len(page.Messages) > 0 {
				stopped = true
				break
			}
			var payload []byte
			nativeSQL(ctx)
			if err := conn.QueryRowContext(ctx, `SELECT payload_json FROM messages WHERE session_id=? AND sequence=?`, id, row.seq).Scan(&payload); err != nil {
				return err
			}
			nativeRows(ctx, 1)
			nativePayload(ctx, payload)
			var msg messages.ChatMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				return fmt.Errorf("decode session message %d: %w", row.seq, err)
			}
			page.Messages = append(page.Messages, msg)
			page.Next++
			used += len(payload)
			runtimemetrics.Add(ctx, runtimemetrics.TranscriptMessagesRead, 1)
			if row.size > q.MaxBytes {
				page.Oversized = true
				stopped = true
				break
			}
		}
		if !stopped && len(selectedRows) < q.Limit && page.Next != page.Through {
			return fmt.Errorf("session message sequence is corrupt: stopped at %d, expected %d", page.Next, page.Through)
		}
		return nil
	})
	if err != nil {
		return TranscriptPage{}, err
	}
	return page, nil
}
