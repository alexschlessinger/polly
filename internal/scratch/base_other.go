//go:build !darwin

package scratch

import "os"

func defaultRoot() string { return RootUnder(os.TempDir()) }
