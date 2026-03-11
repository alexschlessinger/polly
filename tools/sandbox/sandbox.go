package sandbox

import (
	"encoding/json"
	"fmt"
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

// WrapCmd applies sandbox restrictions to cmd if sb is non-nil.
// Returns nil when sb is nil (no sandbox available).
func WrapCmd(sb Sandbox, cmd *exec.Cmd) error {
	if sb == nil {
		return nil
	}
	return sb.Wrap(cmd)
}

// Config controls sandbox permissions. It can be unmarshaled from JSON
// (true for defaults, or an object with optional fields) and merged with
// another Config via the Merge method.
type Config struct {
	// Directories where file writes are allowed (supports ~ expansion).
	// The OS temp dir is included automatically unless DenyWrite is set.
	WritablePaths []string `json:"writablePaths,omitempty"`

	// Allow outbound network access (denied by default).
	AllowNetwork bool `json:"allowNetwork,omitempty"`

	// Block DNS resolution. Only effective when AllowNetwork is true.
	DenyDNS bool `json:"denyDNS,omitempty"`

	// Paths exempted from the DeniedPaths deny list.
	ReadPaths []string `json:"readPaths,omitempty"`

	// If non-empty, only these env vars are passed through to the sandbox.
	AllowEnv []string `json:"allowEnv,omitempty"`

	// Deny all file writes, including to temp directories.
	DenyWrite bool `json:"denyWrite,omitempty"`
}

// DefaultConfig returns the standard base sandbox config (temp-dir-only writes).
func DefaultConfig() Config {
	return Config{WritablePaths: []string{os.TempDir()}}
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

// ParseConfig parses a JSON sandbox field.
// Returns (nil, nil) for absent, null, or false.
// Returns an error for values that are not bool, null, or object
// (e.g. "yes", 123, []) so callers fail closed instead of silently
// running unsandboxed.
func ParseConfig(raw json.RawMessage) (*Config, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// null
	if string(raw) == "null" {
		return nil, nil
	}
	// bool
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return &Config{}, nil
		}
		return nil, nil
	}
	// object
	var c Config
	if json.Unmarshal(raw, &c) == nil {
		return &c, nil
	}
	return nil, fmt.Errorf("unsupported sandbox value: %s (must be true, false, or an object)", string(raw))
}

// Merge returns a new Config combining c (base) with overlay.
// Booleans are OR'd (either side can widen allowances or add restrictions,
// but neither can reduce them). Slices are appended.
func (c Config) Merge(overlay Config) Config {
	c.AllowNetwork = c.AllowNetwork || overlay.AllowNetwork
	c.DenyDNS = c.DenyDNS || overlay.DenyDNS
	c.WritablePaths = append(c.WritablePaths, overlay.WritablePaths...)
	c.ReadPaths = append(c.ReadPaths, overlay.ReadPaths...)
	c.AllowEnv = append(c.AllowEnv, overlay.AllowEnv...)
	c.DenyWrite = c.DenyWrite || overlay.DenyWrite
	return c
}

// expandTilde resolves a ~ prefix to the user's home directory for a single path.
func expandTilde(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
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
