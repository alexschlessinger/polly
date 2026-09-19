//go:build darwin

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// darwinLogPath is the system log's command-line tool. Like the trusted Git
// of the preset audit, it runs on the host rather than in a sandbox: log
// refuses to run under Seatbelt, and only a live stream sees the kernel's
// denial reports, which log show does not keep. It must be root-owned and
// immutable to other users, and it is started with a fixed argument vector
// whose predicate names only the trial's tag, PATH alone in its environment
// and no input. Its output is parsed as data.
const darwinLogPath = "/usr/bin/log"

// darwinCanaryPath is the executable of the canary writes that prove the
// stream live and mark the end of a trial's reports.
const darwinCanaryPath = "/usr/bin/touch"

const (
	// darwinCanaryWait is how long one canary's report may take to arrive
	// while the stream starts; canaries repeat until darwinReadyWait passes.
	darwinCanaryWait = 300 * time.Millisecond
	darwinReadyWait  = 3 * time.Second
	// darwinFlushWait bounds the wait for the canary after the command;
	// darwinFlushGrace more lets the reports the kernel coalesced arrive.
	darwinFlushWait  = 3 * time.Second
	darwinFlushGrace = 250 * time.Millisecond
	// darwinStreamStop is how long the stream has to exit when asked.
	darwinStreamStop = time.Second
	// darwinStreamTimeout ends a stream on its own when the trial has no
	// deadline, so one Polly left behind by a crash does not keep it.
	darwinStreamTimeout = time.Hour
	// darwinMaxRecord bounds one line of the stream.
	darwinMaxRecord = 1 << 20
)

type observerState struct {
	mu       sync.Mutex
	stream   *exec.Cmd
	done     chan struct{} // closed when the stream's output ends
	stopped  bool
	set      denialSet
	canaries int
	seen     map[string]bool // canary names reported
	wake     chan struct{}
	readErr  error
	stderr   *boundedWriter
}

func (o *DenialObserver) initPlatform() error {
	o.seen = make(map[string]bool)
	o.wake = make(chan struct{}, 1)
	return nil
}

// Start starts the host's system log streaming the reports of sb's denials
// and waits until a canary write proves the stream live. sb must be built
// from a config marked by o.Config. An error wrapping ErrDenialsUnobservable
// means the trial can run, but its denials cannot be seen.
func (o *DenialObserver) Start(ctx context.Context, sb Sandbox) error {
	darwin, ok := sb.(*darwinSandbox)
	if !ok || darwin.cfg.denialTag != o.tag {
		return fmt.Errorf("%w: the trial's sandbox was not built from the observer's config", ErrDenialsUnobservable)
	}
	if o.stream != nil {
		return errors.New("denial observer already started")
	}
	if err := validateDarwinTrustedExecutable(darwinLogPath, "denial observer"); err != nil {
		return fmt.Errorf("%w: %w", ErrDenialsUnobservable, err)
	}
	stream := exec.Command(darwinLogPath, "stream", "--style", "ndjson",
		"--timeout", streamTimeout(ctx),
		"--predicate", fmt.Sprintf(`sender == "Sandbox" AND eventMessage CONTAINS "%s"`, o.tag))
	stream.Env = []string{"PATH=/usr/bin:/bin"}
	stream.Dir = "/"
	// Its own process group keeps a terminal's interrupt meant for Polly's
	// foreground work from ending the stream mid-trial.
	stream.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	o.stderr = &boundedWriter{limit: 4 << 10}
	stream.Stderr = o.stderr
	stdout, err := stream.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDenialsUnobservable, err)
	}
	if err := stream.Start(); err != nil {
		return fmt.Errorf("%w: start the system log: %w", ErrDenialsUnobservable, err)
	}
	o.stream = stream
	o.done = make(chan struct{})
	go o.read(stdout)

	deadline := time.Now().Add(darwinReadyWait)
	for {
		seen, err := o.canary(ctx, sb, darwinCanaryWait)
		if err != nil {
			o.stop()
			return err
		}
		if seen {
			return nil
		}
		if time.Now().After(deadline) {
			o.stop()
			return fmt.Errorf("%w: the system log did not report the trial's canary%s", ErrDenialsUnobservable, o.streamFailure())
		}
	}
}

// streamTimeout is the log stream's own time limit: a minute past the
// trial's deadline, or darwinStreamTimeout without one.
func streamTimeout(ctx context.Context) string {
	limit := darwinStreamTimeout
	if deadline, ok := ctx.Deadline(); ok {
		limit = time.Until(deadline) + time.Minute
	}
	return fmt.Sprintf("%dm", int(limit.Minutes())+1)
}

