// Package envstorage manages only directories created for a sandbox environment.
// Declarations contain names, never caller-selected host paths.
package envstorage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/safefile"
)

type Allocation struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // cache, state, config
	Purpose  string `json:"purpose"`
	Recipe   string `json:"recipe,omitempty"`
	Shared   bool   `json:"shared,omitempty"`
	Disabled bool   `json:"disabled,omitempty"` // retained for cleanup after /sandbox forget
}

type Link struct {
	Path      string `json:"path"`
	Target    string `json:"target"`
	Directory bool   `json:"directory,omitempty"`
}

type Spec struct {
	Allocations []Allocation `json:"allocations,omitempty"`
	Links       []Link       `json:"links,omitempty"`
}

func (s Spec) Clone() Spec {
	return Spec{Allocations: slices.Clone(s.Allocations), Links: slices.Clone(s.Links)}
}

func (s Spec) Active() Spec {
	active := s.Clone()
	active.Allocations = slices.DeleteFunc(active.Allocations, func(a Allocation) bool { return a.Disabled })
	active.Links = slices.DeleteFunc(active.Links, func(l Link) bool {
		_, _, a := active.Lookup(l.Path)
		_, _, b := active.Lookup(l.Target)
		return a != nil || b != nil
	})
	return active
}

type Roots struct{ Cache, SharedCache, State, Config, Control string }

func CheckoutKey(checkout string) string {
	h := sha256.Sum256([]byte(checkout))
	return hex.EncodeToString(h[:16])
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func (a Allocation) Key() string { return "@" + a.Kind + "/" + a.Name }

func (s Spec) Validate() error {
	if len(s.Allocations) > 64 || len(s.Links) > 64 {
		return errors.New("an environment supports at most 64 allocations and 64 links")
	}
	seen := map[string]bool{}
	for _, a := range s.Allocations {
		if !validName.MatchString(a.Name) || (a.Kind != "cache" && a.Kind != "state" && a.Kind != "config") {
			return fmt.Errorf("invalid allocation %q", a.Key())
		}
		if a.Shared && a.Kind != "cache" {
			return fmt.Errorf("only disposable caches can be shared: %s", a.Key())
		}
		if a.Purpose == "" || len(a.Purpose) > 256 || len(a.Recipe) > 128 || strings.ContainsAny(a.Purpose+a.Recipe, "\x00\r\n") {
			return fmt.Errorf("invalid purpose or recipe for %s", a.Key())
		}
		if seen[a.Key()] {
			return fmt.Errorf("duplicate allocation %s", a.Key())
		}
		seen[a.Key()] = true
	}
	links := map[string]bool{}
	for _, l := range s.Links {
		a, rel, err := s.Lookup(l.Path)
		if err != nil {
			return err
		}
		b, _, err := s.Lookup(l.Target)
		if err != nil {
			return err
		}
		if a.Kind != "state" || rel == "" || b.Kind != "config" {
			return errors.New("configuration links must join a child of @state to @config")
		}
		if links[l.Path] {
			return fmt.Errorf("duplicate link %s", l.Path)
		}
		links[l.Path] = true
	}
	return nil
}

func (s Spec) Lookup(ref string) (Allocation, string, error) {
	parts := strings.SplitN(ref, "/", 3)
	if len(parts) < 2 {
		return Allocation{}, "", fmt.Errorf("%q must name a managed allocation", ref)
	}
	rel := ""
	if len(parts) == 3 {
		rel = parts[2]
		if !fs.ValidPath(rel) || strings.ContainsAny(rel, "\\\x00\r\n") {
			return Allocation{}, "", fmt.Errorf("invalid managed path %q", ref)
		}
	}
	for _, a := range s.Allocations {
		if a.Key() == parts[0]+"/"+parts[1] {
			return a, rel, nil
		}
	}
	return Allocation{}, "", fmt.Errorf("%q names an undeclared allocation", ref)
}

func (r Roots) Path(a Allocation) string {
	base := r.State
	switch a.Kind {
	case "cache":
		base = r.Cache
		if a.Shared {
			base = r.SharedCache
		}
	case "config":
		base = r.Config
	}
	return filepath.Join(base, a.Name)
}

func (r Roots) Resolve(s Spec, ref string) (string, error) {
	a, rel, err := s.Lookup(ref)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.Path(a), filepath.FromSlash(rel)), nil
}

// EnsureDir refuses symlink components and directories writable by other users.
// Canonicalize the trusted platform base before choosing the managed roots.
func EnsureDir(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("managed storage requires an absolute path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := EnsureDir(filepath.Dir(path)); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("managed directory %s is not a directory", path)
	}
	// Existing ancestors may be system-owned; the managed directory itself is
	// checked for exclusive ownership by callers below.
	real, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(real) != filepath.Clean(path) {
		return fmt.Errorf("managed directory %s has a symlink component", path)
	}
	return nil
}

