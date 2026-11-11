package tools

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestValidateAgentSandboxPreset(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	// Even workspace+git validation must not discover or materialize .git.
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"base", "readonly", "default", "workspace+net+git", "private-home+ssh", "workspace+sshkeys", " git + workspace "} {
		if err := ValidateAgentSandboxPreset(spec); err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
	}
	for _, spec := range []string{"", " ", "git", "net+git", "workspace+", "workspace++net", "unknown", "false"} {
		if err := ValidateAgentSandboxPreset(spec); err == nil {
			t.Fatalf("accepted %q", spec)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, ".git"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("validator mutated Git metadata: %v %v", entries, err)
	}
}

func TestAgentSandboxPresetPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		specs                                 []string
		rootWrite, scratchWrite, net, private bool
	}{
		{name: "workspace", specs: []string{"workspace"}, rootWrite: true, scratchWrite: true},
		{name: "network opt in", specs: []string{"workspace+net"}, rootWrite: true, scratchWrite: true, net: true},
		{name: "base", specs: []string{"base"}, scratchWrite: true},
		{name: "readonly", specs: []string{"readonly"}},
		{name: "private home", specs: []string{"workspace+private-home"}, rootWrite: true, scratchWrite: true, private: true},
		{name: "floor denies network", specs: []string{"workspace", "workspace+net"}, rootWrite: true, scratchWrite: true},
		{name: "floor denies root", specs: []string{"base+net", "workspace+net"}, scratchWrite: true, net: true},
		{name: "floor denies every write", specs: []string{"readonly+net", "workspace"}},
		{name: "request narrows floor", specs: []string{"workspace+net", "base"}, scratchWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, scratch := realTempDir(t), realTempDir(t)
			parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
			defer parent.Close()
			ec, err := parent.ExecutionPolicy(root, ExecutionGrant{Scratch: scratch, SandboxPresets: tc.specs})
			if err != nil {
				t.Fatal(err)
			}
			if got := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(root, "file")) == nil; got != tc.rootWrite {
				t.Fatalf("root write = %v, want %v: %+v", got, tc.rootWrite, ec.Sandbox)
			}
			if got := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(scratch, "file")) == nil; got != tc.scratchWrite {
				t.Fatalf("scratch write = %v, want %v", got, tc.scratchWrite)
			}
			if ec.Sandbox.AllowNetwork != tc.net || ec.Sandbox.PrivateHome != tc.private || ec.ReadOnly == tc.rootWrite {
				t.Fatalf("wrong policy: %+v", ec)
			}
			if err := sandbox.ReadAllowed(ec.Sandbox, root); err != nil {
				t.Fatalf("read-only root hidden: %v", err)
			}
			if parent.HasSandbox() || !parent.UnsafeNoSandbox() {
				t.Fatal("child policy mutated the parent")
			}
		})
	}
}

func TestAgentSandboxInheritedCeiling(t *testing.T) {
	root, scratch := realTempDir(t), realTempDir(t)
	secret, protected := filepath.Join(root, "secret"), filepath.Join(root, "protected")
	for _, path := range []string{secret, protected} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := sandbox.Config{WritablePaths: []string{root}, DenyPaths: []string{secret}, DenyWritePaths: []string{protected}, DenyHostTemp: true, DenyDNS: true, AllowEnv: []string{"PATH"}, Env: map[string]string{"CHILD_CACHE": "restricted"}}
	parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), base), WithUnsafeNoSandbox())
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, Scratch: scratch, SandboxPresets: []string{"workspace+net+sshkeys"}})
	if err != nil {
		t.Fatal(err)
	}
	if ec.Sandbox.Env["CHILD_CACHE"] != "restricted" {
		t.Fatal("inherited policy environment lost")
	}
	if ec.Sandbox.AllowNetwork || !ec.Sandbox.DenyDNS || !ec.Sandbox.DenyHostTemp || !slices.Equal(ec.Sandbox.AllowEnv, base.AllowEnv) {
		t.Fatalf("inherited restrictions widened: %+v", ec.Sandbox)
	}
	if sandbox.ReadAllowed(ec.Sandbox, secret) == nil || sandbox.WriteAllowed(ec.Sandbox, protected) == nil {
		t.Fatal("inherited deny lost")
	}
	if sandbox.ReadAllowed(ec.Sandbox, filepath.Join(sandbox.ResolvedHomeDir(), ".ssh", "id_rsa")) == nil {
		t.Fatal("child sshkeys escaped the inherited credential mask")
	}
	bound, _, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if bound.UnsafeNoSandbox() {
		t.Fatal("explicit policy inherited unsafe bypass")
	}
	if _, err := bound.SetSandboxLayer("escape", &SandboxLayer{Config: sandbox.Config{AllowNetwork: true}}); err == nil {
		t.Fatal("bound policy accepted a widening layer")
	}
	_, cfg, err := bound.newSandboxFor("later tool", &sandbox.Config{AllowNetwork: true, WritablePaths: []string{scratch}, PassEnv: []string{"SECRET_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllowNetwork || slices.Contains(cfg.PassEnv, "SECRET_TOKEN") {
		t.Fatal("later tool overlay widened policy")
	}
	if _, _, err := bound.prepareMCPServerTools(context.Background(), &MCPConfig{Transport: "streamable", URL: "http://127.0.0.1:1"}, "remote", "remote", "", nil); err == nil || !strings.Contains(err.Error(), "network policy") {
		t.Fatalf("remote MCP bypassed network denial: %v", err)
	}
}

func TestAgentSandboxFactoryFailureIsLaunchFailure(t *testing.T) {
	for _, nilSandbox := range []bool{false, true} {
		parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox(), WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
			if nilSandbox {
				return nil, nil
			}
			return nil, errors.New("backend unavailable")
		}, sandbox.Config{}))
		ec, err := parent.ExecutionPolicy(realTempDir(t), ExecutionGrant{SandboxPresets: []string{"base"}})
		if err != nil {
			t.Fatal(err)
		}
		// No process tool is requested; backend failure must still stop launch.
		bound, _, err := parent.BindExecutionContext(ec, []string{})
		if err == nil || bound != nil || !strings.Contains(err.Error(), "construct agent sandbox") {
			t.Fatalf("factory failure became an omission: %v %v", bound, err)
		}
		parent.Close()
	}
}

