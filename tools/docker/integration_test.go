package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// The integration tests need a reachable daemon and a local image; they run
// only with POLLYTOOL_REQUIRE_DOCKER_TESTS=1 (developer machines, not the
// local CI workers, which have no socket). The image must be present; polly
// never pulls. POLLYTOOL_DOCKER_TEST_IMAGE overrides debian:bookworm-slim,
// POLLYTOOL_DOCKER_TEST_GIT_IMAGE overrides golang:1.27 for the Git test.

type dockerFixture struct {
	provider *Provider
	image    string
	helper   string
	info     Info
}

var builtHelper struct {
	path string
	err  error
	done bool
}

func requireDocker(t *testing.T, image string, configure func(*Options)) *dockerFixture {
	t.Helper()
	if os.Getenv("POLLYTOOL_REQUIRE_DOCKER_TESTS") != "1" {
		t.Skip("set POLLYTOOL_REQUIRE_DOCKER_TESTS=1 to run docker integration tests")
	}
	if runtime.GOOS == "windows" {
		t.Skip("bind mode needs a local Unix daemon")
	}
	if image == "" {
		image = os.Getenv("POLLYTOOL_DOCKER_TEST_IMAGE")
	}
	if image == "" {
		image = "debian:bookworm-slim"
	}
	probe, err := New(Options{Image: image})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := probe.Ping(ctx)
	if err != nil {
		t.Fatalf("daemon: %v", err)
	}
	if _, err := probe.ResolveImage(ctx); err != nil {
		t.Fatalf("image %s: %v (pull it first; polly never pulls)", image, err)
	}
	helper := buildHelper(t, info.Arch)
	opts := Options{Image: image, Helper: helper, Policy: sandbox.DefaultConfig(), HomeDir: filepath.Join(t.TempDir(), "docker"), GitIdent: GitIdentity{Name: "Polly Test", Email: "polly@example.invalid"}}
	if configure != nil {
		configure(&opts)
	}
	provider, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &dockerFixture{provider: provider, image: image, helper: helper, info: info}
}

// buildHelper compiles polly for the daemon's platform once per test run.
func buildHelper(t *testing.T, arch string) string {
	t.Helper()
	if builtHelper.done {
		if builtHelper.err != nil {
			t.Fatal(builtHelper.err)
		}
		return builtHelper.path
	}
	builtHelper.done = true
	dir, err := os.MkdirTemp("", "polly-docker-helper-")
	if err != nil {
		builtHelper.err = err
		t.Fatal(err)
	}
	path := filepath.Join(dir, "polly")
	cmd := exec.Command("go", "build", "-o", path, "../../cmd/polly")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		builtHelper.err = errors.New("build helper: " + string(out))
		t.Fatal(builtHelper.err)
	}
	builtHelper.path = path
	return path
}

func (f *dockerFixture) open(t *testing.T, o OpenOptions, scope tools.ToolScope) tools.ToolBinding {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binding, err := f.provider.OpenTools(o)(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { binding.Close() })
	return binding
}

func (f *dockerFixture) containers(t *testing.T, root string) int {
	t.Helper()
	canonical, _ := filepath.EvalSymlinks(root)
	list, err := f.provider.engine.containerList(context.Background(), []string{labelRoot + "=" + canonical})
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func dockerBash(t *testing.T, binding tools.ToolBinding, command string) (string, error) {
	t.Helper()
	tool, ok := binding.Registry.Get("bash")
	if !ok {
		t.Fatal("bash is not served")
	}
	execution, err := binding.Registry.ExecuteTool(context.Background(), tool, map[string]any{"command": command}, time.Minute)
	return execution.Output.Text, err
}

func TestDockerToolsRunInTheContainerOverTheHostWorktree(t *testing.T) {
	f := requireDocker(t, "", nil)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("host notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binding := f.open(t, OpenOptions{Tools: []tools.ToolLoaderInfo{{Name: "bash", Type: "native", Source: "builtin"}, {Name: "read_file", Type: "native", Source: "builtin"}, {Name: "write_file", Type: "native", Source: "builtin"}}}, tools.ToolScope{Root: root, Session: "integration"})
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	text, err := dockerBash(t, binding, "pwd; id -u; uname -s; cat notes.txt; echo HOME=$HOME; ls -d /run/polly/home; test -e /Users/"+os.Getenv("USER")+"/.ssh && echo HOST-HOME-VISIBLE || echo host-home-hidden")
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, text)
	}
	for _, want := range []string{canonicalRoot, "Linux", "host notes", "HOME=/run/polly/home", "host-home-hidden"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bash output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "HOST-HOME-VISIBLE") {
		t.Fatalf("host home visible inside the container:\n%s", text)
	}
	write, _ := binding.Registry.Get("write_file")
	if _, err := binding.Registry.ExecuteTool(context.Background(), write, map[string]any{"path": "made.txt", "content": "made in the container\n"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "made.txt")); err != nil || string(content) != "made in the container\n" {
		t.Fatalf("host did not see the container's write: %q %v", content, err)
	}
	read, _ := binding.Registry.Get("read_file")
	if execution, err := binding.Registry.ExecuteTool(context.Background(), read, map[string]any{"path": "made.txt"}, time.Minute); err != nil || !strings.Contains(execution.Output.Text, "made in the container") {
		t.Fatalf("read_file = %+v, %v", execution.Output, err)
	}
	if text, err := dockerBash(t, binding, "ls /sys/class/net"); err != nil || strings.Contains(text, "eth0") {
		t.Fatalf("network was not denied: %q %v", text, err)
	}
	if f.containers(t, root) != 1 {
		t.Fatal("container missing while bound")
	}
	if err := binding.Close(); err != nil {
		t.Fatal(err)
	}
	if f.containers(t, root) != 0 {
		t.Fatal("standalone close left the container")
	}
}

