package skills

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExpandPath expands ~ and ~/ prefixes to the user's home directory.
// Named home expansions like ~otheruser are rejected.
func ExpandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		switch {
		case path == "~":
			path = homeDir
		case strings.HasPrefix(path, "~/"):
			path = filepath.Join(homeDir, path[2:])
		default:
			return "", fmt.Errorf("unsupported home-directory expansion %q (only ~ and ~/... are supported)", path)
		}
	}
	return filepath.Clean(path), nil
}

// DefaultDir returns the default skill directory (~/.pollytool/skills) if it
// exists. The boolean indicates whether the directory was found.
func DefaultDir() (string, bool, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve home directory: %w", err)
	}
	path := filepath.Join(homeDir, ".pollytool", "skills")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return path, true, nil
}

// ResolveDirs expands, validates, and deduplicates skill directory paths.
// If paths is empty, the default skill directory is used when present.
func ResolveDirs(paths []string) ([]string, error) {
	if len(paths) == 0 {
		defaultDir, ok, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		paths = []string{defaultDir}
	}

	seen := make(map[string]bool)
	var resolved []string
	for _, path := range paths {
		expanded, err := ExpandPath(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(expanded)
		if err != nil {
			return nil, fmt.Errorf("skill path %s: %w", expanded, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("skill path %s is not a directory", expanded)
		}
		if seen[expanded] {
			continue
		}
		seen[expanded] = true
		resolved = append(resolved, expanded)
	}

	return resolved, nil
}

// ResolvedSkill holds the result of resolving a --skill source.
type ResolvedSkill struct {
	Dir  string // local directory containing SKILL.md
	Name string // skill name (directory basename)
}

// ResolveSkill resolves a skill source (local path, git URL, or archive URL)
// to a local directory. Remote sources are cached under ~/.pollytool/cache/skills/.
func ResolveSkill(source string) (*ResolvedSkill, error) {
	if isSkillURL(source) {
		return resolveRemoteSkill(source)
	}
	return resolveLocalSkill(source)
}

// skillCacheDir returns the cache directory for a given URL, creating it if needed.
func skillCacheDir(rawURL string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	h := sha256.Sum256([]byte(rawURL))
	hash := hex.EncodeToString(h[:8])
	cacheDir := filepath.Join(homeDir, ".pollytool", "cache", "skills", hash)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	return cacheDir, nil
}

func isSkillURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func resolveLocalSkill(source string) (*ResolvedSkill, error) {
	expanded, err := ExpandPath(source)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(expanded)
	if err != nil {
		return nil, fmt.Errorf("skill path %s: %w", expanded, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skill path %s is not a directory", expanded)
	}
	skillFile := filepath.Join(expanded, skillFileName)
	if _, err := os.Stat(skillFile); err != nil {
		return nil, fmt.Errorf("skill path %s: missing %s", expanded, skillFileName)
	}
	return &ResolvedSkill{
		Dir:  expanded,
		Name: filepath.Base(expanded),
	}, nil
}

func resolveRemoteSkill(source string) (*ResolvedSkill, error) {
	cacheDir, err := skillCacheDir(source)
	if err != nil {
		return nil, err
	}

	// Check if already cached.
	if name, err := findSkillInDir(cacheDir); err == nil {
		return &ResolvedSkill{Dir: filepath.Join(cacheDir, name), Name: name}, nil
	}

	if isGitURL(source) {
		return cloneGitSkill(source, cacheDir)
	}
	return fetchArchiveSkill(source, cacheDir)
}

func isGitURL(s string) bool {
	if strings.HasSuffix(s, ".git") {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "github.com" || host == "gitlab.com"
}

// gitTreeURL holds the parsed components of a GitHub/GitLab /tree/ URL.
type gitTreeURL struct {
	RepoURL string // e.g. https://github.com/user/repo
	Ref     string // branch or tag, e.g. "main"
	Subpath string // subdirectory within repo, e.g. "skills/pdf"
}

// parseGitTreeURL extracts repo URL, ref, and subpath from GitHub/GitLab
// /tree/<ref>/path style URLs. Returns nil if the URL is not in that format.
func parseGitTreeURL(rawURL string) *gitTreeURL {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "gitlab.com" {
		return nil
	}
	// Path format: /<owner>/<repo>/tree/<ref>[/<subpath>...]
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "tree" {
		return nil
	}
	result := &gitTreeURL{
		RepoURL: u.Scheme + "://" + u.Host + "/" + parts[0] + "/" + parts[1],
		Ref:     parts[3],
	}
	if len(parts) > 4 {
		result.Subpath = strings.Join(parts[4:], "/")
	}
	return result
}

// moveDir moves src to dest, falling back to a recursive copy when
// os.Rename fails (e.g. cross-device moves between /tmp and /home).
func moveDir(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	return copyDir(src, dest)
}

func copyDir(src, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode()&0755)
	})
}

func cloneGitSkill(rawURL, cacheDir string) (*ResolvedSkill, error) {
	// Clone into a temp dir, then move into cache to avoid partial state.
	tmpDir, err := os.MkdirTemp("", "polly-skill-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := rawURL
	var subpath string

	// Handle GitHub/GitLab /tree/ URLs by extracting the real repo URL.
	if parsed := parseGitTreeURL(rawURL); parsed != nil {
		cloneURL = parsed.RepoURL
		subpath = parsed.Subpath
		cmd := exec.Command("git", "clone", "--depth", "1", "--branch", parsed.Ref, cloneURL, tmpDir)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git clone %s: %w", rawURL, err)
		}
	} else {
		cmd := exec.Command("git", "clone", "--depth", "1", cloneURL, tmpDir)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git clone %s: %w", rawURL, err)
		}
	}

	// If a subpath was specified, look for the skill there directly.
	searchDir := tmpDir
	if subpath != "" {
		searchDir = filepath.Join(tmpDir, subpath)
		if info, err := os.Stat(searchDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("skill from %s: subpath %q not found in repo", rawURL, subpath)
		}
		// Check if the subpath itself contains SKILL.md.
		if _, err := os.Stat(filepath.Join(searchDir, skillFileName)); err == nil {
			name := filepath.Base(subpath)
			dest := filepath.Join(cacheDir, name)
			if err := moveDir(searchDir, dest); err != nil {
				return nil, fmt.Errorf("cache skill from %s: %w", rawURL, err)
			}
			return &ResolvedSkill{Dir: dest, Name: name}, nil
		}
	}

	name, err := findSkillInDir(searchDir)
	if err != nil {
		return nil, fmt.Errorf("skill from %s: %w", rawURL, err)
	}

	dest := filepath.Join(cacheDir, name)
	if err := moveDir(filepath.Join(searchDir, name), dest); err != nil {
		return nil, fmt.Errorf("cache skill from %s: %w", rawURL, err)
	}

	return &ResolvedSkill{Dir: dest, Name: name}, nil
}

