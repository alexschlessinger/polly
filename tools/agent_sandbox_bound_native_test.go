package tools

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentSandboxNativeBoundUnsandboxedParent(t *testing.T) {
	skipUnlessSandboxTests(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	network := fmt.Sprintf("printf probe > /dev/tcp/127.0.0.1/%d", listener.Addr().(*net.TCPAddr).Port)
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readonly=%v", readOnly), func(t *testing.T) {
			root, scratch := realTempDir(t), realTempDir(t)
			secret, protected := filepath.Join(root, "secret"), filepath.Join(root, "protected")
			for _, path := range []string{secret, protected} {
				if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
			defer parent.Close()
			for _, name := range []string{"bash", "read_file", "write_file"} {
				if _, err := parent.LoadToolAuto(name); err != nil {
					t.Fatal(err)
				}
			}
			ec, err := parent.ExecutionPolicy(root, ExecutionGrant{ReadOnly: readOnly, Scratch: scratch, DeniedReads: []string{secret}, DeniedWrites: []string{protected}})
			if err != nil {
				t.Fatal(err)
			}
			ec.Sandbox.PrivateHome = true
			bound, _, err := parent.BindExecutionContext(ec, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bound.Close()
			bash, _ := bound.Get("bash")
			if out, err := bash.Execute(context.Background(), map[string]any{"command": network}); err != nil {
				t.Fatalf("unsandboxed bound parent network: %s %v", out, err)
			}
			for _, specs := range [][]string{{"workspace+net"}, {"workspace", "workspace+net"}} {
				childEC, err := bound.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, Scratch: scratch, SandboxPresets: specs})
				if err != nil {
					t.Fatal(err)
				}
				child, _, err := bound.BindExecutionContext(childEC, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer child.Close()
				bash, _ := child.Get("bash")
				for command, want := range map[string]bool{
					network:                                 len(specs) == 1,
					"cat secret":                            false,
					"printf probe > protected":              false,
					"printf probe > root-file":              !readOnly,
					`printf probe > "$TMPDIR/scratch-file"`: true,
				} {
					out, err := bash.Execute(context.Background(), map[string]any{"command": command})
					if (err == nil) != want {
						t.Fatalf("%v: %s: %s %v; want success %v", specs, command, out, err, want)
					}
				}
				reader, _ := child.Get("read_file")
				if _, err := reader.Execute(context.Background(), map[string]any{"path": secret}); err == nil {
					t.Fatal("lost native read denial")
				}
				writer, _ := child.Get("write_file")
				for path, want := range map[string]bool{protected: false, "native-file": !readOnly, filepath.Join(scratch, "native-file"): true} {
					_, err := writer.Execute(context.Background(), map[string]any{"path": path, "content": "probe"})
					if (err == nil) != want {
						t.Fatalf("native write %s: %v; want success %v", path, err, want)
					}
				}
			}
			if bound.HasSandbox() || !bound.UnsafeNoSandbox() {
				t.Fatal("child changed parent's process posture")
			}
		})
	}
}
