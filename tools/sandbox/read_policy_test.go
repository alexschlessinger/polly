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
	if err := ReadMasked(Config{}, filepath.Join(home, "secret", "key")); err != nil {
		t.Fatalf("ReadMasked inside a private root = %v, want masks only", err)
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

// aliasFixture returns a directory and a symlink alias to it, so a policy path
// can be spelled one segment shorter than its canonical route.
func aliasFixture(t *testing.T) (real, alias string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real = filepath.Join(dir, "real")
	mustMkdirAll(t, real)
	alias = filepath.Join(dir, "a")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return real, alias
}

func TestReadAllowedDepthIsCanonicalForAliasSpelledMask(t *testing.T) {
	real, alias := aliasFixture(t)
	mustMkdirAll(t, filepath.Join(real, "root"))
	// The grant is canonical (one segment deeper than the alias); the mask is
	// spelled through the alias and does not exist yet. The mask is still the
	// deeper rule.
	cfg := Config{ReadPaths: []string{filepath.Join(real, "root")}, DenyPaths: []string{filepath.Join(alias, "root", "secret")}}
	if err := ReadAllowed(cfg, filepath.Join(alias, "root", "secret", "f")); err == nil {
		t.Fatal("ReadAllowed under an alias-spelled mask = nil, want the deeper mask to win")
	}
	if err := ReadAllowed(cfg, filepath.Join(real, "root", "secret", "f")); err == nil {
		t.Fatal("ReadAllowed under the canonical spelling of the mask = nil, want the deeper mask to win")
	}
	if err := ReadAllowed(cfg, filepath.Join(alias, "root", "open")); err != nil {
		t.Fatalf("ReadAllowed beside the mask = %v, want the grant", err)
	}
}

func TestReadAllowedAliasSpelledPrivateRootWinsOverEqualGrant(t *testing.T) {
	real, alias := aliasFixture(t)
	mustMkdirAll(t, filepath.Join(real, "home", "pub"))
	home := filepath.Join(alias, "home")
	modelPrivateRoots(t, home)
	for _, cfg := range []Config{{WritablePaths: []string{home}}, {ReadPaths: []string{filepath.Join(real, "home")}}} {
		if err := ReadAllowed(cfg, filepath.Join(home, "x")); err == nil {
			t.Fatalf("ReadAllowed(%+v) with a grant equal to the alias-spelled private root = nil, want the root to win", cfg)
		}
		if err := WriteAllowed(cfg, filepath.Join(home, "x")); err == nil {
			t.Fatalf("WriteAllowed(%+v) with a grant equal to the alias-spelled private root = nil, want the root to win", cfg)
		}
	}
	if err := ReadAllowed(Config{ReadPaths: []string{filepath.Join(home, "pub")}}, filepath.Join(real, "home", "pub", "f")); err != nil {
		t.Fatalf("ReadAllowed under a deeper grant spelled through the alias = %v, want allowed", err)
	}
}

func TestPolicyFailsClosedWhenFrozenGrantIsRetargeted(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vendor := filepath.Join(dir, "vendor")
	elsewhere := filepath.Join(dir, "elsewhere")
	mustMkdirAll(t, vendor, elsewhere)
	cfg, err := PrepareConfig(Config{WritablePaths: []string{vendor}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReadAllowed(cfg, filepath.Join(vendor, "f")); err != nil {
		t.Fatalf("ReadAllowed before the retarget = %v, want allowed", err)
	}
	if err := os.Remove(vendor); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, vendor); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ReadAllowed(cfg, filepath.Join(elsewhere, "f")); err == nil || !strings.Contains(err.Error(), "rerouted") {
		t.Fatalf("ReadAllowed after the grant was retargeted = %v, want a rerouted-grant failure", err)
	}
	if err := WriteAllowed(cfg, filepath.Join(elsewhere, "f")); err == nil || !strings.Contains(err.Error(), "rerouted") {
		t.Fatalf("WriteAllowed after the grant was retargeted = %v, want a rerouted-grant failure", err)
	}
}

func TestReadPolicyRejectsRelativePaths(t *testing.T) {
	for _, path := range []string{"notes/secret.txt", ".", ""} {
		if err := ReadAllowed(Config{}, path); err == nil {
			t.Fatalf("ReadAllowed(%q) = nil, want a refusal: a relative path meets no rule", path)
		}
		if err := ReadMasked(Config{}, path); err == nil {
			t.Fatalf("ReadMasked(%q) = nil, want a refusal", path)
		}
	}
}

func TestPrepareConfigKeepsGrantsBelowABoundary(t *testing.T) {
	dir := t.TempDir()
	grant := filepath.Join(dir, "grant")
	deny := filepath.Join(grant, "deny")
	inner := filepath.Join(deny, "inner")
	mustMkdirAll(t, inner)

	// A grant inside a denied directory under an outer grant re-opens the
	// denied tree and must survive preparation.
	cfg, err := PrepareConfig(Config{ReadPaths: []string{grant, inner}, DenyPaths: []string{deny}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReadAllowed(cfg, filepath.Join(inner, "c")); err != nil {
		t.Fatalf("prepared config lost the grant below the mask: %v", err)
	}
	if err := ReadAllowed(cfg, filepath.Join(deny, "b")); err == nil {
		t.Fatal("the mask between the grants must still deny")
	}

	// A grant equal to the mask wins the read tie only while it exists.
	cfg, err = PrepareConfig(Config{ReadPaths: []string{grant, deny}, DenyPaths: []string{deny}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReadAllowed(cfg, filepath.Join(deny, "b")); err != nil {
		t.Fatalf("prepared config lost the grant tying the mask: %v", err)
	}

	// Writes follow the same rule.
	cfg, err = PrepareConfig(Config{WritablePaths: []string{grant, inner}, DenyPaths: []string{deny}})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAllowed(cfg, filepath.Join(inner, "c")); err != nil {
		t.Fatalf("prepared config lost the writable grant below the mask: %v", err)
	}
	if err := WriteAllowed(cfg, filepath.Join(deny, "b")); err == nil {
		t.Fatal("the mask between the writable grants must still deny")
	}

	// Without a boundary the inner grant is redundant and is still dropped.
	cfg, err = PrepareConfig(Config{ReadPaths: []string{grant, inner}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ReadPaths) != 1 {
		t.Fatalf("redundant nested grant kept: %v", cfg.ReadPaths)
	}
}

func TestPrepareConfigKeepsGrantInsidePrivateRootUnderOuterGrant(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	project := filepath.Join(home, "proj")
	mustMkdirAll(t, project, filepath.Join(home, "other"))
	modelPrivateRoots(t, home)
	cfg, err := PrepareConfig(Config{ReadPaths: []string{dir, project}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReadAllowed(cfg, filepath.Join(project, "f")); err != nil {
		t.Fatalf("prepared config lost the grant inside the private root: %v", err)
	}
	if err := ReadAllowed(cfg, filepath.Join(home, "other", "f")); err == nil {
		t.Fatal("the private root between the grants must still hide the rest of the home")
	}
}
