package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// RetainCommit admits an existing commit as captured contents, preserving its
// identity and ancestry. Unlike Capture, it never reads dirty file contents or
// filters private paths out of the tree: a prohibited tree must be rejected.
func (m *Manager) RetainCommit(ctx context.Context, source, commit string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validObjectID(commit) {
		return Snapshot{}, errors.New("commit must be a full Git commit object ID")
	}
	commit = strings.ToLower(commit)
	source, err := m.captureSource(ctx, source)
	if err != nil {
		return Snapshot{}, err
	}
	kind, err := m.git(ctx, source, nil, nil, "cat-file", "-t", commit)
	if err != nil {
		return Snapshot{}, err
	}
	if strings.TrimSpace(string(kind)) != "commit" {
		return Snapshot{}, errors.New("commit must name a Git commit object")
	}
	tree, err := m.git(ctx, source, nil, nil, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{ID: ids.New(), Commit: commit, Tree: strings.TrimSpace(string(tree)), Source: source}
	policy, active, err := m.Registry.SandboxReadPolicy()
	if err != nil {
		return Snapshot{}, err
	}
	// Historical paths can be absent today. Retain denied routes through
	// existing parent symlinks (for example /var -> /private/var on macOS)
	// without requiring the final file to exist.
	denied := append([]string(nil), policy.DenyPaths...)
	for _, path := range policy.DenyPaths {
		if resolved, err := sandbox.ResolveExistingPathPrefix(path); err == nil {
			denied = append(denied, resolved)
		}
	}
	policy.DenyPaths = denied
	entries, err := m.git(ctx, source, nil, nil, "ls-tree", "-r", "-z", s.Tree)
	if err != nil {
		return Snapshot{}, err
	}
	var names []byte
	for _, entry := range bytes.Split(entries, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, name, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			return Snapshot{}, errors.New("submodules and non-blob commit entries are unsupported")
		}
		if m.privateSourcePath(name) {
			return Snapshot{}, fmt.Errorf("commit includes private runtime path: %s", name)
		}
		path := filepath.Join(source, name)
		if !sandbox.PathWithin(path, source) {
			return Snapshot{}, errors.New("commit path escaped checkout")
		}
		if active {
			if err := sandbox.ReadAllowed(policy, path); err != nil {
				return Snapshot{}, fmt.Errorf("commit includes a denied file: %w", err)
			}
		}
		// Historical symlinks need their recorded target checked, even when
		// the current checkout has deleted or replaced the link.
		if fields[0] == "120000" && active {
			target, err := m.git(ctx, source, nil, nil, "cat-file", "blob", fields[2])
			if err != nil {
				return Snapshot{}, err
			}
			path := string(target)
			if !filepath.IsAbs(path) {
				path = filepath.Join(source, filepath.Dir(name), path)
			}
			if err := sandbox.ReadAllowed(policy, path); err != nil {
				return Snapshot{}, fmt.Errorf("commit includes a denied symlink: %w", err)
			}
		}
		names = append(append(names, name...), 0)
	}
	// A private temporary index selects attributes from this commit, not
	// the current files. This neither modifies the real index nor runs filters.
	index := filepath.Join(m.Directory, "commit-index-"+ids.New())
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := m.git(ctx, source, env, nil, "read-tree", s.Tree); err != nil {
		return Snapshot{}, err
	}
	if err := m.checkFilters(ctx, source, env, names, true); err != nil {
		return Snapshot{}, err
	}
	return m.retainSnapshot(ctx, s)
}
