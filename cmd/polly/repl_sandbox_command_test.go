package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
)

// sandboxCommandREPL is a managed REPL on a session in the profile test
// workspace, with bash loaded under a no-op sandbox and the workspace's
// profile opened the way a start opens it.
func sandboxCommandREPL(t *testing.T, config *Config) *managedREPL {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "ctx")
	r := newManagedREPL(config, "ctx", 0, 0)
	r.state = &conversationState{session: session, toolRegistry: addDirRegistry(t), sandboxProfile: openSandboxProfile(config)}
	if _, err := r.state.toolRegistry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	return r
}

// commandOutput runs a command line and returns what it printed.
func commandOutput(r *managedREPL, line string) string {
	clearTranscriptForTest(r.model)
	r.runCommand(line)
	return strings.Join(transcriptTexts(r.model), "\n")
}

func bashSandboxConfig(t *testing.T, r *managedREPL) tools.SandboxInfo {
	t.Helper()
	bash, ok := r.state.toolRegistry.Get("bash")
	if !ok {
		t.Fatal("bash is not loaded")
	}
	info := tools.SandboxDetails(bash)
	if info.Config == nil {
		t.Fatal("bash has no sandbox config")
	}
	return info
}

func TestSandboxCommandAllowsShowsAndForgets(t *testing.T) {
	home, _ := profileTestHome(t)
	protos := filepath.Join(home, "src", "protos")
	mkdirs(t, protos)
	writeFile(t, filepath.Join(home, ".zshrc"), "")
	t.Setenv("NPM_TOKEN", "secret")
	r := sandboxCommandREPL(t, &Config{})
	profile := r.state.sandboxProfile

	if got := commandOutput(r, "/sandbox"); !strings.Contains(got, "no sandbox profile for this workspace") {
		t.Fatalf("/sandbox on an empty profile = %q", got)
	}

	if got := commandOutput(r, "/sandbox allow read ~/src/protos"); !strings.Contains(got, "allowed read ~/src/protos; it applies now") {
		t.Fatalf("allow read = %q", got)
	}
	if reads := bashSandboxConfig(t, r).Config.ReadPaths; !slices.Contains(reads, protos) {
		t.Fatalf("bash reads %v after allow read, want %s", reads, protos)
	}
	if cfg, _, err := r.state.toolRegistry.SandboxReadPolicy(); err != nil || !slices.Contains(cfg.ReadPaths, protos) {
		t.Fatalf("file tools' policy = %v, %v; want %s", cfg.ReadPaths, err, protos)
	}

	got := commandOutput(r, "/sandbox allow passenv NPM_TOKEN --members")
	if !strings.Contains(got, "a credential: sandboxed commands and swarm members see it while the workspace's origin stays git@example.com:acme/api.git") || strings.Contains(got, "not set") {
		t.Fatalf("allow passenv = %q", got)
	}
	if got := commandOutput(r, "/sandbox allow env GOCACHE=@cache/go-build"); !strings.Contains(got, "allowed env GOCACHE=@cache/go-build") {
		t.Fatalf("allow env = %q", got)
	}
	if got := commandOutput(r, "/sandbox allow env CARGO_TARGET_DIR=target"); !strings.Contains(got, "allowed env CARGO_TARGET_DIR=@workspace/target") {
		t.Fatalf("allow env with a workspace path = %q, want it stored from @workspace", got)
	}
	cfg := bashSandboxConfig(t, r).Config
	if !slices.Contains(cfg.PassEnv, "NPM_TOKEN") || cfg.Env["GOCACHE"] != filepath.Join(profile.ws.cache, "go-build") || cfg.Env["CARGO_TARGET_DIR"] != filepath.Join(profile.ws.dir, "target") {
		t.Fatalf("bash sandbox = passEnv %v, env %v; want the profile's", cfg.PassEnv, cfg.Env)
	}

	// A refused item says why and changes nothing.
	if got := commandOutput(r, "/sandbox allow write ~/.zshrc"); !strings.Contains(got, "write ~/.zshrc refused: it reaches ~/.zshrc, where the host runs code from") {
		t.Fatalf("allow write ~/.zshrc = %q", got)
	}
	if got := commandOutput(r, "/sandbox allow read ~/no-such-dir"); !strings.Contains(got, "sandbox profile: ~/no-such-dir does not exist") {
		t.Fatalf("allow read of a missing path = %q", got)
	}

	show := commandOutput(r, "/sandbox show")
	for _, want := range []string{
		"sandbox profile · ~/.pollytool/workspaces/" + profile.ws.key + "/sandbox.json",
		"1. read ~/src/protos",
		"2. passenv NPM_TOKEN --members · credential",
		"3. env GOCACHE=@cache/go-build",
		"4. env CARGO_TARGET_DIR=@workspace/target",
		"@cache is " + homeRelativePath(profile.ws.cache),
	} {
		if !strings.Contains(show, want) {
			t.Fatalf("/sandbox show = %q, missing %q", show, want)
		}
	}
	saved, err := readSandboxProfile(profile.ws.profile)
	if err != nil || len(saved.Items) != 4 || saved.Items[1].Origin != "git@example.com:acme/api.git" || saved.Workspace != profile.ws.commonDir {
		t.Fatalf("saved profile = %+v, %v", saved, err)
	}

	if got := commandOutput(r, "/sandbox forget 1"); !strings.Contains(got, "forgot read ~/src/protos") {
		t.Fatalf("forget 1 = %q", got)
	}
	if reads := bashSandboxConfig(t, r).Config.ReadPaths; slices.Contains(reads, protos) {
		t.Fatalf("bash still reads %s after forget", protos)
	}
	if got := commandOutput(r, "/sandbox forget NPM_TOKEN"); !strings.Contains(got, "forgot passenv NPM_TOKEN --members") {
		t.Fatalf("forget NPM_TOKEN = %q", got)
	}
	if got := commandOutput(r, "/sandbox forget 9"); !strings.Contains(got, "no item 9") {
		t.Fatalf("forget 9 = %q", got)
	}
	if got := commandOutput(r, "/sandbox forget all"); !strings.Contains(got, "forgot all 2 items") {
		t.Fatalf("forget all = %q", got)
	}
	if _, err := os.Stat(profile.ws.profile); !os.IsNotExist(err) {
		t.Fatalf("forget all left the profile file: %v", err)
	}
	if cfg := bashSandboxConfig(t, r).Config; len(cfg.Env) != 0 || len(cfg.PassEnv) != 0 {
		t.Fatalf("bash sandbox after forget all = env %v, passEnv %v; want the profile gone", cfg.Env, cfg.PassEnv)
	}
}

