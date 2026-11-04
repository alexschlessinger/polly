package tools

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestAgentSandboxBoundUnsandboxedParent(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "readonly"}[readOnly], func(t *testing.T) {
			root, scratch := realTempDir(t), realTempDir(t)
			secret, protected := filepath.Join(root, "secret"), filepath.Join(root, "protected")
			for _, path := range []string{secret, protected} {
				if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
			defer parent.Close()
			ec, err := parent.ExecutionPolicy(root, ExecutionGrant{Scratch: scratch, ReadOnly: readOnly, DeniedReads: []string{secret}, DeniedWrites: []string{protected}})
			if err != nil {
				t.Fatal(err)
			}
			ec.Sandbox.PrivateHome = true
			ec.Sandbox.DenyHostTemp = true
			ec.Sandbox.DenyDNS = true
			ec.Sandbox.AllowEnv = []string{"PATH"}
			bound, _, err := parent.BindExecutionContext(ec, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bound.Close()
			derived := bound.Derive()
			defer derived.Close()
			for _, source := range []*ToolRegistry{bound, derived} {
				for _, tc := range []struct {
					specs []string
					net   bool
				}{
					{[]string{"workspace+net"}, true},
					{[]string{"workspace"}, false},
					{[]string{"workspace", "workspace+net"}, false},
				} {
					child, err := source.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, Scratch: scratch, SandboxPresets: tc.specs})
					if err != nil {
						t.Fatal(err)
					}
					cfg := child.Sandbox
					if cfg.AllowNetwork != tc.net {
						t.Fatalf("%v: network = %v, want %v", tc.specs, cfg.AllowNetwork, tc.net)
					}
					if !cfg.PrivateHome || !cfg.DenyHostTemp || !cfg.DenyDNS || !slices.Equal(cfg.AllowEnv, ec.Sandbox.AllowEnv) {
						t.Fatalf("lost filesystem/environment restrictions: %+v", cfg)
					}
					if sandbox.ReadAllowed(cfg, secret) == nil || sandbox.WriteAllowed(cfg, protected) == nil {
						t.Fatal("lost native filesystem denial")
					}
					if got := sandbox.WriteAllowed(cfg, filepath.Join(root, "file")) != nil; got != readOnly || child.ReadOnly != readOnly {
						t.Fatalf("root readonly = %v, want %v", got, readOnly)
					}
					if err := sandbox.WriteAllowed(cfg, filepath.Join(scratch, "file")); err != nil {
						t.Fatalf("lost scratch grant: %v", err)
					}
				}
			}
			// Without an explicit preset, a bound unsafe parent stays unsafe.
			omitted, err := bound.ExecutionPolicy(root, ExecutionGrant{})
			if err != nil {
				t.Fatal(err)
			}
			child, _, err := bound.BindExecutionContext(omitted, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			if bound.HasSandbox() || child.HasSandbox() || !bound.UnsafeNoSandbox() || !child.UnsafeNoSandbox() {
				t.Fatal("changed omitted sandbox behavior")
			}
		})
	}
}

func TestAgentSandboxBoundUnsandboxedReadOnlySource(t *testing.T) {
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	source := realTempDir(t)
	ec, err := parent.ExecutionPolicy(source, ExecutionGrant{ReadOnly: true, Scratch: realTempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	child, err := bound.ExecutionPolicy(realTempDir(t), ExecutionGrant{SourceRoot: source, Scratch: realTempDir(t), SandboxPresets: []string{"workspace+net"}})
	if err != nil {
		t.Fatal(err)
	}
	if !child.Sandbox.AllowNetwork || !child.ReadOnly || sandbox.WriteAllowed(child.Sandbox, child.Root) == nil {
		t.Fatalf("network opt-in changed source filesystem authority: %+v", child)
	}
	if err := sandbox.WriteAllowed(child.Sandbox, child.Scratch); err != nil {
		t.Fatalf("lost child scratch: %v", err)
	}
}

func TestAgentSandboxBoundUnsandboxedSSHGrants(t *testing.T) {
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
	// A short path keeps the socket below the macOS sockaddr limit.
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
	for _, denied := range []bool{false, true} {
		parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
		defer parent.Close()
		ec, err := parent.ExecutionPolicy(root, ExecutionGrant{})
		if err != nil {
			t.Fatal(err)
		}
		ec.Sandbox.PrivateHome = true
		ec.Sandbox.ReadPaths = []string{filepath.Join(ssh, "config")}
		if denied {
			ec.Sandbox.DenyPaths = []string{ssh, socket}
		}
		bound, _, err := parent.BindExecutionContext(ec, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer bound.Close()
		for _, tc := range []struct {
			specs               []string
			config, keys, agent bool
		}{
			{[]string{"workspace+ssh"}, true, false, true},
			{[]string{"workspace+sshkeys"}, true, false, false},
			{[]string{"workspace", "workspace+sshkeys+ssh"}, false, false, false},
		} {
			child, err := bound.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: tc.specs})
			if err != nil {
				t.Fatal(err)
			}
			cfg := child.Sandbox
			if !cfg.PrivateHome {
				t.Fatal("SSH opt-in dropped private-home")
			}
			if got := sandbox.ReadAllowed(cfg, filepath.Join(ssh, "config")) == nil; got != (tc.config && !denied) {
				t.Fatalf("%v, denied=%v: SSH config read = %v", tc.specs, denied, got)
			}
			if got := sandbox.ReadAllowed(cfg, filepath.Join(ssh, "id_rsa")) == nil; got != (tc.keys && !denied) {
				t.Fatalf("%v, denied=%v: SSH key read = %v", tc.specs, denied, got)
			}
			if got := len(cfg.AllowUnixSockets) != 0; got != (tc.agent && !denied) {
				t.Fatalf("%v, denied=%v: SSH socket grant = %v", tc.specs, denied, got)
			}
		}
	}
}

func TestAgentSandboxBoundPrivateHomeReadGrants(t *testing.T) {
	skipIfWindows(t)
	home, root := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	allowed := filepath.Join(home, "allowed")
	sibling := filepath.Join(home, "sibling")
	gitConfig := filepath.Join(home, ".gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	for _, path := range []string{allowed, sibling, gitConfig} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	ec.Sandbox.PrivateHome = true
	ec.Sandbox.ReadPaths = []string{allowed}
	bound, _, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	for _, spec := range []string{"workspace+net", "workspace+net+private-home"} {
		child, err := bound.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: []string{spec}})
		if err != nil {
			t.Fatal(err)
		}
		if !child.Sandbox.AllowNetwork || !child.Sandbox.PrivateHome {
			t.Fatalf("lost process opt-in or private-home: %+v", child.Sandbox)
		}
		for _, hidden := range []string{home, sibling, gitConfig} {
			if sandbox.ReadAllowed(child.Sandbox, hidden) == nil {
				t.Fatalf("%s exposed inherited private-home path %s", spec, hidden)
			}
		}
		if spec == "workspace+net" {
			if err := sandbox.ReadAllowed(child.Sandbox, allowed); err != nil {
				t.Fatalf("lost inherited file-only read grant: %v", err)
			}
		}
	}
}
