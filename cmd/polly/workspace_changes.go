package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/worktree"
)

// workspaceChangeState owns a session's immutable baseline and serializes
// observations. Tool history stays independent of the latest workspace report.
type workspaceChangeState struct {
	mu            sync.Mutex
	tracker       *worktree.ChangeTracker
	baseline      *sessions.WorkspaceBaseline
	reason        string
	closed        bool
	lastData      []byte
	lastReport    *tools.FileChanges
	startupDone   chan struct{}
	ready         chan struct{}
	cancelStartup context.CancelFunc
	startupErr    error
	initialReport *artifacts.Ref
}

// startWorkspaceChanges publishes only the lifecycle handles. The worker owns
// the result until startupDone closes; metadata is committed by the caller of
// finishWorkspaceChanges, on the UI thread for managed sessions.
func (s *conversationState) startWorkspaceChanges(ctx context.Context, tracker *worktree.ChangeTracker) {
	if tracker == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	state := &workspaceChangeState{tracker: tracker, startupDone: make(chan struct{}), ready: make(chan struct{}), cancelStartup: cancel}
	s.workspaceChanges = state
	go func() {
		defer cancel()
		defer func() {
			state.startupErr = context.Cause(ctx)
			close(state.startupDone)
		}()
		s.prepareWorkspaceBaseline(ctx, tracker)
		if ctx.Err() != nil {
			return
		}
		report := tools.FileChanges{Reason: state.reason}
		if state.reason == "" && state.baseline != nil {
			var err error
			report, err = tracker.WorkspaceChanges(ctx, state.baseline.Root, state.baseline.Tree)
			if err != nil {
				report = tools.FileChanges{Root: state.baseline.Root, Reason: err.Error()}
			}
		}
		state.lastData, _ = json.Marshal(report)
		report.ObservedAt = time.Now().UTC()
		data, err := json.Marshal(report)
		if err == nil {
			var ref artifacts.Ref
			ref, err = s.artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, MIMEType: "application/json", Name: "workspace-changes.json", Data: data})
			if err == nil {
				state.initialReport = &ref
			}
		}
		if err != nil {
			state.lastData = nil
			report.Reason = "could not save workspace changes: " + err.Error()
			report.Tracked = false
		}
		state.lastReport = &report
	}()
}

func (s *conversationState) workspaceChangesPending() bool {
	if s == nil || s.workspaceChanges == nil || s.workspaceChanges.ready == nil {
		return false
	}
	select {
	case <-s.workspaceChanges.ready:
		return false
	default:
		return true
	}
}

func (s *conversationState) waitWorkspaceChanges(ctx context.Context) error {
	if s == nil || s.workspaceChanges == nil || s.workspaceChanges.ready == nil {
		return nil
	}
	state := s.workspaceChanges
	select {
	case <-state.ready:
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.startupErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// finishWorkspaceChanges never waits for Git. Its caller serializes this
// metadata update with UI commands before releasing queued input.
func (s *conversationState) finishWorkspaceChanges(ctx context.Context) bool {
	if !s.workspaceChangesPending() {
		return false
	}
	state := s.workspaceChanges
	select {
	case <-state.startupDone:
	default:
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !s.workspaceChangesPending() {
		return false
	}
	defer close(state.ready)
	if state.startupErr != nil {
		return true
	}
	if err := updateMetadata(ctx, s.session, func(md *sessions.Metadata) {
		if state.baseline != nil {
			md.ChangeBaseline = state.baseline
		}
		if state.initialReport != nil {
			md.WorkspaceChanges = state.initialReport
		}
	}); err != nil {
		state.reason = "could not save workspace baseline: " + err.Error()
		state.lastData = nil
		state.lastReport = &tools.FileChanges{Reason: state.reason}
	}
	return true
}

func (s *conversationState) initializeWorkspaceChanges(ctx context.Context, tracker *worktree.ChangeTracker) {
	s.startWorkspaceChanges(ctx, tracker)
	if s.workspaceChanges == nil {
		return
	}
	<-s.workspaceChanges.startupDone
	s.finishWorkspaceChanges(ctx)
}

func (s *conversationState) prepareWorkspaceBaseline(ctx context.Context, tracker *worktree.ChangeTracker) {
	state := s.workspaceChanges
	md, err := s.session.GetMetadata(ctx)
	if err != nil {
		state.reason = err.Error()
		return
	}
	if md.ChangeBaseline != nil {
		state.baseline = md.ChangeBaseline
		root := s.toolRegistry.ExecutionRoot()
		if root == "" {
			root, _ = os.Getwd()
		}
		root, _ = filepath.EvalSymlinks(root)
		if !sandbox.PathWithin(root, state.baseline.Root) {
			state.reason = "session baseline belongs to another workspace: " + state.baseline.Root
			return
		}
		reader, err := s.artifactStore.Open(ctx, state.baseline.Pack.ID)
		if err != nil {
			state.reason = "saved baseline unavailable: " + err.Error()
			return
		}
		pack, err := io.ReadAll(io.LimitReader(reader, worktree.ChangeMaxBaselineBytes+1))
		_ = reader.Close()
		if err == nil {
			err = tracker.RestoreBaseline(ctx, worktree.ChangeBaseline{Root: state.baseline.Root, Tree: state.baseline.Tree, Pack: pack})
		}
		if err != nil {
			state.reason = "saved baseline unavailable: " + err.Error()
		}
	} else {
		baseline, reason, err := tracker.CaptureBaseline(ctx, s.toolRegistry.ExecutionRoot())
		if err != nil {
			state.reason = err.Error()
			return
		}
		if reason != "" {
			state.reason = reason
			return
		}
		ref, err := s.artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, MIMEType: "application/x-git-packed-objects", Name: "workspace-baseline.pack", Data: baseline.Pack})
		if err != nil {
			state.reason = err.Error()
			return
		}
		state.baseline = &sessions.WorkspaceBaseline{Root: baseline.Root, Tree: baseline.Tree, Pack: ref}
	}
}

// refreshWorkspaceChanges also runs after cancellation: files written before
// a command failed still belong in the net report. Errors become visible
// coverage information and never turn a successful tool into a failure.
func (s *conversationState) refreshWorkspaceChanges(ctx context.Context) *tools.FileChanges {
	state := s.workspaceChanges
	if state == nil {
		return nil
	}
	if err := s.waitWorkspaceChanges(ctx); err != nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	report := tools.FileChanges{Reason: state.reason}
	if state.reason == "" && state.baseline != nil {
		var err error
		report, err = state.tracker.WorkspaceChanges(ctx, state.baseline.Root, state.baseline.Tree)
		if err != nil {
			report = tools.FileChanges{Root: state.baseline.Root, Reason: err.Error()}
		}
	}
	semantic, err := json.Marshal(report)
	if err == nil && state.lastReport != nil && bytes.Equal(semantic, state.lastData) {
		return state.lastReport
	}
	report.ObservedAt = time.Now().UTC()
	data, err := json.Marshal(report)
	if err == nil {
		var ref artifacts.Ref
		ref, err = s.artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindBinary, MIMEType: "application/json", Name: "workspace-changes.json", Data: data})
		if err == nil {
			err = updateMetadata(ctx, s.session, func(md *sessions.Metadata) { md.WorkspaceChanges = &ref })
		}
	}
	if err != nil {
		report.Reason = "could not save workspace changes: " + err.Error()
		report.Tracked = false
	} else {
		state.lastData = semantic
		state.lastReport = &report
	}
	return &report
}

