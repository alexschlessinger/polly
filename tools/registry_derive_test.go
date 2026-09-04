package tools

import (
	"context"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func funcTool(name string) *Func {
	return &Func{Name: name, Desc: name, Run: func(context.Context, Args) (string, error) { return name, nil }}
}

func toolNames(tools []Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.GetName())
	}
	return names
}

func TestDerivedRegistrySeesParentToolsThroughItsAllowList(t *testing.T) {
	parent := NewToolRegistry([]Tool{funcTool("alpha"), funcTool("beta"), funcTool("gamma_one"), funcTool("gamma_two")})
	view := parent.Derive(AllowTools("alpha", "gamma_*"), DenyTools("gamma_two"))

	if got := toolNames(view.All()); !slices.Equal(got, []string{"alpha", "gamma_one"}) {
		t.Fatalf("view.All() = %v, want alpha and gamma_one", got)
	}
	if got := toolNames(parent.All()); len(got) != 4 {
		t.Fatalf("parent.All() = %v, want every tool", got)
	}
	if _, ok := view.Get("beta"); ok {
		t.Fatal("the allow-list let beta through")
	}
	if _, exists, allowed := view.GetIfAllowed("beta"); !exists || allowed {
		t.Fatalf("GetIfAllowed(beta) = exists %v allowed %v, want a hidden tool that exists", exists, allowed)
	}
	if _, exists, _ := view.GetIfAllowed("nope"); exists {
		t.Fatal("an unknown tool exists in the view")
	}
	viewTool, _ := view.Get("alpha")
	parentTool, _ := parent.Get("alpha")
	if viewTool != parentTool {
		t.Fatal("the view serves a copy rather than the parent's tool")
	}
	schemas := view.GetSchemas()
	if len(schemas) != 2 || schemas[0].Title() != "alpha" || schemas[1].Title() != "gamma_one" {
		t.Fatalf("view schemas = %d, want the two allowed tools", len(schemas))
	}
	if got := toolNames(parent.Derive().All()); len(got) != 4 {
		t.Fatalf("an unfiltered view sees %v, want every tool", got)
	}
}

func TestDerivedRegistryOwnToolsShadowAndStayPrivate(t *testing.T) {
	parent := NewToolRegistry([]Tool{funcTool("alpha"), funcTool("beta")})
	view := parent.Derive(AllowTools("alpha"))

	// The allow-list bounds the view's own tools too, until they are marked
	// always allowed, the way an agent marks its built-ins.
	own := funcTool("beta")
	view.Register(own)
	if _, ok := view.Get("beta"); ok {
		t.Fatal("a private tool outside the allow-list is visible")
	}
	view.MarkAlwaysAllowed("beta")
	if got, _ := view.Get("beta"); got != own {
		t.Fatal("the view's own beta does not shadow the parent's")
	}
	if got, _ := parent.Get("beta"); got == own {
		t.Fatal("the view's beta replaced the parent's")
	}
	view.Register(funcTool("notes"))
	view.MarkAlwaysAllowed("notes")
	if _, ok := view.Get("notes"); !ok {
		t.Fatal("the view's own notes tool is missing")
	}
	if _, exists, _ := parent.GetIfAllowed("notes"); exists {
		t.Fatal("a private tool leaked into the parent")
	}
	if got := toolNames(view.All()); !slices.Equal(got, []string{"alpha", "beta", "notes"}) {
		t.Fatalf("view.All() = %v", got)
	}

	var loaders []string
	for _, info := range view.GetActiveToolLoaders() {
		loaders = append(loaders, info.Name)
	}
	slices.Sort(loaders)
	if !slices.Equal(loaders, []string{"alpha", "beta", "notes"}) {
		t.Fatalf("view loaders = %v", loaders)
	}
	if got := parent.GetActiveToolLoaders(); len(got) != 2 {
		t.Fatalf("parent loaders = %v, want its own two", got)
	}
}

