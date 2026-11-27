package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// runAttachOrStop serves --attach and --stop on a control backend: it finds
// the worker that runs the session's turn, whatever its setup, and follows
// that turn here, or stops it, as Ctrl-C in its own launch would.
func (r *commandRunner) runAttachOrStop() error {
	b := r.config.Backend
	name := b.AttachName
	if b.Stop {
		name = b.StopOf
	}
	if name == "" && r.contextID != "" {
		name = r.contextID
	}
	ctx := r.ctx
	if name == "" {
		last, err := r.backend.store.GetLast(ctx)
		if err != nil {
			return err
		}
		if last == "" {
			return errors.New("no session to follow: name one")
		}
		name = last
	}
	signalCtx, cancel := setupSignalHandling(ctx)
	defer cancel()
	if b.Stop {
		return r.backend.stopSession(ctx, name, os.Stdout)
	}
	return r.backend.attachSession(signalCtx, r.config, &r.conversationOpener, name)
}

// sessionBoot is a bootstrap that reaches the backend's daemon, whatever
// setup holds a session there.
func (b *remoteBackend) sessionBoot(ctx context.Context) (control.Bootstrap, error) {
	w, err := b.workerFor(ctx, b.config.Launch.Model)
	if err != nil {
		return control.Bootstrap{}, err
	}
	return w.boot, nil
}

// joinRunning reaches the worker that runs session id's turn: nil when no
// worker runs it.
func (b *remoteBackend) joinRunning(ctx context.Context, id control.ID) (*control.Client, error) {
	boot, err := b.sessionBoot(ctx)
	if err != nil {
		return nil, err
	}
	c, err := control.JoinSession(ctx, boot, id)
	if control.ErrNoWorker(err) {
		return nil, nil
	}
	return c, err
}

// stopSession stops the running turn of the session named name, by where
// its lines begin, so that a later turn is never stopped in its place.
func (b *remoteBackend) stopSession(ctx context.Context, name string, out *os.File) error {
	md, err := b.store.GetMetadata(ctx, name)
	if err != nil {
		return err
	}
	view, err := b.store.ReadView(ctx, sessions.ViewTarget{Name: md.Name}, "")
	if err != nil {
		return err
	}
	c, err := b.joinRunning(ctx, view.ID)
	if err != nil {
		return err
	}
	if c == nil {
		return &exitError{code: 1, err: fmt.Errorf("no turn of session %s runs", md.Name)}
	}
	defer c.Close()
	watched, err := c.Watch(ctx, control.Watch{Agent: view.ID, Kinds: []string{control.KindEnd}})
	if err != nil {
		return err
	}
	defer c.Unwatch(context.WithoutCancel(ctx), view.ID)
	if watched.Row == nil || !watched.Row.Running() || watched.Row.From == nil {
		return &exitError{code: 1, err: fmt.Errorf("no turn of session %s runs", md.Name)}
	}
	stopped, err := c.Stop(ctx, control.StopReq{Agent: view.ID, From: watched.Row.From})
	if err != nil {
		return err
	}
	if !stopped.Running {
		return &exitError{code: 1, err: fmt.Errorf("no turn of session %s runs", md.Name)}
	}
	fmt.Fprintf(out, "stopped the running turn of %s\n", md.Name)
	return nil
}

// attachSession follows the running turn of the session named name here:
// its own lines from where the turn began, then what it tells, to its end.
// Ctrl-C detaches and leaves the turn running. A session at rest says how
// its last turn ended.
func (b *remoteBackend) attachSession(ctx context.Context, config *Config, o *conversationOpener, name string) error {
	settings := config.Launch.clone()
	state, err := b.openRemoteState(ctx, o, name, settings, false)
	if err != nil {
		return err
	}
	defer state.Close()
	id := state.remote.session.ID()
	c, err := b.joinRunning(ctx, id)
	if err != nil {
		return err
	}
	if c == nil {
		info, err := b.store.ReadView(ctx, sessions.ViewTarget{Name: name}, "")
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "no turn of %s runs; its last of %d lines ended %s\n", name, info.Sequence, info.Metadata.LastUsed.Format(time.DateTime))
		return nil
	}
	defer c.Close()
	ui := newLineTurnUIWithCapabilities(config, nil, state.outputCapabilities)
	ui.interactive = true
	view := newRemoteTurnView(state, ui, false, false)
	follow := &remoteFollow{client: c, id: id, view: view, detach: true, reach: func(ctx context.Context) (*control.Client, error) {
		c, err := b.joinRunning(ctx, id)
		if err == nil && c == nil {
			err = errors.New("the turn's worker is gone")
		}
		return c, err
	}}
	ui.Start()
	defer ui.Stop()
	if err := follow.watch(ctx, true); err != nil {
		return err
	}
	defer follow.close()
	// The turn's own lines so far, from where it began.
	var from uint64
	if row := follow.lastRow; row != nil && row.From != nil {
		from = *row.From
	}
	lines, err := c.HistoryFrom(ctx, id, from, nil)
	if err != nil {
		return err
	}
	view.committed = from
	view.catchUp(lines)
	started := time.Now()
	var reply control.Reply
	if view.ended {
		// The turn ended between the join and the watch.
		reply, err = view.endAnswer()
	} else {
		reply, err = follow.run(ctx, nil)
	}
	if errors.Is(err, errDetached) {
		fmt.Fprintln(os.Stderr, "\ndetached: the turn goes on in the backend")
		return nil
	}
	if err != nil {
		return err
	}
	complete := func(reason messages.StopReason, err error) {
		ui.CompleteTurn(turnCompletion{Reason: reason, Err: err, Elapsed: time.Since(started), ProgressSaved: true})
	}
	code, err := view.finish(ctx, config, nil, reply, complete, settings.Model, started)
	if code != 0 {
		return &exitError{code: code, err: err}
	}
	return err
}
