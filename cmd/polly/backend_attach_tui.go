package main

import (
	"context"
	"errors"
	"time"

	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/messages"
)

// A backend session whose turn runs in another terminal opens in the TUI to
// follow that turn: the tab's transcript ends where the turn began, and the
// turn's own lines and what it tells from then are the tab's turn, as if it
// had been asked here. Esc stops it, by where its lines begin, so that a
// later turn is never stopped in its place; once it ends the composer is the
// tab's as any other's.

// remoteAttach is a turn that runs elsewhere, found as its session opened:
// the connection to the worker that runs it, where its lines begin, and the
// prompt it began with.
type remoteAttach struct {
	client *control.Client
	from   uint64
	prompt messages.ChatMessage
}

// runningTurn finds the turn that runs the session now, if one does, and
// hides its lines from the session's history until it is followed.
func (b *remoteBackend) runningTurn(ctx context.Context, sess *control.RPCSession) (*remoteAttach, error) {
	inUse, err := b.store.InUse(ctx, sess.ID())
	if err != nil || !inUse {
		return nil, err
	}
	c, err := b.joinRunning(ctx, sess.ID())
	if err != nil || c == nil {
		return nil, err
	}
	watched, err := c.Watch(ctx, control.Watch{Agent: sess.ID(), Kinds: []string{control.KindEnd}})
	if err == nil {
		_ = c.Unwatch(ctx, sess.ID())
	}
	if err != nil || watched.Row == nil || !watched.Row.Running() || watched.Row.From == nil {
		c.Close()
		return nil, err
	}
	from := *watched.Row.From
	upto := from + 1
	lines, err := c.HistoryFrom(ctx, sess.ID(), from, &upto)
	if err != nil {
		c.Close()
		return nil, err
	}
	a := &remoteAttach{client: c, from: from}
	if len(lines) == 1 {
		if msg, err := control.LineToMessage(lines[0], sess.ID(), b.store.Artifacts(sess.ID())); err == nil {
			a.prompt = msg
		}
	}
	sess.HideFrom(from)
	return a, nil
}

// followRemoteTurnOn starts following the turn that ran elsewhere as tab's
// session opened, as the tab's turn.
func (r *managedREPL) followRemoteTurnOn(tab *replTab) {
	if tab.state == nil || tab.state.remote == nil || tab.state.remote.attach == nil || tab.model != r.model {
		return
	}
	a := tab.state.remote.attach
	display := a.prompt.GetContent()
	if display == "" {
		display = "(a turn running elsewhere)"
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if err := r.submitManagedTurnLocked(managedTurnInput{displayText: display, userMessage: a.prompt, attach: true}); err != nil {
		r.model.appendNoticeLine("could not follow the running turn: " + err.Error())
	}
}

// followRemoteTurn follows the session's turn that runs elsewhere to its end
// in turnUI. ctx's end stops the turn.
func followRemoteTurn(ctx context.Context, config *Config, state *conversationState, turnUI TurnUI) (err error) {
	r := state.remote
	a := r.attach
	r.attach = nil
	if a == nil {
		return errors.New("no turn to follow")
	}
	defer a.client.Close()
	defer r.session.ShowAll()
	turnUI.Start()
	defer turnUI.Stop()
	state.setTurnUI(turnUI)
	defer state.setTurnUI(nil)
	started := time.Now()
	completed := false
	complete := func(reason messages.StopReason, err error) {
		if !completed {
			completed = true
			turnUI.CompleteTurn(turnCompletion{Reason: reason, Err: err, Elapsed: time.Since(started), ProgressSaved: true})
		}
	}
	defer func() {
		if !completed {
			complete(messages.StopReasonError, err)
		}
	}()
	id := r.session.ID()
	view := newRemoteTurnView(state, turnUI, false, false)
	from := a.from
	follow := &remoteFollow{client: a.client, id: id, view: view, stopFrom: &from, reach: func(ctx context.Context) (*control.Client, error) {
		c, err := r.backend.joinRunning(ctx, id)
		if err == nil && c == nil {
			err = errors.New("the turn's worker is gone")
		}
		return c, err
	}}
	if err := follow.watch(ctx, true); err != nil {
		return err
	}
	defer follow.close()
	lines, err := a.client.HistoryFrom(ctx, id, a.from+1, nil)
	if err != nil {
		return err
	}
	view.committed = a.from + 1
	view.catchUp(lines)
	var reply control.Reply
	if view.ended {
		reply, err = view.endAnswer()
	} else {
		reply, err = follow.run(ctx, nil)
	}
	if err != nil {
		return err
	}
	_, err = view.finish(ctx, config, nil, reply, complete, state.settings.Model, started)
	if errors.Is(err, errStoppedElsewhere) && view.interrupted {
		err = context.Cause(ctx)
	}
	return err
}
