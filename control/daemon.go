package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DaemonDirName is the directory, under the user's own runtime directory,
// that a daemon's sockets and locks live in, and DefaultRuntimeDirName the
// default store directory under the home: the reference daemon's own names,
// which every launch of it shares.
const (
	DaemonDirName         = "polly-zig"
	DefaultRuntimeDirName = ".polly-zig"
	RuntimeDirVar         = "POLLYTOOL_RUNTIME_DIR"
	WorkspaceVar          = "POLLYTOOL_WORKSPACE"
)

// RuntimeDir is the daemon's runtime directory as a launch in environ
// resolves it: POLLYTOOL_RUNTIME_DIR when set and absolute, else under the
// home.
func RuntimeDir(environ []string) (string, error) {
	env := environMap(environ)
	if dir := env[RuntimeDirVar]; dir != "" && filepath.IsAbs(dir) {
		return dir, nil
	}
	home := env["HOME"]
	if home == "" {
		return "", fmt.Errorf("HOME is not set; set it, or %s, so sessions have somewhere to live", RuntimeDirVar)
	}
	return filepath.Join(home, DefaultRuntimeDirName), nil
}

// SetupLog is the log a daemon started for build key writes to.
func SetupLog(runtimeDir, buildKey string) string {
	return filepath.Join(runtimeDir, "serve", fmt.Sprintf("%d-%s.log", Protocol, buildKey))
}

// Bootstrap is what a launch needs to reach its setup's worker.
type Bootstrap struct {
	// Target is where the daemon is.
	Target Target
	// Program, Version and BuildKey are the exec:// binary's; empty for a
	// unix:// target.
	Program  Program
	Version  string
	BuildKey string
	// Start is the start line sent when the worker is not there yet; its
	// Setup.Key is the key the worker is joined and greeted by.
	Start Start
	// Environ is the launch's environment, which a daemon it starts is
	// started with, less every provider's key.
	Environ []string
	// Limit bounds how long a launch looks for a daemon whose lock is held.
	Limit time.Duration
	// SocketDir overrides where sockets and locks are (tests).
	SocketDir string
}

// Reached is a greeted connection to a setup's worker.
type Reached struct {
	Client *Client
	// Started reports that this launch started the daemon.
	Started bool
}

// ErrSetupFailed is a setup the daemon could not make; its text says why.
type ErrSetupFailed struct{ Failed MadeFailed }

func (e *ErrSetupFailed) Error() string {
	return "the backend could not make the setup: " + e.Failed.Text
}

// Reach finds the setup's worker, or starts the daemon or the worker, and
// greets it with the setup's key.
func Reach(ctx context.Context, b Bootstrap) (*Reached, error) {
	if b.Limit <= 0 {
		b.Limit = 2 * time.Second
	}
	key := b.Start.Setup.Key
	if !isHexKey(key) {
		return nil, fmt.Errorf("setup key %q is not 32 hex digits", key)
	}
	for looks := 0; ; looks++ {
		client, started, err := connectOrStart(ctx, b)
		if err != nil {
			return nil, err
		}
		var made Made
		if started {
			made, err = withTimeout(ctx, startTimeout, func(ctx context.Context) (Made, error) { return client.Start(ctx, b.Start) })
			if err != nil {
				client.Close()
				return nil, fmt.Errorf("the backend daemon refused its start: %w", err)
			}
		} else {
			made, err = withTimeout(ctx, stepTimeout, func(ctx context.Context) (Made, error) { return client.Join(ctx, JoinReq{Setup: key}) })
			if err != nil {
				client.Close()
				return nil, fmt.Errorf("the backend daemon refused its join: %w", err)
			}
			if made.NeedStart {
				made, err = withTimeout(ctx, startTimeout, func(ctx context.Context) (Made, error) { return client.Start(ctx, b.Start) })
				if err != nil {
					client.Close()
					return nil, fmt.Errorf("the backend daemon refused its start: %w", err)
				}
			}
		}
		switch {
		case made.Failed != nil:
			client.Close()
			return nil, &ErrSetupFailed{Failed: *made.Failed}
		case made.Again:
			client.Close()
			if looks < 2 {
				continue
			}
			return nil, errors.New("the backend daemon was ending each time it was joined")
		case !made.Made:
			client.Close()
			return nil, errors.New("the backend daemon answered with neither made, need_start, failed nor again")
		}
		if _, err := withTimeout(ctx, stepTimeout, func(ctx context.Context) (Welcome, error) { return client.Hello(ctx, key) }); err != nil {
			client.Close()
			return nil, fmt.Errorf("the backend refused hello: %w", err)
		}
		return &Reached{Client: client, Started: started}, nil
	}
}

// Rejoin reaches the setup's worker that is there now, by its key, without
// starting anything: for a launch whose connection ended during a turn.
func Rejoin(ctx context.Context, b Bootstrap) (*Client, error) {
	client, err := dialExisting(ctx, b)
	if err != nil {
		return nil, err
	}
	made, err := client.Join(ctx, JoinReq{Setup: b.Start.Setup.Key, ExistingOnly: true})
	if err != nil {
		client.Close()
		return nil, err
	}
	if !made.Made {
		client.Close()
		return nil, errors.New("the backend has no worker for this setup any more")
	}
	if _, err := client.Hello(ctx, b.Start.Setup.Key); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// JoinSession reaches the worker that holds session, whatever its setup:
// for an attach or a stop from another terminal. It starts nothing.
func JoinSession(ctx context.Context, b Bootstrap, session ID) (*Client, error) {
	client, err := dialExisting(ctx, b)
	if err != nil {
		return nil, err
	}
	made, err := client.Join(ctx, JoinReq{Session: session, ExistingOnly: true})
	if err != nil {
		client.Close()
		return nil, err
	}
	if !made.Made || made.Setup == "" {
		client.Close()
		return nil, errNoWorker
	}
	if _, err := client.Hello(ctx, made.Setup); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

var errNoWorker = errors.New("no backend worker runs that session")

// How long a step of reaching a worker may take: a start makes the setup's
// engine, which loads its MCP servers and skills.
const (
	stepTimeout  = 30 * time.Second
	startTimeout = 2 * time.Minute
)

func withTimeout[T any](ctx context.Context, d time.Duration, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	v, err := f(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		var zero T
		return zero, fmt.Errorf("the backend did not answer within %s", d)
	}
	return v, err
}

// ErrNoWorker reports that no worker holds a session.
func ErrNoWorker(err error) bool { return errors.Is(err, errNoWorker) }

func isHexKey(key string) bool { return IsID(strings.ToLower(key)) && key == strings.ToLower(key) }
