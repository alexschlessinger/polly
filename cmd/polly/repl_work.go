package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// replWork owns off-screen I/O. Shutdown cancels reads and pending launches,
// then waits for their cleanup before closing sessions.
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

// closeTabState retires a display and closes its runtime off the event loop.
// Runtime.Close drains delegated execution and durable apply receipts before
// conversationState.Close releases the parent lease.
func (r *managedREPL) closeTabState(tab *replTab) {
	cacheView := r.retiredChildViewTask(tab)
	activity := tab.agentActivity
	var owner *replModel
	if tab.parent != nil {
		owner = tab.parent.model
	}
	state, name := tab.state, tab.name
	if state == nil {
		return
	}
	r.background(func() {
		var view *cachedChildView
		if cacheView != nil {
			view = cacheView()
		}
		if err := state.Close(); err != nil {
			r.work.recordError(fmt.Errorf("close %s: %w", name, err))
			r.postUI(r.work.ctx, func() {
				r.model.mu.Lock()
				r.model.appendErrorLine(fmt.Sprintf("close %s: %v", name, err))
				r.model.mu.Unlock()
			})
		}
		if view != nil {
			r.postUI(r.work.ctx, func() {
				if activity != nil && owner != nil {
					owner.mu.Lock()
					activity.viewID = view.info.ID
					owner.mu.Unlock()
				}
				r.admitChildDisplay(view)
			})
		}
	})
}