func TestSandboxCommandWithoutAProfileThisLaunch(t *testing.T) {
	home, _ := profileTestHome(t)
	mkdirs(t, filepath.Join(home, "src", "protos"))

	// --nosandboxprofile saves a change for later launches only.
	r := sandboxCommandREPL(t, &Config{NoSandboxProfile: true})
	if got := commandOutput(r, "/sandbox allow read ~/src/protos"); !strings.Contains(got, "saved for later launches (this one runs with --nosandboxprofile)") {
		t.Fatalf("allow under --nosandboxprofile = %q", got)
	}
	if reads := bashSandboxConfig(t, r).Config.ReadPaths; slices.Contains(reads, filepath.Join(home, "src", "protos")) {
		t.Fatalf("bash reads %v; want the profile left out of this launch", reads)
	}
	if got := commandOutput(r, "/sandbox"); !strings.Contains(got, "off this launch (--nosandboxprofile)") || !strings.Contains(got, "1. read ~/src/protos") {
		t.Fatalf("/sandbox under --nosandboxprofile = %q", got)
	}

	// Under --nosandbox there is no profile to change.
	r.state.sandboxProfile = nil
	if got := commandOutput(r, "/sandbox allow read ~/src/protos"); !strings.Contains(got, "the sandbox is off") {
		t.Fatalf("allow under --nosandbox = %q", got)
	}
}

func TestSandboxCommandBusySafetyAndCompletion(t *testing.T) {
	for line, want := range map[string]bool{"/sandbox": true, "/sandbox show": true, "/sandbox allow read /x": false, "/sandbox forget 1": false} {
		if got := defaultReplCommands.busySafeCommand(line); got != want {
			t.Errorf("busySafeCommand(%q) = %v, want %v", line, got, want)
		}
	}
	ctx := &replCommandContext{}
	if got := completeSandboxCommand(ctx, []string{"/sandbox"}, ""); !slices.Equal(got, []string{"allow", "forget", "show"}) {
		t.Errorf("subcommands = %v", got)
	}
	if got := completeSandboxCommand(ctx, []string{"/sandbox", "allow", "p"}, "p"); !slices.Equal(got, []string{"passenv"}) {
		t.Errorf("kinds = %v", got)
	}
	if got := completeSandboxCommand(ctx, []string{"/sandbox", "forget"}, ""); !slices.Equal(got, []string{"all"}) {
		t.Errorf("forget without a session = %v", got)
	}
}
