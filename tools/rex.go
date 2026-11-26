package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/rex"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// RexToolNames lists the opt-in native Rex tool names registered by
// WithNativeTools. None are loaded into a registry until explicitly selected.
var RexToolNames = []string{
	"rex_inspect",
	"rex_layout",
	"rex_terminal",
	"rex_lua",
	"rex_api",
	"rex_client",
}

// sandboxRexRunner routes every finite rex CLI invocation through Polly's
// sandbox factory and process-group tracker.
type sandboxRexRunner struct {
	workDir string
	sb      sandbox.Sandbox
	env     map[string]string
	groups  *processGroups
}

func (r *sandboxRexRunner) Run(ctx context.Context, spec rex.CommandSpec) (rex.CommandResult, error) {
	stdout := newBoundedBuffer(capturedOutputLimit)
	stderr := newBoundedBuffer(capturedOutputLimit)

	dir := spec.Dir
	if dir == "" {
		dir = r.workDir
	}
	env := r.env
	if len(spec.Env) > 0 {
		merged := maps.Clone(r.env)
		if merged == nil {
			merged = make(map[string]string, len(spec.Env))
		}
		maps.Copy(merged, spec.Env)
		env = merged
	}

	_, err := runFiniteCommand(ctx, r.sb, finiteCommand{
		name:   spec.Binary,
		args:   spec.Args,
		stdin:  spec.Stdin,
		dir:    dir,
		env:    env,
		groups: r.groups,
		stdout: stdout,
		stderr: stderr,
	})

	var outBuf, errBuf strings.Builder
	stdout.writeTo(&outBuf)
	stderr.writeTo(&errBuf)
	res := rex.CommandResult{
		Stdout: []byte(outBuf.String()),
		Stderr: []byte(errBuf.String()),
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		if code, ok := shellExitCode(err); ok {
			res.ExitCode = code
			return res, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return res, fmt.Errorf("%w: %w", rex.ErrBinaryNotFound, err)
		}
		return res, err
	}
	return res, nil
}

func (r *sandboxRexRunner) Start(context.Context, rex.CommandSpec) (rex.StreamProcess, error) {
	return nil, fmt.Errorf("streaming rex subprocesses are not supported by finite tool runner")
}

type rexToolBase struct {
	name       string
	workDir    string
	sandbox    sandbox.Sandbox
	sandboxCfg *sandbox.Config
	env        map[string]string
	groups     *processGroups
	runner     rex.CommandRunner
}

func newRexToolBase(registry *ToolRegistry, name string) (rexToolBase, error) {
	if err := registry.requireProcessSandbox(name); err != nil {
		return rexToolBase{}, err
	}
	base := rexToolBase{
		name:    name,
		workDir: registry.executionRoot,
		groups:  registry.processGroups,
	}
	if registry.sandboxFactory == nil {
		if registry.executionPolicy != nil {
			base.env = maps.Clone(registry.executionPolicy.Env)
		}
		return base, nil
	}
	sb, cfg, err := registry.newSandboxFor(name, nil)
	if err != nil {
		return rexToolBase{}, fmt.Errorf("sandbox for %s: %w", name, err)
	}
	base.sandbox = sb
	base.sandboxCfg = copySandboxConfig(&cfg)
	return base, nil
}

func (b *rexToolBase) GetName() string   { return b.name }
func (b *rexToolBase) GetType() string   { return "native" }
func (b *rexToolBase) GetSource() string { return "builtin" }

func (b *rexToolBase) SandboxDetails() SandboxInfo {
	return SandboxInfo{
		Capable: true,
		Active:  b.sandbox != nil,
		Config:  copySandboxConfig(b.sandboxCfg),
	}
}

func (b *rexToolBase) BindExecutionContext(bound *ToolRegistry, _ ExecutionContext) (Tool, error) {
	return rebindNative(bound, b.name)
}

