package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// profileJudge decides which profile items may apply in a workspace. It is
// built from host facts that can change between sessions, such as the PATH,
// so the same rules judge an item when /sandbox allow adds it and at every
// start, and a hand-edited profile gets no more than a typed command.
type profileJudge struct {
	ws   sandboxWorkspace
	home string
	// protected is polly's own state, which no item reaches: its
	// configuration, sessions and profiles, its cache, and member scratch.
	protected []string
	// hostExec lists where the host runs code from, which no write reaches.
	hostExec []string
	// credentials is the credential deny list, expanded.
	credentials []string
}

func newProfileJudge(ws sandboxWorkspace) profileJudge {
	j := profileJudge{ws: ws, hostExec: sandbox.HostExecutionPaths()}
	if home, err := os.UserHomeDir(); err == nil {
		j.home = canonicalProfilePath(home)
		j.protected = append(j.protected, filepath.Join(home, userConfigDirName))
	}
	if cache, err := pollyCacheDir(); err == nil {
		j.protected = append(j.protected, cache)
	}
	j.protected = append(j.protected, scratch.Root())
	j.protected = append(j.protected, envstorage.PrivateRoots()...)
	for _, denied := range sandbox.ExpandHome(sandbox.DeniedPaths) {
		j.credentials = append(j.credentials, denied.Path)
	}
	return j
}

// check judges an item on its own: nil when it may apply, with whether it
// exposes a credential. A credential item is a read at or inside a masked
// credential path, or a passed-through variable.
func (j profileJudge) check(item sandboxProfileItem) (credential bool, err error) {
	if (item.Automatic || item.Managed) && item.Kind != profileEnv {
		return false, errors.New("automatic preparation can only set managed environment paths")
	}
	switch item.Kind {
	case profileRead:
		path := expandHomePath(item.Path)
		if err := j.checkPath(profileRead, path); err != nil {
			return false, err
		}
		return sandbox.ReadMasked(sandbox.Config{}, path) != nil, nil
	case profileWrite:
		return false, j.checkWrite(expandHomePath(item.Path))
	case profileEnv:
		if err := checkProfileEnvName(item.Name); err != nil {
			return false, err
		}
		_, err := j.itemEnvPath(item)
		return false, err
	case profilePassEnv:
		return true, checkProfilePassEnv(item.Name)
	}
	return false, fmt.Errorf("unknown item kind %q", item.Kind)
}

func (j profileJudge) itemEnvPath(item sandboxProfileItem) (string, error) {
	if item.managed() {
		return j.ws.storageRoots().Resolve(j.ws.storage.Active(), item.Value)
	}
	return j.envValuePath(item.Value)
}

// checkPath judges the path of a read or write item. It must be absolute;
// it may not be the filesystem root, the home directory or an ancestor of
// it, or reach polly's own state outside the workspace's cache directory;
// and it must lie outside the working directory, which the --sandbox preset
// already governs.
func (j profileJudge) checkPath(kind, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	for _, spelling := range pathSpellings(path) {
		if filepath.Dir(spelling) == spelling {
			return errors.New("it is the filesystem root")
		}
		if j.home != "" && sandbox.PathWithin(j.home, spelling) {
			return errors.New("it is the home directory or holds it; grant a directory inside it")
		}
	}
	if !pathWithinAny(path, j.ws.cache) {
		if ref, ok := overlapping(path, j.protected); ok {
			return fmt.Errorf("it reaches polly's own state in %s", homeRelativePath(ref))
		}
	}
	if pathWithinAny(path, j.ws.dir) {
		if kind == profileRead {
			return errors.New(profileAlreadyReadable)
		}
		return errors.New("it is inside the workspace, whose writes the --sandbox preset decides")
	}
	return nil
}

