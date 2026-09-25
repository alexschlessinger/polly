package worktree

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
		l.MaxUntrackedFileBytes = defaultMaxUntrackedFileBytes
	}
	if l.MaxUntrackedBytes <= 0 {
		l.MaxUntrackedBytes = defaultMaxUntrackedBytes
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
// private shadow index and writes it as a tree object; a change report diffs
// two such trees. Every Git command runs the trusted executable under the
// runtime sandbox posture, and every write lands under the tracker's own
// directory: the shadow index, and the objects through GIT_OBJECT_DIRECTORY
// with the repository's objects as a read-only alternate. The repository's
// index, refs and objects are never written.
//
// Each observer owns its shadow indexes for its lifetime, so their stat
// caches stay warm. A snapshot reconciles ignored paths, then asks git status
// whether anything changed since the last tree it wrote. One tracker serves
// every repository a session touches, the parent
// checkout and swarm member worktrees alike; repositories are set up on
// first use.
type ChangeTracker struct {
	registry     *tools.ToolRegistry
	directory    string
	indexes      string
	indexLease   io.Closer
	privatePaths []string
	limits       ChangeLimits
	git          string

	mu    sync.Mutex
	repos map[string]*trackedRepo // by resolved directory and by toplevel
}

// ChangeReasonNotRepository is the reason reported for a directory that no
// Git repository contains. Unlike other reasons it is not remembered: a
// repository initialized later is observed from then on.
const ChangeReasonNotRepository = "not a git repository"

// trackedRepo is one repository's tracking state. reason is set for the
// tracker's lifetime when the repository cannot be observed, except for
// ChangeReasonNotRepository, which the tracker rechecks on every request.
type trackedRepo struct {
	once    sync.Once
	lease   io.Closer
	top     string
	runner  *gitRunner
	private []string
	env     []string // object store routing and the shadow index
	reason  string
	err     error

	// snapMu serializes snapshots, which share the shadow index; lastTree
	// is the tree the shadow index was last written as, or "" when unknown,
	// and timeouts counts consecutive snapshot deadlines.
	snapMu   sync.Mutex
	lastTree string
	timeouts int
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
	t.indexes, err = os.MkdirTemp(directory, "indexes-")
	if err != nil {
		return nil, err
	}
	t.indexLease, err = lockChangeStore(filepath.Join(directory, ".lease-"+filepath.Base(t.indexes)), false)
	if err != nil {
		_ = os.RemoveAll(t.indexes)
		return nil, err
	}
	return t, nil
}

// Directory is where the tracker keeps its indexes and objects.
func (t *ChangeTracker) Directory() string { return t.directory }

// Close releases this observer's indexes and unshared snapshot stores. Objects are a
// disposable cache; durable baselines must be exported by their owner.
func (t *ChangeTracker) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	stores := make(map[string]bool)
	for _, repo := range t.repos {
		if repo.lease != nil {
			_ = repo.lease.Close()
			repo.lease = nil
			if len(repo.env) > 1 {
				stores[filepath.Dir(strings.TrimPrefix(repo.env[1], "GIT_OBJECT_DIRECTORY="))] = true
			}
		}
	}
	// Once no observer needs a store, its snapshots have no durable owners;
	// session baselines and rendered reports are persisted by the host.
	for store := range stores {
		lease, err := lockChangeStore(filepath.Join(t.directory, ".lease-"+filepath.Base(store)), true)
		if err == nil {
			_ = os.RemoveAll(store)
			_ = lease.Close()
		}
	}
	if t.indexLease != nil {
		_ = t.indexLease.Close()
		t.indexLease = nil
	}
	err := os.RemoveAll(t.indexes)
	_ = os.Remove(filepath.Join(t.directory, ".lease-"+filepath.Base(t.indexes)))
	return err
}

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
		if entry.IsDir() {
			lease, err := lockChangeStore(filepath.Join(t.directory, ".lease-"+entry.Name()), true)
			if err != nil {
				continue
			}
			_ = os.RemoveAll(path)
			_ = lease.Close()
		} else if strings.HasPrefix(entry.Name(), "index-") {
			_ = os.Remove(path)
		}
	}
}

