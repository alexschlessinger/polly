package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestAgentSandboxRejectsReplacedInheritedAuthority(t *testing.T) {
	skipIfWindows(t)
	for _, boundSource := range []bool{false, true} {
		for _, replacement := range []string{"directory", "symlink"} {
			name := "sandboxed/" + replacement
			if boundSource {
				name = "native-bound/" + replacement
			}
			t.Run(name, func(t *testing.T) {
				home, root := realTempDir(t), realTempDir(t)
				t.Setenv("HOME", home)
				granted := filepath.Join(home, "granted")
				hidden := filepath.Join(home, "hidden")
				for _, path := range []string{granted, hidden} {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				cfg := sandbox.Config{WritablePaths: []string{root}, ReadPaths: []string{granted}, PrivateHome: true, DenyHostTemp: true}
				var source *ToolRegistry
				if boundSource {
					parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
					defer parent.Close()
					ec, err := parent.ExecutionPolicy(root, ExecutionGrant{})
					if err != nil {
						t.Fatal(err)
					}
					ec.Sandbox = cfg
					source, _, err = parent.BindExecutionContext(ec, nil)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					source = NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), cfg))
				}
				defer source.Close()
				frozen, active, err := source.SandboxReadPolicy()
				if err != nil || !active {
					t.Fatalf("freeze parent policy: active=%v err=%v", active, err)
				}
				if sandbox.ReadAllowed(frozen, granted) != nil || sandbox.ReadAllowed(frozen, hidden) == nil {
					t.Fatal("parent fixture lacks a file scope boundary")
				}
				if err := os.Rename(granted, granted+"-original"); err != nil {
					t.Fatal(err)
				}
				if replacement == "symlink" {
					err = os.Symlink(hidden, granted)
				} else {
					err = os.Mkdir(granted, 0o700)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sandbox.CompileReadPolicy(frozen); err == nil {
					t.Fatal("parent accepted a replaced frozen grant")
				}
				for _, registry := range []*ToolRegistry{source, source.Derive()} {
					if registry != source {
						defer registry.Close()
					}
					_, err := registry.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, SandboxPresets: []string{"workspace+net"}})
					if err == nil || !strings.Contains(err.Error(), "frozen sandbox authority") {
						t.Fatalf("child recaptured replaced inherited authority: %v", err)
					}
				}
			})
		}
	}
}
