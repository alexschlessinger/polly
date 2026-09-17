package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// compiledFixture builds a policy that meets every kind of route: nested
// grants and masks, an alias-spelled mask that does not exist, a file in the
// middle of a queried path, and a modeled private root with a grant inside.
func compiledFixture(t *testing.T) (Config, []string) {
	t.Helper()
	real, alias := aliasFixture(t)
	grant := filepath.Join(real, "grant")
	deny := filepath.Join(grant, "deny")
	inner := filepath.Join(deny, "inner")
	mask := filepath.Join(inner, "mask")
	home := filepath.Join(real, "home")
	pub := filepath.Join(home, "pub")
	mustMkdirAll(t, mask, pub, filepath.Join(real, "open"))
	file := filepath.Join(real, "open", "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	modelPrivateRoots(t, home)
	cfg := Config{
		ReadPaths: []string{grant, inner, filepath.Join(alias, "open"), pub},
		DenyPaths: []string{deny, mask, filepath.Join(alias, "absent"), filepath.Join(real, "open", "file")},
	}
	var paths []string
	for _, base := range []string{real, alias} {
		paths = append(paths,
			filepath.Join(base, "grant", "a"),
			filepath.Join(base, "grant", "deny", "b"),
			filepath.Join(base, "grant", "deny", "inner", "c"),
			filepath.Join(base, "grant", "deny", "inner", "mask", "d"),
			filepath.Join(base, "absent", "e"),
			filepath.Join(base, "absent"),
			filepath.Join(base, "open", "f"),
			filepath.Join(base, "open", "file"),
			filepath.Join(base, "open", "file", "below"),
			filepath.Join(base, "home", "secret"),
			filepath.Join(base, "home", "pub", "g"),
			filepath.Join(base, "home"),
			filepath.Join(base, "elsewhere", "h"),
			base,
		)
	}
	return cfg, append(paths, "relative/path", "", "/")
}

func TestCompiledReadPolicyMatchesPerCall(t *testing.T) {
	cfg, paths := compiledFixture(t)
	policy, err := CompileReadPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	masks, err := compileReadPolicy(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if got, want := errText(policy.Allowed(path)), errText(ReadAllowed(cfg, path)); got != want {
			t.Errorf("Allowed(%s) = %q, ReadAllowed = %q", path, got, want)
		}
		if got, want := errText(masks.Allowed(path)), errText(ReadMasked(cfg, path)); got != want {
			t.Errorf("masks.Allowed(%s) = %q, ReadMasked = %q", path, got, want)
		}
	}
}

func TestCompiledReadPolicyRouteCreatedAfterCompile(t *testing.T) {
	real, alias := aliasFixture(t)
	cfg := Config{ReadPaths: []string{real}, DenyPaths: []string{filepath.Join(alias, "secret")}}
	policy, err := CompileReadPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(real, "secret"))
	for _, path := range []string{filepath.Join(alias, "secret", "f"), filepath.Join(real, "secret", "f")} {
		if err := policy.Allowed(path); err == nil {
			t.Fatalf("Allowed(%s) after the mask was created = nil, want masked", path)
		}
	}
	if err := policy.Allowed(filepath.Join(real, "open")); err != nil {
		t.Fatalf("Allowed beside the mask = %v, want the grant", err)
	}
}

func TestCompiledReadPolicyAliasCreatedAfterCompile(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret")
	mustMkdirAll(t, secret)
	policy, err := CompileReadPolicy(Config{ReadPaths: []string{dir}, DenyPaths: []string{secret}})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := policy.Allowed(filepath.Join(link, "f")); err == nil {
		t.Fatal("Allowed through an alias created after compilation = nil, want the canonical route masked")
	}
}

func TestCompiledReadPolicyFailsClosedWhenGrantRetargetedMidLoop(t *testing.T) {
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
	policy, err := CompileReadPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Allowed(filepath.Join(vendor, "f")); err != nil {
		t.Fatalf("Allowed before the retarget = %v, want allowed", err)
	}
	if err := os.Remove(vendor); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, vendor); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := policy.Allowed(filepath.Join(elsewhere, "f")); err == nil || !strings.Contains(err.Error(), "sandbox authority path") {
		t.Fatalf("Allowed after the grant was retargeted = %v, want an authority failure", err)
	}
	if _, err := CompileReadPolicy(cfg); err == nil || !strings.Contains(err.Error(), "rerouted") {
		t.Fatalf("CompileReadPolicy after the retarget = %v, want a rerouted-grant failure", err)
	}
}

func TestCompiledReadPolicyZeroValueFailsClosed(t *testing.T) {
	var policy ReadPolicy
	if err := policy.Allowed(filepath.Join(t.TempDir(), "f")); err == nil || !strings.Contains(err.Error(), "sandbox read policy") {
		t.Fatalf("zero ReadPolicy.Allowed = %v, want a refusal", err)
	}
}

func TestCompiledReadPolicyPrivateRootsCapturedAtCompile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	mustMkdirAll(t, home)
	modelPrivateRoots(t, home)
	policy, err := CompileReadPolicy(Config{})
	if err != nil {
		t.Fatal(err)
	}
	modelPrivateRoots(t)
	if err := ReadAllowed(Config{}, filepath.Join(home, "f")); err != nil {
		t.Fatalf("ReadAllowed with no private roots = %v, want allowed", err)
	}
	if err := policy.Allowed(filepath.Join(home, "f")); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("compiled policy after the roots changed = %v, want the compile-time root", err)
	}
}
