package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedCall struct {
	Spec CommandSpec
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []recordedCall
	run   func(ctx context.Context, spec CommandSpec) (CommandResult, error)
	start func(ctx context.Context, spec CommandSpec) (StreamProcess, error)
}

func (f *fakeRunner) Run(ctx context.Context, spec CommandSpec) (CommandResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{Spec: spec})
	fn := f.run
	f.mu.Unlock()
	if fn == nil {
		return CommandResult{Stdout: []byte("{}")}, nil
	}
	return fn(ctx, spec)
}

func (f *fakeRunner) Start(ctx context.Context, spec CommandSpec) (StreamProcess, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{Spec: spec})
	fn := f.start
	f.mu.Unlock()
	if fn == nil {
		return nil, errors.New("start not configured")
	}
	return fn(ctx, spec)
}

func (f *fakeRunner) lastCall() CommandSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return CommandSpec{}
	}
	return f.calls[len(f.calls)-1].Spec
}

type fakeStreamProcess struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter

	mu     sync.Mutex
	killed bool
	done   chan struct{}
	err    error
}

func newFakeStreamProcess() *fakeStreamProcess {
	soR, soW := io.Pipe()
	seR, seW := io.Pipe()
	return &fakeStreamProcess{
		stdoutR: soR,
		stdoutW: soW,
		stderrR: seR,
		stderrW: seW,
		done:    make(chan struct{}),
	}
}

func (p *fakeStreamProcess) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeStreamProcess) Stderr() io.ReadCloser { return p.stderrR }

func (p *fakeStreamProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *fakeStreamProcess) Kill() error {
	p.mu.Lock()
	already := p.killed
	p.killed = true
	p.mu.Unlock()
	if !already {
		_ = p.stdoutW.Close()
		_ = p.stderrW.Close()
		select {
		case <-p.done:
		default:
			close(p.done)
		}
	}
	return nil
}

func (p *fakeStreamProcess) Finish(err error) {
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	_ = p.stdoutW.Close()
	_ = p.stderrW.Close()
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestAmbientResolutionAndRemoteScoping(t *testing.T) {
	lookup := envMap(map[string]string{
		"REX_SESSION": "session:ambient",
		"REX_BLOCK":   "block:ambient",
	})

	local := NewClient(WithEnvLookup(lookup))
	if got := local.EffectiveSession(); got != "session:ambient" {
		t.Fatalf("EffectiveSession = %q, want session:ambient", got)
	}
	if got := local.EffectiveBlock(); got != "block:ambient" {
		t.Fatalf("EffectiveBlock = %q, want block:ambient", got)
	}

	remote := local.ForServer("devbox")
	if got := remote.EffectiveSession(); got != "" {
		t.Fatalf("remote EffectiveSession = %q, want empty", got)
	}
	if got := remote.EffectiveBlock(); got != "" {
		t.Fatalf("remote EffectiveBlock = %q, want empty", got)
	}

	remoteExplicit := remote.ForSession("session:remote").ForBlock("block:remote")
	if got := remoteExplicit.EffectiveSession(); got != "session:remote" {
		t.Fatalf("remoteExplicit EffectiveSession = %q, want session:remote", got)
	}
	if got := remoteExplicit.EffectiveBlock(); got != "block:remote" {
		t.Fatalf("remoteExplicit EffectiveBlock = %q, want block:remote", got)
	}
}

func TestGlobalFlagsAndAPICall(t *testing.T) {
	fr := &fakeRunner{
		run: func(_ context.Context, spec CommandSpec) (CommandResult, error) {
			return CommandResult{Stdout: []byte(`{"rex_version":"0.1.0-preview.21","api_revision":18}`)}, nil
		},
	}
	client := NewClient(
		WithBinary("/opt/rex/rex"),
		WithServer("devbox"),
		WithClient("macbook"),
		WithAutoStart(false),
		WithTimeout(5*time.Second),
		WithCommandRunner(fr),
		WithEnvLookup(envMap(map[string]string{"REX_SESSION": "session:ambient"})),
	)

	ver, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver.RexVersion != "0.1.0-preview.21" || ver.APIRevision != 18 {
		t.Fatalf("unexpected version: %+v", ver)
	}
	got := fr.lastCall()
	if got.Binary != "/opt/rex/rex" {
		t.Fatalf("binary = %q, want /opt/rex/rex", got.Binary)
	}
	wantArgs := []string{"-S", "devbox", "-C", "macbook", "--autostart=false", "--timeout", "5s", "--color", "never", "api", "call", "server.version", "-"}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Fatalf("args = %v, want %v", got.Args, wantArgs)
	}
	if string(got.Stdin) != "{}" {
		t.Fatalf("stdin = %q, want {}", string(got.Stdin))
	}
}

