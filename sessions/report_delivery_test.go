package sessions

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestReportInputCommitsWithConsumption(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	parent := acquireNamed(t, store, "parent")
	other := acquireNamed(t, store, "other")
	for _, name := range []string{"parent", "other"} {
		if err := store.PostReport(ctx, name, Report{Child: "helper", Status: ReportFinished, Text: "reply"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := parent.PeekReports(ctx)
	if err != nil || len(first) != 1 || first[0].ID <= 0 {
		t.Fatalf("peek = %v, %v", first, err)
	}
	otherReports, err := other.PeekReports(ctx)
	if err != nil || len(otherReports) != 1 {
		t.Fatal(err)
	}
	// Force failure after the message insert and report deletion. Both must
	// roll back, leaving the parent free to retry the same input.
	if err := store.withWrite(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `CREATE TRIGGER reject_report_input BEFORE UPDATE OF next_sequence ON sessions BEGIN SELECT RAISE(ABORT, 'test failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	input := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "agent helper finished\nreply"}
	ids := []int64{first[0].ID, otherReports[0].ID}
	if err := parent.AddReportMessage(ctx, input, ids); err == nil {
		t.Fatal("expected injected commit failure")
	}
	if history, err := parent.GetHistory(ctx); err != nil || len(history) != 0 {
		t.Fatalf("failed commit retained input: %v %v", history, err)
	}
	if reports, err := parent.PeekReports(ctx); err != nil || len(reports) != 1 || reports[0].ID != first[0].ID {
		t.Fatalf("failed commit lost report: %v %v", reports, err)
	}
	if err := store.withWrite(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "DROP TRIGGER reject_report_input")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := parent.AddReportMessage(ctx, input, ids); err != nil {
		t.Fatal(err)
	}
	if reports, err := parent.PeekReports(ctx); err != nil || len(reports) != 0 {
		t.Fatalf("committed report remains: %v %v", reports, err)
	}
	if reports, err := other.PeekReports(ctx); err != nil || len(reports) != 1 {
		t.Fatalf("another parent's report was consumed: %v %v", reports, err)
	}
	if history, err := parent.GetHistory(ctx); err != nil || len(history) != 1 || history[0].Content != input.Content {
		t.Fatalf("committed input = %v %v", history, err)
	}
}

func TestEmptyReportPollDoesNotAcquireADatabaseWriter(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if reports, err := parent.PeekReports(ctx); err != nil || len(reports) != 0 {
		t.Fatalf("empty poll waited for the database writer: %v %v", reports, err)
	}
}
