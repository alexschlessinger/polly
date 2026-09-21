package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm/replay"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// A shot fixture gives a headless run its state: sessions seeded into the
// store before the run opens, and model turns the replay provider plays when
// the script types a prompt. The fixture decides the launch context and the
// launch model, so the run needs neither a credential nor a chosen context.
// The file format is documented in docs/CLI.md under --shot-fixture.

// shotFixture is one parsed fixture file.
type shotFixture struct {
	// Name names the replay model, replay/<Name>; empty takes the file's name.
	Name     string        `json:"name,omitempty"`
	Sessions []shotSession `json:"sessions,omitempty"`
	Turns    []replay.Turn `json:"turns,omitempty"`
}

// shotSession is one store record to seed.
type shotSession struct {
	Name string `json:"name"`
	// Launch marks the context the run opens; at most one, defaulting to
	// the first session.
	Launch bool `json:"launch,omitempty"`
	// Parent names an earlier session the store links this one under.
	Parent string `json:"parent,omitempty"`
	// Metadata is sessions.Metadata in its JSON form, overlaid on the launch
	// defaults field by field; Name is always the entry's own and Model
	// defaults to the replay model.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// History is appended verbatim.
	History []messages.ChatMessage `json:"history,omitempty"`
	// Artifacts are the payloads History's parts reference, stored into the
	// session's artifact store before the history. Artifact ids are content
	// addressed, so a stored payload lands under the id the parts carry.
	Artifacts []shotArtifact `json:"artifacts,omitempty"`
}

// shotArtifact is one artifact payload with the metadata its Ref is built
// from; Data is base64 in the file.
type shotArtifact struct {
	ID         string         `json:"id,omitempty"`
	Kind       artifacts.Kind `json:"kind"`
	MIMEType   string         `json:"mime_type,omitempty"`
	Name       string         `json:"name,omitempty"`
	ImageToken string         `json:"image_token,omitempty"`
	Reference  string         `json:"reference,omitempty"`
	Data       []byte         `json:"data"`
}

func (a shotArtifact) blob() artifacts.Blob {
	return artifacts.Blob{Kind: a.Kind, MIMEType: a.MIMEType, Name: a.Name, ImageToken: a.ImageToken, Reference: a.Reference, Data: a.Data}
}

// parseShotFixture decodes and validates a fixture, so every mistake is
// reported before anything is seeded or played.
func parseShotFixture(data []byte) (*shotFixture, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f shotFixture
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse fixture: %w", err)
	}
	if len(f.Sessions) == 0 && len(f.Turns) == 0 {
		return nil, errors.New("fixture: has neither sessions nor turns")
	}
	seen := map[string]bool{}
	launches := 0
	for i, s := range f.Sessions {
		if s.Name == "" {
			return nil, fmt.Errorf("fixture: sessions[%d]: name is required", i)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("fixture: sessions[%d]: duplicate name %q", i, s.Name)
		}
		if s.Parent != "" && !seen[s.Parent] {
			return nil, fmt.Errorf("fixture: sessions[%d]: parent %q is not an earlier session", i, s.Parent)
		}
		if len(s.Metadata) > 0 {
			dec := json.NewDecoder(bytes.NewReader(s.Metadata))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&sessions.Metadata{}); err != nil {
				return nil, fmt.Errorf("fixture: sessions[%d].metadata: %w", i, err)
			}
		}
		for j, a := range s.Artifacts {
			switch a.Kind {
			case artifacts.KindText, artifacts.KindImage, artifacts.KindBinary:
			default:
				return nil, fmt.Errorf("fixture: sessions[%d].artifacts[%d]: unknown kind %q", i, j, a.Kind)
			}
			if len(a.Data) == 0 {
				return nil, fmt.Errorf("fixture: sessions[%d].artifacts[%d]: no data", i, j)
			}
			if a.ID != "" && artifacts.RefForBlob(a.blob()).ID != a.ID {
				return nil, fmt.Errorf("fixture: sessions[%d].artifacts[%d]: data does not hash to %s", i, j, a.ID)
			}
		}
		seen[s.Name] = true
		if s.Launch {
			launches++
		}
	}
	if launches > 1 {
		return nil, errors.New("fixture: more than one session is marked launch")
	}
	if err := replay.Validate(f.Turns); err != nil {
		return nil, fmt.Errorf("fixture: %w", err)
	}
	return &f, nil
}

