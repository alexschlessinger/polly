package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Sandbox wraps an exec.Cmd to apply platform-specific restrictions.
type Sandbox interface {
	// Wrap modifies cmd so it runs inside the sandbox.
	// The original command and args are preserved semantically;
	// the implementation may prepend a wrapper binary.
	Wrap(cmd *exec.Cmd) error
}

// Config controls sandbox permissions.
type Config struct {
	// Directories where file writes are allowed.
	// The OS temp dir is always included automatically.
	WritablePaths []string

	// Allow outbound network access (denied by default).
	AllowNetwork bool
}

// DeniedPathKind identifies how a denied path should be masked by platform sandboxes.
type DeniedPathKind string

const (
	DeniedPathDir  DeniedPathKind = "dir"
	DeniedPathFile DeniedPathKind = "file"
)

// DeniedPath describes a sensitive path blocked from sandboxed reads.
type DeniedPath struct {
	Path string
	Kind DeniedPathKind
}

// DeniedPaths are sensitive locations blocked from read access inside the sandbox.
// Paths starting with ~ are expanded to the user's home directory.
var DeniedPaths = []DeniedPath{
	{Path: "~/.ssh", Kind: DeniedPathDir},
	{Path: "~/.gnupg", Kind: DeniedPathDir},
	{Path: "~/.gpg", Kind: DeniedPathDir},
	{Path: "~/.aws", Kind: DeniedPathDir},
	{Path: "~/.azure", Kind: DeniedPathDir},
	{Path: "~/.config/gcloud", Kind: DeniedPathDir},
	{Path: "~/.kube", Kind: DeniedPathDir},
	{Path: "~/.docker/config.json", Kind: DeniedPathFile},
	{Path: "~/.npmrc", Kind: DeniedPathFile},
	{Path: "~/.pypirc", Kind: DeniedPathFile},
	{Path: "~/.gem/credentials", Kind: DeniedPathFile},
	{Path: "~/.cargo/credentials", Kind: DeniedPathFile},
	{Path: "~/.config/gh", Kind: DeniedPathDir},
	{Path: "~/.netrc", Kind: DeniedPathFile},
	{Path: "~/.git-credentials", Kind: DeniedPathFile},
	{Path: "~/.local/share/keyrings", Kind: DeniedPathDir},
	{Path: "~/Library/Keychains", Kind: DeniedPathDir},
}

// Spec describes per-tool sandbox overrides. It can be unmarshaled from
// true (use defaults) or an object with optional fields.
type Spec struct {
	AllowNetwork  bool     `json:"allowNetwork,omitempty"`
	WritablePaths []string `json:"writablePaths,omitempty"`
}

// ParseSpec parses a JSON sandbox field. Returns nil for absent, null, or false.
func ParseSpec(raw json.RawMessage) *Spec {
	if len(raw) == 0 {
		return nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return &Spec{}
		}
		return nil
	}
	var s Spec
	if json.Unmarshal(raw, &s) == nil {
		return &s
	}
	return nil
}

// MergeInto applies spec overrides onto a base Config.
func (s *Spec) MergeInto(base Config) Config {
	base.AllowNetwork = base.AllowNetwork || s.AllowNetwork
	base.WritablePaths = append(base.WritablePaths, s.WritablePaths...)
	return base
}

// ExpandHome resolves ~ prefixes to the user's home directory.
func ExpandHome(paths []DeniedPath) []DeniedPath {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	expanded := make([]DeniedPath, 0, len(paths))
	for _, p := range paths {
		path := p.Path
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		expanded = append(expanded, DeniedPath{Path: path, Kind: p.Kind})
	}
	return expanded
}
