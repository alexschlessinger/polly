package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestPublishedFilesSurviveWorkspaceRelease(t *testing.T) {
	t.Parallel()
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "checkout"}[git], func(t *testing.T) {
			ctx := context.Background()
			var r *Runtime
			var scratch string
			var published Publication
			contents := [][]byte{[]byte("{\r\n  \"command\": \"project-check --input input.bin\"\r\n}"), {0, 255, 128, '\r', '\n', 0}, {}}
			model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				for _, msg := range req.Messages {
					if msg.ToolName == "swarm_publish" {
						if err := json.Unmarshal([]byte(msg.GetContent()), &published); err != nil || len(published.Artifacts) != len(contents) {
							t.Errorf("publication result = %s, %v", msg.GetContent(), err)
						}
						return answer("Restore reproduction.json and input.bin; requires project-check, command: project-check --input input.bin; publication " + published.ID)
					}
				}
				s, err := r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				scratch = onlyContext(t, s).Scratch
				paths := []string{filepath.Join(scratch, "reproduction.json"), filepath.Join(scratch, "input.bin"), filepath.Join(scratch, "empty")}
				for i, path := range paths {
					if err := os.WriteFile(path, contents[i], 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "capture", Name: "swarm_publish", Arguments: tools.Result(map[string]any{"files": paths, "scope": "workspace", "text": "Restore as reproduction.json, input.bin and empty in your scratch. Requires project-check; reproduction command: project-check --input input.bin"})}}}
			})
			r = scratchRuntime(t, model, git)
			result, err := r.Agent(ctx, "", AgentRequest{Label: "File handoff", Task: "capture reproducer", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			admitted := tools.Result(admitParent(t, r))
			awaitReleased(t, r, result.Session)
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Fatalf("worker scratch survived release: %v", err)
			}
			if len(published.Artifacts) != len(contents) {
				t.Fatalf("published refs = %+v", published.Artifacts)
			}
			parent := llm.NewAgent(nil, r.config.Registry, llm.AgentConfig{ArtifactStore: r.config.Parent.ArtifactStore(), OpenArtifact: r.parent.OpenPublishedArtifact})
			defer parent.Close()
			restore, exists, allowed := parent.ToolRegistry().GetIfAllowed("restore_artifact")
			if !exists || !allowed {
				t.Fatal("parent cannot restore published files")
			}
			restored := t.TempDir()
			for i, ref := range published.Artifacts {
				if !strings.Contains(admitted, ref.ID) || !strings.Contains(admitted, filepath.Base(ref.Name)) {
					t.Fatalf("handoff omitted ID or filename: %s", admitted)
				}
				_, reader, err := r.parent.OpenPublishedArtifact(ctx, ref.ID)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				closeErr := reader.Close()
				if err != nil || closeErr != nil || !bytes.Equal(data, contents[i]) {
					t.Fatalf("published bytes = %q, %v, %v", data, err, closeErr)
				}
				path := filepath.Join(restored, filepath.Base(ref.Name))
				if _, err := restore.Execute(ctx, map[string]any{"id": ref.ID, "path": path}); err != nil {
					t.Fatal(err)
				}
				data, err = os.ReadFile(path)
				if err != nil || !bytes.Equal(data, contents[i]) {
					t.Fatalf("restored bytes = %q, %v", data, err)
				}
			}
			if !strings.Contains(admitted, "project-check --input input.bin") {
				t.Fatalf("handoff omitted the reproduction command: %s", admitted)
			}
		})
	}
}

type countingCaptureStore struct {
	artifacts.Store
	puts int
}

func (s *countingCaptureStore) Put(_ context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	s.puts++
	return artifacts.Ref{Bytes: int64(len(blob.Data))}, nil
}

func TestPublicationFileLimits(t *testing.T) {
	t.Parallel()
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	path := filepath.Join(t.TempDir(), "large")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(publicationFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store := &countingCaptureStore{}
	if _, err := capturePublicationFiles(context.Background(), registry, store, tools.Args{"files": []string{path}}); err == nil || store.puts != 0 {
		t.Fatalf("oversized file: puts=%d, %v", store.puts, err)
	}
	if err := os.Truncate(path, publicationFileBytes); err != nil {
		t.Fatal(err)
	}
	if refs, err := capturePublicationFiles(context.Background(), registry, store, tools.Args{"files": []string{path, path, path, path, path}}); err == nil || len(refs) != 0 || store.puts != 4 {
		t.Fatalf("aggregate bound: refs=%v, puts=%d, %v", refs, store.puts, err)
	}
}

func TestPublishFilesFailureDoesNotPublishPartialHandoff(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	ctx := context.Background()
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("swarm_publish")
	path := filepath.Join(t.TempDir(), "probe")
	data := []byte("private until the entire publication succeeds")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := artifacts.RefForBlob(artifacts.Blob{Data: data})
	for _, files := range []any{nil, "not an array", []any{1}, []string{""}, make([]string, publicationMaxFiles+1), []string{path, path + ".missing"}} {
		if _, err := tool.Execute(ctx, map[string]any{"files": files, "text": "not published"}); err == nil {
			t.Fatalf("accepted invalid capture: %#v", files)
		}
	}
	if _, err := tool.Execute(ctx, map[string]any{"files": []string{path}, "kind": "invalid", "text": "not published"}); err == nil {
		t.Fatal("accepted invalid publication")
	}
	unowned := artifacts.RefForBlob(artifacts.Blob{Data: []byte("not owned")})
	if out, err := tool.Execute(ctx, map[string]any{"files": []string{path}, "artifacts": []string{unowned.ID}, "text": "pinning must roll back"}); err == nil || out != "" {
		t.Fatalf("rolled-back publication returned receipt %q, %v", out, err)
	}
	s, err := r.State(ctx)
	if err != nil || len(s.Publications) != 0 {
		t.Fatalf("partial publications = %+v, %v", s.Publications, err)
	}
	if _, reader, err := r.parent.OpenPublishedArtifact(ctx, ref.ID); err == nil {
		reader.Close()
		t.Fatal("failed publication exposed a privately captured artifact")
	}
	existing, err := r.config.Parent.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("existing evidence")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(ctx, map[string]any{"files": []string{path}, "artifacts": []string{existing.ID}, "text": "complete"})
	if err != nil || !strings.Contains(out, ref.ID) || !strings.Contains(out, existing.ID) || !strings.Contains(out, "probe") {
		t.Fatalf("successful mixed capture = %s, %v", out, err)
	}
	for _, id := range []string{ref.ID, existing.ID} {
		_, reader, err := r.parent.OpenPublishedArtifact(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
