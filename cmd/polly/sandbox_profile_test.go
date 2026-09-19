package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// profileTestHome makes a home directory with a Git workspace inside it,
// moves into the workspace, and returns both, canonical. The workspace's
// origin is git@example.com:acme/api.git.
func profileTestHome(t *testing.T) (home, ws string) {
	t.Helper()
	skipIfWindows(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("ZDOTDIR", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("PATH", filepath.Join(home, ".cargo", "bin")+string(os.PathListSeparator)+"/usr/bin")
	ws = filepath.Join(home, "src", "api")
	mkdirs(t, filepath.Join(ws, ".git"), filepath.Join(home, ".cargo", "bin"))
	writeFile(t, filepath.Join(ws, ".git", "config"), "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@example.com:acme/api.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	t.Chdir(ws)
	return home, ws
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxWorkspaceIsSharedByEveryCheckoutOfARepository(t *testing.T) {
	home, ws := profileTestHome(t)
	main, err := resolveSandboxWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if main.origin != "git@example.com:acme/api.git" {
		t.Fatalf("origin = %q, want the origin remote's url", main.origin)
	}
	if want := filepath.Join(home, ".pollytool", "workspaces", main.key, "sandbox.json"); main.profile != want {
		t.Fatalf("profile = %q, want %q", main.profile, want)
	}
	if want := filepath.Join(home, "Library", "Caches", "pollytool", "ws", main.key); runtime.GOOS == "darwin" && main.cache != want {
		t.Fatalf("cache = %q, want %q", main.cache, want)
	}

	sub := filepath.Join(ws, "pkg", "inner")
	linked := filepath.Join(home, "src", "api-linked")
	gitDir := filepath.Join(ws, ".git", "worktrees", "api-linked")
	mkdirs(t, sub, linked, gitDir)
	writeFile(t, filepath.Join(linked, ".git"), "gitdir: "+gitDir+"\n")
	writeFile(t, filepath.Join(gitDir, "commondir"), "../..\n")
	for name, dir := range map[string]string{"subdirectory": sub, "linked worktree": linked} {
		got, err := resolveSandboxWorkspace(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.key != main.key || got.profile != main.profile || got.dir != dir {
			t.Fatalf("%s: workspace %+v, want the key of %+v with its own directory", name, got, main)
		}
	}

	other := filepath.Join(home, "notes")
	mkdirs(t, other)
	got, err := resolveSandboxWorkspace(other)
	if err != nil {
		t.Fatal(err)
	}
	if got.key == main.key || got.commonDir != "" || got.origin != "" {
		t.Fatalf("directory outside Git = %+v, want its own key and no repository", got)
	}
}

func TestReadSandboxProfileRefusesUnsafeFiles(t *testing.T) {
	home, _ := profileTestHome(t)
	dir := filepath.Join(home, ".pollytool", "workspaces", "k")
	path := filepath.Join(dir, "sandbox.json")
	if profile, err := readSandboxProfile(path); err != nil || len(profile.Items) != 0 {
		t.Fatalf("missing profile = %+v, %v; want an empty one", profile, err)
	}
	valid := `{"version":1,"workspace":"/w","items":[{"kind":"read","path":"~/src/protos"}]}`
	writeFile(t, path, valid)
	if profile, err := readSandboxProfile(path); err != nil || len(profile.Items) != 1 || profile.Items[0].Path != "~/src/protos" {
		t.Fatalf("valid profile = %+v, %v", profile, err)
	}

	for name, tc := range map[string]struct {
		content string
		mode    os.FileMode
		want    string
	}{
		"writable by others": {valid, 0o666, "writable by other users"},
		"unknown field":      {`{"version":1,"items":[],"extra":true}`, 0o600, "unknown field"},
		"later version":      {`{"version":2,"items":[]}`, 0o600, "version 2"},
		"trailing data":      {valid + `{}`, 0o600, "data after"},
		"too large":          {strings.Repeat(" ", sandboxProfileMaxSize+1), 0o600, "larger than"},
	} {
		writeFile(t, path, tc.content)
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := readSandboxProfile(path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
	}

	writeFile(t, filepath.Join(home, "elsewhere.json"), valid)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere.json"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := readSandboxProfile(path); err == nil {
		t.Error("a symlinked profile was read")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := readSandboxProfile(path); err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Errorf("profile directory writable by others: error = %v", err)
	}
}

func TestWriteSandboxProfileRoundTrips(t *testing.T) {
	home, _ := profileTestHome(t)
	path := filepath.Join(home, ".pollytool", "workspaces", "k", "sandbox.json")
	profile := sandboxProfile{Workspace: "/w", Items: []sandboxProfileItem{
		{Kind: profileRead, Path: "~/src/protos"},
		{Kind: profilePassEnv, Name: "NPM_TOKEN", Members: true, Credential: true, Origin: "git@example.com:acme/api.git"},
	}}
	if err := writeSandboxProfile(path, profile); err != nil {
		t.Fatal(err)
	}
	got, err := readSandboxProfile(path)
	if err != nil || got.Version != sandboxProfileVersion || !slices.Equal(got.Items, profile.Items) {
		t.Fatalf("read back %+v, %v; want %+v", got, err, profile)
	}
	for file, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode = %v, %v; want %v", file, info.Mode().Perm(), err, want)
		}
	}
	if err := writeSandboxProfile(path, sandboxProfile{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an empty profile left the file behind: %v", err)
	}
}

func TestProfileJudgeRules(t *testing.T) {
	home, ws := profileTestHome(t)
	workspace, err := resolveSandboxWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "src", "other")
	mkdirs(t, filepath.Join(other, ".git"), filepath.Join(other, "build"), filepath.Join(home, "src", "protos"),
		filepath.Join(home, ".foo", "cache"), filepath.Join(home, ".cargo", "registry"), filepath.Join(home, ".pollytool"))
	writeFile(t, filepath.Join(home, ".npmrc"), "")
	judge := newProfileJudge(workspace)
	cache, err := pollyCacheDir()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		item       sandboxProfileItem
		problem    string
		credential bool
	}{
		{item: sandboxProfileItem{Kind: profileRead, Path: "~/src/protos"}},
		{item: sandboxProfileItem{Kind: profileRead, Path: "~/.npmrc"}, credential: true},
		{item: sandboxProfileItem{Kind: profileRead, Path: "~"}, problem: "home directory"},
		{item: sandboxProfileItem{Kind: profileRead, Path: "/"}, problem: "filesystem root"},
		{item: sandboxProfileItem{Kind: profileRead, Path: "src/protos"}, problem: "not an absolute path"},
		{item: sandboxProfileItem{Kind: profileRead, Path: filepath.Join(ws, "pkg")}, problem: "already readable"},
		{item: sandboxProfileItem{Kind: profileRead, Path: "~/.pollytool"}, problem: "polly's own state"},
		{item: sandboxProfileItem{Kind: profileRead, Path: filepath.Join(cache, "attachments")}, problem: "polly's own state"},
		{item: sandboxProfileItem{Kind: profileRead, Path: filepath.Join(workspace.cache, "go")}},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/.foo/cache"}},
		{item: sandboxProfileItem{Kind: profileWrite, Path: filepath.Join(ws, "out")}, problem: "--sandbox preset"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/.cargo/registry"}, problem: "host runs code from"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/.zshrc"}, problem: "host runs code from"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/Library/LaunchAgents"}, problem: "host runs code from"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/.ssh"}, problem: "credential path"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/.npmrc"}, problem: "credential path"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/src"}, problem: "workspace's Git metadata"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: filepath.Join(other, "build")}, problem: "inside the Git repository"},
		{item: sandboxProfileItem{Kind: profileWrite, Path: "~/.pollytool"}, problem: "polly's own state"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "GOCACHE", Value: "@cache/go-build"}},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "CARGO_TARGET_DIR", Value: "@workspace/target"}},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "OUT", Value: "@workspace"}},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "OUT", Value: "@workspace/../x"}, problem: "leaves @workspace"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "OUT", Value: "/tmp/x"}, problem: "must start with"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "OUT", Value: "@cache/a\nb"}, problem: "one line"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "PATH", Value: "@cache"}, problem: "shell's own environment"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "TMPDIR", Value: "@cache"}, problem: "temp directory"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "LD_PRELOAD", Value: "@cache/x.so"}, problem: "loads code"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "GIT_DIR", Value: "@workspace"}, problem: "Git, SSH or GPG"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "NPM_TOKEN", Value: "@cache"}, problem: "credential-shaped"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "POLLYTOOL_MODEL", Value: "@cache"}, problem: "polly's own configuration"},
		{item: sandboxProfileItem{Kind: profileEnv, Name: "2BAD", Value: "@cache"}, problem: "not a variable name"},
		{item: sandboxProfileItem{Kind: profilePassEnv, Name: "NPM_TOKEN"}, credential: true},
		{item: sandboxProfileItem{Kind: profilePassEnv, Name: "SSH_AUTH_SOCK"}, problem: "socket", credential: true},
		{item: sandboxProfileItem{Kind: profilePassEnv, Name: "POLLYTOOL_OPENAIKEY"}, problem: "polly's own configuration", credential: true},
		{item: sandboxProfileItem{Kind: profilePassEnv, Name: "GOPATH"}, problem: "does not strip it", credential: true},
		{item: sandboxProfileItem{Kind: "socket", Path: "/tmp/s"}, problem: "unknown item kind"},
	} {
		credential, err := judge.check(tc.item)
		switch {
		case tc.problem == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.item, err)
		case tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)):
			t.Errorf("%s: error = %v, want %q", tc.item, err, tc.problem)
		case credential != tc.credential:
			t.Errorf("%s: credential = %v, want %v", tc.item, credential, tc.credential)
		}
	}

	// A new write grant is also refused when it holds another repository;
	// the look runs only when the item is added.
	holder := filepath.Join(home, "work")
	mkdirs(t, filepath.Join(holder, "team", "repo", ".git"))
	if err := judge.checkWrite(holder); err != nil {
		t.Fatalf("checkWrite(%s) = %v, want the load-time rules to pass it", holder, err)
	}
	if err := judge.checkNewWrite(holder); err == nil || !strings.Contains(err.Error(), "holds the Git repository") {
		t.Fatalf("checkNewWrite(%s) = %v, want the repository inside refused", holder, err)
	}
}

