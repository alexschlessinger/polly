package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestSessionStoragePolicyPrecedesToolLoading(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	t.Chdir(root)
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: filepath.Join(root, "private.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	paths, err := sessionPrivatePaths(store)
	if err != nil {
		t.Fatal(err)
	}
	var configs []sandbox.Config
	original := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		configs = append(configs, cfg)
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = original })
	opts, probe, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base"}, nil, paths...)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, opts...)
	defer registry.Close()
	read := tools.NewReadFileTool(registry)
	for _, path := range paths {
		if _, err := read.Execute(context.Background(), map[string]any{"path": path}); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Fatalf("private read not denied: %s: %v", path, err)
		}
		if len(configs) == 0 || !slices.Contains(configs[0].DenyPaths, path) {
			t.Fatal("startup factory missed database policy")
		}
	}
	// Host storage remains usable independently of model-tool access.
	session, err := store.Acquire(context.Background(), "host", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.GetHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateStorageDeniedToShellAndMCP(t *testing.T) {
	skipIfWindows(t)
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("requires host sandbox execution")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("MCP fixture requires python3")
	}
	root := t.TempDir()
	t.Chdir(root)
	var paths []string
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := filepath.Join(root, "private.db") + suffix
		if err := os.WriteFile(path, []byte("PRIVATE_STORAGE_FIXTURE"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	control := filepath.Join(root, "public.txt")
	if err := os.WriteFile(control, []byte("PUBLIC_STORAGE_FIXTURE"), 0600); err != nil {
		t.Fatal(err)
	}
	// Expose the fixture workspace through Linux's private /tmp while keeping
	// the database and sidecars explicitly denied inside that workspace.
	opts, probe, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "workspace"}, nil, paths...)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, opts...)
	defer registry.Close()
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	bash, _, _ := registry.GetIfAllowed("bash")
	if out, err := bash.Execute(context.Background(), map[string]any{"command": "cat '" + control + "'"}); err != nil || !strings.Contains(out, "PUBLIC_STORAGE_FIXTURE") {
		t.Fatalf("shell cannot read public fixture: %s %v", out, err)
	}
	for _, path := range paths {
		out, _ := bash.Execute(context.Background(), map[string]any{"command": "cat '" + path + "'"})
		if strings.Contains(out, "PRIVATE_STORAGE_FIXTURE") {
			t.Fatal("shell read private storage")
		}
	}
	script := filepath.Join(root, "mcp_probe.py")
	code := `import json, sys
with open(sys.argv[1]) as source:
    if source.read() != "PUBLIC_STORAGE_FIXTURE":
        raise RuntimeError("MCP cannot read public fixture")
leaked = False
for path in sys.argv[2:]:
    try:
        with open(path) as source:
            leaked |= "PRIVATE_STORAGE_FIXTURE" in source.read()
    except OSError:
        pass
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    method = request.get("method")
    if method == "initialize":
        result = {"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"storage-probe","version":"1"}}
    elif method == "tools/list":
        result = {"tools":[{"name":"storage_probe","description":"fixture","inputSchema":{"type":"object","properties":{}}}]}
    else:
        result = {"content":[{"type":"text","text":"leaked" if leaked else "blocked"}]}
    print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":result}), flush=True)
`
	if err := os.WriteFile(script, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "mcp.json")
	data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"probe": map[string]any{"command": python, "args": append([]string{script, control}, paths...)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := registry.LoadMCPServer(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Servers) != 1 || len(loaded.Servers[0].ToolNames) != 1 {
		t.Fatalf("MCP fixture tools: %+v", loaded)
	}
	tool, _, _ := registry.GetIfAllowed(loaded.Servers[0].ToolNames[0])
	if tool == nil {
		t.Fatal("MCP probe not registered")
	}
	out, err := tool.Execute(context.Background(), nil)
	if err != nil || !strings.Contains(out, "blocked") || strings.Contains(out, "leaked") {
		t.Fatalf("MCP storage policy: %s %v", out, err)
	}
}

func TestStoragePolicyIncludesCanonicalPromotionAndSidecars(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := filepath.Join(alias, "future", "database.db") + suffix
		got, err := canonicalStoragePath(path)
		if err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(want, "future", "database.db")+suffix {
			t.Fatalf("canonical sidecar: %s", got)
		}
	}
	paths, err := sessionPrivatePaths(nil)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if !slices.Contains(paths, filepath.Join(home, ".pollytool", "polly.db")+suffix) {
			t.Fatal("missing promotion exclusion")
		}
	}
	opts, probe, err := sandboxRegistryOptionsWithWarnings(&Config{NoSandbox: true}, nil, paths...)
	if err != nil || len(opts) != 1 || probe != nil {
		t.Fatal("explicit unsafe semantics changed")
	}
}
