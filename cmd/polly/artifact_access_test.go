package main

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestConversationReadArtifactOpensPublishedEvidence(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	state, err := (&conversationOpener{config: &Config{NoSkills: true, NoSandbox: true}, sessionStore: store, cmd: getCommand()}).openNew(ctx, "artifact-parent", false)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	author, err := store.Acquire(ctx, "artifact-author", sessions.AcquireOptions{Parent: "artifact-parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer author.Close()
	ref, err := author.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("shared evidence\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := author.(sessions.CoordinationSession).UpdateCoordination(ctx, func(s *sessions.CoordinationState) error {
		s.Pins = []string{ref.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	registry := state.effectiveTools()
	if _, ok := registry.Get("swarm_read_artifact"); ok {
		t.Fatal("duplicate swarm artifact tool is still registered")
	}
	reader, ok := registry.Get("read_artifact")
	if !ok {
		t.Fatal("read_artifact missing")
	}
	if out, err := reader.Execute(ctx, map[string]any{"id": ref.ID, "query": "evidence"}); err != nil || out != "1: shared evidence\n" {
		t.Fatalf("parent read = %q, %v", out, err)
	}
}
