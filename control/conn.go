package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// MaxLineBytes is the longest line either end of a connection reads.
const MaxLineBytes = 512 << 20

// Wire failure kinds every contract shares, beside the control contract's
// refusals.
const (
	KindCancelled     = "cancelled"
	KindUnknownMethod = "unknown_method"
	KindRefused       = "refused"
	KindInternal      = "internal"
)

// RPCError is a request's failure, as its reply said it.
type RPCError struct {
	Method  string
	Code    int
	Message string
	// Kind is what code goes by: a refusal's kind, or one of the wire's.
	Kind string
	// Member is the member of the request a refusal is about.
	Member  string
	Payload json.RawMessage
}

func (e *RPCError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Kind
	}
	if e.Member != "" {
		return fmt.Sprintf("%s: %s (%s: %s)", e.Method, msg, e.Kind, e.Member)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Method, msg, e.Kind)
}

// IsRefusal reports whether err is a refusal of the given kind.
func IsRefusal(err error, kind string) bool {
	var rpc *RPCError
	return errors.As(err, &rpc) && rpc.Kind == kind
}

// ErrClosed is the failure of every request on a connection that ended.
var ErrClosed = errors.New("control connection closed")

// Notification is an event line: a method and its params.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Conn is one end of a JSON-RPC 2.0 connection of newline-delimited lines.
// One goroutine reads it and hands each reply to the request that waits for
// it and each notification, in order, to the handler; any goroutine may
// write, a line at a time.
type Conn struct {
	rwc      io.ReadWriteCloser
	notify   func(Notification)
	wmu      sync.Mutex
	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan rpcReply
	done     chan struct{}
	err      error
	closeErr sync.Once
}

type rpcReply struct {
	result json.RawMessage
	err    *RPCError
}

// NewConn starts reading rwc. notify is called on the reading goroutine for
// each notification, in the order they came; it must not block for long or
// make requests that need the reader.
func NewConn(rwc io.ReadWriteCloser, notify func(Notification)) *Conn {
	c := &Conn{
		rwc:     rwc,
		notify:  notify,
		pending: map[uint64]chan rpcReply{},
		done:    make(chan struct{}),
	}
	go c.read()
	return c
}

// Done is closed once the connection has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err says why the connection ended, once it has.
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close ends the connection: every open request fails.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	return c.rwc.Close()
}

func (c *Conn) fail(err error) {
	c.closeErr.Do(func() {
		c.mu.Lock()
		c.err = err
		pending := c.pending
		c.pending = map[uint64]chan rpcReply{}
		c.mu.Unlock()
		for _, ch := range pending {
			close(ch)
		}
		close(c.done)
	})
}

func (c *Conn) read() {
	r := bufio.NewReaderSize(c.rwc, 256<<10)
	for {
		line, err := readLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = ErrClosed
			}
			c.fail(err)
			_ = c.rwc.Close()
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		c.dispatch(line)
	}
}

// readLine reads one whole line, its newline dropped; a line longer than
// MaxLineBytes ends the connection.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxLineBytes {
			return nil, fmt.Errorf("control line over %d bytes", MaxLineBytes)
		}
		switch {
		case err == nil:
			if buf == nil {
				out := make([]byte, len(chunk)-1)
				copy(out, chunk)
				return out, nil
			}
			return append(buf, chunk[:len(chunk)-1]...), nil
		case errors.Is(err, bufio.ErrBufferFull):
			buf = append(buf, chunk...)
		default:
			// A line cut by the end of the pipe is dropped.
			return nil, err
		}
	}
}

type wireLine struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		} `json:"data"`
	} `json:"error"`
}

func (c *Conn) dispatch(line []byte) {
	var msg wireLine
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if msg.ID == nil {
		if msg.Method != "" && c.notify != nil {
			c.notify(Notification{Method: msg.Method, Params: msg.Params})
		}
		return
	}
	if msg.Method != "" {
		// A request of the far side's: this end serves none.
		c.answerUnknown(*msg.ID, msg.Method)
		return
	}
	id, err := strconv.ParseUint(string(*msg.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	reply := rpcReply{result: msg.Result}
	if msg.Error != nil {
		e := &RPCError{Code: msg.Error.Code, Message: msg.Error.Message}
		if msg.Error.Data != nil {
			e.Kind = msg.Error.Data.Kind
			e.Payload = msg.Error.Data.Payload
			if len(e.Payload) > 0 {
				var p struct {
					Member string `json:"member"`
				}
				if json.Unmarshal(e.Payload, &p) == nil {
					e.Member = p.Member
				}
			}
		}
		reply.err = e
	}
	ch <- reply
}

func (c *Conn) answerUnknown(id json.RawMessage, method string) {
	_ = method
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"this end serves no requests","data":{"kind":"unknown_method"}}}`+"\n", id)
	go func() { _ = c.writeLine([]byte(line)) }()
}

// writeLine writes line and its newline in one write, under the lock, so
// that no copy of it is left in a buffer of the connection's.
func (c *Conn) writeLine(line []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.Err(); err != nil {
		return err
	}
	if _, err := c.rwc.Write(line); err != nil {
		c.fail(err)
		return err
	}
	return nil
}

// send writes a request whose params are already JSON, and returns its id
// and where its reply comes.
func (c *Conn) send(method string, params []byte) (uint64, chan rpcReply, error) {
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return 0, nil, err
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	var line bytes.Buffer
	line.Grow(len(params) + len(method) + 48)
	line.WriteString(`{"jsonrpc":"2.0","id":`)
	line.WriteString(strconv.FormatUint(id, 10))
	line.WriteString(`,"method":`)
	quoted, _ := jsonString(method)
	line.WriteString(quoted)
	line.WriteString(`,"params":`)
	if len(params) == 0 {
		line.WriteString("{}")
	} else {
		line.Write(params)
	}
	line.WriteString("}\n")
	err := c.writeLine(line.Bytes())
	wipe(line.Bytes())
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return 0, nil, err
	}
	return id, ch, nil
}

// CallRaw makes a request with params that are JSON already and returns its
// result's JSON. A ctx that ends first asks the far side to cancel the
// request and returns ctx's error; the reply that still comes is dropped.
func (c *Conn) CallRaw(ctx context.Context, method string, params []byte) (json.RawMessage, error) {
	id, ch, err := c.send(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case reply, ok := <-ch:
		if !ok {
			return nil, c.errOrClosed()
		}
		if reply.err != nil {
			reply.err.Method = method
			return nil, reply.err
		}
		return reply.result, nil
	case <-ctx.Done():
		c.cancel(id)
		return nil, ctx.Err()
	}
}

// Call marshals params, makes the request and unmarshals its result into
// result, when result is not nil.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	raw, err := c.CallRaw(ctx, method, data)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("%s: read the result: %w", method, err)
	}
	return nil
}

// cancel asks the far side to cancel request id, without waiting.
func (c *Conn) cancel(id uint64) {
	params := []byte(`{"request":` + strconv.FormatUint(id, 10) + `}`)
	go func() {
		_, ch, err := c.send("cancel", params)
		if err != nil {
			return
		}
		<-ch
	}()
}

func (c *Conn) errOrClosed() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}

// wipe overwrites b: for lines that held a key.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
