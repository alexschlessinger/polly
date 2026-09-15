package tools

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func toolNamesOf(ts []Tool) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.GetName())
	}
	slices.Sort(names)
	return names
}

func TestGenericRegistryHasNoNativeSetup(t *testing.T) {
	r := NewToolRegistry(nil)
	defer r.Close()
	for _, name := range []string{"bash", "read_file", "list_dir", "write_file", "edit_file", "view_image"} {
		if r.HasNativeTool(name) {
			t.Fatalf("generic registry offers native %s", name)
		}
	}
	if _, err := r.LoadToolAuto("read_file"); err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("LoadToolAuto(read_file) on a generic registry = %v, want a missing-file error", err)
	}
	if _, ok := r.Get("view_image"); ok {
		t.Fatal("generic registry registered view_image")
	}
	if len(r.All()) != 0 {
		t.Fatalf("generic registry serves %v", toolNamesOf(r.All()))
	}
	if _, _, err := r.BindExecutionContext(ExecutionContext{Root: t.TempDir()}, nil); !errors.Is(err, ErrNativeToolsRequired) {
		t.Fatalf("BindExecutionContext on a generic registry = %v, want ErrNativeToolsRequired", err)
	}
	if r.Derive().HasNativeTool("bash") {
		t.Fatal("derivation installed native tools")
	}
}

func TestWithNativeToolsInstallsFactoriesAndViewImage(t *testing.T) {
	r := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer r.Close()
	for _, name := range []string{"bash", "read_file", "list_dir", "write_file", "edit_file", "view_image"} {
		if !r.HasNativeTool(name) {
			t.Fatalf("native registry lacks %s", name)
		}
	}
	if _, ok := r.Get("view_image"); !ok || !r.isBuiltin("view_image") {
		t.Fatal("native setup did not register view_image as a built-in")
	}
	if got := toolNamesOf(r.All()); !slices.Equal(got, []string{"view_image"}) {
		t.Fatalf("native registry serves %v before any load", got)
	}
	for _, info := range r.GetActiveToolLoaders() {
		if info.Name == "view_image" {
			t.Fatal("a built-in was reported as a loaded tool")
		}
	}
	if _, err := r.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	if got := toolNamesOf(r.All()); !slices.Equal(got, []string{"read_file", "view_image"}) {
		t.Fatalf("native registry serves %v", got)
	}
	if loaders := r.GetActiveToolLoaders(); len(loaders) != 1 || loaders[0].Name != "read_file" {
		t.Fatalf("loaders = %+v, want read_file alone", loaders)
	}
}

func TestDerivedRegistryFindsNativeFactoriesThroughParent(t *testing.T) {
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	view := parent.Derive().Derive()
	if !view.HasNativeTool("read_file") {
		t.Fatal("a twice-derived view lost the native constructors")
	}
	if _, err := view.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	tool, ok := view.tools["read_file"]
	if !ok {
		t.Fatal("the view did not load its own read_file")
	}
	if tool.(*readFileTool).registry != view {
		t.Fatal("the native constructor bound the tool to another registry")
	}
	if _, exists, _ := parent.GetIfAllowed("read_file"); exists {
		t.Fatal("a tool loaded on the view landed in the parent")
	}
	if len(view.nativeTools) != 0 {
		t.Fatal("the view carries a native constructor table of its own")
	}
}

func TestDeriveSharesSandboxStateWithoutPreparing(t *testing.T) {
	plain := NewToolRegistry(nil)
	if plain.baseSandboxPrepared {
		t.Fatal("a registry without sandbox options prepared a base config")
	}
	if view := plain.Derive(); view.baseSandboxPrepared || view.HasSandbox() {
		t.Fatal("derivation prepared or invented a sandbox")
	}
	factory := func(sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }
	withSandbox := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.DefaultConfig()))
	view := withSandbox.Derive()
	if !view.HasSandbox() || view.sandboxParent != withSandbox {
		t.Fatal("derivation dropped the live sandbox parent")
	}
}

func TestDeriveKeepsBuiltinsThroughViewsAndPolicies(t *testing.T) {
	parent := NewToolRegistry([]Tool{&Func{Name: "other"}, &Func{Name: "swarm_status"}}, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	parent.MarkAlwaysAllowed("swarm_status")

	view := parent.Derive(AllowTools("other"))
	if _, ok := view.Get("view_image"); !ok {
		t.Fatal("an allow-list hid the built-in view_image")
	}
	view.stageSkillAllowance([]string{"other"}, nil)
	view.CommitPendingChanges()
	if _, ok := view.Get("view_image"); !ok {
		t.Fatal("a skill policy hid the built-in view_image")
	}
	if _, ok := view.Get("other"); !ok {
		t.Fatal("the selected tool is missing")
	}

	denied := parent.Derive(DenyTools("swarm_*"))
	if _, ok := denied.Get("swarm_status"); ok {
		t.Fatal("a deny pattern did not hide the parent's always-allowed tool")
	}
	if _, ok := denied.Get("view_image"); !ok {
		t.Fatal("a deny pattern hid the built-in")
	}
}
