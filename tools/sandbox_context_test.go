package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestSandboxContextUsesLiveLayersWithoutEnvironmentValues(t *testing.T) {
	root, extra := realTempDir(t), realTempDir(t)
	registry := stubSandboxRegistry(t, sandbox.Config{WritablePaths: []string{root}, AllowNetwork: true})
	defer registry.Close()
	derived := registry.Derive()
	defer derived.Close()
	before, err := derived.SandboxContext()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SetSandboxLayer("profile", &SandboxLayer{Config: sandbox.Config{
		PrivateHome: true, DenyDNS: true, ReadPaths: []string{extra}, WritablePaths: []string{extra},
		PassEnv: []string{"NPM_TOKEN"}, Env: map[string]string{"GOCACHE": "DO_NOT_EXPOSE_CONFIG_VALUE"},
	}}); err != nil {
		t.Fatal(err)
	}
	after, err := derived.SandboxContext()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{root, extra, "Home: private", "Process network: allowed; DNS blocked", "GOCACHE", "NPM_TOKEN", "MCP servers and other tools may have different permissions"} {
		if !strings.Contains(after, want) {
			t.Fatalf("missing %q in context:\n%s", want, after)
		}
	}
	if strings.Contains(after, "DO_NOT_EXPOSE_CONFIG_VALUE") {
		t.Fatal("environment value leaked into the prompt")
	}
	if _, err := registry.SetSandboxLayer("profile", nil); err != nil {
		t.Fatal(err)
	}
	restored, err := derived.SandboxContext()
	if err != nil || restored != before {
		t.Fatalf("removing the layer did not restore the context: %v\n%s", err, restored)
	}
}

func TestSandboxContextModes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		registry     *ToolRegistry
		want, absent string
	}{
		{name: "nil"},
		{name: "unconfigured", registry: NewToolRegistry(nil)},
		{name: "unsafe", registry: NewToolRegistry(nil, WithUnsafeNoSandbox()), want: "Process sandbox: disabled", absent: "Home:"},
		{name: "readonly", registry: stubSandboxRegistry(t, sandbox.Config{DenyWrite: true}), want: "Writes: all denied, including temporary files.", absent: "temp write grants"},
		{name: "no host temp", registry: stubSandboxRegistry(t, sandbox.Config{DenyHostTemp: true}), want: "Host temp: no implicit write grant.", absent: "Additional temp write grants"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.registry != nil {
				defer tc.registry.Close()
			}
			got, err := tc.registry.SandboxContext()
			if err != nil || tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) || tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Fatalf("context = %q, %v", got, err)
			}
		})
	}
}

func TestSandboxContextPolicyErrorsAreNotReportedAsPermissions(t *testing.T) {
	registry := stubSandboxRegistry(t, sandbox.Config{Env: map[string]string{"invalid=name": "value"}})
	defer registry.Close()
	if got, err := registry.SandboxContext(); err == nil || got != "" {
		t.Fatalf("invalid policy context = %q, %v", got, err)
	}
}

func TestSandboxContextListsAreBoundedAndQuoted(t *testing.T) {
	values := []string{"line\nbreak", "line\nbreak", strings.Repeat("x", 2000)}
	for i := 15; i >= 0; i-- {
		values = append(values, fmt.Sprintf("path-%02d", i))
	}
	var b strings.Builder
	writeSandboxContextList(&b, "Paths", values)
	got := b.String()
	if len(got) > 1200 || strings.Count(got, "\n") != 1 || !strings.Contains(got, `"line\nbreak"`) || !strings.Contains(got, "(10 more entries omitted)") {
		t.Fatalf("unbounded or ambiguous list: %q", got)
	}
	if strings.Index(got, "path-00") > strings.Index(got, "path-01") {
		t.Fatal("list is not sorted")
	}
}

func TestSandboxContextDoesNotConfuseGitPointerWithMetadata(t *testing.T) {
	root := realTempDir(t)
	t.Chdir(root)
	git := filepath.Join(root, ".git")
	if err := os.WriteFile(git, []byte("gitdir: elsewhere\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := stubSandboxRegistry(t, sandbox.Config{WritablePaths: []string{root}, DenyWritePaths: []string{git}})
	defer registry.Close()
	got, err := registry.SandboxContext()
	if err != nil || strings.Contains(got, "Git metadata: read-only") {
		t.Fatalf("protected pointer mistaken for read-only metadata: %v\n%s", err, got)
	}
}
