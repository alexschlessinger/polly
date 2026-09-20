package sandbox

import (
	"path/filepath"
	"slices"
)

// ExposedCredentials reports what of the credential deny list a config hands
// to sandboxed commands: the read and write grants at or inside a masked
// credential path (a grant at least as deep as the mask wins for reads), and
// the credential-shaped environment variables it lets through, by passEnv or
// by an allowEnv allowlist. A grant that merely contains a credential path
// exposes nothing, since the deeper mask still wins, and the operator's own
// denyPaths are not credentials. Explicit grants are the operator's choice;
// callers show the result so the choice stays visible, and nothing here
// refuses it.
func ExposedCredentials(cfg Config) (paths, env []string) {
	masks, err := compileReadPolicy(Config{}, nil)
	if err != nil {
		return nil, nil
	}
	for _, path := range slices.Concat(cfg.ReadPaths, cfg.WritablePaths) {
		if path == "" {
			continue
		}
		path = filepath.Clean(expandTilde(path))
		if masks.Allowed(path) != nil && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	names := cfg.PassEnv
	if len(cfg.AllowEnv) > 0 {
		names = cfg.AllowEnv
	}
	for _, name := range names {
		if isSensitiveEnv(name) && !slices.Contains(env, name) {
			env = append(env, name)
		}
	}
	return paths, env
}
