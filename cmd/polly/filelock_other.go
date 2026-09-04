//go:build !unix

package main

import "os"

// lockFile is a no-op where advisory file locks are unavailable.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
