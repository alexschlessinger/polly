package sandbox

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRexPresetValidationAndOptIn(t *testing.T) {
	if err := ValidatePreset("rex"); err != nil {
		t.Fatalf("ValidatePreset(rex): %v", err)
	}
	if err := ValidatePreset("workspace+git+net+rex"); err != nil {
		t.Fatalf("ValidatePreset(workspace+git+net+rex): %v", err)
	}

	def, err := ParsePresetAtRoot(DefaultPresetSpec, t.TempDir())
	if err != nil {
		t.Fatalf("ParsePresetAtRoot(default): %v", err)
	}
	for _, env := range []string{"REX_SESSION", "REX_BLOCK", "REX_RUNTIME_DIR"} {
		if slices.Contains(def.PassEnv, env) {
			t.Fatalf("default preset must not pass %s", env)
		}
	}
	if len(def.AllowUnixSockets) != 0 {
		t.Fatalf("default preset must not grant Unix sockets: %v", def.AllowUnixSockets)
	}
}

func TestRexPresetResolvesLiveSocketAndIgnoresRegularFile(t *testing.T) {
	// Keep the socket path short for macOS/Linux sun_path limits.
	dir, err := os.MkdirTemp("/tmp", "polly-rex-sock-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// A regular file named server.sock must NOT be granted as a Unix socket.
	fakeFile := filepath.Join(dir, "server.sock")
	if err := os.WriteFile(fakeFile, []byte("not a socket"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("REX_SERVER", "")
	t.Setenv("REX_RUNTIME_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("XDG_STATE_HOME", dir)

	cfg, err := ParsePreset("rex")
	if err != nil {
		t.Fatalf("ParsePreset(rex) with regular file: %v", err)
	}
	for _, wantEnv := range []string{"REX_SESSION", "REX_BLOCK", "REX_RUNTIME_DIR"} {
		if !slices.Contains(cfg.PassEnv, wantEnv) {
			t.Fatalf("PassEnv missing %q: %v", wantEnv, cfg.PassEnv)
		}
	}
	if slices.Contains(cfg.AllowUnixSockets, fakeFile) {
		t.Fatalf("regular file must not be granted in AllowUnixSockets: %v", cfg.AllowUnixSockets)
	}

	// Replace with an actual live Unix socket.
	if err := os.Remove(fakeFile); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ln, err := net.Listen("unix", fakeFile)
	if err != nil {
		t.Fatalf("net.Listen unix: %v", err)
	}
	defer ln.Close()

	realSock, err := filepath.EvalSymlinks(fakeFile)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	cfg, err = ParsePreset("rex")
	if err != nil {
		t.Fatalf("ParsePreset(rex) with live socket: %v", err)
	}
	if !slices.Contains(cfg.AllowUnixSockets, realSock) {
		t.Fatalf("AllowUnixSockets = %v, want %q", cfg.AllowUnixSockets, realSock)
	}

	// Also verify unix:// URI form in REX_SERVER (including percent-encoded spaces).
	spaceDir := filepath.Join(dir, "App Support")
	if err := os.MkdirAll(spaceDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	uriSock := filepath.Join(spaceDir, "s.sock")
	ln2, err := net.Listen("unix", uriSock)
	if err != nil {
		t.Fatalf("net.Listen uriSock: %v", err)
	}
	defer ln2.Close()
	realURISock, err := filepath.EvalSymlinks(uriSock)
	if err != nil {
		t.Fatalf("EvalSymlinks uriSock: %v", err)
	}

	t.Setenv("REX_SERVER", (&url.URL{Scheme: "unix", Path: uriSock}).String())
	cfgURI, err := ParsePreset("rex")
	if err != nil {
		t.Fatalf("ParsePreset(rex) with REX_SERVER unix://: %v", err)
	}
	if !slices.Contains(cfgURI.AllowUnixSockets, realURISock) {
		t.Fatalf("AllowUnixSockets = %v, want %q", cfgURI.AllowUnixSockets, realURISock)
	}

	// Child restriction must not allow escalating to rex if parent did not grant it.
	parent, err := ParsePreset("base")
	if err != nil {
		t.Fatalf("ParsePreset(base): %v", err)
	}
	restricted, err := RestrictPreset(parent, cfgURI)
	if err != nil {
		t.Fatalf("RestrictPreset: %v", err)
	}
	if len(restricted.AllowUnixSockets) != 0 || slices.Contains(restricted.PassEnv, "REX_SESSION") {
		t.Fatalf("child rex preset escalated past parent: sockets=%v passEnv=%v", restricted.AllowUnixSockets, restricted.PassEnv)
	}
}
