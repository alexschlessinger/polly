package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// localFileMu serializes the in-process file mutations of write_file and
// edit_file. Tool calls in one batch run concurrently and an edit is a whole
// file read-modify-write, so two unserialized edits of one file would both
// report success while the last writer restored the first writer's old text.
var localFileMu sync.Mutex

// localRoutes returns the spellings a sandbox policy must approve before abs
// is opened: the path as given and its symlink-resolved route, resolved
// through the deepest existing ancestor when the target does not exist yet.
// The resolved route is what gets opened, and it is opened without following
// symlinks, so the object opened is the one the policy judged even if a
// concurrent command rewrites a link between the check and the open.
func localRoutes(abs string) (routes []string, resolved string) {
	resolved = abs
	if r, err := sandbox.ResolveExistingPathPrefix(abs); err == nil {
		resolved = filepath.Clean(r)
	}
	return pathRoutes(abs, resolved), resolved
}

// pathRoutes lists the spellings a policy must approve: the path as given
// and, when it differs, its resolved route.
func pathRoutes(abs, resolved string) []string {
	if resolved == abs {
		return []string{abs}
	}
	return []string{abs, resolved}
}

// resolveLocalRoutes resolves path against the registry's execution root and
// returns the spelling the model used, the routes a policy must approve and
// the resolved route to open.
func resolveLocalRoutes(registry *ToolRegistry, path string) (abs string, routes []string, resolved string, err error) {
	abs, err = registry.ResolvePath(path)
	if err != nil {
		return "", nil, "", err
	}
	routes, resolved = localRoutes(abs)
	return abs, routes, resolved, nil
}

// openLocalRead resolves path, enforces the read policy on every route and
// opens the resolved route read-only; op names the operation in errors.
func openLocalRead(registry *ToolRegistry, op, path string) (string, *os.File, os.FileInfo, error) {
	abs, routes, resolved, err := resolveLocalRoutes(registry, path)
	if err != nil {
		return "", nil, nil, err
	}
	if err := checkReadPolicy(registry, routes...); err != nil {
		return "", nil, nil, err
	}
	f, info, err := openLocalRegular(resolved, os.O_RDONLY, 0)
	if err != nil {
		return "", nil, nil, describeOpenError(op, abs, err)
	}
	return abs, f, info, nil
}

// readBoundedRegular reads all of an open regular file of at most maxBytes
// bytes. tooLarge reports a file over the bound, judged by its size before
// the read and by the bytes actually read because the file can grow in
// between; the read never truncates silently.
func readBoundedRegular(f *os.File, info os.FileInfo, maxBytes int64) (data []byte, tooLarge bool, err error) {
	if info.Size() > maxBytes {
		return nil, true, nil
	}
	data, err = io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	return data, int64(len(data)) > maxBytes, nil
}

// checkPathPolicy enforces the registry's base sandbox policy, via allowed,
// on every route of an in-process file access. Inactive sandboxing leaves
// access unrestricted, just like wrapped commands.
func checkPathPolicy(registry *ToolRegistry, allowed func(sandbox.Config, string) error, routes []string) error {
	cfg, active, err := registry.SandboxReadPolicy()
	if err != nil {
		return fmt.Errorf("resolve sandbox policy: %w", err)
	}
	if !active {
		return nil
	}
	for _, route := range routes {
		if err := allowed(cfg, route); err != nil {
			return err
		}
	}
	return nil
}

// checkReadPolicy enforces the registry's base sandbox read policy on every
// route of an in-process read.
func checkReadPolicy(registry *ToolRegistry, routes ...string) error {
	return checkPathPolicy(registry, sandbox.ReadAllowed, routes)
}

// compileReadPolicy is checkReadPolicy for a loop over many paths: the
// registry's base read policy compiled once, or nil when sandboxing is
// inactive and every read is allowed. Compile per loop, never keep one.
func compileReadPolicy(registry *ToolRegistry) (*sandbox.ReadPolicy, error) {
	cfg, active, err := registry.SandboxReadPolicy()
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox policy: %w", err)
	}
	if !active {
		return nil, nil
	}
	policy, err := sandbox.CompileReadPolicy(cfg)
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

// readRoutesAllowed enforces a compiled read policy on every route of an
// in-process read; a nil policy allows every read.
func readRoutesAllowed(policy *sandbox.ReadPolicy, routes ...string) error {
	if policy == nil {
		return nil
	}
	for _, route := range routes {
		if err := policy.Allowed(route); err != nil {
			return err
		}
	}
	return nil
}

// checkWritePolicy enforces the registry's base sandbox write policy on every
// route of an in-process write.
func checkWritePolicy(registry *ToolRegistry, routes ...string) error {
	return checkPathPolicy(registry, sandbox.WriteAllowed, routes)
}

// openLocalRegular opens the resolved route of a local file with flag and
// perm, refusing symlinks and special files at open time, and returns the
// descriptor with its verified metadata.
func openLocalRegular(resolved string, flag int, perm os.FileMode) (*os.File, os.FileInfo, error) {
	f, err := safefile.OpenRegular(resolved, flag, perm)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// describeOpenError formats an open failure for the model: a special file is
// described as such, everything else is prefixed with the operation and the
// path as the model spelled it.
func describeOpenError(op, abs string, err error) error {
	var notRegular *safefile.NotRegularError
	if errors.As(err, &notRegular) {
		return &safefile.NotRegularError{Path: abs, Mode: notRegular.Mode}
	}
	return fmt.Errorf("%s %s: %w", op, abs, err)
}
