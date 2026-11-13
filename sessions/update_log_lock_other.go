//go:build !unix && !windows

package sessions

import "os"

// lockLogFile reports no lock taken: without one this platform cannot tell a
// log whose owner is gone from one still in use, so it recovers none.
func lockLogFile(*os.File) (bool, error) { return false, nil }
