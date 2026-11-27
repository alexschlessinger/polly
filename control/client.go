package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Lifecycle lines a daemon writes before it shuts a connection.
const (
	// MethodBehind says the connection fell behind and was dropped: its
	// turns go on, and the client catches up from the store.
	MethodBehind = "behind"
	// MethodWorkerGone says the worker that served the connection's setup
	// died while the daemon lives.
	MethodWorkerGone = "worker_gone"
)

// Client speaks the control contract over one connection to a daemon.
type Client struct {
	conn *Conn

	mu   sync.Mutex
	subs map[*Subscription]struct{}

	behind     atomic.Bool
	workerGone atomic.Bool

	// Welcome is what the engine said to this client's hello.
	Welcome Welcome
	// SetupKey is the key of the setup the client was greeted for.
	SetupKey string
}

// NewClient reads and writes the control contract on rwc.
func NewClient(rwc io.ReadWriteCloser) *Client {
	c := &Client{subs: map[*Subscription]struct{}{}}
	c.conn = NewConn(rwc, c.onNotify)
	return c
}

// Close ends the connection. Turns it asked go on in the daemon.
func (c *Client) Close() error { return c.conn.Close() }

// Done is closed once the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.conn.Done() }

// Err says why the connection ended, once it has.
func (c *Client) Err() error { return c.conn.Err() }

// Behind reports that the daemon dropped this connection for falling behind.
func (c *Client) Behind() bool { return c.behind.Load() }

// WorkerGone reports that the daemon's worker for this setup died.
func (c *Client) WorkerGone() bool { return c.workerGone.Load() }

func (c *Client) onNotify(n Notification) {
	switch n.Method {
	case MethodBehind:
		c.behind.Store(true)
		return
	case MethodWorkerGone:
		c.workerGone.Store(true)
		return
	}
	ev, err := ParseEvent(n.Method, n.Params)
	if err != nil {
		return
	}
	c.mu.Lock()
	for s := range c.subs {
		if s.match == nil || s.match(ev) {
			s.push(ev)
		}
	}
	c.mu.Unlock()
}

// Subscription receives the events of the agents it matches, in the order
// the connection brought them. Its queue is unbounded so the reader never
// waits on a slow consumer.
type Subscription struct {
	c      *Client
	match  func(Event) bool
	mu     sync.Mutex
	queue  []Event
	signal chan struct{}
	closed bool
}

// Subscribe receives every event match accepts; nil accepts every event.
func (c *Client) Subscribe(match func(Event) bool) *Subscription {
	s := &Subscription{c: c, match: match, signal: make(chan struct{}, 1)}
	c.mu.Lock()
	c.subs[s] = struct{}{}
	c.mu.Unlock()
	return s
}

// SubscribeAgents receives the events of the agents ids names, and, with
// children, of any agent whose parent is among them or among the children
// it has seen.
func (c *Client) SubscribeAgents(children bool, ids ...ID) *Subscription {
	known := map[ID]bool{}
	for _, id := range ids {
		known[id] = true
	}
	var mu sync.Mutex
	return c.Subscribe(func(ev Event) bool {
		mu.Lock()
		defer mu.Unlock()
		if known[ev.Agent] {
			return true
		}
		if children && ev.Parent != "" && known[ev.Parent] {
			known[ev.Agent] = true
			return true
		}
		return false
	})
}

func (s *Subscription) push(ev Event) {
	s.mu.Lock()
	if !s.closed {
		s.queue = append(s.queue, ev)
	}
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// Close stops the subscription.
func (s *Subscription) Close() {
	s.c.mu.Lock()
	delete(s.c.subs, s)
	s.c.mu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.queue = nil
	s.mu.Unlock()
}

// ErrEndOfEvents is what Next returns once the connection has ended and every
// event it brought was taken.
var ErrEndOfEvents = errors.New("control connection ended")

// Next returns the next event, waiting for one until ctx ends or the
// connection ends with nothing left.
func (s *Subscription) Next(ctx context.Context) (Event, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue[0] = Event{}
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return ev, nil
		}
		s.mu.Unlock()
		select {
		case <-s.signal:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-s.c.conn.Done():
			// Take what came before the end.
			s.mu.Lock()
			empty := len(s.queue) == 0
			s.mu.Unlock()
			if empty {
				return Event{}, ErrEndOfEvents
			}
		}
	}
}

// Ready is signaled when an event may be waiting.
func (s *Subscription) Ready() <-chan struct{} { return s.signal }

// TryNext returns the next event without waiting.
func (s *Subscription) TryNext() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Event{}, false
	}
	ev := s.queue[0]
	s.queue[0] = Event{}
	s.queue = s.queue[1:]
	return ev, true
}

// ------------------------------------------------------------- requests

