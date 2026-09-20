package main

import (
	"os"
	"testing"
)

// TestMain runs the package under a fresh HOME. Flags read
// ~/.pollytool/config as a default source, so the developer's own defaults
// would otherwise leak into every parse. The directory sits under the real
// home because polly refuses a HOME under the system temp directory. A
// fixture rerunning this binary as polly has set HOME itself and keeps it.
// Sandbox environments that deny home writes can point POLLYTOOL_TEST_HOME at
// a writable base outside the temp directories; it only picks the parent the
// test home is created under.
func TestMain(m *testing.M) {
	if os.Getenv("POLLY_TEST_ONESHOT_ARGS") != "" {
		os.Exit(m.Run())
	}
	base := os.Getenv("POLLYTOOL_TEST_HOME")
	if base == "" {
		realHome, err := os.UserHomeDir()
		if err != nil {
			panic("resolve home directory: " + err.Error())
		}
		base = realHome
	}
	home, err := os.MkdirTemp(base, ".polly-test-home-*")
	if err != nil {
		panic("create test home: " + err.Error())
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
