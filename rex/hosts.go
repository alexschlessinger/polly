package rex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// HostEntry describes a remote Rex host known to the server.
type HostEntry struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Endpoints []string `json:"endpoints,omitempty"`
	HostID    string   `json:"host_id,omitempty"`
	AddedFrom string   `json:"added_from,omitempty"`
	Source    string   `json:"source,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// HostDiagnostic is a configuration warning or error reported by hosts.list.
type HostDiagnostic struct {
	Severity string `json:"severity,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message,omitempty"`
}

// HostsList is returned by hosts.list.
type HostsList struct {
	Hosts       []HostEntry      `json:"hosts"`
	Hash        string           `json:"hash,omitempty"`
	Diagnostics []HostDiagnostic `json:"diagnostics,omitempty"`
}

// HostAttempt records one endpoint probe in "rex hosts check --json".
type HostAttempt struct {
	Endpoint string `json:"endpoint"`
	Error    string `json:"error,omitempty"`
}

// HostCheckEntry reports reachability for one host in "rex hosts check --json".
type HostCheckEntry struct {
	ID       string        `json:"id,omitempty"`
	Label    string        `json:"label"`
	Endpoint string        `json:"endpoint,omitempty"`
	Status   string        `json:"status"`
	Attempts []HostAttempt `json:"attempts,omitempty"`
}

// ListHosts lists remote hosts configured on the connected Rex server.
func (c *Client) ListHosts(ctx context.Context) (*HostsList, error) {
	var out HostsList
	if err := c.CallSession(ctx, "", "hosts.list", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckHosts probes every configured host via "rex hosts check --json".
func (c *Client) CheckHosts(ctx context.Context) ([]HostCheckEntry, error) {
	var out struct {
		Hosts []HostCheckEntry `json:"hosts"`
	}
	if err := c.runJSON(ctx, []string{"hosts", "check", "--json"}, nil, "hosts.check", &out); err != nil {
		return nil, err
	}
	return out.Hosts, nil
}

// ForwardEntry represents a tunnel block returned by "rex forward ls --json".
type ForwardEntry struct {
	BlockID   string          `json:"block_id,omitempty"`
	ID        string          `json:"id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Direction string          `json:"direction,omitempty"`
	Listen    string          `json:"listen,omitempty"`
	Target    string          `json:"target,omitempty"`
	State     string          `json:"state,omitempty"`
	Extra     json.RawMessage `json:"-"`
}

// ListForwards lists port forwards in session (or across all sessions when all is true).
func (c *Client) ListForwards(ctx context.Context, session string, all bool) ([]ForwardEntry, error) {
	subArgs := []string{"forward", "ls", "--json"}
	if all {
		subArgs = append(subArgs, "--all")
	} else if sid := c.resolveSession(session); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	var out struct {
		Forwards []ForwardEntry `json:"forwards"`
	}
	if err := c.runJSON(ctx, subArgs, nil, "forward.ls", &out); err != nil {
		return nil, err
	}
	return out.Forwards, nil
}

// CloseForwardsRequest configures "rex forward close".
type CloseForwardsRequest struct {
	SessionID string
	IDs       []string
	Orphaned  bool
	All       bool
}

// CloseForwards closes specific, orphaned, or all tunnel blocks in a session.
func (c *Client) CloseForwards(ctx context.Context, req CloseForwardsRequest) error {
	subArgs := []string{"forward", "close"}
	if sid := c.resolveSession(req.SessionID); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	if req.Orphaned {
		subArgs = append(subArgs, "--orphaned")
	}
	if req.All {
		subArgs = append(subArgs, "--all")
	}
	subArgs = append(subArgs, req.IDs...)
	_, err := c.runRaw(ctx, subArgs, nil, "forward.close")
	return err
}

// OpenForwardOptions configures "rex forward open --no-open".
type OpenForwardOptions struct {
	SessionID string
	Bind      string
	Port      int
	Target    string
}

// ShareForwardOptions configures "rex forward share".
type ShareForwardOptions struct {
	SessionID string
	Bind      string
	Port      int
	Target    string
}

// NewForwardOptions configures "rex forward new" with one or more -L / -R specs.
type NewForwardOptions struct {
	SessionID string
	Local     []string
	Remote    []string
}

// Forward represents a running port-forwarding process managed by Rex.
// Call Close to terminate the forward and wait for its subprocess to exit.
type Forward struct {
	address string
	proc    StreamProcess
	cancel  context.CancelFunc

	done   chan struct{}
	mu     sync.Mutex
	waitEr error
}

// Address returns the printed listen URL or host:port for forwards started via
// OpenForward or ShareForward. It is empty for NewForward.
func (f *Forward) Address() string { return f.address }

// Wait blocks until the underlying forward process exits.
func (f *Forward) Wait() error {
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitEr
}

