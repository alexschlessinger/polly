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
