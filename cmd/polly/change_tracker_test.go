package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
)

func TestConversationTracksToolChangesWithoutWorkspaceBaseline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	t.Chdir(t.TempDir())
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.WriteFile("a.txt", []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "add", "a.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	ctx := context.Background()
	config := &Config{NoSkills: true, NoSandbox: true, Tools: []string{"bash"}}
	store := testOpenMemoryStore(t, nil)
	state, err := (&conversationOpener{config: config, sessionStore: store, cmd: getCommand()}).openNew(ctx, "tool-changes", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if state.changeTracker == nil || any(state.toolRegistry.ChangeTracker()) != state.changeTracker {
		t.Fatal("conversation must own the registry's per-tool tracker")
	}
	assertNoWorkspaceReport := func() {
		t.Helper()
		md, err := state.session.GetMetadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if md.ChangeBaseline != nil || md.WorkspaceChanges != nil {
			t.Fatalf("conversation persisted workspace tracking: %+v", md)
		}
	}
	assertNoWorkspaceReport()
	bash, ok := state.effectiveTools().Get("bash")
	if !ok {
		t.Fatal("bash is not installed")
	}
	out, err := bash.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"command": "printf 'after\\n' > a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := out.Data.(tools.CommandResult)
	if !ok || result.Changes == nil || !result.Changes.Tracked || len(result.Changes.Changes) != 1 {
		t.Fatalf("bash lost its per-tool diff: %+v", out.Data)
	}
	change := result.Changes.Changes[0]
	if change.Path != "a.txt" || change.Additions != 1 || change.Deletions != 1 {
		t.Fatalf("bash diff = %+v", change)
	}
	assertNoWorkspaceReport()
}

type testChangeTrackerCloser struct {
	closed bool
	err    error
}

func (c *testChangeTrackerCloser) Close() error {
	c.closed = true
	return c.err
}

func TestConversationClosesToolChangeTracker(t *testing.T) {
	want := errors.New("tracker close failed")
	tracker := &testChangeTrackerCloser{err: want}
	state := &conversationState{changeTracker: tracker}
	if err := state.Close(); !errors.Is(err, want) || !tracker.closed {
		t.Fatalf("Close = %v, tracker closed = %v", err, tracker.closed)
	}
}
