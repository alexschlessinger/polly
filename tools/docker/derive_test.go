package docker

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	canonical, _ := filepath.EvalSymlinks(root)
	return canonical
}

func mountByTarget(mounts []mount, target string) (mount, bool) {
	for _, m := range mounts {
		if m.Target == target {
			return m, true
		}
	}
	return mount{}, false
}

func TestDeriveMountsPinsGitMetadataAndRoutesLinkedWorktrees(t *testing.T) {
	root := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.Config{DenyWritePaths: []string{filepath.Join(root, ".git", "config"), filepath.Join(root, ".git", "hooks")}}
	mounts, err := deriveMounts(tools.ToolScope{Root: root}, policy, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := mountByTarget(mounts, root); !ok || m.ReadOnly {
		t.Fatalf("root mount %+v", mounts)
	}
	for _, pin := range policy.DenyWritePaths {
		if m, ok := mountByTarget(mounts, pin); !ok || !m.ReadOnly || m.Source != pin {
			t.Fatalf("pin %s missing or writable: %+v", pin, mounts)
		}
	}
	if len(mounts) != 3 {
		t.Fatalf("unexpected mounts %+v", mounts)
	}

	worktreeDir := filepath.Join(t.TempDir(), "slot", "tree")
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "worktree", "add", "-q", "--detach", worktreeDir, "HEAD")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %s %v", out, err)
	}
	worktree, _ := filepath.EvalSymlinks(worktreeDir)
	routing, err := sandbox.DiscoverGitRouting(worktree)
	if err != nil || !routing.Linked() {
		t.Fatalf("routing %+v %v", routing, err)
	}
	scratch := t.TempDir()
	scope := tools.ToolScope{Root: worktree, Grant: tools.ExecutionGrant{Scratch: scratch, DeniedWrites: []string{routing.CommonDir, worktree + "/.git"}, DeniedReads: []string{filepath.Dir(filepath.Dir(worktree)), root}}}
	mounts, err = deriveMounts(scope, sandbox.Config{}, []string{t.TempDir()}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := mountByTarget(mounts, routing.CommonDir); !ok || !m.ReadOnly {
		t.Fatalf("common dir mount %+v", mounts)
	}
	if m, ok := mountByTarget(mounts, routing.GitDir); !ok || m.ReadOnly {
		t.Fatalf("worktree git dir mount %+v", mounts)
	}
	if m, ok := mountByTarget(mounts, worktree+"/.git"); !ok || !m.ReadOnly {
		t.Fatalf("worktree .git pointer not pinned: %+v", mounts)
	}
	canonicalScratch, _ := filepath.EvalSymlinks(scratch)
	if m, ok := mountByTarget(mounts, canonicalScratch); !ok || m.ReadOnly {
		t.Fatalf("scratch mount %+v", mounts)
	}
	skills := 0
	for _, m := range mounts {
		if m.ReadOnly && m.Source != routing.CommonDir && m.Source != worktree+"/.git" {
			skills++
		}
	}
	if skills != 1 {
		t.Fatalf("skill root mount %+v", mounts)
	}
	// The runtime directory and the source root are denied reads that contain
	// or sit beside the granted paths; both are satisfied structurally. A
	// denial inside a mounted tree is not.
	scope.Grant.DeniedReads = append(scope.Grant.DeniedReads, filepath.Join(worktree, "private"))
	if _, err := deriveMounts(scope, sandbox.Config{}, nil, "", ""); err == nil {
		t.Fatal("a denied read inside the worktree was accepted")
	}
	// A skill root inside a denied read that grants nothing back is refused.
	denied := t.TempDir()
	if err := os.MkdirAll(filepath.Join(denied, "skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := deriveMounts(tools.ToolScope{Root: worktree, Grant: tools.ExecutionGrant{DeniedReads: []string{denied}}}, sandbox.Config{}, []string{filepath.Join(denied, "skill")}, "", ""); err == nil {
		t.Fatal("a skill root inside a denied read was mounted")
	}
	// A missing protected path fails closed.
	if _, err := deriveMounts(tools.ToolScope{Root: root, Grant: tools.ExecutionGrant{DeniedWrites: []string{filepath.Join(root, ".git", "absent")}}}, sandbox.Config{}, nil, "", ""); err == nil {
		t.Fatal("a missing pin was ignored")
	}
}

func TestReadOnlyScopeMountsRootReadOnly(t *testing.T) {
	root := t.TempDir()
	mounts, err := deriveMounts(tools.ToolScope{Root: root, Grant: tools.ExecutionGrant{ReadOnly: true}}, sandbox.Config{}, nil, "", "")
	if err != nil || len(mounts) != 1 || !mounts[0].ReadOnly {
		t.Fatalf("mounts %+v %v", mounts, err)
	}
}

func TestLabelsAndNames(t *testing.T) {
	spec := containerSpec{session: "s", root: "/work", mode: ModeBind, imageID: "sha256:a", protocol: 1, pids: 4096, user: "501:20", mounts: []mount{{Type: "bind", Source: "/work", Target: "/work"}}}
	labels := computeLabels(map[string]string{"team": "qa", labelRoot: "spoofed"}, spec)
	if labels["team"] != "qa" || labels[labelRoot] != "/work" || labels[labelNetwork] != "none" {
		t.Fatalf("labels %v", labels)
	}
	if !labelsMatch(labels, labels) {
		t.Fatal("labels do not match themselves")
	}
	other := computeLabels(nil, spec)
	other["team"] = "ops"
	if !labelsMatch(other, labels) {
		t.Fatal("a caller label difference counted as a mismatch")
	}
	spec.mounts[0].ReadOnly = true
	if labelsMatch(computeLabels(nil, spec), labels) {
		t.Fatal("a mount change did not count as a mismatch")
	}
	sum := sha256.Sum256([]byte("s|/work"))
	if containerName("s", "/work") != "polly-"+hex.EncodeToString(sum[:])[:12] {
		t.Fatalf("name %s", containerName("s", "/work"))
	}
}

func TestResolveEndpoint(t *testing.T) {
	config := t.TempDir()
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	ep, err := resolveEndpoint("")
	if err != nil || ep.network != "unix" || ep.address != "/var/run/docker.sock" {
		t.Fatalf("default endpoint %+v %v", ep, err)
	}
	ep, err = resolveEndpoint("tcp://10.0.0.5:2375")
	if err != nil || ep.network != "tcp" || ep.address != "10.0.0.5:2375" || ep.tls != nil {
		t.Fatalf("tcp override %+v %v", ep, err)
	}
	t.Setenv("DOCKER_HOST", "unix:///tmp/other.sock")
	if ep, err = resolveEndpoint(""); err != nil || ep.address != "/tmp/other.sock" {
		t.Fatalf("DOCKER_HOST %+v %v", ep, err)
	}
	t.Setenv("DOCKER_HOST", "ssh://user@remote")
	if _, err = resolveEndpoint(""); !errors.Is(err, ErrUnsupportedHost) {
		t.Fatalf("ssh = %v", err)
	}
	t.Setenv("DOCKER_HOST", "")
	sum := sha256.Sum256([]byte("orb"))
	meta := filepath.Join(config, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]any{"Name": "orb", "Endpoints": map[string]any{"docker": map[string]any{"Host": "unix:///Users/me/.orb/docker.sock"}}})
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte(`{"currentContext":"orb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ep, err = resolveEndpoint(""); err != nil || ep.address != "/Users/me/.orb/docker.sock" {
		t.Fatalf("context endpoint %+v %v", ep, err)
	}
	t.Setenv("DOCKER_CONTEXT", "default")
	if ep, err = resolveEndpoint(""); err != nil || ep.address != "/var/run/docker.sock" {
		t.Fatalf("default context %+v %v", ep, err)
	}
	t.Setenv("DOCKER_CONTEXT", "missing")
	if _, err = resolveEndpoint(""); err == nil {
		t.Fatal("unknown context accepted")
	}
}

func TestStdcopyDemux(t *testing.T) {
	var stream bytes.Buffer
	stdout := stdcopyWriter{writer: &stream, stream: streamStdout}
	stderr := stdcopyWriter{writer: &stream, stream: streamStderr}
	stdout.Write([]byte("hello "))
	stderr.Write([]byte("noise"))
	stdout.Write([]byte("world\n"))
	var errs bytes.Buffer
	reader := newStdcopyReader(bufio.NewReaderSize(&stream, 4), &errs)
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "hello world\n" || errs.String() != "noise" {
		t.Fatalf("demux %q %q %v", got, errs.String(), err)
	}
}

func TestLimitParsing(t *testing.T) {
	for value, want := range map[string]int64{"": 0, "512m": 512 << 20, "2g": 2 << 30, "1024k": 1 << 20, "4096": 4096, "10b": 10} {
		if got, err := parseMemory(value); err != nil || got != want {
			t.Fatalf("parseMemory(%q) = %d, %v", value, got, err)
		}
	}
	for _, bad := range []string{"-1", "x", "0"} {
		if _, err := parseMemory(bad); err == nil {
			t.Fatalf("parseMemory(%q) accepted", bad)
		}
	}
	if got, err := parseCPUs("1.5"); err != nil || got != 1500000000 {
		t.Fatalf("parseCPUs = %d, %v", got, err)
	}
	if _, err := parseCPUs("-2"); err == nil {
		t.Fatal("negative CPUs accepted")
	}
	opts := Options{Image: "x", Host: "unix:///tmp/x.sock"}
	if err := opts.validate(); err != nil || opts.PIDs != DefaultPIDs || opts.Mode != ModeBind {
		t.Fatalf("defaults %+v %v", opts, err)
	}
	if !strings.Contains((&endpoint{network: "unix", address: "/tmp/x.sock"}).String(), "x.sock") {
		t.Fatal("endpoint string")
	}
}
