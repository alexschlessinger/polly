package scratch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ownerSuffix names the record written beside a scratch directory, holding the
// path of the slot that owns it. A scratch is created outside the directory
// that owns it, so nothing else records the link; the record lives in the
// scratch root, which no sandboxed process may write.
const ownerSuffix = ".owner"

// RootEnv names the variable that relocates the scratch root. The sandbox
// sets it for the commands it wraps where the host root is denied to them,
// so a polly or a test suite started inside one claims scratch it may write;
// a user may set it too. Its value is used as given, cleaned.
const RootEnv = "POLLYTOOL_SCRATCH_ROOT"

// Root is the parent of every runtime-owned scratch directory. It sits in the
// OS temp area rather than under the private home so that no ancestor of a
// scratch directory is denied to the process that owns it: a tool that opens
// each component of a path in turn, as internal/safefile does, cannot reach a
// directory whose ancestor is a private root, however the leaf is granted. The
// sandbox keeps the isolation by making Root private in turn, with its own
// entry left traversable so that walk succeeds; see docs/SANDBOX.md.
func Root() string {
	if dir := os.Getenv(RootEnv); dir != "" && filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return defaultRoot()
}

// RootUnder names a scratch root inside a granted temporary directory. Nested
// processes receive it explicitly rather than falling back to the host root.
func RootUnder(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	name := "polly"
	if uid := os.Getuid(); uid >= 0 {
		name = fmt.Sprintf("%s-%d", name, uid)
	}
	return filepath.Join(dir, name)
}

// PrivateRoots includes the active root, the platform default, and the former
// TMPDIR-based default. Relocating scratch must not expose another runtime's
// slots or scratch retained from an older executable.
func PrivateRoots() []string {
	roots := []string{Root()}
	for _, root := range []string{defaultRoot(), RootUnder(os.TempDir())} {
		found := false
		for _, existing := range roots {
			found = found || existing == root
		}
		if !found {
			roots = append(roots, root)
		}
	}
	return roots
}

// NestedRoot is the scratch root handed, through RootEnv, to a sandboxed
// command that has no scratch of its own and may not write the root: a
// directory inside the root that the sandbox grants to that command alone,
// so a polly started there keeps its members apart from the host's.
func NestedRoot() string { return filepath.Join(Root(), "nested") }

// DirFor names the scratch directory belonging to a slot. Slot names repeat
// across repositories and runtimes, so the owning directory is digested into
// the name: two managers never claim one scratch.
func DirFor(slot string) string {
	sum := sha256.Sum256([]byte(filepath.Dir(slot)))
	return filepath.Join(Root(), filepath.Base(slot)+"-"+hex.EncodeToString(sum[:4]))
}

// EnsureRoot creates Root and reports it, refusing an entry that is not a
// directory this user owns exclusively. The OS temp area is shared on Unix, so
// an entry already standing at that name is not trusted until it is checked.
func EnsureRoot() (string, error) {
	root := Root()
	if err := ensureExclusiveDir(root); err != nil {
		return "", err
	}
	return root, nil
}

// EnsureNestedRoot creates NestedRoot inside an ensured Root and reports it.
func EnsureNestedRoot() (string, error) {
	if _, err := EnsureRoot(); err != nil {
		return "", err
	}
	nested := NestedRoot()
	if err := ensureExclusiveDir(nested); err != nil {
		return "", err
	}
	return nested, nil
}

func ensureExclusiveDir(dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("scratch root %s is not a directory", dir)
	}
	if err := exclusiveToCurrentUser(info); err != nil {
		return fmt.Errorf("scratch root %s: %w", dir, err)
	}
	return nil
}

// Reserve creates the scratch directory for slot, reporting fs.ErrExist when
// another occupant already holds it.
func Reserve(slot string) (string, error) {
	dir, err := prepare(slot)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// Claim prepares an empty scratch directory for slot, discarding whatever a
// previous occupant of that slot left behind.
func Claim(slot string) (string, error) {
	dir, err := prepare(slot)
	if err != nil {
		return "", err
	}
	if err := RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// prepare records slot's ownership of its scratch and reports the path. The
// record is written before the directory exists so that a crash in between
// leaves a record Sweep can act on rather than a directory it cannot place.
func prepare(slot string) (string, error) {
	if _, err := EnsureRoot(); err != nil {
		return "", err
	}
	dir := DirFor(slot)
	if err := os.WriteFile(dir+ownerSuffix, []byte(slot), 0600); err != nil {
		return "", err
	}
	return dir, nil
}

// Release removes a scratch directory and then the ownership record beside
// it. The record outlives a failed removal: Sweep finds a scratch by its
// record alone, so removing the record first would strand the directory.
func Release(dir string) error {
	if err := RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Remove(dir + ownerSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Sweep reclaims scratch directories whose owner is gone. A runtime directory
// removed wholesale rather than released — a deleted checkout area, a test's
// temporary directory — leaves its scratches behind with nothing else to find
// them by. A scratch whose owner is still on disk is never touched, so a sweep
// is safe while another polly is running.
func Sweep() error {
	var errs []error
	for _, root := range PrivateRoots() {
		errs = append(errs, sweepRoot(root))
	}
	return errors.Join(errs...)
}

func sweepRoot(path string) error {
	root, err := openExclusiveRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ownerSuffix)
		if !ok {
			continue
		}
		owner, err := readOwner(root, name)
		if err != nil {
			continue // Untrusted record, or raced with another sweep/release.
		}
		// Park uses the hold's name directly; ordinary claims use DirFor.
		if name != filepath.Base(DirFor(owner)) && name != filepath.Base(owner) {
			continue
		}
		// Slots are created on demand, so their runtime directory is the
		// liveness test. Only absence, not a permission or I/O error, proves
		// the owner is gone.
		if _, err := os.Stat(filepath.Dir(owner)); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		errs = append(errs, releaseAt(root, name))
	}
	return errors.Join(errs...)
}

// Park moves dir aside as name under the root, recording owner beside it so
// that Sweep reclaims the held directory once owner's directory is gone. A
// previous hold of the same name is discarded first. The move fails across
// roots, and then dir stays where it was.
func Park(dir, name, owner string) (string, error) {
	root, err := EnsureRoot()
	if err != nil {
		return "", err
	}
	held := filepath.Join(root, name)
	if err := RemoveAll(held); err != nil {
		return "", err
	}
	if err := os.WriteFile(held+ownerSuffix, []byte(owner), 0600); err != nil {
		return "", err
	}
	if err := os.Rename(dir, held); err != nil {
		os.Remove(held + ownerSuffix)
		return "", err
	}
	return held, nil
}

// Adopt moves a held directory into dest, replacing what dest holds, and
// drops the hold's record; dest keeps its own. A failed move discards the
// hold and leaves dest empty, so a caller never finds either half-moved.
func Adopt(held, dest string) error {
	if err := RemoveAll(dest); err != nil {
		return err
	}
	if err := os.Rename(held, dest); err != nil {
		return errors.Join(err, Release(held), os.MkdirAll(dest, 0700))
	}
	if err := os.Remove(held + ownerSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
