package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func openTestStore(t *testing.T, mode StoreMode, defaults *Metadata, autoTTL time.Duration) (*SQLiteStore, string) {
	t.Helper()
	config := StoreConfig{Mode: mode, DefaultMetadata: defaults, AutoSessionTTL: autoTTL}
	path := ""
	if mode == ModeDisk {
		path = filepath.Join(t.TempDir(), "sessions", "polly.db")
		config.Path = path
	}
	store, err := OpenStore(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func acquireNamed(t *testing.T, store *SQLiteStore, name string) Session {
	t.Helper()
	session, err := store.Acquire(context.Background(), name, AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func readArtifact(t *testing.T, store artifacts.Store, id string) []byte {
	t.Helper()
	reader, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return data
}

func TestSQLiteSessionContract(t *testing.T) {
	for _, mode := range []StoreMode{ModeMemory, ModeDisk} {
		mode := mode
		t.Run(map[StoreMode]string{ModeMemory: "memory", ModeDisk: "disk"}[mode], func(t *testing.T) {
			defaults := &Metadata{SystemPrompt: "be useful", Model: "test-model", MaxHistoryTokens: 20}
			store, _ := openTestStore(t, mode, defaults, 7*24*time.Hour)
			ctx := context.Background()
			session := acquireNamed(t, store, "alpha")

			history, err := session.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 1 || history[0].Role != messages.MessageRoleSystem || history[0].Content != "be useful" {
				t.Fatalf("initial history = %#v", history)
			}

			batch := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "hello", Metadata: map[string]any{"source": "test"}},
				{Role: messages.MessageRoleAssistant, Content: "hi", ToolCalls: []messages.ChatMessageToolCall{{ID: "call-1", Name: "clock", Arguments: "{}"}}},
				{Role: messages.MessageRoleTool, ToolCallID: "call-1", Content: "noon"},
			}
			if err := session.AddMessages(ctx, batch); err != nil {
				t.Fatal(err)
			}
			history, err = session.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 4 || history[1].Content != "hello" || history[3].Content != "noon" {
				t.Fatalf("history = %#v", history)
			}
			history[1].Content = "mutated"
			history[2].ToolCalls[0].Name = "mutated"
			history[1].Metadata["source"] = "mutated"
			again, err := session.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if again[1].Content != "hello" || again[2].ToolCalls[0].Name != "clock" || again[1].Metadata["source"] != "test" {
				t.Fatalf("history was not defensive: %#v", again)
			}

			metadata, err := session.GetMetadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Name != "alpha" || metadata.Model != "test-model" || metadata.Created.IsZero() || metadata.LastUsed.IsZero() {
				t.Fatalf("metadata = %+v", metadata)
			}
			metadata.Model = ""
			metadata.Temperature = 0
			metadata.MaxTokens = 0
			metadata.ActiveSkills = []string{}
			metadata.SystemPrompt = "replacement"
			if err := session.SetMetadata(ctx, metadata); err != nil {
				t.Fatal(err)
			}
			metadata.Model = "caller mutation"
			stored, err := session.GetMetadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Model != "" || stored.Temperature != 0 || stored.MaxTokens != 0 || stored.SystemPrompt != "replacement" {
				t.Fatalf("zero-valued metadata was not preserved: %+v", stored)
			}

			counts, err := session.GetMessageCounts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if counts["system"] != 1 || counts["user"] != 1 || counts["assistant"] != 1 || counts["tool"] != 1 {
				t.Fatalf("message counts = %#v", counts)
			}
			if calls, err := session.GetToolCallCount(ctx); err != nil || calls != 1 {
				t.Fatalf("tool calls = %d, %v", calls, err)
			}
			if total, err := session.GetTotalTokens(ctx); err != nil || total <= 0 {
				t.Fatalf("total tokens = %d, %v", total, err)
			}
			if capacity, err := session.GetCapacityPercentage(ctx); err != nil || capacity <= 0 {
				t.Fatalf("capacity = %v, %v", capacity, err)
			}

			if err := session.Clear(ctx); err != nil {
				t.Fatal(err)
			}
			history, err = session.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 1 || history[0].Content != "replacement" {
				t.Fatalf("cleared history = %#v", history)
			}

			names, err := store.List(ctx)
			if err != nil || !reflect.DeepEqual(names, []string{"alpha"}) {
				t.Fatalf("list = %#v, %v", names, err)
			}
			if exists, err := store.Exists(ctx, "alpha"); err != nil || !exists {
				t.Fatalf("exists = %v, %v", exists, err)
			}
			all, err := store.GetAllMetadata(ctx)
			if err != nil || all["alpha"].SystemPrompt != "replacement" {
				t.Fatalf("all metadata = %#v, %v", all, err)
			}
			summaries, err := store.ListSummaries(ctx)
			if err != nil || len(summaries) != 1 || summaries[0].Metadata.Name != "alpha" || summaries[0].MessageCount != 1 {
				t.Fatalf("session summaries = %#v, %v", summaries, err)
			}
			if last, err := store.GetLast(ctx); err != nil || last != "alpha" {
				t.Fatalf("last = %q, %v", last, err)
			}
		})
	}
}

func TestDiskStoreReopensWithoutTouchingLegacyContexts(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, "contexts", "legacy.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"history":["leave me"]}`)
	if err := os.WriteFile(legacyPath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "polly.db")
	config := StoreConfig{Mode: ModeDisk, Path: dbPath, DefaultMetadata: &Metadata{SystemPrompt: "sys"}}
	store, err := OpenStore(config)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Acquire(context.Background(), "durable", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "persisted"}); err != nil {
		t.Fatal(err)
	}
	cacheID, err := session.CacheSessionID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Acquire(context.Background(), "durable", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	history, err := again.GetHistory(context.Background())
	if err != nil || len(history) != 2 || history[1].Content != "persisted" {
		t.Fatalf("reopened history = %#v, %v", history, err)
	}
	reopenedCacheID, err := again.CacheSessionID(context.Background())
	if err != nil || reopenedCacheID != cacheID {
		t.Fatalf("cache ID after reopen = %q, %v; want %q", reopenedCacheID, err, cacheID)
	}
	gotLegacy, err := os.ReadFile(legacyPath)
	if err != nil || !reflect.DeepEqual(gotLegacy, legacy) {
		t.Fatalf("legacy context changed: %q, %v", gotLegacy, err)
	}
}

func TestSessionRenameIdentityCollisionAndDeleteRecreate(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, time.Hour)
	ctx := context.Background()
	first := acquireNamed(t, store, "first")
	second := acquireNamed(t, store, "taken")
	_ = second
	before, err := first.CacheSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Rename(ctx, "taken"); err == nil {
		t.Fatal("rename collision succeeded")
	}
	if err := first.Rename(ctx, "bad/name"); err == nil {
		t.Fatal("invalid rename succeeded")
	}
	if err := first.Rename(ctx, "keeper"); err != nil {
		t.Fatal(err)
	}
	after, err := first.CacheSessionID(ctx)
	if err != nil || after != before {
		t.Fatalf("rename changed cache identity: %q -> %q (%v)", before, after, err)
	}
	if name, err := first.GetName(ctx); err != nil || name != "keeper" {
		t.Fatalf("renamed name = %q, %v", name, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "keeper"); err != nil {
		t.Fatal(err)
	}
	recreated, err := store.Acquire(ctx, "keeper", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer recreated.Close()
	recreatedID, err := recreated.CacheSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recreatedID == before {
		t.Fatal("delete/recreate retained immutable identity")
	}
}

func TestAddMessagesIsAtomicOnEncodingAndDatabaseFailure(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "atomic")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "kept"}); err != nil {
		t.Fatal(err)
	}
	bad := messages.ChatMessage{Role: messages.MessageRoleAssistant, Metadata: map[string]any{"bad": make(chan int)}}
	if err := session.AddMessages(ctx, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "lost"}, bad}); err == nil {
		t.Fatal("unencodable batch succeeded")
	}
	history, err := session.GetHistory(ctx)
	if err != nil || len(history) != 1 || history[0].Content != "kept" {
		t.Fatalf("history after encoding failure = %#v, %v", history, err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER reject_message BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessages(ctx, []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, Content: "one"},
		{Role: messages.MessageRoleAssistant, Content: "two"},
	}); err == nil {
		t.Fatal("injected database failure succeeded")
	}
	history, err = session.GetHistory(ctx)
	if err != nil || len(history) != 1 || history[0].Content != "kept" {
		t.Fatalf("history after database failure = %#v, %v", history, err)
	}
}

