package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/helper"
)

func TestBindModeAdmitsSeveralBindingsPerRootAndCopyModeOne(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	root := t.TempDir()
	p := testProvider(t, f, nil)
	scope := tools.ToolScope{Root: root, Session: "shared"}
	member := openScope(t, p, nativeLoad("bash"), scope)
	workflow := openScope(t, p, nativeLoad("bash"), scope)
	creates, execs, _, removed, _ := f.snapshot()
	if len(creates) != 1 || len(execs) != 2 || len(removed) != 0 {
		t.Fatalf("two bindings: creates %d execs %d removed %d", len(creates), len(execs), len(removed))
	}
	if err := p.Destroy(context.Background(), root); err == nil {
		t.Fatal("destroy succeeded under open bindings")
	}
	member.Close()
	if _, _, _, removed, _ := f.snapshot(); len(removed) != 0 {
		t.Fatal("closing one binding removed the container another still uses")
	}
	workflow.Close()
	if _, _, _, removed, _ := f.snapshot(); len(removed) != 1 {
		t.Fatalf("closing the last binding did not remove the container: %v", removed)
	}

	f2, scripted, git, copyRoot, _ := copyFixture(t)
	_ = scripted
	copy := copyProvider(t, f2, git, nil)
	first := openScope(t, copy, OpenOptions{}, tools.ToolScope{Root: copyRoot, Session: "copy-shared"})
	defer first.Close()
	if _, err := copy.OpenTools(OpenOptions{})(context.Background(), tools.ToolScope{Root: copyRoot, Session: "copy-shared"}); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("second copy binding = %v", err)
	}
}

func TestPruneRemovesContainersOfUnknownSessions(t *testing.T) {
	f := newFakeEngine(t, helper.Options{})
	p := testProvider(t, f, nil)
	live, gone, anonymous := t.TempDir(), t.TempDir(), t.TempDir()
	openScope(t, p, OpenOptions{KeepOnClose: true}, tools.ToolScope{Root: live, Session: "live-session"}).Close()
	openScope(t, p, OpenOptions{KeepOnClose: true}, tools.ToolScope{Root: gone, Session: "gone-session"}).Close()
	openScope(t, p, OpenOptions{KeepOnClose: true}, tools.ToolScope{Root: anonymous}).Close()
	keep := func(session string) bool { return session == "live-session" }
	ctx := context.Background()
	pruned, err := Prune(ctx, f.host(), keep, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 2 {
		t.Fatalf("dry run would prune %+v", pruned)
	}
	if _, _, _, removed, _ := f.snapshot(); len(removed) != 0 {
		t.Fatal("dry run removed containers")
	}
	pruned, err = Prune(ctx, f.host(), keep, false, false)
	if err != nil || len(pruned) != 2 {
		t.Fatalf("prune = %+v, %v", pruned, err)
	}
	for _, entry := range pruned {
		if entry.Session == "live-session" || entry.Name == "" {
			t.Fatalf("pruned %+v", entry)
		}
	}
	_, _, _, removed, containers := f.snapshot()
	if len(removed) != 2 || len(containers) != 1 {
		t.Fatalf("removed %v containers %d", removed, len(containers))
	}
	if pruned, err = Prune(ctx, f.host(), keep, true, false); err != nil || len(pruned) != 1 || pruned[0].Session != "live-session" {
		t.Fatalf("prune --all = %+v, %v", pruned, err)
	}
	if _, _, _, _, containers := f.snapshot(); len(containers) != 0 {
		t.Fatal("prune --all left containers")
	}
}
