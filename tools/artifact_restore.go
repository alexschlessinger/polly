package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/safefile"
)

// RegisterPrivateMutation installs an agent's private mutation without the
// always-allowed exemption of its recall tools. Unlike ordinary local tool
// registrations, it remains subject to the source registry's current tool
// selection and skill policies, even though the source does not own this tool.
func (r *ToolRegistry) RegisterPrivateMutation(tool Tool) {
	r.Register(tool)
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.viewAllowed
	r.viewAllowed = func(name string) bool {
		if previous != nil && !previous(name) {
			return false
		}
		return name != tool.GetName() || r.parent == nil || r.parent.privateMutationAllowed(name)
	}
}

// RegisterArtifactRestore is retained for callers installing restore_artifact.
func (r *ToolRegistry) RegisterArtifactRestore(tool Tool) {
	r.RegisterPrivateMutation(tool)
}

func (r *ToolRegistry) privateMutationAllowed(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.viewVisibleLocked(name) && r.isToolAllowedLocked(name) &&
		(r.parent == nil || r.parent.privateMutationAllowed(name))
}

// RestoreArtifact streams an already-authorized artifact into a NEW regular
// file, returning its absolute path. It owns and closes source on every path.
// The destination directory must exist and permit writes to both the explicit
// destination and a temporary sibling. The completed file is linked into place
// only after size, SHA-256, read/close and sync checks succeed. No source mode is
// preserved: the new file is private (0600, subject to umask), never executable.
func RestoreArtifact(ctx context.Context, registry *ToolRegistry, path string, ref artifacts.Ref, source io.ReadCloser) (abs string, err error) {
	if source == nil {
		return "", fmt.Errorf("artifact reader is missing")
	}
	defer func() {
		if source != nil {
			err = errors.Join(err, source.Close())
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !artifacts.ValidID(ref.ID) || ref.Bytes < 0 {
		return "", fmt.Errorf("invalid artifact metadata")
	}
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, routes, resolved, err := resolveLocalRoutes(registry, path)
	if err != nil {
		return "", err
	}
	if err := checkWritePolicy(registry, routes...); err != nil {
		return "", err
	}
	unlock := lockLocalFiles()
	defer unlock()
	// Check the original spelling too: a dangling leaf symlink must not turn
	// a request for a new file into a write to its nonexistent target.
	if _, err := os.Lstat(abs); err == nil {
		return "", fmt.Errorf("restore %s: destination already exists", abs)
	} else if !os.IsNotExist(err) {
		return "", describeOpenError("restore", abs, err)
	}
	parent := filepath.Dir(resolved)
	root, err := openArtifactRestoreDirectory(parent)
	if err != nil {
		return "", describeOpenError("restore", abs, err)
	}
	defer root.Close()
	name := filepath.Base(resolved)
	if _, err := root.Lstat(name); err == nil {
		return "", fmt.Errorf("restore %s: destination already exists", abs)
	} else if !os.IsNotExist(err) {
		return "", describeOpenError("restore", abs, err)
	}
	// A pinned directory keeps creation, installation and cleanup on the
	// approved route even if another process replaces an ancestor with a link.
	tempName := ".polly-restore-" + rand.Text()
	if err := checkWritePolicy(registry, filepath.Join(filepath.Dir(abs), tempName), filepath.Join(parent, tempName)); err != nil {
		return "", err
	}
	f, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", describeOpenError("restore", abs, err)
	}
	defer func() {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		err = errors.Join(err, root.Remove(tempName))
	}()
	hash := sha256.New()
	reader := artifactRestoreReader{ctx: ctx, reader: source}
	n, err := io.CopyBuffer(io.MultiWriter(f, hash), io.LimitReader(reader, ref.Bytes), make([]byte, 32<<10))
	if err != nil {
		return "", fmt.Errorf("restore %s: read artifact: %w", abs, err)
	}
	if n != ref.Bytes {
		return "", fmt.Errorf("artifact size mismatch: expected %d bytes, read %d", ref.Bytes, n)
	}
	var extra [1]byte
	if n, readErr := io.ReadFull(reader, extra[:]); n != 0 {
		return "", fmt.Errorf("artifact size mismatch: more than %d bytes", ref.Bytes)
	} else if readErr != io.EOF {
		return "", fmt.Errorf("read artifact end: %w", readErr)
	}
	if got := fmt.Sprintf("sha256:%x", hash.Sum(nil)); got != ref.ID {
		return "", fmt.Errorf("artifact SHA-256 digest mismatch")
	}
	closeErr := source.Close()
	source = nil
	if closeErr != nil {
		return "", fmt.Errorf("close artifact: %w", closeErr)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync restored file: %w", err)
	}
	closeErr = f.Close()
	f = nil
	if closeErr != nil {
		return "", fmt.Errorf("close restored file: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Link, unlike Rename, cannot replace an existing destination, including
	// a symlink planted while the artifact was being read. Unsupported filesystems
	// fail closed rather than falling back to a potentially destructive rename.
	if err := root.Link(tempName, name); err != nil {
		return "", describeOpenError("restore", abs, err)
	}
	return abs, nil
}

// openArtifactRestoreDirectory uses safefile's no-follow walk to pin the
// approved directory, then verifies os.Root opened that same directory. Root
// supplies portable descriptor-relative create/link/remove operations.
func openArtifactRestoreDirectory(path string) (*os.Root, error) {
	dir, err := safefile.OpenDirectory(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err == nil && !os.SameFile(info, actual) {
		err = fmt.Errorf("destination directory changed during open")
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

type artifactRestoreReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r artifactRestoreReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
