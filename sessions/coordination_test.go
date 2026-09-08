package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestPromotionPreservesHandlesAndConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(StoreConfig{Mode: ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	id := parent.(ViewIdentity).ViewID()
	cache, _ := parent.CacheSessionID(ctx)
	ref, err := parent.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "saved.db")
	var wg sync.WaitGroup
	for j := 0; j < 8; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Promote(ctx, path); err != nil {
				t.Error(err)
			}
			if err := parent.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := parent.(ViewIdentity).ViewID(); got != id {
		t.Fatal("identity changed")
	}
	if got, _ := parent.CacheSessionID(ctx); got != cache {
		t.Fatal("cache identity changed")
	}
	history, err := parent.GetHistory(ctx)
	if err != nil || len(history) != 8 {
		t.Fatalf("lost writes %d %v", len(history), err)
	}
	reader, err := parent.ArtifactStore().Open(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "evidence" {
		t.Fatalf("artifact %s %v", data, err)
	}
	if err = parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	view, err := reopened.ReadView(ctx, ViewTarget{ID: id}, "")
	if err != nil || len(view.History) != 8 {
		t.Fatalf("not durable %+v %v", view, err)
	}
}

func TestCoordinationCheckpointRollbackAndPins(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(StoreConfig{Mode: ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	coord := child.(CoordinationSession)
	ref, err := child.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("published")})
	if err != nil {
		t.Fatal(err)
	}
	if err = coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Pins = []string{ref.ID}
		s.Records["mail"] = map[string]json.RawMessage{"m": json.RawMessage(`{"delivered":false}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	veto := errors.New("projection veto")
	err = coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Append = messages.User("not committed")
		s.Records["mail"]["m"] = json.RawMessage(`{"delivered":true}`)
		return veto
	})
	if !errors.Is(err, veto) {
		t.Fatal(err)
	}
	history, _ := child.GetHistory(ctx)
	if len(history) != 0 {
		t.Fatal("failed transaction appended history")
	}
	state, _ := coord.ReadCoordination(ctx)
	if string(state.Records["mail"]["m"]) != `{"delivered":false}` {
		t.Fatal("failed transaction delivered mail")
	}
	if err = child.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.db.QueryRow("SELECT count(*) FROM artifact_blobs").Scan(&count); err != nil || count != 1 {
		t.Fatalf("publication was collected: %d %v", count, err)
	}
	parent.Close()
	if err = store.Delete(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if err = store.db.QueryRow("SELECT count(*) FROM artifact_blobs").Scan(&count); err != nil || count != 0 {
		t.Fatalf("publication outlived parent: %d %v", count, err)
	}
}
