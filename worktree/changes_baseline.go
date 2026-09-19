package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChangeBaseline is a session's initial tracked working tree. Pack is a
// self-contained Git pack, including objects borrowed from the source repo.
// The host owns its lifetime (normally as a session artifact), so cache
// cleanup and source-repository GC cannot erase the baseline.
type ChangeBaseline struct {
	Root string
	Tree string
	Pack []byte
}

const ChangeMaxBaselineBytes = 64 << 20

type limitedPack struct{ bytes.Buffer }

func (b *limitedPack) Write(p []byte) (int, error) {
	if b.Len()+len(p) > ChangeMaxBaselineBytes {
		return 0, errors.New("workspace baseline exceeds 64 MiB")
	}
	return b.Buffer.Write(p)
}

// CaptureBaseline captures tracked working files, retaining pre-existing
// tracked edits but excluding non-ignored untracked files. Comparing a full
// snapshot against it therefore shows those untracked files as additions.
func (t *ChangeTracker) CaptureBaseline(ctx context.Context, dir string) (ChangeBaseline, string, error) {
	repo, err := t.repo(ctx, dir)
	if err != nil {
		return ChangeBaseline{}, "", err
	}
	tree, ok, reason, err := t.snapshot(ctx, repo)
	if err != nil || !ok {
		return ChangeBaseline{}, reason, err
	}
	ctx, cancel := context.WithTimeout(ctx, t.limits.SnapshotTimeout)
	defer cancel()
	tracked, err := repo.runner.git(ctx, repo.top, nil, nil, "ls-files", "-z")
	if err != nil {
		return ChangeBaseline{}, "", err
	}
	names := make(map[string]bool)
	for _, name := range bytes.Split(tracked, []byte{0}) {
		names[string(name)] = true
	}
	temp, err := os.MkdirTemp(t.indexes, "baseline-")
	if err != nil {
		return ChangeBaseline{}, "", err
	}
	defer os.RemoveAll(temp)
	env := append([]string(nil), repo.env...)
	env[0] = "GIT_INDEX_FILE=" + filepath.Join(temp, "index")
	if _, err = repo.runner.git(ctx, repo.top, env, nil, "read-tree", tree); err != nil {
		return ChangeBaseline{}, "", err
	}
	all, err := repo.runner.git(ctx, repo.top, env, nil, "ls-files", "-z")
	if err != nil {
		return ChangeBaseline{}, "", err
	}
	var remove []byte
	for _, name := range bytes.Split(all, []byte{0}) {
		if len(name) > 0 && !names[string(name)] {
			remove = append(append(remove, name...), 0)
		}
	}
	if len(remove) > 0 {
		if _, err = repo.runner.git(ctx, repo.top, env, remove, "update-index", "--force-remove", "-z", "--stdin"); err != nil {
			return ChangeBaseline{}, "", err
		}
	}
	out, err := repo.runner.git(ctx, repo.top, env, nil, "write-tree")
	if err != nil {
		return ChangeBaseline{}, "", err
	}
	tree = strings.TrimSpace(string(out))
	var pack limitedPack
	if err = repo.runner.runTo(ctx, repo.top, env, []byte(tree+"\n"), true, &pack, "pack-objects", "--stdout", "--revs"); err != nil {
		return ChangeBaseline{}, "", err
	}
	return ChangeBaseline{Root: repo.top, Tree: tree, Pack: pack.Bytes()}, "", nil
}

// RestoreBaseline imports a host-owned baseline into the disposable object
// cache. No source repository objects or refs are written.
func (t *ChangeTracker) RestoreBaseline(ctx context.Context, base ChangeBaseline) error {
	if len(base.Pack) > ChangeMaxBaselineBytes {
		return errors.New("workspace baseline exceeds 64 MiB")
	}
	if len(base.Tree) != 40 && len(base.Tree) != 64 {
		return errors.New("invalid baseline tree")
	}
	for _, c := range base.Tree {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return errors.New("invalid baseline tree")
		}
	}
	repo, err := t.repo(ctx, base.Root)
	if err != nil {
		return err
	}
	repo.snapMu.Lock()
	defer repo.snapMu.Unlock()
	if repo.reason != "" {
		return errors.New(repo.reason)
	}
	ctx, cancel := context.WithTimeout(ctx, t.limits.SnapshotTimeout)
	defer cancel()
	if _, err = repo.runner.git(ctx, repo.top, repo.env, base.Pack, "index-pack", "--stdin"); err != nil {
		return err
	}
	out, err := repo.runner.git(ctx, repo.top, repo.env, nil, "cat-file", "-t", base.Tree)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "tree" {
		return fmt.Errorf("baseline does not name a tree")
	}
	return nil
}
