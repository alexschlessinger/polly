package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/urfave/cli/v3"
)

// remoteBackend is a run's control backend: the workers its sessions' turns
// run in, one for each setup (a provider and what the launch loads), and the
// store every session is read through. All of it crosses the backend's
// socket; nothing of the backend's files is read here.
type remoteBackend struct {
	target  control.Target
	config  *Config
	environ []string
	cwd     string

	mu      sync.Mutex
	workers map[string]*remoteWorker
	// store reads sessions through the worker of the launch's own model.
	store  *control.RPCStore
	closed bool
}

// remoteWorker is the connection to one setup's worker, reached on first use
// and again after its connection ends.
type remoteWorker struct {
	backend *remoteBackend
	boot    control.Bootstrap

	mu     sync.Mutex
	client *control.Client
}

func newRemoteBackend(config *Config) (*remoteBackend, error) {
	target, err := control.ParseTarget(config.Backend.URL)
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve the working directory: %w", err)
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	b := &remoteBackend{target: target, config: config, environ: os.Environ(), cwd: cwd, workers: map[string]*remoteWorker{}}
	b.store = control.NewRPCStore(b.storeClient)
	return b, nil
}

func providerOf(model string) string {
	provider, _, _ := strings.Cut(model, "/")
	return strings.ToLower(provider)
}

func (b *remoteBackend) launchOptions(provider string) control.LaunchOptions {
	servers, scripts, _ := backendToolSelection(b.config.Tools)
	return control.LaunchOptions{
		Target:    b.target,
		Provider:  provider,
		BaseURL:   b.config.BaseURL,
		Cwd:       b.cwd,
		Environ:   b.environ,
		SkillDirs: b.config.Launch.SkillDirs,
		Workspace: b.config.Backend.Workspace,
		Servers:   servers,
		Scripts:   scripts,
	}
}

// workerFor is the worker that runs model's turns: one setup per provider,
// so a /model switch to another provider reaches a second worker on the
// same daemon.
func (b *remoteBackend) workerFor(ctx context.Context, model string) (*remoteWorker, error) {
	if err := validateBackendModel(model); err != nil {
		return nil, err
	}
	boot, err := control.Prepare(ctx, b.launchOptions(providerOf(model)))
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("the control backend is closed")
	}
	key := boot.Start.Setup.Key
	if w := b.workers[key]; w != nil {
		return w, nil
	}
	w := &remoteWorker{backend: b, boot: boot}
	b.workers[key] = w
	return w, nil
}

// conn is the worker's connection, reached where there is none and reached
// again where the last one ended.
func (w *remoteWorker) conn(ctx context.Context) (*control.Client, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil && w.client.Err() == nil {
		return w.client, nil
	}
	if w.client != nil {
		// The worker was there: look for it again, and start it again only
		// where it has gone.
		if c, err := control.Rejoin(ctx, w.boot); err == nil {
			w.client = c
			return c, nil
		}
	}
	reached, err := control.Reach(ctx, w.boot)
	if err != nil {
		return nil, err
	}
	w.client = reached.Client
	return w.client, nil
}

// reconnect drops the worker's connection and reaches its worker again,
// starting nothing: for a turn whose connection ended while it ran.
func (w *remoteWorker) reconnect(ctx context.Context) (*control.Client, error) {
	w.backend.mu.Lock()
	closed := w.backend.closed
	w.backend.mu.Unlock()
	if closed {
		return nil, errors.New("the control backend is closed")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil && w.client.Err() == nil {
		return w.client, nil
	}
	c, err := control.Rejoin(ctx, w.boot)
	if err != nil {
		return nil, err
	}
	w.client = c
	return c, nil
}

func (w *remoteWorker) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil {
		_ = w.client.Close()
		w.client = nil
	}
}

// storeClient is the connection store reads go through: any worker that is
// connected, else the worker of the launch's model.
func (b *remoteBackend) storeClient(ctx context.Context) (*control.Client, error) {
	b.mu.Lock()
	for _, w := range b.workers {
		w.mu.Lock()
		c := w.client
		w.mu.Unlock()
		if c != nil && c.Err() == nil {
			b.mu.Unlock()
			return c, nil
		}
	}
	b.mu.Unlock()
	w, err := b.workerFor(ctx, b.config.Launch.Model)
	if err != nil {
		return nil, err
	}
	return w.conn(ctx)
}

// Close lets every connection go. Turns that run go on in the backend.
func (b *remoteBackend) Close() error {
	b.mu.Lock()
	workers := make([]*remoteWorker, 0, len(b.workers))
	for _, w := range b.workers {
		workers = append(workers, w)
	}
	b.closed = true
	b.mu.Unlock()
	for _, w := range workers {
		w.close()
	}
	return nil
}