func TestListAndDescribeMethods(t *testing.T) {
	fr := &fakeRunner{
		run: func(_ context.Context, spec CommandSpec) (CommandResult, error) {
			if len(spec.Args) >= 3 && spec.Args[2] == "api" && spec.Args[3] == "list" {
				return CommandResult{Stdout: []byte(`{
					"methods": [{"name":"session.view","description":"View session","input_schema":{"type":"object"}}],
					"block_types": [{"creator":"com.superlogical.terminal","methods":[{"name":"format","args_schema":{"type":"object"},"output_schema":{"type":"object"}}]}]
				}`)}, nil
			}
			return CommandResult{Stdout: []byte(`{"name":"session.new_split","description":"Split","input_schema":{"type":"object"}}`)}, nil
		},
	}
	c := NewClient(WithCommandRunner(fr), WithEnvLookup(envMap(nil)))

	list, err := c.ListMethods(context.Background())
	if err != nil {
		t.Fatalf("ListMethods: %v", err)
	}
	if len(list.Methods) != 1 || list.Methods[0].Name != "session.view" {
		t.Fatalf("unexpected methods: %+v", list.Methods)
	}
	if len(list.BlockTypes) != 1 || len(list.BlockTypes[0].Methods) != 1 {
		t.Fatalf("unexpected block types: %+v", list.BlockTypes)
	}
	if len(list.BlockTypes[0].Methods[0].EffectiveInputSchema()) == 0 {
		t.Fatalf("expected EffectiveInputSchema to fall back to ArgsSchema")
	}

	doc, err := c.DescribeMethod(context.Background(), "session.new_split")
	if err != nil {
		t.Fatalf("DescribeMethod: %v", err)
	}
	if doc.Name != "session.new_split" {
		t.Fatalf("doc.Name = %q", doc.Name)
	}
}