func TestSQLiteArtifactsChunkDeduplicateIsolateAndCollect(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	first := acquireNamed(t, store, "first")
	second := acquireNamed(t, store, "second")
	firstStore := first.ArtifactStore()
	secondStore := second.ArtifactStore()

	values := [][]byte{
		{},
		[]byte("small"),
		[]byte(strings.Repeat("x", artifactChunkSize+17)),
		[]byte(strings.Repeat("y", 2*artifactChunkSize+31)),
	}
	for _, value := range values {
		ref, err := firstStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: value})
		if err != nil {
			t.Fatal(err)
		}
		if got := readArtifact(t, firstStore, ref.ID); !reflect.DeepEqual(got, value) {
			t.Fatalf("artifact round trip: got %d bytes, want %d", len(got), len(value))
		}
	}

	shared := []byte(strings.Repeat("shared", 200_000))
	firstRef, err := firstStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: shared})
	if err != nil {
		t.Fatal(err)
	}
	secondRef, err := secondStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: shared})
	if err != nil {
		t.Fatal(err)
	}
	if firstRef.ID != secondRef.ID {
		t.Fatalf("deduplicated IDs differ: %q != %q", firstRef.ID, secondRef.ID)
	}
	var sharedRows int
	if err := store.db.QueryRowContext(ctx,
		"SELECT count(*) FROM artifact_blobs WHERE digest = ?", mustDigest(t, firstRef.ID)).Scan(&sharedRows); err != nil || sharedRows != 1 {
		t.Fatalf("shared blob rows = %d, %v", sharedRows, err)
	}

	private, err := firstStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("private")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondStore.Open(ctx, private.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-session open = %v", err)
	}

	if err := first.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := firstStore.Open(ctx, firstRef.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first open after clear = %v", err)
	}
	if got := readArtifact(t, secondStore, secondRef.ID); !reflect.DeepEqual(got, shared) {
		t.Fatal("shared artifact did not survive first session clear")
	}
	if err := secondStore.RemoveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx,
		"SELECT count(*) FROM artifact_blobs WHERE digest = ?", mustDigest(t, secondRef.ID)).Scan(&sharedRows); err != nil || sharedRows != 0 {
		t.Fatalf("orphaned shared blob rows = %d, %v", sharedRows, err)
	}
}

