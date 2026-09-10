package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func fixture(t *testing.T) (*Manager, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git fixture")
	}
	// Other sandbox packages replace paths under their HOME to test races.
	// Keep this fixture's credential reservations independent of those paths.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	root := filepath.Join(dir, "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "test")
	gitTest(t, root, "config", "user.email", "test@example.invalid")
	writeTest(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "base")
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	t.Cleanup(func() { registry.Close() })
	m, err := New(context.Background(), Config{Root: root, Directory: filepath.Join(dir, "runtime"), Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

func TestGitVersionGate(t *testing.T) {
	for _, version := range []string{"git version 2.38.5", "git version 2.39.9", "git version unknown", "bad"} {
		if validateGitVersion(version) == nil {
			t.Fatalf("accepted unsupported %q", version)
		}
	}
	for _, version := range []string{"git version 2.40.0", "git version 2.50.1 (Apple Git-155)", "git version 3.0.0"} {
		if err := validateGitVersion(version); err != nil {
			t.Fatalf("rejected %q: %v", version, err)
		}
	}
}

func TestCaptureRetainsSameSizeEditWithRacyIndex(t *testing.T) {
	m, root := fixture(t)
	gitTest(t, root, "config", "core.trustctime", "false")
	path := filepath.Join(root, "racy.bin")
	stamp := time.Now().Add(-5 * time.Second).Truncate(time.Second)
	writeTest(t, path, "\x00\x01\x02")
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", ".")
	indexPath := filepath.Join(root, ".git", "index")
	if err := os.Chtimes(indexPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, path, "\x00\xff\x03")
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Capture(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, root, "show", snapshot.Commit+":racy.bin"); !bytes.Equal(got, []byte{0, 255, 3}) {
		t.Fatalf("snapshot lost the same-size edit: %v", got)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(indexPath)
	if err != nil || !bytes.Equal(before, after) || !info.ModTime().Equal(stamp) {
		t.Fatalf("capture changed the parent index: %v", err)
	}
}

func TestIntegrationBinaryRenameDeletionAndCleanupDrift(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	writeTest(t, filepath.Join(root, "blob.bin"), "\x00\x01\x02")
	writeTest(t, filepath.Join(root, "remove.txt"), "remove this\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "fixture assets")
	index, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(child.Path, "a.txt"), filepath.Join(child.Path, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(child.Path, "remove.txt")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(child.Path, "blob.bin"), "\x00\xff\x03")
	candidate, err := m.Capture(ctx, child.Path)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := m.Preview(ctx, base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(ctx, preview); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("rename source remains")
	}
	if _, err := os.Stat(filepath.Join(root, "remove.txt")); !os.IsNotExist(err) {
		t.Fatal("deleted file remains")
	}
	if _, err := os.Stat(filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(root, "blob.bin"))
	if err != nil || !bytes.Equal(blob, []byte{0, 255, 3}) {
		t.Fatalf("binary result %v %v", blob, err)
	}
	after, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if !bytes.Equal(index, after) {
		t.Fatal("integration modified index")
	}
	writeTest(t, filepath.Join(child.Path, "late.txt"), "retain this\n")
	if err := m.Cleanup(ctx, child, candidate.Tree); err == nil {
		t.Fatal("cleanup discarded edits made after acceptance")
	}
	if _, err := os.Stat(filepath.Join(child.Path, "late.txt")); err != nil {
		t.Fatal("cleanup changed drifted files")
	}
}

func TestMemberProcessSandbox(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	old, root := fixture(t)
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()))
	defer registry.Close()
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	m, err := New(context.Background(), Config{Root: root, Directory: old.Directory, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	denied := []string{m.Root}
	for _, slot := range m.Slots {
		if slot != filepath.Dir(child.Path) {
			denied = append(denied, slot)
		}
	}
	ec, err := registry.ExecutionPolicy(child.Path, tools.ExecutionGrant{DeniedReads: denied, DeniedWrites: []string{m.GitDir, filepath.Join(child.Path, ".git")}, Scratch: child.ScratchDir()})
	if err != nil {
		t.Fatal(err)
	}
	ec.Sandbox.ReadPaths = []string{m.GitDir}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	bash, _ := bound.Get("bash")
	if out, err := bash.Execute(ctx, map[string]any{"command": `pwd; git status --porcelain; printf allowed > member.txt; printf t > "$TMPDIR/probe" && test -f '` + child.ScratchDir() + `/probe'`}); err != nil || !strings.Contains(out, child.Path) {
		t.Fatalf("member command: %s %v", out, err)
	}
	// Construct this sibling after the member sandbox. Its pre-reserved path
	// must already be hidden, including through direct shell reads.
	sibling, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	quote := func(path string) string { return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'" }
	for _, command := range []string{"cat " + quote(filepath.Join(m.Root, "a.txt")), "cat " + quote(filepath.Join(sibling.Path, "a.txt")), "ls " + quote(sibling.ScratchDir()), "git add member.txt", "printf bad > " + quote(filepath.Join(m.GitDir, "config"))} {
		if out, err := bash.Execute(ctx, map[string]any{"command": command}); err == nil {
			t.Fatalf("sandbox allowed %s: %s", command, out)
		}
	}
	if _, err := m.Capture(ctx, child.Path); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePresetWorktreeAdministration(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	for _, preset := range []string{"workspace", "workspace+git"} {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/linked=%t", preset, linked), func(t *testing.T) {
				old, main := fixture(t)
				root := main
				if linked {
					root = filepath.Join(t.TempDir(), "parent")
					gitTest(t, main, "worktree", "add", "--detach", root, "HEAD")
				}
				t.Chdir(root)
				cfg, err := sandbox.ParsePreset(preset)
				if err != nil {
					t.Fatal(err)
				}
				registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, cfg))
				defer registry.Close()
				if _, err := registry.LoadToolAuto("bash"); err != nil {
					t.Fatal(err)
				}
				m, err := New(context.Background(), Config{Root: root, Directory: old.Directory, Registry: registry, MaxWorktrees: 8})
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				indexPath := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "--path-format=absolute", "--git-path", "index")))
				index, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				head := gitTest(t, root, "rev-parse", "HEAD")
				writeTest(t, filepath.Join(root, "dirty.txt"), "review this uncommitted file\n")
				base, err := m.Capture(ctx, root)
				if err != nil {
					t.Fatalf("runtime snapshot under workspace preset: %v", err)
				}
				child, err := m.Create(ctx, base)
				if err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(filepath.Join(child.Path, "dirty.txt")); err != nil || string(got) != "review this uncommitted file\n" {
					t.Fatalf("dirty snapshot: %q %v", got, err)
				}
				if linked || preset == "workspace" {
					parentBash, _ := registry.Get("bash")
					if out, err := parentBash.Execute(ctx, map[string]any{"command": "git add dirty.txt"}); err == nil {
						t.Fatalf("runtime grant leaked into parent tool: %s", out)
					}
				}
				if _, err := m.git(ctx, root, nil, nil, "config", "polly.runtime-test", "forbidden"); err == nil {
					t.Fatal("runtime could rewrite repository config")
				}
				writeTest(t, filepath.Join(child.Path, "a.txt"), "accepted change\n")
				candidate, err := m.Capture(ctx, child.Path)
				if err != nil {
					t.Fatal(err)
				}
				preview, err := m.Preview(ctx, base, candidate)
				if err != nil {
					t.Fatal(err)
				}
				if err := m.Apply(ctx, preview); err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(got) != "accepted change\n" {
					t.Fatalf("apply: %q %v", got, err)
				}
				if err := m.Cleanup(ctx, child, candidate.Tree); err != nil {
					t.Fatal(err)
				}
				if err := m.Cleanup(ctx, preview.Checkout, preview.Merged.Tree); err != nil {
					t.Fatal(err)
				}
				if err := m.CleanupSnapshotRefs(ctx); err != nil {
					t.Fatal(err)
				}
				after, _ := os.ReadFile(indexPath)
				if !bytes.Equal(index, after) || !bytes.Equal(head, gitTest(t, root, "rev-parse", "HEAD")) {
					t.Fatal("runtime changed parent index or HEAD")
				}
			})
		}
	}
}