func (b *rexToolBase) clientForArgs(args map[string]any) *rex.Client {
	a := Args(args)
	runner := b.runner
	if runner == nil {
		runner = &sandboxRexRunner{
			workDir: b.workDir,
			sb:      b.sandbox,
			env:     b.env,
			groups:  b.groups,
		}
	}
	opts := []rex.Option{
		rex.WithCommandRunner(runner),
	}
	if server := strings.TrimSpace(a.String("server")); server != "" {
		opts = append(opts, rex.WithServer(server))
	}
	if clientID := strings.TrimSpace(a.String("client_id")); clientID != "" {
		opts = append(opts, rex.WithClient(clientID))
	} else if clientID := strings.TrimSpace(a.String("client")); clientID != "" {
		opts = append(opts, rex.WithClient(clientID))
	}
	if session := strings.TrimSpace(a.String("session")); session != "" {
		opts = append(opts, rex.WithSession(session))
	}
	if block := strings.TrimSpace(a.String("block")); block != "" {
		opts = append(opts, rex.WithBlock(block))
	} else if block := strings.TrimSpace(a.String("block_id")); block != "" {
		opts = append(opts, rex.WithBlock(block))
	}
	return rex.NewClient(opts...)
}

func (b *rexToolBase) sandboxSuffix() string {
	if b.sandbox != nil {
		return " [sandboxed; requires rex preset or socket grant to reach local Rex server]"
	}
	return ""
}

func wrapRexError(err error) error {
	if err == nil {
		return nil
	}
	var te *ToolError
	if errors.As(err, &te) {
		return te
	}
	var apiErr *rex.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.Code
		if code == "" {
			switch {
			case errors.Is(err, rex.ErrRemoteControlDisabled):
				code = "remote_control_disabled"
			case errors.Is(err, rex.ErrNotFound):
				code = "not_found"
			case errors.Is(err, rex.ErrBadRequest):
				code = "bad_request"
			case errors.Is(err, rex.ErrConflict):
				code = "conflict"
			case errors.Is(err, rex.ErrUnreachable):
				code = "unreachable"
			default:
				code = "rex_error"
			}
		}
		return NewToolError(apiErr.Error(), code)
	}
	switch {
	case errors.Is(err, rex.ErrBinaryNotFound):
		return NewToolError(err.Error(), "binary_not_found")
	case errors.Is(err, rex.ErrRemoteControlDisabled):
		return NewToolError(err.Error(), "remote_control_disabled")
	case errors.Is(err, rex.ErrUnreachable):
		return NewToolError(err.Error(), "unreachable")
	default:
		return NewToolError(err.Error(), "rex_error")
	}
}

func jsonToolOutput(v any) (ToolOutput, error) {
	if raw, ok := v.(json.RawMessage); ok {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" {
			return ToolOutput{Text: "{}"}, nil
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			pretty, err := json.MarshalIndent(parsed, "", "  ")
			if err == nil {
				return ToolOutput{Text: string(pretty), Data: parsed}, nil
			}
		}
		return ToolOutput{Text: trimmed}, nil
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ToolOutput{}, NewToolError(fmt.Sprintf("encode result: %v", err), "encode_error")
	}
	return ToolOutput{Text: string(pretty), Data: v}, nil
}

// ---------------------------------------------------------------------------
// 1. rex_inspect (read-only)
// ---------------------------------------------------------------------------

type rexInspectTool struct {
	rexToolBase
}

func (t *rexInspectTool) ReadsOnly() bool { return true }

func (t *rexInspectTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"rex_inspect",
		"Inspect Rex server, sessions, windows, blocks, connected clients, actions, hosts, port forwards, and self-documented API schemas (read-only)"+t.sandboxSuffix(),
		schema.Params{
			"action": schema.Enum("Inspection action to perform",
				"whoami", "version", "status", "sessions", "session_view",
				"blocks", "block_describe", "block_types", "clients", "client_inspect",
				"actions", "hosts", "hosts_check", "forwards", "api_list", "api_describe",
			),
			"server":  schema.S("Optional remote Rex host label or address (-S)"),
			"session": schema.S("Optional session ID or label"),
			"block":   schema.S("Optional block ID (for block_describe)"),
			"client":  schema.S("Optional connected client ID or label (for client_inspect or actions)"),
			"method":  schema.S("RPC method name for api_describe (e.g. session.new_split)"),
			"all":     schema.Bool("List forwards across all sessions (for forwards)"),
		},
		"action",
	)
}

