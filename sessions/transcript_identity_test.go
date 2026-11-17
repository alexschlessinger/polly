package sessions

import (
	"context"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestTranscriptIdentityAtomicAndDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session := acquireNamed(t, store, "child")
	stale, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coord := session.(CoordinationSession)
	identity := TranscriptIdentity{Execution: "first", Generation: 1}
	if err := coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Transcript = &identity
		s.Append = messages.User("augmented task")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A stale settings writer cannot erase the checkpoint's newer identity.
	stale.Description = "updated settings"
	if err := session.SetMetadata(ctx, stale); err != nil {
		t.Fatal(err)
	}
	view, err := store.ReadView(ctx, ViewTarget{Name: "child"}, "")
	if err != nil || view.Metadata.Transcript == nil || *view.Metadata.Transcript != (TranscriptIdentity{Execution: "first", Generation: 1, Sequence: view.Sequence}) {
		t.Fatalf("checkpoint identity: view=%+v err=%v", view, err)
	}
	// Encoding fails after the identity write, so both must roll back.
	if err := coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Transcript = &TranscriptIdentity{Execution: "uncommitted", Generation: 2}
		s.Append = []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Metadata: map[string]any{"bad": make(chan struct{})}}}
		return nil
	}); err == nil {
		t.Fatal("invalid append committed")
	}
	unchanged, err := store.ReadView(ctx, ViewTarget{ID: view.ID}, view.Revision)
	if err != nil || !unchanged.Unchanged || *unchanged.Metadata.Transcript != *view.Metadata.Transcript {
		t.Fatalf("failed append changed saved identity: %+v %v", unchanged, err)
	}
	// A generation can advance with no new messages at a request checkpoint.
	if err := coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Transcript = &TranscriptIdentity{Execution: "first", Generation: 2}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	saved, err := reader.ReadView(ctx, ViewTarget{ID: view.ID}, view.Revision)
	if err != nil || saved.Unchanged || saved.Metadata.Transcript == nil || *saved.Metadata.Transcript != (TranscriptIdentity{Execution: "first", Generation: 2, Sequence: saved.Sequence}) {
		t.Fatalf("reopened identity: %+v %v", saved, err)
	}
}

// Binding a transcript writes the binding alone: every other setting reads
// back as it was stored, whatever its text looks like as JSON.
func TestTranscriptIdentityLeavesOtherSettingsAsStored(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session := acquireNamed(t, store, "child")
	metadata, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SystemPrompt = "quotes \" and \\ slashes, <html> & 'entities', \u2028 separators, emoji \U0001F642, \u00e9\n\ttabs"
	metadata.Description = `{"looks":"like json","n":1e3,"transcript":null}`
	metadata.Temperature = 0.1
	metadata.MaxTokens = 1 << 53
	metadata.ToolTimeout = time.Duration(math.MaxInt64)
	metadata.ActiveSkills = []string{"", "$.transcript"}
	if err := session.SetMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	want, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coord := session.(CoordinationSession)
	var sequence int64
	for generation := 1; generation <= 2; generation++ {
		if err := coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
			s.Transcript = &TranscriptIdentity{Execution: "execution", Generation: generation}
			s.Append = messages.User("task")
			sequence = s.Sequence + 1
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want.Transcript = &TranscriptIdentity{Execution: "execution", Generation: 2, Sequence: sequence}
	want.LastUsed = got.LastUsed
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings after binding:\n got %+v\nwant %+v", got, want)
	}
}

func TestTranscriptIdentityResetsAndIndependentAppends(t *testing.T) {
	for _, operation := range []string{"clear", "reset", "append"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			session := acquireNamed(t, store, "child")
			if err := session.(CoordinationSession).UpdateCoordination(ctx, func(s *CoordinationState) error {
				s.Transcript = &TranscriptIdentity{Execution: "old", Generation: 1}
				s.Append = messages.User("old task")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "clear":
				err = session.Clear(ctx)
			case "reset":
				metadata, e := session.GetMetadata(ctx)
				if e != nil {
					t.Fatal(e)
				}
				err = session.Reset(ctx, metadata)
			case "append":
				err = session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "independent task"})
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := store.ReadView(ctx, ViewTarget{Name: "child"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if identity := view.Metadata.Transcript; identity != nil && identity.Sequence == view.Sequence {
				t.Fatalf("%s retained old transcript identity: %+v", operation, identity)
			}
		})
	}
}