func TestRuntimeGitPreservesExplicitRestrictions(t *testing.T) {
	m, root := fixture(t)
	t.Chdir(root)
	base, err := sandbox.ParsePreset("workspace")
	if err != nil {
		t.Fatal(err)
	}
	base, err = sandbox.PrepareConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		deny sandbox.Config
	}{
		{"same path as preset", sandbox.Config{DenyWritePaths: []string{m.GitDir}}},
		{"object store", sandbox.Config{DenyWritePaths: []string{filepath.Join(m.GitDir, "objects")}}},
		{"ancestor", sandbox.Config{DenyWritePaths: []string{root}}},
		{"read denial", sandbox.Config{DenyPaths: []string{m.GitDir}}},
		{"all writes", sandbox.Config{DenyWrite: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := sandbox.PrepareConfig(base.Merge(tc.deny))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sandbox.RuntimeGitConfig(cfg, m.GitDir, m.Directory); err == nil {
				t.Fatal("runtime bypassed explicit sandbox restriction")
			}
		})
	}
	other := filepath.Join(root, "private.txt")
	writeTest(t, other, "private")
	cfg, err := sandbox.RuntimeGitConfig(base.Merge(sandbox.Config{DenyPaths: []string{other}}), m.GitDir, m.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ReadAllowed(cfg, other) == nil || sandbox.WriteAllowed(base, filepath.Join(m.GitDir, "objects", "new")) == nil {
		t.Fatal("runtime changed a read restriction or the original policy")
	}
}

func TestWorktreeErrorsDistinguishNonRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git fixture")
	}
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	root := t.TempDir()
	c := Config{Root: root, Directory: t.TempDir(), Registry: registry}
	if _, err := New(context.Background(), c); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("outside Git: %v", err)
	}
	writeTest(t, filepath.Join(root, ".git"), "gitdir: /missing/polly-gitdir\n")
	if _, err := New(context.Background(), c); err == nil || errors.Is(err, ErrNotRepository) {
		t.Fatalf("broken Git incorrectly permits live fallback: %v", err)
	}
}

func gitTest(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return out
}
func writeTest(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDirtyCaptureAndIntegrationPreserveParent(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	writeTest(t, filepath.Join(root, "staged.txt"), "staged\n")
	gitTest(t, root, "add", "staged.txt")
	writeTest(t, filepath.Join(root, "staged.txt"), "unstaged too\n")
	writeTest(t, filepath.Join(root, "new.txt"), "new\n")
	index, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	head := gitTest(t, root, "rev-parse", "HEAD")
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(c.Path, "staged.txt"))
	if string(data) != "unstaged too\n" {
		t.Fatalf("dirty source missing: %q", data)
	}
	writeTest(t, filepath.Join(c.Path, "a.txt"), "changed\ntwo\nthree\n")
	candidate, err := m.Capture(ctx, c.Path)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "parent-only.txt"), "preserve\n")
	p, err := m.Preview(ctx, base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if p.Conflicts != "" {
		t.Fatal(p.Conflicts)
	}
	if err = m.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	gotIndex, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if !bytes.Equal(index, gotIndex) || !bytes.Equal(head, gitTest(t, root, "rev-parse", "HEAD")) {
		t.Fatal("integration changed parent index or HEAD")
	}
	data, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "changed\ntwo\nthree\n" {
		t.Fatalf("delta missing: %q", data)
	}
	if _, err = os.Stat(filepath.Join(root, "parent-only.txt")); err != nil {
		t.Fatal("lost parent change")
	}
	if err = m.Cleanup(ctx, c, ""); err == nil {
		t.Fatal("cleanup discarded unintegrated work")
	}
}