func TestComposableLayoutAndSplit(t *testing.T) {
	fr := &fakeRunner{
		run: func(_ context.Context, spec CommandSpec) (CommandResult, error) {
			return CommandResult{Stdout: []byte(`{"anchor_id":"block:1","block_ids":["block:2","block:3"],"split_ids":["split:1","split:2"],"revision":9}`)}, nil
		},
	}
	c := NewClient(
		WithCommandRunner(fr),
		WithEnvLookup(envMap(map[string]string{"REX_SESSION": "session:1"})),
	)

	layout := HorizontalSplit(0.6,
		ShellLayout("editor", &TerminalOptions{Command: []string{"nvim"}, Cwd: "/work"}),
		VerticalSplit(0.5,
			ShellLayout("tests", &TerminalOptions{Command: []string{"go", "test", "./..."}, Exit: KeepOpen()}),
			ShellLayout("logs", nil),
		),
	)
	res, err := c.NewSplit(context.Background(), NewSplitRequest{
		AnchorID:  "block:1",
		Direction: DirectionHorizontal,
		Side:      SideAfter,
		Ratio:     0.5,
		Layout:    layout,
		Focus:     true,
	})
	if err != nil {
		t.Fatalf("NewSplit: %v", err)
	}
	if len(res.BlockIDs) != 2 || res.Revision != 9 {
		t.Fatalf("unexpected NewSplitResponse: %+v", res)
	}

	call := fr.lastCall()
	wantArgs := []string{"--color", "never", "api", "call", "-s", "session:1", "session.new_split", "-"}
	if !reflect.DeepEqual(call.Args, wantArgs) {
		t.Fatalf("args = %v, want %v", call.Args, wantArgs)
	}
	var payload map[string]any
	if err := json.Unmarshal(call.Stdin, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["direction"] != "horizontal" || payload["side"] != "after" {
		t.Fatalf("unexpected split payload: %v", payload)
	}
	if _, hasSession := payload["session_id"]; hasSession {
		t.Fatalf("session_id should be injected via -s, not duplicated in payload: %v", payload)
	}
}

func TestCaptureBlobPaginationAndSinglePage(t *testing.T) {
	var calls int
	fr := &fakeRunner{
		run: func(_ context.Context, spec CommandSpec) (CommandResult, error) {
			calls++
			method := spec.Args[len(spec.Args)-2]
			switch method {
			case "com.superlogical.terminal.format":
				return CommandResult{Stdout: []byte(`{"content":"hello ","eof":false,"id":42,"total":17}`)}, nil
			case "com.superlogical.terminal.blob_read":
				var req struct {
					BlockID string `json:"block_id"`
					Args    struct {
						ID     int64 `json:"id"`
						Offset int   `json:"offset"`
					} `json:"args"`
				}
				if err := json.Unmarshal(spec.Stdin, &req); err != nil {
					t.Fatalf("unmarshal blob_read: %v", err)
				}
				if req.Args.ID != 42 {
					t.Fatalf("blob id = %d, want 42", req.Args.ID)
				}
				if req.Args.Offset == 6 {
					return CommandResult{Stdout: []byte(`{"data":"world","eof":false,"id":42,"total":17}`)}, nil
				}
				if req.Args.Offset == 11 {
					return CommandResult{Stdout: []byte(`{"data":"! done","eof":true,"id":42,"total":17}`)}, nil
				}
				t.Fatalf("unexpected blob offset %d", req.Args.Offset)
			default:
				t.Fatalf("unexpected method %q", method)
			}
			return CommandResult{}, nil
		},
	}

	c := NewClient(WithCommandRunner(fr), WithSession("session:1"), WithBlock("block:1"))
	res, err := c.Capture(context.Background(), "", "", CaptureOptions{Format: CapturePlain, Trim: true, MaxBytes: 6})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if res.Content != "hello world! done" || !res.EOF || res.Total != 17 {
		t.Fatalf("unexpected CaptureResult: %+v", res)
	}
	if calls != 3 {
		t.Fatalf("expected 3 RPC calls (1 format + 2 blob_read), got %d", calls)
	}

	calls = 0
	page, err := c.Capture(context.Background(), "", "", CaptureOptions{SinglePage: true})
	if err != nil {
		t.Fatalf("Capture SinglePage: %v", err)
	}
	if page.Content != "hello " || page.EOF || calls != 1 {
		t.Fatalf("unexpected single page result: %+v (calls=%d)", page, calls)
	}
}

func TestSendKeyAndWrite(t *testing.T) {
	fr := &fakeRunner{
		run: func(_ context.Context, spec CommandSpec) (CommandResult, error) {
			if strings.Contains(strings.Join(spec.Args, " "), "terminal.write") {
				return CommandResult{Stdout: []byte(`{"bytes_written":5}`)}, nil
			}
			return CommandResult{}, nil
		},
	}
	c := NewClient(WithCommandRunner(fr), WithSession("s1"), WithBlock("b1"))

	n, err := c.Write(context.Background(), "", "", "hello")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5 {
		t.Fatalf("bytes_written = %d, want 5", n)
	}

	if err := c.SendKey(context.Background(), "", "", "ctrl+c", "enter"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}
	wantArgs := []string{"--color", "never", "send-key", "-s", "s1", "-b", "b1", "ctrl+c", "enter"}
	if got := fr.lastCall().Args; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("SendKey args = %v, want %v", got, wantArgs)
	}
}