// Snapshot implements tools.ChangeTracker.
func (t *ChangeTracker) Snapshot(ctx context.Context, dir string) (string, bool, string, error) {
	repo, err := t.repo(ctx, dir)
	if err != nil {
		return "", false, "", err
	}
	return t.snapshot(ctx, repo)
}

func (t *ChangeTracker) snapshot(ctx context.Context, repo *trackedRepo) (string, bool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.limits.SnapshotTimeout)
	defer cancel()
	repo.snapMu.Lock()
	defer repo.snapMu.Unlock()
	if repo.reason != "" {
		return "", false, repo.reason, nil
	}
	for _, path := range []string{t.indexes, filepath.Dir(strings.TrimPrefix(repo.env[1], "GIT_OBJECT_DIRECTORY="))} {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
	if err := t.dropIgnoredUntracked(ctx, repo); err != nil {
		return "", false, "", t.timedOut(ctx, repo, err)
	}
	changed, untracked, err := t.status(ctx, repo)
	if err != nil {
		return "", false, "", t.timedOut(ctx, repo, err)
	}
	if !changed && repo.lastTree != "" {
		repo.timeouts = 0
		return repo.lastTree, true, "", nil
	}
	if reason := t.untrackedWithinLimits(repo, untracked); reason != "" {
		return "", false, reason, nil
	}
	tree, err := repo.runner.addAndWriteTree(ctx, repo.top, repo.env, repo.private)
	if err != nil {
		return "", false, "", t.timedOut(ctx, repo, err)
	}
	repo.lastTree = tree
	repo.timeouts = 0
	return tree, true, "", nil
}

// A file added only to the shadow index must not become permanently tracked
// there when the user later ignores it. Real tracked files still ignore ignore
// rules, just as they do in the user's Git index.
func (t *ChangeTracker) dropIgnoredUntracked(ctx context.Context, repo *trackedRepo) error {
	ignored, err := repo.runner.git(ctx, repo.top, repo.env, nil, "ls-files", "--cached", "--ignored", "--exclude-standard", "-z")
	if err != nil || len(ignored) == 0 {
		return err
	}
	real, err := repo.runner.git(ctx, repo.top, nil, nil, "ls-files", "-z")
	if err != nil {
		return err
	}
	tracked := make(map[string]bool)
	for _, name := range bytes.Split(real, []byte{0}) {
		tracked[string(name)] = true
	}
	var remove []byte
	for _, name := range bytes.Split(ignored, []byte{0}) {
		if len(name) > 0 && !tracked[string(name)] {
			remove = append(append(remove, name...), 0)
		}
	}
	if len(remove) == 0 {
		return nil
	}
	_, err = repo.runner.git(ctx, repo.top, repo.env, remove, "update-index", "--force-remove", "-z", "--stdin")
	repo.lastTree = ""
	return err
}

// status asks git whether the working tree differs from the shadow index,
// and lists the untracked files it would stage. Private paths, which the
// shadow index never holds, are ignored.
func (t *ChangeTracker) status(ctx context.Context, repo *trackedRepo) (changed bool, untracked []string, err error) {
	out, err := repo.runner.git(ctx, repo.top, repo.env, nil, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return false, nil, err
	}
	fields := bytes.Split(out, []byte{0})
	for i := 0; i < len(fields); i++ {
		record := string(fields[i])
		if record == "" {
			continue
		}
		switch record[0] {
		case '?':
			path := strings.TrimPrefix(record, "? ")
			if privatePath(repo.private, path) {
				continue
			}
			changed = true
			untracked = append(untracked, path)
		case '1', '2', 'u':
			// "1 XY ..." and "2 XY ..." carry the index and working tree
			// states in X and Y; only Y matters here. A rename record has
			// a second path field.
			if record[0] == '2' {
				i++
			}
			if len(record) > 3 && record[3] != '.' {
				changed = true
			} else if record[0] == 'u' {
				changed = true
			}
		}
	}
	return changed, untracked, nil
}

// timedOut records a snapshot deadline and disables the repository after two
// in a row; other errors pass through. The caller holds snapMu.
func (t *ChangeTracker) timedOut(ctx context.Context, repo *trackedRepo, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	repo.timeouts++
	if repo.timeouts >= 2 {
		repo.reason = "disabled after repeated snapshot timeouts"
	}
	return errors.New("snapshot timed out")
}

