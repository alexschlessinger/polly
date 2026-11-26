package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/rex"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type fakeRexToolRunner struct {
	last rex.CommandSpec
	fn   func(ctx context.Context, spec rex.CommandSpec) (rex.CommandResult, error)
}

func (f *fakeRexToolRunner) Run(ctx context.Context, spec rex.CommandSpec) (rex.CommandResult, error) {
	f.last = spec
	if f.fn != nil {
		return f.fn(ctx, spec)
	}
	return rex.CommandResult{Stdout: []byte("{}")}, nil
}

func (f *fakeRexToolRunner) Start(context.Context, rex.CommandSpec) (rex.StreamProcess, error) {
	return nil, errors.New("unused")
}

func TestRexToolsOptInAndSandboxFailClosed(t *testing.T) {
	// Generic registry must not offer or install any Rex tools.
	generic := NewToolRegistry(nil)
	defer generic.Close()
	for _, name := range RexToolNames {
		if generic.HasNativeTool(name) {
			t.Fatalf("generic registry unexpectedly offers %s", name)
		}
	}

	// Native registry without sandbox or WithUnsafeNoSandbox fails closed.
	strict := NewToolRegistry(nil, WithNativeTools())
	defer strict.Close()
	for _, name := range RexToolNames {
		if !strict.HasNativeTool(name) {
			t.Fatalf("native registry missing %s", name)
		}
		if _, err := strict.LoadToolAuto(name); err == nil {
			t.Fatalf("LoadToolAuto(%s) succeeded without sandbox or WithUnsafeNoSandbox", name)
		}
	}

	// Native registry with sandbox factory loads Rex tools with active sandbox metadata.
	factory := mockSandboxFactory(&mockSandbox{})
	sandboxed := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(factory, sandbox.DefaultConfig()))
	defer sandboxed.Close()
	for _, name := range RexToolNames {
		loaded, err := sandboxed.LoadToolAuto(name)
		if err != nil {
			t.Fatalf("LoadToolAuto(%s) with sandbox: %v", name, err)
		}
		if loaded.Type != "native" || len(loaded.Servers) != 1 || len(loaded.Servers[0].ToolNames) != 1 {
			t.Fatalf("LoadToolAuto(%s) = %+v, want 1 native tool", name, loaded)
		}
		tool, ok := sandboxed.Get(name)
		if !ok {
			t.Fatalf("registry missing loaded tool %s", name)
		}
		capable, active := SandboxState(tool)
		if !capable || !active {
			t.Fatalf("%s SandboxState = (%v, %v), want (true, true)", name, capable, active)
		}
		if name == "rex_inspect" && !ReadsOnly(tool) {
			t.Fatalf("rex_inspect must implement ReadOnlyTool")
		}
		if name != "rex_inspect" && ReadsOnly(tool) {
			t.Fatalf("%s must not claim ReadsOnly", name)
		}
	}
}

