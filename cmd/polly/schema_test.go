package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSchemaFileRejectsInvalidSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"type":"made_up"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadSchemaFile(path)
	if err == nil {
		t.Fatalf("expected error for unknown schema type")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "made_up") {
		t.Fatalf("error should name the file and the bad type: %v", err)
	}
}

func TestLoadSchemaFileAcceptsValidSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.json")
	if err := os.WriteFile(path, []byte(`{"type":"object","properties":{"a":{"type":"string"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := loadSchemaFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || !s.Strict || s.Raw["type"] != "object" {
		t.Fatalf("unexpected schema: %+v", s)
	}
}