func TestJudgeSandboxProfileBuildsTheLayer(t *testing.T) {
	home, ws := profileTestHome(t)
	workspace, err := resolveSandboxWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	protos, cacheDir, hidden := filepath.Join(home, "src", "protos"), filepath.Join(home, ".foo", "cache"), filepath.Join(home, "hidden")
	mkdirs(t, protos, cacheDir, hidden)
	writeFile(t, filepath.Join(home, ".npmrc"), "")
	origin := workspace.origin
	profile := sandboxProfile{Items: []sandboxProfileItem{
		{Kind: profileRead, Path: "~/src/protos"},
		{Kind: profileWrite, Path: "~/.foo/cache"},
		{Kind: profileEnv, Name: "GOCACHE", Value: "@cache/go-build"},
		{Kind: profileEnv, Name: "CARGO_TARGET_DIR", Value: "@workspace/target"},
		{Kind: profilePassEnv, Name: "NPM_TOKEN", Credential: true, Origin: origin},
		{Kind: profilePassEnv, Name: "GH_TOKEN", Members: true, Credential: true, Origin: origin},
		{Kind: profileRead, Path: "~/.npmrc", Credential: true, Origin: "git@example.com:other/api.git"},
		{Kind: profileRead, Path: "~/hidden"},
		{Kind: profileEnv, Name: "PATH", Value: "@cache"},
		{Kind: profileRead, Path: "~/.npmrc", Origin: origin},
		{Kind: profilePassEnv, Name: "AWS_SECRET_ACCESS_KEY", Origin: origin},
		{Kind: profileRead, Path: "~/src/removed"},
	}}
	states, layer := judgeSandboxProfile(workspace, profile.Items, sandbox.Config{DenyPaths: []string{hidden}})
	for i, want := range []string{"", "", "", "", "", "", "origin", "denied path", "shell's own environment", "not allowed as one", "not allowed as one", "does not exist"} {
		if got := states[i].problem; want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("item %d (%s) problem = %q, want %q", i+1, profile.Items[i], got, want)
		}
	}
	if !states[4].credential || !states[6].credential || states[0].credential {
		t.Errorf("credential flags = %+v, want the passenv and ~/.npmrc items", states)
	}

	cfg, members := layer.Config, layer.Members
	if !slices.Equal(cfg.ReadPaths, []string{protos}) || !slices.Equal(members.ReadPaths, []string{protos}) {
		t.Errorf("reads = %v, members %v; want only the applied read", cfg.ReadPaths, members.ReadPaths)
	}
	if want := []string{cacheDir, workspace.cache}; !slices.Equal(cfg.WritablePaths, want) || !slices.Equal(members.WritablePaths, want) {
		t.Errorf("writes = %v, members %v; want %v", cfg.WritablePaths, members.WritablePaths, want)
	}
	if info, err := os.Stat(workspace.cache); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("workspace cache directory = %v, %v; want it created 0700", info, err)
	}
	wantEnv := map[string]string{"GOCACHE": filepath.Join(workspace.cache, "go-build"), "CARGO_TARGET_DIR": filepath.Join(ws, "target")}
	for name, value := range wantEnv {
		if cfg.Env[name] != value || members.Env[name] != value {
			t.Errorf("env %s = %q, members %q; want %q", name, cfg.Env[name], members.Env[name], value)
		}
	}
	if !slices.Equal(cfg.PassEnv, []string{"NPM_TOKEN", "GH_TOKEN"}) || !slices.Equal(members.PassEnv, []string{"GH_TOKEN"}) {
		t.Errorf("passEnv = %v, members %v; want both, and only the one marked for members", cfg.PassEnv, members.PassEnv)
	}

	// A base that denies every write leaves the write and the cache redirect
	// out, and keeps the rest.
	states, layer = judgeSandboxProfile(workspace, profile.Items, sandbox.Config{DenyWrite: true})
	if states[1].problem != profileWritesDenied || states[2].problem != profileWritesDenied || states[3].problem != "" {
		t.Errorf("under denyWrite: %+v, want the write and the @cache env out", states[:4])
	}
	if len(layer.Config.WritablePaths) != 0 {
		t.Errorf("under denyWrite: writes %v, want none", layer.Config.WritablePaths)
	}
}

