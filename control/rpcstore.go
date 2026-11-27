package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// The session store of a control backend, read over its connection: every
// operation is a request, and nothing of the backend's files is read here,
// so a store on another machine reads as one beside this process. The
// backend writes its transcripts itself as its turns run; this side only
// reads them, and the sessions it hands out hold no lease.

// ErrUnsupported is an operation a control backend does not serve.
var ErrUnsupported = errors.New("not supported by the control backend")

// ClientSource is the connection a store makes its requests on: the one in
// use now, reached again where the last one ended.
type ClientSource func(ctx context.Context) (*Client, error)

// RPCStore is a sessions.SessionStore and sessions.ViewStore over a control
// connection.
type RPCStore struct {
	client ClientSource

	mu        sync.Mutex
	artifacts map[ID]*RPCArtifactStore
	settings  map[string]*sessions.Metadata
	// spawns caches, by parent, which spawn_agent call made each child:
	// what a parent's transcript says, read again when it grows.
	spawns map[ID]spawnCalls
}

type spawnCalls struct {
	lines  uint64
	byName map[string]string
}

// NewRPCStore reads sessions through the connections client gives.
func NewRPCStore(client ClientSource) *RPCStore {
	return &RPCStore{client: client, artifacts: map[ID]*RPCArtifactStore{}, settings: map[string]*sessions.Metadata{}, spawns: map[ID]spawnCalls{}}
}

var (
	_ sessions.SessionStore = (*RPCStore)(nil)
	_ sessions.ViewStore    = (*RPCStore)(nil)
	_ sessions.Session      = (*RPCSession)(nil)
	_ sessions.ViewIdentity = (*RPCSession)(nil)
	_ artifacts.Store       = (*RPCArtifactStore)(nil)
)

func (s *RPCStore) conn(ctx context.Context) (*Client, error) {
	if s.client == nil {
		return nil, errors.New("no control connection")
	}
	return s.client(ctx)
}

// notFound maps a backend's not_found refusal to the store's error.
func notFound(err error) error {
	if IsRefusal(err, RefusedNotFound) {
		return fmt.Errorf("%w: %v", sessions.ErrSessionNotFound, err)
	}
	return err
}

func (s *RPCStore) info(ctx context.Context, req InfoReq) (Info, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return Info{}, err
	}
	info, err := c.Info(ctx, req)
	return info, notFound(err)
}

func (s *RPCStore) list(ctx context.Context, filter string) (SessionList, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return SessionList{}, err
	}
	return c.Sessions(ctx, SessionsReq{Filter: filter})
}

// MetadataOf is the record's metadata as polly keeps it.
func MetadataOf(md RecordMetadata, final *Final) *sessions.Metadata {
	out := &sessions.Metadata{
		Name:         md.Name,
		Description:  md.Description,
		Parent:       md.Parent,
		Model:        md.Model,
		ActiveSkills: append([]string(nil), md.ActiveSkills...),
	}
	if md.LastUsedNs != 0 {
		out.LastUsed = time.Unix(0, md.LastUsedNs)
		out.Created = out.LastUsed
	}
	if final != nil {
		switch final.State {
		case StateDone:
			out.SpawnOutcome = sessions.ReportFinished
		case StateCancelled:
			out.SpawnOutcome = sessions.ReportCanceled
		case StateFailed:
			out.SpawnOutcome = sessions.ReportFailed
		}
		if final.Started != 0 {
			out.Created = time.UnixMilli(final.Started)
		}
	}
	return out
}

func entryMetadata(e SessionEntry) *sessions.Metadata {
	return MetadataOf(RecordMetadata{Name: e.Name, LastUsedNs: e.LastUsedNs, Description: e.Description, Parent: e.Parent, Model: e.Model, ActiveSkills: e.ActiveSkills, Daemon: e.Daemon}, nil)
}

// withSettings lays the settings this process set on a session (/set,
// /model) over what the backend's record says: they live only here.
func (s *RPCStore) withSettings(md *sessions.Metadata) *sessions.Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kept := s.settings[md.Name]; kept != nil {
		merged := *kept
		merged.Name, merged.LastUsed, merged.Created = md.Name, md.LastUsed, md.Created
		merged.Description, merged.Parent, merged.SpawnOutcome = md.Description, md.Parent, md.SpawnOutcome
		merged.ActiveSkills = md.ActiveSkills
		if md.Model != "" && merged.Model == "" {
			merged.Model = md.Model
		}
		return &merged
	}
	return md
}

