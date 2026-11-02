package scratch

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinDefaultScratchFitsUnixSocketsWithLongTempDir(t *testing.T) {
	owner := t.TempDir()
	longTemp := filepath.Join(owner, "var", "folders", strings.Repeat("a", 32), "T")
	if err := os.MkdirAll(longTemp, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(RootEnv, "")
	t.Setenv("TMPDIR", longTemp)
	if filepath.Dir(Root()) != "/private/tmp" {
		t.Fatalf("default scratch root follows long TMPDIR: %s", Root())
	}
	dir, err := Reserve(filepath.Join(owner, "slot-0000"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Release(dir); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"psock1234567890/a.sock", "pagent1234567890/agent.sock"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("worker socket path (%d bytes) does not bind: %v", len(path), err)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDarwinPrivateRootsKeepPreviousTempRootMasked(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	active := isolateRoot(t)
	want := []string{active, RootUnder("/private/tmp"), RootUnder(os.TempDir())}
	roots := PrivateRoots()
	for _, root := range want {
		found := false
		for _, got := range roots {
			found = found || got == root
		}
		if !found {
			t.Fatalf("private roots %v omitted %s", roots, root)
		}
	}
}
