// Package controltest is an in-process control daemon for tests: it speaks
// the control contract's lines over a Unix socket, keeps sessions in
// memory, and runs each turn by a script the test gives.
package controltest

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/control"
)

// Session is one session the server keeps.
type Session struct {
	ID       control.ID
	ParentID control.ID
	Name     string
	Parent   string
	Label    string
	Model    string
	LastUsed time.Time
	Lines    []json.RawMessage
	Outputs  map[string][]byte
	Final    *control.Final
	// Running is set while a turn holds the session.
	Running bool
	from    uint64
	cancel  context.CancelFunc
}

// Script runs one turn. It tells what the turn does through t and returns
// how the turn ended; t.Commit keeps lines.
type Script func(ctx context.Context, t *Turn) control.Reply

// Server is a control daemon in the test's process.
type Server struct {
	t      testing.TB
	Socket string
	Script Script

	mu       sync.Mutex
	sessions map[control.ID]*Session
	order    []control.ID
	conns    map[*conn]struct{}
	setups   map[string]bool
	prompts  []control.Prompt
	stops    []control.StopReq
	ver      uint64
	ln       net.Listener
	// Version is what the server's welcome says.
	Version string
	// Instance is the instance its welcome says.
	Instance string
}

// New starts a server on a socket of its own; it ends with the test.
func New(t testing.TB, script Script) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &Server{t: t, Socket: filepath.Join(dir, "s.sock"), Script: script, sessions: map[control.ID]*Session{}, conns: map[*conn]struct{}{}, setups: map[string]bool{}, Version: "test 1", Instance: "00000000000000aa"}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	t.Cleanup(func() { s.Close() })
	go s.accept()
	return s
}

// URL is the backend URL of the server's socket.
func (s *Server) URL() string { return "unix://" + s.Socket }

// Close stops the server and drops every connection.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

// Prompts are the prompts the server was asked, in order.
func (s *Server) Prompts() []control.Prompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.prompts)
}

// Stops are the stops the server was asked, in order.
func (s *Server) Stops() []control.StopReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.stops)
}

// Add keeps a session as it is given, and returns it.
func (s *Server) Add(sess *Session) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ID == "" {
		sess.ID, _ = control.NewID()
	}
	if sess.LastUsed.IsZero() {
		sess.LastUsed = time.Now()
	}
	if sess.Outputs == nil {
		sess.Outputs = map[string][]byte{}
	}
	s.sessions[sess.ID] = sess
	s.order = append(s.order, sess.ID)
	return sess
}

// Get is the session with id, or nil.
func (s *Server) Get(id control.ID) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// Running reports whether a turn holds the session with id.
func (s *Server) Running(id control.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	return sess != nil && sess.Running
}

// Lines is how many lines the session with id holds.
func (s *Server) Lines(id control.ID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[id]; sess != nil {
		return len(sess.Lines)
	}
	return 0
}

// ByName is the session named name, or nil.
func (s *Server) ByName(name string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byNameLocked(name)
}

func (s *Server) byNameLocked(name string) *Session {
	for _, id := range s.order {
		if s.sessions[id].Name == name {
			return s.sessions[id]
		}
	}
	return nil
}

// DropConnections ends every connection as a daemon that fell over would,
// first telling each it fell behind when behind is set.
func (s *Server) DropConnections(behind bool) {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if behind {
			c.notify(control.MethodBehind, json.RawMessage("{}"))
		}
		c.close()
	}
}

func (s *Server) accept() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &conn{s: s, nc: nc, watch: map[control.ID]watching{}}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go c.serve()
	}
}

type watching struct {
	kinds    map[string]bool
	children bool
}

type conn struct {
	s     *Server
	nc    net.Conn
	wmu   sync.Mutex
	watch map[control.ID]watching // guarded by s.mu
	setup string
	gone  bool
}

func (c *conn) close() {
	_ = c.nc.Close()
	c.s.mu.Lock()
	delete(c.s.conns, c)
	c.gone = true
	c.s.mu.Unlock()
}

func (c *conn) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.nc.Write(append(data, '\n'))
}

