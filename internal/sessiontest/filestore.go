// Package sessiontest provides storage conformance fixtures shared by session
// and runtime tests. The file store is deliberately independent of SQLite: each
// operation decodes a JSON snapshot, and writes commit by atomic file replacement.
// It is a test implementation, not a production backend: leases coordinate only
// handles opened through one fixture, with explicit revocation instead of timers.
package sessiontest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

type fileDatabase struct {
	mu         sync.Mutex
	path       string
	leases     map[string]*fileSession
	failCommit bool
}

type snapshot struct {
	Sessions map[string]*conversation
	Blobs    map[string][]byte
}

type conversation struct {
	Parent   string
	Metadata sessions.Metadata
	History  []messages.ChatMessage
	Revision uint64
	Owned    map[string]bool
	Pins     map[string]bool
	Members  map[string]bool
	Records  map[string]map[string]json.RawMessage
}

type fileStore struct {
	db     *fileDatabase
	closed bool
}

type fileSession struct {
	store  *fileStore
	id     string
	ctx    context.Context
	cancel context.CancelCauseFunc
}

var (
	_ sessions.SessionStore          = (*fileStore)(nil)
	_ sessions.ViewStore             = (*fileStore)(nil)
	_ sessions.CoordinationViewStore = (*fileStore)(nil)
	_ sessions.Session               = (*fileSession)(nil)
	_ sessions.CoordinationSession   = (*fileSession)(nil)
)