func TestPreviewConflictsAndDriftRefuseApply(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(c.Path, "a.txt"), "child\n")
	candidate, err := m.Capture(ctx, c.Path)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "a.txt"), "parent\n")
	p, err := m.Preview(ctx, base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if p.Conflicts == "" {
		t.Fatal("conflict not detected")
	}
	if err = m.Apply(ctx, p); err == nil {
		t.Fatal("applied conflict")
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "parent\n" {
		t.Fatal("preview changed parent")
	}
	writeTest(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree\n")
	p, err = m.Preview(ctx, base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "drift.txt"), "drift")
	if err = m.Apply(ctx, p); err == nil {
		t.Fatal("applied stale preview")
	}
}

func TestCaptureHonorsGlobalExcludesAndRefusesFilters(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	// A global excludes file outside the checkout: the user's git hides .env,
	// so the snapshot must hide it too even though isolated commands see no
	// global configuration.
	home := t.TempDir()
	writeTest(t, filepath.Join(home, "ignore"), ".env\n")
	writeTest(t, filepath.Join(home, "gitconfig"), "[core]\n\texcludesFile = "+filepath.Join(home, "ignore")+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	m, err := New(ctx, m.Config)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, ".env"), "SECRET=1\n")
	writeTest(t, filepath.Join(root, "kept.txt"), "kept\n")
	s, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	listing := string(gitTest(t, root, "ls-tree", "-r", "--name-only", s.Tree))
	if strings.Contains(listing, ".env") || !strings.Contains(listing, "kept.txt") {
		t.Fatalf("snapshot ignored the global excludes file: %q", listing)
	}
	// LFS installs its filter globally; only paths whose attributes use a
	// filter make the repository unsupported.
	writeTest(t, filepath.Join(home, "gitconfig"), "[filter \"lfs\"]\n\tclean = cat\n\tsmudge = cat\n")
	if _, err = m.Capture(ctx, root); err != nil {
		t.Fatalf("a configured but unused filter blocked capture: %v", err)
	}
	writeTest(t, filepath.Join(root, ".gitattributes"), "*.bin filter=lfs\n")
	writeTest(t, filepath.Join(root, "big.bin"), "binary\n")
	if _, err = m.Capture(ctx, root); err == nil || !strings.Contains(err.Error(), "content filters") {
		t.Fatalf("filtered path accepted: %v", err)
	}
}

func TestValidateTreeRefusesGitlinksAndOversizedUntrackedBlobs(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	head := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	index := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := m.git(ctx, root, env, nil, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub"); err != nil {
		t.Fatal(err)
	}
	tree, err := m.git(ctx, root, env, nil, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.validateTree(ctx, root, strings.TrimSpace(string(tree)), nil); err == nil || !strings.Contains(err.Error(), "submodules") {
		t.Fatalf("gitlink published: %v", err)
	}
	m.MaxUntrackedFileBytes = 4
	if err := m.validateTree(ctx, root, head+"^{tree}", nil); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized untracked blob published: %v", err)
	}
	if err := m.validateTree(ctx, root, head+"^{tree}", map[string]bool{"a.txt": true}); err != nil {
		t.Fatalf("tracked blob capped: %v", err)
	}
}

func TestFailedCreateReleasesSlotAndNewReclaimsStaleClaims(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	bogus := base
	bogus.Commit = strings.Repeat("0", 40)
	if _, err := m.Create(ctx, bogus); err == nil {
		t.Fatal("bogus commit checked out")
	}
	if _, err := os.Stat(filepath.Join(m.Slots[0], "owner")); err == nil {
		t.Fatal("failed create kept its slot claim")
	}
	c, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(c.Path) != m.Slots[0] {
		t.Fatalf("released slot not reused: %s", c.Path)
	}
	// A claim with no manifest is a crashed create once it is old enough; a
	// fresh one may belong to another runtime mid-checkout and stays.
	stale := filepath.Join(m.Slots[1], "owner")
	writeTest(t, stale, "deadbeef")
	old := time.Now().Add(-2 * staleClaim)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(m.Slots[2], "owner"), "cafef00d")
	if _, err := New(ctx, m.Config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale claim survived New")
	}
	if _, err := os.Stat(filepath.Join(m.Slots[2], "owner")); err != nil {
		t.Fatal("fresh claim reclaimed")
	}
	if _, err := os.Stat(filepath.Join(m.Slots[0], "owner")); err != nil {
		t.Fatal("live checkout reclaimed")
	}
}

