package sandbox

import (
	"path/filepath"
	"strings"
)

// PathWithin reports whether path lexically equals parent or lies beneath it.
// Both arguments must already be clean absolute paths; no symlinks are
// resolved. A relative result that is itself absolute (a different volume on
// Windows) is outside.
func PathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