// Shell returns the command itself: a trial on macOS needs no report of its
// own.
func (o *DenialObserver) Shell(command string, reportFD int) TrialShell {
	return TrialShell{Script: command}
}

// Finish marks the end of the command's reports with a second canary, stops
// the stream and returns what it saw. The report is unused on macOS.
func (o *DenialObserver) Finish(ctx context.Context, sb Sandbox, report []byte) (Observation, error) {
	if o.stream == nil {
		return Observation{}, errors.New("denial observer not started")
	}
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), darwinFlushWait)
	defer cancel()
	var incomplete string
	flushed, err := o.canary(flushCtx, sb, darwinFlushWait)
	switch {
	case err != nil:
		incomplete = "the end of the trial's reports could not be marked: " + err.Error()
	case !flushed:
		incomplete = "the system log did not report the end of the trial, so later denials may be missing" + o.streamFailure()
	default:
		time.Sleep(darwinFlushGrace)
	}
	o.stop()
	o.mu.Lock()
	defer o.mu.Unlock()
	if incomplete == "" && o.readErr != nil {
		incomplete = "the system log's output could not be read: " + o.readErr.Error()
	}
	return Observation{Denials: slices.Clone(o.set.denials), Truncated: o.set.truncated, Incomplete: incomplete}, nil
}

// Close stops the stream if it still runs.
func (o *DenialObserver) Close() error {
	if o.stream != nil {
		o.stop()
	}
	return nil
}

// canary writes a new file into the home directory through sb, which every
// trial profile denies under its tag, and reports whether the stream
// reported that denial within wait.
func (o *DenialObserver) canary(ctx context.Context, sb Sandbox, wait time.Duration) (bool, error) {
	o.mu.Lock()
	o.canaries++
	name := fmt.Sprintf(".%s-canary-%d", o.tag, o.canaries)
	o.mu.Unlock()
	path := filepath.Join(o.home, name)
	cmd := exec.CommandContext(ctx, darwinCanaryPath, path)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		return false, err
	}
	runErr := cmd.Run()
	if err := cleanup(); err != nil {
		return false, err
	}
	if runErr == nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("%w: the trial's sandbox let its canary write the home directory", ErrDenialsUnobservable)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		o.mu.Lock()
		seen := o.seen[name]
		o.mu.Unlock()
		if seen {
			return true, nil
		}
		select {
		case <-o.wake:
		case <-o.done:
			o.mu.Lock()
			defer o.mu.Unlock()
			return o.seen[name], nil
		case <-timer.C:
			return false, nil
		}
	}
}

// read parses the stream: one JSON event per line after a plain header line.
func (o *DenialObserver) read(stdout io.Reader) {
	defer close(o.done)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64<<10), darwinMaxRecord)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var event struct {
			EventMessage string `json:"eventMessage"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if d, ok := parseSeatbeltReport(event.EventMessage, o.tag); ok {
			o.record(d)
		}
	}
	if err := scanner.Err(); err != nil {
		o.mu.Lock()
		o.readErr = err
		o.mu.Unlock()
		// Keep the stream from blocking on a full pipe until it is stopped.
		_, _ = io.Copy(io.Discard, stdout)
	}
}

func (o *DenialObserver) record(d Denial) {
	o.mu.Lock()
	defer o.mu.Unlock()
	// A canary is known by its name, which holds the tag, whatever
	// spelling of the home directory the kernel reports it under.
	if name := filepath.Base(d.Path); strings.HasPrefix(name, "."+o.tag+"-canary-") {
		o.seen[name] = true
		select {
		case o.wake <- struct{}{}:
		default:
		}
		return
	}
	if !seatbeltNoise(d, o.home) {
		o.set.add(d)
	}
}

// stop ends the stream and waits for its output to be read.
func (o *DenialObserver) stop() {
	if o.stopped {
		return
	}
	o.stopped = true
	_ = o.stream.Process.Signal(syscall.SIGTERM)
	select {
	case <-o.done:
	case <-time.After(darwinStreamStop):
		_ = o.stream.Process.Kill()
		<-o.done
	}
	_ = o.stream.Wait()
}

// streamFailure is what the stream said on its way out, for an error.
func (o *DenialObserver) streamFailure() string {
	if message := strings.TrimSpace(o.stderr.String()); message != "" {
		return " (" + message + ")"
	}
	return ""
}

// boundedWriter keeps the first limit bytes written to it.
type boundedWriter struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.limit - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (w *boundedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
