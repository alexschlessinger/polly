package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func agentToolPolicyFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgentToolExecutableCeiling(t *testing.T) {
	skipIfWindows(t)
	home, root := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "hidden")
	script := agentToolPolicyFixture(t, dir, "probe.sh", "#!/bin/sh\nexit 0\n")
	secret := agentToolPolicyFixture(t, dir, "secret", "fixture-secret")
	alias := filepath.Join(root, "alias.sh")
	if err := os.Symlink(script, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                  string
		read                  []string
		deny                  []string
		command               string
		legacy, fail, sibling bool
	}{
		{name: "hidden executable", command: script, fail: true},
		{name: "hidden symlink target", command: alias, fail: true},
		{name: "file only", command: script, read: []string{script}},
		{name: "file only via alias", command: alias, read: []string{script}},
		{name: "directory authorized", command: script, read: []string{dir}, sibling: true},
		{name: "deeper mask retained", command: script, read: []string{dir}, deny: []string{secret}},
		{name: "executable masked", command: script, deny: []string{script}, fail: true},
		{name: "legacy exposure", command: script, legacy: true, sibling: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sandbox.Config{PrivateHome: true, DenyHostTemp: true, WritablePaths: []string{root}, ReadPaths: tc.read, DenyPaths: tc.deny}
			var captured sandbox.Config
			r := NewToolRegistry(nil, WithSandboxFactory(func(cfg sandbox.Config) (sandbox.Sandbox, error) {
				captured = cfg
				return &mockSandbox{}, nil
			}, cfg))
			defer r.Close()
			if !tc.legacy {
				r.executionSandboxPresets = []string{"workspace+private-home"}
			}
			for _, derived := range []bool{false, true} {
				registry := r
				if derived {
					registry = r.Derive()
					defer registry.Close()
				}
				check := func(err error) {
					t.Helper()
					if (err != nil) != tc.fail {
						t.Fatalf("derived=%v: error %v, want failure %v", derived, err, tc.fail)
					}
					if err == nil {
						if err := sandbox.ReadAllowed(captured, script); err != nil {
							t.Fatalf("authorized executable hidden: %v", err)
						}
						if got := sandbox.ReadAllowed(captured, secret) == nil; got != tc.sibling {
							t.Fatalf("sibling read = %v, want %v", got, tc.sibling)
						}
					}
				}
				_, _, err := registry.newSandboxFor("shell", nil, tc.command)
				check(err)
				_, _, err = registry.newServerSandbox("mcp", nil, tc.command)
				check(err)
				_, err = registry.newSchemaSandbox(tc.command)
				check(err)
				tool := &ShellTool{Command: tc.command}
				_, err = tool.BindExecutionContext(registry, ExecutionContext{Root: root, Sandbox: cfg})
				check(err)
			}
		})
	}
}

func TestAgentToolBindingOmitsHiddenExecutable(t *testing.T) {
	skipIfWindows(t)
	home, root := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	script := agentToolPolicyFixture(t, filepath.Join(home, "hidden"), "probe.sh", "#!/bin/sh\nexit 0\n")
	tool := &ShellTool{Command: script, schema: schema.ToolSchemaFromString(`{"title":"probe","type":"object"}`)}
	parent := NewToolRegistry([]Tool{tool}, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{WritablePaths: []string{root}}))
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, SandboxPresets: []string{"workspace+private-home"}})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if !slices.Contains(omitted, "probe") {
		t.Fatalf("hidden shell not omitted: %v", omitted)
	}
	if err := bound.ValidateToolSelection([]string{"probe"}, nil); err == nil {
		t.Fatal("required hidden shell accepted")
	}
	for _, registry := range []*ToolRegistry{bound, bound.Derive()} {
		defer registry.Close()
		if _, err := registry.LoadShellTool(script); err == nil || !strings.Contains(err.Error(), "agent sandbox policy") {
			t.Fatalf("late discovery ran hidden executable: %v", err)
		}
		if _, _, err := registry.prepareMCPServerTools(context.Background(), &MCPConfig{Command: script}, "probe", "probe", "", nil); err == nil || !strings.Contains(err.Error(), "agent sandbox policy") {
			t.Fatalf("late MCP ran hidden executable: %v", err)
		}
	}
}

