// Package worktree captures dirty Git checkouts and prepares isolated changes.
// Members edit files; only this runtime performs repository Git writes.
package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type Snapshot struct {
	ID     string `json:"id"`
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	Source string `json:"source"`
}
type Checkout struct {
	ID   string   `json:"id"`
	Path string   `json:"path"`
	Base Snapshot `json:"base"`
}
type Preview struct {
	ID        string   `json:"id"`
	Parent    Snapshot `json:"parent"`
	Candidate Snapshot `json:"candidate"`
	Merged    Snapshot `json:"merged"`
	Checkout  Checkout `json:"checkout"`
	Conflicts string   `json:"conflicts,omitempty"`
}
type Config struct {
	Root, Directory                          string
	Registry                                 *tools.ToolRegistry
	MaxUntrackedFileBytes, MaxUntrackedBytes int64
	// MaxWorktrees reserves paths before any member sandbox starts. A member
	// can then deny future sibling paths as well as existing ones.
	MaxWorktrees int
}
type Manager struct {
	Config
	Git, GitDir string
	Slots       []string
	mu          sync.Mutex
	sandbox     sandbox.Sandbox
	// userConfig pins the user's global ignore and attribute files for the
	// isolated commands, which otherwise see no global configuration.
	userConfig []string
}

// staleClaim is how long a slot claim without a checkout manifest is trusted
// to be a create still in progress in another runtime before New reclaims it.
const staleClaim = time.Hour

// ErrNotRepository permits a host to offer live, read-only research outside Git.
// Broken repositories and sandbox failures never return this sentinel.
var ErrNotRepository = errors.New("isolated editing requires a Git checkout")

