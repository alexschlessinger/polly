package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/messages"
)

const transcriptDisplayTable = "transcript_display"

var transcriptDisplaySpec = schemaTableSpec{
	columns: []schemaColumnSpec{
		{"session_id", "BLOB", 1, 1, ""}, {"sequence", "INTEGER", 1, 2, ""},
		{"real_user", "INTEGER", 1, 0, ""}, {"relevant", "INTEGER", 1, 0, ""},
		{"turn_sequence", "INTEGER", 1, 0, ""}, {"barrier_sequence", "INTEGER", 1, 0, ""}, {"answer_barrier_sequence", "INTEGER", 1, 0, ""},
		{"fact_json", "BLOB", 1, 0, ""}, {"user_json", "BLOB", 1, 0, ""},
	}, withoutRowID: 1,
	requiredSQL: []string{"check(sequence>=0)", "check(real_userin(0,1))", "check(relevantin(0,1))", "onupdatecascade"},
}

func createTranscriptDisplaySchema(ctx context.Context, conn *sql.Conn) error {
	return execAll(ctx, conn,
		`CREATE TABLE IF NOT EXISTS transcript_display (
 session_id BLOB NOT NULL, sequence INTEGER NOT NULL CHECK(sequence>=0),
 real_user INTEGER NOT NULL CHECK(real_user IN (0,1)), relevant INTEGER NOT NULL CHECK(relevant IN (0,1)),
 turn_sequence INTEGER NOT NULL, barrier_sequence INTEGER NOT NULL, answer_barrier_sequence INTEGER NOT NULL,
 fact_json BLOB NOT NULL, user_json BLOB NOT NULL,
 PRIMARY KEY(session_id,sequence),
 FOREIGN KEY(session_id,sequence) REFERENCES messages(session_id,sequence) ON DELETE CASCADE ON UPDATE CASCADE
 ) STRICT, WITHOUT ROWID`,
	)
}

func validateTranscriptDisplaySchema(ctx context.Context, conn *sql.Conn) error {
	if err := validateSchemaTable(ctx, conn, transcriptDisplayTable, transcriptDisplaySpec); err != nil {
		return fmt.Errorf("session database transcript display: %w", err)
	}
	if err := validateForeignKeys(ctx, conn, transcriptDisplayTable, map[string]bool{"session_id>messages.session_id:CASCADE": true, "sequence>messages.sequence:CASCADE": true}); err != nil {
		return err
	}
	var composite bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_list('transcript_display') GROUP BY id HAVING count(*)=2 AND group_concat("from",',')='session_id,sequence')`).Scan(&composite); err != nil {
		return err
	}
	if !composite {
		return schemaCorruption("transcript display lacks composite message foreign key")
	}
	return nil
}

// writeTranscriptDisplay uses the persisted representation, not caller-owned
// metadata types. Message and derived row are committed in the same transaction.
func writeTranscriptDisplay(ctx context.Context, conn *sql.Conn, id []byte, seq int64, payload []byte) error {
	var msg messages.ChatMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return err
	}
	turn, barrier, answerBarrier := int64(-1), int64(-1), int64(-1)
	if seq > 0 {
		if err := conn.QueryRowContext(ctx, `SELECT turn_sequence,barrier_sequence,answer_barrier_sequence FROM transcript_display WHERE session_id=? AND sequence=?`, id, seq-1).Scan(&turn, &barrier, &answerBarrier); err != nil {
			return fmt.Errorf("read previous display boundary: %w", err)
		}
	}
	realUser := msg.Role == messages.MessageRoleUser && !msg.IsAgentSynthetic()
	if realUser {
		turn = seq
	}
	fact, relevant := displayFact(msg)
	fact.Sequence, fact.Turn, fact.Barrier, fact.AnswerBarrier = seq, turn, barrier, answerBarrier
	factJSON := []byte(`{}`)
	var err error
	if relevant {
		factJSON, err = json.Marshal(fact)
		if err != nil {
			return err
		}
	}
	userJSON := []byte(`{}`)
	if realUser {
		userJSON, err = json.Marshal(displayUser(msg))
		if err != nil {
			return err
		}
	}
	if realUser || msg.Role == messages.MessageRoleAssistant && msg.ModelText() != "" {
		barrier = seq
	}
	if realUser || msg.Role == messages.MessageRoleAssistant && msg.GetContent() != "" {
		answerBarrier = seq
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO transcript_display(session_id,sequence,real_user,relevant,turn_sequence,barrier_sequence,answer_barrier_sequence,fact_json,user_json) VALUES(?,?,?,?,?,?,?,?,?)`, id, seq, realUser, relevant, turn, barrier, answerBarrier, factJSON, userJSON)
	return err
}

// Backfill runs only inside a version upgrade. Current opens and reads must not
// repair incomplete display state. Keyset batches bound retained payloads.
func backfillTranscriptDisplay(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `DELETE FROM transcript_display`); err != nil {
		return err
	}
	var lastID []byte
	var lastSeq int64
	for {
		type row struct {
			id  []byte
			seq int64
		}
		var batch []row
		query := `SELECT session_id,sequence FROM messages`
		var args []any
		if lastID != nil {
			query += ` WHERE (session_id,sequence)>(?,?)`
			args = []any{lastID, lastSeq}
		}
		query += ` ORDER BY session_id,sequence LIMIT 128`
		if err := queryEach(ctx, conn, query, args, func(rows *sql.Rows) error {
			var r row
			if err := rows.Scan(&r.id, &r.seq); err != nil {
				return err
			}
			batch = append(batch, r)
			return nil
		}); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, r := range batch {
			var payload []byte
			if err := conn.QueryRowContext(ctx, `SELECT payload_json FROM messages WHERE session_id=? AND sequence=?`, r.id, r.seq).Scan(&payload); err != nil {
				return err
			}
			if err := writeTranscriptDisplay(ctx, conn, r.id, r.seq, payload); err != nil {
				return err
			}
		}
		lastID, lastSeq = batch[len(batch)-1].id, batch[len(batch)-1].seq
	}
}