// Kept reports whether this process set settings on the session named name.
func (s *RPCStore) Kept(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings[name] != nil
}

func (s *RPCStore) keepSettings(name string, md *sessions.Metadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := *md
	s.settings[name] = &copied
}

// Acquire opens a session by name. One that is not there is made by the
// backend on its first prompt, under an id drawn here.
func (s *RPCStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	var info Info
	var err error
	if opts.ExpectedID != "" {
		info, err = s.info(ctx, InfoReq{ID: opts.ExpectedID})
		if err == nil && name != "" && info.Metadata.Name != name {
			err = fmt.Errorf("%w: session %s is no longer named %s", sessions.ErrSessionNotFound, opts.ExpectedID, name)
		}
	} else if name == "" {
		// No name: a new session, named by the backend as it makes it.
		if opts.ExistingOnly {
			return nil, sessions.ErrSessionNotFound
		}
		err = sessions.ErrSessionNotFound
	} else {
		info, err = s.info(ctx, InfoReq{Name: name})
	}
	switch {
	case err == nil:
		if opts.ExpectedID == "" && opts.Auto {
			return nil, sessions.ErrSessionExists
		}
		md := s.withSettings(MetadataOf(info.Metadata, info.Final))
		return s.session(info.ID, info.ParentID, md, false), nil
	case errors.Is(err, sessions.ErrSessionNotFound) && !opts.ExistingOnly && opts.ExpectedID == "":
		if opts.Parent != "" {
			return nil, fmt.Errorf("making a child session: %w", ErrUnsupported)
		}
		id, err := NewID()
		if err != nil {
			return nil, err
		}
		md := s.withSettings(&sessions.Metadata{Name: name, Created: time.Now(), LastUsed: time.Now()})
		return s.session(id, "", md, true), nil
	default:
		return nil, err
	}
}

func (s *RPCStore) session(id, parentID ID, md *sessions.Metadata, pending bool) *RPCSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &RPCSession{store: s, id: id, parentID: parentID, md: md, pending: pending, ctx: ctx, cancel: cancel}
}

// Delete is not served: the backend's sessions are its own.
func (s *RPCStore) Delete(context.Context, string) error {
	return fmt.Errorf("deleting a session: %w", ErrUnsupported)
}

// List names every session.
func (s *RPCStore) List(ctx context.Context) ([]string, error) {
	list, err := s.list(ctx, "all")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Sessions))
	for _, e := range list.Sessions {
		names = append(names, e.Name)
	}
	return names, nil
}

// InUse reports whether a turn holds the session with id now.
func (s *RPCStore) InUse(ctx context.Context, id ID) (bool, error) {
	info, err := s.info(ctx, InfoReq{ID: id})
	return info.InUse, err
}

// Exists reports whether a session has the name.
func (s *RPCStore) Exists(ctx context.Context, name string) (bool, error) {
	_, err := s.info(ctx, InfoReq{Name: name})
	if errors.Is(err, sessions.ErrSessionNotFound) {
		return false, nil
	}
	return err == nil, err
}

// GetMetadata reads one session's metadata by name.
func (s *RPCStore) GetMetadata(ctx context.Context, name string) (*sessions.Metadata, error) {
	info, err := s.info(ctx, InfoReq{Name: name})
	if err != nil {
		return nil, err
	}
	return s.withSettings(MetadataOf(info.Metadata, info.Final)), nil
}

// GetAllMetadata reads every session's metadata, by name.
func (s *RPCStore) GetAllMetadata(ctx context.Context) (map[string]*sessions.Metadata, error) {
	list, err := s.list(ctx, "all")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*sessions.Metadata, len(list.Sessions))
	for _, e := range list.Sessions {
		out[e.Name] = s.withSettings(entryMetadata(e))
	}
	return out, nil
}

