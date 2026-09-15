package docker

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
)

// ErrBusy reports a resync attempted while a call or sync is in flight.
var ErrBusy = errors.New("container binding is busy")

// syncTimeout bounds one synchronisation, independent of the tool call
// that triggered it.
const syncTimeout = 2 * time.Minute

// syncer serialises synchronisations and lets concurrent finished calls
// share one: a call that finished before a sync started is covered by it.
// The agent runs a batch's tools in parallel, so an N-call batch costs one
// or two syncs rather than N, and every call still returns only after its
// effects are on the host.
type syncer struct {
	mu        sync.Mutex
	cond      *sync.Cond
	running   bool
	started   uint64
	completed uint64
	results   map[uint64]error
	dirty     bool
	run       func(context.Context) error
}

func newSyncer(run func(context.Context) error) *syncer {
	s := &syncer{run: run, results: map[uint64]error{}}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// after synchronises on behalf of a call that has just finished, sharing a
// sync that starts later with every other such call.
func (s *syncer) after(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.started + 1
	for s.completed < target {
		if s.running {
			s.cond.Wait()
			continue
		}
		s.running = true
		s.started++
		number := s.started
		s.mu.Unlock()
		err := s.run(ctx)
		s.mu.Lock()
		s.running = false
		s.completed = number
		s.results[number] = err
		delete(s.results, number-8)
		s.cond.Broadcast()
	}
	return s.results[target]
}

// exclusive runs fn with no sync running and none able to start until it
// returns; it refuses when one is running.
func (s *syncer) exclusive(fn func() error) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrBusy
	}
	s.running = true
	s.mu.Unlock()
	err := fn()
	s.mu.Lock()
	s.running = false
	s.cond.Broadcast()
	s.mu.Unlock()
	return err
}

// writeCapable reports whether a finished call may have changed the
// workspace: file tools say so themselves, and anything that ran a process
// is assumed to have written, whether or not it exited well.
func writeCapable(info protocol.ToolInfo, result protocol.Result, err error) bool {
	switch {
	case info.Type == "shell" || info.Type == "mcp":
		return true
	case info.Name == "bash":
		return true
	case info.Name == "write_file" || info.Name == "edit_file":
		return result.Wrote && err == nil
	}
	return false
}

// syncFailed is the error a call returns when its effects could not be
// brought to the host.
func syncFailed(cause error) error {
	return tools.NewToolError(fmt.Sprintf("container still holds the edit: %v; the next successful sync or resync reconciles it", cause), "sync_failed")
}

// copyTransport moves a copy's changes to the host: it asks the helper to
// stage them, fetches the staging directory, and hands the tar and the
// deletions to the host's Git.
type copyTransport struct {
	engine    *engine
	container string
	session   *session
	git       GitAccess
	root      string
}

func (t *copyTransport) collect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
	defer cancel()
	frame, err := t.session.call(ctx, protocol.TypeSync, protocol.Sync{Op: protocol.SyncCollect})
	if err != nil {
		return err
	}
	synced, err := protocol.Decode[protocol.Synced](frame)
	if err != nil {
		return err
	}
	if synced.Staging == "" {
		if len(synced.Deleted) == 0 {
			return nil
		}
		return t.git.ImportChanges(ctx, t.root, emptyTar(), synced.Deleted)
	}
	archive, err := t.engine.getArchive(ctx, t.container, synced.Staging)
	if err != nil {
		return err
	}
	defer archive.Close()
	if err := t.git.ImportChanges(ctx, t.root, stripTarPrefix(archive, path.Base(synced.Staging)), synced.Deleted); err != nil {
		return err
	}
	_, err = t.session.call(ctx, protocol.TypeSync, protocol.Sync{Op: protocol.SyncCollected, Seq: synced.Seq})
	return err
}

// push brings the host checkout's divergence from tree into the copy:
// deletions first, then the changed files.
func (t *copyTransport) push(ctx context.Context, tree string) error {
	changed, deleted, err := t.git.Divergence(ctx, t.root, tree)
	if err != nil {
		return err
	}
	if len(changed) == 0 && len(deleted) == 0 {
		return nil
	}
	if _, err := t.session.call(ctx, protocol.TypeSync, protocol.Sync{Op: protocol.SyncApply, Deleted: deleted, Changed: changed}); err != nil {
		return err
	}
	if len(changed) == 0 {
		return nil
	}
	archive := tarFiles(t.root, changed, os.Getuid(), os.Getgid())
	defer archive.Close()
	return t.engine.putArchive(ctx, t.container, t.root, archive)
}

// putLayout puts the copy's layout in place: the tree directory holding a
// bundle of the base commit, and the scratch directory. It needs no helper,
// so it runs before the helper's exec, whose working directory is the tree.
func (t *copyTransport) putLayout(ctx context.Context, parent, treeName, scratchName, commit string) error {
	bundle, err := t.bundleFile(ctx, commit)
	if err != nil {
		return err
	}
	defer os.Remove(bundle)
	archive := layoutTar(treeName, scratchName, bundle, os.Getuid(), os.Getgid())
	defer archive.Close()
	if err := t.engine.putArchive(ctx, t.container, parent, archive); err != nil {
		return fmt.Errorf("put copy layout: %w", err)
	}
	return nil
}

