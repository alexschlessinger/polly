package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// A turn on a control backend: polly asks the backend's worker for the turn,
// watches its session's root (and its children) from before it starts, and
// tells the turn's UI what the events say, as a turn of its own engine tells
// it through the agent's callbacks. The turn is the backend's: a cancel here
// asks it to stop, and a connection that ends is reached again and caught up
// from the session's committed lines.

// errStoppedElsewhere is a turn another terminal stopped.
var errStoppedElsewhere = errors.New("the turn was stopped from elsewhere")

// runRemoteTurn runs one turn of state's session in its backend and returns
// the exit code its outcome maps to.
func runRemoteTurn(ctx context.Context, config *Config, state *conversationState, userMsg messages.ChatMessage, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI) (exitCode int, finalErr error) {
	r := state.remote
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return 1, errors.New("a turn of this session is already running")
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	settings := state.settings.clone()
	worker, err := r.backend.workerFor(ctx, settings.Model)
	if err != nil {
		return 1, err
	}
	client, err := worker.conn(ctx)
	if err != nil {
		return 1, err
	}
	content, err := control.PromptLine(userMsg, func(ref *artifacts.Ref) ([]byte, error) {
		return readImageArtifact(ctx, state.artifactStore, ref)
	})
	if err != nil {
		return 1, fmt.Errorf("prompt was not added to the conversation: %w", err)
	}

	if turnUI == nil {
		turnUI = newLineTurnUIWithCapabilities(config, inputReader, state.outputCapabilities)
	}
	settled := false
	if line, ok := turnUI.(*lineTurnUI); ok {
		settled = !config.Stream && !line.interactive
		line.settledOutput = settled
	}
	turnUI.Start()
	defer turnUI.Stop()
	state.setTurnUI(turnUI)
	defer state.setTurnUI(nil)

	started := time.Now()
	view := newRemoteTurnView(state, turnUI, settled, config.SchemaPath != "")
	completed := false
	complete := func(reason messages.StopReason, err error) {
		if !completed {
			completed = true
			turnUI.CompleteTurn(turnCompletion{Reason: reason, Err: err, Elapsed: time.Since(started), ProgressSaved: true})
		}
	}
	defer func() {
		if !completed {
			complete(messages.StopReasonError, finalErr)
		}
	}()

	session := r.session
	id := session.ID()
	name, _ := session.GetName(ctx)
	promptReq := control.Prompt{
		Cwd:           worker.boot.Start.Setup.Dir,
		Model:         settings.Model,
		System:        settings.SystemPrompt,
		MaxIterations: uint32(max(settings.MaxIterations, 0)),
		MaxTokens:     uint32(max(settings.MaxTokens, 0)),
		MaxContext:    uint32(max(settings.MaxHistoryTokens, 0)),
		Timeout:       control.Nanos(remoteTurnTimeout),
		ToolTimeout:   control.Nanos(settings.ToolTimeout),
		Effort:        control.Effort(settings.ThinkingEffort),
		Env:           control.PromptEnv(r.backend.environ),
		Content:       content,
	}
	if promptReq.MaxIterations == 0 {
		promptReq.MaxIterations = 1024
	}
	if config.Timeout > 0 {
		promptReq.Timeout = control.Nanos(config.Timeout)
	}
	if settings.Temperature != 0 {
		t := float32(settings.Temperature)
		promptReq.Temperature = &t
	}
	if _, _, allow := backendToolSelection(config.Tools); len(allow) > 0 {
		promptReq.Allow = allow
	}
	if schema != nil {
		raw, err := os.ReadFile(config.SchemaPath)
		if err != nil {
			return 1, fmt.Errorf("failed to read schema: %w", err)
		}
		promptReq.ResponseSchema = json.RawMessage(compactJSON(raw))
	}
	if session.Pending() {
		promptReq.Session = control.Session{New: true, NewID: id, NewName: name}
	} else {
		promptReq.Session = control.Session{ID: id}
	}

	follow := &remoteFollow{worker: worker, client: client, id: id, view: view}
	if err := follow.watch(ctx, false); err != nil {
		return 1, err
	}
	defer follow.close()

	replies := make(chan promptAnswer, 1)
	go func() {
		// The prompt is the backend's once asked: a cancel here stops the
		// turn by `stop`, and the answer is still waited for.
		reply, err := client.Prompt(context.WithoutCancel(ctx), promptReq)
		replies <- promptAnswer{reply, err}
	}()

	reply, err := follow.run(ctx, replies)
	if err != nil {
		if control.IsRefusal(err, control.RefusedBusy) {
			err = fmt.Errorf("session %s has a turn running elsewhere: follow it with --attach %s", name, name)
		}
		return 1, err
	}
	if session.Pending() || reply.Name != "" {
		session.Made(reply.Name)
	}
	if reply.Unkept != "" {
		turnUI.AppendWarning("the session could not be kept: " + reply.Unkept)
	}
	return view.finish(ctx, config, schema, reply, complete, settings.Model, started)
}

func compactJSON(raw []byte) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		data, _ := json.Marshal(string(raw))
		return data
	}
	out, _ := json.Marshal(v)
	return out
}

