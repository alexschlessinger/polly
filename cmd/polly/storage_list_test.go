package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func TestStoreIntegrityChecksByCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	config, _ := parseStorageTestConfig(t)
	store, err := setupSessionStore(config, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, seed := range []struct{ name, parent string }{{"parent", ""}, {"child", "parent"}} {
		session, err := store.Acquire(context.Background(), seed.name, sessions.AcquireOptions{Parent: seed.parent})
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := defaultStorePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	// Only quick_check detects this otherwise-unread CHECK violation.
	for _, query := range []string{
		"PRAGMA ignore_check_constraints = ON",
		`INSERT INTO artifact_blobs(digest,byte_count,chunk_count,created_ns)
		 VALUES(zeroblob(32),-1,0,0)`,
	} {
		if _, err := raw.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		args []string
		list bool
	}{
		{"conversation", nil, false},
		{"list", []string{"--list"}, true},
		{"flat list", []string{"--list", "--flat"}, true},
		{"show", []string{"--show", "parent"}, false},
		{"export", []string{"--export", "parent"}, false},
		{"create", []string{"--create", "another"}, false},
		{"delete", []string{"--delete", "parent"}, false},
		{"reset", []string{"--reset", "parent"}, false},
		{"purge", []string{"--purge"}, false},
		{"add", []string{"--add"}, false},
		{"list with fixture", []string{"--list", "--shot-script", "-", "--shot-fixture", "fixture.json"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config, cmd := parseStorageTestConfig(t, tt.args...)
			store, err := setupSessionStore(config, "", true)
			if err == nil {
				defer store.Close()
			}
			if tt.args == nil {
				// A conversation opens despite the corruption and schedules
				// the check instead.
				if err != nil {
					t.Fatalf("open = %v, want the integrity check deferred", err)
				}
				status, err := store.(*sessions.SQLiteStore).MaintenanceStatus(context.Background())
				if err != nil || status.IntegrityInterval != conversationIntegrityInterval {
					t.Fatalf("maintenance status = %+v, %v", status, err)
				}
				return
			}
			if !tt.list {
				if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
					t.Fatalf("open = %v, want integrity check failure", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			output := captureStdout(t, func() {
				if err := handleListContexts(context.Background(), store, cmd.Bool("flat")); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.HasPrefix(output, "parent [") || !strings.Contains(output, "child") {
				t.Fatalf("missing saved sessions: %q", output)
			}
			if cmd.Bool("flat") {
				if strings.Contains(output, "↳") || !strings.Contains(output, "\nchild [") || !strings.Contains(output, "(spawned by parent)") {
					t.Fatalf("flat list = %q", output)
				}
			} else if !strings.Contains(output, "\n  ↳ child [") {
				t.Fatalf("nested list = %q", output)
			}
		})
	}
}