func TestArtifactReaderCancellationEarlyCloseAndCorruption(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "artifacts")
	artifactStore := session.ArtifactStore()
	ctx := context.Background()
	ref, err := artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: []byte(strings.Repeat("z", artifactChunkSize+10))})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := artifactStore.Open(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 11)
	if n, err := reader.Read(buf); err != nil || n != len(buf) {
		t.Fatalf("early read = %d, %v", n, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(buf); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after close = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := artifactStore.Put(canceled, artifacts.Blob{Data: []byte("nope")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Put = %v", err)
	}
	if _, err := artifactStore.Open(canceled, ref.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Open = %v", err)
	}

	digest := mustDigest(t, ref.ID)
	if _, err := store.db.ExecContext(ctx, `
		UPDATE artifact_chunks SET data = zeroblob(length(data))
		WHERE digest = ? AND chunk_index = 0`, digest); err != nil {
		t.Fatal(err)
	}
	corrupt, err := artifactStore.Open(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(corrupt)
	_ = corrupt.Close()
	if !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("corrupt read = %v", err)
	}
	if _, err := artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: []byte(strings.Repeat("z", artifactChunkSize+10))}); !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("duplicate Put over corruption = %v", err)
	}

	extraRef, err := artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: []byte("extra-chunk-target")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO artifact_chunks(digest,chunk_index,data) VALUES(?,?,?)`,
		mustDigest(t, extraRef.ID), 1, []byte("trailing")); err != nil {
		t.Fatal(err)
	}
	extraReader, err := artifactStore.Open(ctx, extraRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(extraReader)
	_ = extraReader.Close()
	if !errors.Is(err, ErrArtifactCorrupt) {
		t.Fatalf("extra trailing chunk read = %v", err)
	}
}

func TestArtifactQueuedResetCanReattachMaterializedBytes(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "queued")
	artifactStore := session.ArtifactStore()
	ctx := context.Background()
	original := []byte("queued image bytes")
	ref, err := artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: original})
	if err != nil {
		t.Fatal(err)
	}
	materialized := append([]byte(nil), readArtifact(t, artifactStore, ref.ID)...)
	if err := session.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactStore.Open(ctx, ref.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleared queued artifact = %v", err)
	}
	restored, err := artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: materialized})
	if err != nil || restored.ID != ref.ID {
		t.Fatalf("restored ref = %+v, %v", restored, err)
	}
}

func mustDigest(t *testing.T, id string) []byte {
	t.Helper()
	digest, err := artifactDigest(id)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestLeaseContentionTakeoverAndTypedLoss(t *testing.T) {
	oldHeartbeat, oldStale, oldTimeout, oldRetry := leaseHeartbeatInterval, leaseStaleAfter, leaseAcquireTimeout, leaseRetryInterval
	leaseHeartbeatInterval = time.Hour
	leaseStaleAfter = time.Second
	leaseAcquireTimeout = 5 * time.Second
	leaseRetryInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		leaseHeartbeatInterval, leaseStaleAfter = oldHeartbeat, oldStale
		leaseAcquireTimeout, leaseRetryInterval = oldTimeout, oldRetry
	})

	dbPath := filepath.Join(t.TempDir(), "polly.db")
	config := StoreConfig{Mode: ModeDisk, Path: dbPath}
	firstStore, err := OpenStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := OpenStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	ctx := context.Background()
	first, err := firstStore.Acquire(ctx, "shared", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	artifactRef, err := first.ArtifactStore().Put(ctx, artifacts.Blob{Data: []byte("leased bytes")})
	if err != nil {
		t.Fatal(err)
	}
	leasedReader, err := first.ArtifactStore().Open(ctx, artifactRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer leasedReader.Close()
	// Keep the intentionally contended acquire quick without imposing the same
	// tiny budget on fresh-database setup, which is much slower under -race.
	leaseAcquireTimeout = 60 * time.Millisecond
	if _, err := secondStore.Acquire(ctx, "shared", AcquireOptions{}); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("contended acquire = %v", err)
	}
	if err := secondStore.Delete(ctx, "shared"); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("delete active session = %v", err)
	}

	// The contended acquire above needs the short budget; the takeover below
	// must instead survive a slow CI disk committing its write transaction.
	leaseAcquireTimeout = 5 * time.Second

	if _, err := firstStore.db.ExecContext(ctx,
		"UPDATE session_leases SET expires_ns = ? WHERE session_id = ?", time.Now().Add(-time.Second).UnixNano(), first.(*sqliteSession).id); err != nil {
		t.Fatal(err)
	}
	takeover, err := secondStore.Acquire(ctx, "shared", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer takeover.Close()
	if err := first.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "lost"}); !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("mutation after takeover = %v", err)
	}
	select {
	case <-first.Context().Done():
		if !errors.Is(context.Cause(first.Context()), ErrSessionLeaseLost) {
			t.Fatalf("lease context cause = %v", context.Cause(first.Context()))
		}
	default:
		t.Fatal("lease loss did not cancel session context")
	}
	if _, err := leasedReader.Read(make([]byte, 1)); !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("artifact read after lease loss = %v", err)
	}
}

func TestHeartbeatRenewsLease(t *testing.T) {
	oldHeartbeat, oldStale := leaseHeartbeatInterval, leaseStaleAfter
	leaseHeartbeatInterval = 10 * time.Millisecond
	leaseStaleAfter = 100 * time.Millisecond
	t.Cleanup(func() { leaseHeartbeatInterval, leaseStaleAfter = oldHeartbeat, oldStale })
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "heartbeat")
	concrete := session.(*sqliteSession)
	before := concrete.expiresNS.Load()
	deadline := time.Now().Add(time.Second)
	for concrete.expiresNS.Load() <= before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if concrete.expiresNS.Load() <= before {
		t.Fatal("lease heartbeat did not advance expiry")
	}
}

func TestHeartbeatConnectionStarvationDoesNotLoseOwnedLease(t *testing.T) {
	oldHeartbeat, oldStale := leaseHeartbeatInterval, leaseStaleAfter
	leaseHeartbeatInterval = 5 * time.Millisecond
	leaseStaleAfter = 20 * time.Millisecond
	t.Cleanup(func() { leaseHeartbeatInterval, leaseStaleAfter = oldHeartbeat, oldStale })
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "starved-heartbeat")
	concrete := session.(*sqliteSession)
	before := concrete.expiresNS.Load()

	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * leaseStaleAfter)
	select {
	case <-session.Context().Done():
		t.Fatalf("connection starvation canceled owned session: %v", context.Cause(session.Context()))
	default:
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for concrete.expiresNS.Load() <= before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if concrete.expiresNS.Load() <= before {
		t.Fatal("heartbeat did not renew after connection starvation")
	}
	if err := session.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "still owned"}); err != nil {
		t.Fatalf("mutation after connection starvation: %v", err)
	}
}

func TestHeartbeatDetectsTakeoverAndCancelsActiveTurnContext(t *testing.T) {
	oldHeartbeat, oldStale := leaseHeartbeatInterval, leaseStaleAfter
	leaseHeartbeatInterval = 10 * time.Millisecond
	leaseStaleAfter = 30 * time.Millisecond
	t.Cleanup(func() { leaseHeartbeatInterval, leaseStaleAfter = oldHeartbeat, oldStale })
	firstStore, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	first, err := firstStore.Acquire(ctx, "active-turn", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := firstStore.db.ExecContext(ctx,
		"UPDATE session_leases SET owner_token = ? WHERE session_id = ?", []byte("replacementowner"), first.(*sqliteSession).id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.Context().Done():
		if !errors.Is(context.Cause(first.Context()), ErrSessionLeaseLost) {
			t.Fatalf("active-turn cancellation cause = %v", context.Cause(first.Context()))
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not cancel active-turn context after takeover")
	}
}

func TestExpirySkipsLeaseAndCollectsAfterClose(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: time.Second}, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "expiring")
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Data: []byte("expire")})
	if err != nil {
		t.Fatal(err)
	}
	concrete := session.(*sqliteSession)
	if _, err := store.db.ExecContext(ctx,
		"UPDATE sessions SET updated_ns = ? WHERE id = ?", time.Now().Add(-time.Hour).UnixNano(), concrete.id); err != nil {
		t.Fatal(err)
	}
	if err := store.Expire(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, "expiring"); err != nil || !exists {
		t.Fatalf("active expired session exists = %v, %v", exists, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Expire(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, "expiring"); err != nil || exists {
		t.Fatalf("closed expired session exists = %v, %v", exists, err)
	}
	var blobs int
	if err := store.db.QueryRowContext(ctx,
		"SELECT count(*) FROM artifact_blobs WHERE digest = ?", mustDigest(t, ref.ID)).Scan(&blobs); err != nil || blobs != 0 {
		t.Fatalf("expired artifact blobs = %d, %v", blobs, err)
	}
}

func TestAcquireRetiresExpiredSession(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: time.Second, SystemPrompt: "seed"}, 0)
	ctx := context.Background()

	session, err := store.Acquire(ctx, "stale", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Data: []byte("stale")})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	firstID := append([]byte(nil), session.(*sqliteSession).id...)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		"UPDATE sessions SET updated_ns = ? WHERE id = ?", time.Now().Add(-time.Hour).UnixNano(), firstID); err != nil {
		t.Fatal(err)
	}

	reacquired := acquireNamed(t, store, "stale")
	history, err := reacquired.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Role != messages.MessageRoleSystem || history[0].Content != "seed" {
		t.Fatalf("retired session history = %+v", history)
	}
	if equalBytes(reacquired.(*sqliteSession).id, firstID) {
		t.Fatal("expired session kept its identity across reacquire")
	}
	var blobs int
	if err := store.db.QueryRowContext(ctx,
		"SELECT count(*) FROM artifact_blobs WHERE digest = ?", mustDigest(t, ref.ID)).Scan(&blobs); err != nil || blobs != 0 {
		t.Fatalf("retired session artifact blobs = %d, %v", blobs, err)
	}
}

func TestAcquireKeepsExpiredSessionWithLiveLease(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: time.Second}, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "held")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "keep me"}); err != nil {
		t.Fatal(err)
	}
	concrete := session.(*sqliteSession)
	if _, err := store.db.ExecContext(ctx,
		"UPDATE sessions SET updated_ns = ? WHERE id = ?", time.Now().Add(-time.Hour).UnixNano(), concrete.id); err != nil {
		t.Fatal(err)
	}
	owner, err := randomBytes(16)
	if err != nil {
		t.Fatal(err)
	}
	_, _, busy, err := store.tryAcquire(ctx, "held", AcquireOptions{}, owner)
	if err != nil || !busy {
		t.Fatalf("expired-but-leased acquire busy = %v, %v", busy, err)
	}
	history, err := session.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Content != "keep me" {
		t.Fatalf("held session history = %+v", history)
	}
}

func TestStoreConfigCleanupIntervalOverride(t *testing.T) {
	store, err := OpenStore(StoreConfig{Mode: ModeMemory, CleanupInterval: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.cleanup != 5*time.Minute {
		t.Fatalf("cleanup interval = %v", store.cleanup)
	}
}

func TestAutoRetentionMetadataRoundTripRenameAndEmptyClose(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 7*24*time.Hour)
	ctx := context.Background()
	auto, err := store.Acquire(ctx, "auto", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	concrete := auto.(*sqliteSession)
	before, err := auto.GetLastUsed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := auto.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.TTL != 7*24*time.Hour {
		t.Fatalf("auto TTL = %v", metadata.TTL)
	}
	time.Sleep(time.Millisecond)
	if err := auto.SetMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	afterNoop, err := auto.GetLastUsed(ctx)
	if err != nil || !afterNoop.Equal(before) {
		t.Fatalf("metadata no-op advanced timestamp: %v -> %v (%v)", before, afterNoop, err)
	}
	var explicit int
	if err := store.db.QueryRowContext(ctx,
		"SELECT ttl_explicit FROM sessions WHERE id = ?", concrete.id).Scan(&explicit); err != nil || explicit != 0 {
		t.Fatalf("implicit TTL became explicit: %d, %v", explicit, err)
	}
	if err := auto.Rename(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	renamed, err := auto.GetMetadata(ctx)
	if err != nil || renamed.TTL != 0 {
		t.Fatalf("renamed auto TTL = %v, %v", renamed.TTL, err)
	}
	if err := auto.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, "kept"); err != nil || !exists {
		t.Fatalf("renamed empty auto was deleted: %v, %v", exists, err)
	}

	unused, err := store.Acquire(ctx, "unused", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := unused.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, "unused"); err != nil || exists {
		t.Fatalf("unused auto exists = %v, %v", exists, err)
	}
}

func TestSameNameRenamePromotesAutoRetention(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 7*24*time.Hour)
	ctx := context.Background()
	session, err := store.Acquire(ctx, "generated-name", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	concrete := session.(*sqliteSession)
	if err := session.Rename(ctx, "generated-name"); err != nil {
		t.Fatal(err)
	}
	var retention string
	var ttlNS int64
	var ttlExplicit int
	if err := store.db.QueryRowContext(ctx, `
		SELECT retention,ttl_ns,ttl_explicit FROM sessions WHERE id = ?`, concrete.id).
		Scan(&retention, &ttlNS, &ttlExplicit); err != nil {
		t.Fatal(err)
	}
	if retention != retentionNamed || ttlNS != 0 || ttlExplicit != 0 {
		t.Fatalf("same-name rename state = retention %q, TTL %v, explicit %d", retention, time.Duration(ttlNS), ttlExplicit)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, "generated-name"); err != nil || !exists {
		t.Fatalf("same-name renamed session exists = %v, %v", exists, err)
	}
}

func TestAutoSessionWithDefaultTTLKeepsExplicitTTLOnRename(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: 2 * time.Hour}, 7*24*time.Hour)
	ctx := context.Background()
	session, err := store.Acquire(ctx, "auto-explicit", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Rename(ctx, "named-explicit"); err != nil {
		t.Fatal(err)
	}
	metadata, err := session.GetMetadata(ctx)
	if err != nil || metadata.TTL != 2*time.Hour {
		t.Fatalf("explicit TTL after rename = %v, %v", metadata.TTL, err)
	}
}

func TestAutoCloseUsesDurableTurnStateAcrossSystemPromptChanges(t *testing.T) {
	ctx := context.Background()

	withTurn, _ := openTestStore(t, ModeMemory, nil, time.Hour)
	used, err := withTurn.Acquire(ctx, "used", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := used.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "real turn"}); err != nil {
		t.Fatal(err)
	}
	metadata, err := used.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SystemPrompt = "added after the turn"
	if err := used.SetMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	if err := used.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, err := withTurn.Exists(ctx, "used"); err != nil || !exists {
		t.Fatalf("used auto session exists = %v, %v", exists, err)
	}

	withoutTurn, _ := openTestStore(t, ModeMemory, &Metadata{SystemPrompt: "creation prompt"}, time.Hour)
	unused, err := withoutTurn.Acquire(ctx, "unused-prompt", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = unused.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SystemPrompt = ""
	if err := unused.SetMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	if err := unused.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, err := withoutTurn.Exists(ctx, "unused-prompt"); err != nil || exists {
		t.Fatalf("unused auto session exists = %v, %v", exists, err)
	}
}

func TestAutoTurnMarkerSurvivesClearAndPreservesIdentity(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, time.Hour)
	ctx := context.Background()
	session, err := store.Acquire(ctx, "used-then-cleared", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	cacheID, err := session.CacheSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "a real turn"}); err != nil {
		t.Fatal(err)
	}
	if err := session.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, "used-then-cleared"); err != nil || !exists {
		t.Fatalf("cleared used auto session exists = %v, %v", exists, err)
	}
	reopened, err := store.Acquire(ctx, "used-then-cleared", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedID, err := reopened.CacheSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reopenedID != cacheID {
		t.Fatalf("cache identity changed after clear/reopen: %q != %q", reopenedID, cacheID)
	}

	unused, err := store.Acquire(ctx, "never-used", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := unused.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists(ctx, "never-used"); err != nil || exists {
		t.Fatalf("never-used auto session exists = %v, %v", exists, err)
	}
}

func TestStoreCloseCancelsSessionWithTypedCause(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "closing")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	<-session.Context().Done()
	if !errors.Is(context.Cause(session.Context()), ErrStoreClosed) {
		t.Fatalf("store-close context cause = %v", context.Cause(session.Context()))
	}
	if _, err := session.GetHistory(context.Background()); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("operation after store close = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestStoreCloseRacingAcquireReturnsStoreClosed(t *testing.T) {
	oldTimeout, oldRetry := leaseAcquireTimeout, leaseRetryInterval
	leaseAcquireTimeout = time.Second
	leaseRetryInterval = 5 * time.Millisecond
	t.Cleanup(func() { leaseAcquireTimeout, leaseRetryInterval = oldTimeout, oldRetry })
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	active := acquireNamed(t, store, "busy")
	_ = active
	result := make(chan error, 1)
	go func() {
		_, err := store.Acquire(context.Background(), "busy", AcquireOptions{})
		result <- err
	}()
	time.Sleep(15 * time.Millisecond)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("Acquire racing Close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not stop when store closed")
	}
}

func TestConcurrentSessionAndStoreCloseWaitForLeaseCleanup(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	session := acquireNamed(t, store, "close-race")
	concrete := session.(*sqliteSession)
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sessionResult := make(chan error, 1)
	go func() { sessionResult <- session.Close() }()
	deadline := time.Now().Add(time.Second)
	for !concrete.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !concrete.closed.Load() {
		t.Fatal("Session.Close did not begin")
	}
	storeResult := make(chan error, 1)
	go func() { storeResult <- store.Close() }()
	select {
	case err := <-storeResult:
		t.Fatalf("Store.Close returned before session cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-sessionResult; err != nil {
		t.Fatal(err)
	}
	if err := <-storeResult; err != nil {
		t.Fatal(err)
	}
	select {
	case <-concrete.closeDone:
	case <-time.After(time.Second):
		t.Fatal("full session close did not complete")
	}
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var leaseCount int
	if err := reopened.db.QueryRow("SELECT count(*) FROM session_leases").Scan(&leaseCount); err != nil || leaseCount != 0 {
		t.Fatalf("leases after concurrent close = %d, %v", leaseCount, err)
	}
}

func TestConcurrentDifferentSessions(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	left := acquireNamed(t, store, "left")
	right := acquireNamed(t, store, "right")
	const count = 30
	var wg sync.WaitGroup
	for index, session := range []Session{left, right} {
		index, session := index, session
		wg.Add(1)
		go func() {
			defer wg.Done()
			for messageIndex := 0; messageIndex < count; messageIndex++ {
				if err := session.AddMessage(ctx, messages.ChatMessage{
					Role: messages.MessageRoleUser, Content: strings.Repeat(string(rune('a'+index)), messageIndex+1),
				}); err != nil {
					t.Errorf("AddMessage: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, session := range []Session{left, right} {
		history, err := session.GetHistory(ctx)
		if err != nil || len(history) != count {
			t.Fatalf("concurrent history length = %d, %v", len(history), err)
		}
	}
}

func TestListSummariesReportLiveLeases(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	held := acquireNamed(t, store, "held")
	released := acquireNamed(t, store, "released")
	if err := released.Close(); err != nil {
		t.Fatal(err)
	}

	inUse := func() map[string]bool {
		t.Helper()
		summaries, err := store.ListSummaries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		result := make(map[string]bool, len(summaries))
		for _, summary := range summaries {
			result[summary.Metadata.Name] = summary.InUse
		}
		return result
	}
	if got := inUse(); !got["held"] || got["released"] {
		t.Fatalf("in-use summaries = %v, want held only", got)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if got := inUse(); got["held"] || got["released"] {
		t.Fatalf("in-use summaries after close = %v, want none", got)
	}
}

func TestConcurrentAppendsOneSessionRemainOrdered(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "one")
	ctx := context.Background()
	const count = 40
	var wg sync.WaitGroup
	for index := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := session.AddMessage(ctx, messages.ChatMessage{
				Role: messages.MessageRoleUser, Content: strings.Repeat("x", index+1),
			}); err != nil {
				t.Errorf("AddMessage: %v", err)
			}
		}()
	}
	wg.Wait()
	history, err := session.GetHistory(ctx)
	if err != nil || len(history) != count {
		t.Fatalf("history length = %d, %v", len(history), err)
	}
}

func TestSchemaConfigurationRejectionAndPermissions(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "nested", "polly.db")
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	var journalMode, quickCheck string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil || strings.ToLower(journalMode) != "wal" {
		t.Fatalf("journal_mode = %q, %v", journalMode, err)
	}
	if err := store.db.QueryRow("PRAGMA quick_check").Scan(&quickCheck); err != nil || quickCheck != "ok" {
		t.Fatalf("quick_check = %q, %v", quickCheck, err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(dbPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("database mode = %v, %v", info, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	futurePath := filepath.Join(root, "future.db")
	raw, err := sql.Open("sqlite", futurePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: futurePath}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future schema open = %v", err)
	}

	unversionedPath := filepath.Join(root, "unversioned.db")
	raw, err = sql.Open("sqlite", unversionedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE foreign_data(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: unversionedPath}); err == nil || !strings.Contains(err.Error(), "unversioned") {
		t.Fatalf("unversioned schema open = %v", err)
	}

	orphanPath := filepath.Join(root, "orphan.db")
	orphanStore, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: orphanPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := orphanStore.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err = sql.Open("sqlite", orphanPath+"?_foreign_keys=off")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		INSERT INTO messages(session_id,sequence,payload_json)
		VALUES(zeroblob(16),0,x'7b7d')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: orphanPath}); err == nil || !strings.Contains(err.Error(), "foreign-key") {
		t.Fatalf("foreign-key corrupt schema open = %v", err)
	}

	alteredPath := filepath.Join(root, "altered.db")
	alteredStore, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: alteredPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := alteredStore.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err = sql.Open("sqlite", alteredPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("ALTER TABLE sessions DROP COLUMN settings_json"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: alteredPath}); err == nil || !strings.Contains(err.Error(), "schema v1") {
		t.Fatalf("structurally altered schema open = %v", err)
	}

	wrongVacuumPath := filepath.Join(root, "wrong-vacuum.db")
	wrongVacuumStore, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: wrongVacuumPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongVacuumStore.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err = sql.Open("sqlite", wrongVacuumPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA auto_vacuum = NONE"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("VACUUM"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: wrongVacuumPath}); err == nil || !strings.Contains(err.Error(), "auto-vacuum") {
		t.Fatalf("wrong auto-vacuum database open = %v", err)
	}

	corruptPath := filepath.Join(root, "corrupt.db")
	original := []byte("not a sqlite database")
	if err := os.WriteFile(corruptPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: corruptPath}); err == nil {
		t.Fatal("corrupt database was silently opened")
	}
	got, err := os.ReadFile(corruptPath)
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("corrupt database was replaced: %q, %v", got, err)
	}
}

func TestDiskStoreProtectsLiveSQLiteFilesInExistingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	setPermissiveTestUmask(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "polly.db")
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := acquireNamed(t, store, "private")
	if err := session.AddMessage(context.Background(), messages.ChatMessage{
		Role: messages.MessageRoleUser, Content: "secret transcript",
	}); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("caller-owned directory mode = %v, %v; want 0755", info, err)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat live SQLite file %q: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("live SQLite file %q mode = %04o, want 0600", path, got)
		}
	}
	wal, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wal, []byte("secret transcript")) {
		t.Fatal("live WAL did not contain the transcript fixture")
	}
}

func TestDiskStoreRejectsNonRegularPathWithoutChangingIt(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "polly.db")
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// Windows reports writable directories as 0777 regardless of Chmod, so
	// compare against the observed pre-open mode instead of a literal.
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: dbPath}); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("directory database path open = %v", err)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != before.Mode().Perm() {
		t.Fatalf("rejected directory mode changed to %04o", got)
	}
}

func TestDiskStoreRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated privileges on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.db")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "polly.db")
	if err := os.Symlink(target, dbPath); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: dbPath}); err == nil || !strings.Contains(err.Error(), "symbolic-link") {
		t.Fatalf("symlink database path open = %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("symlink target mode changed to %04o", got)
	}
}

func TestDiskStoreProtectsOrRejectsExistingSidecarsBeforeOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits and symlinks are not reliable on Windows")
	}
	t.Run("permissive WAL", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "polly.db")
		walPath := dbPath + "-wal"
		if err := os.WriteFile(walPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := prepareDiskStore(dbPath); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(walPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("preexisting WAL mode = %04o, want 0600", got)
		}
	})

	t.Run("WAL symlink", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "polly.db")
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("unchanged"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dbPath+"-wal"); err != nil {
			t.Fatal(err)
		}
		_, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: dbPath})
		if err == nil || !strings.Contains(err.Error(), "symbolic-link") {
			t.Fatalf("OpenStore with WAL symlink error = %v", err)
		}
		contents, readErr := os.ReadFile(target)
		if readErr != nil || string(contents) != "unchanged" {
			t.Fatalf("sidecar symlink target changed: contents %q, error %v", contents, readErr)
		}
	})
}

func TestConcurrentFirstOpenSerializesSchemaMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "polly.db")
	const openers = 24
	start := make(chan struct{})
	errs := make(chan error, openers)
	var wg sync.WaitGroup
	for range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: dbPath})
			if err == nil {
				err = store.Close()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent first open: %v", err)
		}
	}

	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: dbPath})
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version after concurrent opens = %d, %v", version, err)
	}
}

func TestMemoryStoreRunsAutomaticExpiry(t *testing.T) {
	oldCleanupInterval := cleanupInterval
	cleanupInterval = 5 * time.Millisecond
	t.Cleanup(func() { cleanupInterval = oldCleanupInterval })

	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: time.Second}, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "memory-expiry")
	concrete := session.(*sqliteSession)
	if _, err := store.db.ExecContext(ctx,
		"UPDATE sessions SET updated_ns = ? WHERE id = ?", time.Now().Add(-time.Hour).UnixNano(), concrete.id); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		exists, err := store.Exists(ctx, "memory-expiry")
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("memory store did not automatically expire the session")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResetAtomicallyReplacesMetadataContentsAndArtifacts(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{
		SystemPrompt: "old system",
		Model:        "old-model",
	}, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "resettable")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "old turn"}); err != nil {
		t.Fatal(err)
	}
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Data: []byte("old artifact")})
	if err != nil {
		t.Fatal(err)
	}
	before, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &Metadata{
		Name:         "caller-controlled",
		Created:      time.Unix(1, 0),
		LastUsed:     time.Unix(2, 0),
		SystemPrompt: "new system",
		Model:        "new-model",
		TTL:          2 * time.Hour,
	}
	if err := session.Reset(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	after, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Windows wall-clock granularity can leave LastUsed equal to the prior
	// stamp, so only reject regressions and caller-controlled values.
	if after.Name != "resettable" || !after.Created.Equal(before.Created) ||
		after.LastUsed.Before(before.LastUsed) || after.LastUsed.Equal(replacement.LastUsed) {
		t.Fatalf("canonical metadata after reset = %+v; before %+v", after, before)
	}
	if after.SystemPrompt != "new system" || after.Model != "new-model" || after.TTL != 2*time.Hour {
		t.Fatalf("replacement metadata after reset = %+v", after)
	}
	history, err := session.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Role != messages.MessageRoleSystem || history[0].Content != "new system" {
		t.Fatalf("history after reset = %#v", history)
	}
	if _, err := session.ArtifactStore().Open(ctx, ref.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact after reset = %v", err)
	}
}

func TestResetRollsBackMetadataContentsAndArtifactsTogether(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{SystemPrompt: "old system", Model: "old-model"}, 0)
	ctx := context.Background()
	session := acquireNamed(t, store, "rollback")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "old turn"}); err != nil {
		t.Fatal(err)
	}
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Data: []byte("keep me")})
	if err != nil {
		t.Fatal(err)
	}
	beforeMetadata, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := session.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER reject_reset BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'blocked reset'); END`); err != nil {
		t.Fatal(err)
	}

	replacement := cloneMetadata(beforeMetadata)
	replacement.SystemPrompt = "new system"
	replacement.Model = "new-model"
	if err := session.Reset(ctx, replacement); err == nil || !strings.Contains(err.Error(), "blocked reset") {
		t.Fatalf("blocked reset error = %v", err)
	}
	afterMetadata, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterMetadata, beforeMetadata) {
		t.Fatalf("metadata changed after rolled-back reset: got %+v, want %+v", afterMetadata, beforeMetadata)
	}
	afterHistory, err := session.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterHistory, beforeHistory) {
		t.Fatalf("history changed after rolled-back reset: got %#v, want %#v", afterHistory, beforeHistory)
	}
	if got := readArtifact(t, session.ArtifactStore(), ref.ID); string(got) != "keep me" {
		t.Fatalf("artifact changed after rolled-back reset: %q", got)
	}
}

