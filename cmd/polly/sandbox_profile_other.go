//go:build !unix

package main

import "io/fs"

// ownedByUserAlone has nothing to check where the sandbox, and so a profile,
// does not run.
func ownedByUserAlone(fs.FileInfo) error { return nil }
