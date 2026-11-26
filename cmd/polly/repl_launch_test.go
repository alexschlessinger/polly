package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
)

func TestRelaunchArgsKeepOptionsAndReplaceTheSession(t *testing.T) {
	args := []string{"--model", "x/y", "-c", "old", "--sandbox", "workspace", "--context=older", "-L", "--last", "--setup", "-context", "oldest", "--effort", "high"}
	kept := []string{"--model", "x/y", "--sandbox", "workspace", "--effort", "high"}
	if got := relaunchArgs(args, ""); !slices.Equal(got, kept) {
		t.Fatalf("a new session relaunches with %q, want %q", got, kept)
	}
	if got, want := relaunchArgs(args, "next"), append(slices.Clone(kept), "--context", "next"); !slices.Equal(got, want) {
		t.Fatalf("a named session relaunches with %q, want %q", got, want)
	}
}

func TestPaneCommandRunsInThePanesOwnTerminal(t *testing.T) {
	owner := []string{"HOME=/h", "TERM=xterm", "REX_BLOCK=block:owner", "POLLYTOOL_MODEL=m", "COLORTERM=truecolor"}
	pane := []string{"HOME=/elsewhere", "TERM=xterm-rex", "REX_BLOCK=block:pane", "REX_SERVER=unix:///s", "PATH=/bin"}
	got := paneExecEnv(owner, pane)
	want := []string{"HOME=/h", "POLLYTOOL_MODEL=m", "TERM=xterm-rex", "REX_BLOCK=block:pane", "REX_SERVER=unix:///s"}
	if !slices.Equal(got, want) {
		t.Fatalf("pane command environment = %q, want %q", got, want)
	}
}

// becomePane plays the proxy in the terminal the host opened last: it greets
// the owner with that pane's token and returns the command it is told to
// become.
func becomePane(t *testing.T, fake *fakePanes) paneExec {
	t.Helper()
	argv := fake.opened[len(fake.opened)-1]
	conn, err := net.Dial("unix", argv[2])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	hello := append(paneSize{cols: 80, rows: 24}.encode(), argv[3]...)
	if err := writePaneFrame(conn, paneFrameHello, append(hello, 0)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil || len(data) == 0 || data[0] != paneModeExec {
		t.Fatalf("the owner answered %q, %v; want a command", data, err)
	}
	var x paneExec
	if err := json.Unmarshal(data[1:], &x); err != nil {
		t.Fatal(err)
	}
	return x
}

func TestSessionsOpenAsTabsOfTheHost(t *testing.T) {
	t.Setenv("POLLYTOOL_CONTEXT", "this-one")
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	fake := hostPanes(t, r)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	r.model.mu.Lock()
	r.requestNewTabLocked()
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool { return fake.opens() == 1 })
	if x := becomePane(t, fake); x.Argv[0] != exe || slices.Contains(x.Argv, "--context") {
		t.Fatalf("a new session's tab runs %q, want this polly with no session named", x.Argv)
	}
	r.model.mu.Lock()
	r.requestOpenLocked("saved")
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool { return fake.opens() == 2 })
	if len(r.tabs) != 1 || r.opening != "" {
		t.Fatalf("sessions opened inside this polly: tabs=%d opening=%q", len(r.tabs), r.opening)
	}
	if !slices.Equal(fake.places, []panePlace{paneTab, paneTab}) {
		t.Fatalf("sessions were placed at %v, want tabs", fake.places)
	}
	// The second pane is told to become polly on the named session, with this
	// process's environment less the variable that chose its own session.
	x := becomePane(t, fake)
	if x.Argv[0] != exe || !slices.Equal(x.Argv[len(x.Argv)-2:], []string{"--context", "saved"}) {
		t.Fatalf("the tab runs %q, want this polly on --context saved", x.Argv)
	}
	if slices.Contains(x.Env, "POLLYTOOL_CONTEXT=this-one") || !slices.ContainsFunc(x.Env, func(e string) bool { return strings.HasPrefix(e, "PATH=") }) {
		t.Fatal("the tab's environment is not this process's without its session")
	}
	if dir, _ := os.Getwd(); x.Dir != dir {
		t.Fatalf("the tab starts in %q, want %q", x.Dir, dir)
	}
	// A token is spent by the pane that used it.
	if _, ok := r.panes.takeExec(fake.opened[1][3]); ok {
		t.Fatal("a pane's command outlived the pane that became it")
	}

	// A session this polly already shows is shown, not launched again.
	r.model.mu.Lock()
	r.requestOpenLocked("root")
	r.model.mu.Unlock()
	if fake.opens() != 2 {
		t.Fatal("an open session was launched in another tab")
	}
}

func TestSessionOpensHereWhenTheHostFails(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	fake := hostPanes(t, r)
	fake.err = errors.New("no rex here")
	r.model.mu.Lock()
	r.requestNewTabLocked()
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool { return r.panes.tabless })
	if r.opening == "" {
		t.Fatal("the session did not fall back to this polly")
	}
	if text := plainStyledText(strings.Join(transcriptTexts(r.model), "\n")); !strings.Contains(text, "Rex tab unavailable") {
		t.Fatalf("the failure was not reported: %s", text)
	}
	r.finishOpen(<-r.openDone)
	if len(r.tabs) != 2 {
		t.Fatalf("tabs = %d, want the session opened here", len(r.tabs))
	}
	// The host is not asked again.
	r.model.mu.Lock()
	r.requestNewTabLocked()
	r.model.mu.Unlock()
	if fake.opens() != 1 || r.opening == "" {
		t.Fatalf("a failed host was retried: opens=%d opening=%q", fake.opens(), r.opening)
	}
	r.finishOpen(<-r.openDone)
	if len(r.tabs) != 3 {
		t.Fatalf("tabs = %d, want the second session opened here too", len(r.tabs))
	}
}

func TestListeningSweepsSocketsOfDeadPollys(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	hostPanes(t, r)
	root, err := scratch.EnsureRoot()
	if err != nil {
		t.Fatal(err)
	}
	// One socket nobody listens on any more, and one a living polly holds.
	dead := filepath.Join(root, "pane-1.sock")
	gone, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	gone.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = gone.Close()
	live := filepath.Join(root, "pane-2.sock")
	other, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := r.listenPanes(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dead polly's socket was left behind: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a living polly's socket was removed: %v", err)
	}
}

func TestEditorRunsThroughTheHost(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "myedit --wait")
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	fake := hostPanes(t, r)
	if err := r.openEditor("/plans/draft one.md"); err != nil {
		t.Fatal(err)
	}
	pumpUI(t, r, func() bool { return fake.opens() == 1 })
	if fake.places[0] != paneIfTerminal {
		t.Fatalf("the editor was placed at %v: only a terminal editor wants a pane", fake.places[0])
	}
	// The shell splits the editor's arguments; the path stays one word.
	want := []string{"/bin/sh", "-c", `myedit --wait "$@"`, "sh", "/plans/draft one.md"}
	if x := becomePane(t, fake); !slices.Equal(x.Argv, want) {
		t.Fatalf("the editor runs as %q, want %q", x.Argv, want)
	}
}
