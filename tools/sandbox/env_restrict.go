package sandbox

import (
	"fmt"
	"strings"
)

// RestrictAllowEnv intersects a tool-local ambient environment allowlist with
// cfg's existing filter. An absent allowlist leaves that filter unchanged.
// Name matching follows the process filter: allow/pass names are case-sensitive,
// while the default sensitive-name detection is case-insensitive. Explicit Env
// values remain policy-selected configuration, not ambient passthrough grants.
//
// An empty intersection is rejected: Config's empty AllowEnv means the default
// sensitive-name filter, not deny-all. Callers must fail/omit the incompatible
// tool rather than silently restore the default ambient environment.
func (cfg Config) RestrictAllowEnv(allow []string) (Config, error) {
	if len(allow) == 0 {
		return cfg, nil
	}
	candidates := make([]string, 0, len(allow))
	for _, name := range allow {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return Config{}, fmt.Errorf("invalid allowEnv name %q", name)
		}
		candidates = append(candidates, name+"=")
	}
	filtered, _ := filterEnv(candidates, cfg.AllowEnv, cfg.PassEnv)
	if len(filtered) == 0 {
		return Config{}, fmt.Errorf("tool allowEnv has no names permitted by the inherited environment policy")
	}
	cfg.AllowEnv = make([]string, 0, len(filtered))
	for _, entry := range filtered {
		name, _, _ := strings.Cut(entry, "=")
		cfg.AllowEnv = append(cfg.AllowEnv, name)
	}
	return cfg, nil
}
