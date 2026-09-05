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
func loadRepositoryInstructions(registry *tools.ToolRegistry) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("read working directory: %w", err)
	}
	var cfg sandbox.Config
	var active bool
	if registry != nil {
		cfg, active, err = registry.SandboxReadPolicy()
		if err != nil {
			return "", fmt.Errorf("resolve repository instruction policy: %w", err)
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

	dirs := []string{cwd}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		marker, err := resolve(filepath.Join(dir, ".git"))
		if err != nil {
			return "", fmt.Errorf("find repository root: %w", err)
		}
		info, err := os.Lstat(marker)
		if err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			slices.Reverse(dirs)
			break
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("find repository root: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			dirs = []string{cwd}
			break
		}
		dirs = append(dirs, parent)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Working directory: %q\n\n<repository_instructions>\n", cwd)
	total := 0
	for _, dir := range dirs {
		path := filepath.Join(dir, "AGENTS.md")
		resolved, err := resolve(path)
		if err != nil {
			return "", fmt.Errorf("load repository instructions: %w", err)
		}
		content, err := readRepositoryInstructions(resolved)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("load repository instructions %s: %w", path, err)
		}
		total += len(content)
		if total > maxRepositoryInstructionsBytes {
			return "", fmt.Errorf("repository instructions exceed %d bytes in total", maxRepositoryInstructionsBytes)
		}
		if strings.TrimSpace(content) != "" {
			fmt.Fprintf(&b, "<file path=\"%s\" scope=\"%s\">\n%s\n</file>\n",
				html.EscapeString(path), html.EscapeString(dir), html.EscapeString(content))
		}
	}
	b.WriteString("</repository_instructions>")
	return b.String(), nil
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
