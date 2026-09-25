//go:build unix

package codex

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive advisory lock on path, creating it readable
// by its owner alone, and returns the release. It waits as long as ctx
// allows, polling every 25ms.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, err
		}
		if err := sleepFor(ctx, 25*time.Millisecond); err != nil {
			f.Close()
			return nil, err
		}
	}
}
