// Package artifacts stores large or binary conversation payloads outside the
// JSON transcript while leaving stable, serializable references in messages.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Kind describes how an artifact may be projected back to a model.
type Kind string

const (
	KindText   Kind = "text"
	KindImage  Kind = "image"
	KindBinary Kind = "binary"
)

// Ref is the durable, provider-neutral description stored in a transcript.
// ID is opaque to callers; stores currently use a full sha256 digest.
type Ref struct {
	ID         string `json:"id"`
	Kind       Kind   `json:"kind"`
	MIMEType   string `json:"mime_type,omitempty"`
	Name       string `json:"name,omitempty"`
	ImageToken string `json:"image_token,omitempty"`
	Reference  string `json:"reference,omitempty"`
	Bytes      int64  `json:"bytes"`
	Lines      int    `json:"lines,omitempty"`
}

// Blob is a payload plus the display/projection metadata used to build Ref.
type Blob struct {
	Kind       Kind
	MIMEType   string
	Name       string
	ImageToken string
	Reference  string
	Data       []byte
}

// Store is deliberately small. Metadata lives in transcript refs; the store
// owns immutable bytes addressed by ID.
type Store interface {
	Put(context.Context, Blob) (Ref, error)
	Open(context.Context, string) (io.ReadCloser, error)
	RemoveAll() error
}

var ErrInvalidID = errors.New("invalid artifact id")

const idPrefix = "sha256:"

// ValidID reports whether id is a canonical full artifact ID.
func ValidID(id string) bool {
	if !strings.HasPrefix(id, idPrefix) || len(id) != len(idPrefix)+sha256.Size*2 {
		return false
	}
	digest := strings.TrimPrefix(id, idPrefix)
	if digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func idFor(data []byte) string {
	sum := sha256.Sum256(data)
	return idPrefix + hex.EncodeToString(sum[:])
}

func refFor(blob Blob) Ref {
	id := idFor(blob.Data)
	lines := 0
	if blob.Kind == KindText {
		lines = lineCount(blob.Data)
	}
	imageToken := blob.ImageToken
	if blob.Kind == KindImage && imageToken == "" {
		if strings.HasPrefix(blob.Reference, "[image ") && strings.HasSuffix(blob.Reference, "]") {
			imageToken = blob.Reference
		} else {
			imageToken = "[image " + id + "]"
		}
	}
	return Ref{
		ID:         id,
		Kind:       blob.Kind,
		MIMEType:   blob.MIMEType,
		Name:       blob.Name,
		ImageToken: imageToken,
		Reference:  blob.Reference,
		Bytes:      int64(len(blob.Data)),
		Lines:      lines,
	}
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

// MemoryStore is used by ephemeral sessions and tests.
type MemoryStore struct {
	mu    sync.RWMutex
	blobs map[string][]byte
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{blobs: make(map[string][]byte)}
}

func (s *MemoryStore) Put(ctx context.Context, blob Blob) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	ref := refFor(blob)
	s.mu.Lock()
	if s.blobs == nil {
		s.blobs = make(map[string][]byte)
	}
	if _, exists := s.blobs[ref.ID]; !exists {
		s.blobs[ref.ID] = append([]byte(nil), blob.Data...)
	}
	s.mu.Unlock()
	return ref, nil
}

func (s *MemoryStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ValidID(id) {
		return nil, ErrInvalidID
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

func (s *MemoryStore) RemoveAll() error {
	s.mu.Lock()
	s.blobs = make(map[string][]byte)
	s.mu.Unlock()
	return nil
}

// FileStore keeps one session namespace in root. Blobs are immutable regular
// files named by their digest, with no caller-controlled path components.
type FileStore struct {
	root string
	mu   sync.RWMutex
}

func NewFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("artifact root is empty")
	}
	s := &FileStore{root: root}
	if err := s.ensureRoot(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileStore) ensureRoot() error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	// MkdirAll respects an existing directory's mode; tighten it explicitly.
	if err := os.Chmod(s.root, 0o700); err != nil {
		return fmt.Errorf("protect artifact directory: %w", err)
	}
	return nil
}

func (s *FileStore) path(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrInvalidID
	}
	return filepath.Join(s.root, strings.TrimPrefix(id, idPrefix)+".blob"), nil
}

func (s *FileStore) Put(ctx context.Context, blob Blob) (ref Ref, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if err := s.ensureRoot(); err != nil {
		return Ref{}, err
	}
	ref = refFor(blob)
	path, _ := s.path(ref.ID)
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return Ref{}, fmt.Errorf("artifact target is not a regular file")
		}
		if info.Size() != int64(len(blob.Data)) {
			return Ref{}, fmt.Errorf("artifact digest collision or corrupt stored size")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return Ref{}, fmt.Errorf("protect artifact: %w", err)
		}
		matches, verifyErr := fileMatchesArtifactID(path, ref.ID)
		if verifyErr != nil {
			return Ref{}, fmt.Errorf("verify stored artifact: %w", verifyErr)
		}
		if !matches {
			return Ref{}, fmt.Errorf("artifact digest collision or corrupt stored content")
		}
		return ref, nil
	} else if !os.IsNotExist(statErr) {
		return Ref{}, statErr
	}

	tmp, err := os.CreateTemp(s.root, ".artifact-*.tmp")
	if err != nil {
		return Ref{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return Ref{}, err
	}
	if _, err = tmp.Write(blob.Data); err != nil {
		return Ref{}, err
	}
	if err = tmp.Sync(); err != nil {
		return Ref{}, err
	}
	if err = tmp.Close(); err != nil {
		return Ref{}, err
	}
	if err = ctx.Err(); err != nil {
		return Ref{}, err
	}
	if err = os.Rename(tmpName, path); err != nil {
		// A concurrent writer of the same immutable digest is equivalent.
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() && info.Size() == int64(len(blob.Data)) {
			if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
				return Ref{}, fmt.Errorf("protect concurrent artifact: %w", chmodErr)
			}
			matches, verifyErr := fileMatchesArtifactID(path, ref.ID)
			if verifyErr != nil {
				return Ref{}, fmt.Errorf("verify concurrent artifact: %w", verifyErr)
			}
			if matches {
				_ = os.Remove(tmpName)
				return ref, nil
			}
		}
		return Ref{}, err
	}
	if dir, openErr := os.Open(s.root); openErr == nil {
		syncErr := dir.Sync()
		closeErr := dir.Close()
		if syncErr != nil {
			return Ref{}, fmt.Errorf("sync artifact directory: %w", syncErr)
		}
		if closeErr != nil {
			return Ref{}, fmt.Errorf("close artifact directory: %w", closeErr)
		}
	} else {
		return Ref{}, fmt.Errorf("open artifact directory for sync: %w", openErr)
	}
	return ref, nil
}

func fileMatchesArtifactID(path, id string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return false, copyErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	return idPrefix+hex.EncodeToString(h.Sum(nil)) == id, nil
}

func (s *FileStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("artifact is not a regular file")
	}
	return f, nil
}

func (s *FileStore) RemoveAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.root); err != nil {
		return err
	}
	return nil
}
