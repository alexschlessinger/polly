package scratch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootIsOutsideTheHomeDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	root := Root()
	// The whole point of the location: no ancestor of a scratch is a private
	// root, so opening each component of a path into it succeeds.
	for dir := root; dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if dir == home {
			t.Fatalf("scratch root %s is inside the private home %s", root, home)
		}
	}
	if !strings.HasPrefix(filepath.Base(root), "polly-") {
		t.Fatalf("scratch root %s is not named for polly", root)
	}
}

func TestDirForSeparatesIdenticalSlotNames(t *testing.T) {
	first := DirFor(filepath.Join("/one/runtime", "slot-0000"))
	second := DirFor(filepath.Join("/other/runtime", "slot-0000"))
	if first == second {
		t.Fatalf("slots of different runtimes share a scratch: %s", first)
	}
	for _, dir := range []string{first, second} {
		if filepath.Dir(dir) != Root() {
			t.Fatalf("scratch %s is not in the scratch root %s", dir, Root())
		}
		if !strings.HasPrefix(filepath.Base(dir), "slot-0000-") {
			t.Fatalf("scratch %s does not name its slot", dir)
		}
	}
	if DirFor("/one/runtime/slot-0000") != first {
		t.Fatal("DirFor is not stable for one slot")
	}
}

// isolateRoot relocates the scratch root for one test, so the test never
// touches the root a running polly or a parallel test package is using.
func isolateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	t.Setenv(RootEnv, root)
	return root
}

func TestRootEnvRelocatesTheRoot(t *testing.T) {
	root := isolateRoot(t)
	if Root() != root {
		t.Fatalf("Root() = %s, want %s", Root(), root)
	}
	if got := EnsureRootMust(t); got != root {
		t.Fatalf("EnsureRoot() = %s, want %s", got, root)
	}
	if filepath.Dir(DirFor("/x/slot-0000")) != root || filepath.Dir(NestedRoot()) != root {
		t.Fatalf("DirFor %s and NestedRoot %s do not lie in %s", DirFor("/x/slot-0000"), NestedRoot(), root)
	}
	t.Setenv(RootEnv, "relative/root")
	if Root() == "relative/root" {
		t.Fatal("a relative override was honoured")
	}
}

func EnsureRootMust(t *testing.T) string {
	t.Helper()
	root, err := EnsureRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// A scratch is a member's $TMPDIR, and Unix socket paths inside it must fit
// sockaddr_un: the names polly adds under the temp directory stay short.
func TestScratchNamesAreShort(t *testing.T) {
	base := filepath.Base(DirFor("/x/slot-0000"))
	if want := len("slot-0000-") + 8; len(base) != want {
		t.Fatalf("scratch name %s is %d bytes, want %d", base, len(base), want)
	}
	root := filepath.Base(RootUnder(t.TempDir()))
	if len(root) > len("polly-")+10 {
		t.Fatalf("root name %s is too long", root)
	}
}

func TestClaimEmptiesAReusedSlotAndReserveRefusesAHeldOne(t *testing.T) {
	isolateRoot(t)
	slot := filepath.Join(t.TempDir(), "slot-0000")
	dir, err := Claim(slot)
	if err != nil {
		t.Fatal(err)
	}
	defer RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(slot); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Reserve of a held slot = %v, want fs.ErrExist", err)
	}
	if _, err := Claim(slot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Claim kept a previous occupant's file: %v", err)
	}
}

func TestSweepReclaimsScratchOfARemovedOwnerOnly(t *testing.T) {
	isolateRoot(t)
	held := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(held, 0700); err != nil {
		t.Fatal(err)
	}
	living, err := Claim(filepath.Join(held, "slot-0000"))
	if err != nil {
		t.Fatal(err)
	}
	defer Release(living)
	gone := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(gone, 0700); err != nil {
		t.Fatal(err)
	}
	orphan, err := Claim(filepath.Join(gone, "slot-0000"))
	if err != nil {
		t.Fatal(err)
	}
	// A runtime area removed wholesale rather than released: the only record
	// of the scratch it owned is the one Sweep reads.
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(living); err != nil {
		t.Fatalf("sweep removed a scratch whose owner is on disk: %v", err)
	}
	for _, path := range []string{orphan, orphan + ownerSuffix} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("sweep kept %s: %v", path, err)
		}
	}
}
