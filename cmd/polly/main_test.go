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
func TestMain(m *testing.M) {
	if os.Getenv("POLLY_TEST_ONESHOT_ARGS") != "" {
		os.Exit(m.Run())
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		panic("resolve home directory: " + err.Error())
	}
	home, err := os.MkdirTemp(realHome, ".polly-test-home-*")
	if err != nil {
		panic("create test home: " + err.Error())
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
