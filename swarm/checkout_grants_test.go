package swarm

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestCheckoutReadGrantsHonorOperatorDenials(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, "repo", ".git")
	config := filepath.Join(dir, "home", ".gitconfig")
	hidden := filepath.Join(dir, "home", ".private", "gitconfig")
	for _, path := range []string{gitDir, filepath.Dir(config), filepath.Dir(hidden)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{config, hidden} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := sandbox.Config{DenyPaths: []string{filepath.Dir(hidden)}}
	grants, err := checkoutReadGrants(base, []string{filepath.Join(dir, "secrets")}, gitDir, []string{config, hidden})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{gitDir, config}; !slices.Equal(grants, want) {
		t.Fatalf("checkoutReadGrants() = %v, want the Git directory and the unmasked config %v", grants, want)
	}

	// A Git directory under an operator's private path is not re-opened by
	// the checkout's convenience grant; the member is refused instead.
	if _, err := checkoutReadGrants(sandbox.Config{}, []string{filepath.Join(dir, "repo")}, gitDir, nil); err == nil || !strings.Contains(err.Error(), "Git directory") {
		t.Fatalf("checkoutReadGrants() with a private repository = %v, want a refusal", err)
	}
}
