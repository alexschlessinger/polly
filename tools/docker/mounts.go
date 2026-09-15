package docker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// mount is one container mount. Bind mounts use canonical host paths as
// both source and target so paths mean the same thing on both sides.
type mount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

// helperMountPath is where a host-supplied helper binary appears.
const helperMountPath = "/run/polly/bin/polly"

// deriveMounts computes the bind-mode mount set for a scope: the worktree
// at its host path, its Git routing, read-only pins over the policy's
// protected Git metadata, the scratch, skill roots, and nothing else from
// the host. It then checks the set against every denied read: a mount
// inside a denial, or a denial inside a mount, cannot be honored
// structurally and fails closed. The scope's own root and scratch are the
// grants inside the coordinator's private roots, so a denial that merely
// contains them is satisfied by mounting only them.
func deriveMounts(scope tools.ToolScope, policy sandbox.Config, skillRoots []string, resolvEmpty, helper string) ([]mount, error) {
	root, err := canonicalDir(scope.Root)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	mounts := []mount{{Type: "bind", Source: root, Target: root, ReadOnly: scope.Grant.ReadOnly}}
	granted := []string{root}

	routing, err := sandbox.DiscoverGitRouting(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("git routing: %w", err)
	case routing.Linked():
		mounts = append(mounts,
			mount{Type: "bind", Source: routing.CommonDir, Target: routing.CommonDir, ReadOnly: true},
			mount{Type: "bind", Source: routing.GitDir, Target: routing.GitDir, ReadOnly: scope.Grant.ReadOnly})
		granted = append(granted, routing.CommonDir, routing.GitDir)
	case !sandbox.PathWithin(routing.GitDir, root):
		mounts = append(mounts, mount{Type: "bind", Source: routing.GitDir, Target: routing.GitDir, ReadOnly: true})
		granted = append(granted, routing.GitDir)
	}

	if scope.Grant.Scratch != "" {
		scratch, err := canonicalDir(scope.Grant.Scratch)
		if err != nil {
			return nil, fmt.Errorf("scratch: %w", err)
		}
		mounts = append(mounts, mount{Type: "bind", Source: scratch, Target: scratch})
		granted = append(granted, scratch)
	}

	// Protected Git metadata inside a writable mount becomes a read-only
	// mount over itself; hooks and configuration stay unwritable without a
	// process sandbox. A pin that no longer exists fails closed.
	var pins []string
	for _, path := range append(append([]string(nil), policy.DenyWritePaths...), scope.Grant.DeniedWrites...) {
		canonical, err := canonicalPath(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && !withinAny(filepath.Clean(path), granted) {
				continue
			}
			return nil, fmt.Errorf("protected path %s: %w", path, err)
		}
		if !insideWritable(canonical, mounts) {
			continue
		}
		pins = append(pins, canonical)
	}
	pins = outermost(pins)
	for _, pin := range pins {
		mounts = append(mounts, mount{Type: "bind", Source: pin, Target: pin, ReadOnly: true})
	}

	for _, skillRoot := range skillRoots {
		canonical, err := canonicalDir(skillRoot)
		if err != nil {
			continue
		}
		if withinAny(canonical, granted) {
			continue
		}
		mounts = append(mounts, mount{Type: "bind", Source: canonical, Target: canonical, ReadOnly: true})
	}
	if helper != "" {
		canonical, err := canonicalPath(helper)
		if err != nil {
			return nil, fmt.Errorf("helper binary: %w", err)
		}
		mounts = append(mounts, mount{Type: "bind", Source: canonical, Target: helperMountPath, ReadOnly: true})
	}
	if resolvEmpty != "" {
		mounts = append(mounts, mount{Type: "bind", Source: resolvEmpty, Target: "/etc/resolv.conf", ReadOnly: true})
	}

	if err := assertDeniedReads(mounts, denials(scope, policy), granted); err != nil {
		return nil, err
	}
	return mounts, nil
}

// denials lists every path a read must not reach: the credential masks,
// the policy's denied paths and the scope's denied reads.
func denials(scope tools.ToolScope, policy sandbox.Config) []string {
	var denied []string
	for _, entry := range sandbox.ExpandHome(sandbox.DeniedPaths) {
		denied = append(denied, entry.Path)
	}
	denied = append(denied, policy.DenyPaths...)
	denied = append(denied, scope.Grant.DeniedReads...)
	var canonical []string
	for _, path := range denied {
		if resolved, err := canonicalPath(path); err == nil {
			canonical = append(canonical, resolved)
		} else if resolved, err := sandbox.ResolveExistingPathPrefix(filepath.Clean(path)); err == nil {
			// A denial that does not exist yet still masks its route.
			canonical = append(canonical, resolved)
		} else {
			canonical = append(canonical, filepath.Clean(path))
		}
	}
	return canonical
}

// assertDeniedReads fails when a bind mount lies inside a denied read that
// does not merely contain a granted path, or when a denied read lies inside
// a mounted tree: neither can be honored by mounting.
func assertDeniedReads(mounts []mount, denied, granted []string) error {
	for _, m := range mounts {
		if m.Type != "bind" || m.Target == "/etc/resolv.conf" || m.Target == helperMountPath {
			continue
		}
		for _, d := range denied {
			if sandbox.PathWithin(m.Source, d) {
				grantedInside := false
				for _, g := range granted {
					if sandbox.PathWithin(g, d) && sandbox.PathWithin(m.Source, g) {
						grantedInside = true
						break
					}
				}
				if !grantedInside {
					return fmt.Errorf("mount %s lies inside denied read %s", m.Source, d)
				}
			}
			if sandbox.PathWithin(d, m.Source) && d != m.Source {
				return fmt.Errorf("denied read %s lies inside mount %s and cannot be hidden in a container", d, m.Source)
			}
		}
	}
	return nil
}

func canonicalDir(path string) (string, error) {
	canonical, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return canonical, nil
}

func canonicalPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not absolute", path)
	}
	return filepath.EvalSymlinks(filepath.Clean(path))
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		if sandbox.PathWithin(path, root) {
			return true
		}
	}
	return false
}

// insideWritable reports whether the deepest mount containing path is
// writable, so a read-only pin over it is needed.
func insideWritable(path string, mounts []mount) bool {
	deepest := -1
	for i, m := range mounts {
		if m.Type != "bind" || !sandbox.PathWithin(path, m.Source) {
			continue
		}
		if deepest < 0 || len(m.Source) > len(mounts[deepest].Source) {
			deepest = i
		}
	}
	return deepest >= 0 && !mounts[deepest].ReadOnly && path != mounts[deepest].Source
}

// outermost drops pins nested inside another pin.
func outermost(pins []string) []string {
	slices.Sort(pins)
	var kept []string
	for _, pin := range pins {
		if len(kept) > 0 && sandbox.PathWithin(pin, kept[len(kept)-1]) {
			continue
		}
		kept = append(kept, pin)
	}
	return kept
}