func New(ctx context.Context, c Config) (*Manager, error) {
	if c.Registry == nil {
		return nil, errors.New("worktree registry is required")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	c.Root = root
	// Distinguish a live research directory outside Git from a broken or
	// inaccessible repository. Only the former permits read-only fallback.
	for path := root; ; path = filepath.Dir(path) {
		_, err := os.Lstat(filepath.Join(path, ".git"))
		if err == nil {
			if path != root {
				return nil, fmt.Errorf("source must be a checkout root; Git root is %s", path)
			}
			break
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		if filepath.Dir(path) == path {
			return nil, ErrNotRepository
		}
	}
	directory, err := filepath.Abs(c.Directory)
	if err != nil {
		return nil, err
	}
	if sandbox.PathWithin(directory, root) {
		return nil, errors.New("runtime worktree directory must be outside source checkout")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	c.Directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	if c.MaxUntrackedFileBytes <= 0 {
		c.MaxUntrackedFileBytes = 32 << 20
	}
	if c.MaxUntrackedBytes <= 0 {
		c.MaxUntrackedBytes = 256 << 20
	}
	git, err := sandbox.TrustedGitExecutable([]string{c.Directory})
	if err != nil {
		return nil, err
	}
	m := &Manager{Config: c, Git: git}
	// Resolve Git metadata read-only before granting runtime administrative
	// writes. Member sandboxes are constructed separately and deny these paths.
	cfg, _, err := c.Registry.SandboxReadPolicy()
	if err != nil {
		return nil, err
	}
	cfg.WritablePaths = []string{c.Directory}
	cfg, err = sandbox.RuntimeGitReadConfig(cfg, c.Root)
	if err != nil {
		return nil, err
	}
	if c.Registry.HasSandbox() {
		m.sandbox, err = c.Registry.NewSandboxDirect(cfg)
		if err != nil {
			return nil, err
		}
	}
	if !c.Registry.HasSandbox() && !c.Registry.UnsafeNoSandbox() {
		return nil, errors.New("editing requires a process sandbox or explicit unsafe acknowledgement")
	}
	version, err := m.git(ctx, c.Root, nil, nil, "--version")
	if err != nil {
		return nil, err
	}
	if err := validateGitVersion(string(version)); err != nil {
		return nil, err
	}
	// Global ignore and attribute files decide what a capture stages. Read
	// them as the user's own git would, then pin them for isolated commands.
	for _, key := range []string{"core.excludesFile", "core.attributesFile"} {
		value, _ := m.gitUser(ctx, c.Root, "config", "--get", "--type=path", key)
		if path := strings.TrimSpace(string(value)); path != "" {
			m.userConfig = append(m.userConfig, "-c", key+"="+path)
		}
	}
	gitdir, err := m.git(ctx, c.Root, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	m.GitDir = strings.TrimSpace(string(gitdir))
	for _, path := range []string{filepath.Join(c.Root, ".git"), m.GitDir} {
		real, e := filepath.EvalSymlinks(path)
		if e != nil {
			return nil, e
		}
		if real != path {
			return nil, fmt.Errorf("symlinked Git metadata is unsupported: %s", path)
		}
	}
	cfg, err = sandbox.RuntimeGitConfig(cfg, m.GitDir, c.Directory)
	if err != nil {
		return nil, fmt.Errorf("prepare runtime Git administration: %w", err)
	}
	if c.Registry.HasSandbox() {
		m.sandbox, err = c.Registry.NewSandboxDirect(cfg)
		if err != nil {
			return nil, err
		}
	}
	if m.MaxWorktrees <= 0 {
		m.MaxWorktrees = 512
	}
	for n := 0; n < m.MaxWorktrees; n++ {
		slot := filepath.Join(c.Directory, fmt.Sprintf("slot-%04d", n))
		if err := os.MkdirAll(slot, 0700); err != nil {
			return nil, err
		}
		m.Slots = append(m.Slots, slot)
		m.reclaimStale(ctx, slot)
	}
	return m, nil
}

// reclaimStale frees a slot whose claim never produced a checkout manifest:
// a create that failed or a runtime that died mid-checkout. Another live
// runtime could still be inside its create, so only old claims are reclaimed.
func (m *Manager) reclaimStale(ctx context.Context, slot string) {
	ownerPath := filepath.Join(slot, "owner")
	owner, err := os.ReadFile(ownerPath)
	if err != nil || strings.ContainsAny(string(owner), `/\`) {
		return
	}
	if _, err := os.Stat(filepath.Join(m.Directory, string(owner)+".json")); err == nil {
		return
	}
	info, err := os.Stat(ownerPath)
	if err != nil || time.Since(info.ModTime()) < staleClaim {
		return
	}
	m.releaseSlot(ctx, filepath.Join(slot, "tree"))
}

// releaseSlot returns a claimed slot after a failed or abandoned create. The
// caller guarantees no checkout manifest names the claim.
func (m *Manager) releaseSlot(ctx context.Context, tree string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if _, err := os.Lstat(tree); err == nil {
		// worktree remove refuses trees it never finished registering.
		if _, err := m.git(ctx, m.Root, nil, nil, "worktree", "remove", "--force", tree); err != nil {
			os.RemoveAll(tree)
			m.git(ctx, m.Root, nil, nil, "worktree", "prune")
		}
	}
	os.Remove(filepath.Join(filepath.Dir(tree), "owner"))
}

func validateGitVersion(version string) error {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "git version "), ".")
	if len(parts) < 2 {
		return errors.New("cannot determine Git version")
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return errors.New("cannot determine Git version")
	}
	if major < 2 || major == 2 && minor < 40 {
		return errors.New("integration requires Git 2.40 or newer")
	}
	return nil
}

// git runs a repository command with the user's global and system
// configuration hidden, so hooks, aliases, and helpers never fire. The ignore
// and attribute files the user configured globally still apply via userConfig.
func (m *Manager) git(ctx context.Context, cwd string, env []string, input []byte, args ...string) ([]byte, error) {
	return m.run(ctx, cwd, env, input, true, args...)
}

// gitUser reads configuration and attributes exactly as the user's own git
// resolves them. Only queries belong here; it never writes.
func (m *Manager) gitUser(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	return m.run(ctx, cwd, nil, nil, false, args...)
}

func (m *Manager) run(ctx context.Context, cwd string, env []string, input []byte, isolate bool, args ...string) ([]byte, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false"}
	if isolate {
		base = append(base, m.userConfig...)
	}
	cmd := exec.CommandContext(ctx, m.Git, append(base, args...)...)
	cmd.Dir = cwd
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		userConfigKey := key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_NOSYSTEM"
		if !strings.HasPrefix(key, "GIT_") || !isolate && userConfigKey {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_AUTHOR_NAME=Polly runtime", "GIT_AUTHOR_EMAIL=polly@localhost", "GIT_COMMITTER_NAME=Polly runtime", "GIT_COMMITTER_EMAIL=polly@localhost")
	if isolate {
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = bytes.NewReader(input)
	cleanup, err := sandbox.WrapCmdManaged(m.sandbox, cmd)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

func (m *Manager) Capture(ctx context.Context, source string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, err := m.capture(ctx, source)
	if err != nil {
		return Snapshot{}, fmt.Errorf("capture snapshot from %q: %w", source, err)
	}
	return snapshot, nil
}

func seedIndex(source, destination string) error {
	file, err := os.Open(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	seed, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination, seed, 0600); err != nil {
		return err
	}
	// Git uses the index timestamp to rehash racily clean entries. A fresh
	// timestamp on these copied stat records can silently hide same-size edits.
	return os.Chtimes(destination, info.ModTime(), info.ModTime())
}
func (m *Manager) capture(ctx context.Context, source string, reuse ...Snapshot) (Snapshot, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return Snapshot{}, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return Snapshot{}, err
	}
	common, err := m.git(ctx, source, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(string(common)) != m.GitDir {
		return Snapshot{}, errors.New("v1 sources must belong to the parent's Git repository")
	}
	top, err := m.git(ctx, source, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(string(top)) != source {
		return Snapshot{}, errors.New("snapshot source must be a checkout root")
	}
	for _, key := range []string{"core.sparseCheckout", "core.splitIndex"} {
		out, _ := m.git(ctx, source, nil, nil, "config", "--bool", key)
		if strings.TrimSpace(string(out)) == "true" {
			return Snapshot{}, fmt.Errorf("unsupported repository setting %s", key)
		}
	}
	readPolicy, readActive, err := m.Registry.SandboxReadPolicy()
	if err != nil {
		return Snapshot{}, err
	}
	stages, err := m.git(ctx, source, nil, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return Snapshot{}, err
	}
	tracked := make(map[string]bool)
	var names []byte
	for _, entry := range bytes.Split(stages, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, name, _ := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[2] != "0" || fields[0] == "160000" {
			return Snapshot{}, errors.New("conflicted indexes and submodules are unsupported")
		}
		if err := m.checkSourcePath(source, name, readPolicy, readActive); err != nil {
			return Snapshot{}, err
		}
		tracked[name] = true
		names = append(append(names, name...), 0)
	}
	untracked, err := m.git(ctx, source, nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return Snapshot{}, err
	}
	var total int64
	for _, name := range bytes.Split(untracked, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		if err := m.checkSourcePath(source, string(name), readPolicy, readActive); err != nil {
			return Snapshot{}, err
		}
		names = append(append(names, name...), 0)
		info, e := os.Lstat(filepath.Join(source, string(name)))
		if e != nil {
			return Snapshot{}, e
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return Snapshot{}, fmt.Errorf("unsupported snapshot file: %s", name)
		}
		total += info.Size()
		if info.Size() > m.MaxUntrackedFileBytes || total > m.MaxUntrackedBytes {
			return Snapshot{}, fmt.Errorf("snapshot untracked size limit exceeded at %s", name)
		}
	}
	if err := m.checkFilters(ctx, source, names); err != nil {
		return Snapshot{}, err
	}
	indexPath, err := m.git(ctx, source, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return Snapshot{}, err
	}
	index := filepath.Join(m.Directory, "index-"+ids.New())
	defer os.Remove(index)
	if err := seedIndex(strings.TrimSpace(string(indexPath)), index); err != nil {
		return Snapshot{}, err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	paths, err := m.git(ctx, source, env, nil, "ls-files", "-z")
	if err != nil {
		return Snapshot{}, err
	}
	if len(paths) > 0 {
		if _, err = m.git(ctx, source, env, paths, "update-index", "--no-assume-unchanged", "--no-skip-worktree", "-z", "--stdin"); err != nil {
			return Snapshot{}, err
		}
	}
	if _, err = m.git(ctx, source, env, nil, "add", "-A", "--", "."); err != nil {
		return Snapshot{}, err
	}
	tree, err := m.git(ctx, source, env, nil, "write-tree")
	if err != nil {
		return Snapshot{}, err
	}
	// A second capture refuses a changing source instead of publishing a
	// known inconsistent set. External writers cannot be locked by Polly.
	if _, err = m.git(ctx, source, env, nil, "add", "-A", "--", "."); err != nil {
		return Snapshot{}, err
	}
	check, err := m.git(ctx, source, env, nil, "write-tree")
	if err != nil {
		return Snapshot{}, err
	}
	if !bytes.Equal(tree, check) {
		return Snapshot{}, errors.New("source changed during snapshot; retry")
	}
	// The checks above read the live index and worktree; the tree was built
	// afterwards. Only what the tree itself contains gets published.
	if err := m.validateTree(ctx, source, strings.TrimSpace(string(tree)), tracked); err != nil {
		return Snapshot{}, err
	}
	if len(reuse) == 1 && reuse[0].Tree == strings.TrimSpace(string(tree)) {
		return reuse[0], nil
	}
	return m.snapshotTree(ctx, strings.TrimSpace(string(tree)), source)
}

// checkFilters refuses a capture whose paths carry a content filter: `add`
// would store cleaned blobs, and members' checkouts would need the filter's
// smudge (for LFS, a network fetch) to read them. Attributes are resolved as
// the user's git resolves them, including a global core.attributesFile.
func (m *Manager) checkFilters(ctx context.Context, source string, names []byte) error {
	if len(names) == 0 {
		return nil
	}
	out, err := m.run(ctx, source, nil, names, false, "check-attr", "-z", "--stdin", "filter")
	if err != nil {
		return err
	}
	fields := bytes.Split(out, []byte{0})
	for i := 2; i < len(fields); i += 3 {
		if string(fields[i]) != "unspecified" {
			return fmt.Errorf("repositories with content filters or LFS are unsupported: %s", fields[i-2])
		}
	}
	return nil
}

// validateTree applies the capture rules to the tree actually written: no
// gitlinks, and the untracked size caps for every blob the live index did not
// already track when validation ran.
func (m *Manager) validateTree(ctx context.Context, source, tree string, tracked map[string]bool) error {
	entries, err := m.git(ctx, source, nil, nil, "ls-tree", "-r", "-l", "-z", tree)
	if err != nil {
		return err
	}
	var total int64
	for _, entry := range bytes.Split(entries, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, name, _ := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		if len(fields) != 4 {
			return errors.New("unreadable snapshot tree")
		}
		if fields[0] == "160000" || fields[1] != "blob" {
			return errors.New("conflicted indexes and submodules are unsupported")
		}
		if tracked[name] {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return errors.New("unreadable snapshot tree")
		}
		total += size
		if size > m.MaxUntrackedFileBytes || total > m.MaxUntrackedBytes {
			return fmt.Errorf("snapshot untracked size limit exceeded at %s", name)
		}
	}
	return nil
}

func (m *Manager) snapshotTree(ctx context.Context, tree, source string) (Snapshot, error) {
	s := Snapshot{ID: ids.New(), Tree: tree, Source: source}
	commit, err := m.git(ctx, m.Root, nil, []byte("polly immutable snapshot\n"), "commit-tree", tree)
	if err != nil {
		return Snapshot{}, err
	}
	s.Commit = strings.TrimSpace(string(commit))
	if _, err = m.git(ctx, m.Root, nil, nil, "update-ref", "refs/polly/snapshots/"+s.ID, s.Commit); err != nil {
		return Snapshot{}, err
	}
	if err = m.manifest("snapshot-"+s.ID, s); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// CleanupSnapshotRefs removes only references recorded in this runtime's
// manifests. Call after every retained checkout and publication is retired.
func (m *Manager) CleanupSnapshotRefs(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, err := filepath.Glob(filepath.Join(m.Directory, "snapshot-*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var snapshot Snapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return err
		}
		if snapshot.ID == "" || filepath.Base(file) != "snapshot-"+snapshot.ID+".json" {
			return errors.New("invalid snapshot manifest")
		}
		if _, err := m.git(ctx, m.Root, nil, nil, "update-ref", "-d", "refs/polly/snapshots/"+snapshot.ID, snapshot.Commit); err != nil {
			return err
		}
		if err := os.Remove(file); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) checkSourcePath(source, name string, cfg sandbox.Config, active bool) error {
	path := filepath.Join(source, name)
	if !sandbox.PathWithin(path, source) {
		return errors.New("snapshot path escaped checkout")
	}
	if active {
		if err := sandbox.ReadAllowed(cfg, path); err != nil {
			return fmt.Errorf("snapshot includes a denied file: %w", err)
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} // tracked deletion
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("unsupported snapshot file: %s", name)
	}
	return nil
}
func (m *Manager) Create(ctx context.Context, s Snapshot) (Checkout, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.create(ctx, s)
}
func (m *Manager) create(ctx context.Context, s Snapshot) (Checkout, error) {
	c := Checkout{ID: ids.New(), Base: s}
	for _, slot := range m.Slots {
		owner, err := os.OpenFile(filepath.Join(slot, "owner"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return Checkout{}, err
		}
		_, err = owner.WriteString(c.ID)
		closeErr := owner.Close()
		if err != nil {
			return Checkout{}, err
		}
		if closeErr != nil {
			return Checkout{}, closeErr
		}
		c.Path = filepath.Join(slot, "tree")
		break
	}
	if c.Path == "" {
		return Checkout{}, errors.New("worktree capacity exhausted; explicitly clean integrated worktrees")
	}
	if _, err := m.git(ctx, m.Root, nil, nil, "worktree", "add", "--detach", c.Path, s.Commit); err != nil {
		m.releaseSlot(ctx, c.Path)
		return Checkout{}, err
	}
	if err := m.manifest(c.ID, c); err != nil {
		m.releaseSlot(ctx, c.Path)
		return Checkout{}, err
	}
	return c, nil
}
func (m *Manager) manifest(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	temp := filepath.Join(m.Directory, name+".json.tmp")
	if err = os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(m.Directory, name+".json"))
}

func (m *Manager) Preview(ctx context.Context, base, candidate Snapshot) (Preview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	parent, err := m.capture(ctx, m.Root)
	if err != nil {
		return Preview{}, err
	}
	p := Preview{ID: ids.New(), Parent: parent, Candidate: candidate}
	merged, mergeErr := m.git(ctx, m.Root, nil, nil, "merge-tree", "--write-tree", "--merge-base="+base.Commit, parent.Commit, candidate.Commit)
	lines := strings.SplitN(string(merged), "\n", 2)
	tree := strings.TrimSpace(lines[0])
	if len(tree) != 40 && len(tree) != 64 {
		return Preview{}, mergeErr
	}
	if mergeErr != nil {
		p.Conflicts = string(merged)
	}
	p.Merged, err = m.snapshotTree(ctx, tree, m.Root)
	if err != nil {
		return Preview{}, err
	}
	p.Checkout, err = m.create(ctx, p.Merged)
	if err != nil {
		return Preview{}, err
	}
	if err = m.manifest(p.ID, p); err != nil {
		return Preview{}, err
	}
	return p, nil
}

// Apply is the low-level single-preview convenience API. Coordinating hosts
// use the phased API to persist intent and detach their write context.
func (m *Manager) Apply(ctx context.Context, p Preview) error {
	if p.Conflicts != "" {
		return errors.New("resolve conflicts in an isolated checkout and publish a new candidate")
	}
	plan, err := m.BuildApplyPlan(ctx, p.ID, p.Parent, p.Merged, "tree")
	if err != nil {
		return err
	}
	if _, err = m.PreflightApply(ctx, plan); err != nil {
		return err
	}
	if err = m.RecordApply(p.ID, map[string]any{"plan": plan, "status": "applying"}); err != nil {
		return err
	}
	if err = m.WriteApply(ctx, plan); err != nil {
		return err
	}
	return m.RecordApply(p.ID, map[string]any{"plan": plan, "status": "applied"})
}

// Cleanup removes a runtime checkout only when its current tree still matches
// the parent's accepted cleanup evidence. An empty expectedTree means its base.
func (m *Manager) Cleanup(ctx context.Context, c Checkout, expectedTree string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, readErr := os.ReadFile(filepath.Join(filepath.Dir(c.Path), "owner"))
	if c.ID == "" || !sandbox.PathWithin(c.Path, m.Directory) || readErr != nil || string(owner) != c.ID {
		return errors.New("not a runtime-owned worktree")
	}
	current, err := m.capture(ctx, c.Path)
	if err != nil {
		return err
	}
	if expectedTree == "" {
		expectedTree = c.Base.Tree
	}
	if current.Tree != expectedTree {
		return errors.New("cleanup refuses unintegrated changes")
	}
	if _, err = m.git(ctx, m.Root, nil, nil, "worktree", "remove", "--force", c.Path); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(filepath.Dir(c.Path), "owner")); err != nil {
		return err
	}
	return os.Remove(filepath.Join(m.Directory, c.ID+".json"))
}
