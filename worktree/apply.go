package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

var ErrParentChanged = errors.New("parent changed since preparation; prepare again")

// PathState describes Git content and filesystem type, including absence.
type PathState struct {
	Exists bool   `json:"exists"`
	Kind   string `json:"kind,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Object string `json:"object,omitempty"`
}

type PathChange struct {
	Path   string    `json:"path"`
	Before PathState `json:"before"`
	After  PathState `json:"after"`
}

// ApplyPlan is immutable evidence for one delta. Patch bytes are reconstructed
// from pinned snapshots and checked against PatchHash before execution.
type ApplyPlan struct {
	ID        string       `json:"id"`
	Parent    Snapshot     `json:"parent"`
	Merged    Snapshot     `json:"merged"`
	Drift     string       `json:"drift"`
	PatchHash string       `json:"patchHash"`
	Paths     []PathChange `json:"paths"`
}

func (m *Manager) treeStates(ctx context.Context, snapshot Snapshot) (map[string]PathState, error) {
	out, err := m.git(ctx, m.Root, nil, nil, "ls-tree", "-rz", "--full-tree", snapshot.Tree)
	if err != nil {
		return nil, err
	}
	states := map[string]PathState{}
	for _, row := range bytes.Split(out, []byte{0}) {
		if len(row) == 0 {
			continue
		}
		header, name, ok := strings.Cut(string(row), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, errors.New("invalid Git tree entry")
		}
		kind := "file"
		if fields[0] == "120000" {
			kind = "symlink"
		} else if fields[0] != "100644" && fields[0] != "100755" {
			return nil, fmt.Errorf("unsupported integration entry %s", name)
		}
		states[name] = PathState{Exists: true, Kind: kind, Mode: fields[0], Object: fields[2]}
	}
	return states, nil
}

func (m *Manager) BuildApplyPlan(ctx context.Context, id string, parent, merged Snapshot, drift string) (ApplyPlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if drift != "tree" && drift != "paths" {
		return ApplyPlan{}, errors.New("drift must be paths or tree")
	}
	before, err := m.treeStates(ctx, parent)
	if err != nil {
		return ApplyPlan{}, err
	}
	after, err := m.treeStates(ctx, merged)
	if err != nil {
		return ApplyPlan{}, err
	}
	paths := map[string]bool{}
	for path, state := range before {
		if state != after[path] {
			paths[path] = true
		}
	}
	for path, state := range after {
		if state != before[path] {
			paths[path] = true
		}
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	p := ApplyPlan{ID: id, Parent: parent, Merged: merged, Drift: drift, Paths: []PathChange{}}
	for _, path := range names {
		p.Paths = append(p.Paths, PathChange{Path: path, Before: before[path], After: after[path]})
	}
	patch, err := m.patch(ctx, p)
	if err != nil {
		return ApplyPlan{}, err
	}
	p.PatchHash = fmt.Sprintf("%x", sha256.Sum256(patch))
	return p, nil
}

func (m *Manager) patch(ctx context.Context, p ApplyPlan) ([]byte, error) {
	patch, err := m.git(ctx, m.Root, nil, nil, "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", p.Parent.Commit, p.Merged.Commit, "--")
	if err == nil && p.PatchHash != "" && fmt.Sprintf("%x", sha256.Sum256(patch)) != p.PatchHash {
		return nil, errors.New("integration patch identity changed")
	}
	return patch, err
}

// ancestors checks directory types without following symlinks, including
// paths that did not exist when the candidate was captured.
func (m *Manager) ancestors(path string) error {
	full := filepath.Join(m.Root, filepath.FromSlash(path))
	if path == "" || filepath.IsAbs(path) || !sandbox.PathWithin(full, m.Root) || full == m.Root {
		return errors.New("invalid integration path")
	}
	dir := m.Root
	parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	for _, part := range parts {
		if part == "." {
			continue
		}
		if part == ".." {
			return errors.New("invalid integration path")
		}
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: ancestor of %s is no longer a directory", ErrParentChanged, path)
		}
	}

	return nil
}

func (m *Manager) currentStates(ctx context.Context, p ApplyPlan) (Snapshot, map[string]PathState, error) {
	for _, change := range p.Paths {
		if err := m.ancestors(change.Path); err != nil {
			return Snapshot{}, nil, err
		}
	}
	current, err := m.capture(ctx, m.Root)
	if err != nil {
		return Snapshot{}, nil, err
	}
	states, err := m.treeStates(ctx, current)
	if err != nil {
		return Snapshot{}, nil, err
	}
	for _, change := range p.Paths {
		info, statErr := os.Lstat(filepath.Join(m.Root, filepath.FromSlash(change.Path)))
		if errors.Is(statErr, os.ErrNotExist) {
			delete(states, change.Path)
		} else if statErr != nil {
			return Snapshot{}, nil, statErr
		} else if info.IsDir() {
			states[change.Path] = PathState{Exists: true, Kind: "directory"}
		}
	}
	return current, states, nil
}

// PreflightApply performs every cancellable check before the write boundary.
// The caller holds the parent execution gate until the durable outcome exists.
func (m *Manager) PreflightApply(ctx context.Context, p ApplyPlan) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, states, err := m.currentStates(ctx, p)
	if err != nil {
		return Snapshot{}, err
	}
	if p.Drift == "tree" && current.Tree != p.Parent.Tree {
		return Snapshot{}, ErrParentChanged
	}
	for _, change := range p.Paths {
		if states[change.Path] != change.Before {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrParentChanged, change.Path)
		}
	}
	patch, err := m.patch(ctx, p)
	if err == nil {
		err = m.applyPatch(ctx, patch, true)
	}
	return current, err
}

// WriteApply writes only files. Context detachment and outcome persistence
// belong to the coordinating runtime; this method still honors lease fencing.
func (m *Manager) WriteApply(ctx context.Context, p ApplyPlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	patch, err := m.patch(ctx, p)
	if err != nil {
		return err
	}
	return m.applyPatch(ctx, patch, false)
}

func (m *Manager) applyPatch(ctx context.Context, patch []byte, check bool) error {
	cfg, _, err := m.Registry.SandboxReadPolicy()
	if err != nil {
		return err
	}
	if cfg.DenyWrite {
		return errors.New("parent policy forbids integration writes")
	}
	if len(patch) == 0 {
		return ctx.Err()
	}
	cfg.WritablePaths = []string{m.Root, m.Directory}
	cfg = cfg.Merge(sandbox.Config{DenyWritePaths: []string{m.GitDir, filepath.Join(m.Root, ".git")}})
	saved := m.sandbox
	if m.Registry.HasSandbox() {
		m.sandbox, err = m.Registry.NewSandboxDirect(cfg)
		if err != nil {
			return err
		}
	}
	defer func() { m.sandbox = saved }()
	args := []string{"apply", "--binary"}
	if check {
		args = append(args, "--check")
	}
	_, err = m.git(ctx, m.Root, nil, patch, append(args, "-")...)
	return err
}

// ReconcileApply observes files, never replays a write. Untouched paths do not
// obscure whether this particular delta happened.
func (m *Manager) ReconcileApply(ctx context.Context, p ApplyPlan) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, states, err := m.currentStates(ctx, p)
	if err != nil {
		return "recovery_required", err
	}
	before, after := true, true
	for _, change := range p.Paths {
		before = before && states[change.Path] == change.Before
		after = after && states[change.Path] == change.After
	}
	if after { // Includes an empty delta: there are no filesystem effects left.
		return "applied", nil
	}
	if before {
		return "not_applied", nil
	}
	return "recovery_required", nil
}

func (m *Manager) RecordApply(id string, value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.manifest("apply-"+id, value)
}