func (t *rexInspectTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

func (t *rexInspectTool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	a := Args(args)
	action := strings.TrimSpace(a.String("action"))
	client := t.clientForArgs(args)

	var (
		res any
		err error
	)
	switch action {
	case "whoami":
		res, err = client.Whoami(ctx)
	case "version":
		res, err = client.Version(ctx)
	case "status":
		res, err = client.Status(ctx)
	case "sessions":
		res, err = client.ListSessions(ctx)
	case "session_view":
		res, err = client.InspectSession(ctx, a.String("session"))
	case "blocks":
		res, err = client.ListBlocks(ctx, a.String("session"))
	case "block_describe":
		res, err = client.DescribeBlock(ctx, a.String("session"), a.String("block"))
	case "block_types":
		res, err = client.BlockTypes(ctx)
	case "clients":
		res, err = client.ListClients(ctx, a.String("session"))
	case "client_inspect":
		res, err = client.InspectClient(ctx, a.String("client"))
	case "actions":
		res, err = client.ListActions(ctx, a.String("client"))
	case "hosts":
		res, err = client.ListHosts(ctx)
	case "hosts_check":
		res, err = client.CheckHosts(ctx)
	case "forwards":
		res, err = client.ListForwards(ctx, a.String("session"), a.Bool("all"))
	case "api_list":
		res, err = client.ListMethods(ctx)
	case "api_describe":
		method := strings.TrimSpace(a.String("method"))
		if method == "" {
			return ToolOutput{}, NewToolError("method is required for api_describe", "invalid_args")
		}
		res, err = client.DescribeMethod(ctx, method)
	default:
		return ToolOutput{}, NewToolError(fmt.Sprintf("unknown rex_inspect action %q", action), "invalid_args")
	}
	if err != nil {
		return ToolOutput{}, wrapRexError(err)
	}
	return jsonToolOutput(res)
}

// ---------------------------------------------------------------------------
// 2. rex_layout (sessions, tabs, splits, blocks, floating layers)
// ---------------------------------------------------------------------------

type rexLayoutTool struct {
	rexToolBase
}

func (t *rexLayoutTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"rex_layout",
		"Manage Rex sessions, tabs (windows), split layouts, detached blocks, pane moves/swaps/resizes/zoom, and floating layers"+t.sandboxSuffix(),
		schema.Params{
			"action": schema.Enum("Layout operation to perform",
				"create_session", "rename_session", "destroy_session",
				"new_window", "rename_window", "close_window", "focus_window", "move_window",
				"new_split", "new_block", "move_block", "swap_blocks", "resize_pane",
				"zoom_block", "focus_block", "focus_direction", "set_block_label",
				"detach_block", "close_block", "float_block", "new_layer",
				"resize_layer", "set_layer_state", "raise_layer", "lower_layer", "close_layer",
			),
			"server":           schema.S("Optional remote Rex host label or address (-S)"),
			"session":          schema.S("Optional session ID or label"),
			"window_id":        schema.S("Target window ID"),
			"before_window_id": schema.S("Window ID to move before (for move_window)"),
			"block_id":         schema.S("Target block ID"),
			"other_block_id":   schema.S("Second block ID (for swap_blocks)"),
			"anchor_id":        schema.S("Anchor block or split ID (for new_split or move_block)"),
			"before_block_id":  schema.S("Left/top block ID (for resize_pane)"),
			"after_block_id":   schema.S("Right/bottom block ID (for resize_pane)"),
			"layer_id":         schema.S("Floating layer ID"),
			"below_layer_id":   schema.S("Optional layer ID to lower below (for lower_layer)"),
			"label":            schema.S("Display label for a session, window, or block"),
			"direction":        schema.Enum("Split or focus direction", "horizontal", "vertical", "left", "right", "up", "down"),
			"side":             schema.Enum("Side for new_split or move_block", "before", "after"),
			"ratio":            map[string]any{"type": "number", "description": "Split ratio in (0, 1)"},
			"focus":            schema.Bool("Focus the created or updated window/block/layer"),
			"zoom":             schema.Bool("Explicit zoom state for zoom_block (omit to toggle)"),
			"wrap":             schema.Bool("Wrap around for focus_direction"),
			"state":            schema.Enum("Floating layer state", "normal", "minimized"),
			"bounds":           map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "description": "Floating layer bounds [x, y, w, h] in window fractions"},
			"flavor":           schema.S("Block flavor (defaults to com.superlogical.terminal.shell)"),
			"command":          schema.Strings("Command and arguments to run in a new terminal block"),
			"cwd":              schema.S("Working directory for a new terminal block"),
			"keep_open":        schema.Bool("Keep a new terminal block open after its command exits"),
			"interactive":      schema.Bool("Load interactive shell startup files when running command"),
			"layout":           map[string]any{"type": "object", "description": "Optional raw Rex Layout tree object overriding command/flavor"},
		},
		"action",
	)
}

