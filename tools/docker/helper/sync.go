package helper

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
)

// copyState is a copy-mode container's synchronisation state: the paths
// the helper knows the host to have pushed or collected, so their later
// disappearance reports as a deletion, and the staging directories awaiting
// the host's fetch.
type copyState struct {
	root    string
	staging string
	known   map[string]bool
	seq     int
}

// stagingDir is the directory beside the copy's tree, inside its volume,
// that holds collected changes until the host fetches them. The daemon's
// archive API reads volumes; it cannot read the container's tmpfs.
const stagingDir = ".polly-sync"

func (s *server) handleSync(frame protocol.Frame) error {
	req, err := protocol.Decode[protocol.Sync](frame)
	if err != nil {
		return s.fail(frame.ID, protocol.CodeProtocol, err.Error())
	}
	s.mu.Lock()
	hello := s.hello
	state := s.copy
	s.mu.Unlock()
	if hello == nil {
		return s.fail(frame.ID, protocol.CodeProtocol, "sync before hello")
	}
	if hello.Mode != "copy" {
		return s.fail(frame.ID, protocol.CodeUnsupported, "sync applies to copy mode only")
	}
	if state == nil {
		state = &copyState{root: hello.Root, staging: filepath.Join(filepath.Dir(hello.Root), stagingDir), known: map[string]bool{}}
		s.mu.Lock()
		s.copy = state
		s.mu.Unlock()
	}
	var reply protocol.Synced
	switch req.Op {
	case protocol.SyncBootstrap:
		err = state.bootstrap(req)
	case protocol.SyncReset:
		err = state.reset(req)
	case protocol.SyncApply:
		err = state.apply(req)
	case protocol.SyncCollect:
		reply, err = state.collect()
	case protocol.SyncCollected:
		err = os.RemoveAll(filepath.Join(state.staging, fmt.Sprint(req.Seq)))
	default:
		return s.fail(frame.ID, protocol.CodeProtocol, fmt.Sprintf("unknown sync operation %q", req.Op))
	}
	if err != nil {
		return s.fail(frame.ID, protocol.CodeInternal, err.Error())
	}
	return s.conn.Send(frame.ID, protocol.TypeSynced, reply)
}

func (c *copyState) git(input []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = c.root
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	cmd.Stdin = bytes.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

// bootstrap turns the empty tree the host put in place into a checkout of
// the base commit, then removes the bundle.
func (c *copyState) bootstrap(req protocol.Sync) error {
	if req.Commit == "" || req.Bundle == "" {
		return errors.New("bootstrap needs a commit and a bundle")
	}
	if _, err := os.Stat(filepath.Join(c.root, ".git")); err == nil {
		return errors.New("copy is already initialised")
	}
	if _, err := c.git(nil, "init", "-q"); err != nil {
		return err
	}
	if err := c.fetchAndCheckout(req.Commit, req.Bundle); err != nil {
		return err
	}
	c.known = map[string]bool{}
	return nil
}

// reset moves the copy to the commit and discards changes; ignored files,
// the copy's own build state, survive.
func (c *copyState) reset(req protocol.Sync) error {
	if req.Commit == "" {
		return errors.New("reset needs a commit")
	}
	if req.Bundle != "" {
		if _, err := c.git(nil, "fetch", "-q", req.Bundle, protocol.BundleRef(req.Commit)); err != nil {
			return err
		}
		_ = os.Remove(req.Bundle)
	}
	if _, err := c.git(nil, "reset", "-q", "--hard", req.Commit); err != nil {
		return err
	}
	if _, err := c.git(nil, "clean", "-qfd"); err != nil {
		return err
	}
	c.known = map[string]bool{}
	return nil
}

func (c *copyState) fetchAndCheckout(commit, bundle string) error {
	if _, err := c.git(nil, "fetch", "-q", bundle, protocol.BundleRef(commit)); err != nil {
		return err
	}
	if _, err := c.git(nil, "checkout", "-q", "--detach", commit); err != nil {
		return err
	}
	return os.Remove(bundle)
}

// apply removes deleted paths and records pushed ones as known.
func (c *copyState) apply(req protocol.Sync) error {
	for _, name := range req.Deleted {
		rel, err := safeRelative(name)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(c.root, rel)); err != nil {
			return err
		}
		delete(c.known, rel)
	}
	for _, name := range req.Changed {
		rel, err := safeRelative(name)
		if err != nil {
			return err
		}
		c.known[rel] = true
	}
	return nil
}

// collect stages every changed, added or untracked non-ignored file for the
// host and lists what disappeared: tracked deletions from status, and known
// files that no longer exist.
func (c *copyState) collect() (protocol.Synced, error) {
	out, err := c.git(nil, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return protocol.Synced{}, err
	}
	var changed, deleted []string
	for _, entry := range bytes.Split(out, []byte{0}) {
		line := string(entry)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "? "):
			changed = append(changed, strings.TrimPrefix(line, "? "))
		case strings.HasPrefix(line, "1 ") && len(fields) >= 9:
			name := strings.Join(fields[8:], " ")
			if strings.Contains(fields[1], "D") {
				deleted = append(deleted, name)
			} else {
				changed = append(changed, name)
			}
		case strings.HasPrefix(line, "u "):
			return protocol.Synced{}, errors.New("copy has unmerged paths")
		}
	}
	for name := range c.known {
		if _, err := os.Lstat(filepath.Join(c.root, name)); errors.Is(err, os.ErrNotExist) {
			deleted = append(deleted, name)
			delete(c.known, name)
		}
	}
	reply := protocol.Synced{Deleted: deleted}
	if len(changed) == 0 {
		return reply, nil
	}
	c.seq++
	staging := filepath.Join(c.staging, fmt.Sprint(c.seq))
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return protocol.Synced{}, err
	}
	for _, name := range changed {
		rel, err := safeRelative(name)
		if err != nil {
			return protocol.Synced{}, err
		}
		if err := copyEntry(filepath.Join(c.root, rel), filepath.Join(staging, rel)); err != nil {
			return protocol.Synced{}, err
		}
		c.known[rel] = true
	}
	reply.Seq, reply.Staging, reply.Changed = c.seq, staging, changed
	return reply, nil
}

func safeRelative(name string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(name))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("sync path %q escapes the copy", name)
	}
	return rel, nil
}

// copyEntry copies a regular file or symlink into the staging tree.
func copyEntry(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(link, target)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
