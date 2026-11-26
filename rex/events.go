package rex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// SubscribeOptions configures "rex events --json".
type SubscribeOptions struct {
	SessionID string
	All       bool
}

// Event is one decoded JSON event emitted by "rex events --json".
type Event struct {
	Type      string          `json:"type"`
	Event     string          `json:"event,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	WindowID  string          `json:"window_id,omitempty"`
	BlockID   string          `json:"block_id,omitempty"`
	Creator   string          `json:"creator,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

// EventStream delivers live server/session events from "rex events --json".
// Call Close when finished to stop the underlying subprocess cleanly.
type EventStream struct {
	events chan Event
	proc   StreamProcess
	cancel context.CancelFunc

	done chan struct{}
	mu   sync.Mutex
	err  error
}

// Events returns the channel of decoded events. It closes when the stream
// finishes, errors, or is closed.
func (s *EventStream) Events() <-chan Event { return s.events }

// Err returns the terminal error, if any, after the Events channel closes.
func (s *EventStream) Err() error {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close cancels the event stream and waits for the underlying process to exit.
func (s *EventStream) Close() error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	var killErr error
	if s.proc != nil {
		killErr = s.proc.Kill()
	}
	<-s.done
	if killErr != nil {
		return killErr
	}
	return nil
}

// Subscribe starts a live NDJSON event subscription via "rex events --json".
func (c *Client) Subscribe(ctx context.Context, opts SubscribeOptions) (*EventStream, error) {
	subArgs := []string{"events", "--json"}
	if opts.All {
		subArgs = append(subArgs, "--all")
	} else if sid := c.resolveSession(opts.SessionID); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	runCtx, cancel := context.WithCancel(ctx)
	proc, err := c.startStream(runCtx, subArgs, nil)
	if err != nil {
		cancel()
		return nil, err
	}

	stream := &EventStream{
		events: make(chan Event, 32),
		proc:   proc,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	go stream.pump(runCtx)
	return stream, nil
}

func (s *EventStream) pump(ctx context.Context) {
	defer close(s.done)
	defer close(s.events)

	var stderrBuf strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if se := s.proc.Stderr(); se != nil {
			_, _ = io.Copy(&stderrBuf, se)
		}
	}()

	reader := bufio.NewReaderSize(s.proc.Stdout(), 64*1024)
	var streamErr error
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			ev, parseErr := decodeEvent(trimmed)
			if parseErr != nil {
				streamErr = fmt.Errorf("rex events: decode event: %w", parseErr)
				break
			}
			select {
			case s.events <- ev:
			case <-ctx.Done():
				streamErr = ctx.Err()
			}
			if streamErr != nil {
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
	}

	if ctx.Err() != nil {
		_ = s.proc.Kill()
	}
	waitErr := s.proc.Wait()
	<-stderrDone

	if streamErr == nil && ctx.Err() == nil && waitErr != nil {
		if msg := cleanErrorMessage(stderrBuf.String()); msg != "" {
			streamErr = fmt.Errorf("rex events: %s", msg)
		} else {
			streamErr = fmt.Errorf("rex events: %w", waitErr)
		}
	} else if streamErr == nil && ctx.Err() != nil {
		streamErr = ctx.Err()
	}

	s.mu.Lock()
	s.err = streamErr
	s.mu.Unlock()
}

func decodeEvent(raw []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Event{}, err
	}
	ev.Raw = append(json.RawMessage(nil), raw...)
	return ev, nil
}

// WaitEvent subscribes to events and blocks until match returns true, ctx is
// canceled, or the stream terminates. The subscription is always closed before
// WaitEvent returns.
func (c *Client) WaitEvent(ctx context.Context, opts SubscribeOptions, match func(Event) bool) (Event, error) {
	if match == nil {
		return Event{}, fmt.Errorf("rex wait event: match predicate is required")
	}
	stream, err := c.Subscribe(ctx, opts)
	if err != nil {
		return Event{}, err
	}
	defer stream.Close()

	for {
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case ev, ok := <-stream.Events():
			if !ok {
				if err := stream.Err(); err != nil {
					return Event{}, err
				}
				return Event{}, io.EOF
			}
			if match(ev) {
				return ev, nil
			}
		}
	}
}
