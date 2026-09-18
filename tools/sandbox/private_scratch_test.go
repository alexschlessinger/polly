package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/scratch"
)

func TestScratchRootIsAPrivatePolicyRoot(t *testing.T) {
	roots := platformPrivatePolicyRoots()
	if len(roots) == 0 {
		t.Skip("platform has no private roots")
	}
	found := false
	for _, root := range roots {
		if root == scratch.Root() {
			found = true
		}
	}
	if !found {
		t.Fatalf("scratch root %s is not private: %v", scratch.Root(), roots)
	}
}

// The scratch root hides every slot inside it without hiding its own entry: a
// command opens the root, which walking into its own scratch one component at
// a time requires, and still reads nothing beneath it but its own grant.
func TestScratchRootTraversableButPrivateSandbox(t *testing.T) {
	skipIfWindows(t)
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	root, err := scratch.EnsureRoot()
	if err != nil {
		t.Fatal(err)
	}
	unique := fmt.Sprintf("%d", os.Getpid())
	mine, sibling := filepath.Join(root, "test-mine-"+unique), filepath.Join(root, "test-sibling-"+unique)
	for _, dir := range []string{mine, sibling} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(filepath.Base(dir)+" bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	sb, err := New(Config{WritablePaths: []string{mine}}.Merge(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `ls "$1" > /dev/null && cat "$2" && ! cat "$3"`, "sh", root, filepath.Join(mine, "f"), filepath.Join(sibling, "f"))
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "test-mine-"+unique+" bytes") || strings.Contains(string(out), "test-sibling-"+unique+" bytes") {
		t.Fatalf("scratch root traversal and isolation: %s %v", out, err)
	}
}
