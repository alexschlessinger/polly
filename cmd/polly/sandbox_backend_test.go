package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/docker"
)

// fakeDaemon answers what the backend resolver asks a daemon: ping,
// version, info, and image inspection for the images it holds.
func fakeDaemon(t *testing.T, images map[string]string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("OK")) })
	mux.HandleFunc("GET /v1.41/version", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Version": "29.4.0", "ApiVersion": "1.54", "Os": "linux", "Arch": "arm64"})
	})
	mux.HandleFunc("GET /v1.41/info", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"Rootless":false}`)) })
	mux.HandleFunc("GET /v1.41/images/{ref}/json", func(w http.ResponseWriter, r *http.Request) {
		id, ok := images[r.PathValue("ref")]
		if !ok {
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Id": id})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return "tcp://" + strings.TrimPrefix(server.URL, "http://")
}

func isolateDocker(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("POLLYTOOL_SANDBOX_HELPER", "")
	t.Chdir(t.TempDir())
}

func TestResolveSandboxBackendSelection(t *testing.T) {
	isolateDocker(t)
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/polly-test/docker.sock")
	ctx := context.Background()
	resolve := func(config *Config) (*sandboxBackend, error) {
		return resolveSandboxBackend(ctx, config, nil, nil, nil)
	}

	// Nothing configured: native, silently.
	backend, err := resolve(&Config{SandboxPreset: "base"})
	if err != nil || backend.docker() || backend.fallback != "" || len(backend.hints) != 0 {
		t.Fatalf("nothing configured: %+v %v", backend, err)
	}
	// A repository image file is a hint, never a selection.
	if err := os.MkdirAll(".polly", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".polly", "image"), []byte("polly/go:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend, err = resolve(&Config{SandboxPreset: "base"})
	if err != nil || backend.docker() || len(backend.hints) != 1 || !strings.Contains(backend.hints[0], "polly/go:latest") {
		t.Fatalf("image file: %+v %v", backend, err)
	}
	// Explicit docker without an image fails closed.
	if _, err := resolve(&Config{SandboxPreset: "base", SandboxBackend: sandboxBackendDocker}); err == nil || !strings.Contains(err.Error(), "no image configured") {
		t.Fatalf("docker without image = %v", err)
	}
	// An image under auto with an unreachable daemon falls back with a notice.
	backend, err = resolve(&Config{SandboxPreset: "base", SandboxImage: "polly/base:latest"})
	if err != nil || backend.docker() || !strings.Contains(backend.fallback, "did not answer") || !strings.Contains(backend.fallback, "using the native sandbox") {
		t.Fatalf("auto fallback: %+v %v", backend, err)
	}
	// Explicit docker with an unreachable daemon fails closed.
	if _, err := resolve(&Config{SandboxPreset: "base", SandboxImage: "polly/base:latest", SandboxBackend: sandboxBackendDocker}); err == nil || !strings.Contains(err.Error(), "sandbox backend docker requested but unavailable") {
		t.Fatalf("docker unreachable = %v", err)
	}
	// Explicit native and --nosandbox ignore the image.
	for _, config := range []*Config{{SandboxPreset: "base", SandboxImage: "polly/base:latest", SandboxBackend: sandboxBackendNative}, {NoSandbox: true, SandboxImage: "polly/base:latest"}} {
		if backend, err := resolve(config); err != nil || backend.docker() || backend.fallback != "" {
			t.Fatalf("native selection %+v: %+v %v", config, backend, err)
		}
	}
	// An unsupported policy falls back under auto and fails explicit docker.
	backend, err = resolve(&Config{SandboxPreset: "base+ssh", SandboxImage: "polly/base:latest"})
	if err != nil || backend.docker() {
		t.Fatalf("ssh preset under auto: %+v %v", backend, err)
	}

	// A daemon that answers: a missing image fails closed even under auto; a
	// present one selects docker.
	t.Setenv("DOCKER_HOST", fakeDaemon(t, map[string]string{"polly/base:latest": "sha256:0123456789abcdef"}))
	if _, err := resolve(&Config{SandboxPreset: "base", SandboxImage: "polly/missing:latest"}); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("missing image = %v", err)
	}
	backend, err = resolve(&Config{SandboxPreset: "base", SandboxImage: "polly/base:latest", SandboxMode: "bind"})
	if err != nil || !backend.docker() || backend.imageID != "sha256:0123456789abcdef" || backend.mode != docker.ModeBind {
		t.Fatalf("docker selected: %+v %v", backend, err)
	}
	// Mode auto is copy for a tcp daemon.
	backend, err = resolve(&Config{SandboxPreset: "base", SandboxImage: "polly/base:latest"})
	if err != nil || !backend.docker() || backend.mode != docker.ModeCopy {
		t.Fatalf("auto mode for a remote daemon: %+v %v", backend, err)
	}
}

func TestNoSandboxRejectsExplicitDockerBackend(t *testing.T) {
	if err := runConfigValidationCommand("--nosandbox", "--sandbox-backend", "docker"); err == nil || !strings.Contains(err.Error(), "--sandbox-backend docker") {
		t.Fatalf("conflict = %v", err)
	}
	if err := runConfigValidationCommand("--nosandbox", "--sandbox-image", "polly/base:latest"); err != nil {
		t.Fatalf("an image under --nosandbox is ignored, not a conflict: %v", err)
	}
	if err := runConfigValidationCommand("--sandbox-backend", "container"); err == nil {
		t.Fatal("unknown backend accepted")
	}
	if err := runConfigValidationCommand("--sandbox-mode", "sync"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestDockerPostureAndNotices(t *testing.T) {
	isolateDocker(t)
	provider, err := docker.New(docker.Options{Image: "polly/base:latest", Host: "unix:///nonexistent/polly-test/docker.sock"})
	if err != nil {
		t.Fatal(err)
	}
	state := &conversationState{sandboxBackend: &sandboxBackend{name: sandboxBackendDocker, provider: provider, image: "polly/base:latest", imageID: "sha256:0123456789abcdef0000", mode: docker.ModeBind}}
	posture := currentSandboxPosture(&Config{SandboxPreset: "workspace+net+git"}, state)
	if !strings.Contains(posture.settingString(), "backend: docker; image: polly/base:latest@0123456789ab; mode: bind") {
		t.Fatalf("setting = %q", posture.settingString())
	}
	if line := posture.summaryLine(false); line != "Sandbox docker · polly/base:latest@0123456789ab · bind" {
		t.Fatalf("summary = %q", line)
	}
	if notice := posture.noticeString(); !strings.Contains(notice, "Sandbox docker") {
		t.Fatalf("docker start notice = %q", notice)
	}

	// A native run with a fallback and a hint reports both; quiet silences.
	native := &conversationState{sandboxBackend: &sandboxBackend{name: sandboxBackendNative, fallback: "docker image polly/base:latest is configured but the daemon did not answer; using the native sandbox", hints: []string{".polly/image names polly/go:latest"}}}
	posture = currentSandboxPosture(&Config{SandboxPreset: "base"}, native)
	notice := posture.noticeString()
	for _, want := range []string{"Sandbox unavailable", "did not answer", ".polly/image"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("native notice %q lacks %q", notice, want)
		}
	}
	var out bytes.Buffer
	writeFallbackSandboxNotice(&out, &Config{SandboxPreset: "base", Quiet: true}, native)
	if out.Len() != 0 {
		t.Fatalf("--quiet printed %q", out.String())
	}
	writeFallbackSandboxNotice(&out, &Config{SandboxPreset: "base"}, native)
	if !strings.Contains(out.String(), "did not answer") {
		t.Fatalf("notice not printed: %q", out.String())
	}
}

func TestSandboxBuildPrintWritesDockerfilesWithoutRunningDocker(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	cmd := getCommand()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"polly", "sandbox", "build", "--print", "--dir", dir, "--ref", "abc123", "go"}); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"base", "go"} {
		content, err := os.ReadFile(filepath.Join(dir, variant, "Dockerfile"))
		if err != nil || !bytes.Contains(content, []byte("FROM ")) {
			t.Fatalf("%s Dockerfile: %q %v", variant, content, err)
		}
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "--tag polly/base:latest") || !strings.Contains(lines[1], "--tag polly/go:latest") || !strings.Contains(lines[0], "POLLY_REF=abc123") {
		t.Fatalf("printed commands %q", out.String())
	}
	if err := getCommand().Run(context.Background(), []string{"polly", "sandbox", "build", "--print", "--dir", dir, "rust"}); err == nil {
		t.Fatal("unknown variant accepted")
	}
}
