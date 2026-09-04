package skills

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	if dir, err := findCachedSkill(cacheDir); err == nil {
		return &ResolvedSkill{Dir: dir, Name: filepath.Base(dir)}, nil
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

// Remote skill trees are bounded so a hostile archive cannot exhaust disk or
// inodes: the per-file, whole-tree, and entry-count limits all fail the fetch
// rather than silently truncating what lands in the cache.
const (
	maxArchiveFileBytes  = 32 << 20
	maxArchiveTotalBytes = 128 << 20
	maxArchiveEntries    = 10000
)

// rejectSymlinks fails when the tree under dir contains a symbolic link or any
// other non-regular file. Remote skill files are read by path later, so a link
// planted by the repository could otherwise point a skill file at a host
// secret and have it copied, displayed, or sent to a model.
func rejectSymlinks(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir || d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			return fmt.Errorf("%s is not a regular file (%s); remote skills may not contain symbolic links or special files", rel, d.Type())
		}
		return nil
	})
}

// moveDir moves the fetched skill tree at src to dest. Trees containing
// symbolic links are rejected, and the copy fallback is used only for
// cross-device moves, copying regular files by content so nothing is read
// through a link.
func moveDir(src, dest string) error {
	if err := rejectSymlinks(src); err != nil {
		return err
	}
	err := os.Rename(src, dest)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	return copyDir(src, dest)
}

func copyDir(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode()&0755)
	})
}

// stagingDir creates a scratch directory beside cacheDir for a fetch in
// progress, so that only a complete, validated skill is ever moved into the
// cache (by a same-device rename) and a failed fetch leaves nothing that a
// later run could mistake for a cached skill.
func stagingDir(cacheDir string) (string, func(), error) {
	dir, err := os.MkdirTemp(filepath.Dir(cacheDir), filepath.Base(cacheDir)+".partial-")
	if err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// stageSkill locates the skill inside a freshly fetched tree and moves it to
// cacheDir/<name>. The name is the skill directory's name, or the SKILL.md
// frontmatter name when the file sits at the tree root and the fetched
// directory therefore has no meaningful name of its own.
func stageSkill(tree, cacheDir string) (*ResolvedSkill, error) {
	skillDir, err := findSkillDir(tree)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(skillDir)
	if skillDir == tree {
		name, err = skillNameFromFile(filepath.Join(tree, skillFileName))
		if err != nil {
			return nil, err
		}
	}
	dest := filepath.Join(cacheDir, name)
	if err := os.RemoveAll(dest); err != nil {
		return nil, fmt.Errorf("clear cache entry: %w", err)
	}
	if err := moveDir(skillDir, dest); err != nil {
		return nil, fmt.Errorf("cache skill: %w", err)
	}
	return &ResolvedSkill{Dir: dest, Name: name}, nil
}

// skillNameFromFile reads the validated skill name from a SKILL.md so a
// root-level skill can be cached under a directory the catalog will accept.
func skillNameFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	meta, _, err := parseSkillMarkdown(string(data))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", skillFileName, err)
	}
	name := strings.TrimSpace(meta.Name)
	if name == "" || len(name) > 64 || !skillNamePattern.MatchString(name) {
		return "", fmt.Errorf("%s has no valid skill name", skillFileName)
	}
	return name, nil
}

func cloneGitSkill(rawURL, cacheDir string) (*ResolvedSkill, error) {
	// Clone into a staging dir beside the cache, then move the skill into
	// place so a failed clone never leaves partial state in the cache.
	tmpDir, cleanup, err := stagingDir(cacheDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()

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
		if info, err := os.Lstat(searchDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("skill from %s: subpath %q not found in repo", rawURL, subpath)
		}
	}

	resolved, err := stageSkill(searchDir, cacheDir)
	if err != nil {
		return nil, fmt.Errorf("skill from %s: %w", rawURL, err)
	}
	return resolved, nil
}

// findSkillDir returns the directory holding SKILL.md within a fetched tree:
// dir itself, or its single skill subdirectory.
func findSkillDir(dir string) (string, error) {
	if _, err := os.Lstat(filepath.Join(dir, skillFileName)); err == nil {
		return dir, nil
	}
	if sub, err := findCachedSkill(dir); err == nil {
		return sub, nil
	}
	return "", fmt.Errorf("no %s found", skillFileName)
}

