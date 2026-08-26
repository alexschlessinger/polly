package main

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func testArtifactStore(t *testing.T) artifacts.Store {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	return testAcquireSession(t, store, "artifacts").ArtifactStore()
}

func testOpenMemoryStore(t *testing.T, metadata *sessions.Metadata) sessions.SessionStore {
	t.Helper()
	store, err := sessions.OpenStore(sessions.StoreConfig{
		Mode:            sessions.ModeMemory,
		DefaultMetadata: metadata,
	})
	if err != nil {
		t.Fatalf("open memory session store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testOpenDiskStore(t *testing.T, path string, metadata *sessions.Metadata) sessions.SessionStore {
	t.Helper()
	store, err := sessions.OpenStore(sessions.StoreConfig{
		Mode:            sessions.ModeDisk,
		Path:            path,
		DefaultMetadata: metadata,
	})
	if err != nil {
		t.Fatalf("open disk session store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testAcquireSession(t *testing.T, store sessions.SessionStore, name string) sessions.Session {
	t.Helper()
	session, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{})
	if err != nil {
		t.Fatalf("acquire session %q: %v", name, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func testAcquireAutoSession(t *testing.T, store sessions.SessionStore, name string) sessions.Session {
	t.Helper()
	session, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{Auto: true})
	if err != nil {
		t.Fatalf("acquire auto session %q: %v", name, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func testSessionHistory(t *testing.T, session sessions.Session) []messages.ChatMessage {
	t.Helper()
	history, err := session.GetHistory(context.Background())
	if err != nil {
		t.Fatalf("get session history: %v", err)
	}
	return history
}

func testAddMessage(t *testing.T, session sessions.Session, message messages.ChatMessage) {
	t.Helper()
	if err := session.AddMessage(context.Background(), message); err != nil {
		t.Fatalf("add message: %v", err)
	}
}

func testAddMessages(t *testing.T, session sessions.Session, message []messages.ChatMessage) {
	t.Helper()
	if err := session.AddMessages(context.Background(), message); err != nil {
		t.Fatalf("add messages: %v", err)
	}
}

func testStoreExists(t *testing.T, store sessions.SessionStore, name string) bool {
	t.Helper()
	exists, err := store.Exists(context.Background(), name)
	if err != nil {
		t.Fatalf("check session %q existence: %v", name, err)
	}
	return exists
}
