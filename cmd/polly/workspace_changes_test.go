package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestWorkspaceChangesOverrideToolHistory(t *testing.T) {
	m := newReplModel()
	call := messages.ChatMessageToolCall{ID: "edit", Name: "edit_file"}
	m.inspections.setResult(call, toolDataResult(t, call, "", editChanges("x.txt")))
	report := tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{tools.DiffFileChange("new.txt", "", "new\n", false, true)}}
	m.setWorkspaceChanges(workspaceChangesPresentation(&report))
	a, d, n := m.changeStats()
	if a != 1 || d != 0 || n != 1 {
		t.Fatalf("net stats: +%d -%d %d files", a, d, n)
	}
	projected, err := (changesView{}).Project(context.Background(), viewSource{model: m}, viewState{expandAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.changesInspector.items) != 1 || projected.changesInspector.items[0].key != "new.txt" {
		t.Fatalf("net rows: %+v", projected.changesInspector.items)
	}
	report.Changes = nil
	m.setWorkspaceChanges(workspaceChangesPresentation(&report))
	if a, d, n = m.changeStats(); a != 0 || d != 0 || n != 0 {
		t.Fatalf("reverted stats: %d %d %d", a, d, n)
	}
}

func TestWorkspaceChangesCoverageAndFullBody(t *testing.T) {
	report := tools.FileChanges{Tracked: true, Truncated: true, Omitted: 5, Changes: []tools.FileChange{
		tools.DiffFileChange("many.txt", "", strings.Repeat("line\n", 600), false, true),
		{Path: "large.txt", Kind: tools.ChangeModified, CountsUnknown: true, Truncated: true},
	}}
	m := newReplModel()
	m.setWorkspaceChanges(workspaceChangesPresentation(&report))
	projected, err := (changesView{}).Project(context.Background(), viewSource{model: m}, viewState{expandAll: true})
	if err != nil {
		t.Fatal(err)
	}
	list := projected.changesInspector
	if !strings.Contains(plainStyledText(list.summary), "5 more files omitted") || !strings.Contains(plainStyledText(list.summary), "partial line counts") {
		t.Fatalf("summary: %s", list.summary)
	}
	if strings.Count(plainStyledText(list.items[0].body), "+line") != 600 {
		t.Fatal("net diff truncated at the old display limit")
	}
	if !strings.Contains(plainStyledText(list.items[1].title), "counts unavailable") || !strings.Contains(plainStyledText(list.items[1].body), "truncated") {
		t.Fatalf("missing coverage: %+v", list.items[1])
	}
}

func TestWorkspaceChangesArtifactReload(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	session, err := store.Acquire(ctx, "changes", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	report := tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{tools.DiffFileChange("new.txt", "", "new\n", false, true)}}
	data, _ := json.Marshal(report)
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	md, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md.WorkspaceChanges = &ref
	if err = session.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}
	md, err = session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored := loadWorkspaceChanges(ctx, md, session.ArtifactStore())
	if restored == nil || !restored.tracked || len(restored.changes) != 1 || restored.changes[0].path != "new.txt" {
		t.Fatalf("restored: %+v", restored)
	}
	copy := childDisplayCopy(&replModel{workspaceChanges: restored})
	if copy.workspaceChanges != restored {
		t.Fatal("display copy lost workspace report")
	}
}

func TestWorkspaceChangesSessionResumeAndClear(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	ctx := context.Background()
	root := t.TempDir()
	t.Chdir(root)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.invalid")
	write("a.txt", "before\n")
	git("add", ".")
	git("commit", "-qm", "base")
	write("new.txt", "untracked\n")
	store := testOpenMemoryStore(t, nil)
	cache := t.TempDir()
	open := func() *conversationState {
		t.Helper()
		session, err := store.Acquire(ctx, "net", sessions.AcquireOptions{})
		if err != nil {
			t.Fatal(err)
		}
		registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
		tracker, err := worktree.NewChangeTracker(registry, cache, nil, worktree.ChangeLimits{})
		if err != nil {
			t.Fatal(err)
		}
		state := &conversationState{session: session, toolRegistry: registry, artifactStore: session.ArtifactStore()}
		state.initializeWorkspaceChanges(ctx, tracker)
		if state.workspaceChanges.reason != "" {
			t.Fatal(state.workspaceChanges.reason)
		}
		return state
	}
	first := open()
	write("a.txt", "after\n")
	report := first.refreshWorkspaceChanges(ctx)
	if again := first.refreshWorkspaceChanges(ctx); again != report {
		t.Fatal("unchanged report was persisted again")
	}
	if !report.Tracked || len(report.Changes) != 2 {
		t.Fatalf("first report: %+v", report)
	}
	if err := first.session.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	second := open()
	defer second.Close()
	md, err := second.session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restored := loadWorkspaceChanges(ctx, md, second.artifactStore)
	if restored == nil || !restored.tracked || len(restored.changes) != 2 || !strings.Contains(restored.changes[0].diff, "-before\n+after\n") {
		t.Fatalf("resumed baseline: %+v", restored)
	}
	write("a.txt", "before\n")
	report = second.refreshWorkspaceChanges(ctx)
	if !report.Tracked || len(report.Changes) != 1 || report.Changes[0].Path != "new.txt" {
		t.Fatalf("reverted resumed changes: %+v", report)
	}
}

