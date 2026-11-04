package sandbox

import "slices"

// RestrictPreset intersects cfg with a materialized preset. Unlike Merge,
// network and grants can only narrow. Policy environment bindings and strict
// environment allowlists belong to cfg; presets cannot replace them. Callers
// add their rebound root/scratch grants to the preset before restricting it.
// Both inputs are prepared so path aliases cannot change the intersection.
func RestrictPreset(cfg, preset Config) (Config, error) {
	var err error
	if cfg, err = PrepareConfig(cfg); err != nil {
		return Config{}, err
	}
	if preset, err = PrepareConfig(preset); err != nil {
		return Config{}, err
	}
	// Merge only restrictions and frozen identity/Git records, never grants.
	out := cfg.Merge(Config{
		PrivateHome: preset.PrivateHome, DenyDNS: preset.DenyDNS,
		DenyPaths: preset.DenyPaths, DenyWritePaths: preset.DenyWritePaths,
		DenyWrite: preset.DenyWrite, DenyHostTemp: preset.DenyHostTemp,
		gitPolicies: preset.gitPolicies, authorityPaths: preset.authorityPaths,
		grantSymlinks: preset.grantSymlinks,
	})
	out.AllowNetwork = cfg.AllowNetwork && preset.AllowNetwork
	out.WritablePaths = nil
	for _, path := range slices.Concat(cfg.WritablePaths, preset.WritablePaths) {
		if WriteAllowed(cfg, path) == nil && WriteAllowed(preset, path) == nil && !slices.Contains(out.WritablePaths, path) {
			out.WritablePaths = append(out.WritablePaths, path)
		}
	}
	out.ReadPaths = nil
	for _, path := range slices.Concat(cfg.ReadPaths, preset.ReadPaths) {
		if ReadAllowed(cfg, path) == nil && ReadAllowed(preset, path) == nil && !slices.Contains(out.ReadPaths, path) {
			out.ReadPaths = append(out.ReadPaths, path)
		}
	}
	out.visiblePaths = nil
	for _, path := range cfg.visiblePaths {
		if ReadAllowed(preset, path) == nil {
			out.visiblePaths = append(out.visiblePaths, path)
		}
	}
	out.AllowUnixSockets = nil
	for _, path := range cfg.AllowUnixSockets {
		if slices.Contains(preset.AllowUnixSockets, path) {
			out.AllowUnixSockets = append(out.AllowUnixSockets, path)
		}
	}
	out.PassEnv = nil
	for _, name := range cfg.PassEnv {
		if slices.Contains(preset.PassEnv, name) {
			out.PassEnv = append(out.PassEnv, name)
		}
	}
	return out, nil
}
