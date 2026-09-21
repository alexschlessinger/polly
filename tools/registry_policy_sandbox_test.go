package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// Policy changes reach the bash that loaded before them under the real
// sandbox: an added directory inside the private home becomes readable, and a
// layer taken back hides its grant again, from bash and from read_file alike,
// including when a derived registry loaded its own bash.
func TestPolicyChangesReachLoadedBashSandbox(t *testing.T) {
	skipUnlessSandboxTests(t)
	home := realTempDir(t)
	t.Setenv("HOME", home)
	added := filepath.Join(home, "src", "shared")
	layered := filepath.Join(home, "src", "cache")
	for dir, value := range map[string]string{added: "added-value", layered: "layer-value"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fixture.txt"), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for depth, name := range []string{"root", "child", "nested"} {
		t.Run(name, func(t *testing.T) {
			root := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, sandbox.DefaultConfig().Merge(sandbox.Config{PrivateHome: true})))
			t.Cleanup(func() { _ = root.Close() })
			registry := root
			for range depth {
				child := registry.Derive()
				t.Cleanup(func() { _ = child.Close() })
				registry = child
			}
			if _, err := registry.LoadToolAuto("bash"); err != nil {
				t.Fatal(err)
			}
			read := func(dir string) (string, error) {
				bash, ok := registry.Get("bash")
				if !ok {
					t.Fatal("bash is not loaded")
				}
				return bash.Execute(context.Background(), map[string]any{"command": "cat '" + filepath.Join(dir, "fixture.txt") + "'"})
			}

			if out, err := read(added); err == nil || strings.Contains(out, "added-value") {
				t.Fatalf("bash read the private home before any grant: %q %v", out, err)
			}
			if _, err := root.AppendBaseReadPaths(added); err != nil {
				t.Fatal(err)
			}
			if out, err := read(added); err != nil || !strings.Contains(out, "added-value") {
				t.Fatalf("bash after the added directory: %q %v", out, err)
			}

			readFile := func(dir string) (string, error) {
				return NewReadFileTool(registry).Execute(context.Background(), map[string]any{"path": filepath.Join(dir, "fixture.txt")})
			}
			if _, err := root.SetSandboxLayer("profile", &SandboxLayer{Config: sandbox.Config{ReadPaths: []string{layered}}}); err != nil {
				t.Fatal(err)
			}
			if out, err := read(layered); err != nil || !strings.Contains(out, "layer-value") {
				t.Fatalf("bash under the layer: %q %v", out, err)
			}
			if out, err := readFile(layered); err != nil || !strings.Contains(out, "layer-value") {
				t.Fatalf("read_file under the layer: %q %v", out, err)
			}
			if _, err := root.SetSandboxLayer("profile", nil); err != nil {
				t.Fatal(err)
			}
			if out, err := read(layered); err == nil || strings.Contains(out, "layer-value") {
				t.Fatalf("bash still read the layer's grant after it was removed: %q %v", out, err)
			}
			if out, err := readFile(layered); err == nil || strings.Contains(out, "layer-value") {
				t.Fatalf("read_file still read the layer's grant after it was removed: %q %v", out, err)
			}
			if out, err := read(added); err != nil || !strings.Contains(out, "added-value") {
				t.Fatalf("removing the layer lost the added directory: %q %v", out, err)
			}
		})
	}
}