// findSkillInDir looks for a SKILL.md in dir itself or in a single subdirectory.
func findSkillInDir(dir string) (string, error) {
	// Check if SKILL.md is at the root of the cloned repo.
	if _, err := os.Stat(filepath.Join(dir, skillFileName)); err == nil {
		return filepath.Base(dir), nil
	}

	// Look for a single skill subdirectory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), skillFileName)); err == nil {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("no %s found", skillFileName)
}

func fetchArchiveSkill(rawURL, cacheDir string) (*ResolvedSkill, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}

	lower := strings.ToLower(rawURL)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		if err := extractTarGz(resp.Body, cacheDir); err != nil {
			return nil, fmt.Errorf("extract %s: %w", rawURL, err)
		}
	case strings.HasSuffix(lower, ".zip"):
		if err := extractZipFromHTTP(resp.Body, cacheDir); err != nil {
			return nil, fmt.Errorf("extract %s: %w", rawURL, err)
		}
	default:
		return nil, fmt.Errorf("unsupported archive format: %s (expected .tar.gz, .tgz, or .zip)", rawURL)
	}

	name, err := findSkillInDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("skill from %s: %w", rawURL, err)
	}

	return &ResolvedSkill{Dir: filepath.Join(cacheDir, name), Name: name}, nil
}

func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, filepath.Clean(header.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) && target != filepath.Clean(destDir) {
			continue // skip entries that escape destDir
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode)&0755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, maxSkillFileSize*100)); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

func extractZipFromHTTP(r io.Reader, destDir string) error {
	// zip needs random access, so buffer to a temp file.
	tmp, err := os.CreateTemp("", "polly-skill-zip-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := io.Copy(tmp, io.LimitReader(r, maxSkillFileSize*100)); err != nil {
		return err
	}

	info, err := tmp.Stat()
	if err != nil {
		return err
	}

	zr, err := zip.NewReader(tmp, info.Size())
	if err != nil {
		return err
	}

	for _, f := range zr.File {
		target := filepath.Join(destDir, filepath.Clean(f.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) && target != filepath.Clean(destDir) {
			continue
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()&0755)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(rc, maxSkillFileSize*100))
		rc.Close()
		out.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}

// LoadCatalog resolves dirs and discovers skills. Returns nil if no skills are
// found or if dirs resolves to nothing.
func LoadCatalog(dirs []string) (*Catalog, error) {
	resolved, err := ResolveDirs(dirs)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return nil, nil
	}

	catalog, err := Discover(resolved)
	if err != nil {
		return nil, err
	}
	if catalog.IsEmpty() {
		return nil, nil
	}
	return catalog, nil
}