func TestSessionOperationsPreserveCallerCancellationCause(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "typed-cause")
	want := errors.New("typed caller cancellation")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(want)

	checks := []struct {
		name string
		run  func() error
	}{
		{name: "history", run: func() error { _, err := session.GetHistory(ctx); return err }},
		{name: "message", run: func() error {
			return session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "nope"})
		}},
		{name: "empty message batch", run: func() error { return session.AddMessages(ctx, nil) }},
		{name: "artifact put", run: func() error {
			_, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Data: []byte("nope")})
			return err
		}},
		{name: "artifact open", run: func() error {
			_, err := session.ArtifactStore().Open(ctx, strings.Repeat("0", 64))
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, want) {
				t.Fatalf("operation error = %v, want caller cause %v", err, want)
			}
		})
	}
}

func TestConnectionConfigurationIsSharedAcrossModes(t *testing.T) {
	for _, mode := range []StoreMode{ModeMemory, ModeDisk} {
		mode := mode
		t.Run(map[StoreMode]string{ModeMemory: "memory", ModeDisk: "disk"}[mode], func(t *testing.T) {
			store, _ := openTestStore(t, mode, nil, 0)
			for pragma, want := range map[string]int64{
				"foreign_keys":   1,
				"busy_timeout":   10_000,
				"synchronous":    2, // FULL
				"trusted_schema": 0,
			} {
				var got int64
				if err := store.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
					t.Fatalf("PRAGMA %s = %d, %v; want %d", pragma, got, err, want)
				}
			}

			var journal string
			if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
				t.Fatal(err)
			}
			wantJournal := "memory"
			if mode == ModeDisk {
				wantJournal = "wal"
				var autoVacuum, journalLimit int64
				if err := store.db.QueryRow("PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil || autoVacuum != 2 {
					t.Fatalf("PRAGMA auto_vacuum = %d, %v; want incremental", autoVacuum, err)
				}
				if err := store.db.QueryRow("PRAGMA journal_size_limit").Scan(&journalLimit); err != nil || journalLimit != journalSizeLimit {
					t.Fatalf("PRAGMA journal_size_limit = %d, %v; want %d", journalLimit, err, journalSizeLimit)
				}
			}
			if strings.ToLower(journal) != wantJournal {
				t.Fatalf("journal mode = %q, want %q", journal, wantJournal)
			}
			stats := store.db.Stats()
			if stats.MaxOpenConnections != 1 || stats.OpenConnections != 1 || stats.Idle != 1 {
				t.Fatalf("connection pool stats = %+v; want one open idle connection", stats)
			}
		})
	}

	first, _ := openTestStore(t, ModeMemory, nil, 0)
	second, _ := openTestStore(t, ModeMemory, nil, 0)
	if first.path == "" || first.path == second.path {
		t.Fatalf("memory database names are not unique: %q and %q", first.path, second.path)
	}
}