// remoteSession is what a conversation state on a control backend holds
// beside polly's own: the backend, and the session as it knows it.
type remoteSession struct {
	backend *remoteBackend
	session *control.RPCSession
	// mu guards the turn in progress; only one runs per session.
	mu      sync.Mutex
	running bool
	// attach is a turn that ran elsewhere as the session was opened, which
	// the tab follows as its first turn (backend_attach_tui.go).
	attach *remoteAttach
}

// openRemoteState opens contextID on the backend: a conversation state with
// no agent of its own, whose turns run in the backend.
func (b *remoteBackend) openRemoteState(ctx context.Context, o *conversationOpener, contextID string, settings Settings, auto bool) (*conversationState, error) {
	if err := validateBackendModel(settings.Model); err != nil {
		return nil, err
	}
	sess, err := b.store.Acquire(ctx, contextID, sessions.AcquireOptions{Auto: auto})
	if errors.Is(err, sessions.ErrSessionExists) && auto {
		sess, err = b.store.Acquire(ctx, contextID, sessions.AcquireOptions{})
	}
	if err != nil {
		return nil, err
	}
	rpc, ok := sess.(*control.RPCSession)
	if !ok {
		return nil, errors.New("the control backend opened a session it does not serve")
	}
	md, err := sess.GetMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("read session metadata: %w", err)
	}
	if md.Parent != "" {
		return nil, fmt.Errorf("session %s is an agent of %s: it is inspected from its parent's tab and runs from its parent's turns", md.Name, md.Parent)
	}
	// What this process runs the session with lives here, over the record.
	applySettingsToMetadata(md, &settings)
	_ = sess.SetMetadata(ctx, md)
	var attach *remoteAttach
	if o.followRunning && !rpc.Pending() {
		if attach, err = b.runningTurn(ctx, rpc); err != nil {
			return nil, err
		}
	}
	state := &conversationState{
		sessionStore:       b.store,
		session:            sess,
		settings:           settings,
		metadataBaseURL:    o.config.BaseURL,
		artifactStore:      sess.ArtifactStore(),
		sandboxWarnings:    newBroadWritablePathWarner(),
		displayContract:    o.displayContract,
		outputCapabilities: o.outputCapabilities,
		toolRegistry:       tools.NewToolRegistry(nil),
		remote:             &remoteSession{backend: b, session: rpc, attach: attach},
	}
	return state, nil
}

// prepareRemote resolves the settings an existing session of the backend
// runs with: the launch's, with what this process set on it before (/set,
// /model), and the model its record names where no flag names one. The
// backend's record holds no settings of polly's: a prompt carries them.
func (b *remoteBackend) prepareRemote(ctx context.Context, contextID string, settings Settings, cmd *cli.Command) (string, Settings, error) {
	md, err := b.store.GetMetadata(ctx, contextID)
	if errors.Is(err, sessions.ErrSessionNotFound) {
		return contextID, settings, nil
	}
	if err != nil {
		return "", Settings{}, fmt.Errorf("read context metadata: %w", err)
	}
	if b.store.Kept(md.Name) {
		for _, spec := range settingSpecs {
			if spec.flagged() && !spec.flagSet(cmd) && spec.fromMeta != nil {
				spec.fromMeta(&settings, md)
			}
		}
	} else if md.Model != "" && !flagGiven(cmd, "model") && validateBackendModel(md.Model) == nil {
		settings.Model = md.Model
	}
	return md.Name, settings, nil
}

// applySettingsToMetadata records settings in md as polly's store keeps them.
func applySettingsToMetadata(md *sessions.Metadata, settings *Settings) {
	for _, spec := range settingSpecs {
		if spec.toMeta != nil {
			spec.toMeta(settings, md)
		}
	}
}

// remoteTurnTimeout is a turn's stream-stall timeout when the launch names
// none.
const remoteTurnTimeout = 30 * time.Minute

// mastheadLine says, where a launch shows its sandbox, that turns run in the
// backend and which one.
func (b *remoteBackend) mastheadLine() string {
	name := b.target.Socket
	if b.target.Scheme == control.SchemeExec {
		name = filepath.Base(b.target.Binary)
	}
	b.mu.Lock()
	for _, w := range b.workers {
		w.mu.Lock()
		if w.client != nil && w.client.Welcome.Version != "" {
			version, _, _ := strings.Cut(w.client.Welcome.Version, " (")
			name += " " + version
		}
		w.mu.Unlock()
		break
	}
	b.mu.Unlock()
	line := "Backend " + name
	if ws := b.config.Backend.Workspace; ws != "" && ws != "local" {
		line += " · tools on " + ws
	}
	return line + " · its tools run outside polly's sandbox"
}

// mastheadPosture is the line the masthead says where tools run: the
// sandbox's posture, or, for a backend's session, the backend.
func mastheadPosture(config *Config, state *conversationState) string {
	if state != nil && state.remote != nil {
		return state.remote.backend.mastheadLine()
	}
	return currentSandboxPosture(config, state).summaryLine(false)
}
