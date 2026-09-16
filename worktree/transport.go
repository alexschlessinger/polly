package worktree

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// The operations in this file move a checkout's content between the host
// worktree and a copy kept elsewhere (a container's, say). The host worktree
// is always the durable truth; the copy is rebuilt from it at any time. The
// manager keeps the policy: private paths, untracked caps, content filters,
// and the admission of what comes back.

// fixedDate makes a base commit of the same tree the same commit each time,
// so a copy already holding it needs no transfer.
var fixedDate = []string{"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}

// runStream is run with stdout streamed to w instead of buffered.
func (m *Manager) runStream(ctx context.Context, cwd string, env []string, input []byte, isolate bool, w io.Writer, args ...string) error {
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
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_AUTHOR_NAME=Polly runtime", "GIT_AUTHOR_EMAIL=polly@localhost", "GIT_COMMITTER_NAME=Polly runtime", "GIT_COMMITTER_EMAIL=polly@localhost")
	if isolate {
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = bytes.NewReader(input)
	cleanup, err := sandbox.WrapCmdManaged(m.sandbox, cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Base is the commit a copy of root starts from: HEAD when it is already a
// parentless snapshot commit (a member checkout), otherwise a parentless
// commit of HEAD's tree, so the copy never receives history. The tree is
// what a divergence is measured against.
func (m *Manager) Base(ctx context.Context, root string) (commit, tree string, err error) {
	source, err := m.captureSource(ctx, root)
	if err != nil {
		return "", "", err
	}
	parents, err := m.git(ctx, source, nil, nil, "rev-list", "--parents", "-n1", "HEAD")
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(string(parents))
	if len(fields) == 0 {
		return "", "", errors.New("checkout has no HEAD commit")
	}
	treeOut, err := m.git(ctx, source, nil, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", "", err
	}
	tree = strings.TrimSpace(string(treeOut))
	if len(fields) == 1 {
		return fields[0], tree, nil
	}
	base, err := m.git(ctx, source, fixedDate, []byte("polly copy base\n"), "commit-tree", tree)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(base)), tree, nil
}

// WriteBundle streams a bundle holding commit and everything it reaches,
// under the reference protocol.BundleRef names. A bundle carries references,
// not bare commits, so one is retained for the duration.
func (m *Manager) WriteBundle(ctx context.Context, root, commit string, w io.Writer) error {
	source, err := m.captureSource(ctx, root)
	if err != nil {
		return err
	}
	ref := protocol.BundleRef(commit)
	if _, err := m.git(ctx, source, nil, nil, "update-ref", ref, commit); err != nil {
		return err
	}
	defer m.git(context.WithoutCancel(ctx), source, nil, nil, "update-ref", "-d", ref)
	return m.runStream(ctx, source, nil, nil, true, w, "bundle", "create", "-", ref)
}

// Divergence lists what root's working tree holds beyond tree: the paths
// whose content differs or is new, and the tracked paths that are gone,
// under the capture rules. The live index is never touched.
func (m *Manager) Divergence(ctx context.Context, root, tree string) (changed, deleted []string, err error) {
	source, err := m.captureSource(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	st, err := m.stageIndex(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	defer st.cleanup()
	out, err := m.git(ctx, source, st.env, nil, "diff-index", "--cached", "--name-status", "-z", "--no-renames", tree)
	if err != nil {
		return nil, nil, err
	}
	fields := bytes.Split(out, []byte{0})
	for i := 0; i+1 < len(fields); i += 2 {
		status, name := string(fields[i]), string(fields[i+1])
		if status == "" {
			continue
		}
		switch status[0] {
		case 'D':
			deleted = append(deleted, name)
		default:
			changed = append(changed, name)
		}
	}
	return changed, deleted, nil
}

// importable reports whether root is a tree this manager owns: its source
// root or a claimed checkout slot.
func (m *Manager) importable(root string) error {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if canonical == m.Root {
		return nil
	}
	if sandbox.PathWithin(canonical, m.Directory) && filepath.Base(canonical) == "tree" {
		if _, err := os.Stat(filepath.Join(filepath.Dir(canonical), "owner")); err == nil {
			return nil
		}
	}
	return errors.New("not a runtime-owned worktree")
}

// ImportChanges applies a copy's changes to root: the tar's regular files,
// directories and symlinks, then the deletions. It is the boundary where a
// copy's output becomes host files, so every name must be a clean relative
// path inside root that crosses no symlink and names no private path; only
// those entry types are accepted, and the untracked size caps bound the
// import. Files are written beside their target and renamed into place.
func (m *Manager) ImportChanges(ctx context.Context, root string, changes io.Reader, deleted []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.importable(root); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	var total int64
	reader := tar.NewReader(changes)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read changes: %w", err)
		}
		rel, err := m.importPath(canonical, header.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		target := filepath.Join(canonical, rel)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size > m.MaxUntrackedFileBytes {
				return fmt.Errorf("imported file %s exceeds the size limit", rel)
			}
			total += header.Size
			if total > m.MaxUntrackedBytes {
				return errors.New("imported changes exceed the total size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeImported(target, reader, header); err != nil {
				return err
			}
		default:
			return fmt.Errorf("imported entry %s has an unsupported type", rel)
		}
	}
	for _, name := range deleted {
		rel, err := m.importPath(canonical, name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		target := filepath.Join(canonical, rel)
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		pruneEmptyParents(canonical, filepath.Dir(target))
	}
	return nil
}

// importPath admits one tar name: clean, relative, inside root, not a
// private path, and with no symlink among its existing ancestors.
func (m *Manager) importPath(root, name string) (string, error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	if name == "" || name == "." {
		return "", nil
	}
	if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") || strings.HasSuffix(name, "/..") {
		return "", fmt.Errorf("imported path %q escapes the worktree", name)
	}
	rel := filepath.Clean(filepath.FromSlash(name))
	if rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("imported path %q escapes the worktree", name)
	}
	if m.privateSourcePath(filepath.ToSlash(rel)) {
		return "", fmt.Errorf("imported path %q is private", name)
	}
	if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return "", fmt.Errorf("imported path %q is Git metadata", name)
	}
	dir := filepath.Dir(rel)
	for dir != "." {
		info, err := os.Lstat(filepath.Join(root, dir))
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("imported path %q crosses a symlink", name)
		}
		dir = filepath.Dir(dir)
	}
	return rel, nil
}

func writeImported(target string, content io.Reader, header *tar.Header) error {
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".polly-import-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := io.CopyN(temp, content, header.Size); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(os.FileMode(header.Mode) & 0o777); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}

// pruneEmptyParents removes directories emptied by a deletion, up to root.
func pruneEmptyParents(root, dir string) {
	for dir != root && sandbox.PathWithin(dir, root) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