func TestSandboxProfileAppliesAtStart(t *testing.T) {
	home, ws := profileTestHome(t)
	originalNewSandbox := newSandbox
	newSandbox = func(sandbox.Config) (sandbox.Sandbox, error) { return passthroughSandbox{}, nil }
	t.Cleanup(func() { newSandbox = originalNewSandbox })
	protos := filepath.Join(home, "src", "protos")
	mkdirs(t, protos)
	workspace, err := resolveSandboxWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSandboxProfile(workspace.profile, sandboxProfile{Items: []sandboxProfileItem{
		{Kind: profileRead, Path: "~/src/protos"},
		{Kind: profileEnv, Name: "PATH", Value: "@cache"},
		// Redundant in this checkout, maybe not in another worktree: listed,
		// not applied, and no notice.
		{Kind: profileRead, Path: filepath.Join(ws, "pkg")},
	}}); err != nil {
		t.Fatal(err)
	}
	open := func(config *Config) (*tools.ToolRegistry, *sandboxProfileState, []string) {
		t.Helper()
		warnings := newBroadWritablePathWarner()
		opts, probe, profile, err := sandboxRegistryOptionsWithWarnings(config, warnings, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := probe.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		registry := tools.NewToolRegistry(nil, opts...)
		t.Cleanup(func() { _ = registry.Close() })
		return registry, profile, warnings.Drain()
	}
	reads := func(registry *tools.ToolRegistry) []string {
		cfg, _, err := registry.SandboxReadPolicy()
		if err != nil {
			t.Fatal(err)
		}
		return cfg.ReadPaths
	}

	config := &Config{SandboxPreset: "base"}
	registry, profile, notices := open(config)
	if !slices.Contains(reads(registry), protos) {
		t.Fatalf("reads = %v, want the profile's %s", reads(registry), protos)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "sandbox profile item 2 (env PATH=@cache) not applied") {
		t.Fatalf("notices = %q, want the refused item named", notices)
	}
	posture := currentSandboxPosture(config, &conversationState{toolRegistry: registry, sandboxProfile: profile})
	if posture.profile != "profile: 1 item (2 not applied)" || !strings.Contains(posture.summaryLine(false), posture.profile) {
		t.Fatalf("posture profile = %q, line %q", posture.profile, posture.summaryLine(false))
	}

	registry, profile, notices = open(&Config{SandboxPreset: "base", NoSandboxProfile: true})
	if slices.Contains(reads(registry), protos) || len(notices) != 0 || profile.summary() != "profile off" {
		t.Fatalf("--nosandboxprofile: reads %v, notices %q, summary %q; want the profile left out quietly", reads(registry), notices, profile.summary())
	}
	if _, _, profile, err := sandboxRegistryOptionsWithWarnings(&Config{NoSandbox: true}, nil, nil, nil); err != nil || profile != nil {
		t.Fatalf("--nosandbox: profile %+v, %v; want none", profile, err)
	}
}

// Under the real sandbox, a profile applied at start lets bash write the
// workspace's cache directory, which an env item points into, and read a
// directory it grants, both inside the private home; without the profile it
// can do neither.
func TestSandboxProfileUnderTheRealSandbox(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process sandbox")
	}
	home, ws := profileTestHome(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	writeFile(t, filepath.Join(home, "src", "protos", "fixture.txt"), "proto-value\n")
	workspace, err := resolveSandboxWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSandboxProfile(workspace.profile, sandboxProfile{Items: []sandboxProfileItem{
		{Kind: profileRead, Path: "~/src/protos"},
		{Kind: profileEnv, Name: "TOOL_CACHE", Value: "@cache/tool"},
	}}); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(workspace.cache, "tool", "f")
	command := "mkdir -p '" + filepath.Dir(cacheFile) + "' && echo cached > '" + cacheFile + "' && cat '" + cacheFile + "' ~/src/protos/fixture.txt && echo \"$TOOL_CACHE\""
	run := func(config *Config) (string, error) {
		t.Helper()
		opts, probe, _, err := sandboxRegistryOptionsWithWarnings(config, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := probe.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		registry := tools.NewToolRegistry(nil, opts...)
		t.Cleanup(func() { _ = registry.Close() })
		bash, err := registry.LoadToolAuto("bash")
		if err != nil {
			t.Fatal(err)
		}
		tool, _ := registry.Get(bash.Servers[0].ToolNames[0])
		return tool.Execute(context.Background(), map[string]any{"command": command})
	}

	out, err := run(&Config{SandboxPreset: "base"})
	if err != nil || !strings.Contains(out, "cached") || !strings.Contains(out, "proto-value") || !strings.Contains(out, filepath.Join(workspace.cache, "tool")) {
		t.Fatalf("bash under the profile: %q %v", out, err)
	}
	if err := os.RemoveAll(workspace.cache); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := run(&Config{SandboxPreset: "base", NoSandboxProfile: true}); err == nil && strings.Contains(out, "cached") {
		t.Fatalf("bash wrote the workspace cache without the profile: %q", out)
	}
}
