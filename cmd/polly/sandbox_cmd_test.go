package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxCommandHostsTheHiddenHelper(t *testing.T) {
	root := getCommand()
	var sandboxCmd, helperCmd = root.Command("sandbox"), (*struct{})(nil)
	if sandboxCmd == nil {
		t.Fatal("polly sandbox is not registered")
	}
	_ = helperCmd
	helper := sandboxCmd.Command("helper")
	if helper == nil || !helper.Hidden {
		t.Fatalf("polly sandbox helper = %+v, want a hidden subcommand", helper)
	}
}

// The container sandbox is constructible only in helper mode, and helper mode
// is entered in exactly one place in the command: the helper subcommand.
func TestEnterHelperModeHasOneCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "EnterHelperMode(") {
			callers = append(callers, name)
		}
	}
	if len(callers) != 1 || callers[0] != "sandbox_cmd.go" {
		t.Fatalf("EnterHelperMode callers = %v, want sandbox_cmd.go alone", callers)
	}
}

func TestSandboxPruneCommandIsRegistered(t *testing.T) {
	prune := getCommand().Command("sandbox").Command("prune")
	if prune == nil || prune.Hidden {
		t.Fatalf("polly sandbox prune = %+v", prune)
	}
	for _, name := range []string{"dry-run", "all", "host"} {
		found := false
		for _, flag := range prune.Flags {
			for _, candidate := range flag.Names() {
				if candidate == name {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("prune lacks the --%s flag", name)
		}
	}
}
