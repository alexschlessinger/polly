package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestExposedCredentialsNamesGrantsAtOrInsideMasks(t *testing.T) {
	home := tempHome(t)
	for _, dir := range []string{".aws/sso", ".ssh", ".gnupg/private-keys-v1.d", ".cargo", "Library/Keychains", "src"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".cargo", "credentials.toml"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ReadPaths: []string{
			"~/.aws/sso",
			filepath.Join(home, ".ssh"),
			"~/.cargo/credentials.toml",
			// Containing a mask exposes nothing: the deeper mask still wins.
			"~/Library",
			"~/.cargo",
			"~/src",
		},
		WritablePaths: []string{"~/.gnupg/private-keys-v1.d"},
		PassEnv:       []string{"NPM_TOKEN", "EDITOR", "NPM_TOKEN"},
	}
	paths, env := ExposedCredentials(cfg)
	want := []string{
		filepath.Join(home, ".aws", "sso"),
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".cargo", "credentials.toml"),
		filepath.Join(home, ".gnupg", "private-keys-v1.d"),
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("ExposedCredentials paths = %v, want %v", paths, want)
	}
	if !slices.Equal(env, []string{"NPM_TOKEN"}) {
		t.Fatalf("ExposedCredentials env = %v, want [NPM_TOKEN]", env)
	}
}

func TestExposedCredentialsAllowEnvReplacesPassEnv(t *testing.T) {
	tempHome(t)
	_, env := ExposedCredentials(Config{AllowEnv: []string{"PATH", "GH_TOKEN"}, PassEnv: []string{"NPM_TOKEN"}})
	if !slices.Equal(env, []string{"GH_TOKEN"}) {
		t.Fatalf("ExposedCredentials env = %v, want only the allowlisted GH_TOKEN", env)
	}
}

func TestExposedCredentialsEmptyForDefaults(t *testing.T) {
	tempHome(t)
	if paths, env := ExposedCredentials(DefaultConfig()); len(paths) != 0 || len(env) != 0 {
		t.Fatalf("ExposedCredentials(DefaultConfig()) = %v, %v, want nothing", paths, env)
	}
}
