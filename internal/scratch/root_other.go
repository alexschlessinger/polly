//go:build !unix

package scratch

import "io/fs"

// exclusiveToCurrentUser has nothing to check where the OS temp directory is
// already per-user. Permission bits are not consulted: Go reports every
// Windows directory as 0777, so a mode check would refuse them all.
func exclusiveToCurrentUser(fs.FileInfo) error { return nil }