func TestWorkspaceChangesIgnoreOlderAsyncReport(t *testing.T) {
	m := newReplModel()
	latest := &fileChanges{tracked: true, observedAt: time.Now()}
	m.setWorkspaceChanges(latest)
	m.setWorkspaceChanges(&fileChanges{tracked: true, observedAt: latest.observedAt.Add(-time.Second)})
	if m.workspaceChanges != latest {
		t.Fatal("older background refresh replaced a newer report")
	}
}

func TestWorkspaceChangesLiveInspector(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "net-live")
	r.setupWidgets()
	r.showTab(0)
	screen.SetSize(120, 40)
	report := tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{tools.DiffFileChange("untracked.txt", "", "new content\n", false, true)}}
	r.model.setWorkspaceChanges(workspaceChangesPresentation(&report))
	r.inspectCommand("changes")
	v := waitInspector(t, r, 120)
	if len(v.model.changesInspector.items) != 1 {
		t.Fatalf("live projection lost report: %s", inspectorText(v))
	}
	r.toggleChangesInspectorItems()
	text := inspectorText(waitInspector(t, r, 120))
	if !strings.Contains(text, "untracked.txt") || !strings.Contains(text, "+new content") {
		t.Fatalf("live net diff: %s", text)
	}
}

// Block after Git has captured the baseline but before it can be published.
// This keeps the startup ordering tests independent of machine speed.
type startupArtifactBarrier struct {
	artifacts.Store
	entered chan struct{}
	release chan struct{}
}

func (b *startupArtifactBarrier) Put(ctx context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	if blob.Name == "workspace-baseline.pack" {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
			return artifacts.Ref{}, context.Cause(ctx)
		}
	}
	return b.Store.Put(ctx, blob)
}

func TestWorkspaceStartupAsync(t *testing.T) {
	for _, cancelStartup := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelStartup), func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "test"}, {"config", "user.email", "test@example.invalid"}} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			if err := os.WriteFile("a.txt", []byte("before\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "base"}} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			store := testOpenMemoryStore(t, nil)
			session, err := store.Acquire(context.Background(), "startup", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
			tracker, err := worktree.NewChangeTracker(registry, t.TempDir(), nil, worktree.ChangeLimits{})
			if err != nil {
				t.Fatal(err)
			}
			barrier := &startupArtifactBarrier{Store: session.ArtifactStore(), entered: make(chan struct{}), release: make(chan struct{})}
			state := &conversationState{session: session, toolRegistry: registry, artifactStore: barrier}
			t.Cleanup(func() { _ = state.Close() })
			state.startWorkspaceChanges(session.Context(), tracker)
			select {
			case <-barrier.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("baseline did not reach barrier")
			}
			if !state.workspaceChangesPending() || state.finishWorkspaceChanges(context.Background()) {
				t.Fatal("startup completed before baseline was ready")
			}
			r := newManagedREPL(&Config{}, "-", 0, 0)
			t.Cleanup(func() { _ = r.work.close() })
			if err := r.addTab(state); err != nil {
				t.Fatal(err)
			}
			r.model.mu.Lock()
			r.model.ed.setText("hello")
			r.submitComposerLocked()
			r.model.ed.setText("/swarm resume member")
			r.submitComposerLocked()
			r.model.mu.Unlock()
			if r.model.busy || len(r.model.queue) != 2 {
				t.Fatal("input was not held during startup")
			}
			runs := make(chan struct{}, 1)
			runTurn := func(context.Context, string, TurnUI) error { runs <- struct{}{}; return nil }
			r.startQueued(context.Background(), r.visibleTab(), runTurn)
			if len(runs) != 0 || r.visibleTab().turnDone != nil {
				t.Fatal("queued turn ran before baseline")
			}
			waitCtx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := state.waitWorkspaceChanges(waitCtx); !errors.Is(err, context.Canceled) {
				t.Fatalf("wait cancellation: %v", err)
			}
			if cancelStartup {
				state.workspaceChanges.stopStartup()
				if err := state.waitWorkspaceChanges(context.Background()); !errors.Is(err, context.Canceled) {
					t.Fatalf("closed startup did not release waiters: %v", err)
				}
				select {
				case <-state.workspaceChanges.startupDone:
				default:
					t.Fatal("stop did not join worker")
				}
				md, err := session.GetMetadata(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if md.ChangeBaseline != nil {
					t.Fatal("canceled startup persisted baseline")
				}
				return
			}
			if err := updateMetadata(context.Background(), session, func(md *sessions.Metadata) { md.Description = "edited while loading" }); err != nil {
				t.Fatal(err)
			}
			close(barrier.release)
			select {
			case <-state.workspaceChanges.startupDone:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not finish")
			}
			if !state.finishWorkspaceChanges(context.Background()) || state.workspaceChangesPending() {
				t.Fatal("completed startup not published")
			}
			md, err := session.GetMetadata(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if md.Description != "edited while loading" || md.ChangeBaseline == nil || md.WorkspaceChanges == nil {
				t.Fatalf("startup lost metadata: %+v", md)
			}
			if report := state.workspaceChanges.currentReport(); report == nil || !report.Tracked || report.Reason != "" {
				t.Fatalf("initial report: %+v", report)
			}
			r.startQueued(context.Background(), r.visibleTab(), runTurn)
			select {
			case <-runs:
			case <-time.After(time.Second):
				t.Fatal("queued turn did not start after baseline")
			}
			<-r.visibleTab().turnDone
			r.visibleTab().turnCancel()
			if err := os.WriteFile("a.txt", []byte("after\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if report := state.refreshWorkspaceChanges(context.Background()); report == nil || len(report.Changes) != 1 {
				t.Fatalf("fresh baseline cannot report edits without reimport: %+v", report)
			}
		})
	}
}