func TestAPIErrorParsingAndUnwrap(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		stderr   string
		wantCode string
		wantIs   error
	}{
		{
			name:     "bad request status and code",
			exitCode: 1,
			stderr:   "Error: input: validating: block_id is required (status=400 code=bad_request)\n",
			wantCode: "bad_request",
			wantIs:   ErrBadRequest,
		},
		{
			name:     "session not found",
			exitCode: 3,
			stderr:   "Error: session missing (status=404 code=session_not_found)\n",
			wantCode: "session_not_found",
			wantIs:   ErrNotFound,
		},
		{
			name:     "remote control disabled",
			exitCode: 1,
			stderr:   "Error: Rex.app is not accepting actions. Turn on Remote Control in its Settings under Rex Server.\n",
			wantIs:   ErrRemoteControlDisabled,
		},
		{
			name:     "conflict",
			exitCode: 1,
			stderr:   "Error: block limit reached (status=409 code=conflict)\n",
			wantCode: "conflict",
			wantIs:   ErrConflict,
		},
		{
			name:     "unreachable socket",
			exitCode: 1,
			stderr:   "Error: dial unix /tmp/rex/server.sock: connect: connection refused\n",
			wantIs:   ErrUnreachable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{
				run: func(_ context.Context, _ CommandSpec) (CommandResult, error) {
					return CommandResult{ExitCode: tc.exitCode, Stderr: []byte(tc.stderr)}, nil
				},
			}
			c := NewClient(WithCommandRunner(fr))
			_, err := c.Version(context.Background())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *APIError, got %T: %v", err, err)
			}
			if apiErr.Code != tc.wantCode {
				t.Fatalf("Code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.wantIs)
			}
		})
	}
}

func TestEvalLua(t *testing.T) {
	fr := &fakeRunner{
		run: func(_ context.Context, spec CommandSpec) (CommandResult, error) {
			return CommandResult{Stdout: []byte(`{"sum":42}`)}, nil
		},
	}
	c := NewClient(WithCommandRunner(fr), WithSession("s1"), WithBlock("b1"))
	var out struct {
		Sum int `json:"sum"`
	}
	err := c.EvalLua(context.Background(), "return {sum = rex.args.x + 1}", LuaOptions{
		For:      10 * time.Second,
		FailFast: true,
		Args:     map[string]any{"x": 41},
	}, &out)
	if err != nil {
		t.Fatalf("EvalLua: %v", err)
	}
	if out.Sum != 42 {
		t.Fatalf("Sum = %d, want 42", out.Sum)
	}
	call := fr.lastCall()
	wantArgs := []string{"--color", "never", "do", "-s", "s1", "-b", "b1", "--for", "10s", "--fail-fast", "--args", `{"x":41}`, "-"}
	if !reflect.DeepEqual(call.Args, wantArgs) {
		t.Fatalf("args = %v, want %v", call.Args, wantArgs)
	}
	if !bytes.Equal(call.Stdin, []byte("return {sum = rex.args.x + 1}")) {
		t.Fatalf("stdin = %q", string(call.Stdin))
	}
}

