package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools/docker"
)

// The CLI assembly against a real daemon: developer-run with
// POLLYTOOL_REQUIRE_DOCKER_TESTS=1, like tools/docker's integration tests.
func TestDockerBackendOpensAConversationInAContainer(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_DOCKER_TESTS") != "1" {
		t.Skip("set POLLYTOOL_REQUIRE_DOCKER_TESTS=1 to run docker integration tests")
	}
	image := os.Getenv("POLLYTOOL_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "debian:bookworm-slim"
	}
	probe, err := docker.New(docker.Options{Image: image})
	if err != nil {
		t.Fatal(err)
	}
	info, err := probe.Ping(context.Background())
	if err != nil {
		t.Fatalf("daemon: %v", err)
	}
	helper := filepath.Join(t.TempDir(), "polly")
	build := exec.Command("go", "build", "-o", helper, ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+info.Arch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %s %v", out, err)
	}
	t.Setenv("POLLYTOOL_SANDBOX_HELPER", helper)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("host notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	store := testOpenMemoryStore(t, nil)
	config := &Config{NoSkills: true, SandboxPreset: "workspace", SandboxImage: image, SandboxMode: "bind", Tools: []string{"bash", "read_file"}}
	state, err := (&conversationOpener{config: config, sessionStore: store, cmd: getCommand()}).openNew(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if !state.sandboxBackend.docker() {
		t.Fatalf("backend = %+v", state.sandboxBackend)
	}
	posture := currentSandboxPosture(config, state)
	if !posture.docker || !strings.Contains(posture.summaryLine(false), "Sandbox docker") {
		t.Fatalf("posture %+v", posture)
	}
	bash, ok := state.toolRegistry.Get("bash")
	if !ok {
		t.Fatal("bash is not served through the container")
	}
	execution, err := state.toolRegistry.ExecuteTool(context.Background(), bash, map[string]any{"command": "uname -s; cat notes.txt; echo HOME=$HOME"}, time.Minute)
	if err != nil || !strings.Contains(execution.Output.Text, "Linux") || !strings.Contains(execution.Output.Text, "host notes") || !strings.Contains(execution.Output.Text, "HOME=/run/polly/home") {
		t.Fatalf("bash in the container = %+v, %v", execution.Output, err)
	}
	loaders := state.toolRegistry.GetActiveToolLoaders()
	names := make([]string, 0, len(loaders))
	for _, info := range loaders {
		names = append(names, info.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "bash") || !strings.Contains(strings.Join(names, ","), "read_file") {
		t.Fatalf("persisted loaders %v", names)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	if err := probe.Destroy(context.Background(), canonicalRoot); err != nil {
		t.Fatal(err)
	}
	// A missing image fails closed at open rather than starting natively.
	config.SandboxImage = "polly/does-not-exist:never"
	if _, err := (&conversationOpener{config: config, sessionStore: store, cmd: getCommand()}).openNew(context.Background(), "", false); err == nil || !errors.Is(err, docker.ErrImageMissing) && !strings.Contains(err.Error(), "not present") {
		t.Fatalf("missing image = %v", err)
	}
}
