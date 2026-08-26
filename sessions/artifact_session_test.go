package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestFileSessionArtifactNamespaceSurvivesRenameAndCleansUp(t *testing.T) {
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
	artifactStore, err := session.ArtifactStore()
	if err != nil {
		t.Fatal(err)
	}
	first, err := artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("before rename")})
	if err != nil {
		t.Fatal(err)
	}
	namespace := session.ArtifactNamespace
	root := artifactNamespacePath(baseDir, namespace)
	if !validArtifactNamespace(namespace) {
		t.Fatalf("artifact namespace = %q", namespace)
	}

	if err := session.Rename("after"); err != nil {
		t.Fatal(err)
	}
	if session.ArtifactNamespace != namespace {
		t.Fatalf("rename changed namespace from %q to %q", namespace, session.ArtifactNamespace)
	}
	if _, err := artifactStore.Open(context.Background(), first.ID); err != nil {
		t.Fatalf("renamed session lost artifact: %v", err)
	}

	if err := session.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact namespace after clear: %v", err)
	}
	second, err := artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("after clear")})
	if err != nil {
		t.Fatalf("artifact store did not recreate cleared namespace: %v", err)
	}
	if _, err := artifactStore.Open(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}

	session.Close()
	store.Delete("after")
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact namespace after delete: %v", err)
	}
}

func TestFileSessionExpiryRemovesArtifactNamespace(t *testing.T) {
	baseDir := t.TempDir()
	storeInterface, err := NewFileSessionStore(baseDir, &Metadata{TTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	store := storeInterface.(*FileSessionStore)
	sessionInterface, err := store.Get("expiring")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionInterface.(*FileSession)
	artifactStore, err := session.ArtifactStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindBinary, Data: []byte("expire me")}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(baseDir, ".artifacts", session.ArtifactNamespace)
	session.Close()
	time.Sleep(time.Millisecond)

	store.Expire()
	if store.Exists("expiring") {
		t.Fatal("expired session still exists")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact namespace after expiry: %v", err)
	}
}

func TestLegacyFileSessionAcquiresArtifactNamespaceLazily(t *testing.T) {
	baseDir := t.TempDir()
	storeInterface, err := NewFileSessionStore(baseDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := storeInterface.(*FileSessionStore)
	sessionInterface, err := store.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionInterface.(*FileSession)
	if err := session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "keep me"}); err != nil {
		t.Fatal(err)
	}
	session.Close()

	path := filepath.Join(baseDir, "legacy.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "artifactNamespace")
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	reopenedInterface, err := store.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	reopened := reopenedInterface.(*FileSession)
	if reopened.ArtifactNamespace != "" || len(reopened.GetHistory()) != 1 || reopened.GetHistory()[0].Content != "keep me" {
		t.Fatalf("legacy session load = namespace %q history %#v", reopened.ArtifactNamespace, reopened.GetHistory())
	}
	if _, err := reopened.ArtifactStore(); err != nil {
		t.Fatal(err)
	}
	if !validArtifactNamespace(reopened.ArtifactNamespace) || len(reopened.GetHistory()) != 1 {
		t.Fatalf("lazy migration = namespace %q history %#v", reopened.ArtifactNamespace, reopened.GetHistory())
	}
	reopened.Close()
}

func TestLocalSessionClearDeleteAndExpiryRemoveArtifacts(t *testing.T) {
	store := NewSyncMapSessionStore(nil).(*SyncMapSessionStore)
	sessionInterface, err := store.Get("local")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionInterface.(*LocalSession)
	artifactStore, err := session.ArtifactStore()
	if err != nil {
		t.Fatal(err)
	}
	first, err := artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("first")})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactStore.Open(context.Background(), first.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact after clear = %v", err)
	}

	second, err := artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("second")})
	if err != nil {
		t.Fatal(err)
	}
	store.Delete("local")
	if _, err := artifactStore.Open(context.Background(), second.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact after delete = %v", err)
	}

	expiringInterface, err := store.Get("expiring-local")
	if err != nil {
		t.Fatal(err)
	}
	expiring := expiringInterface.(*LocalSession)
	expiringStore, err := expiring.ArtifactStore()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := expiringStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("third")})
	if err != nil {
		t.Fatal(err)
	}
	expiring.mu.Lock()
	expiring.metadata.TTL = time.Nanosecond
	expiring.last = time.Now().Add(-time.Hour)
	expiring.mu.Unlock()
	time.Sleep(time.Millisecond)
	store.Expire()
	if _, err := expiringStore.Open(context.Background(), ref.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact after expiry = %v", err)
	}
}

func TestFileSessionClearCommitsEvenWhenArtifactCleanupFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	baseDir := t.TempDir()
	storeInterface, err := NewFileSessionStore(baseDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionInterface, err := storeInterface.Get("stuck-artifacts")
	if err != nil {
		t.Fatal(err)
	}
	session := sessionInterface.(*FileSession)
	artifactStore, err := session.ArtifactStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("stuck blob")}); err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "clear me"}); err != nil {
		t.Fatal(err)
	}
	root := artifactNamespacePath(baseDir, session.ArtifactNamespace)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	// The committed clear is what callers act on; an artifact-cleanup failure
	// must not be reported as "the reset did not happen".
	if err := session.Clear(); err != nil {
		t.Fatalf("Clear = %v with history already committed empty", err)
	}
	if got := session.GetHistory(); len(got) != 0 {
		t.Fatalf("history after clear = %#v", got)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("cleanup unexpectedly succeeded; this test no longer exercises the failure path: %v", err)
	}
	session.Close()

	reopened, err := storeInterface.Get("stuck-artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.GetHistory(); len(got) != 0 {
		t.Fatalf("durable history after clear = %#v", got)
	}
	reopened.Close()
}
