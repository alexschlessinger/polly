package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// A trial reports what the real sandbox denied its command, with each
// denial's cause, applies a candidate to that one command only, and leaves
// the registry's policy as it was.
func TestRunTrialReportsWhatTheSandboxDenied(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process sandbox")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	notes := filepath.Join(home, "notes")
	if err := os.MkdirAll(notes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "todo.txt"), []byte("trial-fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session's workspace inside the home directory, as the workspace
	// preset grants it.
	work := filepath.Join(home, "src", "project")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	base := sandbox.DefaultConfig()
	base.WritablePaths = append(base.WritablePaths, work)
	registry := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, base))
	defer registry.Close()
	registry.executionRoot = work
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := `cat "$HOME/notes/todo.txt"; mkdir -p "$HOME/.cache/trial-tool/objects" && echo x > "$HOME/.cache/trial-tool/objects/a"; echo done; exit 3`

	result, err := registry.RunTrial(ctx, command, sandbox.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 3 || !strings.Contains(result.Output, "done") || strings.Contains(result.Output, "trial-fixture") {
		t.Fatalf("trial = exit %d, output %q; want exit 3 and no fixture", result.ExitCode, result.Output)
	}
	obs := result.Observation
	t.Logf("observation: %+v", obs)
	if obs.Incomplete != "" {
		t.Fatalf("observation incomplete: %s", obs.Incomplete)
	}
	cache := filepath.Join(home, ".cache")
	write := slices.IndexFunc(obs.Denials, func(d sandbox.Denial) bool {
		return d.Access == sandbox.AccessWrite && (d.Path == cache || sandbox.PathWithin(d.Path, cache))
	})
	if write < 0 || obs.Denials[write].Cause != sandbox.CauseNotWritable {
		t.Fatalf("denials %+v; want a write under %s the policy does not allow", obs.Denials, cache)
	}
	switch runtime.GOOS {
	case "darwin":
		read := slices.IndexFunc(obs.Denials, func(d sandbox.Denial) bool {
			return d.Access == sandbox.AccessRead && (d.Path == notes || sandbox.PathWithin(d.Path, notes))
		})
		if read < 0 || obs.Denials[read].Cause != sandbox.CausePrivate {
			t.Fatalf("denials %+v; want a private read under %s", obs.Denials, notes)
		}
		for _, d := range obs.Denials {
			if strings.Contains(d.Path, "canary") || d.Path == "/dev/dtracehelper" {
				t.Fatalf("observation kept a canary or noise: %+v", d)
			}
		}
	case "linux":
		if d := obs.Denials[write]; !d.Discarded || d.Path != filepath.Join(cache, "trial-tool", "objects") || d.Count != 4 {
			t.Fatalf("discarded write = %+v; want the objects directory with 4 entries", d)
		}
		if obs.Limit == "" {
			t.Fatal("a Linux observation must say what it cannot see")
		}
	}

	// A candidate reaches the trial's command and nothing else.
	result, err = registry.RunTrial(ctx, `cat "$HOME/notes/todo.txt"`, sandbox.Config{ReadPaths: []string{notes}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Output, "trial-fixture") {
		t.Fatalf("trial with the candidate = exit %d, output %q", result.ExitCode, result.Output)
	}
	if slices.ContainsFunc(result.Observation.Denials, func(d sandbox.Denial) bool { return d.Access == sandbox.AccessRead }) {
		t.Fatalf("the candidate's read was still denied: %+v", result.Observation.Denials)
	}
	if cfg, _, err := registry.SandboxReadPolicy(); err != nil || slices.Contains(cfg.ReadPaths, notes) {
		t.Fatalf("registry policy after a trial = %v, %v; want the candidate gone", cfg.ReadPaths, err)
	}
}