func TestGetHistoryRejectsLeadingMiddleAndTrailingSequenceLoss(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence int
	}{
		{name: "leading", sequence: 0},
		{name: "middle", sequence: 1},
		{name: "trailing", sequence: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			session := acquireNamed(t, store, test.name)
			ctx := context.Background()
			if err := session.AddMessages(ctx, []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "one"},
				{Role: messages.MessageRoleAssistant, Content: "two"},
				{Role: messages.MessageRoleUser, Content: "three"},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx,
				"DELETE FROM messages WHERE session_id = ? AND sequence = ?", session.(*sqliteSession).id, test.sequence); err != nil {
				t.Fatal(err)
			}
			if _, err := session.GetHistory(ctx); err == nil || !strings.Contains(err.Error(), "sequence is corrupt") {
				t.Fatalf("GetHistory after %s loss = %v", test.name, err)
			}
		})
	}
}

func TestStoreConfigValidation(t *testing.T) {
	for _, config := range []StoreConfig{
		{},
		{Mode: ModeDisk},
		{Mode: ModeMemory, AutoSessionTTL: -1},
		{Mode: ModeMemory, DefaultMetadata: &Metadata{TTL: -1}},
		{Mode: ModeMemory, CleanupInterval: -1},
	} {
		if store, err := OpenStore(config); err == nil {
			_ = store.Close()
			t.Fatalf("invalid config opened: %+v", config)
		}
	}
}

