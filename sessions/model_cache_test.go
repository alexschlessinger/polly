package sessions

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestSharedModelCacheSurvivesRestartWithoutSessions(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t, ModeDisk, nil, 0)
	if err := store.PutModelCache(ctx, "scope", []byte(`{"models":[],"secret":false}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	raw, err := reopened.GetModelCache(ctx, "scope")
	if err != nil || len(raw) == 0 {
		t.Fatalf("cache lost: %s %v", raw, err)
	}
	if _, err := reopened.GetModelCache(ctx, "other-scope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("scope leak: %v", err)
	}
	memory, _ := openTestStore(t, ModeMemory, nil, 0)
	if _, err := memory.GetModelCache(ctx, "scope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("memory store read disk cache")
	}
	if err := memory.PutModelCache(ctx, "scope", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	other, _ := openTestStore(t, ModeMemory, nil, 0)
	if _, err := other.GetModelCache(ctx, "scope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("memory stores share data")
	}
}
