package sessions

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func openExtraContractStore(t *testing.T, mode StoreMode, autoTTL time.Duration) (*SQLiteStore, StoreConfig) {
	t.Helper()
	config := StoreConfig{Mode: mode, AutoSessionTTL: autoTTL}
	if mode == ModeDisk {
		config.Path = filepath.Join(t.TempDir(), "polly.db")
	}
	store, err := OpenStore(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, config
}

func reopenExtraContractDiskStore(t *testing.T, store *SQLiteStore, config StoreConfig) *SQLiteStore {
	t.Helper()
	if config.Mode != ModeDisk {
		return store
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func TestSQLiteContractAcquireExistingDoesNotAdvanceLastUsed(t *testing.T) {
	for _, mode := range []StoreMode{ModeMemory, ModeDisk} {
		mode := mode
		t.Run(map[StoreMode]string{ModeMemory: "memory", ModeDisk: "reopened-disk"}[mode], func(t *testing.T) {
			ctx := context.Background()
			store, config := openExtraContractStore(t, mode, 0)

			older, err := store.Acquire(ctx, "older", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := older.Close(); err != nil {
				t.Fatal(err)
			}
			newer, err := store.Acquire(ctx, "newer", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := newer.Close(); err != nil {
				t.Fatal(err)
			}

			olderUsed := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
			newerUsed := olderUsed.Add(time.Hour)
			for name, used := range map[string]time.Time{"older": olderUsed, "newer": newerUsed} {
				if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET updated_ns = ? WHERE name = ?", used.UnixNano(), name); err != nil {
					t.Fatal(err)
				}
			}

			store = reopenExtraContractDiskStore(t, store, config)
			if last, err := store.GetLast(ctx); err != nil || last != "newer" {
				t.Fatalf("GetLast() before read-only acquire = %q, %v", last, err)
			}

			reopenedOlder, err := store.Acquire(ctx, "older", AcquireOptions{Auto: true})
			if err != nil {
				t.Fatal(err)
			}
			gotUsed, err := reopenedOlder.GetLastUsed(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if gotUsed.UnixNano() != olderUsed.UnixNano() {
				t.Fatalf("read-only Acquire advanced LastUsed: got %v, want %v", gotUsed, olderUsed)
			}
			if err := reopenedOlder.Close(); err != nil {
				t.Fatal(err)
			}
			if last, err := store.GetLast(ctx); err != nil || last != "newer" {
				t.Fatalf("GetLast() after read-only acquire = %q, %v", last, err)
			}
			all, err := store.GetAllMetadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if all["older"].LastUsed.UnixNano() != olderUsed.UnixNano() {
				t.Fatalf("stored LastUsed after close = %v, want %v", all["older"].LastUsed, olderUsed)
			}
		})
	}
}

func TestSQLiteContractReplaysCompleteChatMessageJSON(t *testing.T) {
	for _, mode := range []StoreMode{ModeMemory, ModeDisk} {
		mode := mode
		t.Run(map[StoreMode]string{ModeMemory: "memory", ModeDisk: "reopened-disk"}[mode], func(t *testing.T) {
			ctx := context.Background()
			store, config := openExtraContractStore(t, mode, 0)
			session, err := store.Acquire(ctx, "opaque-message", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{
				Kind: artifacts.KindImage, MIMEType: "image/png", Name: "frame.png",
				ImageToken: "[image #7]", Reference: "[image #7]", Data: []byte("opaque image bytes"),
			})
			if err != nil {
				t.Fatal(err)
			}
			expected := messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "complete provider-neutral payload",
				Parts: []messages.ContentPart{
					{Type: "text", Text: "part text", FileName: "note.txt"},
					{Type: "image_artifact", MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken, Artifact: &ref},
					{Type: "image_url", ImageURL: "https://example.invalid/image.png", Reference: "remote"},
				},
				ToolCalls:  []messages.ChatMessageToolCall{{ID: "provider-call-17", Name: "inspect", Arguments: `{"detail":"full"}`}},
				ToolCallID: "provider-parent-3",
				ToolName:   "inspect",
				Reasoning:  "provider reasoning payload",
				Metadata: map[string]any{
					"nested": map[string]any{
						"enabled": true,
						"values":  []any{"alpha", float64(7), map[string]any{"leaf": "value"}},
					},
					"arbitrary": []any{nil, false, float64(2.5)},
				},
				StopReason: messages.StopReasonMaxTokens,
			}
			if err := session.AddMessage(ctx, expected); err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}

			store = reopenExtraContractDiskStore(t, store, config)
			session, err = store.Acquire(ctx, "opaque-message", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			history, err := session.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 1 || !reflect.DeepEqual(history[0], expected) {
				t.Fatalf("replayed message = %#v, want %#v", history, expected)
			}
		})
	}
}

func TestSQLiteContractTimeToExpiry(t *testing.T) {
	const autoTTL = 2 * time.Hour
	for _, mode := range []StoreMode{ModeMemory, ModeDisk} {
		mode := mode
		t.Run(map[StoreMode]string{ModeMemory: "memory", ModeDisk: "reopened-disk"}[mode], func(t *testing.T) {
			ctx := context.Background()
			store, config := openExtraContractStore(t, mode, autoTTL)
			named, err := store.Acquire(ctx, "named", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := named.Close(); err != nil {
				t.Fatal(err)
			}
			auto, err := store.Acquire(ctx, "auto", AcquireOptions{Auto: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := auto.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "keep this auto session"}); err != nil {
				t.Fatal(err)
			}
			if err := auto.Close(); err != nil {
				t.Fatal(err)
			}

			store = reopenExtraContractDiskStore(t, store, config)
			named, err = store.Acquire(ctx, "named", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if remaining, err := named.GetTimeToExpiry(ctx); err != nil || remaining != 0 {
				t.Fatalf("named GetTimeToExpiry() = %v, %v; want zero", remaining, err)
			}
			if err := named.Close(); err != nil {
				t.Fatal(err)
			}

			auto, err = store.Acquire(ctx, "auto", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			remaining, err := auto.GetTimeToExpiry(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if remaining <= autoTTL-time.Minute || remaining > autoTTL {
				t.Fatalf("auto GetTimeToExpiry() = %v, want (%v, %v]", remaining, autoTTL-time.Minute, autoTTL)
			}
			if err := auto.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLiteContractDeleteCollectsOnlyOrphanedArtifacts(t *testing.T) {
	for _, mode := range []StoreMode{ModeMemory, ModeDisk} {
		mode := mode
		t.Run(map[StoreMode]string{ModeMemory: "memory", ModeDisk: "reopened-disk"}[mode], func(t *testing.T) {
			ctx := context.Background()
			store, config := openExtraContractStore(t, mode, 0)
			first, err := store.Acquire(ctx, "first", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.Acquire(ctx, "second", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sharedBytes := []byte("shared bytes")
			sharedFirst, err := first.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: sharedBytes})
			if err != nil {
				t.Fatal(err)
			}
			sharedSecond, err := second.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: sharedBytes})
			if err != nil {
				t.Fatal(err)
			}
			if sharedFirst.ID != sharedSecond.ID {
				t.Fatalf("shared artifact IDs differ: %q != %q", sharedFirst.ID, sharedSecond.ID)
			}
			private, err := first.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: []byte("first-only bytes")})
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}

			store = reopenExtraContractDiskStore(t, store, config)
			if err := store.Delete(ctx, "first"); err != nil {
				t.Fatal(err)
			}
			if got := extraContractBlobCount(t, store, private); got != 0 {
				t.Fatalf("private blob rows after first delete = %d, want 0", got)
			}
			if got := extraContractBlobCount(t, store, sharedFirst); got != 1 {
				t.Fatalf("shared blob rows after first delete = %d, want 1", got)
			}
			second, err = store.Acquire(ctx, "second", AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := readArtifact(t, second.ArtifactStore(), sharedSecond.ID); !reflect.DeepEqual(got, sharedBytes) {
				t.Fatalf("shared artifact after first delete = %q", got)
			}
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Delete(ctx, "second"); err != nil {
				t.Fatal(err)
			}
			if got := extraContractBlobCount(t, store, sharedFirst); got != 0 {
				t.Fatalf("shared blob rows after second delete = %d, want 0", got)
			}
		})
	}
}

func extraContractBlobCount(t *testing.T, store *SQLiteStore, ref artifacts.Ref) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM artifact_blobs WHERE digest = ?", mustDigest(t, ref.ID)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
