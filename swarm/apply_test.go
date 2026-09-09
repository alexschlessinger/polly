package swarm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/worktree"
)

type applySandbox func(*exec.Cmd) error

func (f applySandbox) Wrap(cmd *exec.Cmd) error { return f(cmd) }

func applyFixture(t *testing.T, empty bool) (*Runtime, worktree.ApplyPlan) {
	t.Helper()
	skipIfWindows(t)
	r := runtimeTest(t, nilModel(), 1, 4)
	root := r.config.Root
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "test"}, {"config", "user.email", "test@example.invalid"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v", out, err)
		}
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "a.txt"), "base\n")
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v", out, err)
		}
	}
	ctx := context.Background()
	m, err := r.manager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		write(filepath.Join(c.Path, "a.txt"), "candidate\n")
	}
	merged, err := m.Capture(ctx, c.Path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := m.BuildApplyPlan(ctx, "candidate", base, merged, "tree")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.update(ctx, func(s *State) error {
		s.Tasks["task"] = &Task{ID: "task", Revision: 1, AcceptedRevision: 1, Status: "awaiting_review", Snapshot: merged.ID}
		s.Integrations[plan.ID] = &IntegrationCandidate{ID: plan.ID, Status: "ready", Inputs: []IntegrationInput{{TaskReference: TaskReference{Task: "task", Revision: 1}, Base: base, Submitted: merged}}, Parent: base, Merged: merged, Drift: "tree", Plan: plan, Accepted: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return r, plan
}

func nilModel() modelFunc {
	return modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") })
}

func TestApplyReceiptsEmptyDeltaAndRecovery(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "patch", true: "empty"}[empty], func(t *testing.T) {
			r, plan := applyFixture(t, empty)
			ctx := context.Background()
			if _, err := r.ApplyIntegration(ctx, plan.ID); err != nil {
				t.Fatal(err)
			}
			s, err := r.read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if s.Applies[plan.ID] == nil || s.Applies[plan.ID].Status != "applied" || s.Tasks["task"].Status != "done" {
				t.Fatal("receipt or task missing", s)
			}
			// A lost reply cannot repeat the patch, even after a later parent edit.
			if err := os.WriteFile(filepath.Join(r.config.Root, "a.txt"), []byte("later\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ApplyIntegration(ctx, plan.ID); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(filepath.Join(r.config.Root, "a.txt")); string(data) != "later\n" {
				t.Fatal("duplicate apply wrote files")
			}
		})
	}
	r, plan := applyFixture(t, false)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		s.Applies[plan.ID] = &ApplyRecord{ID: plan.ID, Plan: plan, Tasks: []TaskReference{{Task: "task", Revision: 1}}, Status: "applying"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.recoverApplies(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if s.Applies[plan.ID].Status != "not_applied" {
		t.Fatal("before states did not permit explicit retry")
	}
	// Simulate process death after the filesystem effect and before completion.
	if err := r.worktrees.WriteApply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := r.update(ctx, func(s *State) error { s.Applies[plan.ID].Status = "applying"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.recoverApplies(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	if s.Applies[plan.ID].Status != "applied" || s.Tasks["task"].Status != "done" {
		t.Fatal("after states did not reconcile")
	}
	if err := r.update(ctx, func(s *State) error {
		s.Tasks["task"].Revision++
		s.Tasks["task"].Status = "running"
		s.Applies[plan.ID].Status = "applying"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.recoverApplies(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	if s.Tasks["task"].Status != "running" || s.Applies[plan.ID].Status != "recovery_required" {
		t.Fatal("recovery overwrote later task revision")
	}
}

func TestApplyFinishesAfterCancellationAndShutdownWaits(t *testing.T) {
	r, plan := applyFixture(t, false)
	started, finish := make(chan struct{}), make(chan struct{})
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		return applySandbox(func(cmd *exec.Cmd) error {
			if slices.Contains(cmd.Args, "apply") && !slices.Contains(cmd.Args, "--check") {
				close(started)
				<-finish
			}
			return nil
		}), nil
	}, sandbox.Config{}))
	defer registry.Close()
	r.worktrees.Registry = registry
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.ApplyIntegration(ctx, plan.ID); done <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("write never began")
	}
	cancel()
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("shutdown abandoned write")
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown stuck")
	}
	s, err := r.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Applies[plan.ID].Status != "applied" || s.Tasks["task"].Status != "done" {
		t.Fatal("outcome not recorded")
	}
}

func TestApplyCancellationBeforeWriteAndWriteTimeout(t *testing.T) {
	r, plan := applyFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ApplyIntegration(ctx, plan.ID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s, _ := r.read(context.Background())
	if len(s.Applies) != 0 {
		t.Fatal("canceled preflight wrote intent")
	}
	r.config.ApplyTimeout = 20 * time.Millisecond
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		return applySandbox(func(cmd *exec.Cmd) error {
			if slices.Contains(cmd.Args, "apply") && !slices.Contains(cmd.Args, "--check") {
				cmd.Path = "/bin/sleep"
				cmd.Args = []string{"sleep", "2"}
			}
			return nil
		}), nil
	}, sandbox.Config{}))
	defer registry.Close()
	r.worktrees.Registry = registry
	if _, err := r.ApplyIntegration(context.Background(), plan.ID); err == nil {
		t.Fatal("timeout succeeded")
	}
	s, _ = r.read(context.Background())
	if s.Applies[plan.ID].Status != "recovery_required" || s.Tasks["task"].Status == "done" {
		t.Fatal("timeout claimed completion")
	}
}

func TestApplyLeaseLossLeavesRecoverableIntent(t *testing.T) {
	r, plan := applyFixture(t, false)
	started, finish := make(chan struct{}), make(chan struct{})
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		return applySandbox(func(cmd *exec.Cmd) error {
			if slices.Contains(cmd.Args, "apply") && !slices.Contains(cmd.Args, "--check") {
				close(started)
				<-finish
			}
			return nil
		}), nil
	}, sandbox.Config{}))
	defer registry.Close()
	r.worktrees.Registry = registry
	done := make(chan error, 1)
	go func() { _, err := r.ApplyIntegration(context.Background(), plan.ID); done <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("write never began")
	}
	if err := r.config.Parent.Close(); err != nil {
		close(finish)
		t.Fatal(err)
	}
	close(finish)
	if err := <-done; err == nil {
		t.Fatal("lease loss reported success")
	}
	parent, err := r.config.Store.Acquire(context.Background(), "parent", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	raw, err := parent.(sessions.CoordinationSession).ReadCoordination(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := decodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Applies[plan.ID].Status != "applying" || s.Tasks["task"].Status == "done" {
		t.Fatal("lease loss lost recoverable intent")
	}
	if data, _ := os.ReadFile(filepath.Join(r.config.Root, "a.txt")); string(data) != "base\n" {
		t.Fatal("write continued after lease loss")
	}
}
