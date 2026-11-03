package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestRestoreArtifactExactBytes(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("CRLF\r\nno final newline"), {0, 0xff, 0xfe, '\r', '\n', 0}, bytes.Repeat([]byte("binary\x00\r\n"), 20000)} {
		dir := t.TempDir()
		path := filepath.Join(dir, "restored")
		ref := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindBinary, Data: data})
		source := &restoreTestReader{reader: bytes.NewReader(data)}
		registry := NewToolRegistry(nil)
		defer registry.Close()
		got, err := RestoreArtifact(context.Background(), registry, path, ref, source)
		if err != nil || got != path {
			t.Fatalf("RestoreArtifact = %q, %v", got, err)
		}
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, data) {
			t.Fatalf("restored %d bytes incorrectly: got %d, %v", len(data), len(actual), err)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 != 0 {
			t.Fatalf("restored mode = %v, %v", info, err)
		}
		if source.closed != 1 || source.maxRead > 32<<10 {
			t.Fatalf("source closes=%d max read=%d", source.closed, source.maxRead)
		}
		assertRestoreEntries(t, dir, "restored")
	}
}

func TestRestoreArtifactFailureCleanup(t *testing.T) {
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("expected")})
	failure := errors.New("injected stream failure")
	for _, tc := range []struct {
		name  string
		data  string
		read  error
		close error
		want  string
	}{
		{name: "short", data: "short", want: "size mismatch"},
		{name: "long", data: "expected EXTRA", want: "size mismatch"},
		{name: "digest", data: "corrupt!", want: "digest mismatch"},
		{name: "read", data: "expect", read: failure, want: failure.Error()},
		{name: "end read", data: "expected", read: failure, want: failure.Error()},
		{name: "close", data: "expected", close: failure, want: failure.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := &restoreTestReader{reader: strings.NewReader(tc.data), readErr: tc.read, closeErr: tc.close}
			_, err := RestoreArtifact(context.Background(), NewToolRegistry(nil), filepath.Join(dir, "out"), ref, source)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("restore failure = %v, want %q", err, tc.want)
			}
			if source.closed != 1 {
				t.Fatalf("source closed %d times", source.closed)
			}
			assertRestoreEntries(t, dir)
		})
	}
	t.Run("cancellation", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := &restoreTestReader{reader: strings.NewReader("expected"), beforeRead: cancel}
		_, err := RestoreArtifact(ctx, NewToolRegistry(nil), filepath.Join(dir, "out"), ref, source)
		if !errors.Is(err, context.Canceled) || source.closed != 1 {
			t.Fatalf("cancellation = %v, closes=%d", err, source.closed)
		}
		assertRestoreEntries(t, dir)
	})
}

func TestRestoreArtifactRefusesExistingDestinations(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "out")
			target := filepath.Join(dir, "target")
			switch kind {
			case "file":
				writeTestFile(t, dir, "out", "keep me")
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "symlink" {
					writeTestFile(t, dir, "target", "keep me")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("replace")})
			source := &restoreTestReader{reader: strings.NewReader("replace")}
			if _, err := RestoreArtifact(context.Background(), NewToolRegistry(nil), path, ref, source); err == nil {
				t.Fatal("overwrote an existing destination")
			}
			if source.closed != 1 || source.maxRead != 0 {
				t.Fatalf("source closed=%d maxRead=%d", source.closed, source.maxRead)
			}
			switch kind {
			case "file", "symlink":
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "keep me" {
					t.Fatalf("existing content changed: %q %v", data, err)
				}
			case "dangling symlink":
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("created dangling symlink target: %v", err)
				}
			}
		})
	}
}

func TestRestoreArtifactPolicyAndSymlinkRoutes(t *testing.T) {
	allowed, denied := realTempDir(t), realTempDir(t)
	registry := stubSandboxRegistry(t, sandbox.Config{WritablePaths: []string{allowed}, DenyHostTemp: true, DenyPaths: []string{denied}})
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("ok")})
	restore := func(path string) error {
		_, err := RestoreArtifact(context.Background(), registry, path, ref, io.NopCloser(strings.NewReader("ok")))
		return err
	}
	if err := restore(filepath.Join(denied, "out")); err == nil {
		t.Fatal("restored to denied path")
	}
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(denied, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := restore(filepath.Join(link, "out")); err == nil {
		t.Fatal("restored through symlink to denied path")
	}
	if err := restore(filepath.Join(allowed, "out")); err != nil {
		t.Fatal(err)
	}
	assertRestoreEntries(t, denied)
	// A safe symlinked parent is usable after both routes are approved.
	safe := filepath.Join(allowed, "safe")
	if err := os.Symlink(allowed, safe); err != nil {
		t.Fatal(err)
	}
	if err := restore(filepath.Join(safe, "other")); err != nil {
		t.Fatalf("safe route: %v", err)
	}
}

