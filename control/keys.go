package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Protocol is the number of the control contract this package speaks.
const Protocol = 1

// Program is a backend binary as its keys know it: its real path, its size,
// and the time it last changed, in Unix nanoseconds. Two builds are two
// files, or one file at two moments.
type Program struct {
	Path    string `json:"path"`
	Size    uint64 `json:"size"`
	Changed uint64 `json:"changed"`
}

// ProgramOf reads the identity of the binary at path, following links to
// its real file.
func ProgramOf(path string) (Program, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Program{}, fmt.Errorf("resolve backend binary %s: %w", path, err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return Program{}, fmt.Errorf("resolve backend binary %s: %w", path, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return Program{}, fmt.Errorf("read backend binary %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Program{}, fmt.Errorf("backend binary %s is not a regular file", path)
	}
	return Program{Path: real, Size: uint64(info.Size()), Changed: uint64(info.ModTime().UnixNano())}, nil
}

// versions caches what each build said its version was: a build is one
// file at one moment, and says the same every time.
var versions sync.Map // Program -> string

// Version asks the binary for its version: the first line of `<bin>
// --version`, less its leading program name, which is what the binary
// hashes into its keys. A build is asked once per process.
func Version(ctx context.Context, program Program) (string, error) {
	if v, ok := versions.Load(program); ok {
		return v.(string), nil
	}
	v, err := askVersion(ctx, program)
	if err == nil {
		versions.Store(program, v)
	}
	return v, err
}

func askVersion(ctx context.Context, program Program) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program.Path, "--version")
	cmd.Env = withoutProviderKeys(os.Environ())
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ask %s for its version: %w", program.Path, err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	line = strings.TrimRight(line, "\r")
	_, describe, ok := strings.Cut(line, " ")
	if !ok || strings.TrimSpace(describe) == "" {
		return "", fmt.Errorf("%s --version printed no version: %q", program.Path, line)
	}
	return describe, nil
}

// BuildKey is the key of a backend build: every setup run by the same
// binary shares one daemon, at `<protocol>-<key>.sock`.
func BuildKey(version string, program Program) string {
	h := NewBlake3()
	h.Write([]byte("build\x00"))
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], Protocol)
	h.Write(u32[:])
	h.Write([]byte(version))
	h.Write([]byte{0})
	h.Write([]byte(program.Path))
	h.Write([]byte{0})
	var u64 [8]byte
	binary.LittleEndian.PutUint64(u64[:], program.Size)
	h.Write(u64[:])
	// The change time crosses as a 128-bit little-endian integer: the low
	// eight bytes are the time, and the high eight its (zero) extension.
	var i128 [16]byte
	binary.LittleEndian.PutUint64(i128[:8], program.Changed)
	h.Write(i128[:])
	sum := h.Sum32()
	return hex.EncodeToString(sum[:16])
}

// Place is where skills are read from, as a setup names it.
type Place struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Boundary string `json:"boundary"`
}

// Skill place kinds.
const (
	PlaceNamed    = "named"
	PlacePersonal = "personal"
	PlaceProject  = "project"
)

// SkillManifest is the file a skill directory holds.
const SkillManifest = "SKILL.md"

// Places finds where a launch working in workDir reads skills: the named
// directories, each once, or else the personal directory and then the
// project's from the repository root down to workDir.
func Places(workDir, home, personalDir string, named []string) ([]Place, error) {
	work, err := resolvePath(home, "", workDir)
	if err != nil {
		return nil, fmt.Errorf("skill working directory: %w", err)
	}
	if len(named) > 0 {
		seen := map[string]bool{}
		var out []Place
		for _, raw := range named {
			path, err := resolvePath(home, work, raw)
			if err != nil {
				return nil, fmt.Errorf("skill directory %q: %w", raw, err)
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, Place{Path: path, Kind: PlaceNamed})
		}
		return out, nil
	}
	var out []Place
	if personalDir != "" {
		path, err := resolvePath(home, work, personalDir)
		if err != nil {
			return nil, fmt.Errorf("personal skill directory: %w", err)
		}
		out = append(out, Place{Path: path, Kind: PlacePersonal})
	}
	scopes := repositoryScopeDirs(work)
	boundary, err := filepath.EvalSymlinks(scopes[0])
	if err != nil {
		return nil, fmt.Errorf("project skill boundary does not exist: %w", err)
	}
	for _, dir := range scopes {
		out = append(out, Place{Path: filepath.Join(dir, ".agents", "skills"), Kind: PlaceProject, Boundary: boundary})
	}
	return out, nil
}

