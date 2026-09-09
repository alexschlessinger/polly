package swarm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func submittedInput(t *testing.T, r *Runtime, base worktree.Snapshot, files map[string]string) TaskReference {
	t.Helper()
	ctx := context.Background()
	m, err := r.manager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	for path, text := range files {
		full := filepath.Join(c.Path, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if text == "<delete>" {
			err = os.Remove(full)
		} else {
			err = os.WriteFile(full, []byte(text), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := m.Capture(ctx, c.Path)
	if err != nil {
		t.Fatal(err)
	}
	id := ids.New()
	ref := TaskReference{Task: id, Revision: 1}
	err = r.update(ctx, func(s *State) error {
		run := r.currentRun(s)
		s.Contexts[id] = &ExecutionContext{ID: id, Owner: id, Root: c.Path, Checkout: &c}
		s.Members[id] = &Member{ID: id, Context: id, Task: id, Status: "idle"}
		s.Tasks[id] = &Task{ID: id, Run: run.ID, Owner: id, Status: "awaiting_review", Revision: 1, Snapshot: snapshot.ID}
		s.Snapshots[snapshot.ID] = &snapshot
		s.Snapshots[base.ID] = &base
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func candidateError(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), code) {
		data, _ := json.Marshal(err)
		if !strings.Contains(string(data), code) {
			t.Fatalf("want %s, got %v %s", code, err, data)
		}
	}
}

func TestIntegrationOrderedBasesLazyRefreshAndPathDrift(t *testing.T) {
	r, p := applyFixture(t, false)
	ctx := context.Background()
	first := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "first\n"})
	if err := os.WriteFile(filepath.Join(r.config.Root, "b.txt"), []byte("base b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base2, err := r.worktrees.Capture(ctx, r.config.Root)
	if err != nil {
		t.Fatal(err)
	}
	second := submittedInput(t, r, base2, map[string]string{"b.txt": "second\n"})
	before, _ := r.read(ctx)
	c, err := r.PrepareIntegration(ctx, []TaskReference{first, second}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "ready" || len(c.Pending) != 0 || c.Drift != "paths" {
		t.Fatalf("candidate %+v", c)
	}
	after, _ := r.read(ctx)
	if len(before.Contexts) != len(after.Contexts) || after.Snapshots[c.Merged.ID] == nil {
		t.Fatal("prepare allocated context or omitted merged snapshot")
	}
	if _, err = r.AcceptIntegration(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	refs := func() string {
		cmd := exec.Command("git", "for-each-ref", "--format=%(refname)", "refs/polly/snapshots/")
		cmd.Dir = r.config.Root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	beforeRefs := refs()
	unchanged, err := r.RefreshIntegration(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Changed || unchanged.ID != c.ID || !unchanged.Accepted || beforeRefs != refs() {
		t.Fatal("no-op refresh allocated or discarded acceptance")
	}
	if err := os.WriteFile(filepath.Join(r.config.Root, "unrelated.txt"), []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	receipt, err := r.ApplyIntegration(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "applied" || receipt.ObservedParent.Tree == c.Parent.Tree || receipt.Plan.Merged.Tree != c.Merged.Tree {
		t.Fatal("receipt conflated validation and application trees")
	}
	for path, want := range map[string]string{"a.txt": "first\n", "b.txt": "second\n", "unrelated.txt": "keep\n"} {
		data, _ := os.ReadFile(filepath.Join(r.config.Root, path))
		if string(data) != want {
			t.Fatalf("%s=%q", path, data)
		}
	}
	if again, err := r.ApplyIntegration(ctx, c.ID); err != nil || again.Started != receipt.Started {
		t.Fatal("duplicate receipt changed", err)
	}
	state, _ := r.read(ctx)
	for _, ref := range []TaskReference{first, second} {
		if state.Tasks[ref.Task].Status != "done" {
			t.Fatal("task not completed")
		}
	}
}

func TestIntegrationConflictRepairTailAndSupersession(t *testing.T) {
	r, p := applyFixture(t, false)
	ctx := context.Background()
	a := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "worker\n"})
	b := submittedInput(t, r, p.Parent, map[string]string{"late.txt": "tail\n"})
	if err := os.WriteFile(filepath.Join(r.config.Root, "a.txt"), []byte("parent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := r.PrepareIntegration(ctx, []TaskReference{a, b}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "conflicted" || len(c.Conflicts) == 0 || len(c.Pending) != 1 || c.Pending[0].Task != b.Task {
		t.Fatalf("conflict/tail lost: %+v", c)
	}
	if c.Conflicts[0].Base.ID != p.Parent.ID || len(c.Conflicts[0].Entries) == 0 {
		t.Fatal("conflict provenance lost")
	}
	_, err = r.AcceptIntegration(ctx, c.ID)
	candidateError(t, err, "conflicts")
	bad := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "bad repair\n"})
	_, err = r.ReviseIntegration(ctx, c.ID, bad)
	candidateError(t, err, "invalid_repair")
	repair := submittedInput(t, r, c.Merged, map[string]string{"a.txt": "resolved\n"})
	next, err := r.ReviseIntegration(ctx, c.ID, repair)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == c.ID || next.Status != "ready" || len(next.Pending) != 0 || len(next.Repairs) != 1 {
		t.Fatalf("repair did not continue tail: %+v", next)
	}
	old, _ := r.ReadIntegration(ctx, c.ID)
	if old.Successor != next.ID || old.Status != "superseded" {
		t.Fatal("predecessor not superseded")
	}
	_, err = r.ApplyIntegration(ctx, c.ID)
	candidateError(t, err, "superseded")
	_, err = r.RefreshIntegration(ctx, c.ID)
	candidateError(t, err, "superseded")
	if _, err = r.AcceptIntegration(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ApplyIntegration(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	state, _ := r.read(ctx)
	for _, ref := range []TaskReference{a, b, repair} {
		if state.Tasks[ref.Task].Status != "done" {
			t.Fatal("contribution not done", ref)
		}
	}
	for path, want := range map[string]string{"a.txt": "resolved\n", "late.txt": "tail\n"} {
		data, _ := os.ReadFile(filepath.Join(r.config.Root, path))
		if string(data) != want {
			t.Fatalf("%s=%q", path, data)
		}
	}
}

func TestIntegrationDriftRefreshAndStaleRevision(t *testing.T) {
	for _, drift := range []string{"paths", "tree"} {
		t.Run(drift, func(t *testing.T) {
			r, p := applyFixture(t, false)
			ctx := context.Background()
			ref := submittedInput(t, r, p.Parent, map[string]string{"new.txt": "candidate\n"})
			c, err := r.PrepareIntegration(ctx, []TaskReference{ref}, drift)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.AcceptIntegration(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			path := "unrelated.txt"
			if drift == "paths" {
				path = "new.txt"
			}
			if err := os.WriteFile(filepath.Join(r.config.Root, path), []byte("parent\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = r.ApplyIntegration(ctx, c.ID)
			candidateError(t, err, "parent_changed")
			next, err := r.RefreshIntegration(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !next.Changed || next.ID == c.ID || next.Accepted {
				t.Fatal("changed refresh retained acceptance")
			}
			_, err = r.AcceptIntegration(ctx, c.ID)
			candidateError(t, err, "superseded")
			if err := r.Review(ctx, ref.Task, ref.Revision, false, "revise submission"); err != nil {
				t.Fatal(err)
			}
			_, err = r.RefreshIntegration(ctx, next.ID)
			candidateError(t, err, "stale_task")
		})
	}
}

func TestIntegrationAuthorityAndGenericToolBypass(t *testing.T) {
	r, p := applyFixture(t, false)
	ctx := context.Background()
	r.RegisterParentTools(r.config.Registry)
	for _, name := range []string{"swarm_preview", "swarm_apply"} {
		if _, exists := r.config.Registry.Get(name); exists {
			t.Fatal("obsolete integration alias registered", name)
		}
	}
	for _, key := range []string{"identity", "actor", "parent", "controller", "run", "IDentity"} {
		_, err := r.integrationOperation(ctx, map[string]any{"op": "read", "id": "candidate", key: r.ID})
		candidateError(t, err, "invalid_args")
	}
	ref := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "child\n"})
	s, _ := r.read(ctx)
	member := s.Members[ref.Task]
	c := s.Contexts[member.Context]
	ec, err := r.contextPolicy(ctx, s, c)
	if err != nil {
		t.Fatal(err)
	}
	registry, _, err := r.config.Registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	r.registerMemberTools(registry, member.ID, "", r.parent, false)
	for _, name := range []string{"swarm_integration", "swarm_apply", "swarm_preview", "swarm_review"} {
		if _, exists, allowed := registry.GetIfAllowed(name); exists && allowed {
			t.Fatal("child acquired parent tool", name)
		}
	}
	h := &workflowHost{runtime: r, controller: member.ID}
	defer h.close()
	_, err = h.Call(ctx, workflow.Operation{Kind: "tool", Args: map[string]any{"context": c.ID, "name": "swarm_integration", "args": map[string]any{"op": "apply", "id": "candidate", "actor": r.ID}}})
	candidateError(t, err, "tool_denied")
	if _, err := r.integrationOperation(ctx, map[string]any{"op": "prepare", "tasks": []any{map[string]any{"task": ref.Task, "revision": ref.Revision, "identity": r.ID}}}); err == nil {
		t.Fatal("forged task identity accepted")
	}
}
