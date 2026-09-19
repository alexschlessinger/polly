package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// A workspace sandbox profile holds the user's standing sandbox exceptions
// for one workspace: a repository with every worktree and subdirectory of
// it, or a directory outside Git. It lives outside the repository, at
// ~/.pollytool/workspaces/<key>/sandbox.json, where no sandboxed command can
// reach it, and only /sandbox and the user's own editor write it. At every
// start polly reads it, judges each item again, and applies the items that
// pass as the workspace-profile sandbox layer.
const (
	sandboxProfileLayer   = "workspace-profile"
	sandboxProfileVersion = 1
	sandboxProfileMaxSize = 256 << 10
)

// The item kinds a profile holds, all generic: polly knows the mechanisms,
// never an ecosystem.
const (
	profileRead    = "read"
	profileWrite   = "write"
	profileEnv     = "env"
	profilePassEnv = "passenv"
)

// The placeholders an env item's value starts with: the working directory
// polly runs in, and the workspace's own cache directory.
const (
	profileWorkspaceVar = "@workspace"
	profileCacheVar     = "@cache"
)

// sandboxProfile is the profile file's content.
type sandboxProfile struct {
	Version int `json:"version"`
	// Workspace names the directory the key hashes, the repository's common
	// Git directory or the working directory outside Git, for the reader.
	Workspace string               `json:"workspace"`
	Items     []sandboxProfileItem `json:"items"`
}

// sandboxProfileItem is one exception. Read and write items name a Path, an
// env item a Name and a Value starting with @workspace or @cache, and a
// passenv item the Name of a variable the sandbox otherwise strips.
type sandboxProfileItem struct {
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
	// Members lets a passenv item reach swarm members too; every other kind
	// reaches them always.
	Members bool `json:"members,omitempty"`
	// Credential records that the item was allowed as a credential: a read
	// at or inside a masked credential path, or a passenv item. An item that
	// exposes a credential without it does not apply, so a directory a
	// sandboxed command later swaps for a link to a credential never
	// becomes one.
	Credential bool `json:"credential,omitempty"`
	// Origin is the workspace's origin remote when a credential item was
	// allowed, empty when it had none; the item applies only while the
	// origin is the same, so a different repository cloned to the same path
	// does not inherit a credential.
	Origin string `json:"origin,omitempty"`
}

// String spells the item the way /sandbox allow takes it.
func (item sandboxProfileItem) String() string {
	switch item.Kind {
	case profileRead, profileWrite:
		return item.Kind + " " + item.Path
	case profileEnv:
		return item.Kind + " " + item.Name + "=" + item.Value
	case profilePassEnv:
		if item.Members {
			return item.Kind + " " + item.Name + " --members"
		}
		return item.Kind + " " + item.Name
	}
	return item.Kind
}

// sameSandboxProfileItem reports whether two items grant the same thing, so
// allowing one again replaces the other: the same path for read and write,
// the same variable for env and passenv.
func sameSandboxProfileItem(a, b sandboxProfileItem) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case profileRead, profileWrite:
		return filepath.Clean(expandHomePath(a.Path)) == filepath.Clean(expandHomePath(b.Path))
	}
	return a.Name == b.Name
}

// sandboxWorkspace identifies the workspace a profile belongs to and where
// its files live.
type sandboxWorkspace struct {
	// dir is the canonical working directory, what @workspace names.
	dir string
	// commonDir is the repository's canonical common Git directory, empty
	// outside Git; gitEntry is the .git entry of the checkout holding dir.
	commonDir, gitEntry string
	// origin is the URL of the repository's origin remote, empty when none.
	origin string
	key    string
	// profile is the profile file, cache the workspace's cache directory,
	// what @cache names.
	profile, cache string
}

// resolveSandboxWorkspace identifies the workspace dir belongs to. The key
// hashes the repository's common Git directory, found without running Git,
// so every worktree and subdirectory of a repository shares one profile; a
// directory outside Git is its own workspace.
func resolveSandboxWorkspace(dir string) (sandboxWorkspace, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return sandboxWorkspace{}, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return sandboxWorkspace{}, err
	}
	common, err := sandbox.GitCommonDir(dir)
	if err != nil {
		return sandboxWorkspace{}, fmt.Errorf("find the workspace's Git directory: %w", err)
	}
	identity := "dir:" + dir
	ws := sandboxWorkspace{dir: dir, commonDir: common}
	if common != "" {
		identity = "gitdir:" + common
		ws.gitEntry = gitEntryAbove(dir)
		ws.origin = gitOriginURL(common)
	}
	sum := sha256.Sum256([]byte(identity))
	ws.key = hex.EncodeToString(sum[:16])
	home, err := os.UserHomeDir()
	if err != nil {
		return sandboxWorkspace{}, fmt.Errorf("resolve home directory: %w", err)
	}
	ws.profile = filepath.Join(home, userConfigDirName, "workspaces", ws.key, "sandbox.json")
	cache, err := pollyCacheDir()
	if err != nil {
		return sandboxWorkspace{}, err
	}
	ws.cache = canonicalProfilePath(filepath.Join(cache, "ws", ws.key))
	return ws, nil
}

