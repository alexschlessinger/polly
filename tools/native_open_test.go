package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func nativeSource(t *testing.T, names ...string) *ToolRegistry {
	t.Helper()
	factory := mockSandboxFactory(&mockSandbox{})
	source := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(factory, sandbox.DefaultConfig()))
	t.Cleanup(func() { source.Close() })
	for _, name := range names {
		if _, err := source.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	return source
}

func TestNativeOpenToolsBindsRootAndGrant(t *testing.T) {
	root, scratch := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello from root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := nativeSource(t, "read_file", "write_file")
	open := NativeOpenTools(source)
	binding, err := open(context.Background(), ToolScope{Root: root, Grant: ExecutionGrant{ReadOnly: true, Scratch: scratch}})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	registry := binding.Registry
	if registry == source || registry.parent != nil {
		t.Fatal("the binding shares or derives the source registry")
	}
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	if registry.ExecutionRoot() != canonicalRoot {
		t.Fatalf("execution root = %q, want %q", registry.ExecutionRoot(), canonicalRoot)
	}
	if got := sortedToolNames(registry.All()); !slices.Equal(got, []string{"read_file", "view_image", "write_file"}) {
		t.Fatalf("bound tools = %v", got)
	}
	read, _ := registry.Get("read_file")
	if text, err := read.Execute(context.Background(), map[string]any{"path": "hello.txt"}); err != nil || !strings.Contains(text, "hello from root") {
		t.Fatalf("read_file through the binding = %q, %v", text, err)
	}
	write, _ := registry.Get("write_file")
	if _, err := write.Execute(context.Background(), map[string]any{"path": "new.txt", "content": "x"}); err == nil {
		t.Fatal("a read-only binding wrote into its root")
	}
	if _, err := write.Execute(context.Background(), map[string]any{"path": filepath.Join(scratch, "new.txt"), "content": "x"}); err != nil {
		t.Fatalf("a read-only binding could not write to its scratch: %v", err)
	}
	if binding.Instructions != "" || binding.ToolInstructions != "" {
		t.Fatalf("guidance without a loader or skills: %q %q", binding.Instructions, binding.ToolInstructions)
	}
}

