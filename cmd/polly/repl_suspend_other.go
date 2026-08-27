//go:build windows || plan9 || js || wasip1

package main

import "errors"

func suspendCurrentProcessGroup() error {
	return errors.New("job-control suspension is unsupported on this platform")
}
