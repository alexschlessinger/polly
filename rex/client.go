// Package rex provides a programmatic Go client for the Rex terminal
// multiplexer via Rex's official CLI JSON RPC, Lua scripting, event streaming,
// and port forwarding surfaces.
package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors returned or wrapped by Client operations.
var (
	ErrBinaryNotFound        = errors.New("rex binary not found")
	ErrRemoteControlDisabled = errors.New("rex app remote control disabled")
	ErrNotFound              = errors.New("rex resource not found")
	ErrBadRequest            = errors.New("rex bad request")
	ErrConflict              = errors.New("rex conflict")
	ErrUnreachable           = errors.New("rex server unreachable")
)

// CommandSpec describes a single rex CLI invocation.
type CommandSpec struct {
	Binary string
	Args   []string
	Stdin  []byte
	Env    map[string]string
	Dir    string
}

// CommandResult holds the captured output of a completed rex CLI command.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// StreamProcess represents a long-lived rex CLI subprocess such as "rex events"
// or "rex forward". Callers consume Stdout/Stderr and finish with Wait or Kill.
type StreamProcess interface {
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() error
	Kill() error
}

// CommandRunner executes rex CLI commands. The default runner uses os/exec;
// callers such as Polly's tool registry can supply a sandbox-backed runner.
type CommandRunner interface {
	Run(ctx context.Context, spec CommandSpec) (CommandResult, error)
	Start(ctx context.Context, spec CommandSpec) (StreamProcess, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, spec CommandSpec) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), spec.Env)
	}
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := CommandResult{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return res, fmt.Errorf("%w: %w", ErrBinaryNotFound, err)
		}
		return res, err
	}
	return res, nil
}

func (execRunner) Start(ctx context.Context, spec CommandSpec) (StreamProcess, error) {
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), spec.Env)
	}
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rex stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("rex stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %w", ErrBinaryNotFound, err)
		}
		return nil, err
	}
	return &execStreamProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type execStreamProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (p *execStreamProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execStreamProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *execStreamProcess) Wait() error           { return p.cmd.Wait() }
func (p *execStreamProcess) Kill() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, overridden := extra[k]; overridden {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// APIError captures a structured failure returned by the rex CLI or server.
type APIError struct {
	Command  []string
	Method   string
	ExitCode int
	Status   int
	Code     string
	Message  string
	Stderr   string
	Err      error
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("rex")
	if e.Method != "" {
		b.WriteString(" ")
		b.WriteString(e.Method)
	} else if len(e.Command) > 0 {
		b.WriteString(" ")
		b.WriteString(strings.Join(e.Command, " "))
	}
	b.WriteString(": ")
	if e.Message != "" {
		b.WriteString(e.Message)
	} else if e.Err != nil {
		b.WriteString(e.Err.Error())
	} else {
		fmt.Fprintf(&b, "exit status %d", e.ExitCode)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " (code=%s", e.Code)
		if e.Status != 0 {
			fmt.Fprintf(&b, " status=%d", e.Status)
		}
		b.WriteByte(')')
	}
	return b.String()
}

func (e *APIError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	switch e.Code {
	case "not_found", "session_not_found", "window_not_found", "block_not_found":
		return ErrNotFound
	case "bad_request":
		return ErrBadRequest
	case "conflict":
		return ErrConflict
	}
	lower := strings.ToLower(e.Message)
	switch {
	case strings.Contains(lower, "not accepting actions") || strings.Contains(lower, "remote control"):
		return ErrRemoteControlDisabled
	case strings.Contains(lower, "not found"):
		return ErrNotFound
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "unreachable") || strings.Contains(lower, "no such file or directory") && strings.Contains(lower, "sock"):
		return ErrUnreachable
	case e.ExitCode == 3:
		return ErrNotFound
	}
	return nil
}

var statusCodeRe = regexp.MustCompile(`\(status=(\d+)\s+code=([a-z0-9_]+)\)`)