func (c *conn) notify(method string, params json.RawMessage) {
	c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (c *conn) serve() {
	defer c.close()
	r := bufio.NewReaderSize(c.nc, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req request
		if json.Unmarshal(line, &req) != nil || req.ID == nil {
			continue
		}
		go c.answer(req)
	}
}

func (c *conn) result(id json.RawMessage, v any) {
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
}

func (c *conn) fail(id json.RawMessage, kind, message, member string) {
	data := map[string]any{"kind": kind}
	if member != "" {
		data["payload"] = map[string]string{"member": member}
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": message, "data": data}})
}

func (c *conn) answer(req request) {
	s := c.s
	switch req.Method {
	case "join":
		var j control.JoinReq
		_ = json.Unmarshal(req.Params, &j)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case j.Session != "":
			sess := s.sessions[j.Session]
			if sess == nil || !sess.Running {
				c.result(req.ID, control.Made{NotFound: true})
				return
			}
			c.setup = "ffffffffffffffffffffffffffffffff"
			for k := range s.setups {
				c.setup = k
				break
			}
			c.result(req.ID, control.Made{Made: true, Setup: c.setup})
		case s.setups[j.Setup]:
			c.setup = j.Setup
			c.result(req.ID, control.Made{Made: true})
		case j.ExistingOnly:
			c.result(req.ID, control.Made{NotFound: true})
		default:
			c.setup = j.Setup
			c.result(req.ID, control.Made{NeedStart: true})
		}
	case "start":
		var st control.Start
		if err := json.Unmarshal(req.Params, &st); err != nil {
			c.fail(req.ID, "invalid", "bad start", "")
			return
		}
		s.mu.Lock()
		s.setups[st.Setup.Key] = true
		c.setup = st.Setup.Key
		s.mu.Unlock()
		c.result(req.ID, control.Made{Made: true})
	case "hello":
		var h control.Hello
		_ = json.Unmarshal(req.Params, &h)
		s.mu.Lock()
		known := s.setups[h.Setup]
		s.mu.Unlock()
		if !known {
			c.fail(req.ID, control.RefusedMismatch, "hello is to say the key of the setup this host serves", "setup")
			return
		}
		c.result(req.ID, control.Welcome{Protocol: control.Protocol, Version: s.Version, Setup: h.Setup, Instance: s.Instance})
	case "watch":
		c.watchReq(req)
	case "prompt":
		c.prompt(req)
	case "stop":
		var st control.StopReq
		_ = json.Unmarshal(req.Params, &st)
		s.mu.Lock()
		s.stops = append(s.stops, st)
		sess := s.sessions[st.Agent]
		running := sess != nil && sess.Running && (st.From == nil || *st.From == sess.from)
		if running && sess.cancel != nil {
			sess.cancel()
		}
		s.mu.Unlock()
		c.result(req.ID, control.Stopped{Running: running})
	case "agents":
		c.agents(req)
	case "sessions":
		c.sessionsReq(req)
	case "info":
		var in control.InfoReq
		_ = json.Unmarshal(req.Params, &in)
		s.mu.Lock()
		var sess *Session
		switch {
		case in.ID != "":
			sess = s.sessions[in.ID]
		case in.Name != "":
			sess = s.byNameLocked(in.Name)
		case in.Last:
			sess = s.lastRootLocked()
		}
		if sess == nil {
			s.mu.Unlock()
			c.fail(req.ID, control.RefusedNotFound, "no session is there by that", "name")
			return
		}
		info := control.Info{ID: sess.ID, ParentID: sess.ParentID, Metadata: control.RecordMetadata{Name: sess.Name, LastUsedNs: sess.LastUsed.UnixNano(), Description: sess.Label, Parent: sess.Parent, Model: sess.Model}, Final: sess.Final, Lines: uint64(len(sess.Lines)), InUse: sess.Running}
		s.mu.Unlock()
		c.result(req.ID, info)
	case "history":
		var h control.HistoryReq
		_ = json.Unmarshal(req.Params, &h)
		s.mu.Lock()
		sess := s.sessions[h.Agent]
		if sess == nil {
			s.mu.Unlock()
			c.fail(req.ID, control.RefusedNotFound, "no session is there by that", "agent")
			return
		}
		end := uint64(len(sess.Lines))
		if h.Upto != nil && *h.Upto < end {
			end = *h.Upto
		}
		var lines []json.RawMessage
		if h.From < end {
			lines = slices.Clone(sess.Lines[h.From:end])
		}
		s.mu.Unlock()
		first := min(h.From, end)
		if lines == nil {
			lines = []json.RawMessage{}
		}
		c.result(req.ID, control.History{First: first, Lines: lines, EOF: true, End: first + uint64(len(lines))})
	case "output":
		var o control.OutputReq
		_ = json.Unmarshal(req.Params, &o)
		s.mu.Lock()
		sess := s.sessions[o.Agent]
		var data []byte
		ok := false
		if sess != nil {
			data, ok = sess.Outputs[o.Name]
		}
		s.mu.Unlock()
		if !ok {
			c.fail(req.ID, control.RefusedNotFound, "the session has no saved output of that name", "name")
			return
		}
		from := min(int(o.Offset), len(data))
		to := len(data)
		if o.Limit > 0 && from+int(o.Limit) < to {
			to = from + int(o.Limit)
		}
		c.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"data": base64.StdEncoding.EncodeToString(data[from:to]), "size": len(data), "offset": from, "eof": to == len(data)}})
	case "cancel":
		c.result(req.ID, map[string]bool{"running": false})
	default:
		c.fail(req.ID, "unknown_method", "no such method", "")
	}
}