func TestSQLiteDataSourceUsesLocalWindowsDriveURI(t *testing.T) {
	dsn := sqliteDataSource(ModeDisk, `C:\Users\alex\polly.db`)
	if !strings.HasPrefix(dsn, "file:///C:/Users/alex/polly.db?") {
		t.Fatalf("Windows disk DSN = %q", dsn)
	}
	if strings.HasPrefix(dsn, "file://C:") {
		t.Fatalf("Windows drive became URI authority: %q", dsn)
	}
}

func TestSQLiteDataSourceUsesRelativeSQLiteURI(t *testing.T) {
	dsn := sqliteDataSource(ModeDisk, "polly.db")
	if !strings.HasPrefix(dsn, "file:///") {
		t.Fatalf("relative disk DSN = %q", dsn)
	}
	if strings.HasPrefix(dsn, "file://polly.db") {
		t.Fatalf("relative path became URI authority: %q", dsn)
	}
}

// TestMigrateSchemaV2StripsLegacyPromptsAndUpgradesImports pins the v1->v2
// data migration: the pre-contract default prompt leaves settings_json and
// its seeded system row, that session renumbers contiguously, a sibling with
// a real persona and every updated_ns stay untouched, --add text imports gain
// a FileName part with their text intact, and a second open is a no-op.
func TestMigrateSchemaV2StripsLegacyPromptsAndUpgradesImports(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	seed := func(name string, defaults *Metadata, history []messages.ChatMessage) {
		t.Helper()
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, DefaultMetadata: defaults})
		if err != nil {
			t.Fatal(err)
		}
		session, err := store.Acquire(ctx, name, AcquireOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := session.AddMessages(ctx, history); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	imported := messages.ChatMessage{
		Role:     messages.MessageRoleUser,
		Content:  "=== notes.txt ===\nbody",
		Metadata: map[string]any{messages.MetadataKeyContextImport: true},
	}
	typed := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "=== typed ===\nnot an import"}
	reply := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "ok"}
	seed("legacy", &Metadata{SystemPrompt: legacySystemPromptDefaults[1], Model: "openai/gpt-5.4"}, []messages.ChatMessage{imported, typed, reply})
	seed("custom", &Metadata{SystemPrompt: "be a pirate"}, []messages.ChatMessage{typed})

	before, err := func() (map[string]*Metadata, error) {
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		if err != nil {
			return nil, err
		}
		defer store.Close()
		return store.GetAllMetadata(ctx)
	}()
	if err != nil {
		t.Fatal(err)
	}

	rewind := func() {
		t.Helper()
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	version := func() int {
		t.Helper()
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		var v int
		if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	check := func(pass string) {
		t.Helper()
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		if err != nil {
			t.Fatalf("%s: %v", pass, err)
		}
		defer store.Close()
		all, err := store.GetAllMetadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := all["legacy"]; got.SystemPrompt != "" || got.Model != "openai/gpt-5.4" || !got.LastUsed.Equal(before["legacy"].LastUsed) {
			t.Fatalf("%s: legacy metadata = %+v, want the prompt stripped and the rest untouched (before %+v)", pass, got, before["legacy"])
		}
		if got := all["custom"]; got.SystemPrompt != "be a pirate" || !got.LastUsed.Equal(before["custom"].LastUsed) {
			t.Fatalf("%s: custom metadata = %+v, want untouched", pass, got)
		}
		legacy, err := store.Acquire(ctx, "legacy", AcquireOptions{})
		if err != nil {
			t.Fatal(err)
		}
		history, err := legacy.GetHistory(ctx)
		if err != nil {
			t.Fatalf("%s: legacy history: %v", pass, err)
		}
		_ = legacy.Close()
		upgraded := imported
		upgraded.Content = ""
		upgraded.Parts = []messages.ContentPart{{Type: "text", Text: imported.Content, FileName: "notes.txt"}}
		if want := []messages.ChatMessage{upgraded, typed, reply}; !reflect.DeepEqual(history, want) {
			t.Fatalf("%s: legacy history = %#v, want %#v", pass, history, want)
		}
		custom, err := store.Acquire(ctx, "custom", AcquireOptions{})
		if err != nil {
			t.Fatal(err)
		}
		history, err = custom.GetHistory(ctx)
		if err != nil {
			t.Fatalf("%s: custom history: %v", pass, err)
		}
		_ = custom.Close()
		if want := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "be a pirate"}, typed}; !reflect.DeepEqual(history, want) {
			t.Fatalf("%s: custom history = %#v, want %#v", pass, history, want)
		}
	}

	rewind()
	check("migrated open")
	if got := version(); got != schemaVersion {
		t.Fatalf("user_version after migration = %d, want %d", got, schemaVersion)
	}
	check("second open")
	rewind()
	check("re-run on migrated data")
}

