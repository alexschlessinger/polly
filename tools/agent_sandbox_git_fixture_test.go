package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func makeAgentSandboxGitRepo(t *testing.T, root string) string {
	t.Helper()
	gitDir := filepath.Join(root, ".git")
	for _, dir := range []string{"hooks", "objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(gitDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"config": "[core]\n", "HEAD": "ref: refs/heads/main\n"} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return gitDir
}

func TestAgentSandboxExistingGitGuardsRetainMetadataWrites(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	root := realTempDir(t)
	gitDirs := []string{makeAgentSandboxGitRepo(t, root), makeAgentSandboxGitRepo(t, filepath.Join(root, "nested"))}
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: []string{"workspace+git"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, gitDir := range gitDirs {
		if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(gitDir, "index")); err != nil {
			t.Fatalf("workspace+git lost metadata writes: %v", err)
		}
		for _, leaf := range []string{"config", "hooks"} {
			if sandbox.WriteAllowed(ec.Sandbox, filepath.Join(gitDir, leaf)) == nil {
				t.Fatalf("unprotected Git guard %s", leaf)
			}
		}
	}
}
