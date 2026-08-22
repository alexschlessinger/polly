//go:build !darwin && !linux

package sandbox

import (
	"fmt"
	"runtime"
)

// New returns an error on unsupported platforms.
func New(cfg Config) (Sandbox, error) {
	return nil, fmt.Errorf("sandboxing is unsupported on %s", runtime.GOOS)
}

func freezeAuthorityPathsForPlatform(cfg Config) (Config, error) {
	return freezeAuthorityPaths(cfg)
}

// Unsupported backends have no private mount roots whose writes can be
// excluded from the host-visible Git persistence audit. Keep the shared
// policy conservative so this package continues to compile on those targets.
func gitHostWritablePath(string) bool {
	return true
}
