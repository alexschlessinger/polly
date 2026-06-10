package sandbox

import (
	"bytes"
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

// Probe runs a trivial command through the sandbox to confirm it can actually
// start. Construction (New) only checks that the backend binary exists; it does
// not catch environments where the backend is present but fails at runtime
// (e.g. bwrap unable to create a mountpoint). Callers use this to fail fast with
// an actionable error instead of letting every tool call silently return a
// refusal. Returns nil when sb is nil (nothing to probe).
func Probe(sb Sandbox) error {
	if sb == nil {
		return nil
	}
	cmd := exec.Command("true")
	if err := sb.Wrap(cmd); err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
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

	// Extra paths blocked from reads, in addition to DeniedPaths (supports ~ expansion).
	DenyPaths []string `json:"denyPaths,omitempty"`

	// If non-empty, only these env vars are passed through to the sandbox.
	AllowEnv []string `json:"allowEnv,omitempty"`

	// Deny all file writes, including to temp directories.
	DenyWrite bool `json:"denyWrite,omitempty"`
}

// DefaultConfig returns the standard base sandbox config (temp-dir-only writes).
func DefaultConfig() Config {
	return Config{WritablePaths: []string{os.TempDir()}}
}

// validateConfig rejects only configs the sandbox fundamentally cannot honor.
// It deliberately does NOT reject a missing writablePaths entry: a path that
// doesn't exist yet (or was removed since the config was saved) must not brick
// tool loading or session restore — that one stale path would otherwise abort
// the whole startup. Missing writable binds are skipped per-command instead
// (buildBwrapArgs on Linux drops them; the macOS profile rule is inert for a
// path that isn't there), so writes there simply fail at runtime — fail-closed.
func validateConfig(cfg Config) error {
	if _, err := os.UserHomeDir(); err != nil {
		return fmt.Errorf("cannot resolve home directory for credential masking: %w", err)
	}
	return nil
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
// but neither can reduce them). Slices are concatenated into fresh arrays.
func (c Config) Merge(overlay Config) Config {
	c.AllowNetwork = c.AllowNetwork || overlay.AllowNetwork
	c.DenyDNS = c.DenyDNS || overlay.DenyDNS
	c.WritablePaths = concatStrings(c.WritablePaths, overlay.WritablePaths)
	c.ReadPaths = concatStrings(c.ReadPaths, overlay.ReadPaths)
	c.DenyPaths = concatStrings(c.DenyPaths, overlay.DenyPaths)
	c.AllowEnv = concatStrings(c.AllowEnv, overlay.AllowEnv)
	c.DenyWrite = c.DenyWrite || overlay.DenyWrite
	return c
}

// concatStrings returns a/b joined in a freshly allocated slice. Merge must not
// append onto its receiver's backing array: the registry reuses one
// baseSandboxCfg for every tool, so appending an overlay's entries in place
// could write into spare capacity shared with another tool's config and let one
// tool's denyPaths silently overwrite another's.
func concatStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// Default-stripped env vars: agent sockets give a sandboxed process use of the
// user's keys even with ~/.ssh and ~/.gnupg masked, and credential-shaped
// names (FOO_API_KEY, FOO_TOKEN, ...) are secrets regardless of which tool
// they belong to. AllowEnv passes any of these through explicitly.
var (
	sensitiveEnvNames    = map[string]bool{"SSH_AUTH_SOCK": true, "GPG_AGENT_INFO": true}
	sensitiveEnvPrefixes = []string{"POLLYTOOL_", "AWS_"}
	sensitiveEnvSuffixes = []string{
		"_API_KEY", "_APIKEY", "_TOKEN", "_SECRET", "_SECRET_KEY",
		"_ACCESS_KEY", "_PASSWORD", "_PASSPHRASE", "_CREDENTIALS", "_PRIVATE_KEY",
	}
)

func isSensitiveEnv(name string) bool {
	upper := strings.ToUpper(name)
	if sensitiveEnvNames[upper] {
		return true
	}
	for _, p := range sensitiveEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, s := range sensitiveEnvSuffixes {
		if strings.HasSuffix(upper, s) {
			return true
		}
	}
	return false
}

// filterEnv returns the env vars a sandboxed process should receive, plus the
// names (never values) of the vars it removed, for debug logging.
// With allowEnv set, only those names pass (an explicit allowlist wins over
// the sensitivity heuristics). Otherwise everything passes except
// sensitive-looking vars — see isSensitiveEnv.
func filterEnv(env, allowEnv []string) (filtered, stripped []string) {
	// Never leave filtered nil: os/exec treats a nil Env as "inherit the full
	// parent environment", so a config that strips every var (e.g. an allowEnv
	// listing only names absent from the environment) would hand the sandboxed
	// process every secret this function exists to remove. A non-nil empty
	// slice means "no environment", which is the fail-closed behavior we want.
	filtered = make([]string, 0, len(env))
	if len(allowEnv) > 0 {
		allowed := make(map[string]bool, len(allowEnv))
		for _, k := range allowEnv {
			allowed[k] = true
		}
		for _, e := range env {
			if k, _, _ := strings.Cut(e, "="); allowed[k] {
				filtered = append(filtered, e)
			} else {
				stripped = append(stripped, k)
			}
		}
		return filtered, stripped
	}
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); !isSensitiveEnv(k) {
			filtered = append(filtered, e)
		} else {
			stripped = append(stripped, k)
		}
	}
	return filtered, stripped
}

// commandSummary returns the first two argv entries — enough to identify the
// wrapped tool in debug logs without capturing argument payloads.
func commandSummary(args []string) string {
	if len(args) > 2 {
		args = args[:2]
	}
	return strings.Join(args, " ")
}

// allDeniedPaths combines the built-in deny list with cfg.DenyPaths, all
// tilde-expanded. User entries get their kind from a stat (missing paths
// default to file; platforms that need existence handle that themselves).
func allDeniedPaths(cfg Config) []DeniedPath {
	paths := ExpandHome(DeniedPaths)
	for _, p := range cfg.DenyPaths {
		expanded := expandTilde(p)
		kind := DeniedPathFile
		if fi, err := os.Stat(expanded); err == nil && fi.IsDir() {
			kind = DeniedPathDir
		}
		paths = append(paths, DeniedPath{Path: expanded, Kind: kind})
	}
	return paths
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
