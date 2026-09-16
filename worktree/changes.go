package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// ChangeLimits bounds what the change tracker will observe. Zero values take
// the defaults.
type ChangeLimits struct {
	// MaxIndexEntries disables tracking for a repository whose index is
	// larger; snapshots would take too long. Default 100000.
	MaxIndexEntries int
	// MaxUntrackedFileBytes and MaxUntrackedBytes skip a snapshot when
	// untracked files are larger than this, alone or in total, so a build
	// artifact is never hashed. Defaults 32 MiB and 256 MiB.
	MaxUntrackedFileBytes, MaxUntrackedBytes int64
	// SnapshotTimeout bounds each snapshot; two consecutive timeouts disable
	// the repository for the tracker's lifetime. Default 5s.
	SnapshotTimeout time.Duration
}

func (l ChangeLimits) withDefaults() ChangeLimits {
	if l.MaxIndexEntries <= 0 {
		l.MaxIndexEntries = 100_000
	}
	if l.MaxUntrackedFileBytes <= 0 {
		l.MaxUntrackedFileBytes = 32 << 20
	}
	if l.MaxUntrackedBytes <= 0 {
		l.MaxUntrackedBytes = 256 << 20
	}
	if l.SnapshotTimeout <= 0 {
		l.SnapshotTimeout = 5 * time.Second
	}
	return l
}

// changeObjectsRetention is how long a repository's snapshot objects are kept
// after their last use.
const changeObjectsRetention = 7 * 24 * time.Hour

// ChangeTracker implements tools.ChangeTracker over Git trees: a snapshot
// stages the working tree of the repository containing a directory into a
// private index and writes it as a tree object; a change report diffs two
// such trees. Every Git command runs the trusted executable under the
// runtime sandbox posture, and every write lands under the tracker's own
// directory: the private index, and the objects through GIT_OBJECT_DIRECTORY
// with the repository's objects as a read-only alternate. The repository's
// index, refs and objects are never written.
//
// One tracker serves every repository a session touches, the parent checkout
// and swarm member worktrees alike; repositories are set up on first use.
type ChangeTracker struct {
	registry     *tools.ToolRegistry
	directory    string
	privatePaths []string
	limits       ChangeLimits
	git          string

	mu    sync.Mutex
	repos map[string]*trackedRepo // by resolved directory and by toplevel
}

// trackedRepo is one repository's tracking state. reason is set for the
// tracker's lifetime when the repository cannot be observed.
type trackedRepo struct {
	once    sync.Once
	top     string
	common  string
	index   string
	objects string
	runner  *gitRunner
	private []string
	env     []string
	reason  string
	err     error

	timeoutMu sync.Mutex
	timeouts  int
}

// NewChangeTracker prepares a tracker whose private indexes and objects live
// under directory. privatePaths are runtime-owned files excluded from every
// snapshot, as for Config.PrivatePaths. It fails when the registry has
// neither a process sandbox nor the explicit unsafe acknowledgement, since
// the tracker runs Git against the user's repository.
func NewChangeTracker(registry *tools.ToolRegistry, directory string, privatePaths []string, limits ChangeLimits) (*ChangeTracker, error) {
	if registry == nil {
		return nil, errors.New("change tracker registry is required")
	}
	if !registry.HasSandbox() && !registry.UnsafeNoSandbox() {
		return nil, errors.New("change tracking requires a process sandbox or explicit unsafe acknowledgement")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	git, err := sandbox.TrustedGitExecutable([]string{directory})
	if err != nil {
		return nil, err
	}
	t := &ChangeTracker{
		registry:     registry,
		directory:    directory,
		privatePaths: append([]string(nil), privatePaths...),
		limits:       limits.withDefaults(),
		git:          git,
		repos:        make(map[string]*trackedRepo),
	}
	t.prune()
	return t, nil
}

// Directory is where the tracker keeps its indexes and objects.
func (t *ChangeTracker) Directory() string { return t.directory }

// prune removes repository object stores unused for changeObjectsRetention
// and stale temporary indexes.
func (t *ChangeTracker) prune() {
	entries, err := os.ReadDir(t.directory)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-changeObjectsRetention)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(t.directory, entry.Name())
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "index-") {
			_ = os.RemoveAll(path)
		}
	}
}

