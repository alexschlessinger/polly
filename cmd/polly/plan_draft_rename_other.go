//go:build !linux && !darwin && !windows

package main

import (
	"errors"
	"os"
)

// Never fall back to a check followed by a potentially destructive rename.
func renameDraftNoReplace(*os.File, string, string) error { return errors.ErrUnsupported }