func TestRexToolsExecuteWithFakeRunner(t *testing.T) {
	runner := &fakeRexToolRunner{}
	inspect := &rexInspectTool{rexToolBase: rexToolBase{name: "rex_inspect", runner: runner}}
	layout := &rexLayoutTool{rexToolBase: rexToolBase{name: "rex_layout", runner: runner}}
	term := &rexTerminalTool{rexToolBase: rexToolBase{name: "rex_terminal", runner: runner}}
	luaTool := &rexLuaTool{rexToolBase: rexToolBase{name: "rex_lua", runner: runner}}
	apiTool := &rexAPITool{rexToolBase: rexToolBase{name: "rex_api", runner: runner}}
	clientTool := &rexClientTool{rexToolBase: rexToolBase{name: "rex_client", runner: runner}}

	// 1. rex_inspect version
	runner.fn = func(_ context.Context, _ rex.CommandSpec) (rex.CommandResult, error) {
		return rex.CommandResult{Stdout: []byte(`{"rex_version":"0.1.0-preview.21","api_revision":18}`)}, nil
	}
	out, err := inspect.ExecuteOutput(context.Background(), map[string]any{"action": "version"})
	if err != nil {
		t.Fatalf("rex_inspect version: %v", err)
	}
	if !strings.Contains(out.Text, "0.1.0-preview.21") {
		t.Fatalf("unexpected inspect output: %s", out.Text)
	}

	// 2. rex_layout new_split
	runner.fn = func(_ context.Context, spec rex.CommandSpec) (rex.CommandResult, error) {
		if !strings.Contains(string(spec.Stdin), `"direction":"horizontal"`) {
			t.Fatalf("unexpected split stdin: %s", string(spec.Stdin))
		}
		return rex.CommandResult{Stdout: []byte(`{"anchor_id":"b1","block_ids":["b2"],"split_ids":["sp1"],"revision":3}`)}, nil
	}
	if _, err := layout.ExecuteOutput(context.Background(), map[string]any{
		"action":    "new_split",
		"session":   "s1",
		"anchor_id": "b1",
		"direction": "horizontal",
		"side":      "after",
		"label":     "tests",
		"command":   []any{"go", "test", "./..."},
		"keep_open": true,
	}); err != nil {
		t.Fatalf("rex_layout new_split: %v", err)
	}

	// 3. rex_terminal capture returns raw text
	runner.fn = func(_ context.Context, _ rex.CommandSpec) (rex.CommandResult, error) {
		return rex.CommandResult{Stdout: []byte(`{"content":"PASS\n","eof":true,"total":5}`)}, nil
	}
	capOut, err := term.ExecuteOutput(context.Background(), map[string]any{
		"action":  "capture",
		"session": "s1",
		"block":   "b2",
		"trim":    true,
	})
	if err != nil {
		t.Fatalf("rex_terminal capture: %v", err)
	}
	if capOut.Text != "PASS\n" {
		t.Fatalf("capture text = %q, want %q", capOut.Text, "PASS\n")
	}

	// 4. rex_lua
	runner.fn = func(_ context.Context, spec rex.CommandSpec) (rex.CommandResult, error) {
		if string(spec.Stdin) != "return {ok = true}" {
			t.Fatalf("lua stdin = %q", string(spec.Stdin))
		}
		return rex.CommandResult{Stdout: []byte(`{"ok":true}`)}, nil
	}
	luaOut, err := luaTool.ExecuteOutput(context.Background(), map[string]any{
		"script": "return {ok = true}",
		"args":   map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatalf("rex_lua: %v", err)
	}
	if !strings.Contains(luaOut.Text, `"ok": true`) {
		t.Fatalf("unexpected lua output: %s", luaOut.Text)
	}

	// 5. rex_api injects block_id when block is supplied
	runner.fn = func(_ context.Context, spec rex.CommandSpec) (rex.CommandResult, error) {
		var body map[string]any
		if err := json.Unmarshal(spec.Stdin, &body); err != nil {
			t.Fatalf("decode api stdin: %v", err)
		}
		if body["block_id"] != "b2" {
			t.Fatalf("expected injected block_id=b2, got %v", body)
		}
		return rex.CommandResult{Stdout: []byte(`{"status":"ok"}`)}, nil
	}
	if _, err := apiTool.ExecuteOutput(context.Background(), map[string]any{
		"method":  "com.superlogical.terminal.ports",
		"session": "s1",
		"block":   "b2",
	}); err != nil {
		t.Fatalf("rex_api: %v", err)
	}

	// 6. rex_client Remote Control refusal maps to structured ToolError
	runner.fn = func(_ context.Context, _ rex.CommandSpec) (rex.CommandResult, error) {
		return rex.CommandResult{
			ExitCode: 1,
			Stderr:   []byte("Error: Rex.app is not accepting actions. Turn on Remote Control.\n"),
		}, nil
	}
	_, err = clientTool.ExecuteOutput(context.Background(), map[string]any{
		"action":    "perform",
		"client_id": "client:1",
		"name":      "client.sidebar.toggle",
	})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("expected *ToolError, got %T: %v", err, err)
	}
	if toolErr.Code != "remote_control_disabled" {
		t.Fatalf("ToolError.Code = %q, want remote_control_disabled", toolErr.Code)
	}
}

func TestRexToolRebindsAcrossExecutionContext(t *testing.T) {
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	if _, err := parent.LoadToolAuto("rex_inspect"); err != nil {
		t.Fatalf("LoadToolAuto: %v", err)
	}
	bound, omitted, err := parent.BindExecutionContext(ExecutionContext{Root: t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("BindExecutionContext: %v", err)
	}
	defer bound.Close()
	if len(omitted) != 0 {
		t.Fatalf("unexpected omitted tools: %v", omitted)
	}
	if _, ok := bound.Get("rex_inspect"); !ok {
		t.Fatalf("bound registry missing rex_inspect")
	}
}