func parseAPIError(command []string, method string, res CommandResult) *APIError {
	raw := strings.TrimSpace(string(res.Stderr))
	if raw == "" {
		raw = strings.TrimSpace(string(res.Stdout))
	}
	msg := cleanErrorMessage(raw)
	apiErr := &APIError{
		Command:  append([]string(nil), command...),
		Method:   method,
		ExitCode: res.ExitCode,
		Message:  msg,
		Stderr:   raw,
	}
	if m := statusCodeRe.FindStringSubmatch(raw); len(m) == 3 {
		if st, err := strconv.Atoi(m[1]); err == nil {
			apiErr.Status = st
		}
		apiErr.Code = m[2]
	}
	return apiErr
}

func cleanErrorMessage(raw string) string {
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "Error: ")
		line = strings.TrimPrefix(line, "error: ")
		return line
	}
	return ""
}

// Option configures a Client.
type Option func(*Client)

// WithBinary sets the rex executable name or path (defaults to "rex").
func WithBinary(binary string) Option {
	return func(c *Client) {
		if binary != "" {
			c.binary = binary
		}
	}
}

// WithServer targets a remote Rex host label or server address (-S).
// When set, ambient REX_SESSION and REX_BLOCK environment variables are ignored
// unless WithSession or WithBlock is also supplied explicitly.
func WithServer(server string) Option {
	return func(c *Client) {
		c.server = server
	}
}

// WithClient targets a connected Rex client app by label or ID (-C).
func WithClient(clientID string) Option {
	return func(c *Client) {
		c.client = clientID
	}
}

// WithSession sets an explicit default session ID or label for calls.
func WithSession(session string) Option {
	return func(c *Client) {
		c.session = session
		c.sessionExplicit = session != ""
	}
}

// WithBlock sets an explicit default block ID or label for calls.
func WithBlock(block string) Option {
	return func(c *Client) {
		c.block = block
		c.blockExplicit = block != ""
	}
}

// WithTimeout sets the --timeout duration passed to rex for non-streaming calls.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.timeout = d
	}
}

// WithAutoStart controls whether --autostart=false is passed to rex.
func WithAutoStart(enabled bool) Option {
	return func(c *Client) {
		c.autoStartSet = true
		c.autoStart = enabled
	}
}

// WithCommandRunner installs a custom CommandRunner (e.g. sandbox-backed or fake).
func WithCommandRunner(runner CommandRunner) Option {
	return func(c *Client) {
		if runner != nil {
			c.runner = runner
		}
	}
}

// WithEnvLookup overrides environment variable resolution (defaults to os.Getenv).
func WithEnvLookup(lookup func(string) string) Option {
	return func(c *Client) {
		if lookup != nil {
			c.getenv = lookup
		}
	}
}

// Client invokes a Rex server through the rex CLI. A Client value is safe for
// concurrent use once constructed; use Clone or the scoping helpers to derive
// modified copies.
type Client struct {
	binary          string
	server          string
	client          string
	session         string
	sessionExplicit bool
	block           string
	blockExplicit   bool
	timeout         time.Duration
	autoStartSet    bool
	autoStart       bool
	runner          CommandRunner
	getenv          func(string) string
}

