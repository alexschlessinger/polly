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
	mu         sync.Mutex
	tracker    *worktree.ChangeTracker
	baseline   *sessions.WorkspaceBaseline
	reason     string
	closed     bool
	lastData   []byte
	lastReport *tools.FileChanges
}

func (s *conversationState) initializeWorkspaceChanges(ctx context.Context, tracker *worktree.ChangeTracker) {
	if tracker == nil {
		return
	}
	s.workspaceChanges = &workspaceChangeState{tracker: tracker}
	state := s.workspaceChanges
	defer s.refreshWorkspaceChanges(ctx)
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
		if err = tracker.RestoreBaseline(ctx, baseline); err != nil {
			state.reason = err.Error()
			return
		}
		if err = updateMetadata(ctx, s.session, func(md *sessions.Metadata) { md.ChangeBaseline = state.baseline }); err != nil {
			state.reason = err.Error()
			return
		}
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

func (state *workspaceChangeState) close() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.closed = true
	return state.tracker.Close()
}
