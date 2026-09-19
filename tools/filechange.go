package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/textdiff"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// FileChange is one file's difference between two workspace states. Tools
// that mutate files report it in ToolOutput.Data for the user interface; the
// model-facing text of those tools does not include it.
type FileChange struct {
	// Path is slash-separated and relative to FileChanges.Root, or absolute
	// when the file lies outside that root.
	Path string `json:"path"`
	// Kind is ChangeCreated, ChangeModified, or ChangeDeleted.
	Kind      string `json:"kind"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	// Diff is the unified diff body: "--- a/x", "+++ b/x", then hunks. It is
	// empty for binary files and for files over the size limits.
	Diff string `json:"diff,omitempty"`
	// Truncated reports that Diff was cut or omitted
	// because the file or the diff exceeded a limit. CountsUnknown marks
	// omitted or approximate line counts.
	Truncated     bool `json:"truncated,omitempty"`
	Binary        bool `json:"binary,omitempty"`
	CountsUnknown bool `json:"counts_unknown,omitempty"`
}

const (
	ChangeCreated  = "created"
	ChangeModified = "modified"
	ChangeDeleted  = "deleted"
)

// FileChanges is the Data payload of a file-mutating tool call.
type FileChanges struct {
	// ObservedAt orders host-generated workspace reports; per-tool deltas omit it.
	ObservedAt time.Time `json:"observed_at,omitzero"`
	// Root is the absolute workspace root that relative Paths resolve against.
	Root string `json:"root"`
	// Changes is sorted by path and never nil in a tracked result.
	Changes []FileChange `json:"changes"`
	// Tracked is false when the workspace could not be observed at all, for
	// example a bash command outside a Git repository. Reason says why.
	Tracked bool   `json:"tracked"`
	Reason  string `json:"reason,omitempty"`
	// Truncated reports that more files changed than fit; Omitted counts them.
	Truncated bool `json:"truncated,omitempty"`
	Omitted   int  `json:"omitted,omitempty"`
}

// Limits shared by every tool that reports file changes.
const (
	// ChangeMaxFileBytes bounds one side of a diffed file; larger files
	// report that line counts are unavailable.
	ChangeMaxFileBytes = 1 << 20
	// ChangeMaxTotalBytes bounds the diff bodies one call reports; later
	// files keep their counts and lose their body.
	ChangeMaxTotalBytes = 512 << 10
	// ChangeMaxFiles bounds the files one call lists.
	ChangeMaxFiles = 200

	changeDiffContext  = 3
	changeMaxFileBytes = ChangeMaxFileBytes
	changeMaxDiffBytes = 64 << 10 // per file; the body is cut at a hunk boundary
	changeMaxDiffLines = 20000    // differing lines a single diff may search
)

// DiffFileChange describes the change from old to new at path. oldExists and
// newExists select the kind; a missing side is diffed as empty content. Binary
// content (a NUL byte on either side) and oversized files carry no body.
func DiffFileChange(path string, old, new []byte, oldExists, newExists bool) FileChange {
	change := FileChange{Path: path, Kind: ChangeModified}
	switch {
	case !oldExists && newExists:
		change.Kind = ChangeCreated
	case oldExists && !newExists:
		change.Kind = ChangeDeleted
	}
	if bytes.IndexByte(old, 0) >= 0 || bytes.IndexByte(new, 0) >= 0 {
		change.Binary = true
		return change
	}
	if len(old) > changeMaxFileBytes || len(new) > changeMaxFileBytes {
		change.CountsUnknown = true
		change.Truncated = true
		return change
	}
	oldName, newName := "a/"+path, "b/"+path
	if !oldExists {
		oldName = "/dev/null"
	}
	if !newExists {
		newName = "/dev/null"
	}
	result := textdiff.Unified(oldName, newName, string(old), string(new), changeDiffContext, changeMaxDiffLines)
	change.Additions, change.Deletions = result.Additions, result.Deletions
	change.Truncated = result.Truncated
	change.CountsUnknown = result.Truncated
	if result.Truncated {
		change.Additions, change.Deletions = 0, 0
	}
	change.Diff = result.Diff
	if len(change.Diff) > changeMaxDiffBytes {
		change.Diff = truncateDiff(change.Diff, changeMaxDiffBytes)
		change.Truncated = true
	}
	return change
}

// truncateDiff prefers a complete hunk boundary, then a complete line. A
// single oversized hunk cannot bypass the byte budget.
func truncateDiff(diff string, limit int) string {
	cut := strings.LastIndex(diff[:limit], "\n@@ ")
	if cut > strings.Index(diff, "\n@@ ") {
		return diff[:cut+1]
	}
	if cut = strings.LastIndexByte(diff[:limit], '\n'); cut >= 0 {
		return diff[:cut+1]
	}
	return ""
}

// ChangeTracker observes a workspace around a command so the tool can report
// what the command changed. Implementations must not modify the workspace or
// its version-control metadata.
type ChangeTracker interface {
	// Snapshot records the state of the workspace containing dir (the
	// process working directory when empty). ok is false, with a reason, when
	// dir is not observable: outside a repository, too large, or disabled.
	Snapshot(ctx context.Context, dir string) (token string, ok bool, reason string, err error)
	// Changes snapshots again and reports what changed since token.
	Changes(ctx context.Context, dir, token string) (FileChanges, error)
}

// WithChangeTracker installs the tracker bash uses to report file changes.
// SetChangeTracker does the same on an existing registry.
func WithChangeTracker(tracker ChangeTracker) RegistryOption {
	return func(o *registryOptions) {
		o.changeTracker = tracker
	}
}

// SetChangeTracker attaches tracker to the registry. Bash tools loaded before
// the call use it from then on.
func (r *ToolRegistry) SetChangeTracker(tracker ChangeTracker) {
	r.mu.Lock()
	r.changeTracker = tracker
	r.mu.Unlock()
}

// ChangeTracker returns the registry's tracker, or its nearest ancestor's.
func (r *ToolRegistry) ChangeTracker() ChangeTracker {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	tracker, parent := r.changeTracker, r.parent
	r.mu.RUnlock()
	if tracker == nil && parent != nil {
		return parent.ChangeTracker()
	}
	return tracker
}

// changeRoot is the workspace root file changes are reported against: the
// execution root of a bound registry, else the process working directory.
func (r *ToolRegistry) changeRoot() string {
	if root := r.ExecutionRoot(); root != "" {
		return root
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// changePath spells abs for a FileChange: relative to root when inside it,
// otherwise absolute.
func changePath(root, abs string) string {
	if root != "" && sandbox.PathWithin(abs, root) {
		if rel, err := filepath.Rel(root, abs); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(abs)
}