// bootstrap initialises the copy from the bundle the layout holds.
func (t *copyTransport) bootstrap(ctx context.Context, commit string) error {
	_, err := t.session.call(ctx, protocol.TypeSync, protocol.Sync{Op: protocol.SyncBootstrap, Root: t.root, Commit: commit, Bundle: path.Join(t.root, bundleName)})
	return err
}

// reset moves the copy to commit, shipping a bundle when the copy may not
// hold it.
func (t *copyTransport) reset(ctx context.Context, commit string, ship bool) error {
	req := protocol.Sync{Op: protocol.SyncReset, Commit: commit}
	if ship {
		bundle, err := t.bundleFile(ctx, commit)
		if err != nil {
			return err
		}
		defer os.Remove(bundle)
		archive := tarFile(bundle, bundleName, 0o644, os.Getuid(), os.Getgid())
		defer archive.Close()
		if err := t.engine.putArchive(ctx, t.container, t.root, archive); err != nil {
			return fmt.Errorf("put bundle: %w", err)
		}
		req.Bundle = path.Join(t.root, bundleName)
	}
	_, err := t.session.call(ctx, protocol.TypeSync, req)
	return err
}

// bundleName is the bundle's name inside the copy's tree while it is
// consumed.
const bundleName = ".polly-base.bundle"

// bundleFile writes the bundle of commit to a host temp file so its size is
// known for the tar.
func (t *copyTransport) bundleFile(ctx context.Context, commit string) (string, error) {
	file, err := os.CreateTemp("", "polly-bundle-*")
	if err != nil {
		return "", err
	}
	if err := t.git.WriteBundle(ctx, t.root, commit, file); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// stagingDirName is the directory beside the copy's tree where the helper
// stages collected changes; it must be inside the volume because the
// daemon's archive API cannot read the container's tmpfs.
const stagingDirName = ".polly-sync"

// layoutTar is the slot layout put at the copy's parent directory: the tree
// directory holding the bundle, the scratch directory, and the staging
// directory. Every entry is explicit and owned by the host user, since the
// daemon creates implicit parents as root.
func layoutTar(treeName, scratchName, bundle string, uid, gid int) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		err := func() error {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: treeName + "/", Mode: 0o755, Uid: uid, Gid: gid, ModTime: time.Now()}); err != nil {
				return err
			}
			if scratchName != "" {
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: scratchName + "/", Mode: 0o700, Uid: uid, Gid: gid, ModTime: time.Now()}); err != nil {
					return err
				}
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: stagingDirName + "/", Mode: 0o700, Uid: uid, Gid: gid, ModTime: time.Now()}); err != nil {
				return err
			}
			return addFile(tw, bundle, path.Join(treeName, bundleName), 0o644, uid, gid)
		}()
		if err == nil {
			err = tw.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader
}

// tarFile is a tar holding one host file under name.
func tarFile(source, name string, mode int64, uid, gid int) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		err := addFile(tw, source, name, mode, uid, gid)
		if err == nil {
			err = tw.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader
}

// tarFiles is a tar of the named paths under root, with every ancestor
// directory written explicitly. Regular files and symlinks travel; anything
// else is skipped.
func tarFiles(root string, names []string, uid, gid int) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		seen := map[string]bool{}
		err := func() error {
			sorted := slices.Clone(names)
			slices.Sort(sorted)
			for _, name := range sorted {
				rel := filepath.ToSlash(filepath.Clean(name))
				for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
					if seen[dir] {
						continue
					}
					seen[dir] = true
					if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, Uid: uid, Gid: gid, ModTime: time.Now()}); err != nil {
						return err
					}
				}
				source := filepath.Join(root, filepath.FromSlash(rel))
				info, err := os.Lstat(source)
				if err != nil {
					return err
				}
				switch {
				case info.Mode()&os.ModeSymlink != 0:
					link, err := os.Readlink(source)
					if err != nil {
						return err
					}
					if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: rel, Linkname: link, Mode: 0o777, Uid: uid, Gid: gid, ModTime: info.ModTime()}); err != nil {
						return err
					}
				case info.Mode().IsRegular():
					if err := addFile(tw, source, rel, int64(info.Mode().Perm()), uid, gid); err != nil {
						return err
					}
				}
			}
			return nil
		}()
		if err == nil {
			err = tw.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader
}

func addFile(tw *tar.Writer, source, name string, mode int64, uid, gid int) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: info.Size(), Uid: uid, Gid: gid, ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)
	return err
}

func emptyTar() io.Reader {
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		writer.CloseWithError(tw.Close())
	}()
	return reader
}

// stripTarPrefix re-frames a tar so that entries lose their leading
// component (the fetched directory's own name).
func stripTarPrefix(archive io.Reader, prefix string) io.Reader {
	reader, writer := io.Pipe()
	go func() {
		in := tar.NewReader(archive)
		out := tar.NewWriter(writer)
		err := func() error {
			for {
				header, err := in.Next()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				name := strings.TrimPrefix(header.Name, "./")
				if name == prefix || name == prefix+"/" {
					continue
				}
				name = strings.TrimPrefix(name, prefix+"/")
				if name == "" {
					continue
				}
				copied := *header
				copied.Name = name
				if err := out.WriteHeader(&copied); err != nil {
					return err
				}
				if header.Typeflag == tar.TypeReg {
					if _, err := io.CopyN(out, in, header.Size); err != nil {
						return err
					}
				}
			}
		}()
		if err == nil {
			err = out.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader
}
