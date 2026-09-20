//go:build !unix && !windows

package worktree

import (
	"errors"
	"io"
)

func lockChangeStore(path string, exclusive bool) (io.Closer, error) {
	return nil, errors.New("change tracking leases are unsupported on this platform")
}
