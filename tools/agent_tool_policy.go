package tools

import (
	"fmt"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// restrictAgentToolSandbox applies tool-local restrictions without restoring
// authority removed by an explicit child preset. AllowEnv is an intersection,
// not the additive merge used for operator-selected policy layers.
func restrictAgentToolSandbox(policy, overlay sandbox.Config) (sandbox.Config, error) {
	cfg := policy.Merge(restrictiveSandboxConfig(overlay))
	return cfg.RestrictAllowEnv(overlay.AllowEnv)
}

// exposeToolExecutable retains the legacy executable-directory exposure unless
// an explicit child preset bounds the registry. In that case exposure only
// materializes authority already present in ceiling (including host-temp paths
// hidden by Linux's private mounts). A file-only grant never exposes siblings.
// Discovery supplies its inherited ceiling separately because it drops write
// grants; its own masks still apply before anything is exposed.
func (r *ToolRegistry) exposeToolExecutable(cfg, ceiling sandbox.Config, path string) (sandbox.Config, error) {
	if len(r.executionSandboxPresets) == 0 {
		return exposeExecutable(cfg, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return sandbox.Config{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return sandbox.Config{}, fmt.Errorf("resolve executable: %w", err)
	}
	// Check both the requested route and the selected canonical target, so a
	// retargeted alias cannot lend its earlier authorization to another file.
	for _, route := range []string{abs, real} {
		if err := sandbox.ReadAllowed(ceiling, route); err != nil {
			return sandbox.Config{}, fmt.Errorf("executable exceeds agent sandbox policy: %w", err)
		}
		if err := sandbox.ReadMasked(cfg, route); err != nil {
			return sandbox.Config{}, fmt.Errorf("executable is masked: %w", err)
		}
	}
	target := real
	dir := filepath.Dir(real)
	if !isHomeDirectory(dir) && dir != filepath.Dir(dir) && sandbox.ReadAllowed(ceiling, dir) == nil && sandbox.ReadMasked(cfg, dir) == nil {
		target = dir
	}
	return sandbox.ExposeReadOnlyPaths(cfg, target)
}