func (s *Server) lastRootLocked() *Session {
	var last *Session
	for _, id := range s.order {
		sess := s.sessions[id]
		if sess.ParentID != "" {
			continue
		}
		if last == nil || !sess.LastUsed.Before(last.LastUsed) {
			last = sess
		}
	}
	return last
}

func (c *conn) watchReq(req request) {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(req.Params, &raw)
	var agent control.ID
	_ = json.Unmarshal(raw["agent"], &agent)
	s := c.s
	s.mu.Lock()
	if on, ok := raw["on"]; ok && string(on) == "false" {
		delete(c.watch, agent)
		s.mu.Unlock()
		c.result(req.ID, map[string]any{})
		return
	}
	var kinds []string
	_ = json.Unmarshal(raw["kinds"], &kinds)
	if len(kinds) == 0 {
		kinds = []string{control.KindDelta, control.KindMessage, control.KindTool, control.KindNotice, control.KindCheckpoint}
	}
	w := watching{kinds: map[string]bool{}}
	for _, k := range kinds {
		w.kinds[k] = true
	}
	_ = json.Unmarshal(raw["children"], &w.children)
	c.watch[agent] = w
	sess := s.sessions[agent]
	var out map[string]any
	if sess != nil {
		state := control.StateIdle
		if sess.Running {
			state = control.StateRunning
		}
		row := control.Row{ID: sess.ID, Parent: sess.ParentID, Label: sess.Label, Model: sess.Model, State: state, At: uint64(len(sess.Lines))}
		if sess.Running {
			from := sess.from
			row.From = &from
		}
		out = map[string]any{"row": row, "lines": len(sess.Lines)}
	} else {
		out = map[string]any{}
	}
	s.mu.Unlock()
	c.result(req.ID, out)
}

func (c *conn) agents(req request) {
	var raw struct {
		IDs    []control.ID `json:"ids"`
		Parent control.ID   `json:"parent"`
	}
	_ = json.Unmarshal(req.Params, &raw)
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ver++
	rows := []control.Row{}
	var ids []control.ID
	if raw.Parent != "" {
		for _, id := range s.order {
			if s.sessions[id].ParentID == raw.Parent {
				ids = append(ids, id)
			}
		}
	} else {
		ids = raw.IDs
	}
	for i, id := range ids {
		sess := s.sessions[id]
		if sess == nil {
			continue
		}
		state := control.StateDone
		if sess.Running {
			state = control.StateRunning
		} else if sess.Final != nil {
			state = sess.Final.State
		}
		idx := uint64(i)
		rows = append(rows, control.Row{ID: sess.ID, Parent: sess.ParentID, Index: &idx, Label: sess.Label, Model: sess.Model, State: state, At: uint64(len(sess.Lines)), Ver: s.ver})
	}
	out := control.Rows{Ver: s.ver, Rows: rows}
	if raw.Parent != "" {
		out.Total, out.IDs = uint64(len(ids)), ids
	}
	c.result(req.ID, out)
}