// gitEntryAbove returns the .git entry of the checkout holding dir, or ""
// when there is none.
func gitEntryAbove(dir string) string {
	for path := dir; ; path = filepath.Dir(path) {
		entry := filepath.Join(path, ".git")
		if _, err := os.Lstat(entry); err == nil {
			return entry
		}
		if filepath.Dir(path) == path {
			return ""
		}
	}
}

// gitOriginURL returns the url of the origin remote in a repository's config
// file, read without running Git, or "" when there is none. Includes are not
// followed: the value only tells one repository from another.
func gitOriginURL(commonDir string) string {
	f, err := os.Open(filepath.Join(commonDir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()
	origin := false
	scanner := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			header, _, _ := strings.Cut(strings.TrimPrefix(line, "["), "]")
			name, sub, _ := strings.Cut(strings.TrimSpace(header), " ")
			origin = strings.EqualFold(name, "remote") && strings.TrimSpace(sub) == `"origin"`
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if origin && ok && strings.EqualFold(strings.TrimSpace(key), "url") {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

// pollyCacheDir is polly's directory in the user cache: $XDG_CACHE_HOME, or
// the platform's cache directory, then pollytool.
func pollyCacheDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "pollytool"), nil
}

// canonicalProfilePath resolves the symlinks in the existing part of path,
// so a path compares to the canonical routes the sandbox freezes.
func canonicalProfilePath(path string) string {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(real)
	}
	if real, err := sandbox.ResolveExistingPathPrefix(path); err == nil {
		return filepath.Clean(real)
	}
	return path
}

// expandHomePath expands a leading ~ to the home directory.
func expandHomePath(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

// readSandboxProfile reads a profile file. A missing file is an empty
// profile. The file must be a regular file, never a symlink, owned by the
// user and writable by no one else, and at most sandboxProfileMaxSize; its
// directory must be the user's own too. Unknown fields and other versions
// are refused, so a profile a later polly wrote never half-applies.
func readSandboxProfile(path string) (sandboxProfile, error) {
	empty := sandboxProfile{Version: sandboxProfileVersion}
	dirInfo, err := os.Lstat(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return sandboxProfile{}, err
	}
	if !dirInfo.IsDir() {
		return sandboxProfile{}, fmt.Errorf("%s is not a directory", filepath.Dir(path))
	}
	if err := ownedByUserAlone(dirInfo); err != nil {
		return sandboxProfile{}, fmt.Errorf("%s %w", filepath.Dir(path), err)
	}
	f, err := safefile.OpenRegular(path, os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return sandboxProfile{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sandboxProfile{}, err
	}
	if err := ownedByUserAlone(info); err != nil {
		return sandboxProfile{}, fmt.Errorf("%s %w", path, err)
	}
	if info.Size() > sandboxProfileMaxSize {
		return sandboxProfile{}, fmt.Errorf("%s is larger than %d KiB", path, sandboxProfileMaxSize>>10)
	}
	data, err := io.ReadAll(io.LimitReader(f, sandboxProfileMaxSize+1))
	if err != nil {
		return sandboxProfile{}, err
	}
	if len(data) > sandboxProfileMaxSize {
		return sandboxProfile{}, fmt.Errorf("%s is larger than %d KiB", path, sandboxProfileMaxSize>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var profile sandboxProfile
	if err := dec.Decode(&profile); err != nil {
		return sandboxProfile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if dec.More() {
		return sandboxProfile{}, fmt.Errorf("parse %s: data after the profile", path)
	}
	if profile.Version != sandboxProfileVersion {
		return sandboxProfile{}, fmt.Errorf("%s has version %d; this polly reads version %d", path, profile.Version, sandboxProfileVersion)
	}
	return profile, nil
}

// writeSandboxProfile replaces the profile file atomically, creating its
// directory 0700 and the file 0600. A profile with no items removes the
// file.
func writeSandboxProfile(path string, profile sandboxProfile) error {
	if len(profile.Items) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		_ = os.Remove(filepath.Dir(path))
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := ownedByUserAlone(info); err != nil {
		return fmt.Errorf("%s %w", dir, err)
	}
	profile.Version = sandboxProfileVersion
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sandbox-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