// remoteFollow follows one root's turn on a worker's connection, and reaches
// the worker again where the connection ends.
type remoteFollow struct {
	worker *remoteWorker
	client *control.Client
	id     control.ID
	view   *remoteTurnView
	sub    *control.Subscription
	// reconnected is set once the connection was reached again: the
	// prompt's own answer is gone with the old one, and the end event is
	// what ends the turn.
	reconnected bool
	// detach makes ctx's end leave the turn running: an attach's Ctrl-C.
	detach bool
	// lastRow is the root's row as the last watch found it.
	lastRow *control.Row
	// reach reaches the worker again after the connection ended; nil uses
	// worker's.
	reach func(ctx context.Context) (*control.Client, error)
	// stopFrom names the turn a stop is for, by where its lines begin: a
	// turn followed from elsewhere, which never stops a later one.
	stopFrom *uint64
}

// errDetached is an attach whose follower let go of the turn.
var errDetached = errors.New("detached; the turn goes on in the backend")

func (f *remoteFollow) watch(ctx context.Context, reply bool) error {
	f.sub = f.client.SubscribeAgents(true, f.id)
	watched, err := f.client.Watch(ctx, control.Watch{Agent: f.id, Kinds: control.AllKinds, Children: true, Reply: reply})
	if err != nil {
		f.sub.Close()
		return err
	}
	f.lastRow = watched.Row
	f.view.watched(watched)
	return nil
}

func (f *remoteFollow) close() {
	if f.sub != nil {
		f.sub.Close()
	}
	if f.client != nil && f.client.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = f.client.Unwatch(ctx, f.id)
		cancel()
	}
}

// stopTurn asks the backend to stop the root's turn, again until it says
// the turn ran, the prompt is answered, or the stop itself fails.
func (f *remoteFollow) stopTurn(done <-chan struct{}) {
	pause := 200 * time.Microsecond
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stopped, err := f.client.Stop(ctx, control.StopReq{Agent: f.id, From: f.stopFrom})
		cancel()
		if err != nil || stopped.Running || f.stopFrom != nil {
			// A stop that names its turn is said once: running false means
			// that turn is over.
			return
		}
		select {
		case <-done:
			return
		case <-time.After(pause):
		}
		pause = min(pause*2, 10*time.Millisecond)
	}
}

// promptAnswer is how a prompt was answered.
type promptAnswer struct {
	reply control.Reply
	err   error
}

// run tells the turn's events until it ends: by the prompt's answer, or, on
// a connection reached again (or an attach, which asked no prompt), by the
// root's end event. ctx's end asks the backend to stop the turn, which is
// then told to its end.
func (f *remoteFollow) run(ctx context.Context, replies <-chan promptAnswer) (control.Reply, error) {
	done := ctx.Done()
	stopDone := make(chan struct{})
	defer close(stopDone)
	for {
		select {
		case a := <-replies:
			replies = nil
			if a.err != nil {
				if f.client.Err() != nil {
					// The connection ended: it is reached again below.
					continue
				}
				return control.Reply{}, a.err
			}
			f.drain()
			// The end may come just after the answer on a hub.
			deadline := time.After(2 * time.Second)
			for !f.view.ended {
				select {
				case <-f.sub.Ready():
					f.drain()
				case <-deadline:
					return a.reply, nil
				case <-f.client.Done():
					f.drain()
					return a.reply, nil
				}
			}
			return a.reply, nil
		case <-f.sub.Ready():
			f.drain()
			if replies == nil && f.view.ended {
				return f.view.endAnswer()
			}
		case <-f.client.Done():
			f.drain()
			if err := f.rejoin(ctx); err != nil {
				return control.Reply{}, err
			}
			// The old connection's answer is gone with it: the end event is
			// what ends the turn now.
			replies = nil
			if f.view.ended {
				return f.view.endAnswer()
			}
		case <-done:
			if f.detach {
				return control.Reply{}, errDetached
			}
			done = nil
			f.view.interrupted = true
			go f.stopTurn(stopDone)
		}
	}
}

// rejoin reaches the worker again after the connection ended during the
// turn, says it fell behind where the daemon said so, and catches up from
// the session's committed lines.
func (f *remoteFollow) rejoin(ctx context.Context) error {
	behind := f.client.Behind()
	if f.sub != nil {
		f.sub.Close()
	}
	reachCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var client *control.Client
	var err error
	reach := f.reach
	if reach == nil {
		reach = f.worker.reconnect
	}
	for attempt := 0; attempt < 5; attempt++ {
		client, err = reach(reachCtx)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("lost the connection to the backend during the turn (it goes on there: follow it with --attach): %w", err)
	}
	if behind {
		f.view.ui.AppendNotice("fell behind the backend; caught up from the session")
	} else {
		f.view.ui.AppendNotice("reconnected to the backend")
	}
	f.client = client
	f.reconnected = true
	committed := f.view.committed
	if err := f.watch(reachCtx, true); err != nil {
		return err
	}
	lines, err := client.HistoryFrom(reachCtx, f.id, committed, nil)
	if err != nil {
		return err
	}
	f.view.catchUp(lines)
	return nil
}

func (f *remoteFollow) drain() {
	for {
		ev, ok := f.sub.TryNext()
		if !ok {
			return
		}
		f.view.event(ev)
	}
}
