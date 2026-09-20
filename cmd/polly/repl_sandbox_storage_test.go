package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestSandboxStorageCommandsPreserveConfigurationAndProject(t *testing.T) {
	state := preparedState(t)
	p := state.sandboxProfile
	config, _ := p.ws.storageRoots().Resolve(p.profile.Storage, "@config/tool/settings")
	dep, _ := p.ws.storageRoots().Resolve(p.profile.Storage, "@state/tool/dep")
	project := filepath.Join(p.ws.dir, "node_modules", "dependency")
	for _, path := range []string{config, dep, project} {
		writeFile(t, path, "keep")
	}
	var lines []string
	c := &replCommandContext{state: state, reply: func(s string) error { lines = append(lines, s); return nil }}
	if err := sandboxStorageCommand(c, []string{"storage"}); err != nil {
		t.Fatal(err)
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "shared across writable worktrees") || !strings.Contains(out, "test@1") {
		t.Fatal(out)
	}
	if err := sandboxStorageCommand(c, []string{"reset", "environment"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{config, project} {
		if b, err := os.ReadFile(path); err != nil || string(b) != "keep" {
			t.Fatalf("preserved file: %q %v", b, err)
		}
	}
	if _, err := os.Stat(dep); !os.IsNotExist(err) {
		t.Fatalf("state survived reset: %v", err)
	}
	if out := sandboxProfileForget(c, []string{"all"}); !strings.Contains(out, "forgot all") {
		t.Fatal(out)
	}
	cfg, _, err := state.toolRegistry.SandboxReadPolicy()
	if err != nil || cfg.Env["TOOL_HOME"] != "" {
		t.Fatalf("settings survived forget: %v %v", cfg.Env, err)
	}
	for _, root := range cfg.WritablePaths {
		if root == filepath.Dir(config) {
			t.Fatal("automatic write authority survived forget")
		}
	}
	file, err := readSandboxProfile(p.ws.profile)
	if err != nil || len(file.Storage.Allocations) != 3 || !file.Storage.Allocations[0].Disabled {
		t.Fatalf("lost cleanup tracking: %+v %v", file, err)
	}
}

func TestSandboxStorageBackgroundGate(t *testing.T) {
	state := preparedState(t)
	var work func(context.Context) ([]string, error)
	c := &replCommandContext{state: state, storageWork: func(_ string, run func(context.Context) ([]string, error)) error { work = run; return nil }}
	if err := sandboxStorageCommand(c, []string{"clean", "caches"}); err != nil {
		t.Fatal(err)
	}
	if release, err := state.toolRegistry.TryEnvironmentUse(); err == nil {
		release()
		t.Fatal("new execution could start before queued cleanup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := work(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if release, err := state.toolRegistry.TryEnvironmentUse(); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

func TestSandboxPrepareRollsBackFactoryAndPersistenceFailures(t *testing.T) {
	state := preparedState(t)
	p := state.sandboxProfile
	before, _ := os.ReadFile(p.ws.profile)
	base, _, _ := state.toolRegistry.BaseSandboxPolicy()
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if cfg.Env["NEW_CACHE"] != "" {
			return nil, errors.New("factory refused changed policy")
		}
		return passthroughSandbox{}, nil
	}, base), tools.WithSandboxLayer(sandboxProfileLayer, *p.applied))
	defer registry.Close()
	state.toolRegistry = registry
	state.sandboxInit.register(registry)
	req := prepareRequest()
	req["env"] = map[string]string{"NEW_CACHE": "@cache/build"}
	if _, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, req); code == "" {
		t.Fatal("factory failure succeeded")
	}
	if data, _ := os.ReadFile(p.ws.profile); string(data) != string(before) {
		t.Fatal("factory failure changed saved profile")
	}
	req["env"] = map[string]string{"OTHER_CACHE": "@cache/build"}
	dir := filepath.Dir(p.ws.profile)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	if _, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, req); code == "" {
		t.Fatal("persistence failure succeeded")
	}
	cfg, _, err := registry.SandboxReadPolicy()
	if err != nil || cfg.Env["OTHER_CACHE"] != "" || cfg.Env["TOOL_HOME"] == "" {
		t.Fatalf("rollback: %v %v", cfg.Env, err)
	}
}

func TestSandboxExplicitManagedCachePreservesLegacyMeaning(t *testing.T) {
	state := preparedState(t)
	ws := state.sandboxProfile.ws
	manual, err := parseSandboxProfileItem(ws, []string{"env", "BUILD_CACHE=@cache/build"})
	if err != nil || !manual.Managed || manual.Automatic {
		t.Fatalf("manual: %+v %v", manual, err)
	}
	judge := newProfileJudge(ws)
	managed, _ := judge.itemEnvPath(manual)
	legacy, _ := judge.itemEnvPath(sandboxProfileItem{Kind: profileEnv, Name: "OLD_CACHE", Value: "@cache/build"})
	if managed == legacy || legacy != filepath.Join(ws.cache, "build") {
		t.Fatalf("legacy redirected: %s %s", managed, legacy)
	}
}