// ListSummaries lists every session with its length and whether a turn
// holds it.
func (s *RPCStore) ListSummaries(ctx context.Context) ([]sessions.SessionSummary, error) {
	list, err := s.list(ctx, "all")
	if err != nil {
		return nil, err
	}
	out := make([]sessions.SessionSummary, 0, len(list.Sessions))
	lines := map[ID]uint64{}
	for _, e := range list.Sessions {
		lines[e.ID] = e.Lines
	}
	for _, e := range list.Sessions {
		md := s.withSettings(entryMetadata(e))
		if e.ParentID != "" {
			md.SpawnCallID = s.spawnCallOf(ctx, e.ParentID, lines[e.ParentID], e.Name)
		}
		out = append(out, sessions.SessionSummary{ID: e.ID, ParentID: e.ParentID, Metadata: md, MessageCount: int(e.Lines), InUse: e.InUse})
	}
	return out, nil
}

// spawnCallOf is the parent's spawn_agent call that made the child named
// name: the call whose result names the child's session. Empty where the
// parent's transcript does not say.
func (s *RPCStore) spawnCallOf(ctx context.Context, parent ID, lines uint64, name string) string {
	s.mu.Lock()
	cached, ok := s.spawns[parent]
	s.mu.Unlock()
	if !ok || cached.lines != lines {
		history, err := s.History(ctx, parent, 0, nil)
		if err != nil {
			return ""
		}
		cached = spawnCalls{lines: lines, byName: SpawnedSessions(history)}
		s.mu.Lock()
		s.spawns[parent] = cached
		s.mu.Unlock()
	}
	return cached.byName[name]
}

// SpawnedSessions reads, from a transcript, which spawn_agent call made
// each child session it names: by session name, the call's id.
func SpawnedSessions(history []messages.ChatMessage) map[string]string {
	out := map[string]string{}
	for _, msg := range history {
		if msg.Role != messages.MessageRoleTool || msg.ToolCallID == "" {
			continue
		}
		if m := childSessionName.FindStringSubmatch(msg.GetContent()); m != nil {
			out[m[1]] = msg.ToolCallID
		}
	}
	return out
}

// SpawnedSession is the child session a spawn_agent result names, or "".
func SpawnedSession(result string) string {
	if m := childSessionName.FindStringSubmatch(result); m != nil {
		return m[1]
	}
	return ""
}

