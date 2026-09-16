// Package docker runs an agent's tools inside a container. The container is
// not another sandbox: it is an OpenTools implementation whose registry holds
// proxies for tools served by polly's helper inside the container. This file
// is the host side of the helper protocol.
package docker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
)

// cancelGrace is how long a cancelled execution waits for the helper's
// terminal reply before returning, keeping the reader loop in step.
const cancelGrace = 5 * time.Second

// defaultHeartbeat is the idle heartbeat interval.
const defaultHeartbeat = 30 * time.Second

// ProtocolError is a terminal error reply from the helper.
type ProtocolError struct {
	Code    string
	Message string
}

func (e *ProtocolError) Error() string { return e.Message + " (" + e.Code + ")" }

// pending is one request awaiting its terminal reply.
type pending struct {
	terminal chan protocol.Frame
}

// session is one exec stream to the helper: it numbers requests, matches
// replies, and fails every in-flight request when the stream ends.
type session struct {
	conn        *protocol.Conn
	closeStream func() error

	mu      sync.Mutex
	next    uint64
	pending map[uint64]*pending
	broken  error
	done    chan struct{}
	missed  int

	closeOnce sync.Once
	closeErr  error
	stopBeat  chan struct{}
}

func newSession(conn *protocol.Conn, closeStream func() error, heartbeat time.Duration) *session {
	s := &session{conn: conn, closeStream: closeStream, pending: map[uint64]*pending{}, done: make(chan struct{}), stopBeat: make(chan struct{})}
	go s.readLoop()
	if heartbeat > 0 {
		go s.heartbeatLoop(heartbeat)
	}
	return s
}

func (s *session) readLoop() {
	defer close(s.done)
	for {
		frame, err := s.conn.Read()
		if err != nil {
			s.fail(fmt.Errorf("container helper connection lost: %w", err))
			return
		}
		s.mu.Lock()
		p := s.pending[frame.ID]
		s.mu.Unlock()
		if p == nil || frame.Type == protocol.TypeProgress {
			continue
		}
		select {
		case p.terminal <- frame:
		default:
		}
	}
}

// fail marks the session broken and answers every waiting request with a
// helper-lost error.
func (s *session) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		return
	}
	s.broken = err
	lost := protocol.Frame{Type: protocol.TypeError}
	lost.Body, _ = encodeBody(protocol.Error{Code: protocol.CodeHelperLost, Message: err.Error()})
	for _, p := range s.pending {
		select {
		case p.terminal <- lost:
		default:
		}
	}
}

func (s *session) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken
}

func (s *session) register() (uint64, *pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		return 0, nil, s.broken
	}
	s.next++
	p := &pending{terminal: make(chan protocol.Frame, 1)}
	s.pending[s.next] = p
	return s.next, p, nil
}

func (s *session) unregister(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

func (s *session) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) == 0
}

// call sends one request and waits for its terminal reply.
func (s *session) call(ctx context.Context, typ string, body any) (protocol.Frame, error) {
	id, p, err := s.register()
	if err != nil {
		return protocol.Frame{}, err
	}
	defer s.unregister(id)
	if err := s.conn.Send(id, typ, body); err != nil {
		s.fail(fmt.Errorf("container helper connection lost: %w", err))
		return protocol.Frame{}, s.failed()
	}
	select {
	case frame := <-p.terminal:
		return terminal(frame)
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	}
}

// terminal turns an error reply into an error.
func terminal(frame protocol.Frame) (protocol.Frame, error) {
	if frame.Type != protocol.TypeError {
		return frame, nil
	}
	failure, err := protocol.Decode[protocol.Error](frame)
	if err != nil {
		return protocol.Frame{}, err
	}
	if failure.Code == protocol.CodeHelperLost {
		return protocol.Frame{}, tools.NewToolError(failure.Message, protocol.CodeHelperLost)
	}
	return protocol.Frame{}, &ProtocolError{Code: failure.Code, Message: failure.Message}
}

// execute runs one tool. When ctx ends first it asks the helper to cancel
// the execution and still waits briefly for the terminal reply, so a
// partial outcome reaches the caller and the stream stays in step.
func (s *session) execute(ctx context.Context, req protocol.Execute) (protocol.Result, error) {
	id, p, err := s.register()
	if err != nil {
		return protocol.Result{}, err
	}
	defer s.unregister(id)
	if err := s.conn.Send(id, protocol.TypeExecute, req); err != nil {
		s.fail(fmt.Errorf("container helper connection lost: %w", err))
		return protocol.Result{}, s.failed()
	}
	var frame protocol.Frame
	select {
	case frame = <-p.terminal:
	case <-ctx.Done():
		_ = s.cancel(id)
		select {
		case frame = <-p.terminal:
		case <-time.After(cancelGrace):
			return protocol.Result{}, ctx.Err()
		}
	}
	frame, err = terminal(frame)
	if err != nil {
		return protocol.Result{}, err
	}
	return protocol.Decode[protocol.Result](frame)
}

// cancel asks the helper to cancel an in-flight execution. It waits for the
// acknowledgement only briefly; the execution's own terminal reply follows.
func (s *session) cancel(id uint64) error {
	ctx, stop := context.WithTimeout(context.Background(), cancelGrace)
	defer stop()
	_, err := s.call(ctx, protocol.TypeCancel, protocol.Cancel{ID: id})
	return err
}

func (s *session) hello(ctx context.Context, hello protocol.Hello) (protocol.Welcome, error) {
	frame, err := s.call(ctx, protocol.TypeHello, hello)
	if err != nil {
		return protocol.Welcome{}, err
	}
	return protocol.Decode[protocol.Welcome](frame)
}

func (s *session) load(ctx context.Context, load protocol.Load) (protocol.Loaded, error) {
	frame, err := s.call(ctx, protocol.TypeLoad, load)
	if err != nil {
		return protocol.Loaded{}, err
	}
	return protocol.Decode[protocol.Loaded](frame)
}

func (s *session) list(ctx context.Context) (protocol.Listed, error) {
	frame, err := s.call(ctx, protocol.TypeList, nil)
	if err != nil {
		return protocol.Listed{}, err
	}
	return protocol.Decode[protocol.Listed](frame)
}

// heartbeatLoop pings the helper while the session is idle; two missed
// replies break the session, so a container stopped under an idle REPL is
// noticed before the next tool call hangs.
func (s *session) heartbeatLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopBeat:
			return
		case <-s.done:
			return
		case <-ticker.C:
		}
		if !s.idle() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		_, err := s.call(ctx, protocol.TypeHeartbeat, nil)
		cancel()
		s.mu.Lock()
		if err != nil {
			s.missed++
		} else {
			s.missed = 0
		}
		missed := s.missed
		s.mu.Unlock()
		if missed >= 2 {
			s.fail(errors.New("container helper stopped answering heartbeats"))
			return
		}
	}
}

// Close ends the stream. The helper sees EOF, cancels what is in flight and
// exits; in-flight callers here get a helper-lost error.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		close(s.stopBeat)
		s.closeErr = s.closeStream()
		s.fail(errors.New("container helper connection closed"))
		select {
		case <-s.done:
		case <-time.After(cancelGrace):
		}
	})
	return s.closeErr
}