func TestSubscribeAndWaitEvent(t *testing.T) {
	proc := newFakeStreamProcess()
	fr := &fakeRunner{
		start: func(_ context.Context, spec CommandSpec) (StreamProcess, error) {
			wantArgs := []string{"--color", "never", "events", "--json", "-s", "s1"}
			if !reflect.DeepEqual(spec.Args, wantArgs) {
				t.Errorf("Subscribe args = %v, want %v", spec.Args, wantArgs)
			}
			return proc, nil
		},
	}
	c := NewClient(WithCommandRunner(fr), WithSession("s1"))

	go func() {
		_, _ = io.WriteString(proc.stdoutW, `{"type":"session_view_changed","session_id":"s1"}`+"\n")
		_, _ = io.WriteString(proc.stdoutW, `{"type":"terminal.child_exited","session_id":"s1","block_id":"b9","data":{"exit_code":0}}`+"\n")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ev, err := c.WaitEvent(ctx, SubscribeOptions{}, func(ev Event) bool {
		return ev.Type == "terminal.child_exited" && ev.BlockID == "b9"
	})
	if err != nil {
		t.Fatalf("WaitEvent: %v", err)
	}
	if ev.BlockID != "b9" || len(ev.Raw) == 0 {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestSubscribeCancellation(t *testing.T) {
	proc := newFakeStreamProcess()
	fr := &fakeRunner{
		start: func(_ context.Context, _ CommandSpec) (StreamProcess, error) {
			return proc, nil
		},
	}
	c := NewClient(WithCommandRunner(fr))
	ctx, cancel := context.WithCancel(context.Background())

	stream, err := c.Subscribe(ctx, SubscribeOptions{All: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("stream.Err() = %v, want context.Canceled", stream.Err())
	}
}

func TestOpenForwardAndClose(t *testing.T) {
	proc := newFakeStreamProcess()
	fr := &fakeRunner{
		start: func(_ context.Context, spec CommandSpec) (StreamProcess, error) {
			wantArgs := []string{"-S", "devbox", "--color", "never", "forward", "open", "--no-open", "-s", "rs1", "--bind", "127.0.0.1", "--port", "8080", "block:srv"}
			if !reflect.DeepEqual(spec.Args, wantArgs) {
				t.Errorf("OpenForward args = %v, want %v", spec.Args, wantArgs)
			}
			return proc, nil
		},
	}
	c := NewClient(WithCommandRunner(fr), WithServer("devbox"), WithSession("rs1"))

	go func() {
		_, _ = io.WriteString(proc.stdoutW, "http://127.0.0.1:54321\n")
	}()

	fwd, err := c.OpenForward(context.Background(), OpenForwardOptions{
		Bind:   "127.0.0.1",
		Port:   8080,
		Target: "block:srv",
	})
	if err != nil {
		t.Fatalf("OpenForward: %v", err)
	}
	if fwd.Address() != "http://127.0.0.1:54321" {
		t.Fatalf("Address = %q, want http://127.0.0.1:54321", fwd.Address())
	}
	if err := fwd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestListActionsArrayAndWrapped(t *testing.T) {
	for _, payload := range []string{
		`[{"name":"pane.split","kind":"client","title":"Split Pane"}]`,
		`{"actions":[{"name":"pane.split","kind":"client","title":"Split Pane"}]}`,
	} {
		fr := &fakeRunner{
			run: func(_ context.Context, _ CommandSpec) (CommandResult, error) {
				return CommandResult{Stdout: []byte(payload)}, nil
			},
		}
		c := NewClient(WithCommandRunner(fr))
		actions, err := c.ListActions(context.Background(), "client:1")
		if err != nil {
			t.Fatalf("ListActions(%s): %v", payload, err)
		}
		if len(actions) != 1 || actions[0].Name != "pane.split" {
			t.Fatalf("unexpected actions: %+v", actions)
		}
	}
}

func TestSessionViewHelpers(t *testing.T) {
	view := &SessionView{
		SessionID:      "s1",
		ActiveWindowID: "w2",
		Windows: []WindowView{
			{WindowID: "w1", FocusedBlockID: "b1"},
			{
				WindowID:       "w2",
				FocusedBlockID: "b2",
				Layers: []LayerView{{
					LayerID: "l1",
					Kind:    "tiled",
					Blocks:  []BlockSummary{{BlockID: "b2", Label: "editor"}},
				}},
			},
		},
	}
	if view.FocusedBlockID() != "b2" {
		t.Fatalf("FocusedBlockID = %q, want b2", view.FocusedBlockID())
	}
	if b := view.FindBlock("editor"); b == nil || b.BlockID != "b2" {
		t.Fatalf("FindBlock(editor) = %+v, want b2", b)
	}
	if b := view.FindBlock("missing"); b != nil {
		t.Fatalf("FindBlock(missing) = %+v, want nil", b)
	}
}
