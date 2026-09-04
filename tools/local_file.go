package tools

import (
	"errors"
	"fmt"
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
	routes = []string{abs}
	if resolved != abs {
		routes = append(routes, resolved)
	}
	return routes, resolved
}

// checkReadPolicy enforces the registry's base sandbox read policy on every
// route of an in-process read. Inactive sandboxing leaves reads unrestricted,
// just like wrapped commands.
func checkReadPolicy(registry *ToolRegistry, routes ...string) error {
	cfg, active, err := registry.SandboxReadPolicy()
	if err != nil {
		return fmt.Errorf("resolve sandbox policy: %w", err)
	}
	if !active {
		return nil
	}
	for _, route := range routes {
		if err := sandbox.ReadAllowed(cfg, route); err != nil {
			return err
		}
	}
	return nil
}

// checkWritePolicy enforces the registry's base sandbox write policy on every
// route of an in-process write. Inactive sandboxing leaves writes
// unrestricted, just like wrapped commands.
func checkWritePolicy(registry *ToolRegistry, routes ...string) error {
	cfg, active, err := registry.SandboxWritePolicy()
	if err != nil {
		return fmt.Errorf("resolve sandbox policy: %w", err)
	}
	if !active {
		return nil
	}
	for _, route := range routes {
		if err := sandbox.WriteAllowed(cfg, route); err != nil {
			return err
		}
	}
	return nil
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