// repositoryScopeDirs lists dir and its ancestors up to the nearest one that
// holds `.git`, root first; or dir alone when none does.
func repositoryScopeDirs(dir string) []string {
	dirs := []string{dir}
	for at := dir; ; {
		if info, err := os.Lstat(filepath.Join(at, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			slices.Reverse(dirs)
			return dirs
		}
		parent := filepath.Dir(at)
		if parent == at {
			return dirs[:1]
		}
		dirs = append(dirs, parent)
		at = parent
	}
}

// resolvePath expands a leading ~ against home and makes path absolute
// against workDir, cleaned.
func resolvePath(home, workDir, path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("invalid path")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			return "", errors.New("no home directory")
		}
		path = filepath.Join(home, strings.TrimLeft(path[1:], "/"))
	} else if strings.HasPrefix(path, "~") {
		return "", errors.New("only ~ and ~/... home expansion are supported")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if workDir == "" {
		return "", errors.New("working directory must be absolute")
	}
	return filepath.Join(workDir, path), nil
}

// SetupKeyInput is a setup as a launch has resolved it: what two launches
// must have alike to share a worker.
type SetupKeyInput struct {
	// Version and Program are the backend build's; both empty for a daemon
	// reached by its socket alone, whose build is not known here.
	Version    string
	Program    Program
	RuntimeDir string
	Provider   string
	BaseURL    string
	Natives    bool
	Servers    []string
	Scripts    []string
	Dir        string
	Home       string
	Skills     []Place
	Environ    []string
	// Workspace is where the tools work (`ssh:HOST[:DIR]`, ...), or empty
	// for here; SkillDirs are the named skill directories for such a setup.
	Workspace string
	SkillDirs []string
}

// maxSourceBytes bounds a source file read for its hash.
const maxSourceBytes = 16 << 20

// SetupKey is the key of a setup, computed as the backend computes it so
// that a launch of either finds the other's worker.
func SetupKey(s SetupKeyInput) (string, error) {
	w := newKeyWriting()
	w.count("protocol", Protocol)
	w.put("version", s.Version)
	w.put("program", s.Program.Path)
	w.count("program_size", s.Program.Size)
	w.count("program_changed", s.Program.Changed)
	w.put("runtime", realOr(s.RuntimeDir))
	provider := s.Provider
	if len(provider) > 64 {
		provider = provider[:64]
	}
	w.put("provider", strings.ToLower(provider))
	w.put("base_url", s.BaseURL)
	env := environMap(s.Environ)
	keyVar := ProviderKeyVar(s.Provider)
	if keyVar != "" {
		w.secret("provider_key", env[keyVar])
	} else {
		w.secret("provider_key", "")
	}
	w.flag("natives", s.Natives)
	w.put("workspace", s.Workspace)
	if s.Workspace != "" {
		w.count("servers", uint64(len(s.Servers)))
		for _, named := range s.Servers {
			w.put("server", named)
		}
		w.count("scripts", uint64(len(s.Scripts)))
		for _, named := range s.Scripts {
			w.put("script", named)
		}
		w.put("dir", s.Dir)
		w.count("skill_dirs", uint64(len(s.SkillDirs)))
		for _, named := range s.SkillDirs {
			w.put("skill_dir", named)
		}
		w.count("skills", uint64(len(s.Skills)))
		for _, place := range s.Skills {
			w.put("place", place.Path)
			w.put("place_kind", place.Kind)
			w.put("place_boundary", place.Boundary)
		}
		w.put("home", s.Home)
		w.environment(s.Environ)
		return w.key(), nil
	}
	w.count("servers", uint64(len(s.Servers)))
	for _, named := range s.Servers {
		file, server, _ := strings.Cut(named, "#")
		path, err := resolvePath(s.Home, s.Dir, file)
		if err != nil {
			return "", fmt.Errorf("%s is no path", named)
		}
		data, err := readSource(path)
		if err != nil {
			return "", err
		}
		w.put("server", realOr(path))
		w.put("server_name", server)
		w.bytes("server_bytes", data)
	}
	w.count("scripts", uint64(len(s.Scripts)))
	for _, named := range s.Scripts {
		path, err := resolvePath(s.Home, s.Dir, named)
		if err != nil {
			return "", fmt.Errorf("%s is no path", named)
		}
		data, err := readSource(path)
		if err != nil {
			return "", err
		}
		w.put("script", realOr(path))
		w.bytes("script_bytes", data)
	}
	if len(s.Servers) > 0 || len(s.Scripts) > 0 {
		w.put("dir", realOr(s.Dir))
	}
	if err := w.skills(s.Skills); err != nil {
		return "", err
	}
	w.put("home", s.Home)
	w.environment(s.Environ)
	return w.key(), nil
}