func (c *conn) sessionsReq(req request) {
	var in control.SessionsReq
	_ = json.Unmarshal(req.Params, &in)
	s := c.s
	s.mu.Lock()
	list := control.SessionList{Sessions: []control.SessionEntry{}}
	for _, id := range s.order {
		sess := s.sessions[id]
		if in.Filter == "roots" && sess.ParentID != "" {
			continue
		}
		list.Sessions = append(list.Sessions, control.SessionEntry{ID: sess.ID, ParentID: sess.ParentID, Name: sess.Name, LastUsedNs: sess.LastUsed.UnixNano(), Description: sess.Label, Parent: sess.Parent, Model: sess.Model, Lines: uint64(len(sess.Lines)), InUse: sess.Running})
	}
	if last := s.lastRootLocked(); last != nil {
		list.Last = last.ID
	}
	s.mu.Unlock()
	c.result(req.ID, list)
}

func (c *conn) prompt(req request) {
	s := c.s
	var p struct{ control.Prompt }
	if err := json.Unmarshal(req.Params, &p.Prompt); err != nil {
		c.fail(req.ID, control.RefusedInvalid, err.Error(), "")
		return
	}
	named := p.Session
	s.mu.Lock()
	s.prompts = append(s.prompts, p.Prompt)
	var sess *Session
	switch {
	case named.New:
		if named.NewName != "" && s.byNameLocked(named.NewName) != nil {
			s.mu.Unlock()
			c.fail(req.ID, control.RefusedExists, "a session has that name", "session")
			return
		}
		id := named.NewID
		if id == "" {
			id, _ = control.NewID()
		}
		name := named.NewName
		if name == "" {
			name = "made-" + id[:6]
		}
		sess = &Session{ID: id, Name: name, Model: p.Model, Outputs: map[string][]byte{}, LastUsed: time.Now()}
		s.sessions[id] = sess
		s.order = append(s.order, id)
	case named.ID != "":
		sess = s.sessions[named.ID]
	case named.Name != "":
		sess = s.byNameLocked(named.Name)
	case named.Last:
		sess = s.lastRootLocked()
	}
	if sess == nil {
		s.mu.Unlock()
		c.fail(req.ID, control.RefusedNotFound, "no session is there by that", "session")
		return
	}
	if sess.Running {
		s.mu.Unlock()
		c.fail(req.ID, control.RefusedBusy, "the session has a turn running", "session")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess.Running, sess.cancel = true, cancel
	sess.from = uint64(len(sess.Lines))
	sess.Lines = append(sess.Lines, p.Content)
	sess.LastUsed = time.Now()
	s.mu.Unlock()
	t := &Turn{s: s, Session: sess, Prompt: p.Prompt}
	t.emit(sess.ID, "", "", control.KindCheckpoint, map[string]any{"lines": t.committed(), "count": 1})
	reply := control.Reply{Stop: control.StopEndTurn}
	if s.Script != nil {
		reply = s.Script(ctx, t)
	}
	if ctx.Err() != nil && reply.Stop == control.StopEndTurn {
		reply.Stop = control.StopCancelled
	}
	reply.Session, reply.Name = sess.ID, sess.Name
	s.mu.Lock()
	sess.Running, sess.cancel = false, nil
	from, at := sess.from, uint64(len(sess.Lines))
	s.mu.Unlock()
	cancel()
	t.emit(sess.ID, "", "", control.KindEnd, map[string]any{"from": from, "at": at, "reply": reply})
	c.result(req.ID, reply)
}

// Turn is one turn the server runs.
type Turn struct {
	s       *Server
	Session *Session
	Prompt  control.Prompt
	pending int
	answer  uint64
}

func (t *Turn) committed() uint64 {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return uint64(len(t.Session.Lines))
}

// emit tells every connection that watches agent, or watches parent with
// its children, an event of kind.
func (t *Turn) emit(agent, parent control.ID, label, kind string, body map[string]any) {
	params := map[string]any{"agent": agent}
	if parent != "" {
		params["parent"] = parent
		if label != "" {
			params["label"] = label
		}
	}
	for k, v := range body {
		params[k] = v
	}
	data, _ := json.Marshal(params)
	t.s.mu.Lock()
	var to []*conn
	for c := range t.s.conns {
		if w, ok := c.watch[agent]; ok && w.kinds[kind] {
			to = append(to, c)
			continue
		}
		if parent != "" {
			if w, ok := c.watch[parent]; ok && w.children {
				to = append(to, c)
			}
		}
	}
	t.s.mu.Unlock()
	for _, c := range to {
		c.notify(kind, data)
	}
}

// Text streams a piece of answer, commentary or reasoning text.
func (t *Turn) Text(kind, text string) {
	t.emit(t.Session.ID, "", "", control.KindDelta, map[string]any{"kind": kind, "at": t.answer, "text": text})
	if kind == control.TextAnswer {
		t.answer += uint64(len(text))
	}
}

// EndText marks the end of a reply's text.
func (t *Turn) EndText() {
	t.emit(t.Session.ID, "", "", control.KindDelta, map[string]any{"kind": control.TextAnswer, "at": t.answer, "text": "", "last": true})
	t.answer = 0
}

// Message tells a transcript line as it is made; it is kept at the next
// Commit.
func (t *Turn) Message(line any, iteration *uint32, usage *control.LineUsage) {
	raw, _ := json.Marshal(line)
	body := map[string]any{"line": json.RawMessage(raw)}
	if iteration != nil {
		body["iteration"] = *iteration
	}
	if usage != nil {
		body["usage"] = usage
	}
	t.emit(t.Session.ID, "", "", control.KindMessage, body)
	t.s.mu.Lock()
	t.Session.Lines = append(t.Session.Lines, raw)
	t.s.mu.Unlock()
	t.pending++
}

// Commit makes the lines told since the last commit durable.
func (t *Turn) Commit() {
	if t.pending == 0 {
		return
	}
	t.emit(t.Session.ID, "", "", control.KindCheckpoint, map[string]any{"lines": t.committed(), "count": t.pending})
	t.pending = 0
}

// Tool tells a call starting or ending.
func (t *Turn) Tool(ev control.ToolEvent) {
	raw, _ := json.Marshal(ev)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	t.emit(t.Session.ID, "", "", control.KindTool, body)
}

// Notice tells a note beside the conversation.
func (t *Turn) Notice(n control.Notice) {
	raw, _ := json.Marshal(n)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	t.emit(t.Session.ID, "", "", control.KindNotice, body)
}

// Child makes a child session of the turn's, labeled label, with lines,
// and tells its parent's watchers its events.
func (t *Turn) Child(name, label string, lines ...any) *Session {
	child := &Session{ParentID: t.Session.ID, Parent: t.Session.Name, Name: name, Label: label, Model: t.Session.Model}
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		child.Lines = append(child.Lines, raw)
	}
	t.s.Add(child)
	return child
}

