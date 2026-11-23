//go:build !unix && !windows

package main

import (
	"errors"
	"os"
)

func openRegularDraft(*os.Root, string) (*os.File, error) { return nil, errors.ErrUnsupported }

// Without a verified single-link regular file, fail closed.
func singleLinkDraft(*os.File, os.FileInfo) bool { return false }