func TestRestoreArtifactInstallationCannotOverwriteRacingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out")
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("artifact")})
	source := &restoreTestReader{reader: strings.NewReader("artifact"), beforeRead: func() {
		writeTestFile(t, dir, "out", "racing writer")
	}}
	if _, err := RestoreArtifact(context.Background(), NewToolRegistry(nil), path, ref, source); err == nil {
		t.Fatal("overwrote racing destination")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "racing writer" {
		t.Fatalf("racing writer changed: %q %v", data, err)
	}
	assertRestoreEntries(t, dir, "out")
}

func TestRestoreArtifactCleanupUsesPinnedDirectory(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("renaming open directories is Unix-specific")
	}
	dir, elsewhere := realTempDir(t), realTempDir(t)
	parent := filepath.Join(dir, "parent")
	moved := filepath.Join(dir, "moved")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("artifact")})
	source := &restoreTestReader{reader: strings.NewReader("bad"), beforeRead: func() {
		if err := os.Rename(parent, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, parent); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := RestoreArtifact(context.Background(), NewToolRegistry(nil), filepath.Join(parent, "out"), ref, source); err == nil {
		t.Fatal("accepted size mismatch")
	}
	assertRestoreEntries(t, moved)
	assertRestoreEntries(t, elsewhere)
}

func TestRestoreArtifactFollowsCurrentPolicy(t *testing.T) {
	skipIfWindows(t)
	dir := realTempDir(t)
	registry := stubSandboxRegistry(t, sandbox.Config{DenyHostTemp: true})
	derived := registry.Derive()
	defer derived.Close()
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("ok")})
	restore := func(name string) error {
		_, err := RestoreArtifact(context.Background(), derived, filepath.Join(dir, name), ref, io.NopCloser(strings.NewReader("ok")))
		return err
	}
	if err := restore("before"); err == nil {
		t.Fatal("restored before grant")
	}
	if _, err := registry.SetSandboxLayer("restore", &SandboxLayer{Config: sandbox.Config{WritablePaths: []string{dir}}}); err != nil {
		t.Fatal(err)
	}
	if err := restore("during"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SetSandboxLayer("restore", nil); err != nil {
		t.Fatal(err)
	}
	if err := restore("after"); err == nil {
		t.Fatal("restored after grant removal")
	}
	assertRestoreEntries(t, dir, "during")
}

func TestRestoreArtifactRegistrationFollowsSourceSkillPolicies(t *testing.T) {
	source := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer source.Close()
	binding, err := NativeOpenTools(source)(context.Background(), ToolScope{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	private := binding.Registry.Derive()
	defer private.Close()
	private.RegisterArtifactRestore(&Func{Name: "restore_artifact"})
	if _, ok := private.Get("restore_artifact"); !ok {
		t.Fatal("unrestricted restore missing")
	}
	source.stageSkillAllowance([]string{"read_*"}, nil)
	source.CommitPendingChanges()
	if _, ok := private.Get("restore_artifact"); ok {
		t.Fatal("source skill policy did not restrict private restore")
	}
	source.stageSkillAllowance([]string{"restore_artifact"}, nil)
	source.CommitPendingChanges()
	if _, ok := private.Get("restore_artifact"); !ok {
		t.Fatal("source skill allowance did not reach private restore")
	}
}

func TestRestoreArtifactRequiresExistingParent(t *testing.T) {
	dir := t.TempDir()
	ref := artifacts.RefForBlob(artifacts.Blob{Data: []byte("artifact")})
	source := &restoreTestReader{reader: strings.NewReader("artifact")}
	if _, err := RestoreArtifact(context.Background(), NewToolRegistry(nil), filepath.Join(dir, "missing", "out"), ref, source); err == nil {
		t.Fatal("created missing parent directory")
	}
	if source.closed != 1 {
		t.Fatalf("source closed %d times", source.closed)
	}
	assertRestoreEntries(t, dir)
}

func assertRestoreEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(names) {
		t.Fatalf("entries in %s = %v, %v; want %v", dir, entries, err, names)
	}
	for i, entry := range entries {
		if entry.Name() != names[i] {
			t.Fatalf("unexpected entry %q; want %q", entry.Name(), names[i])
		}
	}
}

type restoreTestReader struct {
	reader     io.Reader
	readErr    error
	closeErr   error
	closed     int
	maxRead    int
	beforeRead func()
}

func (r *restoreTestReader) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	if r.beforeRead != nil {
		f := r.beforeRead
		r.beforeRead = nil
		f()
	}
	n, err := r.reader.Read(p)
	if err == io.EOF && r.readErr != nil {
		err = r.readErr
	}
	return n, err
}

func (r *restoreTestReader) Close() error {
	r.closed++
	return r.closeErr
}