func TestCreateProvidesSlotScratchAndCleanupRemovesIt(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	scratch := child.ScratchDir()
	if scratch != filepath.Join(filepath.Dir(child.Path), "scratch") {
		t.Fatalf("scratch = %s, want a sibling of %s", scratch, child.Path)
	}
	if info, err := os.Stat(scratch); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch stat = %v %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "note"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup(ctx, child, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup kept the scratch: %v", err)
	}
	stale := filepath.Join(m.Slots[0], "scratch")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "stale"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(again.Path) != m.Slots[0] {
		t.Fatalf("freed slot not reused: %s", again.Path)
	}
	if _, err := os.Stat(filepath.Join(again.ScratchDir(), "stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reused slot kept an old scratch: %v", err)
	}
}

func snapshotRefCount(t *testing.T, root string) int {
	t.Helper()
	out := gitTest(t, root, "for-each-ref", "--format=%(refname)", "refs/polly/snapshots/")
	return len(strings.Fields(string(out)))
}

// An unchanged copy is recognized without a capture; anything that could hide
// an edit from status declines, and the full capture still refuses the edit.
func TestUnchangedCheckSkipsCaptureAndFallsBackOnHiddenEdits(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	expect := func(step string, want bool) {
		t.Helper()
		got, err := m.Unchanged(ctx, child)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if got != want {
			t.Fatalf("%s: unchanged = %v, want %v", step, got, want)
		}
	}
	refuse := func(step string) {
		t.Helper()
		if err := m.Cleanup(ctx, child, ""); err == nil || !strings.Contains(err.Error(), "unintegrated") {
			t.Fatalf("%s: cleanup = %v, want a refusal", step, err)
		}
	}
	expect("fresh checkout", true)
	file := filepath.Join(child.Path, "a.txt")
	writeTest(t, file, "one\ntwo\nthree\nfour\n")
	expect("modified file", false)
	refuse("modified file")
	gitTest(t, child.Path, "update-index", "--assume-unchanged", "a.txt")
	expect("edit hidden by assume-unchanged", false)
	refuse("edit hidden by assume-unchanged")
	gitTest(t, child.Path, "update-index", "--no-assume-unchanged", "a.txt")
	gitTest(t, child.Path, "checkout", "--", "a.txt")
	expect("restored file", true)
	writeTest(t, filepath.Join(child.Path, "new.txt"), "x\n")
	expect("untracked file", false)
	refuse("untracked file")
	if err := os.Remove(filepath.Join(child.Path, "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	expect("deleted file", false)
	refuse("deleted file")
	gitTest(t, child.Path, "checkout", "--", "a.txt")
	gitTest(t, child.Path, "config", "core.sparseCheckout", "true")
	expect("sparse checkout setting", false)
	gitTest(t, child.Path, "config", "--unset", "core.sparseCheckout")
	expect("setting cleared", true)
	writeTest(t, filepath.Join(root, "a.txt"), "changed upstream\n")
	gitTest(t, root, "commit", "-qam", "next")
	next := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	gitTest(t, child.Path, "checkout", "-q", "--detach", next)
	expect("head on another tree", false)
	refuse("head on another tree")
	gitTest(t, child.Path, "checkout", "-q", "--detach", base.Commit)
	expect("head restored", true)
	refs := snapshotRefCount(t, root)
	if err := m.Cleanup(ctx, child, ""); err != nil {
		t.Fatal(err)
	}
	if got := snapshotRefCount(t, root); got != refs {
		t.Fatalf("cleanup of an unchanged copy captured a snapshot: refs %d -> %d", refs, got)
	}
	if _, err := os.Stat(child.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup kept the checkout: %v", err)
	}
	if _, err := m.Unchanged(ctx, child); err == nil {
		t.Fatal("unchanged accepted a checkout the runtime no longer owns")
	}
}
