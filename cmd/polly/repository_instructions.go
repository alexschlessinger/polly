package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

const (
	maxRepositoryInstructionBytes  = 32 << 10
	maxRepositoryInstructionsBytes = 64 << 10
)

// loadRepositoryInstructions reads the nearest Git root's instructions and
// each directory's instructions down to cwd, in increasing specificity. With
// no Git root, only cwd is considered. Re-reading per turn keeps instructions
// current without persisting machine-local guidance in a portable session.
// Guidance never blocks a turn: a file that cannot be loaded is skipped and
// named in the returned warnings.
func loadRepositoryInstructions(registry *tools.ToolRegistry) (instructions string, warnings []string) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", []string{fmt.Sprintf("repository instructions not loaded: read working directory: %v", err)}
	}
	var cfg sandbox.Config
	var active bool
	if registry != nil {
		cfg, active, err = registry.SandboxReadPolicy()
		if err != nil {
			return "", []string{fmt.Sprintf("repository instructions not loaded: resolve read policy: %v", err)}
		}
	}
	// Approve both spellings, then open only the resolved route without
	// following symlinks. This matches the native file tools' read policy.
	resolve := func(path string) (string, error) {
		if active {
			if err := sandbox.ReadAllowed(cfg, path); err != nil {
				return "", err
			}
		}
		resolved, err := sandbox.ResolveExistingPathPrefix(path)
		if err != nil {
			return "", err
		}
		if active {
			if err := sandbox.ReadAllowed(cfg, resolved); err != nil {
				return "", err
			}
		}
		return resolved, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Working directory: %s\n\n<repository_instructions>\n", cwd)
	total := 0
	for _, dir := range repositoryInstructionDirs(cwd) {
		path := filepath.Join(dir, "AGENTS.md")
		// A policy denial covering a directory without instructions is not
		// news; only a file that exists and cannot be loaded is.
		if _, err := os.Lstat(path); err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("repository instructions %s skipped: %v", path, err))
			}
			continue
		}
		resolved, err := resolve(path)
		var content string
		if err == nil {
			content, err = readRepositoryInstructions(resolved)
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("repository instructions %s skipped: %v", path, err))
			continue
		}
		if total+len(content) > maxRepositoryInstructionsBytes {
			warnings = append(warnings, fmt.Sprintf("repository instructions %s skipped: instructions exceed %d bytes in total", path, maxRepositoryInstructionsBytes))
			continue
		}
		total += len(content)
		if strings.TrimSpace(content) != "" {
			fmt.Fprintf(&b, "<file path=\"%s\" scope=\"%s\">\n%s\n</file>\n",
				html.EscapeString(path), html.EscapeString(dir), content)
		}
	}
	b.WriteString("</repository_instructions>")
	return b.String(), warnings
}

// repositoryInstructionDirs lists the directories whose instructions apply
// to cwd, root first: the nearest ancestor holding a .git directory or
// worktree file through cwd, or cwd alone outside Git. Finding the root
// only probes for the marker; nothing is read.
func repositoryInstructionDirs(cwd string) []string {
	dirs := []string{cwd}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			slices.Reverse(dirs)
			return dirs
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return []string{cwd}
		}
		dirs = append(dirs, parent)
	}
}

func readRepositoryInstructions(path string) (string, error) {
	f, err := safefile.OpenRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRepositoryInstructionBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxRepositoryInstructionBytes {
		return "", fmt.Errorf("file exceeds %d bytes", maxRepositoryInstructionBytes)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("file must be UTF-8 text without NUL bytes")
	}
	return string(data), nil
}
