package tools

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestReadArtifactFileExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		kind artifacts.Kind
	}{
		{"empty", nil, artifacts.KindText},
		{"source.go", []byte("package main\r\n// no final newline"), artifacts.KindText},
		{"binary", []byte{0, 255, 128, '\r', '\n', 0}, artifacts.KindBinary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
			defer registry.Close()
			blob, err := ReadArtifactFile(context.Background(), registry, path, int64(len(tc.data)))
			if err != nil || !bytes.Equal(blob.Data, tc.data) || blob.Name != path || blob.Kind != tc.kind {
				t.Fatalf("capture = %+v, %v", blob, err)
			}
			if _, err := ReadArtifactFile(context.Background(), registry, path, math.MaxInt64); err == nil {
				t.Fatal("overflowing limit accepted")
			}
			if len(tc.data) > 0 {
				if blob, err := ReadArtifactFile(context.Background(), registry, path, int64(len(tc.data)-1)); err == nil || blob.Data != nil {
					t.Fatalf("oversized capture = %+v, %v", blob, err)
				}
			}
		})
	}
}

func TestReadArtifactFilePolicyAndInvalidFiles(t *testing.T) {
	root, denied := t.TempDir(), t.TempDir()
	secret := writeTestFile(t, denied, "secret", "private")
	registry := stubSandboxRegistry(t, sandbox.Config{ReadPaths: []string{root}, DenyPaths: []string{denied}})
	for _, path := range []string{secret, root, filepath.Join(root, "missing"), ""} {
		if blob, err := ReadArtifactFile(context.Background(), registry, path, 100); err == nil || blob.Data != nil {
			t.Fatalf("capture %q = %+v, %v", path, blob, err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadArtifactFile(context.Background(), registry, link, 100); err == nil {
		t.Fatal("capture followed a link into a denied path")
	}
}

func TestReadArtifactFileCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry := NewToolRegistry(nil)
	defer registry.Close()
	if _, err := ReadArtifactFile(ctx, registry, "missing", 100); err != context.Canceled {
		t.Fatalf("canceled capture = %v", err)
	}
}
