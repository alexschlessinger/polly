//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A trial's profile tags every deny rule but the signal rule, and removing
// the tags gives back the profile every other command gets.
func TestDarwinProfileTagsEveryDenyRule(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "work")
	git := filepath.Join(work, ".git")
	if err := os.MkdirAll(git, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{WritablePaths: []string{work}, DenyWritePaths: []string{git}, DenyPaths: []string{filepath.Join(home, "private")}},
		{AllowNetwork: true, DenyDNS: true, WritablePaths: []string{work}},
		{AllowNetwork: true, DenyWrite: true},
	} {
		plain := buildProfile(cfg)
		cfg.denialTag = testDenialTag
		tagged := buildProfile(cfg)
		modifier := fmt.Sprintf(" (with message %q)", testDenialTag)
		if untagged := strings.ReplaceAll(tagged, modifier, ""); untagged != plain {
			t.Fatalf("removing the tags changed the profile:\n%s\nwant\n%s", untagged, plain)
		}
		if strings.Contains(plain, "with message") {
			t.Fatalf("an untagged profile carries a message:\n%s", plain)
		}
		denies := 0
		for line := range strings.Lines(tagged) {
			if !strings.HasPrefix(line, "(deny ") {
				continue
			}
			denies++
			if tagged := strings.Contains(line, modifier); tagged == (line == "(deny signal)\n") {
				t.Fatalf("deny rule %q: tagged %v", line, tagged)
			}
		}
		if denies < 3 {
			t.Fatalf("profile has %d deny rules:\n%s", denies, tagged)
		}
	}
}

func TestDenialObserverRefusesAnUntaggedSandbox(t *testing.T) {
	skipIfNoSandboxExec(t)
	t.Setenv("HOME", t.TempDir())
	observer, err := NewDenialObserver()
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Start(context.Background(), sb); !errors.Is(err, ErrDenialsUnobservable) {
		t.Fatalf("Start on an untagged sandbox = %v, want ErrDenialsUnobservable", err)
	}
}

// The observer sees a trial's network denials along with its file denials,
// and nothing of a command run outside the trial's sandbox.
func TestDenialObserverReportsNetworkDenials(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	skipIfNoSandboxExec(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	observer, err := NewDenialObserver()
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	sb, err := New(observer.Config(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := observer.Start(ctx, sb); err != nil {
		t.Fatal(err)
	}
	// Another sandbox's denials never reach this observer.
	other, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "outside")
	run := func(sb Sandbox, script string) {
		cmd := exec.CommandContext(ctx, "bash", "-c", script)
		cleanup, err := WrapCmdManaged(sb, cmd)
		if err != nil {
			t.Fatal(err)
		}
		_ = cmd.Run()
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
	}
	run(other, "touch "+outside)
	run(sb, "/usr/bin/nc -z -w 1 127.0.0.1 9; touch "+filepath.Join(home, "inside"))
	obs, err := observer.Finish(ctx, sb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Incomplete != "" {
		t.Fatalf("observation incomplete: %s", obs.Incomplete)
	}
	has := func(access Access, path string) bool {
		return slices.ContainsFunc(obs.Denials, func(d Denial) bool { return d.Access == access && d.Path == path })
	}
	if !has(AccessNetwork, "remote:*:9") || !has(AccessWrite, filepath.Join(home, "inside")) || has(AccessWrite, outside) {
		t.Fatalf("denials = %+v; want the trial's network and write denials and nothing of the other sandbox", obs.Denials)
	}
}