func privateDir(path string) error {
	if err := EnsureDir(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return owned(info)
}

type receipt struct{ Path, Identity string }

func (r Roots) receipt(path string) string {
	h := sha256.Sum256([]byte(path))
	return filepath.Join(r.Control, "owners", hex.EncodeToString(h[:16])+".json")
}

func (r Roots) check(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return r.checkInfo(path, info)
}

func (r Roots) checkInfo(path string, info fs.FileInfo) error {
	f, err := safefile.OpenRegular(r.receipt(path), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	recordInfo, err := f.Stat()
	if err != nil {
		return err
	}
	if err := owned(recordInfo); err != nil {
		return err
	}
	var rec receipt
	if err := json.NewDecoder(f).Decode(&rec); err != nil {
		return err
	}
	if !info.IsDir() || rec.Path != path || rec.Identity != identity(info) {
		return fmt.Errorf("managed storage was replaced: %s", path)
	}
	if err := owned(info); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != path {
		return fmt.Errorf("managed storage was redirected: %s", path)
	}
	return nil
}

func (r Roots) create(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return r.check(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	// A failed receipt write must not strand an unowned allocation. Remove
	// only the newly created, empty directory on this error path.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(path)
		}
	}()
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(receipt{Path: path, Identity: identity(info)})
	if err := privateDir(filepath.Dir(r.receipt(path))); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.receipt(path)), ".owner-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), r.receipt(path)); err != nil {
		return err
	}
	committed = true
	return nil
}

func (r Roots) Ensure(s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if len(s.Allocations) == 0 {
		return nil
	}
	unlock, err := Lock(filepath.Join(r.Control, "storage.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	for _, a := range s.Allocations {
		if err := r.create(r.Path(a)); err != nil {
			return fmt.Errorf("%s: %w", a.Key(), err)
		}
	}
	return r.links(s)
}

func (r Roots) links(s Spec) error {
	for _, l := range s.Links {
		if err := r.link(s, l); err != nil {
			return err
		}
	}
	return nil
}

func (r Roots) link(s Spec, l Link) error {
	a, path, _ := s.Lookup(l.Path)
	b, target, _ := s.Lookup(l.Target)
	source, err := r.Open(a)
	if err != nil {
		return err
	}
	defer source.Close()
	dest, err := r.Open(b)
	if err != nil {
		return err
	}
	defer dest.Close()
	if err := source.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if target != "" {
		parent := filepath.Dir(target)
		if l.Directory {
			parent = target
		}
		if err := dest.MkdirAll(parent, 0700); err != nil {
			return err
		}
	}
	absolute := filepath.Join(r.Path(b), target)
	if info, err := source.Lstat(path); err == nil {
		got, e := source.Readlink(path)
		if info.Mode()&os.ModeSymlink == 0 || e != nil || got != absolute {
			return fmt.Errorf("configuration link was replaced: %s", l.Path)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return source.Symlink(absolute, path)
}

// Clean requires the caller's exclusive environment lease. Receipts survive
// interrupted deletion. No workspace files or unclassified directories enter it.
func (r Roots) Clean(ctx context.Context, s Spec, state bool) error {
	if err := s.Validate(); err != nil {
		return err
	}
	unlock, err := LockContext(ctx, filepath.Join(r.Control, "storage.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	for _, a := range s.Allocations {
		if a.Kind == "config" || a.Kind == "state" && !state {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path := r.Path(a)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := r.check(path); err != nil {
			return err
		}
		if err := r.emptyOwnedDirectory(ctx, path); err != nil {
			return err
		}
	}
	for _, a := range s.Allocations {
		if err := r.create(r.Path(a)); err != nil {
			return err
		}
	}
	return r.links(s)
}

// Keep the allocation root (and its frozen sandbox identity) intact. All
// deletion and permission repair is relative to an opened, checked directory;
// symbolic links are removed as entries and never traversed.
func (r Roots) emptyOwnedDirectory(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return fmt.Errorf("storage changed during cleanup: %s", path)
	}
	if err := r.checkInfo(path, opened); err != nil {
		return err
	}
	var dirs []string
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, name)
			// Pin a directory before repairing permissions. A concurrent
			// replacement with a hard-linked file must never chmod that file.
			dir, err := root.OpenRoot(name)
			if err != nil {
				return err
			}
			defer dir.Close()
			return dir.Chmod(".", 0700)
		}
		return root.Remove(name)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		name := dirs[i]
		if name == "." {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

func (r Roots) Size(ctx context.Context, a Allocation) (int64, error) {
	root, err := r.Open(a)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	var size int64
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// Open pins the allocation and verifies its recorded identity, including when
// it is a source of configuration copied into a bound context.
func (r Roots) Open(a Allocation) (*os.Root, error) {
	path := r.Path(a)
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err == nil {
		err = r.checkInfo(path, info)
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}
