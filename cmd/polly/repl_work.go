package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// replWork owns off-screen I/O. Shutdown cancels reads and pending launches,
// then waits for their cleanup and any report writes before closing sessions.
type replWork struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed bool
	errs   []error
}

func newREPLWork() *replWork {
	ctx, cancel := context.WithCancel(context.Background())
	return &replWork{ctx: ctx, cancel: cancel}
}

func (w *replWork) begin() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.wg.Add(1)
	return true
}

func (w *replWork) close() error {
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return errors.Join(w.errs...)
}

func (w *replWork) recordError(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	w.errs = append(w.errs, err)
	w.mu.Unlock()
}

func (r *managedREPL) background(fn func()) bool {
	if !r.work.begin() {
		return false
	}
	go func() { defer r.work.wg.Done(); fn() }()
	return true
}

func (r *managedREPL) postUI(ctx context.Context, fn func()) bool {
	select {
	case <-r.work.ctx.Done():
		return false
	case <-ctx.Done():
		return false
	case r.uiTasks <- fn:
		return true
	}
}

// closeTabState waits off the event loop for a child's final report write.
func (r *managedREPL) closeTabState(tab *replTab) {
	done, agentDone, state, name := tab.reportWriteDone, tab.agentWriteDone, tab.state, tab.name
	if state == nil {
		return
	}
	r.background(func() {
		if agentDone != nil {
			<-agentDone
		}
		if done != nil {
			<-done
		}
		if err := state.Close(); err != nil {
			r.work.recordError(fmt.Errorf("close %s: %w", name, err))
			r.postUI(r.work.ctx, func() {
				r.model.mu.Lock()
				r.model.appendErrorLine(fmt.Sprintf("close %s: %v", name, err))
				r.model.mu.Unlock()
			})
		}
	})
}

// childSnapshot freezes settings while the model lock is held. The runtime
// and leased session have their own synchronization; caches are copied.
func (s *conversationState) childSnapshot() *conversationState {
	return &conversationState{
		sessionStore: s.sessionStore, session: s.session, settings: s.settings.clone(),
		toolRegistry: s.toolRegistry, skillCatalog: s.skillCatalog, skillRuntime: s.skillRuntime,
		skillSources: append([]string(nil), s.skillSources...), sandboxWarnings: s.sandboxWarnings,
		outputCapabilities: s.outputCapabilities, contextWindows: s.cachedContextWindows(),
	}
}
