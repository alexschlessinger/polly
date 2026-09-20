package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestManagedEnvironmentRebindsContexts(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := envstorage.Spec{Allocations: []envstorage.Allocation{{Name: "build", Kind: "cache", Purpose: "objects", Shared: true}, {Name: "tool", Kind: "state", Purpose: "dependencies"}, {Name: "tool", Kind: "config", Purpose: "configuration"}}}
	roots := envstorage.Roots{Cache: filepath.Join(base, "cache"), SharedCache: filepath.Join(base, "shared"), State: filepath.Join(base, "state"), Config: filepath.Join(base, "config"), Control: filepath.Join(base, "protected")}
	if err := roots.Ensure(spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roots.Path(spec.Allocations[2]), "settings"), []byte("setting"), 0600); err != nil {
		t.Fatal(err)
	}
	env := &SandboxEnvironment{Storage: spec, Roots: roots, CheckoutCacheRoot: filepath.Join(base, "checkouts-cache"), CheckoutDataRoot: filepath.Join(base, "checkouts-data"), Env: map[string]string{"BUILD_CACHE": "@cache/build", "TOOL_HOME": "@state/tool", "TOOL_CONFIG": "@config/tool"}}
	r := NewToolRegistry(nil, WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) { return nil, nil }, sandbox.Config{}), WithSandboxLayer("profile", SandboxLayer{Environment: env}))
	defer r.Close()
	checkout, scratch := filepath.Join(base, "checkout"), filepath.Join(base, "scratch")
	for _, path := range []string{checkout, scratch} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writable, err := r.ExecutionPolicy(checkout, ExecutionGrant{Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if writable.Sandbox.Env["BUILD_CACHE"] != roots.Path(spec.Allocations[0]) || writable.Sandbox.Env["TOOL_HOME"] == roots.Path(spec.Allocations[1]) {
		t.Fatalf("binding: %v", writable.Sandbox.Env)
	}
	if b, err := os.ReadFile(filepath.Join(writable.Sandbox.Env["TOOL_CONFIG"], "settings")); err != nil || string(b) != "setting" {
		t.Fatalf("config copy: %q %v", b, err)
	}
	for _, path := range []string{filepath.Join(checkout, ".git"), filepath.Join(checkout, "subdir")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	subdir, err := r.ExecutionPolicy(filepath.Join(checkout, "subdir"), ExecutionGrant{Scratch: scratch})
	if err != nil || subdir.Sandbox.Env["TOOL_HOME"] != writable.Sandbox.Env["TOOL_HOME"] {
		t.Fatalf("subdirectory did not reuse checkout state: %v %v", subdir.Sandbox.Env, err)
	}
	ro, err := r.ExecutionPolicy(checkout, ExecutionGrant{ReadOnly: true, Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range ro.Sandbox.Env {
		if !sandbox.PathWithin(value, scratch) {
			t.Fatalf("read-only env outside scratch: %s", value)
		}
	}
	if len(ro.Sandbox.WritablePaths) != 1 || ro.Sandbox.WritablePaths[0] != scratch {
		t.Fatalf("read-only authority: %v", ro.Sandbox.WritablePaths)
	}
	noScratch, err := r.ExecutionPolicy(checkout, ExecutionGrant{ReadOnly: true})
	if err != nil || !noScratch.Sandbox.DenyWrite || noScratch.Sandbox.Env["TOOL_HOME"] != "" {
		t.Fatalf("no scratch: %+v %v", noScratch, err)
	}
	if _, err := r.ExecutionPolicy(checkout, ExecutionGrant{Scratch: scratch, DeniedWrites: []string{env.CheckoutDataRoot}}); err == nil {
		t.Fatal("ignored member deny")
	}
}

func TestEnvironmentMaintenanceGatesDerivedAndBoundTools(t *testing.T) {
	r := NewToolRegistry(nil)
	defer r.Close()
	child := r.Derive().Derive()
	release, err := child.GuardExecution(context.Background(), &Func{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if done, err := r.BeginEnvironmentMaintenance(); err == nil {
		done()
		t.Fatal("cleanup during execution")
	}
	release()
	done, err := r.BeginEnvironmentMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := child.GuardExecution(ctx, &Func{Name: "test"}); err == nil {
		t.Fatal("execution during cleanup")
	}
	done()
	release, err = child.GuardExecution(context.Background(), &Func{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	release()
}