// NewClient constructs a Rex Client with the given functional options.
func NewClient(opts ...Option) *Client {
	c := &Client{
		binary: "rex",
		runner: execRunner{},
		getenv: os.Getenv,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Clone returns a shallow copy of c with additional options applied.
func (c *Client) Clone(opts ...Option) *Client {
	cp := *c
	for _, opt := range opts {
		if opt != nil {
			opt(&cp)
		}
	}
	return &cp
}

// ForServer returns a copy scoped to a remote host label or address (-S).
// Any previously inherited ambient session/block is cleared unless set again.
func (c *Client) ForServer(server string) *Client {
	cp := *c
	cp.server = server
	if !cp.sessionExplicit {
		cp.session = ""
	}
	if !cp.blockExplicit {
		cp.block = ""
	}
	return &cp
}

// ForSession returns a copy scoped to the given session ID or label.
func (c *Client) ForSession(session string) *Client {
	return c.Clone(WithSession(session))
}

// ForBlock returns a copy scoped to the given block ID or label.
func (c *Client) ForBlock(block string) *Client {
	return c.Clone(WithBlock(block))
}

// ForClient returns a copy targeting the given connected app client (-C).
func (c *Client) ForClient(clientID string) *Client {
	return c.Clone(WithClient(clientID))
}

// EffectiveSession returns the session target to use for calls. Explicit
// WithSession wins; otherwise REX_SESSION is read only for the local server
// (when WithServer is empty).
func (c *Client) EffectiveSession() string {
	if c.session != "" {
		return c.session
	}
	if c.server != "" || c.getenv == nil {
		return ""
	}
	return strings.TrimSpace(c.getenv("REX_SESSION"))
}

// EffectiveBlock returns the block target to use for calls. Explicit WithBlock
// wins; otherwise REX_BLOCK is read only for the local server (when WithServer
// is empty).
func (c *Client) EffectiveBlock() string {
	if c.block != "" {
		return c.block
	}
	if c.server != "" || c.getenv == nil {
		return ""
	}
	return strings.TrimSpace(c.getenv("REX_BLOCK"))
}

// ServerTarget returns the configured remote server (-S), if any.
func (c *Client) ServerTarget() string { return c.server }

// ClientTarget returns the configured app client (-C), if any.
func (c *Client) ClientTarget() string { return c.client }

func (c *Client) globalArgs() []string {
	var args []string
	if c.server != "" {
		args = append(args, "-S", c.server)
	}
	if c.client != "" {
		args = append(args, "-C", c.client)
	}
	if c.autoStartSet && !c.autoStart {
		args = append(args, "--autostart=false")
	}
	if c.timeout > 0 {
		args = append(args, "--timeout", c.timeout.String())
	}
	args = append(args, "--color", "never")
	return args
}

func (c *Client) resolveSession(override string) string {
	if override != "" {
		return override
	}
	return c.EffectiveSession()
}

func (c *Client) resolveBlock(override string) string {
	if override != "" {
		return override
	}
	return c.EffectiveBlock()
}

func (c *Client) runRaw(ctx context.Context, subArgs []string, stdin []byte, method string) (CommandResult, error) {
	args := append(c.globalArgs(), subArgs...)
	res, err := c.runner.Run(ctx, CommandSpec{
		Binary: c.binary,
		Args:   args,
		Stdin:  stdin,
	})
	if err != nil {
		return res, fmt.Errorf("rex %s: %w", methodOrCommand(method, subArgs), err)
	}
	if res.ExitCode != 0 {
		return res, parseAPIError(subArgs, method, res)
	}
	return res, nil
}

func (c *Client) runJSON(ctx context.Context, subArgs []string, stdin []byte, method string, out any) error {
	res, err := c.runRaw(ctx, subArgs, stdin, method)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(res.Stdout)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(trimmed, out); err != nil {
		return fmt.Errorf("rex %s: decode json: %w", methodOrCommand(method, subArgs), err)
	}
	return nil
}

func (c *Client) startStream(ctx context.Context, subArgs []string, stdin []byte) (StreamProcess, error) {
	args := append(c.globalArgs(), subArgs...)
	proc, err := c.runner.Start(ctx, CommandSpec{
		Binary: c.binary,
		Args:   args,
		Stdin:  stdin,
	})
	if err != nil {
		return nil, fmt.Errorf("rex %s: %w", methodOrCommand("", subArgs), err)
	}
	return proc, nil
}

func methodOrCommand(method string, subArgs []string) string {
	if method != "" {
		return method
	}
	if len(subArgs) == 0 {
		return "command"
	}
	return strings.Join(subArgs, " ")
}