func TestNativeOpenToolsRendersSkillsAndInstructions(t *testing.T) {
	skillRoot := t.TempDir()
	createSkillWithScript(t, skillRoot, "helper")
	catalog, err := skills.Discover([]string{skillRoot})
	if err != nil {
		t.Fatal(err)
	}
	source := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer source.Close()
	if _, err := NewSkillRuntime(catalog, source); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	open := NativeOpenTools(source, WithNativeInstructions(func(r *ToolRegistry) string {
		return "  REPOSITORY GUIDANCE for " + r.ExecutionRoot() + "\n"
	}))
	binding, err := open(context.Background(), ToolScope{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	canonicalRoot, _ := filepath.EvalSymlinks(root)
	if binding.Instructions != "REPOSITORY GUIDANCE for "+canonicalRoot {
		t.Fatalf("instructions = %q", binding.Instructions)
	}
	if !strings.Contains(binding.ToolInstructions, "Agent Skills") || !strings.Contains(binding.ToolInstructions, "helper") {
		t.Fatalf("tool instructions = %q", binding.ToolInstructions)
	}
	if _, ok := binding.Registry.Get("activate_skill"); !ok {
		t.Fatal("skill tools were not rebound")
	}
}

func TestNativeOpenToolsAllowedToolsAndOmitted(t *testing.T) {
	source := NewToolRegistry([]Tool{&Func{Name: "spawn_agent"}, &Func{Name: "workflow_run"}, &Func{Name: "send_message"}, &Func{Name: "custom"}}, WithNativeTools(), WithUnsafeNoSandbox())
	defer source.Close()
	for _, name := range []string{"read_file", "bash"} {
		if _, err := source.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	open := NativeOpenTools(source)
	root := t.TempDir()

	inherit, err := open(context.Background(), ToolScope{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer inherit.Close()
	if got := sortedToolNames(inherit.Registry.All()); !slices.Equal(got, []string{"bash", "read_file", "view_image"}) {
		t.Fatalf("inherited binding = %v", got)
	}
	for _, name := range []string{"spawn_agent", "workflow_run", "send_message", "custom"} {
		if !slices.Contains(inherit.Omitted, name) {
			t.Fatalf("omitted = %v, want %s", inherit.Omitted, name)
		}
	}

	disabled, err := open(context.Background(), ToolScope{Root: root, AllowedTools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	if got := sortedToolNames(disabled.Registry.All()); !slices.Equal(got, []string{"view_image"}) {
		t.Fatalf("empty selection serves %v, want the built-in alone", got)
	}

	narrowed, err := open(context.Background(), ToolScope{Root: root, AllowedTools: []string{"read_*", "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	defer narrowed.Close()
	if got := sortedToolNames(narrowed.Registry.All()); !slices.Equal(got, []string{"read_file", "view_image"}) {
		t.Fatalf("narrowed binding = %v", got)
	}
	// The binding does not validate the selection; the caller does once its
	// own tools are registered, and built-ins it names count.
	if err := narrowed.Registry.ValidateToolSelection([]string{"read_*", "nope"}, nil); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("validation = %v", err)
	}
	if err := narrowed.Registry.ValidateToolSelection([]string{"read_*", "read_transcript"}, []string{"read_transcript"}); err != nil {
		t.Fatalf("validation with a private built-in = %v", err)
	}
	if err := narrowed.Registry.ValidateToolSelection(nil, nil); err != nil {
		t.Fatalf("nil selection = %v", err)
	}
}

func TestNativeOpenToolsRefusesGenericSource(t *testing.T) {
	scope := ToolScope{Root: t.TempDir()}
	if _, err := NativeOpenTools(NewToolRegistry(nil))(context.Background(), scope); !errors.Is(err, ErrNativeToolsRequired) {
		t.Fatalf("generic source = %v", err)
	}
	if _, err := NativeOpenTools(nil)(context.Background(), scope); !errors.Is(err, ErrNativeToolsRequired) {
		t.Fatalf("nil source = %v", err)
	}
}

func TestNativeOpenToolsCloseIsIdempotentAndFailureReleases(t *testing.T) {
	source := nativeSource(t, "read_file")
	open := NativeOpenTools(source)
	root := t.TempDir()
	nested := filepath.Join(root, "scratch")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := open(context.Background(), ToolScope{Root: root, Grant: ExecutionGrant{Scratch: nested}}); err == nil {
		t.Fatal("a scratch inside the root was accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := open(canceled, ToolScope{Root: root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open = %v", err)
	}
	binding, err := open(context.Background(), ToolScope{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(); err != nil {
		t.Fatal(err)
	}
	if err := binding.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	if len(binding.Registry.All()) != 0 {
		t.Fatal("a closed binding still serves tools")
	}
}

func TestNativeOpenToolsGate(t *testing.T) {
	source := nativeSource(t, "read_file")
	gate := NewExecutionGate()
	binding, err := NativeOpenTools(source)(context.Background(), ToolScope{Root: t.TempDir(), Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	release, err := gate.Exclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := binding.Registry.Get("read_file")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := binding.Registry.ExecuteTool(ctx, tool, map[string]any{"path": "missing"}, time.Second)
	release()
	if result.Invoked || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the binding ignored the shared gate: %+v %v", result, err)
	}
}

func TestNativeOpenToolsRebasesMemberLayerEnvironment(t *testing.T) {
	sourceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := nativeSource(t)
	_, err = source.SetSandboxLayer("profile", &SandboxLayer{Members: sandbox.Config{
		Env: map[string]string{"BUILD_DIR": filepath.Join(sourceRoot, "build")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"scope", "grant"} {
		t.Run(field, func(t *testing.T) {
			scope := ToolScope{Root: root}
			if field == "scope" {
				scope.SourceRoot = sourceRoot
			} else {
				scope.Grant.SourceRoot = sourceRoot
			}
			binding, err := NativeOpenTools(source)(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			defer binding.Close()
			cfg, _, err := binding.Registry.SandboxReadPolicy()
			if err != nil {
				t.Fatal(err)
			}
			if got, want := cfg.Env["BUILD_DIR"], filepath.Join(root, "build"); got != want {
				t.Fatalf("member build path = %q, want %q", got, want)
			}
		})
	}
}
