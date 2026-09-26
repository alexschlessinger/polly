//go:build !unix

package codex

import "context"

// lockFile is a no-op where advisory file locks are unavailable: refreshes
// from several processes are then serialized only within each process.
func lockFile(context.Context, string) (func(), error) { return func() {}, nil }