func (d *fileDatabase) load() (*snapshot, error) {
	b, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return &snapshot{Sessions: map[string]*conversation{}, Blobs: map[string][]byte{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var v snapshot
	err = json.Unmarshal(b, &v)
	return &v, err
}

func (d *fileDatabase) save(v *snapshot) error {
	if d.failCommit {
		return errors.New("injected file commit failure")
	}
	// Collect bytes only when neither private owners nor family publications remain.
	used := map[string]bool{}
	for _, c := range v.Sessions {
		for id := range c.Owned {
			used[id] = true
		}
		for id := range c.Pins {
			used[id] = true
		}
	}
	for id := range v.Blobs {
		if !used[id] {
			delete(v.Blobs, id)
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(d.path), "snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), d.path)
}

func (s *fileStore) transact(ctx context.Context, write bool, fn func(*snapshot) error) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if s.closed {
		return sessions.ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	v, err := s.db.load()
	if err != nil {
		return err
	}
	if err = fn(v); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if write {
		return s.db.save(v)
	}
	return nil
}

func byName(v *snapshot, name string) (string, *conversation) {
	for id, c := range v.Sessions {
		if c.Metadata.Name == name {
			return id, c
		}
	}
	return "", nil
}

func metadata(v *snapshot, c *conversation) *sessions.Metadata {
	m := c.Metadata
	if p := v.Sessions[c.Parent]; p != nil {
		m.Parent = p.Metadata.Name
	}
	return &m
}

func touch(c *conversation) { c.Revision++; c.Metadata.LastUsed = time.Now().UTC() }

func family(id string, c *conversation) string {
	if c.Parent != "" {
		return c.Parent
	}
	return id
}

func freshConversation(name, parent string) *conversation {
	now := time.Now().UTC()
	return &conversation{Parent: parent, Metadata: sessions.Metadata{Name: name, Created: now, LastUsed: now}, Owned: map[string]bool{}, Pins: map[string]bool{}, Members: map[string]bool{}, Records: map[string]map[string]json.RawMessage{}}
}

func (s *fileStore) Acquire(ctx context.Context, name string, o sessions.AcquireOptions) (sessions.Session, error) {
	var out *fileSession
	err := s.transact(ctx, true, func(v *snapshot) error {
		id, c := byName(v, name)
		if c == nil {
			if o.ExistingOnly || o.ExpectedID != "" {
				return sessions.ErrSessionNotFound
			}
			parent := ""
			if o.Parent != "" {
				parent, _ = byName(v, o.Parent)
				if parent == "" {
					return sessions.ErrSessionNotFound
				}
			}
			id = ids.New()
			c = freshConversation(name, parent)
			v.Sessions[id] = c
		}
		if o.ExpectedID != "" && id != o.ExpectedID {
			return sessions.ErrSessionNotFound
		}
		if s.db.leases[id] != nil {
			return sessions.ErrSessionInUse
		}
		owned, cancel := context.WithCancelCause(context.Background())
		out = &fileSession{store: s, id: id, ctx: owned, cancel: cancel}
		s.db.leases[id] = out
		touch(c)
		return nil
	})
	if err != nil {
		if out != nil {
			out.Close()
		}
		return nil, err
	}
	return out, err
}

func (s *fileStore) Delete(ctx context.Context, name string) error {
	return s.transact(ctx, true, func(v *snapshot) error {
		id, c := byName(v, name)
		if c == nil {
			return sessions.ErrSessionNotFound
		}
		if s.db.leases[id] != nil {
			return sessions.ErrSessionInUse
		}
		remove(v, id)
		return nil
	})
}

func remove(v *snapshot, id string) {
	for _, c := range v.Sessions {
		if c.Parent == id {
			c.Metadata.Parent = v.Sessions[id].Metadata.Name
			c.Parent = ""
		}
		delete(c.Members, id)
	}
	delete(v.Sessions, id)
}

func (s *fileStore) ListSummaries(ctx context.Context) ([]sessions.SessionSummary, error) {
	var out []sessions.SessionSummary
	err := s.transact(ctx, false, func(v *snapshot) error {
		for id, c := range v.Sessions {
			out = append(out, sessions.SessionSummary{ID: id, ParentID: c.Parent, Metadata: metadata(v, c), MessageCount: len(c.History), InUse: s.db.leases[id] != nil})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, err
}

func (s *fileStore) List(ctx context.Context) ([]string, error) {
	rows, err := s.ListSummaries(ctx)
	var out []string
	for _, r := range rows {
		out = append(out, r.Metadata.Name)
	}
	return out, err
}

func (s *fileStore) Exists(ctx context.Context, name string) (bool, error) {
	_, err := s.GetMetadata(ctx, name)
	if errors.Is(err, sessions.ErrSessionNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *fileStore) GetMetadata(ctx context.Context, name string) (*sessions.Metadata, error) {
	var out *sessions.Metadata
	err := s.transact(ctx, false, func(v *snapshot) error {
		_, c := byName(v, name)
		if c == nil {
			return sessions.ErrSessionNotFound
		}
		out = metadata(v, c)
		return nil
	})
	return out, err
}

func (s *fileStore) GetAllMetadata(ctx context.Context) (map[string]*sessions.Metadata, error) {
	rows, err := s.ListSummaries(ctx)
	out := map[string]*sessions.Metadata{}
	for _, r := range rows {
		out[r.Metadata.Name] = r.Metadata
	}
	return out, err
}

func (s *fileStore) GetLast(ctx context.Context) (string, error) {
	rows, err := s.ListSummaries(ctx)
	var last string
	var used time.Time
	for _, r := range rows {
		if r.Metadata.LastUsed.After(used) {
			last = r.Metadata.Name
			used = r.Metadata.LastUsed
		}
	}
	return last, err
}

func (s *fileStore) Expire(ctx context.Context) error {
	return s.transact(ctx, true, func(v *snapshot) error {
		pinned := map[string]bool{}
		for id, c := range v.Sessions {
			if len(c.Records) > 0 {
				pinned[id] = true
			}
			for id := range c.Members {
				pinned[id] = true
			}
		}
		for id, c := range v.Sessions {
			if c.Metadata.TTL > 0 && time.Since(c.Metadata.LastUsed) > c.Metadata.TTL && s.db.leases[id] == nil && !pinned[id] {
				remove(v, id)
			}
		}
		return nil
	})
}

func (s *fileStore) Close() error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.closed = true
	for id, h := range s.db.leases {
		if h.store == s {
			h.cancel(sessions.ErrStoreClosed)
			delete(s.db.leases, id)
		}
	}
	return nil
}

func (s *fileSession) Context() context.Context { return s.ctx }

func (s *fileSession) ViewID() string { return s.id }

func (s *fileSession) check() error {
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	if s.store.db.leases[s.id] != s {
		s.cancel(sessions.ErrSessionLeaseLost)
		return sessions.ErrSessionLeaseLost
	}
	return nil
}

func (s *fileSession) transact(ctx context.Context, write bool, fn func(*snapshot, *conversation) error) error {
	return s.store.transact(ctx, write, func(v *snapshot) error {
		if err := s.check(); err != nil {
			return err
		}
		return v.with(s.id, fn)
	})
}

// with runs fn on the conversation id names.
func (v *snapshot) with(id string, fn func(*snapshot, *conversation) error) error {
	c := v.Sessions[id]
	if c == nil {
		return sessions.ErrSessionNotFound
	}
	return fn(v, c)
}

func (s *fileSession) Close() error {
	s.store.db.mu.Lock()
	defer s.store.db.mu.Unlock()
	if s.store.db.leases[s.id] == s {
		delete(s.store.db.leases, s.id)
	}
	s.cancel(context.Canceled)
	return nil
}

func (s *fileSession) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	var out []messages.ChatMessage
	err := s.transact(ctx, false, func(_ *snapshot, c *conversation) error { out = c.History; return nil })
	return out, err
}

func (s *fileSession) AddMessage(ctx context.Context, m messages.ChatMessage) error {
	return s.AddMessages(ctx, []messages.ChatMessage{m})
}

func (s *fileSession) AddMessages(ctx context.Context, m []messages.ChatMessage) error {
	return s.transact(ctx, true, func(_ *snapshot, c *conversation) error { c.History = append(c.History, m...); touch(c); return nil })
}

func clear(c *conversation) {
	c.History = nil
	c.Owned = map[string]bool{}
	if c.Metadata.SystemPrompt != "" {
		c.History = []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: c.Metadata.SystemPrompt}}
	}
	touch(c)
}

func (s *fileSession) Clear(ctx context.Context) error {
	return s.transact(ctx, true, func(_ *snapshot, c *conversation) error { clear(c); return nil })
}

func (s *fileSession) Reset(ctx context.Context, m *sessions.Metadata) error {
	return s.settings(ctx, m, true)
}

func (s *fileSession) SetMetadata(ctx context.Context, m *sessions.Metadata) error {
	return s.settings(ctx, m, false)
}

func (s *fileSession) settings(ctx context.Context, m *sessions.Metadata, reset bool) error {
	if m == nil {
		return errors.New("nil metadata")
	}
	return s.transact(ctx, true, func(_ *snapshot, c *conversation) error {
		next := *m
		current := c.Metadata
		next.Name = current.Name
		next.Created = current.Created
		next.TTL = current.TTL
		next.Parent = current.Parent
		if current.SpawnCallID != "" {
			next.SpawnCallID = current.SpawnCallID
		}
		if current.SpawnOutcome != "" {
			next.SpawnOutcome = current.SpawnOutcome
		}
		if current.SwarmID != "" {
			next.SwarmID = current.SwarmID
		}
		if current.ExecutionContext != "" {
			next.ExecutionContext = current.ExecutionContext
		}
		c.Metadata = next
		if reset {
			clear(c)
		} else {
			touch(c)
		}
		return nil
	})
}

func (s *fileSession) GetName(ctx context.Context) (string, error) {
	m, err := s.GetMetadata(ctx)
	if err != nil {
		return "", err
	}
	return m.Name, nil
}

func (s *fileSession) Rename(ctx context.Context, name string) error {
	return s.transact(ctx, true, func(v *snapshot, c *conversation) error {
		id, _ := byName(v, name)
		if id != "" && id != s.id {
			return errors.New("name exists")
		}
		c.Metadata.Name = name
		touch(c)
		return nil
	})
}

func (s *fileSession) GetMetadata(ctx context.Context) (*sessions.Metadata, error) {
	var out *sessions.Metadata
	err := s.transact(ctx, false, func(v *snapshot, c *conversation) error { out = metadata(v, c); return nil })
	return out, err
}

func (s *fileSession) CacheSessionID(ctx context.Context) (string, error) {
	err := s.transact(ctx, false, func(*snapshot, *conversation) error { return nil })
	return "file-cache-" + s.id, err
}

func (s *fileSession) ArtifactStore() artifacts.Store { return s.artifacts() }

func (s *fileSession) artifacts() *fileArtifacts {
	return &fileArtifacts{store: s.store, id: s.id, lease: s}
}

func (s *fileSession) ReadCoordination(ctx context.Context) (*sessions.CoordinationState, error) {
	var out *sessions.CoordinationState
	err := s.transact(ctx, false, func(v *snapshot, c *conversation) error { out = coordination(v, s.id, c); return nil })
	return out, err
}

func coordination(v *snapshot, id string, c *conversation) *sessions.CoordinationState {
	root := family(id, c)
	return &sessions.CoordinationState{ActorID: id, ParentID: root, Records: v.Sessions[root].Records, Sequence: int64(len(c.History))}
}

func (s *fileSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	if fn == nil {
		return errors.New("nil coordination callback")
	}
	return s.transact(ctx, true, func(v *snapshot, c *conversation) error {
		state := coordination(v, s.id, c)
		rootID := state.ParentID
		root := v.Sessions[rootID]
		sequence := state.Sequence
		if err := fn(state); err != nil {
			return err
		}
		if state.ExpectedSequence != nil && *state.ExpectedSequence != sequence {
			return errors.New("checkpoint sequence changed")
		}
		for kind, group := range state.Records {
			for key, value := range group {
				if kind == "" || key == "" || !json.Valid(value) {
					return errors.New("invalid coordination record")
				}
			}
		}
		if len(state.Members) > 0 && s.id != rootID {
			return errors.New("only parent can register members")
		}
		for _, id := range state.Members {
			member := v.Sessions[id]
			if member == nil || member.Parent != rootID {
				return errors.New("not a direct child")
			}
			root.Members[id] = true
		}
		owner := s.id
		if state.PinOwner != "" && state.PinOwner != s.id {
			member := v.Sessions[state.PinOwner]
			if s.id != rootID || member == nil || member.Parent != rootID {
				return errors.New("invalid pin owner")
			}
			owner = state.PinOwner
		}
		for _, id := range state.Pins {
			if !v.Sessions[owner].Owned[id] {
				return errors.New("artifact not owned")
			}
			root.Pins[id] = true
		}
		root.Records = state.Records
		c.History = append(c.History, state.Append...)
		if len(state.Append) > 0 {
			touch(c)
		}
		return nil
	})
}

func (s *fileSession) OpenPublishedArtifact(ctx context.Context, id string) (artifacts.Ref, io.ReadCloser, error) {
	ref := artifacts.Ref{ID: id}
	var blob []byte
	err := s.transact(ctx, true, func(v *snapshot, c *conversation) error {
		if !v.Sessions[family(s.id, c)].Pins[id] {
			return errors.New("artifact not published")
		}
		blob = v.Blobs[id]
		ref.Bytes = int64(len(blob))
		c.Owned[id] = true
		return nil
	})
	if err != nil {
		return artifacts.Ref{}, nil, err
	}
	return ref, s.artifacts().reader(blob), nil
}

func (s *fileStore) ReadCoordinationView(ctx context.Context, id string) (*sessions.CoordinationState, error) {
	var out *sessions.CoordinationState
	err := s.transact(ctx, false, func(v *snapshot) error {
		c := v.Sessions[id]
		if c == nil || c.Parent != "" {
			return sessions.ErrSessionNotFound
		}
		out = coordination(v, id, c)
		return nil
	})
	return out, err
}

func (s *fileStore) ReadView(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
	var out *sessions.SessionView
	err := s.transact(ctx, false, func(v *snapshot) error {
		id := target.ID
		if id == "" && target.Parent != "" && target.SpawnCallID != "" {
			parent, _ := byName(v, target.Parent)
			for candidate, c := range v.Sessions {
				if parent != "" && c.Parent == parent && c.Metadata.SpawnCallID == target.SpawnCallID {
					if id != "" {
						return errors.New("ambiguous spawn")
					}
					id = candidate
				}
			}
		} else if id == "" {
			id, _ = byName(v, target.Name)
		}
		c := v.Sessions[id]
		if c == nil {
			return sessions.ErrSessionNotFound
		}
		m := metadata(v, c)
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		revision := fmt.Sprintf("%d-%x", c.Revision, sha256.Sum256(b))
		out = &sessions.SessionView{ID: id, ParentID: c.Parent, Revision: revision, Metadata: m, InUse: s.db.leases[id] != nil, Unchanged: known == revision, Artifacts: &fileArtifacts{store: s, id: id}}
		if !out.Unchanged {
			out.History = c.History
		}
		return nil
	})
	return out, err
}

// fileArtifacts is a conversation's artifact store: a leased session's,
// fenced by its lease, or a read-only view's, which takes no lease and
// refuses writes.
type fileArtifacts struct {
	store *fileStore
	id    string
	lease *fileSession // nil for a read-only view
}

func (a *fileArtifacts) transact(ctx context.Context, write bool, fn func(*snapshot, *conversation) error) error {
	if a.lease != nil {
		return a.lease.transact(ctx, write, fn)
	}
	if write {
		return sessions.ErrReadOnlyView
	}
	return a.store.transact(ctx, false, func(v *snapshot) error { return v.with(a.id, fn) })
}

// check fences an open reader without decoding the snapshot: the store must
// still be open, and a leased reader's lease still held.
func (a *fileArtifacts) check() error {
	a.store.db.mu.Lock()
	defer a.store.db.mu.Unlock()
	if a.store.closed {
		return sessions.ErrStoreClosed
	}
	if a.lease != nil {
		return a.lease.check()
	}
	return nil
}

func (a *fileArtifacts) reader(b []byte) io.ReadCloser {
	return &fileReader{reader: bytes.NewReader(b), check: a.check}
}

func (a *fileArtifacts) Put(ctx context.Context, b artifacts.Blob) (artifacts.Ref, error) {
	ref := artifacts.RefForBlob(b)
	err := a.transact(ctx, true, func(v *snapshot, c *conversation) error { v.Blobs[ref.ID] = b.Data; c.Owned[ref.ID] = true; return nil })
	return ref, err
}

func (a *fileArtifacts) RemoveAll(ctx context.Context) error {
	return a.transact(ctx, true, func(_ *snapshot, c *conversation) error { c.Owned = map[string]bool{}; return nil })
}

func (a *fileArtifacts) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if !artifacts.ValidID(id) {
		return nil, artifacts.ErrInvalidID
	}
	var b []byte
	err := a.transact(ctx, false, func(v *snapshot, c *conversation) error {
		if !c.Owned[id] {
			return os.ErrNotExist
		}
		b = v.Blobs[id]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a.reader(b), nil
}

type fileReader struct {
	reader *bytes.Reader
	check  func() error
	closed bool
}

func (r *fileReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if err := r.check(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (r *fileReader) Close() error { r.closed = true; return nil }