// Join is the first line of a connection that joins a daemon.
func (c *Client) Join(ctx context.Context, req JoinReq) (Made, error) {
	req.Protocol = Protocol
	var made Made
	err := c.conn.Call(ctx, "join", req, &made)
	return made, err
}

// Start makes the setup's engine. The params hold provider keys: they are
// written once and overwritten as soon as the start is answered.
func (c *Client) Start(ctx context.Context, start Start) (Made, error) {
	start.Protocol = Protocol
	params, err := json.Marshal(start)
	if err != nil {
		return Made{}, fmt.Errorf("start: %w", err)
	}
	defer wipe(params)
	raw, err := c.conn.CallRaw(ctx, "start", params)
	if err != nil {
		return Made{}, err
	}
	var made Made
	if err := json.Unmarshal(raw, &made); err != nil {
		return Made{}, fmt.Errorf("start: read the result: %w", err)
	}
	return made, nil
}

// Hello greets the engine with the setup's key.
func (c *Client) Hello(ctx context.Context, setup string) (Welcome, error) {
	var w Welcome
	if err := c.conn.Call(ctx, "hello", Hello{Protocol: Protocol, Setup: setup}, &w); err != nil {
		return Welcome{}, err
	}
	c.Welcome, c.SetupKey = w, setup
	return w, nil
}

// Watch says what this client is sent of an agent from now on.
func (c *Client) Watch(ctx context.Context, w Watch) (Watched, error) {
	var out Watched
	err := c.conn.Call(ctx, "watch", w, &out)
	return out, err
}

// Unwatch ends this client's watch of agent.
func (c *Client) Unwatch(ctx context.Context, agent ID) error {
	return c.conn.Call(ctx, "watch", Watch{Agent: agent, Off: true}, nil)
}

// Prompt asks a turn and waits for its end. A ctx that ends first leaves
// the turn running in the daemon: a turn is stopped by Stop.
func (c *Client) Prompt(ctx context.Context, p Prompt) (Reply, error) {
	var out Reply
	err := c.conn.Call(ctx, "prompt", p, &out)
	return out, err
}

// Stop has an agent end its turn.
func (c *Client) Stop(ctx context.Context, req StopReq) (Stopped, error) {
	var out Stopped
	err := c.conn.Call(ctx, "stop", req, &out)
	return out, err
}

// Agents asks what agents are now.
func (c *Client) Agents(ctx context.Context, req AgentsReq) (Rows, error) {
	var out Rows
	err := c.conn.Call(ctx, "agents", req, &out)
	return out, err
}

// Sessions lists the store's sessions.
func (c *Client) Sessions(ctx context.Context, req SessionsReq) (SessionList, error) {
	var out SessionList
	err := c.conn.Call(ctx, "sessions", req, &out)
	return out, err
}

// Info reads one session's record.
func (c *Client) Info(ctx context.Context, req InfoReq) (Info, error) {
	var out Info
	err := c.conn.Call(ctx, "info", req, &out)
	return out, err
}

// History reads one page of a transcript's lines.
func (c *Client) History(ctx context.Context, req HistoryReq) (History, error) {
	var out History
	err := c.conn.Call(ctx, "history", req, &out)
	return out, err
}

// HistoryFrom reads a transcript's lines from line from on, page by page,
// none at or past upto when upto is not nil.
func (c *Client) HistoryFrom(ctx context.Context, agent ID, from uint64, upto *uint64) ([]json.RawMessage, error) {
	var lines []json.RawMessage
	req := HistoryReq{Agent: agent, From: from, Upto: upto}
	for {
		page, err := c.History(ctx, req)
		if err != nil {
			return nil, err
		}
		lines = append(lines, page.Lines...)
		if page.EOF || len(page.Lines) == 0 {
			return lines, nil
		}
		end := page.End
		req.From = page.First + uint64(len(page.Lines))
		req.At = &end
	}
}

// Output reads a byte range of a saved output.
func (c *Client) Output(ctx context.Context, req OutputReq) (Output, error) {
	var out Output
	err := c.conn.Call(ctx, "output", req, &out)
	return out, err
}

// OutputAll reads a whole saved output, at most limit bytes of it.
func (c *Client) OutputAll(ctx context.Context, agent ID, name string, limit int) ([]byte, error) {
	var data []byte
	req := OutputReq{Agent: agent, Name: name}
	for {
		page, err := c.Output(ctx, req)
		if err != nil {
			return nil, err
		}
		data = append(data, page.Data...)
		if limit > 0 && len(data) > limit {
			return nil, fmt.Errorf("saved output %s is larger than %d bytes", name, limit)
		}
		if page.EOF || len(page.Data) == 0 {
			return data, nil
		}
		req.Offset = page.Offset + uint64(len(page.Data))
	}
}
