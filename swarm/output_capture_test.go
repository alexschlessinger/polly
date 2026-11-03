package swarm

import (
	"bytes"
	"context"
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

// Files a member leaves in its output directory reach the parent as artifacts
// of the completed task, byte for byte, named by their relative paths, and
// outlive the workspace they were written in.
func TestOutputFilesCapturedAtCompletion(t *testing.T) {
	t.Parallel()
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "checkout"}[git], func(t *testing.T) {
			ctx := context.Background()
			var r *Runtime
			var scratch string
			contents := map[string][]byte{"reproduction.json": []byte("{\r\n  \"command\": \"project-check --input input.bin\"\r\n}"), "fixtures/input.bin": {0, 255, 128, '\r', '\n', 0}, "empty": {}}
			model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				s, err := r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				scratch = onlyContext(t, s).Scratch
				for name, data := range contents {
					path := filepath.Join(tools.OutputDir(scratch), filepath.FromSlash(name))
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return answer("Outputs hold reproduction.json and fixtures/input.bin; command: project-check --input input.bin")
			})
			r = scratchRuntime(t, model, git)
			suspendAutoRelease(t, r)
			result, err := r.Agent(ctx, "", AgentRequest{Label: "File handoff", Task: "capture reproducer", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Outputs) != len(contents) {
				t.Fatalf("result outputs = %+v", result.Outputs)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			task := s.Tasks[result.Task]
			if task == nil || len(task.Outputs) != len(contents) {
				t.Fatalf("task outputs = %+v", task)
			}
			if entries, err := os.ReadDir(tools.OutputDir(scratch)); err != nil || len(entries) != 0 {
				t.Fatalf("output directory after capture = %v, %v", entries, err)
			}
			admitted := tools.Result(admitParent(t, r))
			parent := llm.NewAgent(nil, r.config.Registry, llm.AgentConfig{ArtifactStore: r.config.Parent.ArtifactStore(), OpenArtifact: r.parent.OpenPublishedArtifact})
			defer parent.Close()
			restore, exists, allowed := parent.ToolRegistry().GetIfAllowed("restore_artifact")
			if !exists || !allowed {
				t.Fatal("parent cannot restore captured outputs")
			}
			restored := t.TempDir()
			seen := map[string]bool{}
			for _, ref := range result.Outputs {
				want, ok := contents[ref.Name]
				if !ok || seen[ref.Name] || ref.Bytes != int64(len(want)) {
					t.Fatalf("unexpected output %+v", ref)
				}
				seen[ref.Name] = true
				if !strings.Contains(admitted, ref.ID) || !strings.Contains(admitted, ref.Name) {
					t.Fatalf("completion notice omitted %s: %s", ref.Name, admitted)
				}
				_, reader, err := r.parent.OpenPublishedArtifact(ctx, ref.ID)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				closeErr := reader.Close()
				if err != nil || closeErr != nil || !bytes.Equal(data, want) {
					t.Fatalf("captured bytes of %s = %q, %v, %v", ref.Name, data, err, closeErr)
				}
				path := filepath.Join(restored, filepath.Base(ref.Name))
				if _, err := restore.Execute(ctx, map[string]any{"id": ref.ID, "path": path}); err != nil {
					t.Fatal(err)
				}
				if data, err = os.ReadFile(path); err != nil || !bytes.Equal(data, want) {
					t.Fatalf("restored bytes of %s = %q, %v", ref.Name, data, err)
				}
			}
			if err := r.Cleanup(ctx, result.Context); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Fatalf("worker scratch survived release: %v", err)
			}
			for _, ref := range result.Outputs {
				_, reader, err := r.parent.OpenPublishedArtifact(ctx, ref.ID)
				if err != nil {
					t.Fatalf("output %s lost with the workspace: %v", ref.Name, err)
				}
				reader.Close()
			}
		})
	}
}

// A capture that cannot be completed exactly fails the completion with the
// reason instead of handing the parent a partial set.
func TestOutputCaptureFailureBlocksCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var r *Runtime
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		s, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		dir := tools.OutputDir(onlyContext(t, s).Scratch)
		if err := os.WriteFile(filepath.Join(dir, "real"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
			t.Skip(err)
		}
		return answer("done")
	})
	r = scratchRuntime(t, model, false)
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Bad handoff", Task: "leave a link", ReadOnly: true})
	s, stateErr := r.State(ctx)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	task := s.Tasks[result.Task]
	if err == nil && (task == nil || task.Status != "blocked") {
		t.Fatalf("link in outputs completed: err=%v task=%+v", err, task)
	}
	if task != nil && !strings.Contains(task.Feedback, "not a regular file") {
		t.Fatalf("task feedback = %q", task.Feedback)
	}
	if len(result.Outputs) != 0 {
		t.Fatalf("partial outputs = %+v", result.Outputs)
	}
}

