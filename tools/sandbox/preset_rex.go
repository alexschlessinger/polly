package sandbox

import (
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// resolveRexSocketPath returns the first existing live Rex server Unix socket
// path from explicit environment variables or platform default locations.
// Relative paths and non-socket filesystem entries are ignored so the preset
// never widens into an unintended path grant.
func resolveRexSocketPath() string {
	var candidates []string

	if raw := strings.TrimSpace(os.Getenv("REX_SERVER")); raw != "" {
		if path := parseUnixSocketCandidate(raw); path != "" {
			candidates = append(candidates, path)
		}
	}
	if dir := strings.TrimSpace(os.Getenv("REX_RUNTIME_DIR")); dir != "" && filepath.IsAbs(dir) {
		candidates = append(candidates, filepath.Join(filepath.Clean(dir), "server.sock"))
	}
	candidates = append(candidates, defaultRexSocketCandidates()...)

	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(candidate))
		if err != nil {
			continue
		}
		info, err := os.Lstat(real)
		if err != nil || info.Mode()&fs.ModeSocket == 0 {
			continue
		}
		return filepath.Clean(real)
	}
	return ""
}

func parseUnixSocketCandidate(raw string) string {
	if strings.HasPrefix(raw, "unix://") {
		u, err := url.Parse(raw)
		if err != nil || u.Path == "" {
			return ""
		}
		if !filepath.IsAbs(u.Path) {
			return ""
		}
		return filepath.Clean(u.Path)
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return ""
}