func (t *rexLayoutTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

func (t *rexLayoutTool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	a := Args(args)
	action := strings.TrimSpace(a.String("action"))
	client := t.clientForArgs(args)
	sid := a.String("session")

	var (
		res any
		err error
	)
	switch action {
	case "create_session":
		req := rex.CreateSessionRequest{Label: a.String("label")}
		if layout, ok, lerr := layoutFromArgs(args); lerr != nil {
			return ToolOutput{}, NewToolError(lerr.Error(), "invalid_args")
		} else if ok {
			req.InitialWindows = []rex.InitialWindowSpec{{
				WindowLabel: a.String("label"),
				Layout:      layout,
			}}
		}
		res, err = client.CreateSession(ctx, req)
	case "rename_session":
		res, err = client.RenameSession(ctx, sid, a.String("label"))
	case "destroy_session":
		err = client.DestroySession(ctx, sid)
		res = map[string]any{"destroyed": true}
	case "new_window":
		layout, _, lerr := layoutFromArgs(args)
		if lerr != nil {
			return ToolOutput{}, NewToolError(lerr.Error(), "invalid_args")
		}
		if layout == nil {
			layout = defaultShellLayoutFromArgs(args)
		}
		var focusPtr *bool
		if _, has := args["focus"]; has {
			v := a.Bool("focus")
			focusPtr = &v
		}
		res, err = client.NewWindow(ctx, rex.NewWindowRequest{
			SessionID:   sid,
			WindowLabel: a.String("label"),
			Layout:      layout,
			Focus:       focusPtr,
		})
	case "rename_window":
		res, err = client.RenameWindow(ctx, sid, a.String("window_id"), a.String("label"))
	case "close_window":
		res, err = client.CloseWindow(ctx, sid, a.String("window_id"))
	case "focus_window":
		res, err = client.FocusWindow(ctx, sid, a.String("window_id"))
	case "move_window":
		res, err = client.MoveWindow(ctx, sid, a.String("window_id"), a.String("before_window_id"))
	case "new_split":
		layout, _, lerr := layoutFromArgs(args)
		if lerr != nil {
			return ToolOutput{}, NewToolError(lerr.Error(), "invalid_args")
		}
		if layout == nil {
			layout = defaultShellLayoutFromArgs(args)
		}
		dir := a.String("direction")
		if dir == "" {
			dir = rex.DirectionHorizontal
		}
		side := a.String("side")
		if side == "" {
			side = rex.SideAfter
		}
		res, err = client.NewSplit(ctx, rex.NewSplitRequest{
			SessionID: sid,
			AnchorID:  a.String("anchor_id"),
			WindowID:  a.String("window_id"),
			Direction: dir,
			Side:      side,
			Ratio:     floatArg(args, "ratio"),
			Layout:    layout,
			Focus:     a.Bool("focus"),
		})
	case "new_block":
		res, err = client.NewBlock(ctx, rex.NewBlockRequest{
			SessionID: sid,
			Flavor:    a.String("flavor"),
			Label:     a.String("label"),
			Options:   terminalOptionsFromArgs(args),
		})
	case "move_block":
		res, err = client.MoveBlock(ctx, rex.MoveBlockRequest{
			SessionID:     sid,
			BlockID:       a.String("block_id"),
			AnchorBlockID: a.String("anchor_id"),
			Direction:     a.String("direction"),
			Side:          a.String("side"),
			Ratio:         floatArg(args, "ratio"),
		})
	case "swap_blocks":
		res, err = client.SwapBlocks(ctx, sid, a.String("block_id"), a.String("other_block_id"))
	case "resize_pane":
		res, err = client.ResizePane(ctx, rex.ResizePaneRequest{
			SessionID:     sid,
			Direction:     a.String("direction"),
			BeforeBlockID: a.String("before_block_id"),
			AfterBlockID:  a.String("after_block_id"),
			Ratio:         floatArg(args, "ratio"),
		})
	case "zoom_block":
		var zoomPtr *bool
		if _, has := args["zoom"]; has {
			v := a.Bool("zoom")
			zoomPtr = &v
		}
		res, err = client.ZoomBlock(ctx, rex.ZoomBlockRequest{
			SessionID: sid,
			BlockID:   a.String("block_id"),
			WindowID:  a.String("window_id"),
			Zoom:      zoomPtr,
		})
	case "focus_block":
		res, err = client.FocusBlock(ctx, sid, a.String("block_id"))
	case "focus_direction":
		res, err = client.FocusDirection(ctx, sid, a.String("direction"), a.Bool("wrap"))
	case "set_block_label":
		res, err = client.SetBlockLabel(ctx, sid, a.String("block_id"), a.String("label"))
	case "detach_block":
		res, err = client.DetachBlock(ctx, sid, a.String("block_id"))
	case "close_block":
		res, err = client.CloseBlock(ctx, sid, a.String("block_id"))
	case "float_block":
		rect, rerr := rectFromArgs(args, "bounds")
		if rerr != nil {
			return ToolOutput{}, NewToolError(rerr.Error(), "invalid_args")
		}
		res, err = client.FloatBlock(ctx, sid, a.String("block_id"), rect)
	case "new_layer":
		rect, rerr := rectFromArgs(args, "bounds")
		if rerr != nil {
			return ToolOutput{}, NewToolError(rerr.Error(), "invalid_args")
		}
		layout, _, lerr := layoutFromArgs(args)
		if lerr != nil {
			return ToolOutput{}, NewToolError(lerr.Error(), "invalid_args")
		}
		if layout == nil {
			layout = defaultShellLayoutFromArgs(args)
		}
		res, err = client.NewLayer(ctx, rex.NewLayerRequest{
			SessionID: sid,
			WindowID:  a.String("window_id"),
			Bounds:    rect,
			Layout:    layout,
			Focus:     a.Bool("focus"),
		})
	case "resize_layer":
		rect, rerr := rectFromArgs(args, "bounds")
		if rerr != nil {
			return ToolOutput{}, NewToolError(rerr.Error(), "invalid_args")
		}
		res, err = client.ResizeLayer(ctx, sid, a.String("layer_id"), rect)
	case "set_layer_state":
		res, err = client.SetLayerState(ctx, rex.SetLayerStateRequest{
			SessionID: sid,
			LayerID:   a.String("layer_id"),
			State:     a.String("state"),
			Focus:     a.Bool("focus"),
		})
	case "raise_layer":
		res, err = client.RaiseLayer(ctx, sid, a.String("layer_id"))
	case "lower_layer":
		res, err = client.LowerLayer(ctx, sid, a.String("layer_id"), a.String("below_layer_id"))
	case "close_layer":
		res, err = client.CloseLayer(ctx, sid, a.String("layer_id"))
	default:
		return ToolOutput{}, NewToolError(fmt.Sprintf("unknown rex_layout action %q", action), "invalid_args")
	}
	if err != nil {
		return ToolOutput{}, wrapRexError(err)
	}
	return jsonToolOutput(res)
}

// ---------------------------------------------------------------------------
// 3. rex_terminal (capture, write, send-key, process, modes, ports, status)
// ---------------------------------------------------------------------------

type rexTerminalTool struct {
	rexToolBase
}

func (t *rexTerminalTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"rex_terminal",
		"Read and control a Rex terminal block: capture output, write text, send keys, inspect process/modes/ports/program_status/pwd/title/size, resize, clear, reset, or read clipboard"+t.sandboxSuffix(),
		schema.Params{
			"action": schema.Enum("Terminal operation to perform",
				"capture", "write", "send_key", "process", "modes", "ports",
				"program_status", "pwd", "title", "size", "resize", "clear", "reset", "clipboard_read",
			),
			"server":    schema.S("Optional remote Rex host label or address (-S)"),
			"session":   schema.S("Optional session ID or label"),
			"block":     schema.S("Target terminal block ID"),
			"format":    schema.Enum("Capture format", "plain", "html", "vt"),
			"trim":      schema.Bool("Trim trailing blank lines on capture"),
			"unwrap":    schema.Bool("Unwrap soft-wrapped lines on capture"),
			"max_bytes": schema.Int("Optional per-page byte limit for capture"),
			"text":      schema.S("Literal text to write (for write)"),
			"keys":      schema.Strings("Named keys to press (for send_key, e.g. enter, ctrl+c)"),
			"columns":   schema.Int("Terminal columns (for resize)"),
			"rows":      schema.Int("Terminal rows (for resize)"),
		},
		"action",
	)
}

