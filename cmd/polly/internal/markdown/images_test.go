package markdown

import (
	"path/filepath"
	"testing"
)

func TestResolveSpaceFoldedPathMissReturnsInput(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.png")
	if got := resolveSpaceFoldedPath(missing); got != missing {
		t.Fatalf("resolveSpaceFoldedPath(missing) = %q, want input unchanged", got)
	}
}