func TestAgentSandboxInheritanceUnchangedWithoutPreset(t *testing.T) {
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(realTempDir(t), ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if bound.HasSandbox() || !bound.UnsafeNoSandbox() {
		t.Fatal("ordinary unsandboxed inheritance changed")
	}
}

func TestAgentSandboxDescendantRetainsFloor(t *testing.T) {
	root := realTempDir(t)
	parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{WritablePaths: []string{root}, AllowNetwork: true}))
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, SandboxPresets: []string{"base"}})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	derived := bound.Derive()
	defer derived.Close()
	for _, specs := range [][]string{nil, {"workspace+net"}} {
		child, err := derived.ExecutionPolicy(realTempDir(t), ExecutionGrant{SandboxPresets: specs, Scratch: realTempDir(t)})
		if err != nil {
			t.Fatal(err)
		}
		if child.Sandbox.AllowNetwork || !child.ReadOnly || sandbox.WriteAllowed(child.Sandbox, child.Root) == nil {
			t.Fatalf("descendant widened floor: %+v", child)
		}
	}
}

func TestAgentSandboxSSHGrantsCannotEscapeFloor(t *testing.T) {
	skipIfWindows(t)
	home, root := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	ssh := filepath.Join(home, ".ssh")
	if err := os.Mkdir(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "known_hosts", "id_rsa"} {
		if err := os.WriteFile(filepath.Join(ssh, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the Unix socket name below macOS's 104-byte sockaddr limit.
	dir, err := os.MkdirTemp(os.TempDir(), "ssh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SSH_AUTH_SOCK", socket)
	for _, tc := range []struct {
		specs               []string
		config, keys, agent bool
	}{
		{[]string{"workspace+sshkeys"}, true, true, false},
		{[]string{"workspace+ssh"}, true, false, true},
		{[]string{"workspace", "workspace+sshkeys+ssh"}, false, false, false},
		{[]string{"workspace+ssh", "workspace+sshkeys+ssh"}, true, false, true},
		{[]string{"workspace+sshkeys+ssh", "workspace+ssh"}, true, false, true},
	} {
		parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
		ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: tc.specs})
		parent.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := sandbox.ReadAllowed(ec.Sandbox, filepath.Join(ssh, "config")) == nil; got != tc.config {
			t.Fatalf("%v: SSH config read = %v", tc.specs, got)
		}
		if got := sandbox.ReadAllowed(ec.Sandbox, filepath.Join(ssh, "id_rsa")) == nil; got != tc.keys {
			t.Fatalf("%v: SSH private key read = %v", tc.specs, got)
		}
		if got := len(ec.Sandbox.AllowUnixSockets) != 0; got != tc.agent {
			t.Fatalf("%v: SSH agent grant = %v", tc.specs, got)
		}
		if got := slices.Contains(ec.Sandbox.PassEnv, "SSH_AUTH_SOCK"); got != tc.agent {
			t.Fatalf("%v: SSH env passthrough = %v", tc.specs, got)
		}
	}
}

func TestAgentSandboxBaseDeniesRootEqualToTemp(t *testing.T) {
	root := realTempDir(t)
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: []string{"base"}})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.WriteAllowed(ec.Sandbox, filepath.Join(root, "file")) == nil {
		t.Fatal("base preset granted root through temp")
	}
}

func TestAgentSandboxInheritedBaseDoesNotAcquireWorkspace(t *testing.T) {
	root, source := realTempDir(t), realTempDir(t)
	parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{DenyHostTemp: true}))
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: source, Scratch: realTempDir(t), SandboxPresets: []string{"workspace+net"}})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.WriteAllowed(ec.Sandbox, root) == nil {
		t.Fatal("inherited base acquired workspace writes")
	}
}

func TestAgentSandboxUnsupportedPlatformFailsBinding(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("unsupported platform test")
	}
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(realTempDir(t), ExecutionGrant{SandboxPresets: []string{"readonly"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parent.BindExecutionContext(ec, []string{}); err == nil {
		t.Fatal("unsupported backend accepted")
	}
}
