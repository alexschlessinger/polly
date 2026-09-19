package envstorage

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T) (Roots, Spec) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native sandbox")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := Roots{Cache: filepath.Join(base, "cache"), SharedCache: filepath.Join(base, "shared"), State: filepath.Join(base, "state"), Config: filepath.Join(base, "config"), Control: filepath.Join(base, "control")}
	s := Spec{Allocations: []Allocation{{Name: "build", Kind: "cache", Purpose: "build cache", Shared: true}, {Name: "tool", Kind: "state", Purpose: "dependencies"}, {Name: "tool", Kind: "config", Purpose: "configuration"}}, Links: []Link{{Path: "@state/tool/settings", Target: "@config/tool/settings"}}}
	return r, s
}

func TestOwnedStorageReset(t *testing.T) {
	r, s := fixture(t)
	if err := r.Ensure(s); err != nil {
		t.Fatal(err)
	}
	if err := r.Ensure(s); err != nil {
		t.Fatal(err)
	}
	config, _ := r.Resolve(s, "@config/tool/settings")
	state, _ := r.Resolve(s, "@state/tool/dependency")
	cache, _ := r.Resolve(s, "@cache/build/object")
	for _, p := range []string{config, state, cache} {
		if err := os.WriteFile(p, []byte("kept"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Clean(context.Background(), s, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache survived: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatal(err)
	}
	if err := r.Clean(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("dependency survived: %v", err)
	}
	link, _ := r.Resolve(s, "@state/tool/settings")
	if b, err := os.ReadFile(link); err != nil || string(b) != "kept" {
		t.Fatalf("configuration: %q %v", b, err)
	}
}

func TestStorageDoesNotAdoptOrFollow(t *testing.T) {
	r, s := fixture(t)
	path := r.Path(s.Allocations[0])
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.Ensure(s); err == nil {
		t.Fatal("adopted an existing directory")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := r.Ensure(s); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, path); err != nil {
		t.Fatal(err)
	}
	if err := r.Clean(context.Background(), s, true); err == nil {
		t.Fatal("cleaned a replaced root")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal(err)
	}
}

func TestStorageRejectsInvalidDeclarations(t *testing.T) {
	_, s := fixture(t)
	for _, ref := range []string{"@state/tool/../outside", "@state/tool//x", "@state/tool/a\\b", "/tmp/x", "@cache/missing"} {
		if _, _, err := s.Lookup(ref); err == nil {
			t.Fatalf("accepted %s", ref)
		}
	}
	s.Allocations[0].Kind = "config"
	if err := s.Validate(); err == nil {
		t.Fatal("shared configuration")
	}
}

func TestLeaseExcludesOtherSessions(t *testing.T) {
	r, _ := fixture(t)
	path := filepath.Join(r.Control, "lease")
	a, err := OpenLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Exclusive(func() error { t.Error("cleanup ran with another lease"); return nil }); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("exclusive: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Exclusive(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
