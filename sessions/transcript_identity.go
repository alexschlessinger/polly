package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// TranscriptIdentity binds a saved conversation boundary to the execution
// that wrote it. Sequence prevents unrelated appends from inheriting the
// binding; execution and generation fence follow-ups and resumed work. Counts
// is the execution's own note of how far it had come at the boundary, which
// the store keeps with the binding and never reads.
type TranscriptIdentity struct {
	Execution  string `json:"execution"`
	Generation int    `json:"generation"`
	Sequence   int64  `json:"sequence"`
	Counts     string `json:"counts,omitempty"`
}

func (i *TranscriptIdentity) validate() error {
	if i.Execution == "" || i.Generation <= 0 {
		return errors.New("invalid transcript execution identity")
	}
	return nil
}

// writeTranscriptIdentity binds the session's conversation, as it ends at
// sequence, to identity; nil removes the binding. The binding is one key of
// the stored settings, which SQLite replaces where it sits: nothing else in
// them is read, decoded or written.
func writeTranscriptIdentity(ctx context.Context, conn *sql.Conn, id []byte, identity *TranscriptIdentity, sequence int64) error {
	change, args := "json_remove(CAST(settings_json AS TEXT), '$.transcript')", []any{}
	if identity != nil {
		if err := identity.validate(); err != nil {
			return err
		}
		bound := *identity
		bound.Sequence = sequence
		data, err := json.Marshal(bound)
		if err != nil {
			return err
		}
		change, args = "json_set(CAST(settings_json AS TEXT), '$.transcript', json(?))", []any{string(data)}
	}
	result, err := conn.ExecContext(ctx, "UPDATE sessions SET settings_json = CAST("+change+" AS BLOB), "+touchSQL+" WHERE id = ?",
		append(args, time.Now().UnixNano(), id)...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		err = sql.ErrNoRows
	}
	return err
}