// Snapshot implements tools.ChangeTracker.
func (t *ChangeTracker) Snapshot(ctx context.Context, dir string) (string, bool, string, error) {
	repo, err := t.repo(ctx, dir)
	if err != nil {
		return "", false, "", err
	}
	if repo.reason != "" {
		return "", false, repo.reason, nil
	}
	return t.snapshot(ctx, repo)
}

func (t *ChangeTracker) snapshot(ctx context.Context, repo *trackedRepo) (string, bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.limits.SnapshotTimeout)
	defer cancel()
	if reason, err := t.untrackedWithinLimits(ctx, repo); err != nil {
		return "", false, "", t.timedOut(ctx, repo, err)
	} else if reason != "" {
		return "", false, reason, nil
	}
	index := filepath.Join(t.directory, "index-"+ids.New())
	defer os.Remove(index)
	if err := seedIndex(repo.index, index); err != nil {
		return "", false, "", err
	}
	tree, err := repo.runner.stageTree(ctx, repo.top, index, repo.private, repo.env)
	if err != nil {
		return "", false, "", t.timedOut(ctx, repo, err)
	}
	repo.timeoutMu.Lock()
	repo.timeouts = 0
	repo.timeoutMu.Unlock()
	return tree, true, "", nil
}

// timedOut records a snapshot deadline and disables the repository after two
// in a row; other errors pass through.
func (t *ChangeTracker) timedOut(ctx context.Context, repo *trackedRepo, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	repo.timeoutMu.Lock()
	defer repo.timeoutMu.Unlock()
	repo.timeouts++
	if repo.timeouts >= 2 {
		repo.reason = "disabled after repeated snapshot timeouts"
	}
	return errors.New("snapshot timed out")
}