func TestAgentToolEnvironmentCeiling(t *testing.T) {
	root := realTempDir(t)
	script := agentToolPolicyFixture(t, root, "probe", "fixture")
	for _, tc := range []struct {
		name              string
		base, local, want []string
		fail              bool
	}{
		{name: "local restriction", local: []string{"PATH"}, want: []string{"PATH"}},
		{name: "inherited restriction", base: []string{"PATH"}, want: []string{"PATH"}},
		{name: "intersection", base: []string{"PATH"}, local: []string{"PATH", "REVIEW_INTERNAL_CONTEXT", "REVIEW_API_TOKEN"}, want: []string{"PATH"}},
		{name: "default sensitivity", local: []string{"PATH", "Review_Api_Token"}, want: []string{"PATH"}},
		{name: "disjoint", base: []string{"PATH"}, local: []string{"REVIEW_INTERNAL_CONTEXT"}, fail: true},
		{name: "sensitive only", local: []string{"Review_Api_Token"}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := sandbox.Config{WritablePaths: []string{root}, AllowEnv: tc.base, Env: map[string]string{"POLICY_ONLY": "configured"}}
			local := sandbox.Config{AllowEnv: tc.local, PassEnv: []string{"REVIEW_API_TOKEN"}, Env: map[string]string{"TOOL_ADDED": "bad"}, AllowNetwork: true}
			parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), base))
			defer parent.Close()
			ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, SandboxPresets: []string{"workspace"}})
			if err != nil {
				t.Fatal(err)
			}
			bound, _, err := parent.BindExecutionContext(ec, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bound.Close()
			check := func(cfg sandbox.Config, err error) {
				t.Helper()
				if (err != nil) != tc.fail {
					t.Fatalf("config %+v, error %v; want failure %v", cfg, err, tc.fail)
				}
				if err == nil && (!slices.Equal(cfg.AllowEnv, tc.want) || cfg.Env["POLICY_ONLY"] != "configured" || cfg.Env["TOOL_ADDED"] != "" || len(cfg.PassEnv) != 0 || cfg.AllowNetwork) {
					t.Fatalf("wrong effective policy: %+v", cfg)
				}
			}
			for _, r := range []*ToolRegistry{bound, bound.Derive()} {
				defer r.Close()
				_, cfg, err := r.newSandboxFor("shell", &local, script)
				check(cfg, err)
				shell := &ShellTool{Command: script, sandboxCfg: &local}
				tool, err := shell.BindExecutionContext(r, ec)
				if err != nil {
					check(sandbox.Config{}, err)
				} else {
					check(*SandboxDetails(tool).Config, nil)
				}
				config, err := r.contextMCPConfig("probe", &MCPConfig{Command: script, Sandbox: sandbox.DeclareConfig(local)})
				if err != nil {
					t.Fatal(err)
				}
				overlay, err := config.SandboxConfig()
				if err != nil {
					t.Fatal(err)
				}
				_, cfg, err = r.newServerSandbox("probe", overlay, script)
				check(cfg, err)
			}
		})
	}
}

func TestAgentToolLegacyEnvironmentUnchanged(t *testing.T) {
	root := realTempDir(t)
	script := agentToolPolicyFixture(t, root, "probe", "fixture")
	base := sandbox.Config{WritablePaths: []string{root}, AllowEnv: []string{"PATH"}}
	local := sandbox.Config{AllowEnv: []string{"OTHER"}}
	r := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), base))
	defer r.Close()
	_, cfg, err := r.newSandboxFor("shell", &local, script)
	if err != nil || !slices.Equal(cfg.AllowEnv, []string{"PATH", "OTHER"}) {
		t.Fatalf("legacy additive policy changed: %+v, %v", cfg, err)
	}
	tool, err := (&ShellTool{Command: script, sandboxCfg: &local}).BindExecutionContext(r, ExecutionContext{Root: root, Sandbox: base})
	if err != nil {
		t.Fatal(err)
	}
	if got := SandboxDetails(tool).Config.AllowEnv; !slices.Equal(got, []string{"PATH"}) {
		t.Fatalf("legacy rebound policy changed: %v", got)
	}
}

func TestAgentToolSchemaEnvironmentCeiling(t *testing.T) {
	root := realTempDir(t)
	script := agentToolPolicyFixture(t, root, "probe", "fixture")
	var captured sandbox.Config
	r := NewToolRegistry(nil, WithSandboxFactory(func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return &mockSandbox{}, nil
	}, sandbox.Config{WritablePaths: []string{root}, AllowEnv: []string{"PATH", "REVIEW_API_TOKEN"}, Env: map[string]string{"EXPLICIT": "not for discovery"}}))
	defer r.Close()
	r.executionSandboxPresets = []string{"workspace"}
	if _, err := r.newSchemaSandbox(script); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(captured.AllowEnv, []string{"PATH"}) || len(captured.Env) != 0 {
		t.Fatalf("discovery lost restrictions or gained explicit configuration: %+v", captured)
	}
}