func loadWorkspaceChanges(ctx context.Context, md *sessions.Metadata, store artifacts.Store) *fileChanges {
	if md == nil || md.WorkspaceChanges == nil {
		return nil
	}
	unavailable := func(err error) *fileChanges {
		return &fileChanges{reason: "saved workspace changes unavailable: " + err.Error()}
	}
	if store == nil {
		return unavailable(fmt.Errorf("artifact store unavailable"))
	}
	reader, err := store.Open(ctx, md.WorkspaceChanges.ID)
	if err != nil {
		return unavailable(err)
	}
	defer reader.Close()
	var report tools.FileChanges
	if err = json.NewDecoder(io.LimitReader(reader, 4<<20)).Decode(&report); err != nil {
		return unavailable(err)
	}
	return workspaceChangesPresentation(&report)
}

func workspaceChangesPresentation(report *tools.FileChanges) *fileChanges {
	if report == nil {
		return nil
	}
	changes := &fileChanges{observedAt: report.ObservedAt, root: report.Root, tracked: report.Tracked, reason: report.Reason, truncated: report.Truncated, omitted: report.Omitted}
	for _, c := range report.Changes {
		changes.changes = append(changes.changes, fileChange{path: c.Path, kind: c.Kind, additions: c.Additions, deletions: c.Deletions, diff: c.Diff, truncated: c.Truncated, binary: c.Binary, countsUnknown: c.CountsUnknown})
	}
	return changes
}

func (m *replModel) setWorkspaceChanges(changes *fileChanges) {
	if changes == nil {
		return
	}
	if m.workspaceChanges != nil && !changes.observedAt.IsZero() && !changes.observedAt.After(m.workspaceChanges.observedAt) {
		return
	}
	m.workspaceChanges = changes
	m.inspections.version++
	m.visual.invalidate()
}

func (t *gotuiTurnUI) RecordWorkspaceChanges(report *tools.FileChanges) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if t.activeLocked() {
		t.model.setWorkspaceChanges(workspaceChangesPresentation(report))
	}
}

func (t *turnExecution) refreshWorkspaceChanges() {
	if !t.turnUI.TurnPersistenceAllowed() {
		return
	}
	report := t.state.refreshWorkspaceChanges(t.ctx)
	if ui, ok := t.turnUI.(interface{ RecordWorkspaceChanges(*tools.FileChanges) }); ok && report != nil {
		ui.RecordWorkspaceChanges(report)
	}
}

func (state *workspaceChangeState) stopStartup() {
	if state.cancelStartup != nil {
		state.cancelStartup()
		<-state.startupDone
		state.mu.Lock()
		defer state.mu.Unlock()
		select {
		case <-state.ready:
		default:
			state.startupErr = context.Canceled
			close(state.ready)
		}
	}
}

func (state *workspaceChangeState) close() error {
	state.stopStartup()
	state.mu.Lock()
	defer state.mu.Unlock()
	state.closed = true
	return state.tracker.Close()
}

func (state *workspaceChangeState) currentReport() *tools.FileChanges {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.lastReport
}
