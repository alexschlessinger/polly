package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func modelPrivateRoots(t *testing.T, roots ...string) {
	t.Helper()
	previous := platformPrivatePolicyRoots
	platformPrivatePolicyRoots = func() []string { return roots }
	t.Cleanup(func() { platformPrivatePolicyRoots = previous })
}

func mustMkdirAll(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadAllowedDeepestRuleWins(t *testing.T) {
	dir := t.TempDir()
	grant := filepath.Join(dir, "grant")
	deny := filepath.Join(grant, "deny")
	inner := filepath.Join(deny, "inner")
	mask := filepath.Join(inner, "mask")
	mustMkdirAll(t, mask)
	cfg := Config{ReadPaths: []string{grant, inner}, DenyPaths: []string{deny, mask}}
	for path, allowed := range map[string]bool{
		filepath.Join(grant, "a"): true,
		filepath.Join(deny, "b"):  false,
		filepath.Join(inner, "c"): true,
		filepath.Join(mask, "d"):  false,
	} {
		err := ReadAllowed(cfg, path)
		if allowed && err != nil {
			t.Fatalf("ReadAllowed(%s) = %v, want allowed", path, err)
		}
		if !allowed && (err == nil || !strings.Contains(err.Error(), "blocked from reads by the sandbox policy")) {
			t.Fatalf("ReadAllowed(%s) = %v, want a mask denial", path, err)
		}
	}
}

func TestReadAllowedTieFavorsGrant(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	mustMkdirAll(t, shared)
	cfg := Config{ReadPaths: []string{shared}, DenyPaths: []string{shared}}
	if err := ReadAllowed(cfg, filepath.Join(shared, "f")); err != nil {
		t.Fatalf("ReadAllowed under a grant equal to the deny = %v, want allowed", err)
	}
}

func TestReadAllowedMissingDenyStillCounts(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DenyPaths: []string{filepath.Join(dir, "absent")}}
	if err := ReadAllowed(cfg, filepath.Join(dir, "absent", "f")); err == nil {
		t.Fatal("ReadAllowed under a missing deny = nil, want a mask denial")
	}
	if err := ReadAllowed(cfg, filepath.Join(dir, "other")); err != nil {
		t.Fatalf("ReadAllowed beside a missing deny = %v, want allowed", err)
	}
}

func TestReadAllowedPrivateRootRequiresGrant(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	pub := filepath.Join(home, "pub")
	mustMkdirAll(t, filepath.Join(home, "secret"), pub)
	modelPrivateRoots(t, home)

	err := ReadAllowed(Config{}, filepath.Join(home, "secret", "key"))
	if err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("ReadAllowed inside a private root = %v, want a private-root denial", err)
	}
	if err := ReadAllowed(Config{}, filepath.Join(dir, "outside")); err != nil {
		t.Fatalf("ReadAllowed outside private roots = %v, want allowed", err)
	}
	granted := Config{ReadPaths: []string{pub}}
	for _, path := range []string{pub, filepath.Join(pub, "f")} {
		if err := ReadAllowed(granted, path); err != nil {
			t.Fatalf("ReadAllowed(%s) with a grant = %v, want allowed", path, err)
		}
	}
	if err := ReadAllowed(granted, filepath.Join(home, "secret")); err == nil {
		t.Fatal("ReadAllowed beside a grant = nil, want the private root to win")
	}
	if err := ReadAllowed(Config{WritablePaths: []string{home}}, filepath.Join(home, "x")); err == nil {
		t.Fatal("ReadAllowed with a grant equal to the private root = nil, want the root to win")
	}
	if err := readMasked(Config{}, filepath.Join(home, "secret", "key")); err != nil {
		t.Fatalf("readMasked inside a private root = %v, want masks only", err)
	}
	if err := WriteAllowed(Config{WritablePaths: []string{home}}, filepath.Join(home, "x")); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("WriteAllowed with a grant equal to the private root = %v, want a private-root denial", err)
	}
	if err := WriteAllowed(Config{WritablePaths: []string{pub}}, filepath.Join(pub, "x")); err != nil {
		t.Fatalf("WriteAllowed under a writable grant inside the private root = %v, want allowed", err)
	}
}

func TestReadAllowedPrivateRootInsideHostTempWins(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	mustMkdirAll(t, home)
	modelPrivateRoots(t, home)
	if err := ReadAllowed(Config{}, filepath.Join(home, "f")); err == nil {
		t.Fatal("ReadAllowed inside a private root under host temp = nil, want the deeper root to win")
	}
	if err := WriteAllowed(Config{}, filepath.Join(home, "f")); err == nil {
		t.Fatal("WriteAllowed inside a private root under host temp = nil, want the deeper root to win")
	}
	if err := ReadAllowed(Config{}, filepath.Join(dir, "f")); err != nil {
		t.Fatalf("ReadAllowed beside the private root under host temp = %v, want the temp grant", err)
	}
}
