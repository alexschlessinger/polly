package sessions

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestV5UpgradePreservesReportsAndAcceptsPauses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v5.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		t.Fatal(err)
	}
	for _, migration := range []func(context.Context, *sql.Conn) error{applySchemaV1, applySchemaV2, applySchemaV3, applySchemaV4, applySchemaV5} {
		if err := migration(ctx, conn); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"parent", "child"} {
		if _, err := conn.ExecContext(ctx, `INSERT INTO sessions(id,name,retention,created_ns,updated_ns,settings_json) VALUES(randomblob(16),?,'named',1,1,?)`, name, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO session_reports(id,session_id,child_id,child,status,body,error,input_tokens,output_tokens,posted_ns)
	 SELECT 42,p.id,c.id,'child','failed','partial finding','old error',7,3,1 FROM sessions p,sessions c WHERE p.name='parent' AND c.name='child'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO swarm_records(parent_id,kind,id,payload_json) SELECT id,'run','saved',? FROM sessions WHERE name='parent'`, []byte(`{"id":"saved","starts":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	reports, err := parent.PeekReports(ctx)
	if err != nil || len(reports) != 1 || reports[0].ID != 42 || reports[0].Text != "partial finding" || reports[0].Error != "old error" || reports[0].InputTokens != 7 || reports[0].OutputTokens != 3 || reports[0].Status != ReportFailed {
		t.Fatalf("migration changed pending report: %+v %v", reports, err)
	}
	state, err := parent.(CoordinationSession).ReadCoordination(ctx)
	if err != nil || string(state.Records["run"]["saved"]) != `{"id":"saved","starts":1}` {
		t.Fatalf("migration changed swarm records: %+v %v", state, err)
	}
	if err := store.PostReport(ctx, "parent", Report{Child: "child", Status: ReportPaused, Text: "new partial", Error: "iteration limit"}); err != nil {
		t.Fatal(err)
	}
	if err := parent.AddReportMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "delivered old reply"}, []int64{42}); err != nil {
		t.Fatal(err)
	}
	reports, err = parent.PeekReports(ctx)
	if err != nil || len(reports) != 1 || reports[0].Status != ReportPaused {
		t.Fatalf("delivery receipt changed during migration: %+v %v", reports, err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE name='child'`); err != nil {
		t.Fatal(err)
	}
	var detached int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM session_reports WHERE child_id IS NULL`).Scan(&detached); err != nil || detached != 1 {
		t.Fatalf("child deletion lost reports: %d %v", detached, err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE name='parent'`); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM session_reports`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("parent report retention changed: %d %v", remaining, err)
	}
}