func TestDerivedRegistryPolicyIsItsOwnAndParentPolicyStillApplies(t *testing.T) {
	parent := NewToolRegistry([]Tool{funcTool("alpha"), funcTool("beta"), funcTool("gamma")})
	view := parent.Derive()

	view.stageSkillAllowance([]string{"alpha"}, nil)
	view.CommitPendingChanges()
	if got := toolNames(view.All()); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("view.All() after its skill policy = %v, want alpha", got)
	}
	if got := toolNames(parent.All()); len(got) != 3 {
		t.Fatalf("the view's skill policy reached the parent: %v", got)
	}

	parent.stageSkillAllowance([]string{"beta"}, nil)
	parent.CommitPendingChanges()
	if got := toolNames(parent.All()); !slices.Equal(got, []string{"beta"}) {
		t.Fatalf("parent.All() = %v, want beta", got)
	}
	if got := toolNames(view.All()); len(got) != 0 {
		t.Fatalf("view.All() = %v, want nothing: alpha is barred by the parent, beta by the view", got)
	}
	if got := toolNames(parent.Derive().All()); !slices.Equal(got, []string{"beta"}) {
		t.Fatalf("a fresh view sees %v, want the parent's beta", got)
	}
}

func TestDerivedRegistryCloseReleasesOnlyItsOwn(t *testing.T) {
	parent := NewToolRegistry([]Tool{funcTool("alpha")})
	parent.toolClients["alpha"] = &MCPClient{}
	view := parent.Derive()
	view.Register(funcTool("notes"))

	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if got := view.All(); len(got) != 0 {
		t.Fatalf("a closed view still serves %v", toolNames(got))
	}
	if _, ok := parent.Get("alpha"); !ok {
		t.Fatal("closing the view took the parent's tool")
	}
	if parent.toolClients["alpha"] == nil {
		t.Fatal("closing the view dropped the parent's MCP client")
	}

	survivor := parent.Derive()
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if got := survivor.All(); len(got) != 0 {
		t.Fatalf("a view outlived its closed parent with %v", toolNames(got))
	}
}

func TestDerivedRegistryLoadsItsOwnNativeTools(t *testing.T) {
	parent := NewToolRegistry(nil, WithUnsafeNoSandbox())
	view := parent.Derive()
	if _, err := view.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	if _, ok := view.Get("read_file"); !ok {
		t.Fatal("the view did not load its own read_file")
	}
	if _, exists, _ := parent.GetIfAllowed("read_file"); exists {
		t.Fatal("a tool loaded on the view landed in the parent")
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if _, exists, _ := view.GetIfAllowed("read_file"); exists {
		t.Fatal("the view kept its tool past Close")
	}
}

func TestDerivedRegistrySkillRuntimeUsesTheParentBash(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	parent := NewToolRegistry(nil, WithUnsafeNoSandbox())
	if _, err := parent.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	view := parent.Derive(AllowTools("read_file"))

	runtime, err := NewSkillRuntime(catalog, view)
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.Enabled() {
		t.Fatal("runtime should be enabled")
	}
	if _, ok := view.Get("activate_skill"); !ok {
		t.Fatal("activate_skill missing from the view")
	}
	if _, exists, _ := parent.GetIfAllowed("activate_skill"); exists {
		t.Fatal("the view's skill tools reached the parent")
	}
	if _, private := view.tools["bash"]; private {
		t.Fatal("the view built its own bash although the parent has one")
	}
	if _, ok := view.Get("bash"); ok {
		t.Fatal("the allow-list let the parent's bash through")
	}
}

func TestDerivedRegistryInheritsSandboxPolicy(t *testing.T) {
	skipIfWindows(t)
	base := sandbox.Config{DenyPaths: []string{"/private/project-secret"}, DenyWrite: true}
	var received sandbox.Config
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		received = cfg
		return &mockSandbox{}, nil
	}
	parent := NewToolRegistry(nil, WithSandboxFactory(factory, base))
	view := parent.Derive()

	if !view.HasSandbox() {
		t.Fatal("the view lost the parent's sandbox")
	}
	want, _, err := parent.SandboxReadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	got, active, err := view.SandboxReadPolicy()
	if err != nil || !active {
		t.Fatalf("view read policy active %v err %v", active, err)
	}
	if !got.DenyWrite || !slices.Equal(got.DenyPaths, want.DenyPaths) {
		t.Fatalf("view policy = %+v, want the parent's %+v", got, want)
	}
	if _, err := view.NewSandbox(nil); err != nil {
		t.Fatal(err)
	}
	if !received.DenyWrite || len(received.DenyPaths) != 1 {
		t.Fatalf("view sandbox built with %+v, want the parent's denies", received)
	}
}