func TestDockerNetworkAllowedAndDNSDenied(t *testing.T) {
	policy := sandbox.DefaultConfig()
	policy.AllowNetwork, policy.DenyDNS = true, true
	f := requireDocker(t, "", func(o *Options) { o.Policy = policy })
	binding := f.open(t, OpenOptions{Tools: []tools.ToolLoaderInfo{{Name: "bash", Type: "native", Source: "builtin"}}}, tools.ToolScope{Root: t.TempDir()})
	text, err := dockerBash(t, binding, "ls /sys/class/net; echo resolv=$(wc -c < /etc/resolv.conf)")
	if err != nil || !strings.Contains(text, "eth0") || !strings.Contains(text, "resolv=0") {
		t.Fatalf("network allowed without DNS: %q %v", text, err)
	}
}

func TestDockerReconnectKeepsStateAndImageChangeDestroys(t *testing.T) {
	f := requireDocker(t, "", nil)
	root := t.TempDir()
	scope := tools.ToolScope{Root: root, Session: "integration-keep"}
	load := OpenOptions{Tools: []tools.ToolLoaderInfo{{Name: "bash", Type: "native", Source: "builtin"}}, KeepOnClose: true}
	first := f.open(t, load, scope)
	if _, err := dockerBash(t, first, "echo installed > $HOME/marker"); err != nil {
		t.Fatal(err)
	}
	first.Close()
	if f.containers(t, root) != 1 {
		t.Fatal("keep-on-close removed the container")
	}
	second := f.open(t, load, scope)
	if text, err := dockerBash(t, second, "cat $HOME/marker"); err != nil || !strings.Contains(text, "installed") {
		t.Fatalf("reconnect lost container state: %q %v", text, err)
	}
	second.Close()

	other := os.Getenv("POLLYTOOL_DOCKER_TEST_GIT_IMAGE")
	if other == "" {
		other = "golang:1.27"
	}
	changed := requireDocker(t, other, nil)
	third := changed.open(t, load, scope)
	if text, err := dockerBash(t, third, "test -e $HOME/marker && echo kept || echo fresh"); err != nil || !strings.Contains(text, "fresh") {
		t.Fatalf("image change did not recreate the container: %q %v", text, err)
	}
	third.Close()
	if err := changed.provider.Destroy(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if f.containers(t, root) != 0 {
		t.Fatal("destroy left a container")
	}
}

func TestDockerCancelAndReadOnlyScratch(t *testing.T) {
	f := requireDocker(t, "", nil)
	root, scratch := t.TempDir(), t.TempDir()
	binding := f.open(t, OpenOptions{Tools: []tools.ToolLoaderInfo{{Name: "bash", Type: "native", Source: "builtin"}}}, tools.ToolScope{Root: root, Grant: tools.ExecutionGrant{ReadOnly: true, Scratch: scratch}})
	tool, _ := binding.Registry.Get("bash")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := binding.Registry.ExecuteTool(ctx, tool, map[string]any{"command": "sleep 60"}, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("cancel took %v", time.Since(start))
	}
	if text, err := dockerBash(t, binding, "pgrep -c sleep || echo none"); err != nil || !strings.Contains(text, "none") && !strings.Contains(text, "0") {
		t.Fatalf("sleep survived cancellation: %q %v", text, err)
	}
	canonicalScratch, _ := filepath.EvalSymlinks(scratch)
	text, err := dockerBash(t, binding, "touch ./blocked 2>&1; echo TMPDIR=$TMPDIR; touch $TMPDIR/ok && echo scratch-ok")
	if err != nil || !strings.Contains(text, "Read-only file system") || !strings.Contains(text, "TMPDIR="+canonicalScratch) || !strings.Contains(text, "scratch-ok") {
		t.Fatalf("read-only scope: %q %v", text, err)
	}
	if _, err := os.Stat(filepath.Join(scratch, "ok")); err != nil {
		t.Fatalf("scratch write not visible on the host: %v", err)
	}
}

func TestDockerGitWorktreeInsideTheContainer(t *testing.T) {
	image := os.Getenv("POLLYTOOL_DOCKER_TEST_GIT_IMAGE")
	if image == "" {
		image = "golang:1.27"
	}
	root := gitRepo(t)
	policy, err := sandbox.PrepareConfig(sandbox.Config{WritablePaths: []string{root}, DenyWritePaths: []string{filepath.Join(root, ".git", "config"), filepath.Join(root, ".git", "hooks")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := requireDocker(t, image, func(o *Options) { o.Policy = policy })
	binding := f.open(t, OpenOptions{Tools: []tools.ToolLoaderInfo{{Name: "bash", Type: "native", Source: "builtin"}}}, tools.ToolScope{Root: root, Session: "integration-git"})
	text, err := dockerBash(t, binding, "git status --short; git config user.name; echo change > tracked.txt; git add tracked.txt; git commit -qm 'from the container' && git log --oneline -1; touch .git/hooks/pre-commit 2>&1 || echo hooks-pinned")
	if err != nil {
		t.Fatalf("git inside the container: %v\n%s", err, text)
	}
	for _, want := range []string{"Polly Test", "from the container", "hooks-pinned"} {
		if !strings.Contains(text, want) {
			t.Fatalf("git output lacks %q:\n%s", want, text)
		}
	}
	log := exec.Command("git", "log", "--oneline", "-1")
	log.Dir = root
	if out, err := log.CombinedOutput(); err != nil || !strings.Contains(string(out), "from the container") {
		t.Fatalf("host repository lacks the container's commit: %s %v", out, err)
	}
}