// checkWrite judges a write item: beyond checkPath, it may not reach a
// credential path, whose files name commands the host runs, a place the
// host runs code from, or Git metadata: the workspace's own, or any of
// another repository, since a hook written there runs the next time
// someone uses Git in it.
func (j profileJudge) checkWrite(path string) error {
	if err := j.checkPath(profileWrite, path); err != nil {
		return err
	}
	if ref, ok := overlapping(path, j.credentials); ok {
		return fmt.Errorf("it reaches the credential path %s, whose files name commands the host runs", homeRelativePath(ref))
	}
	if ref, ok := overlapping(path, j.hostExec); ok {
		return fmt.Errorf("it reaches %s, where the host runs code from", homeRelativePath(ref))
	}
	if ref, ok := overlapping(path, []string{j.ws.gitEntry, j.ws.commonDir}); ok {
		return fmt.Errorf("it reaches the workspace's Git metadata in %s", homeRelativePath(ref))
	}
	if checkout := gitCheckoutHolding(path); checkout != "" && !j.ownCheckout(checkout) {
		return fmt.Errorf("it is inside the Git repository %s", homeRelativePath(checkout))
	}
	return nil
}

// ownCheckout reports whether checkout, a directory with a .git entry, is
// the one the workspace is in.
func (j profileJudge) ownCheckout(checkout string) bool {
	return j.ws.gitEntry != "" && canonicalProfilePath(filepath.Join(checkout, ".git")) == canonicalProfilePath(j.ws.gitEntry)
}

// The bounds of the look for repositories inside a new write grant: three
// levels down, and at most this many directories.
const (
	profileScanDepth = 3
	profileScanLimit = 5000
)

// checkNewWrite is checkWrite for /sandbox allow, which also looks inside the
// directory for another repository. The look is bounded, so it runs only
// when the item is added, not at every start: a repository cloned into the
// directory later is not caught.
func (j profileJudge) checkNewWrite(path string) error {
	if err := j.checkWrite(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return nil
	}
	checkout, complete := repositoryInside(path)
	if checkout != "" {
		return fmt.Errorf("it holds the Git repository %s", homeRelativePath(checkout))
	}
	if !complete {
		return errors.New("it holds too many directories to rule out a Git repository inside it; grant a narrower directory")
	}
	return nil
}

// repositoryInside looks for a .git entry in dir and the directories below
// it, profileScanDepth levels down, and returns the directory holding the
// first one it finds. Symlinked directories are not followed. complete is
// false when the look gave up at profileScanLimit directories.
func repositoryInside(dir string) (checkout string, complete bool) {
	level := []string{dir}
	seen := 0
	for depth := 0; depth <= profileScanDepth && len(level) > 0; depth++ {
		var next []string
		for _, parent := range level {
			entries, err := os.ReadDir(parent)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if strings.EqualFold(entry.Name(), ".git") {
					return parent, true
				}
			}
			if depth == profileScanDepth {
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				if seen++; seen > profileScanLimit {
					return "", false
				}
				next = append(next, filepath.Join(parent, entry.Name()))
			}
		}
		level = next
	}
	return "", true
}

// gitCheckoutHolding returns the nearest directory at or above path, on
// either spelling of it, that has a .git entry, or "" when there is none.
func gitCheckoutHolding(path string) string {
	for _, spelling := range pathSpellings(path) {
		for dir := spelling; ; dir = filepath.Dir(dir) {
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				return dir
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return ""
}

// envValuePath returns the path an env item's value names: @workspace or
// @cache, optionally followed by a relative path that stays inside it.
func (j profileJudge) envValuePath(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") || len(value) > 1024 {
		return "", errors.New("the value must be one line of at most 1024 bytes")
	}
	for _, root := range []struct{ name, path string }{{profileWorkspaceVar, j.ws.dir}, {profileCacheVar, j.ws.cache}} {
		if value == root.name {
			return root.path, nil
		}
		rest, ok := strings.CutPrefix(value, root.name+"/")
		if !ok {
			continue
		}
		rel := filepath.Clean(filepath.FromSlash(rest))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("the value %s leaves %s", value, root.name)
		}
		return filepath.Join(root.path, rel), nil
	}
	return "", fmt.Errorf("the value %s must start with %s or %s", value, profileWorkspaceVar, profileCacheVar)
}

var profileEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkProfileEnvName refuses the variables an env item may not set: those
// that set the shell's own environment or the sandbox's temp directory, run
// startup code or load code into programs that did not ask for it, change
// how Git, SSH or GPG run, point at a host service, or move every program's
// configuration, plus polly's own and credential-shaped names, since an env
// value is configuration, never a secret.
func checkProfileEnvName(name string) error {
	if !profileEnvNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a variable name", name)
	}
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "POLLYTOOL_") {
		return errors.New("it is polly's own configuration")
	}
	if sandbox.SensitiveEnvName(name) {
		return errors.New("it is credential-shaped; pass the variable itself with /sandbox allow passenv")
	}
	for _, rule := range refusedProfileEnv {
		for _, pattern := range rule.names {
			prefix, isPrefix := strings.CutSuffix(pattern, "*")
			if upper == pattern || isPrefix && strings.HasPrefix(upper, prefix) {
				return errors.New(rule.reason)
			}
		}
	}
	return nil
}

var refusedProfileEnv = []struct {
	reason string
	names  []string
}{
	{"it sets the shell's own environment", []string{"PATH", "HOME", "SHELL", "USER", "LOGNAME", "PWD", "OLDPWD", "IFS", "CDPATH"}},
	{"it is the temp directory the sandbox sets", []string{"TMPDIR", "TMP", "TEMP"}},
	{"it runs shell startup code", []string{"ENV", "BASH_ENV", "ZDOTDIR", "PROMPT_COMMAND", "SHELLOPTS", "BASHOPTS", "PS4"}},
	{"it loads code into programs that did not ask for it", []string{"LD_*", "DYLD_*", "NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONSTARTUP", "PYTHONHOME", "PERL5LIB", "PERL5OPT", "PERLLIB", "RUBYOPT", "RUBYLIB", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS"}},
	{"it changes how Git, SSH or GPG run", []string{"GIT_*", "SSH_*", "GPG_*", "GNUPGHOME"}},
	{"it points at a host service", []string{"DOCKER_*", "DBUS_*", "CONTAINER_HOST", "XDG_RUNTIME_DIR"}},
	{"it moves every program's configuration", []string{"XDG_CONFIG_HOME"}},
}

// profileSocketEnv are the stripped variables that point at a socket or a
// host service rather than hold a secret; a profile grants no sockets, and
// the ssh preset is how the agent reaches the sandbox.
var profileSocketEnv = map[string]bool{
	"SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "GPG_AGENT_INFO": true,
	"DBUS_SESSION_BUS_ADDRESS": true, "DBUS_SYSTEM_BUS_ADDRESS": true,
	"DOCKER_HOST": true, "CONTAINER_HOST": true, "XDG_RUNTIME_DIR": true,
	"WAYLAND_DISPLAY": true, "PULSE_SERVER": true,
}

// checkProfilePassEnv refuses a passenv item that would pass polly's own
// configuration or a socket address, or a variable the sandbox does not
// strip, which sandboxed commands see already.
func checkProfilePassEnv(name string) error {
	if !profileEnvNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a variable name", name)
	}
	upper := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(upper, "POLLYTOOL_"):
		return errors.New("it is polly's own configuration, which never reaches sandboxed commands")
	case profileSocketEnv[upper]:
		return errors.New("it points at a socket or host service, which a profile does not grant")
	case !sandbox.SensitiveEnvName(name):
		return errors.New("the sandbox does not strip it, so commands see it already")
	}
	return nil
}

// pathSpellings returns path cleaned and, when it differs, its canonical
// spelling, so a rule holds whichever route a grant takes.
func pathSpellings(path string) []string {
	path = filepath.Clean(path)
	if canonical := canonicalProfilePath(path); canonical != path {
		return []string{path, canonical}
	}
	return []string{path}
}

// pathWithinAny reports whether some spelling of path is at or inside some
// spelling of ref.
func pathWithinAny(path, ref string) bool {
	for _, p := range pathSpellings(path) {
		for _, r := range pathSpellings(ref) {
			if sandbox.PathWithin(p, r) {
				return true
			}
		}
	}
	return false
}

// overlapping returns the first of refs that path is at, inside or holds,
// on any spelling of either.
func overlapping(path string, refs []string) (string, bool) {
	for _, ref := range refs {
		if ref != "" && (pathWithinAny(path, ref) || pathWithinAny(ref, path)) {
			return ref, true
		}
	}
	return "", false
}