func TestReportsWaitForTheirParentAndFollowTheirSessions(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	parent := acquireNamed(t, store, "parent")
	child := acquireNamed(t, store, "child")

	if err := store.PostReport(ctx, "parent", Report{Child: "child", Status: ReportFinished, Text: "done", InputTokens: 7, OutputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.PostReport(ctx, "parent", Report{Child: "other", Status: ReportFailed, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PostReport(ctx, "nobody", Report{Child: "child", Status: ReportFinished}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("posting to a missing session = %v, want ErrSessionNotFound", err)
	}
	if err := store.PostReport(ctx, "parent", Report{Child: "child", Status: "odd"}); err == nil {
		t.Fatal("an unknown status was accepted")
	}
	// The child's report names it as it is called when read, not when posted.
	if err := child.Rename(ctx, "renamed-child"); err != nil {
		t.Fatal(err)
	}
	reports, err := parent.TakeReports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("took %d reports, want 2", len(reports))
	}
	first, second := reports[0], reports[1]
	if first.Child != "renamed-child" || first.Status != ReportFinished || first.Text != "done" || first.InputTokens != 7 || first.OutputTokens != 3 || first.Posted.IsZero() {
		t.Fatalf("first report = %+v", first)
	}
	if second.Child != "other" || second.Status != ReportFailed || second.Error != "boom" {
		t.Fatalf("second report = %+v", second)
	}
	if again, err := parent.TakeReports(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second take = %v, %v; want nothing left", again, err)
	}

	// A report outlives its child, keeping the name the child had.
	if err := store.PostReport(ctx, "parent", Report{Child: "renamed-child", Status: ReportCanceled, Text: "partial"}); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "renamed-child"); err != nil {
		t.Fatal(err)
	}
	reports, err = parent.TakeReports(ctx)
	if err != nil || len(reports) != 1 || reports[0].Child != "renamed-child" || reports[0].Status != ReportCanceled {
		t.Fatalf("report after the child was deleted = %+v, %v", reports, err)
	}

	// Reports go with the session they are addressed to.
	if err := store.PostReport(ctx, "parent", Report{Child: "late", Status: ReportFinished, Text: "late"}); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.TakeReports(ctx); err == nil {
		t.Fatal("a closed session took reports")
	}
	if err := store.Delete(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := store.db.QueryRow("SELECT count(*) FROM session_reports").Scan(&left); err != nil || left != 0 {
		t.Fatalf("reports left after deleting their parent = %d, %v", left, err)
	}
}

func TestParentLinkFollowsRenamesAndSurvivesDeletes(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	parent := acquireNamed(t, store, "gamma")
	if _, err := store.Acquire(ctx, "stray", AcquireOptions{Parent: "nobody"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("acquire under a missing parent = %v, want ErrSessionNotFound", err)
	}
	child, err := store.Acquire(ctx, "delta", AcquireOptions{Auto: true, Parent: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	metadata, err := child.GetMetadata(ctx)
	if err != nil || metadata.Parent != "gamma" {
		t.Fatalf("child parent = %q, %v; want gamma", metadata.Parent, err)
	}

	// Callers cannot move a session under another parent.
	metadata.Parent = "elsewhere"
	metadata.Description = "count files"
	if err := child.SetMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	if metadata, err = child.GetMetadata(ctx); err != nil || metadata.Parent != "gamma" || metadata.Description != "count files" {
		t.Fatalf("after SetMetadata parent = %q, description %q, %v", metadata.Parent, metadata.Description, err)
	}

	// The parent's rename shows through, to the child and to listings.
	if err := parent.Rename(ctx, "gamma-two"); err != nil {
		t.Fatal(err)
	}
	if metadata, err = child.GetMetadata(ctx); err != nil || metadata.Parent != "gamma-two" {
		t.Fatalf("after the parent's rename, child parent = %q, %v", metadata.Parent, err)
	}
	all, err := store.GetAllMetadata(ctx)
	if err != nil || all["delta"].Parent != "gamma-two" {
		t.Fatalf("listed child parent = %q, %v", all["delta"].Parent, err)
	}

	// The child reports to whatever its parent is called now.
	if err := child.Report(ctx, Report{Status: ReportFinished, Text: "done"}); err != nil {
		t.Fatal(err)
	}
	reports, err := parent.TakeReports(ctx)
	if err != nil || len(reports) != 1 || reports[0].Child != "delta" || reports[0].Text != "done" {
		t.Fatalf("parent took %+v, %v", reports, err)
	}

	// A deleted parent leaves the child with the last name it knew, and
	// nowhere to report.
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "gamma-two"); err != nil {
		t.Fatal(err)
	}
	if metadata, err = child.GetMetadata(ctx); err != nil || metadata.Parent != "gamma" {
		t.Fatalf("orphan's remembered parent = %q, %v; want the name it last wrote", metadata.Parent, err)
	}
	if err := child.Report(ctx, Report{Status: ReportFinished}); !errors.Is(err, ErrNoParent) {
		t.Fatalf("orphan report = %v, want ErrNoParent", err)
	}
}

func TestMigrateSchemaV4LinksParentsFromSettings(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	ctx := context.Background()
	parent := acquireNamed(t, store, "gamma")
	child, err := store.Acquire(ctx, "delta", AcquireOptions{Parent: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	// A v3 database carried the parent only as a name in the settings.
	if _, err := store.db.Exec("UPDATE sessions SET parent_id = NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 3"); err != nil {
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
	var linked int
	if err := reopened.db.QueryRow(`
		SELECT count(*) FROM sessions AS s JOIN sessions AS p ON p.id = s.parent_id
		WHERE s.name = 'delta' AND p.name = 'gamma'`).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("linked rows after migration = %d, %v", linked, err)
	}
	gamma, err := reopened.Acquire(ctx, "gamma", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer gamma.Close()
	if err := gamma.Rename(ctx, "gamma-two"); err != nil {
		t.Fatal(err)
	}
	all, err := reopened.GetAllMetadata(ctx)
	if err != nil || all["delta"].Parent != "gamma-two" {
		t.Fatalf("backfilled link did not follow the rename: %q, %v", all["delta"].Parent, err)
	}
}
