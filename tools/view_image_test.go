package tools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func writeTestPNG(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, testPNGBytes(t), 0o600); err != nil {
		t.Fatalf("write png: %v", err)
	}
	return path
}

type stubSandbox struct{}

func (stubSandbox) Wrap(cmd *exec.Cmd) error { return nil }

func stubSandboxRegistry(t *testing.T, cfg sandbox.Config) *ToolRegistry {
	t.Helper()
	factory := func(sandbox.Config) (sandbox.Sandbox, error) { return stubSandbox{}, nil }
	return NewToolRegistry(nil, WithSandboxFactory(factory, cfg))
}

func TestViewImageReadsLocalFile(t *testing.T) {
	path := writeTestPNG(t, t.TempDir(), "sample.png")
	tool := NewViewImageTool(NewToolRegistry(nil))
	output, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": path})
	if err != nil {
		t.Fatalf("ExecuteOutput: %v", err)
	}
	if len(output.Media) != 1 {
		t.Fatalf("expected 1 media item, got %d", len(output.Media))
	}
	media := output.Media[0]
	if media.MIMEType != "image/png" || media.Name != "sample.png" || len(media.Data) == 0 {
		t.Fatalf("unexpected media: %+v", media)
	}
	if !strings.Contains(output.Text, "sample.png") || !strings.Contains(output.Text, "3x2") {
		t.Fatalf("unexpected text: %q", output.Text)
	}
}

func TestViewImageRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("plain text"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	tool := NewViewImageTool(NewToolRegistry(nil))
	if _, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": path}); err == nil {
		t.Fatal("expected error for non-image file")
	}
}

func TestViewImageRequiresSource(t *testing.T) {
	tool := NewViewImageTool(NewToolRegistry(nil))
	if _, err := tool.ExecuteOutput(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestViewImageHonorsSandboxDenyPaths(t *testing.T) {
	denied := t.TempDir()
	path := writeTestPNG(t, denied, "secret.png")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	tool := NewViewImageTool(registry)
	_, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": path})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected sandbox denial, got %v", err)
	}
}

func TestViewImageSandboxReadPathExemption(t *testing.T) {
	denied := t.TempDir()
	allowed := filepath.Join(denied, "shared")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := writeTestPNG(t, allowed, "ok.png")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}, ReadPaths: []string{allowed}})
	tool := NewViewImageTool(registry)
	if _, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": path}); err != nil {
		t.Fatalf("expected read-path exemption to allow read, got %v", err)
	}
}

func TestViewImageSandboxDeniesSymlinkIntoDeniedPath(t *testing.T) {
	denied := t.TempDir()
	target := writeTestPNG(t, denied, "secret.png")
	link := filepath.Join(t.TempDir(), "harmless.png")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	tool := NewViewImageTool(registry)
	_, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": link})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected symlink into denied path to be blocked, got %v", err)
	}
}

func TestViewImageAllowsUnsandboxedDeniedPath(t *testing.T) {
	// Without a sandbox factory the registry applies no read policy, matching
	// bash running unsandboxed.
	denied := t.TempDir()
	path := writeTestPNG(t, denied, "open.png")
	registry := NewToolRegistry(nil)
	tool := NewViewImageTool(registry)
	if _, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": path}); err != nil {
		t.Fatalf("expected unsandboxed read to succeed, got %v", err)
	}
}

func TestViewImageFetchesURL(t *testing.T) {
	data := testPNGBytes(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	tool := NewViewImageTool(NewToolRegistry(nil))
	output, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": server.URL + "/pic.png"})
	if err != nil {
		t.Fatalf("ExecuteOutput: %v", err)
	}
	if len(output.Media) != 1 || output.Media[0].Name != "pic.png" || output.Media[0].MIMEType != "image/png" {
		t.Fatalf("unexpected media: %+v", output.Media)
	}
}

func TestViewImageSandboxDeniesNetwork(t *testing.T) {
	registry := stubSandboxRegistry(t, sandbox.Config{})
	tool := NewViewImageTool(registry)
	_, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": "http://127.0.0.1:1/pic.png"})
	if err == nil || !strings.Contains(err.Error(), "denies network") {
		t.Fatalf("expected network denial, got %v", err)
	}
}

func TestViewImageSandboxAllowsNetworkWhenGranted(t *testing.T) {
	data := testPNGBytes(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data)
	}))
	defer server.Close()

	registry := stubSandboxRegistry(t, sandbox.Config{AllowNetwork: true})
	tool := NewViewImageTool(registry)
	if _, err := tool.ExecuteOutput(context.Background(), map[string]any{"source": server.URL + "/pic"}); err != nil {
		t.Fatalf("expected fetch to succeed with AllowNetwork, got %v", err)
	}
}
