package tools

import (
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// SandboxEnvironment describes the managed part of a layer. The host prepares
// Roots before publishing it. Env values are allocation references, not paths.
// Explicit Members settings override these defaults.
type SandboxEnvironment struct {
	Storage                             envstorage.Spec
	Roots                               envstorage.Roots
	Env                                 map[string]string
	CheckoutCacheRoot, CheckoutDataRoot string
}

func (e *SandboxEnvironment) clone() *SandboxEnvironment {
	if e == nil {
		return nil
	}
	copy := *e
	copy.Storage, copy.Env = e.Storage.Clone(), maps.Clone(e.Env)
	return &copy
}

// environmentForContext never gives a read-only context a writable host cache.
// Mutable member state lives with its existing scratch and is released with it.
func environmentForContext(e *SandboxEnvironment, checkout, scratch string, readOnly bool, denied []string) (sandbox.Config, error) {
	var cfg sandbox.Config
	if scratch == "" {
		return cfg, fmt.Errorf("managed environment needs an execution scratch")
	}
	roots := envstorage.Roots{
		Cache: filepath.Join(scratch, "environment", "cache"), SharedCache: e.Roots.SharedCache,
		State: filepath.Join(scratch, "environment", "state"), Config: filepath.Join(scratch, "environment", "config"),
		Control: e.Roots.Control,
	}
	if !readOnly {
		key := envstorage.CheckoutKey(checkout)
		roots.Cache = filepath.Join(e.CheckoutCacheRoot, key)
		roots.State = filepath.Join(e.CheckoutDataRoot, key, "state")
		roots.Config = filepath.Join(e.CheckoutDataRoot, key, "config")
	}
	spec := e.Storage.Clone()
	for i, a := range spec.Allocations {
		owned, err := e.Roots.Open(a)
		if err != nil {
			return cfg, err
		}
		owned.Close()
		if sandbox.DeniedBy(denied, e.Roots.Path(a)) {
			return cfg, fmt.Errorf("managed allocation %s is denied in this context", a.Key())
		}
		if readOnly {
			spec.Allocations[i].Shared = false
		}
		if sandbox.DeniedBy(denied, roots.Path(spec.Allocations[i])) {
			return cfg, fmt.Errorf("managed allocation %s is denied in this context", a.Key())
		}
	}
	// Shared caches have host-side receipts. Prepare local allocations separately.
	local := spec.Clone()
	local.Allocations = nil
	for _, a := range spec.Allocations {
		if !a.Shared {
			local.Allocations = append(local.Allocations, a)
		}
	}
	if err := roots.Ensure(local); err != nil {
		return cfg, err
	}
	for _, a := range spec.Allocations {
		path := roots.Path(a)
		if a.Shared {
			path = e.Roots.Path(a)
		}
		if a.Kind == "config" && path != e.Roots.Path(a) {
			if err := copyEnvironmentConfig(e.Roots, a, path); err != nil {
				return cfg, err
			}
		}
		cfg.WritablePaths = append(cfg.WritablePaths, path)
	}
	for name, ref := range e.Env {
		value, err := roots.Resolve(spec, ref)
		if err != nil {
			return cfg, err
		}
		if cfg.Env == nil {
			cfg.Env = map[string]string{}
		}
		cfg.Env[name] = value
	}
	return cfg, nil
}

// Configuration is copied without following links or overwriting a context's
// existing edits. Bound both the walk and copied bytes; oversized configuration
// is a setup error, not a reason to silently omit files.
func copyEnvironmentConfig(roots envstorage.Roots, allocation envstorage.Allocation, target string) error {
	src, err := roots.Open(allocation)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer dst.Close()
	count, total := 0, int64(0)
	return fs.WalkDir(src.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		count++
		if count > 4096 {
			return fmt.Errorf("managed configuration has too many files")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration contains a symbolic link: %s", name)
		}
		if d.IsDir() {
			return dst.MkdirAll(name, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("configuration is not a regular file: %s", name)
		}
		if _, err := dst.Lstat(name); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > 16<<20 {
			return fmt.Errorf("managed configuration exceeds 16 MiB")
		}
		// A replacement symlink inside the opened root cannot escape that root.
		f, err := src.Open(name)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
		f.Close()
		if err != nil {
			return err
		}
		if len(data) > 16<<20 {
			return fmt.Errorf("managed configuration grew during copy")
		}
		if err := dst.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		f, err = dst.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
