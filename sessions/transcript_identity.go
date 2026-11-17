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
// binding; execution and generation fence follow-ups and resumed work.
type TranscriptIdentity struct {
	Execution  string `json:"execution"`
	Generation int    `json:"generation"`
	Sequence   int64  `json:"sequence"`
}

func (i *TranscriptIdentity) validate() error {
	if i.Execution == "" || i.Generation <= 0 {
		return errors.New("invalid transcript execution identity")
	}
	return nil
}

func writeTranscriptIdentity(ctx context.Context, conn *sql.Conn, id []byte, identity *TranscriptIdentity, sequence int64) error {
	_, metadata, err := loadMetadata(ctx, conn, id)
	if err != nil {
		return err
	}
	metadata.Transcript = nil
	if identity != nil {
		if err := identity.validate(); err != nil {
			return err
		}
		copy := *identity
		copy.Sequence = sequence
		metadata.Transcript = &copy
	}
	settings, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "UPDATE sessions SET settings_json = ?, "+touchSQL+" WHERE id = ?", settings, time.Now().UnixNano(), id)
	return err
}
