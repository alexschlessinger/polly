package swarm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// captureOutputs stores every regular file a member left in its output
// directory as an artifact of its session, without removing the originals.
// Finalization clears the directory only after the pins and terminal result
// are durable. Each reference is named by its slash-separated relative path.
// Capture shares the limits and the read policy of swarm_publish files and
// fails closed on anything that is not a regular file or that changes while
// it is read, so the parent never receives a partial or torn handoff. A
// missing directory captures nothing.
func captureOutputs(ctx context.Context, registry *tools.ToolRegistry, store artifacts.Store, scratch string) ([]artifacts.Ref, error) {
	return outputCapture{read: tools.ReadArtifactFile}.run(ctx, registry, store, scratch)
}

// outputCapture is the capture with its file reader, so a test can observe
// what happens to a file that changes under the read.
type outputCapture struct {
	read func(context.Context, *tools.ToolRegistry, string, int64) (artifacts.Blob, error)
}

func (o outputCapture) run(ctx context.Context, registry *tools.ToolRegistry, store artifacts.Store, scratch string) ([]artifacts.Ref, error) {
	if scratch == "" {
		return nil, nil
	}
	dir := tools.OutputDir(scratch)
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", outputName(dir, path))
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("output capture: %w", err)
	}
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > publicationMaxFiles {
		return nil, fmt.Errorf("output capture: %d files exceed the limit of %d; remove files from the output directory before completing", len(paths), publicationMaxFiles)
	}
	if store == nil {
		return nil, errors.New("output capture requires an artifact store")
	}
	remaining := int64(publicationTotalFileBytes)
	refs := make([]artifacts.Ref, 0, len(paths))
	for _, path := range paths {
		// A file another process is still writing is not a result. The
		// stat pair brackets the read; a size or modification time that moved,
		// or a read that saw other bytes than the file had, fails the capture.
		before, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("output capture: %w", err)
		}
		blob, err := o.read(ctx, registry, path, min(int64(publicationFileBytes), remaining))
		if err != nil {
			return nil, fmt.Errorf("output capture: %w", err)
		}
		after, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("output capture: %w", err)
		}
		if !after.Mode().IsRegular() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || int64(len(blob.Data)) != before.Size() {
			return nil, fmt.Errorf("output capture: %s changed while it was being captured; stop the processes writing it before finishing", outputName(dir, path))
		}
		blob.Name = outputName(dir, path)
		ref, err := store.Put(ctx, blob)
		if err != nil {
			return nil, fmt.Errorf("output capture: store %s: %w", blob.Name, err)
		}
		remaining -= int64(len(blob.Data))
		refs = append(refs, ref)
	}
	return refs, nil
}

// clearOutputs runs only after the captured files belong to a durable terminal
// result, while the invocation still holds the context lock and reservation.
func clearOutputs(scratch string) error {
	dir := tools.OutputDir(scratch)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("output cleanup: clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("output cleanup: recreate %s: %w", dir, err)
	}
	return nil
}

// outputName is the logical name of a captured file: its slash-separated
// path relative to the output directory.
func outputName(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

// pinOutputs publishes a member's captured outputs to the family, so the
// parent and the workers it shares them with open them like published
// artifacts. The member can pin only bytes its own session stored.
func (r *Runtime) pinOutputs(ctx context.Context, actor string, refs []artifacts.Ref) error {
	if len(refs) == 0 {
		return nil
	}
	if err := r.prepare(ctx); err != nil {
		return err
	}
	return r.parent.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		raw.PinOwner = actor
		for _, ref := range refs {
			raw.Pins = append(raw.Pins, ref.ID)
		}
		return nil
	})
}

// outputsText lists a result's captured output files for a notice.
func outputsText(refs []artifacts.Ref) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, fmt.Sprintf("%q (%s, %d bytes)", ref.Name, ref.ID, ref.Bytes))
	}
	return " Output files captured before release, for read_artifact or restore_artifact({id,path}): " + strings.Join(parts, ", ") + "."
}
