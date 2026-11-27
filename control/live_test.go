//go:build darwin || linux

package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The live tests run against a real backend binary that speaks the control
// contract: POLLY_BACKEND_BIN=/abs/path/to/polly-zig go test ./control -run Live.
// A launch of the binary itself makes a daemon and a worker; the Go side
// must find both by keys it computes on its own, byte for byte.

func liveBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("POLLY_BACKEND_BIN")
	if bin == "" {
		t.Skip("POLLY_BACKEND_BIN is not set")
	}
	if !filepath.IsAbs(bin) {
		t.Fatalf("POLLY_BACKEND_BIN must be absolute: %s", bin)
	}
	return bin
}

// liveProvider is a chat-completions server whose every reply is "done".
func liveProvider(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"done\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

type liveRig struct {
	bin, root, home, runtime, work, baseURL string
	env                                     []string
}

func newLiveRig(t *testing.T) *liveRig {
	bin := liveBinary(t)
	srv := liveProvider(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rig := &liveRig{bin: bin, root: root, home: filepath.Join(root, "home"), runtime: filepath.Join(root, "runtime"), work: filepath.Join(root, "work"), baseURL: srv.URL + "/v1"}
	for _, dir := range []string{rig.home, rig.runtime, filepath.Join(rig.runtime, "skills", "mine"), filepath.Join(rig.work, ".agents", "skills", "alpha"), filepath.Join(rig.work, "sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The work directory is a repository's root, so skills are looked for
	// from there and not above the test's.
	must(t, os.WriteFile(filepath.Join(rig.work, ".git"), []byte("gitdir: /nowhere\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(rig.work, ".agents", "skills", "alpha", "SKILL.md"), []byte("---\nname: alpha\ndescription: a\n---\nalpha\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(rig.runtime, "skills", "mine", "SKILL.md"), []byte("---\nname: mine\ndescription: m\n---\nmine\n"), 0o644))
	rig.env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + rig.home,
		RuntimeDirVar + "=" + rig.runtime,
		"POLLYTOOL_IDLE=45s",
		"LANG=C",
		"SOME_TOKEN=per-prompt",
	}
	return rig
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// launch runs the binary itself, as a user would, in dir.
func (rig *liveRig) launch(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, rig.bin, append([]string{"-m", "openai/fixture", "--base-url", rig.baseURL}, args...)...)
	cmd.Dir = dir
	cmd.Env = rig.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", rig.bin, args, err, out)
	}
	return string(out)
}

func (rig *liveRig) prepare(t *testing.T, target Target, dir string) Bootstrap {
	t.Helper()
	b, err := Prepare(context.Background(), LaunchOptions{Target: target, Provider: "openai", BaseURL: rig.baseURL, Cwd: dir, Environ: rig.env})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLiveKeysFindTheBinarysOwnDaemonAndWorker(t *testing.T) {
	rig := newLiveRig(t)
	out := rig.launch(t, rig.work, "say done")
	if !strings.Contains(out, "done") {
		t.Fatalf("launch output = %q", out)
	}
	target, err := ParseTarget("exec://" + rig.bin)
	must(t, err)
	b := rig.prepare(t, target, rig.work)

	// The build key names the daemon's socket the binary made.
	dir, err := UserSocketDir()
	must(t, err)
	sock := SocketPath(dir, b.BuildKey)
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("no daemon socket at %s for build key %s: %v", sock, b.BuildKey, err)
	}

	// The setup key finds the worker the binary's own launch started: a
	// join that may start nothing is answered made.
	conn, err := dialUnix(context.Background(), sock)
	must(t, err)
	c := NewClient(conn)
	defer c.Close()
	made, err := c.Join(context.Background(), JoinReq{Setup: b.Start.Setup.Key, ExistingOnly: true})
	must(t, err)
	if !made.Made {
		t.Fatalf("setup key %s found no worker of the binary's launch: %+v", b.Start.Setup.Key, made)
	}
	w, err := c.Hello(context.Background(), b.Start.Setup.Key)
	must(t, err)
	if w.Setup != b.Start.Setup.Key || w.Protocol != Protocol {
		t.Fatalf("welcome = %+v", w)
	}

	// A launch from a subdirectory with no skills of its own shares the key.
	sub := rig.prepare(t, target, filepath.Join(rig.work, "sub"))
	if sub.Start.Setup.Key != b.Start.Setup.Key {
		t.Fatalf("subdirectory key %s, want %s", sub.Start.Setup.Key, b.Start.Setup.Key)
	}
	// And one that differs in a setup variable does not.
	other := *rig
	other.env = append(append([]string{}, rig.env...), "TZ=UTC")
	if k := other.prepare(t, target, rig.work).Start.Setup.Key; k == b.Start.Setup.Key {
		t.Fatal("a setup variable did not split the key")
	}
}

func TestLiveStoreReadsAndATurnThroughTheWorker(t *testing.T) {
	rig := newLiveRig(t)
	rig.launch(t, rig.work, "say done")
	target, err := ParseTarget("exec://" + rig.bin)
	must(t, err)
	b := rig.prepare(t, target, rig.work)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reached, err := Reach(ctx, b)
	must(t, err)
	c := reached.Client
	defer c.Close()

	list, err := c.Sessions(ctx, SessionsReq{Filter: "roots"})
	must(t, err)
	if len(list.Sessions) != 1 || list.Last != list.Sessions[0].ID {
		t.Fatalf("sessions = %+v", list)
	}
	root := list.Sessions[0]
	info, err := c.Info(ctx, InfoReq{Name: root.Name})
	must(t, err)
	if info.ID != root.ID || info.Lines != root.Lines || info.Lines < 2 {
		t.Fatalf("info = %+v, listed %+v", info, root)
	}
	lines, err := c.HistoryFrom(ctx, root.ID, 0, nil)
	must(t, err)
	if uint64(len(lines)) != info.Lines {
		t.Fatalf("history has %d lines, info %d", len(lines), info.Lines)
	}
	var first struct{ Role, Content string }
	must(t, json.Unmarshal(lines[0], &first))
	if first.Role != "user" || first.Content != "say done" {
		t.Fatalf("first line = %s", lines[0])
	}
	if _, err := c.Info(ctx, InfoReq{Name: "nobody-is-called-this"}); !IsRefusal(err, RefusedNotFound) {
		t.Fatalf("info of nobody = %v", err)
	}

	// A turn on the same session, watched from before it starts.
	sub := c.SubscribeAgents(true, root.ID)
	defer sub.Close()
	if _, err := c.Watch(ctx, Watch{Agent: root.ID, Kinds: AllKinds, Children: true}); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(map[string]string{"role": "user", "content": "again"})
	reply, err := c.Prompt(ctx, Prompt{
		Session: Session{ID: root.ID}, Cwd: b.Start.Setup.Dir, Model: "openai/fixture",
		MaxIterations: 4, MaxTokens: 1000, Timeout: Nanos(time.Minute), ToolTimeout: Nanos(time.Minute),
		Effort: "off", None: true, Env: PromptEnv(rig.env), Content: content,
	})
	must(t, err)
	if reply.Stop != StopEndTurn || reply.Session != root.ID {
		t.Fatalf("reply = %+v", reply)
	}
	var kinds []string
	var answer strings.Builder
	for {
		ev, err := sub.Next(ctx)
		must(t, err)
		kinds = append(kinds, ev.Method)
		if ev.Delta != nil && ev.Delta.Kind == TextAnswer {
			answer.WriteString(ev.Delta.Text)
		}
		if ev.End != nil {
			if ev.End.Reply == nil || ev.End.Reply.Stop != StopEndTurn {
				t.Fatalf("end = %+v", ev.End)
			}
			break
		}
	}
	if answer.String() != "done" {
		t.Fatalf("answer deltas = %q (events %v)", answer.String(), kinds)
	}
	if !strings.Contains(strings.Join(kinds, ","), "checkpoint") || !strings.Contains(strings.Join(kinds, ","), "message") {
		t.Fatalf("events = %v", kinds)
	}
	after, err := c.Info(ctx, InfoReq{ID: root.ID})
	must(t, err)
	if after.Lines != info.Lines+2 {
		t.Fatalf("lines after the turn = %d, want %d", after.Lines, info.Lines+2)
	}
	more, err := c.HistoryFrom(ctx, root.ID, info.Lines, nil)
	must(t, err)
	if len(more) != 2 {
		t.Fatalf("history from %d = %d lines", info.Lines, len(more))
	}
}

func TestLiveUnixTargetStartsASetupWithoutTheBinary(t *testing.T) {
	rig := newLiveRig(t)
	rig.launch(t, rig.work, "say done")
	execTarget, err := ParseTarget("exec://" + rig.bin)
	must(t, err)
	b := rig.prepare(t, execTarget, rig.work)
	dir, err := UserSocketDir()
	must(t, err)
	unix, err := ParseTarget("unix://" + SocketPath(dir, b.BuildKey))
	must(t, err)
	u := rig.prepare(t, unix, rig.work)
	if u.Start.Executable.Path != "" {
		t.Fatalf("a unix:// start names a program: %+v", u.Start.Executable)
	}
	if u.Start.Setup.Key == b.Start.Setup.Key {
		t.Fatal("a unix:// setup, which knows no build, shares the exec:// key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reached, err := Reach(ctx, u)
	must(t, err)
	defer reached.Client.Close()
	if reached.Started {
		t.Fatal("a unix:// target started a daemon")
	}
	list, err := reached.Client.Sessions(ctx, SessionsReq{})
	must(t, err)
	if len(list.Sessions) == 0 {
		t.Fatal("the unix:// worker reads no sessions of the store")
	}
}

func TestLiveGoStartsTheDaemonAndTheBinarysLaunchJoinsIt(t *testing.T) {
	rig := newLiveRig(t)
	// A copy of the binary is another build: no daemon of it lives yet.
	data, err := os.ReadFile(rig.bin)
	must(t, err)
	copied := filepath.Join(rig.root, "bin", "polly-zig")
	must(t, os.MkdirAll(filepath.Dir(copied), 0o755))
	must(t, os.WriteFile(copied, data, 0o755))
	rig.bin = copied
	target, err := ParseTarget("exec://" + copied)
	must(t, err)
	b := rig.prepare(t, target, rig.work)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reached, err := Reach(ctx, b)
	must(t, err)
	defer reached.Client.Close()
	if !reached.Started {
		t.Fatal("no daemon of a fresh build was there, yet this launch did not start one")
	}
	if reached.Client.Welcome.Setup != b.Start.Setup.Key {
		t.Fatalf("welcome = %+v", reached.Client.Welcome)
	}
	// The binary's own launch finds the daemon Go started, and its worker:
	// the session it makes is read through Go's connection.
	rig.launch(t, rig.work, "say done")
	list, err := reached.Client.Sessions(ctx, SessionsReq{})
	must(t, err)
	if len(list.Sessions) != 1 {
		t.Fatalf("sessions = %+v", list)
	}
	if daemon := list.Sessions[0].Daemon; daemon == "" || !strings.HasSuffix(daemon, b.BuildKey+".sock") {
		t.Fatalf("the launch's session notes daemon %q, want the socket of build %s", daemon, b.BuildKey)
	}
	// The log the daemon was started with is the setup's.
	if _, err := os.Stat(SetupLog(rig.runtime, b.BuildKey)); err != nil {
		t.Fatalf("daemon log: %v", err)
	}
}