func (t *rexTerminalTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

func (t *rexTerminalTool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	a := Args(args)
	action := strings.TrimSpace(a.String("action"))
	client := t.clientForArgs(args)
	sid := a.String("session")
	bid := a.String("block")

	var (
		res any
		err error
	)
	switch action {
	case "capture":
		capRes, cerr := client.Capture(ctx, sid, bid, rex.CaptureOptions{
			Format:   a.String("format"),
			Trim:     a.Bool("trim"),
			Unwrap:   a.Bool("unwrap"),
			MaxBytes: a.Int("max_bytes", 0),
		})
		if cerr != nil {
			return ToolOutput{}, wrapRexError(cerr)
		}
		return ToolOutput{Text: capRes.Content, Data: capRes}, nil
	case "write":
		n, werr := client.Write(ctx, sid, bid, a.String("text"))
		err = werr
		res = map[string]any{"bytes_written": n}
	case "send_key":
		keys := a.StringSlice("keys")
		if len(keys) == 0 {
			return ToolOutput{}, NewToolError("keys must be a non-empty array of key names", "invalid_args")
		}
		err = client.SendKey(ctx, sid, bid, keys...)
		res = map[string]any{"sent": keys}
	case "process":
		res, err = client.Process(ctx, sid, bid)
	case "modes":
		res, err = client.Modes(ctx, sid, bid)
	case "ports":
		res, err = client.Ports(ctx, sid, bid)
	case "program_status":
		res, err = client.ProgramStatus(ctx, sid, bid)
	case "pwd":
		res, err = client.PWD(ctx, sid, bid)
	case "title":
		res, err = client.Title(ctx, sid, bid)
	case "size":
		res, err = client.Size(ctx, sid, bid)
	case "resize":
		cols, rows := a.Int("columns", 0), a.Int("rows", 0)
		if cols <= 0 || rows <= 0 {
			return ToolOutput{}, NewToolError("columns and rows must be positive integers", "invalid_args")
		}
		err = client.Resize(ctx, sid, bid, rex.TerminalSize{Columns: cols, Rows: rows})
		res = map[string]any{"columns": cols, "rows": rows}
	case "clear":
		err = client.Clear(ctx, sid, bid)
		res = map[string]any{"cleared": true}
	case "reset":
		err = client.Reset(ctx, sid, bid)
		res = map[string]any{"reset": true}
	case "clipboard_read":
		res, err = client.ClipboardRead(ctx, sid, bid)
	default:
		return ToolOutput{}, NewToolError(fmt.Sprintf("unknown rex_terminal action %q", action), "invalid_args")
	}
	if err != nil {
		return ToolOutput{}, wrapRexError(err)
	}
	return jsonToolOutput(res)
}

// ---------------------------------------------------------------------------
// 4. rex_lua (atomic multi-step and event-waiting Lua scripting via rex do -)
// ---------------------------------------------------------------------------

type rexLuaTool struct {
	rexToolBase
}

func (t *rexLuaTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"rex_lua",
		"Execute a Rex Lua script via 'rex do -' with full access to rex.session.*, rex.block.call, rex.layout, rex.wait, timers, and JSON arguments/return values"+t.sandboxSuffix(),
		schema.Params{
			"script":      schema.S("Lua script source to execute"),
			"args":        map[string]any{"type": "object", "description": "Optional JSON arguments exposed as rex.args"},
			"server":      schema.S("Optional remote Rex host label or address (-S)"),
			"session":     schema.S("Optional session ID or label"),
			"block":       schema.S("Optional block ID"),
			"all":         schema.Bool("Hear events across all sessions (--all)"),
			"for_seconds": schema.Int("Optional maximum runtime in seconds (--for)"),
			"fail_fast":   schema.Bool("Stop immediately on first handler error (--fail-fast)"),
			"action":      schema.S("Optional action defined by the script to invoke (--action)"),
		},
		"script",
	)
}