// GetLast names the root session used last, or "" for none.
func (s *RPCStore) GetLast(ctx context.Context) (string, error) {
	info, err := s.info(ctx, InfoReq{Last: true})
	if errors.Is(err, sessions.ErrSessionNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return info.Metadata.Name, nil
}

// Expire does nothing: the backend keeps its own sessions.
func (s *RPCStore) Expire(context.Context) error { return nil }

// Close does nothing: the connections are their source's.
func (s *RPCStore) Close() error { return nil }

// Revision is a view's revision: whatever a transcript or its record moves
// on changes it.
func Revision(info Info) string {
	h := sha256.New()
	final, _ := json.Marshal(info.Final)
	for _, part := range []string{info.ID, info.Metadata.Name, strconv.FormatInt(info.Metadata.LastUsedNs, 10), strconv.FormatUint(info.Lines, 10), strconv.FormatBool(info.InUse), string(final)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var childSessionName = regexp.MustCompile(`\(agent session ([^\s·)]+)`)

// ReadView reads a consistent snapshot of a session. Matching knownRevision
// leaves the history out.
func (s *RPCStore) ReadView(ctx context.Context, target sessions.ViewTarget, knownRevision string) (*sessions.SessionView, error) {
	var info Info
	var err error
	switch {
	case target.ID != "":
		info, err = s.info(ctx, InfoReq{ID: target.ID})
	case target.Parent != "" && target.SpawnCallID != "":
		info, err = s.childBySpawn(ctx, target.Parent, target.SpawnCallID)
	case target.Name != "":
		info, err = s.info(ctx, InfoReq{Name: target.Name})
	default:
		return nil, sessions.ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	view := &sessions.SessionView{
		ID:        info.ID,
		Revision:  Revision(info),
		Sequence:  int64(info.Lines),
		ParentID:  info.ParentID,
		Metadata:  s.withSettings(MetadataOf(info.Metadata, info.Final)),
		Artifacts: s.Artifacts(info.ID),
		InUse:     info.InUse,
	}
	if info.ParentID != "" && target.SpawnCallID != "" {
		view.Metadata.SpawnCallID = target.SpawnCallID
	}
	view.Unchanged = view.Revision == knownRevision
	if view.Unchanged {
		return view, nil
	}
	upto := info.Lines
	if view.History, err = s.History(ctx, info.ID, 0, &upto); err != nil {
		return nil, err
	}
	return view, nil
}

// History reads a session's lines from from on, none at or past upto, as
// polly's messages.
func (s *RPCStore) History(ctx context.Context, id ID, from uint64, upto *uint64) ([]messages.ChatMessage, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	lines, err := c.HistoryFrom(ctx, id, from, upto)
	if err != nil {
		return nil, notFound(err)
	}
	refs := s.Artifacts(id)
	out := make([]messages.ChatMessage, 0, len(lines))
	for i, raw := range lines {
		msg, err := LineToMessage(raw, id, refs)
		if err != nil {
			return nil, fmt.Errorf("session %s line %d: %w", id, from+uint64(i), err)
		}
		out = append(out, msg)
	}
	return out, nil
}

// childBySpawn finds the session a parent's spawn_agent call made: the
// result of that call names it.
func (s *RPCStore) childBySpawn(ctx context.Context, parent, spawnCallID string) (Info, error) {
	p, err := s.info(ctx, InfoReq{Name: parent})
	if err != nil {
		return Info{}, err
	}
	history, err := s.History(ctx, p.ID, 0, nil)
	if err != nil {
		return Info{}, err
	}
	for _, msg := range history {
		if msg.Role != messages.MessageRoleTool || msg.ToolCallID != spawnCallID {
			continue
		}
		if m := childSessionName.FindStringSubmatch(msg.GetContent()); m != nil {
			return s.info(ctx, InfoReq{Name: m[1]})
		}
	}
	return Info{}, fmt.Errorf("%w: no agent session of call %s", sessions.ErrSessionNotFound, spawnCallID)
}

// Artifacts is the artifact store of session id: the outputs the backend
// saved with it, read on demand.
func (s *RPCStore) Artifacts(id ID) *RPCArtifactStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.artifacts[id]; a != nil {
		return a
	}
	a := &RPCArtifactStore{store: s, session: id, outputs: map[string]outputRef{}, blobs: map[string][]byte{}}
	s.artifacts[id] = a
	return a
}

// NewID draws a session id: 32 lowercase hex digits.
func NewID() (ID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("draw a session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ------------------------------------------------------------- sessions

// RPCSession is a session of a control backend. It holds no lease: the
// backend holds its session while a turn runs, and writes the transcript
// itself, so the writes of polly's store contract do nothing here.
type RPCSession struct {
	store    *RPCStore
	id       ID
	parentID ID
	ctx      context.Context
	cancel   context.CancelFunc

	mu sync.Mutex
	md *sessions.Metadata
	// pending is a session the backend makes on its first prompt.
	pending bool
	// hideFrom, when set, hides the lines from there on from GetHistory:
	// the lines of a turn that runs elsewhere, which a follower tells as
	// that turn's own.
	hideFrom *uint64
}

// HideFrom hides the session's lines from line from on until ShowAll.
func (s *RPCSession) HideFrom(from uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hideFrom = &from
}

// ShowAll shows every line again.
func (s *RPCSession) ShowAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hideFrom = nil
}

// ID is the session's id.
func (s *RPCSession) ID() ID { return s.id }

// ViewID is the session's id, by which a view reads it.
func (s *RPCSession) ViewID() string { return s.id }

// Pending reports a session the backend has not made yet.
func (s *RPCSession) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// Made records that the backend made the session, under name.
func (s *RPCSession) Made(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = false
	if name != "" {
		s.md.Name = name
	}
	s.md.LastUsed = time.Now()
}

// Context ends when the session is closed.
func (s *RPCSession) Context() context.Context { return s.ctx }

// GetHistory reads the session's transcript.
func (s *RPCSession) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	if s.Pending() {
		return nil, nil
	}
	s.mu.Lock()
	upto := s.hideFrom
	s.mu.Unlock()
	return s.store.History(ctx, s.id, 0, upto)
}

// AddMessage does nothing: the backend writes the transcript as its turn runs.
func (s *RPCSession) AddMessage(context.Context, messages.ChatMessage) error { return nil }

// AddMessages does nothing, as AddMessage.
func (s *RPCSession) AddMessages(context.Context, []messages.ChatMessage) error { return nil }

// Clear is not served.
func (s *RPCSession) Clear(context.Context) error {
	return fmt.Errorf("clearing a session: %w", ErrUnsupported)
}

// Reset is not served.
func (s *RPCSession) Reset(context.Context, *sessions.Metadata) error {
	return fmt.Errorf("resetting a session: %w", ErrUnsupported)
}

// Close lets the session go.
func (s *RPCSession) Close() error {
	s.cancel()
	return nil
}

// GetName is the session's name.
func (s *RPCSession) GetName(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.md.Name, nil
}

// Rename is not served.
func (s *RPCSession) Rename(context.Context, string) error {
	return fmt.Errorf("renaming a session: %w", ErrUnsupported)
}

// GetMetadata is what the record says, with the settings this process set.
func (s *RPCSession) GetMetadata(ctx context.Context) (*sessions.Metadata, error) {
	if !s.Pending() {
		if info, err := s.store.info(ctx, InfoReq{ID: s.id}); err == nil {
			md := s.store.withSettings(MetadataOf(info.Metadata, info.Final))
			s.mu.Lock()
			s.md = md
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := *s.md
	return &copied, nil
}

// SetMetadata keeps the settings in this process: the backend's record is
// its own, and a prompt carries what a turn runs with.
func (s *RPCSession) SetMetadata(_ context.Context, md *sessions.Metadata) error {
	s.mu.Lock()
	name := s.md.Name
	copied := *md
	copied.Name = name
	s.md = &copied
	s.mu.Unlock()
	s.store.keepSettings(name, &copied)
	return nil
}

// CacheSessionID is the session's id.
func (s *RPCSession) CacheSessionID(context.Context) (string, error) { return s.id, nil }

// ArtifactStore reads the outputs the backend saved with the session.
func (s *RPCSession) ArtifactStore() artifacts.Store { return s.store.Artifacts(s.id) }

// ----------------------------------------------------------- artifacts

type outputRef struct {
	name  string
	bytes int64
}

// RPCArtifactStore reads a session's saved outputs over the connection, by
// the references the converter made for them, and keeps what this process
// put (an image pasted into a prompt) in memory until it crosses inline.
type RPCArtifactStore struct {
	store   *RPCStore
	session ID

	mu      sync.Mutex
	outputs map[string]outputRef
	blobs   map[string][]byte
}

// maxOutputRead bounds one saved output read whole.
const maxOutputRead = 64 << 20

// OutputRef is the reference of a saved output of the session's.
func (a *RPCArtifactStore) OutputRef(output string, size int64, mimeType, fileName string) artifacts.Ref {
	sum := sha256.Sum256([]byte("polly control output\x00" + a.session + "\x00" + output))
	id := "sha256:" + hex.EncodeToString(sum[:])
	a.mu.Lock()
	a.outputs[id] = outputRef{name: output, bytes: size}
	a.mu.Unlock()
	if fileName == "" {
		fileName = output
	}
	return artifacts.Ref{ID: id, Kind: artifacts.KindImage, MIMEType: mimeType, Name: fileName, ImageToken: "[image " + output + "]", Bytes: size}
}

// Put keeps a blob in memory, for a prompt that carries it inline.
func (a *RPCArtifactStore) Put(_ context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	ref := artifacts.RefForBlob(blob)
	a.mu.Lock()
	a.blobs[ref.ID] = append([]byte(nil), blob.Data...)
	a.mu.Unlock()
	return ref, nil
}

// Open reads an artifact: one this process put, or a saved output fetched
// from the backend once and kept.
func (a *RPCArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	a.mu.Lock()
	if data, ok := a.blobs[id]; ok {
		a.mu.Unlock()
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	ref, ok := a.outputs[id]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: artifact %s", sessions.ErrSessionNotFound, id)
	}
	c, err := a.store.conn(ctx)
	if err != nil {
		return nil, err
	}
	data, err := c.OutputAll(ctx, a.session, ref.name, maxOutputRead)
	if err != nil {
		return nil, notFound(err)
	}
	a.mu.Lock()
	a.blobs[id] = data
	a.mu.Unlock()
	return io.NopCloser(bytes.NewReader(data)), nil
}

// RemoveAll does nothing: the backend's outputs are its own.
func (a *RPCArtifactStore) RemoveAll(context.Context) error { return nil }
