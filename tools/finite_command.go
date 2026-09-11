package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// ErrCommandOutputIncomplete means capture did not reach EOF. The returned
// output is a partial result, distinct from the bounded buffer's size limit.
// It is never an ordinary CommandError, even if the process also exited nonzero.
var ErrCommandOutputIncomplete = errors.New("command output incomplete")

const commandDrainTimeout = time.Second

type finiteCommand struct {
	name        string
	args        []string
	dir         string
	stdout      *boundedBuffer
	stderr      *boundedBuffer
	acknowledge bool // Bash target-start proof, before sandbox wrapping.
}

// runFiniteCommand returns a bare *exec.ExitError only when target startup and
// capture succeeded and the sole failure is the process exit. All setup and
// capture failures are wrapped so callers cannot mistake them for shell exits.
func runFiniteCommand(ctx context.Context, sb sandbox.Sandbox, spec finiteCommand) (_ *os.ProcessState, err error) {
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, spec.name, spec.args...)
	cmd.Dir = spec.dir
	cmd.WaitDelay = commandDrainTimeout
	capture, err := newCommandCapture(cmd, spec.stdout, spec.stderr, spec.acknowledge)
	if err != nil {
		return nil, err
	}
	defer capture.close()
	cleanup, err := sandbox.WrapFiniteCmdManaged(sb, cmd)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	err = capture.run(ctx, cmd, cancel, cleanup)
	return cmd.ProcessState, err
}

type commandPipe struct {
	name    string
	reader  *os.File
	writer  *os.File
	buffer  *boundedBuffer // nil for the one-byte startup acknowledgment.
	done    chan struct{}
	err     error // Read only after done closes.
	ready   bool
	expired bool // Owned by the drain loop.
}

type commandCapture struct {
	pipes []*commandPipe
	ack   *commandPipe
}

func newCommandCapture(cmd *exec.Cmd, stdout, stderr *boundedBuffer, acknowledge bool) (*commandCapture, error) {
	c := &commandCapture{}
	add := func(name string, buffer *boundedBuffer) (*commandPipe, error) {
		r, w, err := os.Pipe()
		if err != nil {
			c.close()
			return nil, fmt.Errorf("capture %s: %w", name, err)
		}
		p := &commandPipe{name: name, reader: r, writer: w, buffer: buffer, done: make(chan struct{})}
		c.pipes = append(c.pipes, p)
		return p, nil
	}
	out, err := add("stdout", stdout)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = out.writer
	cmd.Stderr = out.writer
	if stdout != stderr {
		errOut, err := add("stderr", stderr)
		if err != nil {
			return nil, err
		}
		cmd.Stderr = errOut.writer
	}
	if acknowledge {
		c.ack, err = add("startup acknowledgment", nil)
		if err != nil {
			return nil, err
		}
		fd := 3 + len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, c.ack.writer)
		last := len(cmd.Args) - 1
		cmd.Args[last] = fmt.Sprintf("printf . >&%d; exec %d>&-; ", fd, fd) + cmd.Args[last]
	}
	return c, nil
}

func (c *commandCapture) close() {
	for _, p := range c.pipes {
		_ = p.writer.Close()
		_ = p.reader.Close()
	}
}

func (p *commandPipe) read() {
	defer close(p.done)
	if p.buffer != nil {
		_, p.err = io.Copy(p.buffer, p.reader)
		return
	}
	var ready [1]byte
	n, err := p.reader.Read(ready[:])
	p.ready = n == 1 && ready[0] == '.'
	if !errors.Is(err, io.EOF) {
		p.err = err
	}
}

func (c *commandCapture) run(ctx context.Context, cmd *exec.Cmd, cancel context.CancelFunc, cleanup func() error) error {
	// Cancel and completion must agree before touching the owned process scope.
	// In particular, cancellation during post-exit draining only bounds capture.
	var mu sync.Mutex
	finished := false
	stop := cmd.Cancel
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return os.ErrProcessDone
		}
		return stop()
	}
	completed := make(chan *commandPipe, len(c.pipes))
	for _, p := range c.pipes {
		go func() { p.read(); completed <- p }()
	}
	startErr := cmd.Start()
	var closeErr error
	for _, p := range c.pipes {
		closeErr = errors.Join(closeErr, p.writer.Close())
	}
	closeErr = errors.Join(closeErr, cleanup())
	if startErr != nil {
		c.close()
		for range c.pipes {
			<-completed
		}
		return fmt.Errorf("launch command: %w", errors.Join(startErr, closeErr))
	}
	if closeErr != nil {
		cancel()
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait() // Exactly one Wait for each successful Start.
		mu.Lock()
		finished = true
		mu.Unlock()
		waited <- err
	}()
	var waitErr, captureErr error
	var deadline <-chan time.Time
	var timer *time.Timer
	startDrain := func() {
		if timer == nil {
			timer = time.NewTimer(commandDrainTimeout)
			deadline = timer.C
		}
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	if closeErr != nil {
		startDrain()
	}
	canceled := ctx.Done()
	pending := len(c.pipes)
	for waited != nil || pending > 0 {
		select {
		case waitErr = <-waited:
			waited = nil
			startDrain()
		case <-canceled:
			canceled = nil
			startDrain()
		case p := <-completed:
			pending--
			if p.err != nil {
				err := fmt.Errorf("%w: %s: %w", ErrCommandOutputIncomplete, p.name, p.err)
				if p.expired {
					err = fmt.Errorf("%w: %s did not close within %s", ErrCommandOutputIncomplete, p.name, commandDrainTimeout)
				}
				captureErr = errors.Join(captureErr, err)
				// A broken capture must not leave a live producer blocked forever.
				cancel()
				startDrain()
			}
		case <-deadline:
			deadline = nil
			for _, p := range c.pipes {
				select {
				case <-p.done:
				default:
					// Closing our reader unblocks Read/Copy. Join it through
					// completed before inspecting buffers or returning.
					p.expired = true
					_ = p.reader.Close()
				}
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if closeErr != nil {
		return fmt.Errorf("close command descriptors: %w", closeErr)
	}
	// A successful wrapper is accepted even if its acknowledgment was lost.
	// A failing launcher without target-start proof is never a command result.
	if c.ack != nil && !c.ack.ready && waitErr != nil {
		return fmt.Errorf("sandbox target did not start: %w", errors.Join(waitErr, captureErr))
	}
	if captureErr != nil {
		return captureErr
	}
	return waitErr
}
