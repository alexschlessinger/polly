package llm

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"

	"github.com/alexschlessinger/pollytool/artifacts"
)

// testArtifactStore keeps llm package tests independent of production storage
// implementations while preserving the content-addressed Ref behavior they
// rely on.
type testArtifactStore struct {
	mu    sync.RWMutex
	blobs map[string][]byte
}

var _ artifacts.Store = (*testArtifactStore)(nil)

func newTestArtifactStore() *testArtifactStore {
	return &testArtifactStore{blobs: make(map[string][]byte)}
}

func (s *testArtifactStore) Put(ctx context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	if err := ctx.Err(); err != nil {
		return artifacts.Ref{}, err
	}
	ref := artifacts.RefForBlob(blob)

	s.mu.Lock()
	if _, exists := s.blobs[ref.ID]; !exists {
		s.blobs[ref.ID] = append([]byte(nil), blob.Data...)
	}
	s.mu.Unlock()
	return ref, nil
}

func (s *testArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !artifacts.ValidID(id) {
		return nil, artifacts.ErrInvalidID
	}
	s.mu.RLock()
	data, ok := s.blobs[id]
	copyData := append([]byte(nil), data...)
	s.mu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(copyData)), nil
}

func (s *testArtifactStore) RemoveAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.blobs = make(map[string][]byte)
	s.mu.Unlock()
	return nil
}