func (t *rexLuaTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

func (t *rexLuaTool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	a := Args(args)
	script := a.String("script")
	if strings.TrimSpace(script) == "" {
		return ToolOutput{}, NewToolError("script must be non-empty", "invalid_args")
	}
	var luaArgs map[string]any
	if raw, ok := args["args"].(map[string]any); ok {
		luaArgs = raw
	}
	var dur time.Duration
	if secs := a.Int("for_seconds", 0); secs > 0 {
		dur = time.Duration(secs) * time.Second
	}
	raw, err := t.clientForArgs(args).EvalLuaRaw(ctx, script, rex.LuaOptions{
		SessionID: a.String("session"),
		BlockID:   a.String("block"),
		All:       a.Bool("all"),
		For:       dur,
		FailFast:  a.Bool("fail_fast"),
		Action:    a.String("action"),
		Args:      luaArgs,
	})
	if err != nil {
		return ToolOutput{}, wrapRexError(err)
	}
	return jsonToolOutput(raw)
}

// ---------------------------------------------------------------------------
// 5. rex_api (generic RPC dispatch for any server or block method)
// ---------------------------------------------------------------------------

type rexAPITool struct {
	rexToolBase
}

func (t *rexAPITool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"rex_api",
		"Call any Rex server or block RPC method directly via 'rex api call' with JSON stdin arguments (use rex_inspect action=api_list/api_describe to discover schemas)"+t.sandboxSuffix(),
		schema.Params{
			"method":  schema.S("Full RPC method name (e.g. session.view, session.resize_pane, com.superlogical.terminal.ports)"),
			"args":    map[string]any{"type": "object", "description": "JSON argument object for the method"},
			"server":  schema.S("Optional remote Rex host label or address (-S)"),
			"session": schema.S("Optional session ID or label (-s)"),
			"block":   schema.S("Optional block ID injected as block_id when omitted from args"),
		},
		"method",
	)
}

