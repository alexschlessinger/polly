package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestAgentSandboxDoesNotMaterializeGitGuards(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	for _, nested := range []bool{false, true} {
		for _, tc := range []struct {
			name                                   string
			specs                                  []string
			readOnly, inherited, metadata, invalid bool
			rootWrite, scratchWrite                bool
		}{
			{name: "readonly first", specs: []string{"readonly", "workspace+git"}},
			{name: "readonly last", specs: []string{"workspace+git", "readonly"}},
			{name: "base first", specs: []string{"base", "workspace+git"}, scratchWrite: true},
			{name: "base last", specs: []string{"workspace+git", "base"}, scratchWrite: true},
			{name: "workspace first", specs: []string{"workspace", "workspace+git"}, rootWrite: true, scratchWrite: true},
			{name: "workspace last", specs: []string{"workspace+git", "workspace"}, rootWrite: true, scratchWrite: true},
			{name: "combined", specs: []string{"readonly+workspace+git"}},
			{name: "readonly grant", specs: []string{"workspace+git"}, readOnly: true, scratchWrite: true},
			{name: "metadata grant", specs: []string{"workspace+git"}, metadata: true, rootWrite: true, scratchWrite: true},
			{name: "inherited readonly", specs: []string{"workspace+git"}, inherited: true},
			{name: "inherited metadata", specs: []string{"workspace+git"}, inherited: true, metadata: true, rootWrite: true, scratchWrite: true},
			{name: "invalid later spec", specs: []string{"workspace+git", "invalid"}, invalid: true},
		} {
			name := tc.name
			if nested {
				name += " nested"
			}
			t.Run(name, func(t *testing.T) {
				root, scratch := realTempDir(t), realTempDir(t)
				repo := root
				if nested {
					repo = filepath.Join(root, "nested")
				}
				gitDir := makeAgentSandboxGitRepo(t, repo)
				if err := os.RemoveAll(filepath.Join(gitDir, "hooks")); err != nil {
					t.Fatal(err)
				}
				opts := []RegistryOption{WithNativeTools(), WithUnsafeNoSandbox()}
				grant := ExecutionGrant{SourceRoot: root, Scratch: scratch, ReadOnly: tc.readOnly, SandboxPresets: tc.specs}
				if tc.inherited {
					cfg := sandbox.Config{WritablePaths: []string{root}}
					if tc.metadata {
						cfg.DenyWritePaths = []string{gitDir}
					} else {
						cfg.DenyWrite = true
					}
					opts = append(opts, WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), cfg))
				} else if tc.metadata {
					grant.DeniedWrites = []string{gitDir}
				}
				parent := NewToolRegistry(nil, opts...)
				defer parent.Close()
				ec, err := parent.ExecutionPolicy(root, grant)
				if (err != nil) != tc.invalid {
					t.Fatalf("policy error = %v, want failure %v", err, tc.invalid)
				}
				if _, err := os.Lstat(filepath.Join(gitDir, "hooks")); !os.IsNotExist(err) {
					t.Fatalf("policy resolution created hooks: %v", err)
				}
				if tc.invalid {
					return
				}
				if got := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(root, "file")) == nil; got != tc.rootWrite {
					t.Fatalf("root writable = %v, want %v", got, tc.rootWrite)
				}
				if got := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(scratch, "file")) == nil; got != tc.scratchWrite {
					t.Fatalf("scratch writable = %v, want %v", got, tc.scratchWrite)
				}
				if sandbox.WriteAllowed(ec.Sandbox, filepath.Join(gitDir, "index")) == nil {
					t.Fatal("fallback did not protect the whole Git metadata tree")
				}
			})
		}
	}
}