// launch is the session the run opens, or nil for a fixture without one.
func (f *shotFixture) launch() *shotSession {
	for i := range f.Sessions {
		if f.Sessions[i].Launch {
			return &f.Sessions[i]
		}
	}
	if len(f.Sessions) > 0 {
		return &f.Sessions[0]
	}
	return nil
}

// loadShotFixture parses config.ShotFixture, installs its turns as the
// replay model and points the launch at it. It returns the name of the
// context the run opens, or "" when the fixture seeds no session.
func loadShotFixture(config *Config) (string, error) {
	if config.ShotScript == "" {
		return "", errors.New("--shot-fixture seeds a --shot-script run: pass the script too")
	}
	data, err := os.ReadFile(config.ShotFixture)
	if err != nil {
		return "", fmt.Errorf("read shot fixture: %w", err)
	}
	fixture, err := parseShotFixture(data)
	if err != nil {
		return "", err
	}
	name := fixture.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(config.ShotFixture), filepath.Ext(config.ShotFixture))
	}
	if name == "" || strings.ContainsAny(name, "/ \t") {
		return "", fmt.Errorf("fixture name %q must be one word without a slash", name)
	}
	if len(fixture.Sessions) > 0 && (config.ContextID != "" || config.UseLastContext) {
		return "", errors.New("the fixture seeds its own sessions and opens one of them: drop --context and --last")
	}
	bus := replay.NewBus()
	replay.Install(name, fixture.Turns, bus)
	config.shotFixture, config.shotBus = fixture, bus
	config.Launch.Model, config.Launch.ModelHost = "replay/"+name, ""
	if launch := fixture.launch(); launch != nil {
		return launch.Name, nil
	}
	return "", nil
}

// seedShotFixture writes every fixture session into store, in fixture order
// so a parent exists before the child linked under it. A session already
// stored under a fixture name is replaced.
func seedShotFixture(ctx context.Context, store sessions.SessionStore, config *Config) error {
	for _, entry := range config.shotFixture.Sessions {
		if err := seedShotSession(ctx, store, config, entry); err != nil {
			return fmt.Errorf("seed fixture session %q: %w", entry.Name, err)
		}
	}
	return nil
}

func seedShotSession(ctx context.Context, store sessions.SessionStore, config *Config, entry shotSession) (retErr error) {
	exists, err := store.Exists(ctx, entry.Name)
	if err != nil {
		return err
	}
	if exists {
		if err := store.Delete(ctx, entry.Name); err != nil {
			return fmt.Errorf("replace stored session: %w", err)
		}
	}
	info := metadataFromConfig(config)
	if len(entry.Metadata) > 0 {
		// Unmarshalling over the defaults keeps every field the fixture
		// leaves out.
		if err := json.Unmarshal(entry.Metadata, info); err != nil {
			return fmt.Errorf("metadata: %w", err)
		}
	}
	info.Name = entry.Name
	// A fixture with turns is answered by the replay model, whatever the
	// session ran on; one without keeps the stored model, which is only
	// ever shown.
	if info.Model == "" || len(config.shotFixture.Turns) > 0 {
		info.Model, info.ModelHost = config.Launch.Model, ""
	}
	now := time.Now()
	info.Created, info.LastUsed = now, now

	session, err := store.Acquire(ctx, entry.Name, sessions.AcquireOptions{Parent: entry.Parent})
	if err != nil {
		return err
	}
	defer func() {
		if err := session.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close: %w", err))
		}
	}()
	if err := session.Reset(ctx, info); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	// The store owns the title: a settings write keeps the stored one.
	if info.Title != "" {
		titled, ok := session.(sessions.TitleSession)
		if !ok {
			return errors.New("the store cannot title a session")
		}
		source := info.TitleSource
		if source == "" {
			source = sessions.TitleSourceUser
		}
		if _, err := titled.SetTitle(ctx, info.Title, source); err != nil {
			return fmt.Errorf("write title: %w", err)
		}
	}
	for _, artifact := range entry.Artifacts {
		if _, err := session.ArtifactStore().Put(ctx, artifact.blob()); err != nil {
			return fmt.Errorf("write artifact %s: %w", artifact.Name, err)
		}
	}
	if len(entry.History) > 0 {
		if err := session.AddMessages(ctx, entry.History); err != nil {
			return fmt.Errorf("write history: %w", err)
		}
	}
	return nil
}