func readSource(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(bufio.NewReader(f)); err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if buf.Len() > maxSourceBytes {
		return nil, fmt.Errorf("cannot read %s: larger than %d bytes", path, maxSourceBytes)
	}
	return buf.Bytes(), nil
}

// realOr is the real path of what is at an absolute path, or the path as it
// is when nothing is there yet.
func realOr(path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// keyWriting writes each thing into the hash as its name's length, its
// name, its own length and itself.
type keyWriting struct{ h *Blake3 }

func newKeyWriting() *keyWriting { return &keyWriting{h: NewBlake3()} }

func (w *keyWriting) put(name, value string) { w.putBytes(name, []byte(value)) }

func (w *keyWriting) putBytes(name string, value []byte) {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(name)))
	w.h.Write(n[:])
	w.h.Write([]byte(name))
	binary.LittleEndian.PutUint64(n[:], uint64(len(value)))
	w.h.Write(n[:])
	w.h.Write(value)
}

func (w *keyWriting) count(name string, n uint64) {
	var value [8]byte
	binary.LittleEndian.PutUint64(value[:], n)
	w.putBytes(name, value[:])
}

func (w *keyWriting) flag(name string, on bool) {
	if on {
		w.put(name, "1")
	} else {
		w.put(name, "0")
	}
}

func (w *keyWriting) bytes(name string, value []byte) {
	sum := Blake3Sum(value)
	w.putBytes(name, sum[:])
}

func (w *keyWriting) secret(name, value string) {
	if value == "" {
		w.put(name, "")
		return
	}
	inner := NewBlake3()
	inner.Write([]byte("polly-zig setup key, a secret: "))
	inner.Write([]byte(value))
	sum := inner.Sum32()
	w.putBytes(name, sum[:])
}

func (w *keyWriting) key() string {
	sum := w.h.Sum32()
	return hex.EncodeToString(sum[:16])
}

type skillManifest struct {
	name    string
	size    uint64
	changed uint64
}

func (w *keyWriting) skills(places []Place) error {
	type holding struct {
		place     Place
		real      string
		manifests []skillManifest
	}
	var held []holding
	for _, place := range places {
		found, err := placeManifests(place.Path)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			continue
		}
		held = append(held, holding{place: place, real: realOr(place.Path), manifests: found})
	}
	w.count("places", uint64(len(held)))
	for _, h := range held {
		w.put("place_kind", h.place.Kind)
		w.put("place_bound", h.place.Boundary)
		w.put("place", h.real)
		w.count("manifests", uint64(len(h.manifests)))
		for _, m := range h.manifests {
			w.put("manifest", m.name)
			w.count("manifest_size", m.size)
			w.count("manifest_changed", m.changed)
		}
	}
	return nil
}

// placeManifests lists the manifests a place holds: its own, or one in each
// directory in it that has one, by name. None for a place that is not there.
func placeManifests(dir string) ([]skillManifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot open the skill directory %s: %w", dir, err)
	}
	if info, err := os.Stat(filepath.Join(dir, SkillManifest)); err == nil {
		return []skillManifest{{name: SkillManifest, size: uint64(info.Size()), changed: uint64(info.ModTime().UnixNano())}}, nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	var found []skillManifest
	for _, name := range names {
		inside := filepath.Join(name, SkillManifest)
		info, err := os.Stat(filepath.Join(dir, inside))
		if err != nil {
			continue
		}
		found = append(found, skillManifest{name: inside, size: uint64(info.Size()), changed: uint64(info.ModTime().UnixNano())})
	}
	return found, nil
}

func (w *keyWriting) environment(environ []string) {
	env := environMap(environ)
	var names []string
	for name := range env {
		if IsProviderKeyVar(name) {
			continue
		}
		if !IsSetupEnv(name) && !isExtraSetupEnv(env, name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	w.count("variables", uint64(len(names)))
	for _, name := range names {
		w.put("variable", name)
		w.put("value", env[name])
	}
}

// environMap reads NAME=VALUE pairs, the last of a name winning.
func environMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		out[name] = value
	}
	return out
}