func TestOutputCaptureLimits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	store := &countingCaptureStore{}
	if refs, err := captureOutputs(ctx, registry, store, ""); refs != nil || err != nil {
		t.Fatalf("no scratch: %v, %v", refs, err)
	}
	scratch := t.TempDir()
	if refs, err := captureOutputs(ctx, registry, store, scratch); refs != nil || err != nil {
		t.Fatalf("missing output directory: %v, %v", refs, err)
	}
	dir := tools.OutputDir(scratch)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if refs, err := captureOutputs(ctx, registry, store, scratch); refs != nil || err != nil {
		t.Fatalf("empty output directory: %v, %v", refs, err)
	}
	for i := range publicationMaxFiles + 1 {
		if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))), []byte{byte(i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := captureOutputs(ctx, registry, store, scratch); err == nil || store.puts != 0 || !strings.Contains(err.Error(), "exceed the limit") {
		t.Fatalf("too many files: puts=%d, %v", store.puts, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, publicationFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := captureOutputs(ctx, registry, store, scratch); err == nil || store.puts != 0 {
		t.Fatalf("oversized file: puts=%d, %v", store.puts, err)
	}
	if err := os.Truncate(large, 3); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	refs, err := captureOutputs(ctx, registry, store, scratch)
	if err != nil || len(refs) != 2 || store.puts != 2 || refs[0].Bytes+refs[1].Bytes != 4 {
		t.Fatalf("capture = %+v, puts=%d, %v", refs, store.puts, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("capture removed originals before finalization: %v, %v", entries, err)
	}
	if err := clearOutputs(scratch); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("output directory after cleanup = %v, %v", entries, err)
	}
}

// The counting store keeps no bytes, so names are checked where the store
// records the blob it was given.
func TestOutputNamesAreRelativeSlashPaths(t *testing.T) {
	t.Parallel()
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	scratch := t.TempDir()
	dir := tools.OutputDir(scratch)
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "c.bin"), []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &namingStore{}
	refs, err := captureOutputs(context.Background(), registry, store, scratch)
	if err != nil || len(refs) != 1 || store.names[0] != "a/b/c.bin" {
		t.Fatalf("refs=%+v names=%v err=%v", refs, store.names, err)
	}
}

type namingStore struct {
	artifacts.Store
	names []string
}

func (s *namingStore) Put(_ context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	s.names = append(s.names, blob.Name)
	return artifacts.RefForBlob(blob), nil
}

// A worker that fails for good still hands its diagnostics to the parent: the
// outputs reach the blocked task and the failure notice.
func TestOutputFilesCapturedOnTerminalFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var r *Runtime
	data := []byte("trace of the failure")
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		s, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tools.OutputDir(onlyContext(t, s).Scratch), "trace.log"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		message := answer("")
		message.Reasoning = "nothing more to say"
		return message
	})
	r = scratchRuntime(t, model, false)
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Failing agent", Task: "diagnose", ReadOnly: true})
	if err == nil {
		t.Fatal("blank final answers completed")
	}
	s, stateErr := r.State(ctx)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	task := s.Tasks[result.Task]
	if task == nil || task.Status != "blocked" || len(task.Outputs) != 1 || task.Outputs[0].Name != "trace.log" || len(result.Outputs) != 1 {
		t.Fatalf("task=%+v result=%+v", task, result)
	}
	if entries, err := os.ReadDir(tools.OutputDir(onlyContext(t, s).Scratch)); err != nil || len(entries) != 0 {
		t.Fatalf("terminal failure did not clear captured outputs: %v, %v", entries, err)
	}
	admitted := tools.Result(admitParent(t, r))
	if !strings.Contains(admitted, "failed task") || !strings.Contains(admitted, task.Outputs[0].ID) {
		t.Fatalf("failure notice omitted the outputs: %s", admitted)
	}
	_, reader, err := r.parent.OpenPublishedArtifact(ctx, task.Outputs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err != nil || closeErr != nil || !bytes.Equal(got, data) {
		t.Fatalf("captured diagnostics = %q, %v, %v", got, err, closeErr)
	}
}

// A file that changes under the read is not captured, and the directory is
// left as it was so the worker can finish writing and complete again.
func TestOutputCaptureRejectsChangingFile(t *testing.T) {
	t.Parallel()
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	scratch := t.TempDir()
	dir := tools.OutputDir(scratch)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "growing.log")
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &countingCaptureStore{}
	capture := outputCapture{read: func(ctx context.Context, registry *tools.ToolRegistry, path string, limit int64) (artifacts.Blob, error) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(" and more")); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return tools.ReadArtifactFile(ctx, registry, path, limit)
	}}
	if _, err := capture.run(context.Background(), registry, store, scratch); err == nil || store.puts != 0 || !strings.Contains(err.Error(), "changed while it was being captured") {
		t.Fatalf("changing file: puts=%d, %v", store.puts, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "partial and more" {
		t.Fatalf("output directory was not left alone: %q, %v", data, err)
	}
	if refs, err := captureOutputs(context.Background(), registry, store, scratch); err != nil || len(refs) != 1 || store.puts != 1 {
		t.Fatalf("settled file: %+v, puts=%d, %v", refs, store.puts, err)
	}
}
