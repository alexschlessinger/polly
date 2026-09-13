package sessions

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestReadArtifactPublishedAccessAndPaging(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(StoreConfig{Mode: ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	acquire := func(name, parent string) Session {
		t.Helper()
		s, err := store.Acquire(ctx, name, AcquireOptions{Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	parent := acquire("parent", "")
	author := acquire("author", "parent")
	peer := acquire("peer", "parent")
	outsider := acquire("outsider", "")
	put := func(blob artifacts.Blob) artifacts.Ref {
		t.Helper()
		ref, err := author.ArtifactStore().Put(ctx, blob)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	text := put(artifacts.Blob{Kind: artifacts.KindText, Data: []byte("first\nneedle\nlast\n")})
	long := put(artifacts.Blob{Kind: artifacts.KindText, Data: []byte(strings.Repeat("x", 2<<20) + "\nneedle after long line\n")})
	private := put(artifacts.Blob{Kind: artifacts.KindText, Data: []byte("private evidence")})
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	image := put(artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: png})
	if err := author.(CoordinationSession).UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Pins = []string{text.ID, long.ID, image.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Session{parent, peer} {
		agent := llm.NewAgent(nil, nil, llm.AgentConfig{ArtifactStore: s.ArtifactStore(), OpenArtifact: s.(CoordinationSession).OpenPublishedArtifact})
		t.Cleanup(func() { agent.Close() })
		tool, ok := agent.ToolRegistry().Get("read_artifact")
		if !ok {
			t.Fatal("read_artifact missing")
		}
		for _, tc := range []struct {
			args map[string]any
			want string
		}{
			{map[string]any{"id": text.ID, "offset": 2, "limit": 1}, "2: needle\n"},
			{map[string]any{"id": text.ID, "query": "needle"}, "2: needle\n"},
			{map[string]any{"id": text.ID, "byte_offset": 6}, "needle\nlast\n"},
			{map[string]any{"id": long.ID, "query": "needle"}, "2: needle after long line\n"},
			{map[string]any{"id": long.ID, "byte_offset": 2 << 20}, "\nneedle after long line\n"},
		} {
			out, err := tool.Execute(ctx, tc.args)
			if err != nil || !strings.Contains(out, tc.want) || len(out) > tools.PageMaxBytes {
				t.Fatalf("read %v = %q, %v", tc.args, out, err)
			}
		}
		out, err := tool.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"id": image.ID})
		if err != nil || len(out.Media) != 1 || out.Media[0].MIMEType != "image/png" || !bytes.Equal(out.Media[0].Data, png) {
			t.Fatalf("published image = %+v, %v", out, err)
		}
		if out, err := tool.Execute(ctx, map[string]any{"id": private.ID}); err == nil || strings.Contains(out, "private evidence") {
			t.Fatalf("private artifact exposed: %q, %v", out, err)
		}
		if r, err := s.ArtifactStore().Open(ctx, private.ID); err == nil {
			r.Close()
			t.Fatal("failed read granted ownership of private artifact")
		}
	}
	if _, r, err := outsider.(CoordinationSession).OpenPublishedArtifact(ctx, text.ID); err == nil {
		r.Close()
		t.Fatal("publication crossed swarm boundary")
	}
	// Acquired session ownership is not enough for ordinary conversation recall.
	// Without a reference or host callback, even an earlier imported artifact is private.
	agent := llm.NewAgent(nil, nil, llm.AgentConfig{ArtifactStore: peer.ArtifactStore()})
	defer agent.Close()
	reader, _ := agent.ToolRegistry().Get("read_artifact")
	if _, err := reader.Execute(ctx, map[string]any{"id": text.ID}); err == nil {
		t.Fatal("session ownership bypassed conversation reference check")
	}
	// Publication retains the bytes after the publishing session is removed.
	author.Close()
	if err := store.Delete(ctx, "author"); err != nil {
		t.Fatal(err)
	}
	ref, r, err := parent.(CoordinationSession).OpenPublishedArtifact(ctx, text.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || closeErr != nil || string(data) != "first\nneedle\nlast\n" || ref.Bytes != int64(len(data)) || ref.Kind != artifacts.KindText {
		t.Fatalf("retained publication = %+v, %q, %v, %v", ref, data, err, closeErr)
	}
}
