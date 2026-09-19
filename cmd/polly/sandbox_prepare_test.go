package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func prepareRequest() map[string]any {
	return map[string]any{
		"allocations": []envstorage.Allocation{
			{Name: "build", Kind: "cache", Purpose: "compiled objects", Recipe: "test@1", Shared: true},
			{Name: "tool", Kind: "state", Purpose: "dependencies", Recipe: "test@1"},
			{Name: "tool", Kind: "config", Purpose: "settings", Recipe: "test@1"},
		},
		"env":   map[string]string{"BUILD_CACHE": "@cache/build", "TOOL_HOME": "@state/tool", "TOOL_CONFIG": "@config/tool"},
		"links": []envstorage.Link{{Path: "@state/tool/settings", Target: "@config/tool/settings"}},
	}
}

func preparedState(t *testing.T) *conversationState {
	t.Helper()
	_, state := sandboxTryState(t)
	t.Cleanup(func() { state.sandboxProfile.Close() })
	startSandboxInit(state)
	if out, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, prepareRequest()); code != "" {
		t.Fatalf("prepare: %s %s", code, out)
	}
	return state
}

func TestSandboxPreparePersistsAndReopens(t *testing.T) {
	state := preparedState(t)
	p := state.sandboxProfile
	before, _ := os.ReadFile(p.ws.profile)
	if out, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, prepareRequest()); code != "" {
		t.Fatalf("repeat: %s %s", code, out)
	}
	after, _ := os.ReadFile(p.ws.profile)
	if string(before) != string(after) {
		t.Fatal("repeated preparation changed saved declarations")
	}
	for _, registry := range []*tools.ToolRegistry{state.toolRegistry, state.toolRegistry.Derive().Derive()} {
		cfg, _, err := registry.SandboxReadPolicy()
		if err != nil || cfg.Env["TOOL_HOME"] != p.ws.storageRoots().Path(p.profile.Storage.Allocations[1]) {
			t.Fatalf("policy: %v %v", cfg.Env, err)
		}
	}
	reopened := openSandboxProfile(&Config{})
	defer reopened.Close()
	layer, ok := reopened.apply(sandbox.Config{}, func(sandbox.Config) (sandbox.Sandbox, error) { return passthroughSandbox{}, nil })
	if !ok || layer.Config.Env["TOOL_HOME"] == "" {
		t.Fatalf("reopen: %v", reopened.applyErr)
	}
	if err := p.lease.Exclusive(func() error { t.Error("cleaned while another session open"); return nil }); err == nil {
		t.Fatal("missing session lease")
	}
}

func TestSandboxPrepareExplicitAndSessionPrecedence(t *testing.T) {
	_, state := sandboxTryState(t)
	t.Cleanup(func() { state.sandboxProfile.Close() })
	manual := sandboxProfileItem{Kind: profileEnv, Name: "BUILD_CACHE", Value: "@workspace/manual"}
	if _, err := state.sandboxProfile.change(state.toolRegistry, func(items []sandboxProfileItem) []sandboxProfileItem { return append(items, manual) }, manual); err != nil {
		t.Fatal(err)
	}
	state.sandboxProfile.session = []sandboxProfileItem{{Kind: profileEnv, Name: "TOOL_HOME", Value: "@workspace/session"}}
	startSandboxInit(state)
	out, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, prepareRequest())
	if code != "" || !strings.Contains(out, "explicit setting") || !strings.Contains(out, "overridden for this session") {
		t.Fatalf("%s %s", code, out)
	}
	cfg, _, _ := state.toolRegistry.SandboxReadPolicy()
	if cfg.Env["BUILD_CACHE"] != filepath.Join(state.sandboxProfile.ws.dir, "manual") || cfg.Env["TOOL_HOME"] != filepath.Join(state.sandboxProfile.ws.dir, "session") {
		t.Fatalf("overrode explicit settings: %v", cfg.Env)
	}
}

func TestSandboxPrepareRejectsAuthorityChanges(t *testing.T) {
	_, state := sandboxTryState(t)
	t.Cleanup(func() { state.sandboxProfile.Close() })
	startSandboxInit(state)
	for _, name := range []string{"HOME", "PATH", "LD_PRELOAD", "BASH_ENV", "NODE_OPTIONS", "NPM_TOKEN", "POLLYTOOL_MODEL", "SSH_AUTH_SOCK", "DOCKER_HOST"} {
		request := prepareRequest()
		request["env"] = map[string]string{name: "@state/tool"}
		if _, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, request); code != sandboxInitBadItem {
			t.Fatalf("allowed %s: %s", name, code)
		}
	}
	for _, value := range []string{"/tmp/host", "@state/tool/../escape", "@workspace/.env", "@state/missing"} {
		request := prepareRequest()
		request["env"] = map[string]string{"TOOL_HOME": value}
		if _, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, request); code == "" {
			t.Fatalf("allowed %s", value)
		}
	}
	request := prepareRequest()
	request["read_paths"] = []string{"/host"}
	if _, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, request); code == "" {
		t.Fatal("accepted host grant")
	}
	if _, err := os.Stat(state.sandboxProfile.ws.profile); !os.IsNotExist(err) {
		t.Fatalf("invalid preparation persisted: %v", err)
	}
	state.sandboxInit.live = false
	if _, code := callSandboxTool(t, context.Background(), state, sandboxPrepareTool, prepareRequest()); code != sandboxInitInactive {
		t.Fatalf("inactive: %s", code)
	}
}

func TestSandboxProfileConcurrentUpdatesPreserveStorage(t *testing.T) {
	state := preparedState(t)
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			copy := &sandboxProfileState{ws: state.sandboxProfile.ws}
			defer copy.Close()
			if err := copy.updateProfile(nil, func(file *sandboxProfile) error {
				file.Items = append(file.Items, sandboxProfileItem{Kind: profileEnv, Name: string(rune('A'+i)) + "_CACHE", Value: "@workspace/cache"})
				return nil
			}, nil, true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	file, err := readSandboxProfile(state.sandboxProfile.ws.profile)
	if err != nil || len(file.Items) != n+3 || len(file.Storage.Allocations) != 3 {
		t.Fatalf("lost update: %+v %v", file, err)
	}
}

func TestSandboxProfileVersionOneUpgradeOnlyOnWrite(t *testing.T) {
	_, state := sandboxTryState(t)
	p := state.sandboxProfile
	data := `{"version":1,"workspace":"old","items":[{"kind":"env","name":"OLD_CACHE","value":"@cache/old"}]}`
	writeFile(t, p.ws.profile, data)
	file, err := readSandboxProfile(p.ws.profile)
	if err != nil || file.Version != 1 {
		t.Fatalf("read v1: %+v %v", file, err)
	}
	got, _ := os.ReadFile(p.ws.profile)
	if string(got) != data {
		t.Fatal("read rewrote v1")
	}
	if err := writeSandboxProfile(p.ws.profile, file); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(p.ws.profile)
	var upgraded sandboxProfile
	if err := json.Unmarshal(got, &upgraded); err != nil || upgraded.Version != 2 || upgraded.Items[0].Automatic {
		t.Fatalf("upgrade: %s %v", got, err)
	}
}
