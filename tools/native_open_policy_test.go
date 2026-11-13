package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// A binding prepares its policy once, with the scope's read paths in it. The
// result is the policy the exported steps arrive at, which prepare a
// read-only root's policy before the read paths are added and again after.
func TestNativeOpenToolsBindsThePolicyOfTheExportedSteps(t *testing.T) {
	canonical := func(path string) string {
		t.Helper()
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return real
	}
	for _, tc := range []struct {
		name    string
		scratch bool
		// masked puts the root inside a directory the source denies, as a
		// member's checkout is inside the runtime directory.
		masked bool
		grant  ExecutionGrant
	}{
		{name: "writable"},
		{name: "writable_with_scratch", scratch: true},
		{name: "read_only", grant: ExecutionGrant{ReadOnly: true}},
		{name: "read_only_with_scratch", scratch: true, grant: ExecutionGrant{ReadOnly: true}},
		{name: "read_only_inside_a_masked_directory", masked: true, grant: ExecutionGrant{ReadOnly: true}},
		{name: "read_only_with_scratch_inside_a_masked_directory", scratch: true, masked: true, grant: ExecutionGrant{ReadOnly: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, shared := canonical(t.TempDir()), canonical(t.TempDir())
			base := sandbox.DefaultConfig()
			if tc.masked {
				base.DenyPaths = []string{root}
				root = filepath.Join(root, "checkout")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			nested := filepath.Join(shared, "nested")
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			linked := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(nested, linked); err != nil {
				t.Fatal(err)
			}
			grant := tc.grant
			if tc.scratch {
				grant.Scratch = canonical(t.TempDir())
			}
			// A grant, one inside it, one reached through a symlink, and one
			// that does not exist.
			readPaths := []string{shared, nested, linked, filepath.Join(shared, "missing")}
			source := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), base))
			defer source.Close()

			binding, err := NativeOpenTools(source)(context.Background(), ToolScope{Root: root, Grant: grant, ReadPaths: readPaths})
			if err != nil {
				t.Fatal(err)
			}
			defer binding.Close()

			ec, err := source.ExecutionPolicy(root, grant)
			if err != nil {
				t.Fatal(err)
			}
			ec.Sandbox.ReadPaths = append(ec.Sandbox.ReadPaths, readPaths...)
			stepped, _, err := source.BindExecutionContext(ec, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer stepped.Close()

			got, _, err := binding.Registry.SandboxReadPolicy()
			if err != nil {
				t.Fatal(err)
			}
			want, _, err := stepped.SandboxReadPolicy()
			if err != nil {
				t.Fatal(err)
			}
			for _, lists := range []struct {
				field     string
				got, want []string
			}{
				{"WritablePaths", got.WritablePaths, want.WritablePaths},
				{"ReadPaths", got.ReadPaths, want.ReadPaths},
				{"DenyPaths", got.DenyPaths, want.DenyPaths},
				{"DenyWritePaths", got.DenyWritePaths, want.DenyWritePaths},
				{"AllowUnixSockets", got.AllowUnixSockets, want.AllowUnixSockets},
			} {
				if !slices.Equal(lists.got, lists.want) {
					t.Fatalf("%s = %q, the exported steps give %q", lists.field, lists.got, lists.want)
				}
			}
			if got.DenyWrite != want.DenyWrite || got.PrivateHome != want.PrivateHome || got.DenyHostTemp != want.DenyHostTemp {
				t.Fatalf("restrictions = %v %v %v, the exported steps give %v %v %v",
					got.DenyWrite, got.PrivateHome, got.DenyHostTemp, want.DenyWrite, want.PrivateHome, want.DenyHostTemp)
			}
			gotText, err := binding.Registry.SandboxContext()
			if err != nil {
				t.Fatal(err)
			}
			wantText, err := stepped.SandboxContext()
			if err != nil {
				t.Fatal(err)
			}
			if gotText != wantText {
				t.Fatalf("sandbox context differs from the exported steps:\n%s\n---\n%s", gotText, wantText)
			}

			gotReads, err := sandbox.CompileReadPolicy(got)
			if err != nil {
				t.Fatal(err)
			}
			wantReads, err := sandbox.CompileReadPolicy(want)
			if err != nil {
				t.Fatal(err)
			}
			probes := []string{
				root, filepath.Join(root, "file"), shared, filepath.Join(nested, "file"), linked,
				filepath.Join(shared, "missing", "file"), filepath.Join(filepath.Dir(root), "elsewhere"),
			}
			if grant.Scratch != "" {
				probes = append(probes, filepath.Join(grant.Scratch, "file"))
			}
			for _, path := range probes {
				if g, w := gotReads.Allowed(path) == nil, wantReads.Allowed(path) == nil; g != w {
					t.Fatalf("read of %s allowed = %v, the exported steps give %v", path, g, w)
				}
				if g, w := sandbox.WriteAllowed(got, path) == nil, sandbox.WriteAllowed(want, path) == nil; g != w {
					t.Fatalf("write of %s allowed = %v, the exported steps give %v", path, g, w)
				}
			}
			// The root stays readable, and writable only as the grant says.
			if err := gotReads.Allowed(filepath.Join(root, "file")); err != nil {
				t.Fatalf("the binding cannot read its root: %v", err)
			}
			if wrote := sandbox.WriteAllowed(got, filepath.Join(root, "file")) == nil; wrote == grant.ReadOnly {
				t.Fatalf("write to the root allowed = %v with ReadOnly = %v", wrote, grant.ReadOnly)
			}
		})
	}
}
