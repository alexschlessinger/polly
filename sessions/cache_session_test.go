package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileCacheSessionIDLifecycle(t *testing.T) {
	baseDir := t.TempDir()
	storeInterface, err := NewFileSessionStore(baseDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := storeInterface.(*FileSessionStore)

	sessionInterface, err := store.Get("before")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionInterface.(*FileSession)
	first, err := session.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	requireOpaqueCacheSessionID(t, first)

	if err := session.Clear(); err != nil {
		t.Fatal(err)
	}
	afterClear, err := session.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if afterClear != first {
		t.Fatalf("clear changed cache session ID from %q to %q", first, afterClear)
	}

	if err := session.Rename("after"); err != nil {
		t.Fatal(err)
	}
	afterRename, err := session.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if afterRename != first {
		t.Fatalf("rename changed cache session ID from %q to %q", first, afterRename)
	}

	session.Close()
	reopenedInterface, err := store.Get("after")
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenedInterface.(*FileSession)
	afterReload, err := reopened.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if afterReload != first {
		t.Fatalf("reload changed cache session ID from %q to %q", first, afterReload)
	}

	reopened.Close()
	store.Delete("after")
	recreatedInterface, err := store.Get("after")
	if err != nil {
		t.Fatal(err)
	}
	recreated := recreatedInterface.(*FileSession)
	recreatedID, err := recreated.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if recreatedID == first {
		t.Fatal("delete and recreation reused the old cache session ID")
	}
	recreated.Close()
}

func TestLegacyFileCacheSessionIDMigratesAndPersists(t *testing.T) {
	baseDir := t.TempDir()
	storeInterface, err := NewFileSessionStore(baseDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := storeInterface.(*FileSessionStore)
	sessionInterface, err := store.Get("legacy-cache")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionInterface.(*FileSession)
	session.Close()

	path := filepath.Join(baseDir, "legacy-cache.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	delete(stored, "artifactNamespace")
	data, err = json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	legacyInterface, err := store.Get("legacy-cache")
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyInterface.(*FileSession)
	if legacy.ArtifactNamespace != "" {
		t.Fatalf("legacy fixture unexpectedly has namespace %q", legacy.ArtifactNamespace)
	}
	id, err := legacy.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	requireOpaqueCacheSessionID(t, id)
	if !validArtifactNamespace(legacy.ArtifactNamespace) {
		t.Fatalf("lazy cache identity did not persist artifact namespace %q", legacy.ArtifactNamespace)
	}
	legacy.Close()

	reopenedInterface, err := store.Get("legacy-cache")
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenedInterface.(*FileSession)
	reloadedID, err := reopened.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if reloadedID != id {
		t.Fatalf("legacy cache ID changed after reload from %q to %q", id, reloadedID)
	}
	reopened.Close()
}

func TestCacheSessionIDConcurrentAccess(t *testing.T) {
	store := NewSyncMapSessionStore(nil)
	sessionInterface, err := store.Get("concurrent")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionInterface.(*LocalSession)

	const workers = 64
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := session.CacheSessionID()
			ids <- id
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for id := range ids {
		requireOpaqueCacheSessionID(t, id)
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("concurrent cache IDs differ: %q and %q", first, id)
		}
	}

	if err := session.Clear(); err != nil {
		t.Fatal(err)
	}
	afterClear, err := session.CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if afterClear != first {
		t.Fatalf("local clear changed cache session ID from %q to %q", first, afterClear)
	}
	store.Delete("concurrent")
	recreatedInterface, err := store.Get("concurrent")
	if err != nil {
		t.Fatal(err)
	}
	recreatedID, err := recreatedInterface.(CacheSession).CacheSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if recreatedID == first {
		t.Fatal("local delete and recreation reused cache session ID")
	}
}

func requireOpaqueCacheSessionID(t *testing.T, id string) {
	t.Helper()
	if len(id) != 64 {
		t.Fatalf("cache session ID length = %d, want 64: %q", len(id), id)
	}
	for _, char := range id {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			t.Fatalf("cache session ID is not lowercase hex: %q", id)
		}
	}
}
