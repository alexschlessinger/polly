package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
)

func TestSwarmCleanupAndForgetUseBackgroundHook(t *testing.T) {
	for _, command := range []string{"/swarm cleanup all", "/swarm cleanup copy", "/swarm forget"} {
		t.Run(command, func(t *testing.T) {
			queued := false
			ctx := &replCommandContext{
				state: &conversationState{swarm: &swarm.Runtime{}},
				swarmMaintenance: func(label, success string, run func(context.Context) error) error {
					queued = label != "" && success != "" && run != nil
					return nil // Uninitialized runtime must never be called synchronously.
				},
			}
			handled, _, err := defaultReplCommands.dispatch(command, ctx)
			if err != nil || !handled || !queued {
				t.Fatalf("command was not queued: handled=%v queued=%v err=%v", handled, queued, err)
			}
		})
	}
}

func TestSwarmMaintenanceLeavesUIResponsiveAndReportsToOrigin(t *testing.T) {
	m, other := newReplModel(), newReplModel()
	tab := &replTab{model: m}
	r := &managedREPL{model: m, tabs: []*replTab{tab, {model: other}}, work: newREPLWork(), uiTasks: make(chan func(), 4)}
	defer r.work.close()
	entered, release := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	err := r.startSwarmMaintenance("swarm cleanup", "cleanup finished", func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance did not start")
	}
	if !m.mu.TryLock() {
		t.Fatal("maintenance held the UI lock")
	}
	m.mu.Unlock()
	r.model = other // Completion must not follow the active tab.
	close(release)
	select {
	case apply := <-r.uiTasks:
		apply()
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance did not report completion")
	}
	if !strings.Contains(strings.Join(transcriptTexts(m), "\n"), "cleanup finished") || strings.Contains(strings.Join(transcriptTexts(other), "\n"), "cleanup finished") {
		t.Fatal("completion went to the wrong workspace")
	}
}

func TestSwarmMaintenanceCancelsOnShutdown(t *testing.T) {
	r := &managedREPL{model: newReplModel(), work: newREPLWork(), uiTasks: make(chan func(), 1)}
	defer r.work.close()
	entered := make(chan struct{})
	if err := r.startSwarmMaintenance("swarm cleanup", "done", func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- r.work.close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not cancel maintenance")
	}
}
