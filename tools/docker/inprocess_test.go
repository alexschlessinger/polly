package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/helper"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestMain(m *testing.M) {
	sandbox.EnterHelperMode()
	os.Exit(m.Run())
}

// inProcess runs the helper as a goroutine over pipes: the whole host path
// is exercised with no container and no Docker.
type inProcess struct {
	session      *session
	helperWriter *io.PipeWriter
	done         chan struct{}
	err          error
}

func startInProcessHelper(t *testing.T, opts helper.Options, heartbeat time.Duration) *inProcess {
	t.Helper()
	hostReader, helperWriter := io.Pipe()
	helperReader, hostWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	p := &inProcess{helperWriter: helperWriter, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.err = helper.Serve(ctx, helperReader, helperWriter, opts)
		helperWriter.Close()
	}()
	closeStream := func() error {
		cancel()
		return hostWriter.Close()
	}
	p.session = newSession(protocol.NewConn(hostReader, hostWriter), closeStream, heartbeat)
	t.Cleanup(func() {
		p.session.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("helper did not exit")
		}
	})
	return p
}

type helloOptions struct {
	scratch   string
	readOnly  bool
	env       []string
	passEnv   []string
	home      string
	gitIdent  protocol.GitIdentity
	sourceDir string
}

func openHelper(t *testing.T, p *inProcess, root string, o helloOptions, load protocol.Load, allow []string) tools.ToolBinding {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	welcome, err := p.session.hello(ctx, protocol.Hello{Protocol: protocol.Version, Mode: "bind", Root: root, SourceRoot: o.sourceDir, Scratch: o.scratch, ReadOnly: o.readOnly, Env: o.env, PassEnv: o.passEnv, Home: o.home, GitIdent: o.gitIdent})
	if err != nil {
		t.Fatal(err)
	}
	if welcome.Protocol != protocol.Version || welcome.UID != os.Getuid() {
		t.Fatalf("welcome %+v", welcome)
	}
	load.AllowedTools = allow
	loaded, err := p.session.load(ctx, load)
	if err != nil {
		t.Fatal(err)
	}
	_, binding := bindSession(tools.ToolScope{Root: root, AllowedTools: allow}, p.session, loaded)
	t.Cleanup(func() { binding.Close() })
	return binding
}

func nativeSpecs(names ...string) protocol.Load {
	var load protocol.Load
	for _, name := range names {
		load.Tools = append(load.Tools, protocol.ToolSpec{Name: name, Type: "native", Source: "builtin"})
	}
	return load
}

func run(t *testing.T, binding tools.ToolBinding, name string, args map[string]any) (tools.ToolOutput, error) {
	t.Helper()
	tool, ok := binding.Registry.Get(name)
	if !ok {
		t.Fatalf("%s is not served", name)
	}
	execution, err := binding.Registry.ExecuteTool(context.Background(), tool, args, 30*time.Second)
	return execution.Output, err
}

func bash(t *testing.T, binding tools.ToolBinding, command string) (string, error) {
	t.Helper()
	output, err := run(t, binding, "bash", map[string]any{"command": command})
	return output.Text, err
}

func servedNames(binding tools.ToolBinding) []string {
	var names []string
	for _, tool := range binding.Registry.All() {
		names = append(names, tool.GetName())
	}
	slices.Sort(names)
	return names
}

// A tool the helper can serve beyond the loaders: independent of the
// execution context, so the native binding keeps it.
type probeTool struct {
	tools.NativeTool
	name string
	run  func(context.Context, map[string]any) (tools.ToolOutput, error)
}