// untrackedWithinLimits reports a reason when the untracked files, which a
// snapshot would hash and store, exceed the limits.
func (t *ChangeTracker) untrackedWithinLimits(repo *trackedRepo, untracked []string) string {
	var total int64
	for _, name := range untracked {
		info, err := os.Lstat(filepath.Join(repo.top, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		total += info.Size()
		if info.Size() > t.limits.MaxUntrackedFileBytes || total > t.limits.MaxUntrackedBytes {
			return "untracked files too large to snapshot"
		}
	}
	return ""
}

// Changes implements tools.ChangeTracker.
func (t *ChangeTracker) Changes(ctx context.Context, dir, before string) (tools.FileChanges, error) {
	return t.changes(ctx, dir, before, false)
}

// WorkspaceChanges compares a session baseline and includes all currently
// untracked non-ignored files as additions, even if they were tracked at baseline.
func (t *ChangeTracker) WorkspaceChanges(ctx context.Context, dir, before string) (tools.FileChanges, error) {
	return t.changes(ctx, dir, before, true)
}

func (t *ChangeTracker) changes(ctx context.Context, dir, before string, includeUntracked bool) (tools.FileChanges, error) {
	repo, err := t.repo(ctx, dir)
	if err != nil {
		return tools.FileChanges{}, err
	}
	after, ok, reason, err := t.snapshot(ctx, repo)
	if err != nil {
		return tools.FileChanges{}, err
	}
	if !ok {
		return tools.FileChanges{Reason: reason}, nil
	}
	result := tools.FileChanges{Root: repo.top, Tracked: true, Changes: []tools.FileChange{}}
	if before == after && !includeUntracked {
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.limits.SnapshotTimeout)
	defer cancel()
	entries, err := t.diffTrees(ctx, repo, before, after)
	if err != nil {
		return tools.FileChanges{}, err
	}
	if includeUntracked {
		entries, err = t.includeUntracked(ctx, repo, after, entries)
		if err != nil {
			return tools.FileChanges{}, err
		}
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
		change := tools.DiffFileChange(entry.path, string(old.data), string(new.data), entry.oldID != "", entry.newID != "")
		if old.large || new.large {
			change.Diff, change.Truncated, change.CountsUnknown = "", true, true
			change.Additions, change.Deletions = 0, 0
		}
		if len(change.Diff) > budget {
			change.Diff, change.Truncated = "", true
		}
		budget -= len(change.Diff)
		result.Changes = append(result.Changes, change)
	}
	return result, nil
}

func (t *ChangeTracker) includeUntracked(ctx context.Context, repo *trackedRepo, tree string, entries []treeEntry) ([]treeEntry, error) {
	paths, err := repo.runner.git(ctx, repo.top, nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || len(paths) == 0 {
		return entries, err
	}
	wanted := make(map[string]bool)
	for _, name := range bytes.Split(paths, []byte{0}) {
		if len(name) > 0 && !privatePath(repo.private, string(name)) {
			wanted[string(name)] = true
		}
	}
	if len(wanted) == 0 {
		return entries, nil
	}
	listing, err := repo.runner.git(ctx, repo.top, repo.env, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]treeEntry, len(entries))
	for _, entry := range entries {
		byPath[entry.path] = entry
	}
	for _, line := range bytes.Split(listing, []byte{0}) {
		meta, name, ok := bytes.Cut(line, []byte{'\t'})
		if !ok || !wanted[string(name)] {
			continue
		}
		fields := strings.Fields(string(meta))
		if len(fields) == 3 && fields[1] == "blob" {
			byPath[string(name)] = treeEntry{path: string(name), newID: fields[2]}
		}
	}
	entries = entries[:0]
	for _, entry := range byPath {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b treeEntry) int { return strings.Compare(a.path, b.path) })
	return entries, nil
}

type treeEntry struct {
	path, oldID, newID string
}

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
	slices.SortFunc(entries, func(a, b treeEntry) int { return strings.Compare(a.path, b.path) })
	return entries, nil
}

type blob struct {
	data  []byte
	large bool
}