// ChildTool tells a child's call starting or ending to its parent's
// watchers.
func (t *Turn) ChildTool(child *Session, ev control.ToolEvent) {
	raw, _ := json.Marshal(ev)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	t.emit(child.ID, t.Session.ID, child.Label, control.KindTool, body)
}

// Assistant is an assistant line with text and optional tool calls.
func Assistant(text string, calls ...control.LineToolCall) control.Line {
	return control.Line{Role: "assistant", Content: text, ToolCalls: calls, StopReason: stopFor(calls)}
}

func stopFor(calls []control.LineToolCall) string {
	if len(calls) > 0 {
		return "tool_use"
	}
	return "end_turn"
}

// ToolResult is a tool line answering call.
func ToolResult(call control.LineToolCall, content string, ok bool) control.Line {
	return control.Line{Role: "tool", ToolCallID: call.ID, ToolName: call.Name, Content: content, ToolSucceeded: &ok}
}

// Say is a script that answers each prompt with text, streamed in words.
func Say(text string) Script {
	return func(ctx context.Context, t *Turn) control.Reply {
		for _, word := range strings.SplitAfter(text, " ") {
			t.Text(control.TextAnswer, word)
		}
		t.EndText()
		iter := uint32(0)
		t.Message(Assistant(text), &iter, &control.LineUsage{InputTokens: 100, OutputTokens: 10, TokensReported: true})
		t.Commit()
		return control.Reply{Stop: control.StopEndTurn, Usage: control.Spent{InputTokens: 100, OutputTokens: 10}, ModelCalls: 1, Context: control.ContextSize{Used: 100, Size: 256000}}
	}
}

// Wait blocks until cond holds or the test's patience ends.
func Wait(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