func (p *probeTool) GetName() string               { return p.name }
func (p *probeTool) ContextIndependent() bool      { return true }
func (p *probeTool) GetSchema() *schema.ToolSchema { return schema.Tool(p.name, "probe", nil) }
func (p *probeTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	out, err := p.run(ctx, args)
	return out.Text, err
}
func (p *probeTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	return p.run(ctx, args)
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.NRGBA{B: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHelloProtocolMismatchEndsTheHelper(t *testing.T) {
	p := startInProcessHelper(t, helper.Options{}, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.session.hello(ctx, protocol.Hello{Protocol: protocol.Version + 1, Root: t.TempDir()})
	var failure *ProtocolError
	if !errors.As(err, &failure) || failure.Code != protocol.CodeProtocol {
		t.Fatalf("mismatch = %v", err)
	}
	select {
	case <-p.done:
		if !errors.Is(p.err, helper.ErrProtocolMismatch) {
			t.Fatalf("helper exit = %v", p.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper kept serving after a protocol mismatch")
	}
}

func TestLoadServesNativeToolsWithMetadataAndIdentity(t *testing.T) {
	root, home := t.TempDir(), filepath.Join(t.TempDir(), "home")
	p := startInProcessHelper(t, helper.Options{Version: "test-build", Instructions: func(r *tools.ToolRegistry) string { return "GUIDANCE for " + r.ExecutionRoot() }}, 0)
	binding := openHelper(t, p, root, helloOptions{home: home, gitIdent: protocol.GitIdentity{Name: "Polly Test", Email: "polly@example.invalid"}}, nativeSpecs("bash", "read_file", "write_file", "edit_file", "list_dir"), nil)
	if got := servedNames(binding); !slices.Equal(got, []string{"bash", "edit_file", "list_dir", "read_file", "view_image", "write_file"}) {
		t.Fatalf("served %v", got)
	}
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	if binding.Instructions != "GUIDANCE for "+canonicalRoot {
		t.Fatalf("instructions = %q", binding.Instructions)
	}
	bashTool, _ := binding.Registry.Get("bash")
	proxy := bashTool.(*proxyTool)
	if !proxy.info.Output || proxy.info.Untimed || proxy.info.Exclusive || proxy.info.Coordinates || proxy.GetType() != "native" || proxy.GetSchema().Title() != "bash" {
		t.Fatalf("bash metadata %+v", proxy.info)
	}
	if details := tools.SandboxDetails(bashTool); !details.Capable || !details.Active {
		t.Fatalf("bash sandbox details %+v", details)
	}
	if !binding.Registry.Builtin("view_image") {
		t.Fatal("view_image lost its built-in standing across the wire")
	}
	config, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil || !strings.Contains(string(config), "name = Polly Test") || !strings.Contains(string(config), "gpgsign = false") {
		t.Fatalf("git identity %q, %v", config, err)
	}
}

func TestBashRunsInTheRootWithSealedEnvironmentAndScratch(t *testing.T) {
	root, scratch := t.TempDir(), t.TempDir()
	t.Setenv("PASSED_TOKEN", "pass")
	t.Setenv("OTHER_TOKEN", "strip")
	p := startInProcessHelper(t, helper.Options{}, 0)
	binding := openHelper(t, p, root, helloOptions{scratch: scratch, env: []string{"SEALED_VALUE=from-host"}, passEnv: []string{"PASSED_TOKEN"}}, nativeSpecs("bash", "write_file"), nil)
	text, err := bash(t, binding, `printf '%s|%s|%s|%s\n' "$SEALED_VALUE" "$PASSED_TOKEN" "$OTHER_TOKEN" "$TMPDIR"; pwd`)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	canonicalScratch, _ := filepath.EvalSymlinks(scratch)
	if !strings.HasPrefix(text, "from-host|pass||"+canonicalScratch+"\n") || !strings.Contains(text, canonicalRoot) {
		t.Fatalf("bash output %q", text)
	}
	if output, err := run(t, binding, "write_file", map[string]any{"path": "made.txt", "content": "hello"}); err != nil || !strings.Contains(output.Text, "made.txt") {
		t.Fatalf("write_file = %+v, %v", output, err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "made.txt")); err != nil || string(content) != "hello" {
		t.Fatalf("file not written through the helper: %q %v", content, err)
	}
}

func TestErrorsAndDataRoundTrip(t *testing.T) {
	root := t.TempDir()
	probe := &probeTool{name: "probe_error", run: func(context.Context, map[string]any) (tools.ToolOutput, error) {
		return tools.ToolOutput{Text: "partial"}, tools.NewToolError("probe failed", "probe_code")
	}}
	p := startInProcessHelper(t, helper.Options{ExtraTools: []tools.Tool{probe}}, 0)
	binding := openHelper(t, p, root, helloOptions{}, nativeSpecs("bash"), nil)
	output, err := run(t, binding, "bash", map[string]any{"command": "echo out; exit 3"})
	var commandErr *tools.CommandError
	if !errors.As(err, &commandErr) || commandErr.ExitCode != 3 || output.Data != (tools.CommandResult{ExitCode: 3}) || !strings.Contains(output.Text, "out") {
		t.Fatalf("command failure = %+v, %v", output, err)
	}
	output, err = run(t, binding, "probe_error", nil)
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != "probe_code" || toolErr.Message != "probe failed" || output.Text != "partial" {
		t.Fatalf("tool failure = %+v, %v", output, err)
	}
	if formatted, ok := tools.FormatToolError(err); !ok || !strings.Contains(formatted, "probe failed") {
		t.Fatalf("formatted tool error = %q, %v", formatted, ok)
	}
}

func TestCancelKillsTheProcessGroupAndKeepsTheStream(t *testing.T) {
	root := t.TempDir()
	p := startInProcessHelper(t, helper.Options{}, 0)
	binding := openHelper(t, p, root, helloOptions{}, nativeSpecs("bash"), nil)
	tool, _ := binding.Registry.Get("bash")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	execution, err := binding.Registry.ExecuteTool(ctx, tool, map[string]any{"command": "sleep 31 & sleep 31; wait"}, 0)
	if !errors.Is(err, context.Canceled) && execution.ContextErr != context.Canceled {
		t.Fatalf("cancel outcome %+v, %v", execution, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancel took %v", time.Since(start))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", "sleep 31").Output()
		if len(bytes.TrimSpace(out)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group survived cancellation: %s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if text, err := bash(t, binding, "echo alive"); err != nil || !strings.Contains(text, "alive") {
		t.Fatalf("stream after cancel: %q %v", text, err)
	}
}

func TestDeadlineIsForwardedAndParallelCallsCorrelate(t *testing.T) {
	root := t.TempDir()
	p := startInProcessHelper(t, helper.Options{}, 0)
	binding := openHelper(t, p, root, helloOptions{}, nativeSpecs("bash"), nil)
	tool, _ := binding.Registry.Get("bash")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	execution, err := binding.Registry.ExecuteTool(ctx, tool, map[string]any{"command": "sleep 5"}, 0)
	if !errors.Is(err, context.DeadlineExceeded) && execution.ContextErr != context.DeadlineExceeded {
		t.Fatalf("deadline outcome %+v, %v", execution, err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("deadline took %v", time.Since(start))
	}

	var wg sync.WaitGroup
	results := make([]string, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			delay := []string{"0.4", "0.1", "0.3", "0.2"}[i]
			text, err := bash(t, binding, "sleep "+delay+"; echo call-"+delay)
			if err != nil {
				t.Error(err)
			}
			results[i] = strings.TrimSpace(text)
		}()
	}
	wg.Wait()
	if !slices.Equal(results, []string{"call-0.4", "call-0.1", "call-0.3", "call-0.2"}) {
		t.Fatalf("parallel results %v", results)
	}
}

func TestMediaAndPipefailRoundTrip(t *testing.T) {
	root := t.TempDir()
	data := pngBytes(t)
	probe := &probeTool{name: "probe_image", run: func(context.Context, map[string]any) (tools.ToolOutput, error) {
		return tools.ToolOutput{Text: "here", Data: map[string]any{"count": 2}, Media: []tools.ToolMedia{{Data: data, MIMEType: "image/png", Name: "probe.png", Reference: "ref-1"}}}, nil
	}}
	p := startInProcessHelper(t, helper.Options{ExtraTools: []tools.Tool{probe}}, 0)
	binding := openHelper(t, p, root, helloOptions{}, nativeSpecs("bash"), nil)
	output, err := run(t, binding, "probe_image", nil)
	if err != nil || len(output.Media) != 1 || !bytes.Equal(output.Media[0].Data, data) || output.Media[0].MIMEType != "image/png" || output.Media[0].Name != "probe.png" || output.Media[0].Reference != "ref-1" {
		t.Fatalf("media = %+v, %v", output, err)
	}
	if count, _ := output.Data.(map[string]any)["count"].(float64); count != 2 {
		t.Fatalf("data = %#v", output.Data)
	}
	tool, _ := binding.Registry.Get("bash")
	if _, err := binding.Registry.ExecuteTool(context.Background(), tool, map[string]any{"command": "false | true"}, time.Minute); err != nil {
		t.Fatalf("pipeline without pipefail failed: %v", err)
	}
	var commandErr *tools.CommandError
	if _, err := binding.Registry.ExecuteTool(tools.WithPipefail(context.Background()), tool, map[string]any{"command": "false | true"}, time.Minute); !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
		t.Fatalf("pipefail was not forwarded: %v", err)
	}
}

func writeSkill(t *testing.T, root, name, allowedTools string, mcpURL string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: test skill\n"
	if allowedTools != "" {
		content += "allowed-tools: " + allowedTools + "\n"
	}
	content += "---\nUse the skill.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if mcpURL != "" {
		config := map[string]any{"mcpServers": map[string]any{"srv": map[string]any{"transport": "streamable", "url": mcpURL, "contextIndependent": true}}}
		encoded, _ := json.Marshal(config)
		if err := os.WriteFile(filepath.Join(dir, "mcp", "server.json"), encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mcpServerURL(t *testing.T, names ...string) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "docker-test", Version: "1"}, nil)
	for _, name := range names {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged reply"}}}, nil
		})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

// An activation the model performs inside the container stages its skill
// allowance on the host, applied by the host's own commit, the way a native
// activation is. Tools a later activation loads stay bounded by the
// binding's view, as natively.
func TestSkillActivationMirrorsTheAllowance(t *testing.T) {
	root, skillRoot := t.TempDir(), t.TempDir()
	writeSkill(t, skillRoot, "helper-skill", "bash", "")
	p := startInProcessHelper(t, helper.Options{}, 0)
	load := nativeSpecs("bash", "read_file")
	load.SkillRoots = []string{skillRoot}
	binding := openHelper(t, p, root, helloOptions{}, load, nil)
	if !strings.Contains(binding.ToolInstructions, "helper-skill") {
		t.Fatalf("tool instructions %q", binding.ToolInstructions)
	}
	registry := binding.Registry
	for _, name := range []string{"activate_skill", "read_skill_file", "bash", "read_file"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("%s is not served: %v", name, servedNames(binding))
		}
	}
	if output, err := run(t, binding, "read_skill_file", map[string]any{"skill": "helper-skill", "path": "SKILL.md"}); err != nil || !strings.Contains(output.Text, "Use the skill") {
		t.Fatalf("read_skill_file = %+v, %v", output, err)
	}
	if _, err := run(t, binding, "activate_skill", map[string]any{"name": "helper-skill"}); err != nil {
		t.Fatal(err)
	}
	// The allowance is invisible until this registry commits.
	if _, ok := registry.Get("read_file"); !ok {
		t.Fatal("allowance applied before commit")
	}
	registry.CommitPendingChanges()
	if _, ok := registry.Get("read_file"); ok {
		t.Fatal("read_file survived the skill's allow-list")
	}
	for _, name := range []string{"bash", "activate_skill", "read_skill_file", "view_image"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("%s not allowed after activation: %v", name, servedNames(binding))
		}
	}
}

func TestMirrorStagesToolsForTheNextCommit(t *testing.T) {
	m := &mirror{registry: tools.NewToolRegistry(nil)}
	defer m.registry.Close()
	m.add(protocol.ToolInfo{Name: "present", Type: "native", Source: "builtin", Schema: json.RawMessage(`{"title":"present","type":"object","properties":{}}`)}, false)
	m.stage([]protocol.ToolInfo{{Name: "srv__later", Type: "mcp", Source: "srv.json", Schema: json.RawMessage(`{"title":"srv__later","type":"object","properties":{}}`)}}, &protocol.Allowance{Patterns: []string{"srv__*"}, AutoAllowed: []string{"srv__later"}})
	if _, ok := m.registry.Get("srv__later"); ok {
		t.Fatal("staged proxy visible before commit")
	}
	if _, ok := m.registry.Get("present"); !ok {
		t.Fatal("allowance applied before commit")
	}
	m.registry.CommitPendingChanges()
	if _, ok := m.registry.Get("srv__later"); !ok {
		t.Fatal("staged proxy missing after commit")
	}
	if _, ok := m.registry.Get("present"); ok {
		t.Fatal("allowance did not bound the earlier tool")
	}
}

func TestSelectionBoundsTheMirrorAndMissingSourcesAreOmitted(t *testing.T) {
	root := t.TempDir()
	p := startInProcessHelper(t, helper.Options{}, 0)
	load := nativeSpecs("bash", "read_file", "list_dir")
	load.Tools = append(load.Tools, protocol.ToolSpec{Name: "missing__tool", Type: "mcp", Source: filepath.Join(t.TempDir(), "absent.json#srv")}, protocol.ToolSpec{Name: "script__go", Type: "shell", Source: filepath.Join(t.TempDir(), "absent.sh")})
	binding := openHelper(t, p, root, helloOptions{}, load, []string{"bash"})
	if got := servedNames(binding); !slices.Equal(got, []string{"bash", "view_image"}) {
		t.Fatalf("narrowed binding serves %v", got)
	}
	for _, name := range []string{"missing__tool", "script__go"} {
		if !slices.Contains(binding.Omitted, name) {
			t.Fatalf("omitted %v lacks %s", binding.Omitted, name)
		}
	}

	disabled := startInProcessHelper(t, helper.Options{}, 0)
	empty := openHelper(t, disabled, root, helloOptions{}, nativeSpecs("bash", "read_file"), []string{})
	if got := servedNames(empty); !slices.Equal(got, []string{"view_image"}) {
		t.Fatalf("empty selection serves %v", got)
	}
}

func TestReadOnlyScopeDeniesWritesAndKeepsScratch(t *testing.T) {
	root, scratch := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startInProcessHelper(t, helper.Options{}, 0)
	binding := openHelper(t, p, root, helloOptions{scratch: scratch, readOnly: true}, nativeSpecs("read_file", "write_file"), nil)
	if output, err := run(t, binding, "read_file", map[string]any{"path": "notes.txt"}); err != nil || !strings.Contains(output.Text, "notes") {
		t.Fatalf("read_file = %+v, %v", output, err)
	}
	if _, err := run(t, binding, "write_file", map[string]any{"path": "new.txt", "content": "x"}); err == nil {
		t.Fatal("read-only scope accepted a write into the root")
	}
	if _, err := run(t, binding, "write_file", map[string]any{"path": filepath.Join(scratch, "new.txt"), "content": "x"}); err != nil {
		t.Fatalf("scratch write refused: %v", err)
	}
}

func TestStreamLossFailsInFlightAndLaterCalls(t *testing.T) {
	root := t.TempDir()
	p := startInProcessHelper(t, helper.Options{}, 0)
	binding := openHelper(t, p, root, helloOptions{}, nativeSpecs("bash"), nil)
	tool, _ := binding.Registry.Get("bash")
	done := make(chan error, 1)
	go func() {
		_, err := binding.Registry.ExecuteTool(context.Background(), tool, map[string]any{"command": "sleep 20"}, 0)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	// The helper's side of the stream goes away under an in-flight call.
	p.helperWriter.Close()
	var toolErr *tools.ToolError
	select {
	case err := <-done:
		if !errors.As(err, &toolErr) || toolErr.Code != protocol.CodeHelperLost {
			t.Fatalf("in-flight call after stream loss = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call did not fail after stream loss")
	}
	if _, err := bash(t, binding, "echo again"); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("later call = %v, want a fast failure", err)
	}
}

func TestHeartbeatBreaksASilentSession(t *testing.T) {
	// The "helper" reads everything and answers nothing.
	hostReader, silentWriter := io.Pipe()
	defer silentWriter.Close()
	helperReader, hostWriter := io.Pipe()
	go io.Copy(io.Discard, helperReader)
	s := newSession(protocol.NewConn(hostReader, hostWriter), func() error { return hostWriter.Close() }, 40*time.Millisecond)
	defer s.Close()
	deadline := time.Now().Add(3 * time.Second)
	for s.failed() == nil {
		if time.Now().After(deadline) {
			t.Fatal("silent helper never broke the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.failed(); !strings.Contains(err.Error(), "heartbeat") {
		t.Fatalf("broken with %v", err)
	}
}
