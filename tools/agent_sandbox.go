package tools

import (
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// ValidateAgentSandboxPreset validates an explicit child sandbox preset without
// filesystem access or mutations. Omission is represented by no preset, not an
// empty string. Filesystem resolution and backend validation happen at child binding.
func ValidateAgentSandboxPreset(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return fmt.Errorf("agent sandbox preset must not be empty")
	}
	return sandbox.ValidatePreset(spec)
}

func (r *ToolRegistry) restrictAgentSandbox(cfg, inherited sandbox.Config, root, scratch string, grant ExecutionGrant, specs []string) (sandbox.Config, error) {
	constrained := r.HasSandbox() || r.executionPolicy != nil
	if constrained {
		// Rebinding relocates workspace authority, not a base/readonly policy's
		// absent workspace grant. Test the source, not the child's temp location.
		source := grant.SourceRoot
		if source == "" {
			var err error
			if source, err = r.workRoot(); err != nil {
				return sandbox.Config{}, err
			}
		}
		if sandbox.WriteAllowed(inherited, source) != nil {
			cfg.DenyWritePaths = append(cfg.DenyWritePaths, root)
		}
	}
	for i, spec := range specs {
		preset, err := sandbox.ParseChildPresetAtRoot(spec, root)
		if err != nil {
			return sandbox.Config{}, fmt.Errorf("agent sandbox %q: %w", spec, err)
		}
		// A base preset cannot acquire workspace writes through the implicit
		// host-temp grant when a member's checkout happens to live under /tmp.
		workspace := slices.ContainsFunc(strings.Split(spec, "+"), func(part string) bool {
			part = strings.TrimSpace(part)
			return part == "workspace" || part == "default"
		})
		if !workspace {
			preset.DenyWritePaths = append(preset.DenyWritePaths, root)
		}
		if scratch != "" && !preset.DenyWrite {
			preset.WritablePaths = append(preset.WritablePaths, scratch)
		}
		if !r.HasSandbox() && len(r.executionSandboxPresets) == 0 && i == 0 {
			// A native filesystem policy is not a process-network ceiling.
			// The first explicit spec under an unsandboxed source may opt into
			// net/SSH, subject to filesystem denials; later specs never can.
			cfg.AllowNetwork = preset.AllowNetwork
			if r.executionPolicy == nil {
				// Only a source without native filesystem constraints may add
				// read exemptions. Bound sources intersect their actual reads
				// below, retaining credential masks and private-home boundaries.
				cfg.ReadPaths = slices.Concat(cfg.ReadPaths, undeniedPaths(preset.ReadPaths, cfg.DenyPaths))
			}
			cfg.AllowUnixSockets = undeniedPaths(preset.AllowUnixSockets, cfg.DenyPaths)
			cfg.PassEnv = slices.Clone(preset.PassEnv)
		}
		cfg, err = sandbox.RestrictPreset(cfg, preset)
		if err != nil {
			return sandbox.Config{}, fmt.Errorf("agent sandbox %q: %w", spec, err)
		}
	}
	return cfg, nil
}