// readBlobs fetches the content of every blob the entries name in one
// cat-file run, marking the ones over tools.ChangeMaxFileBytes instead of
// keeping them.
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
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := repo.runner.runTo(ctx, repo.top, repo.env, want, true, writer, "cat-file", "--batch")
		_ = writer.CloseWithError(err)
		done <- err
	}()
	defer func() { _ = reader.Close(); <-done }()
	input := bufio.NewReader(reader)
	remaining := int64(16 << 20) // bound retained input across all files
	for {
		header, err := input.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		parts := strings.Fields(header)
		if len(parts) != 3 || parts[1] != "blob" {
			return nil, fmt.Errorf("invalid blob response: %q", header)
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid blob size: %q", header)
		}
		if size > tools.ChangeMaxFileBytes || size > remaining {
			blobs[parts[0]] = blob{large: true}
			if _, err := io.CopyN(io.Discard, input, size); err != nil {
				return nil, err
			}
		} else {
			remaining -= size
			data := make([]byte, int(size))
			if _, err := io.ReadFull(input, data); err != nil {
				return nil, err
			}
			blobs[parts[0]] = blob{data: data}
		}
		if ch, err := input.ReadByte(); err != nil || ch != '\n' {
			return nil, fmt.Errorf("invalid blob terminator")
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
	for path := dir; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			dir = path
			break
		}
		if filepath.Dir(path) == path {
			break
		}
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
	if repo.reason == ChangeReasonNotRepository {
		// Forget the miss so a repository initialized here later is found.
		t.mu.Lock()
		if t.repos[dir] == repo {
			delete(t.repos, dir)
		}
		t.mu.Unlock()
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
			return ChangeReasonNotRepository, nil
		}
	}
	if sandbox.PathWithin(t.directory, root) {
		return "runtime directory inside the repository", nil
	}
	cfg, _, err := t.registry.BaseSandboxPolicy()
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
	if err := runner.useSandbox(t.registry, cfg); err != nil {
		return "", err
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
		return ChangeReasonNotRepository, nil
	}
	repo.top = strings.TrimSpace(string(top))
	index, err := runner.git(ctx, root, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	realIndex := strings.TrimSpace(string(index))
	common, err := runner.commonDir(ctx, repo.top)
	if errors.Is(err, errSymlinkedMetadata) {
		return errSymlinkedMetadata.Error(), nil
	}
	if err != nil {
		return "", err
	}
	if key := runner.unsupportedSetting(ctx, repo.top); key != "" {
		return "unsupported repository setting " + key, nil
	}
	extraReads := runner.pinUserConfig(ctx, repo.top)
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
	// The shadow index is keyed by the checkout, so linked worktrees of one
	// repository each get their own; their objects share one store keyed
	// by the common directory.
	sum := sha256.Sum256([]byte(common))
	store := filepath.Join(t.directory, hex.EncodeToString(sum[:16]))
	repo.lease, err = lockChangeStore(filepath.Join(t.directory, ".lease-"+filepath.Base(store)), false)
	if err != nil {
		return "", err
	}
	objects := filepath.Join(store, "objects")
	if err := os.MkdirAll(objects, 0700); err != nil {
		return "", err
	}
	now := time.Now()
	_ = os.Chtimes(store, now, now)
	sum = sha256.Sum256([]byte(repo.top))
	shadow := filepath.Join(t.indexes, "index-"+hex.EncodeToString(sum[:8]))
	repo.env = []string{
		"GIT_INDEX_FILE=" + shadow,
		"GIT_OBJECT_DIRECTORY=" + objects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(common, "objects"),
	}
	if len(extraReads) > 0 {
		cfg.ReadPaths = append(cfg.ReadPaths, extraReads...)
		if err := runner.useSandbox(t.registry, cfg); err != nil {
			return "", err
		}
	}
	// Seed this observer's index from the real index, then clear private
	// entries and flags that could hide working files from add.
	if _, err := os.Lstat(shadow); errors.Is(err, os.ErrNotExist) {
		if err := seedIndex(realIndex, shadow); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := runner.cleanIndex(ctx, repo.top, repo.env, repo.private); err != nil {
		return "", err
	}
	repo.runner = runner
	return "", nil
}
