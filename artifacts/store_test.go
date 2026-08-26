package artifacts

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFileStoreRoundTripAndPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private", "namespace")
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), Blob{
		Kind: KindText, MIMEType: "text/plain", Name: "out.txt", Data: []byte("one\ntwo\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Lines != 2 || ref.Bytes != 8 || !ValidID(ref.ID) {
		t.Fatalf("ref = %+v", ref)
	}
	r, err := store.Open(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("read = %q, %v", data, err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("root mode = %v, err=%v", info, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, err=%v", entries, err)
	}
	if info, err := entries[0].Info(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("blob mode = %v, err=%v", info, err)
	}
	blobPath := filepath.Join(root, entries[0].Name())
	if err := os.Chmod(blobPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), Blob{Kind: KindText, MIMEType: "text/plain", Name: "out.txt", Data: []byte("one\ntwo\n")}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(blobPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("deduplicated blob permissions = %v, err=%v", info, err)
	}
	entries, err = os.ReadDir(root)
	if err != nil || len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".blob" {
		t.Fatalf("atomic write left temporary files: %v, err=%v", entries, err)
	}
}

func TestImageRefsAlwaysHaveStableTokens(t *testing.T) {
	store := NewMemoryStore()
	generated, err := store.Put(context.Background(), Blob{Kind: KindImage, MIMEType: "image/png", Data: []byte("pixels")})
	if err != nil {
		t.Fatal(err)
	}
	if generated.ImageToken != "[image "+generated.ID+"]" {
		t.Fatalf("generated image token = %q", generated.ImageToken)
	}
	explicit, err := store.Put(context.Background(), Blob{
		Kind: KindImage, MIMEType: "image/png", ImageToken: "[image #7]", Data: []byte("other pixels"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.ImageToken != "[image #7]" {
		t.Fatalf("explicit image token = %q", explicit.ImageToken)
	}
}

func TestFileStoreDeduplicatesConcurrentWrites(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	refs := make(chan Ref, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := store.Put(context.Background(), Blob{Kind: KindText, Data: []byte("same")})
			refs <- ref
			errs <- err
		}()
	}
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for ref := range refs {
		if id == "" {
			id = ref.ID
		} else if ref.ID != id {
			t.Fatalf("IDs differ: %q != %q", ref.ID, id)
		}
	}
}

func TestStoresRejectInvalidIDsAndRemoveAll(t *testing.T) {
	stores := []Store{NewMemoryStore()}
	fileStore, err := NewFileStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	stores = append(stores, fileStore)
	for _, store := range stores {
		ref, err := store.Put(context.Background(), Blob{Kind: KindBinary, Data: []byte("payload")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Open(context.Background(), "../../etc/passwd"); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("invalid ID error = %v", err)
		}
		if _, err := store.Open(context.Background(), "sha256:"+strings.Repeat("A", 64)); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("noncanonical ID error = %v", err)
		}
		if err := store.RemoveAll(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Open(context.Background(), ref.ID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("open after remove = %v", err)
		}
	}
}

func TestStoresHonorCanceledContexts(t *testing.T) {
	stores := []Store{NewMemoryStore()}
	fileStore, err := NewFileStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	stores = append(stores, fileStore)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, store := range stores {
		if _, err := store.Put(ctx, Blob{Kind: KindText, Data: []byte("nope")}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Put error = %v", err)
		}
		if _, err := store.Open(ctx, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Open error = %v", err)
		}
	}
}