// handleExportContext prints contextID and the agents it spawned as a shot
// fixture, so a state reached in a real run can be replayed off-screen.
// Machine-bound and store-owned metadata is left out.
func handleExportContext(ctx context.Context, store sessions.SessionStore, contextID string, withArtifacts bool) error {
	fixture, err := exportShotFixture(ctx, store, contextID, withArtifacts)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fixture: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

func exportShotFixture(ctx context.Context, store sessions.SessionStore, contextID string, withArtifacts bool) (*shotFixture, error) {
	reader, ok := store.(sessions.ViewStore)
	if !ok {
		return nil, errors.New("the context store cannot be read without a lease")
	}
	root, err := reader.ReadView(ctx, sessions.ViewTarget{Name: contextID}, "")
	if errors.Is(err, sessions.ErrSessionNotFound) {
		return nil, fmt.Errorf("context '%s' not found", contextID)
	}
	if err != nil {
		return nil, fmt.Errorf("read context: %w", err)
	}
	summaries, err := store.ListSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list contexts: %w", err)
	}
	fixture := &shotFixture{}
	// Breadth-first over the parent links keeps every parent ahead of its
	// children, which is the order the seeder needs.
	type pending struct {
		view   *sessions.SessionView
		parent string
	}
	queue := []pending{{view: root}}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		entry, err := exportShotSession(next.view, next.parent)
		if err != nil {
			return nil, err
		}
		if withArtifacts {
			if entry.Artifacts, err = exportShotArtifacts(ctx, next.view); err != nil {
				return nil, fmt.Errorf("export artifacts of %s: %w", next.view.Metadata.Name, err)
			}
		}
		entry.Launch = next.view == root
		fixture.Sessions = append(fixture.Sessions, entry)
		for _, summary := range summaries {
			if summary.ParentID != next.view.ID {
				continue
			}
			child, err := reader.ReadView(ctx, sessions.ViewTarget{ID: summary.ID}, "")
			if errors.Is(err, sessions.ErrSessionNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read agent %s: %w", summary.Metadata.Name, err)
			}
			queue = append(queue, pending{view: child, parent: next.view.Metadata.Name})
		}
	}
	return fixture, nil
}

// exportedMetadataOmits lists the metadata keys a fixture must not carry:
// store-owned identity and the machine's paths.
var exportedMetadataOmits = []string{
	"name", "created", "lastUsed", "parent", "changeBaseline", "workspaceChanges",
	"extraReadDirs", "skillDirs", "skillSources",
}

func exportShotSession(view *sessions.SessionView, parent string) (shotSession, error) {
	raw, err := json.Marshal(view.Metadata)
	if err != nil {
		return shotSession{}, fmt.Errorf("encode metadata: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return shotSession{}, fmt.Errorf("decode metadata: %w", err)
	}
	for _, key := range exportedMetadataOmits {
		delete(fields, key)
	}
	metadata, err := json.Marshal(fields)
	if err != nil {
		return shotSession{}, fmt.Errorf("encode metadata: %w", err)
	}
	entry := shotSession{Name: view.Metadata.Name, Parent: parent, Metadata: metadata}
	// The seeder writes the system prompt from the metadata, so the stored
	// system message would be doubled.
	for _, msg := range view.History {
		if msg.Role != messages.MessageRoleSystem {
			entry.History = append(entry.History, msg)
		}
	}
	return entry, nil
}

// exportShotArtifacts reads every artifact the view's history references, in
// first-reference order, once each.
func exportShotArtifacts(ctx context.Context, view *sessions.SessionView) ([]shotArtifact, error) {
	var out []shotArtifact
	seen := map[string]bool{}
	for _, msg := range view.History {
		for _, part := range msg.Parts {
			ref := part.Artifact
			if ref == nil || seen[ref.ID] {
				continue
			}
			seen[ref.ID] = true
			reader, err := view.Artifacts.Open(ctx, ref.ID)
			if err != nil {
				return nil, fmt.Errorf("open %s (%s): %w", ref.ID, ref.Name, err)
			}
			data, err := io.ReadAll(reader)
			if closeErr := reader.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return nil, fmt.Errorf("read %s (%s): %w", ref.ID, ref.Name, err)
			}
			out = append(out, shotArtifact{
				ID: ref.ID, Kind: ref.Kind, MIMEType: ref.MIMEType, Name: ref.Name,
				ImageToken: ref.ImageToken, Reference: ref.Reference, Data: data,
			})
		}
	}
	return out, nil
}
