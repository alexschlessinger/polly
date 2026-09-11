package main

import (
	"context"
	"errors"
	"slices"
)

func (c *replCommandContext) maintainSwarm(label, success string, run func(context.Context) error) error {
	if c.swarmMaintenance != nil {
		return c.swarmMaintenance(label, success, run)
	}
	if err := run(c.operationContext()); err != nil {
		return err
	}
	return c.replyLine(success)
}

// Called on the event loop under the originating model's lock. Neither the
// maintenance wait nor its filesystem operations may run under that lock.
func (r *managedREPL) startSwarmMaintenance(label, success string, run func(context.Context) error) error {
	m, tab := r.model, r.visibleTab()
	parent := context.Background()
	if r.state != nil && r.state.session != nil {
		parent = r.state.session.Context()
	}
	if !r.background(func() {
		ctx, cancel := context.WithCancel(parent)
		stop := context.AfterFunc(r.work.ctx, cancel)
		defer cancel()
		defer stop()
		err := run(ctx)
		r.postUI(r.work.ctx, func() {
			if tab != nil && !slices.Contains(r.tabs, tab) {
				return
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if err != nil {
				m.appendErrorLine(label + ": " + err.Error())
			} else {
				m.appendNoticeLine(success)
			}
		})
	}) {
		return errors.New("workspace is closing")
	}
	m.appendNoticeLine(label + " started")
	return nil
}