// findCachedSkill returns the single subdirectory of dir that holds SKILL.md,
// which is the layout every cached skill has.
func findCachedSkill(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, e.Name(), skillFileName)); err == nil {
			return filepath.Join(dir, e.Name()), nil
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

	// Extract into a staging dir beside the cache, then move the skill into
	// place so a failed or truncated extraction never leaves partial state in
	// the cache.
	tmpDir, cleanup, err := stagingDir(cacheDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	lower := strings.ToLower(rawURL)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		if err := extractTarGz(resp.Body, tmpDir); err != nil {
			return nil, fmt.Errorf("extract %s: %w", rawURL, err)
		}
	case strings.HasSuffix(lower, ".zip"):
		if err := extractZipFromHTTP(resp.Body, tmpDir); err != nil {
			return nil, fmt.Errorf("extract %s: %w", rawURL, err)
		}
	default:
		return nil, fmt.Errorf("unsupported archive format: %s (expected .tar.gz, .tgz, or .zip)", rawURL)
	}

	resolved, err := stageSkill(tmpDir, cacheDir)
	if err != nil {
		return nil, fmt.Errorf("skill from %s: %w", rawURL, err)
	}
	return resolved, nil
}

// archiveBudget enforces the entry-count and total-size limits across one
// archive extraction.
type archiveBudget struct {
	entries int
	bytes   int64
}

func (b *archiveBudget) addEntry() error {
	b.entries++
	if b.entries > maxArchiveEntries {
		return fmt.Errorf("archive has more than %d entries", maxArchiveEntries)
	}
	return nil
}

func (b *archiveBudget) addBytes(n int64) error {
	b.bytes += n
	if b.bytes > maxArchiveTotalBytes {
		return fmt.Errorf("archive expands to more than %d bytes", maxArchiveTotalBytes)
	}
	return nil
}

// archiveTarget maps an archive entry name to a path under destDir, rejecting
// absolute names and names that climb out of destDir.
func archiveTarget(destDir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the extraction directory", name)
	}
	return filepath.Join(destDir, clean), nil
}

// writeArchiveFile writes one regular archive entry, failing rather than
// truncating when the entry is larger than declared or than allowed.
func writeArchiveFile(target string, mode os.FileMode, r io.Reader, budget *archiveBudget) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode&0755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxArchiveFileBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if n > maxArchiveFileBytes {
		return fmt.Errorf("archive entry %s is larger than %d bytes", filepath.Base(target), maxArchiveFileBytes)
	}
	return budget.addBytes(n)
}

func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	var budget archiveBudget
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := budget.addEntry(); err != nil {
			return err
		}
		target, err := archiveTarget(destDir, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size > maxArchiveFileBytes {
				return fmt.Errorf("archive entry %s is larger than %d bytes", header.Name, maxArchiveFileBytes)
			}
			if err := writeArchiveFile(target, os.FileMode(header.Mode), tr, &budget); err != nil {
				return err
			}
		default:
			// Links, devices, and other special entries are never materialized.
		}
	}
	return nil
}

func extractZipFromHTTP(r io.Reader, destDir string) error {
	// zip needs random access, so buffer to a temp file, bounded by the same
	// total the expanded tree may reach.
	tmp, err := os.CreateTemp("", "polly-skill-zip-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, io.LimitReader(r, maxArchiveTotalBytes+1))
	if err != nil {
		return err
	}
	if size > maxArchiveTotalBytes {
		return fmt.Errorf("archive is larger than %d bytes", maxArchiveTotalBytes)
	}

	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return err
	}

	var budget archiveBudget
	for _, f := range zr.File {
		if err := budget.addEntry(); err != nil {
			return err
		}
		target, err := archiveTarget(destDir, f.Name)
		if err != nil {
			return err
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			// Links and other special entries are never materialized.
			continue
		}
		if f.UncompressedSize64 > maxArchiveFileBytes {
			return fmt.Errorf("archive entry %s is larger than %d bytes", f.Name, maxArchiveFileBytes)
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeArchiveFile(target, f.Mode(), rc, &budget)
		rc.Close()
		if err != nil {
			return err
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