// untrackedWithinLimits reports a reason when the untracked files, which a
// snapshot would hash, exceed the limits.
func (t *ChangeTracker) untrackedWithinLimits(ctx context.Context, repo *trackedRepo) (string, error) {
	out, err := repo.runner.git(ctx, repo.top, repo.env, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	var total int64
	for _, name := range bytes.Split(out, []byte{0}) {
		if len(name) == 0 || privatePath(repo.private, string(name)) {
			continue
		}
		info, err := os.Lstat(filepath.Join(repo.top, filepath.FromSlash(string(name))))
		if err != nil {
			continue
		}
		total += info.Size()
		if info.Size() > t.limits.MaxUntrackedFileBytes || total > t.limits.MaxUntrackedBytes {
			return "untracked files too large to snapshot", nil
		}
	}
	return "", nil
}

func privatePath(private []string, name string) bool {
	for _, path := range private {
		if name == path || strings.HasPrefix(name, path+"/") {
			return true
		}
	}
	return false
}

// Changes implements tools.ChangeTracker.
func (t *ChangeTracker) Changes(ctx context.Context, dir, before string) (tools.FileChanges, error) {
	repo, err := t.repo(ctx, dir)
	if err != nil {
		return tools.FileChanges{}, err
	}
	if repo.reason != "" {
		return tools.FileChanges{Reason: repo.reason}, nil
	}
	after, ok, reason, err := t.snapshot(ctx, repo)
	if err != nil {
		return tools.FileChanges{}, err
	}
	if !ok {
		return tools.FileChanges{Reason: reason}, nil
	}
	result := tools.FileChanges{Root: repo.top, Tracked: true, Changes: []tools.FileChange{}}
	if before == after {
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.limits.SnapshotTimeout)
	defer cancel()
	entries, err := t.diffTrees(ctx, repo, before, after)
	if err != nil {
		return tools.FileChanges{}, err
	}
	if len(entries) > tools.ChangeMaxFiles {
		result.Truncated = true
		result.Omitted = len(entries) - tools.ChangeMaxFiles
		entries = entries[:tools.ChangeMaxFiles]
	}
	blobs, err := t.readBlobs(ctx, repo, entries)
	if err != nil {
		return tools.FileChanges{}, err
	}
	budget := tools.ChangeMaxTotalBytes
	for _, entry := range entries {
		old, new := blobs[entry.oldID], blobs[entry.newID]
		change := tools.DiffFileChange(entry.path, old.data, new.data, entry.oldID != "", entry.newID != "")
		if old.large || new.large {
			change.Diff, change.Truncated = "", true
		}
		if len(change.Diff) > budget {
			change.Diff, change.Truncated = "", true
		}
		budget -= len(change.Diff)
		result.Changes = append(result.Changes, change)
	}
	return result, nil
}

type treeEntry struct {
	path, oldID, newID string
}

const emptyObjectID = "0000000000000000000000000000000000000000"

// diffTrees lists the files that differ between two trees, sorted by path.
// Renames are reported as a deletion and a creation; submodules are skipped.
func (t *ChangeTracker) diffTrees(ctx context.Context, repo *trackedRepo, before, after string) ([]treeEntry, error) {
	out, err := repo.runner.git(ctx, repo.top, repo.env, nil, "diff-tree", "-r", "-z", "--no-renames", "--raw", before, after)
	if err != nil {
		return nil, err
	}
	fields := bytes.Split(out, []byte{0})
	var entries []treeEntry
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(string(fields[i]), ":"))
		if len(meta) < 5 || meta[0] == "160000" || meta[1] == "160000" {
			continue
		}
		entry := treeEntry{path: string(fields[i+1]), oldID: meta[2], newID: meta[3]}
		if strings.Trim(entry.oldID, "0") == "" {
			entry.oldID = ""
		}
		if strings.Trim(entry.newID, "0") == "" {
			entry.newID = ""
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

type blob struct {
	data  []byte
	large bool
}

// readBlobs fetches the content of every blob the entries name, marking the
// ones over tools.ChangeMaxFileBytes instead of reading them.
func (t *ChangeTracker) readBlobs(ctx context.Context, repo *trackedRepo, entries []treeEntry) (map[string]blob, error) {
	blobs := make(map[string]blob)
	var want []byte
	for _, entry := range entries {
		for _, id := range []string{entry.oldID, entry.newID} {
			if id == "" {
				continue
			}
			if _, seen := blobs[id]; !seen {
				blobs[id] = blob{}
				want = append(append(want, id...), '\n')
			}
		}
	}
	if len(want) == 0 {
		return blobs, nil
	}
	sizes, err := repo.runner.git(ctx, repo.top, repo.env, want, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	var fetch []byte
	for _, line := range strings.Split(strings.TrimSpace(string(sizes)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 || parts[1] != "blob" {
			continue
		}
		size, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		if size > tools.ChangeMaxFileBytes {
			blobs[parts[0]] = blob{large: true}
			continue
		}
		fetch = append(append(fetch, parts[0]...), '\n')
	}
	if len(fetch) == 0 {
		return blobs, nil
	}
	out, err := repo.runner.git(ctx, repo.top, repo.env, fetch, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for len(out) > 0 {
		header, rest, ok := bytes.Cut(out, []byte{'\n'})
		if !ok {
			break
		}
		parts := strings.Fields(string(header))
		if len(parts) != 3 {
			break
		}
		size, err := strconv.Atoi(parts[2])
		if err != nil || size > len(rest) {
			break
		}
		blobs[parts[0]] = blob{data: rest[:size:size]}
		out = rest[size:]
		if len(out) > 0 && out[0] == '\n' {
			out = out[1:]
		}
	}
	return blobs, nil
}

// repo returns the tracking state for the repository containing dir,
// building it on first use. The state records a reason instead of failing
// when the repository cannot be observed.
func (t *ChangeTracker) repo(ctx context.Context, dir string) (*trackedRepo, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return &trackedRepo{reason: "working directory unavailable"}, nil
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	} else {
		return &trackedRepo{reason: "working directory unavailable"}, nil
	}
	t.mu.Lock()
	repo, ok := t.repos[dir]
	if !ok {
		repo = &trackedRepo{}
		t.repos[dir] = repo
	}
	t.mu.Unlock()
	repo.once.Do(func() { repo.reason, repo.err = t.setup(ctx, dir, repo) })
	if repo.err != nil {
		return nil, repo.err
	}
	if repo.top != "" && repo.top != dir {
		// Later requests from the toplevel or another subdirectory share
		// this state.
		t.mu.Lock()
		if _, ok := t.repos[repo.top]; !ok {
			t.repos[repo.top] = repo
		}
		t.mu.Unlock()
	}
	return repo, nil
}

// setup resolves the repository containing dir and prepares its runner. A
// returned reason disables tracking for dir; an error is unexpected.
func (t *ChangeTracker) setup(ctx context.Context, dir string, repo *trackedRepo) (string, error) {
	root := ""
	for path := dir; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			root = path
			break
		}
		if filepath.Dir(path) == path {
			return "not a git repository", nil
		}
	}
	if sandbox.PathWithin(t.directory, root) {
		return "runtime directory inside the repository", nil
	}
	cfg, _, err := t.registry.SandboxReadPolicy()
	if err != nil {
		return "", err
	}
	cfg.ReadPaths = append(cfg.ReadPaths, sandbox.GitUserConfigPaths()...)
	cfg.WritablePaths = []string{t.directory}
	cfg.AllowNetwork = false
	cfg.AllowUnixSockets = nil
	cfg, err = sandbox.RuntimeGitReadConfig(cfg, root)
	if err != nil {
		return "repository metadata is not readable: " + err.Error(), nil
	}
	runner := &gitRunner{Git: t.git}
	if t.registry.HasSandbox() {
		if runner.sandbox, err = t.registry.NewSandboxDirect(cfg); err != nil {
			return "", err
		}
	}
	version, err := runner.git(ctx, root, nil, nil, "--version")
	if err != nil {
		return "", err
	}
	if err := validateGitVersion(string(version)); err != nil {
		return err.Error(), nil
	}
	top, err := runner.git(ctx, root, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "not a git repository", nil
	}
	repo.top = strings.TrimSpace(string(top))
	common, err := runner.git(ctx, root, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	repo.common = strings.TrimSpace(string(common))
	index, err := runner.git(ctx, root, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	repo.index = strings.TrimSpace(string(index))
	for _, path := range []string{filepath.Join(repo.top, ".git"), repo.common} {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		if real != path {
			return "symlinked Git metadata is unsupported", nil
		}
	}
	for _, key := range []string{"core.sparseCheckout", "core.splitIndex"} {
		if out, _ := runner.git(ctx, repo.top, nil, nil, "config", "--bool", key); strings.TrimSpace(string(out)) == "true" {
			return "unsupported repository setting " + key, nil
		}
	}
	// Global ignore and attribute files decide what a snapshot stages. Read
	// them as the user's own git would, then pin them for isolated commands.
	var extraReads []string
	for _, key := range []string{"core.excludesFile", "core.attributesFile"} {
		value, _ := runner.gitUser(ctx, repo.top, "config", "--get", "--type=path", key)
		if path := strings.TrimSpace(string(value)); path != "" {
			runner.userConfig = append(runner.userConfig, "-c", key+"="+path)
			if real, err := filepath.EvalSymlinks(path); err == nil {
				extraReads = append(extraReads, real)
			}
		}
	}
	entries, err := runner.git(ctx, repo.top, nil, nil, "ls-files", "-z")
	if err != nil {
		return "", err
	}
	if bytes.Count(entries, []byte{0}) > t.limits.MaxIndexEntries {
		return "repository too large to snapshot", nil
	}
	repo.private, err = snapshotPrivatePaths(repo.top, t.privatePaths)
	if err != nil {
		return err.Error(), nil
	}
	// Snapshot objects live under the tracker's directory, keyed by the
	// repository's common directory; the repository's own objects are
	// read through the alternate.
	sum := sha256.Sum256([]byte(repo.common))
	store := filepath.Join(t.directory, hex.EncodeToString(sum[:16]))
	repo.objects = filepath.Join(store, "objects")
	if err := os.MkdirAll(repo.objects, 0700); err != nil {
		return "", err
	}
	now := time.Now()
	_ = os.Chtimes(store, now, now)
	repo.env = []string{"GIT_OBJECT_DIRECTORY=" + repo.objects, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(repo.common, "objects")}
	if len(extraReads) > 0 && runner.sandbox != nil {
		cfg.ReadPaths = append(cfg.ReadPaths, extraReads...)
		if runner.sandbox, err = t.registry.NewSandboxDirect(cfg); err != nil {
			return "", err
		}
	}
	repo.runner = runner
	return "", nil
}

// String describes the tracker for logs.
func (t *ChangeTracker) String() string {
	return fmt.Sprintf("change tracker at %s", t.directory)
}
