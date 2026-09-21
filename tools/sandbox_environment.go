package tools

import (
	"fmt"
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
	Storage                             SandboxStorage
	Roots                               SandboxStorageRoots
	Env                                 map[string]string
	CheckoutCacheRoot, CheckoutDataRoot string
}

type SandboxStorage = envstorage.Spec
type SandboxStorageRoots = envstorage.Roots

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
		key := envstorage.CheckoutKey(environmentCheckoutRoot(checkout))
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
			if err := envstorage.CopyConfig(e.Roots, a, roots); err != nil {
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

// Bindings made from a checkout subdirectory reuse that checkout's storage.
// A non-Git execution root continues to identify its own environment.
func environmentCheckoutRoot(root string) string {
	for path := root; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			return path
		}
		if filepath.Dir(path) == path {
			return root
		}
	}
}
