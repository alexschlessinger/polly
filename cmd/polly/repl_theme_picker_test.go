package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThemePickerSaveFailureDistinguishesAppliedFromSaved(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	r, _ := chromeTestREPL(t)
	// A directory at the config path refuses persistence without blocking the UI switch.
	if err := os.MkdirAll(filepath.Join(home, userConfigDirName, userConfigFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.switchTheme("amber-parrot"), "\n")
	if !strings.Contains(got, "applied to UI now") || !strings.Contains(got, "Warning: theme not saved for later launches:") || strings.Contains(got, "saved as default") {
		t.Fatalf("save failure confirmation confuses scopes: %s", got)
	}
	if r.activeThemeName() != "amber-parrot" {
		t.Fatal("save failure reverted the UI switch")
	}
}