// Close stops the forward subprocess and waits for it to exit.
func (f *Forward) Close() error {
	if f == nil {
		return nil
	}
	if f.cancel != nil {
		f.cancel()
	}
	var killErr error
	if f.proc != nil {
		killErr = f.proc.Kill()
	}
	<-f.done
	f.mu.Lock()
	waitErr := f.waitEr
	f.mu.Unlock()
	if killErr != nil {
		return killErr
	}
	if errors.Is(waitErr, context.Canceled) {
		return nil
	}
	return nil
}

// OpenForward starts a local-to-server port forward ("rex forward open --no-open")
// and waits until Rex prints the listening URL before returning the Forward handle.
func (c *Client) OpenForward(ctx context.Context, opts OpenForwardOptions) (*Forward, error) {
	if strings.TrimSpace(opts.Target) == "" {
		return nil, fmt.Errorf("rex forward open: target is required")
	}
	subArgs := []string{"forward", "open", "--no-open"}
	if sid := c.resolveSession(opts.SessionID); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	if opts.Bind != "" {
		subArgs = append(subArgs, "--bind", opts.Bind)
	}
	if opts.Port > 0 {
		subArgs = append(subArgs, "--port", strconv.Itoa(opts.Port))
	}
	subArgs = append(subArgs, opts.Target)
	return c.startForward(ctx, subArgs, true)
}

// ShareForward starts a server-to-local port forward ("rex forward share") and
// waits until Rex prints the remote listening address before returning.
func (c *Client) ShareForward(ctx context.Context, opts ShareForwardOptions) (*Forward, error) {
	if strings.TrimSpace(opts.Target) == "" {
		return nil, fmt.Errorf("rex forward share: target is required")
	}
	subArgs := []string{"forward", "share"}
	if sid := c.resolveSession(opts.SessionID); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	if opts.Bind != "" {
		subArgs = append(subArgs, "--bind", opts.Bind)
	}
	if opts.Port > 0 {
		subArgs = append(subArgs, "--port", strconv.Itoa(opts.Port))
	}
	subArgs = append(subArgs, opts.Target)
	return c.startForward(ctx, subArgs, true)
}

// NewForward starts "rex forward new" with arbitrary -L and -R forward specs.
func (c *Client) NewForward(ctx context.Context, opts NewForwardOptions) (*Forward, error) {
	if len(opts.Local) == 0 && len(opts.Remote) == 0 {
		return nil, fmt.Errorf("rex forward new: at least one local or remote spec is required")
	}
	subArgs := []string{"forward", "new"}
	if sid := c.resolveSession(opts.SessionID); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	for _, spec := range opts.Local {
		subArgs = append(subArgs, "-L", spec)
	}
	for _, spec := range opts.Remote {
		subArgs = append(subArgs, "-R", spec)
	}
	return c.startForward(ctx, subArgs, false)
}

func (c *Client) startForward(ctx context.Context, subArgs []string, expectAddress bool) (*Forward, error) {
	runCtx, cancel := context.WithCancel(ctx)
	proc, err := c.startStream(runCtx, subArgs, nil)
	if err != nil {
		cancel()
		return nil, err
	}

	var stderrBuf strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if se := proc.Stderr(); se != nil {
			_, _ = io.Copy(&stderrBuf, se)
		}
	}()

	var address string
	if expectAddress {
		lineCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			r := bufio.NewReader(proc.Stdout())
			line, readErr := r.ReadString('\n')
			if readErr != nil && strings.TrimSpace(line) == "" {
				errCh <- readErr
				return
			}
			lineCh <- strings.TrimSpace(line)
			_, _ = io.Copy(io.Discard, r)
		}()

		select {
		case addr := <-lineCh:
			address = addr
		case readErr := <-errCh:
			_ = proc.Kill()
			_ = proc.Wait()
			<-stderrDone
			cancel()
			if msg := cleanErrorMessage(stderrBuf.String()); msg != "" {
				return nil, fmt.Errorf("rex %s: %s", strings.Join(subArgs[:2], " "), msg)
			}
			return nil, fmt.Errorf("rex %s: exited before publishing listen address: %w", strings.Join(subArgs[:2], " "), readErr)
		case <-ctx.Done():
			_ = proc.Kill()
			_ = proc.Wait()
			<-stderrDone
			cancel()
			return nil, ctx.Err()
		}
	} else {
		go func() {
			if so := proc.Stdout(); so != nil {
				_, _ = io.Copy(io.Discard, so)
			}
		}()
	}

	fwd := &Forward{
		address: address,
		proc:    proc,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go func() {
		err := proc.Wait()
		<-stderrDone
		fwd.mu.Lock()
		fwd.waitEr = err
		fwd.mu.Unlock()
		close(fwd.done)
	}()
	return fwd, nil
}