func (t *rexAPITool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

func (t *rexAPITool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	a := Args(args)
	method := strings.TrimSpace(a.String("method"))
	if method == "" {
		return ToolOutput{}, NewToolError("method is required", "invalid_args")
	}
	payload := map[string]any{}
	if m, ok := args["args"].(map[string]any); ok {
		maps.Copy(payload, m)
	}
	if bid := strings.TrimSpace(a.String("block")); bid != "" {
		if _, exists := payload["block_id"]; !exists {
			payload["block_id"] = bid
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ToolOutput{}, NewToolError(fmt.Sprintf("encode args: %v", err), "invalid_args")
	}
	raw, err := t.clientForArgs(args).CallRaw(ctx, a.String("session"), method, encoded)
	if err != nil {
		return ToolOutput{}, wrapRexError(err)
	}
	return jsonToolOutput(raw)
}

// ---------------------------------------------------------------------------
// 6. rex_client (connected Rex app actions & init.lua actions)
// ---------------------------------------------------------------------------

type rexClientTool struct {
	rexToolBase
}

func (t *rexClientTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"rex_client",
		"List connected Rex apps, inspect their UI actions, perform client UI actions (requires Remote Control), or invoke server/init.lua actions"+t.sandboxSuffix(),
		schema.Params{
			"action":     schema.Enum("Client operation to perform", "list_clients", "inspect_client", "list_actions", "perform", "invoke"),
			"server":     schema.S("Optional remote Rex host label or address (-S)"),
			"client_id":  schema.S("Connected client ID or label"),
			"session":    schema.S("Optional session ID or label"),
			"block":      schema.S("Optional block ID"),
			"name":       schema.S("Action name to perform or invoke (e.g. session.select, client.sidebar.toggle)"),
			"args":       map[string]any{"type": "object", "description": "Action arguments object"},
			"timeout_ms": schema.Int("Optional timeout in milliseconds (for perform)"),
		},
		"action",
	)
}

func (t *rexClientTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

func (t *rexClientTool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	a := Args(args)
	action := strings.TrimSpace(a.String("action"))
	client := t.clientForArgs(args)
	var actionArgs map[string]any
	if m, ok := args["args"].(map[string]any); ok {
		actionArgs = m
	}

	var (
		res any
		err error
	)
	switch action {
	case "list_clients":
		res, err = client.ListClients(ctx, a.String("session"))
	case "inspect_client":
		res, err = client.InspectClient(ctx, a.String("client_id"))
	case "list_actions":
		res, err = client.ListActions(ctx, a.String("client_id"))
	case "perform":
		res, err = client.PerformClientAction(ctx, rex.PerformActionRequest{
			ClientID:  a.String("client_id"),
			Action:    a.String("name"),
			Args:      actionArgs,
			SessionID: a.String("session"),
			BlockID:   a.String("block"),
			TimeoutMS: a.Int("timeout_ms", 0),
		})
	case "invoke":
		res, err = client.InvokeAction(ctx, rex.InvokeActionRequest{
			Action:    a.String("name"),
			Args:      actionArgs,
			SessionID: a.String("session"),
			BlockID:   a.String("block"),
			Server:    a.String("server"),
		})
	default:
		return ToolOutput{}, NewToolError(fmt.Sprintf("unknown rex_client action %q", action), "invalid_args")
	}
	if err != nil {
		return ToolOutput{}, wrapRexError(err)
	}
	return jsonToolOutput(res)
}

// ---------------------------------------------------------------------------
// Helpers & native tool registration
// ---------------------------------------------------------------------------

func registerRexNativeTools(r *ToolRegistry) {
	r.nativeTools["rex_inspect"] = func(registry *ToolRegistry) (Tool, error) {
		base, err := newRexToolBase(registry, "rex_inspect")
		if err != nil {
			return nil, err
		}
		return &rexInspectTool{rexToolBase: base}, nil
	}
	r.nativeTools["rex_layout"] = func(registry *ToolRegistry) (Tool, error) {
		base, err := newRexToolBase(registry, "rex_layout")
		if err != nil {
			return nil, err
		}
		return &rexLayoutTool{rexToolBase: base}, nil
	}
	r.nativeTools["rex_terminal"] = func(registry *ToolRegistry) (Tool, error) {
		base, err := newRexToolBase(registry, "rex_terminal")
		if err != nil {
			return nil, err
		}
		return &rexTerminalTool{rexToolBase: base}, nil
	}
	r.nativeTools["rex_lua"] = func(registry *ToolRegistry) (Tool, error) {
		base, err := newRexToolBase(registry, "rex_lua")
		if err != nil {
			return nil, err
		}
		return &rexLuaTool{rexToolBase: base}, nil
	}
	r.nativeTools["rex_api"] = func(registry *ToolRegistry) (Tool, error) {
		base, err := newRexToolBase(registry, "rex_api")
		if err != nil {
			return nil, err
		}
		return &rexAPITool{rexToolBase: base}, nil
	}
	r.nativeTools["rex_client"] = func(registry *ToolRegistry) (Tool, error) {
		base, err := newRexToolBase(registry, "rex_client")
		if err != nil {
			return nil, err
		}
		return &rexClientTool{rexToolBase: base}, nil
	}
}

func layoutFromArgs(args map[string]any) (*rex.Layout, bool, error) {
	raw, ok := args["layout"]
	if !ok || raw == nil {
		return nil, false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false, fmt.Errorf("encode layout: %w", err)
	}
	var layout rex.Layout
	if err := json.Unmarshal(encoded, &layout); err != nil {
		return nil, false, fmt.Errorf("decode layout: %w", err)
	}
	return &layout, true, nil
}

func terminalOptionsFromArgs(args map[string]any) *rex.TerminalOptions {
	a := Args(args)
	cmd := a.StringSlice("command")
	cwd := a.String("cwd")
	keepOpen := a.Bool("keep_open")
	interactive := a.Bool("interactive")
	if len(cmd) == 0 && cwd == "" && !keepOpen && !interactive {
		return nil
	}
	opts := &rex.TerminalOptions{
		Command:     cmd,
		Cwd:         cwd,
		Interactive: interactive,
	}
	if keepOpen {
		opts.Exit = rex.KeepOpen()
	}
	return opts
}

func defaultShellLayoutFromArgs(args map[string]any) *rex.Layout {
	a := Args(args)
	flavor := a.String("flavor")
	if flavor == "" || flavor == rex.FlavorTerminalShell {
		return rex.ShellLayout(a.String("label"), terminalOptionsFromArgs(args))
	}
	return rex.BlockLayout(flavor, a.String("label"), nil)
}

func floatArg(args map[string]any, key string) float64 {
	v, ok := args[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}

func rectFromArgs(args map[string]any, key string) (rex.Rect, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return rex.Rect{}, fmt.Errorf("%s is required as [x, y, w, h]", key)
	}
	items, ok := raw.([]any)
	if !ok || len(items) != 4 {
		return rex.Rect{}, fmt.Errorf("%s must be a 4-element number array [x, y, w, h]", key)
	}
	var rect rex.Rect
	for i, item := range items {
		switch n := item.(type) {
		case float64:
			rect[i] = n
		case int:
			rect[i] = float64(n)
		case json.Number:
			f, err := n.Float64()
			if err != nil {
				return rex.Rect{}, fmt.Errorf("%s[%d]: %w", key, i, err)
			}
			rect[i] = f
		default:
			return rex.Rect{}, fmt.Errorf("%s[%d] must be a number", key, i)
		}
	}
	return rect, nil
}
