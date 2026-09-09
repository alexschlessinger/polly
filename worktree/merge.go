package worktree

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type ConflictEntry struct {
	Path   string `json:"path"`
	Stage  int    `json:"stage"`
	Mode   string `json:"mode"`
	Object string `json:"object"`
}

// Conflict preserves Git's structured records, including conflicts that have
// no textual markers or no individual unmerged index entries.
type Conflict struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Paths   []string        `json:"paths"`
	Entries []ConflictEntry `json:"entries,omitempty"`
	Base    Snapshot        `json:"base"`
	Ours    Snapshot        `json:"ours"`
	Theirs  Snapshot        `json:"theirs"`
}
type MergeResult struct {
	Snapshot  Snapshot   `json:"snapshot"`
	Conflicts []Conflict `json:"conflicts"`
}

// Merge creates an immutable snapshot without allocating a checkout. Every
// input supplies its own explicit base; runtime snapshots have no Git parents.
func (m *Manager) Merge(ctx context.Context, base, ours, theirs Snapshot) (MergeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, mergeErr := m.git(ctx, m.Root, nil, nil, "merge-tree", "--write-tree", "-z", "--messages", "--merge-base="+base.Commit, ours.Commit, theirs.Commit)
	conflicted := false
	if mergeErr != nil {
		var exit *exec.ExitError
		if !errors.As(mergeErr, &exit) || exit.ExitCode() != 1 {
			return MergeResult{}, mergeErr
		}
		conflicted = true
	}
	tree, conflicts, err := parseMergeOutput(out, conflicted)
	if err != nil {
		return MergeResult{}, err
	}
	for i := range conflicts {
		conflicts[i].Base = base
		conflicts[i].Ours = ours
		conflicts[i].Theirs = theirs
	}
	snapshot, err := m.snapshotTree(ctx, tree, m.Root)
	return MergeResult{Snapshot: snapshot, Conflicts: conflicts}, err
}

func parseMergeOutput(out []byte, conflicted bool) (string, []Conflict, error) {
	fields := bytes.Split(out, []byte{0})
	if len(fields) == 0 {
		return "", nil, errors.New("empty merge-tree output")
	}
	tree := string(fields[0])
	_, err := hex.DecodeString(tree)
	if err != nil || len(tree) != 40 && len(tree) != 64 {
		return "", nil, errors.New("invalid merge-tree object")
	}
	entries := []ConflictEntry{}
	i := 1
	for i < len(fields) && len(fields[i]) > 0 {
		header, path, ok := strings.Cut(string(fields[i]), "\t")
		bits := strings.Fields(header)
		if !ok || len(bits) != 3 {
			return "", nil, errors.New("invalid merge-tree stage")
		}
		stage, err := strconv.Atoi(bits[2])
		if err != nil || stage < 1 || stage > 3 {
			return "", nil, errors.New("invalid conflict stage")
		}
		entries = append(entries, ConflictEntry{Path: path, Stage: stage, Mode: bits[0], Object: bits[1]})
		i++
	}
	i++ // empty separator before the message records
	conflicts := []Conflict{}
	for i < len(fields) && len(fields[i]) > 0 {
		n, err := strconv.Atoi(string(fields[i]))
		i++
		if err != nil || n < 0 || n > len(fields)-i-2 {
			return "", nil, errors.New("invalid merge-tree message")
		}
		paths := make([]string, n)
		for j := range paths {
			paths[j] = string(fields[i+j])
		}
		i += n
		kind, message := string(fields[i]), string(fields[i+1])
		i += 2
		if strings.HasPrefix(kind, "CONFLICT") {
			conflicts = append(conflicts, Conflict{Type: kind, Message: message, Paths: paths, Entries: entries})
		}
	}
	if conflicted && len(conflicts) == 0 {
		conflicts = append(conflicts, Conflict{Type: "CONFLICT", Message: "Git reported a conflict; inspect all stages and snapshots", Entries: entries})
	}
	if !conflicted && len(conflicts) > 0 {
		return "", nil, fmt.Errorf("merge-tree returned success with conflicts")
	}
	return tree, conflicts, nil
}

// CaptureCurrent reuses the baseline when the parent is unchanged, without
// allocating a snapshot ref or worktree slot for a no-op refresh.
func (m *Manager) CaptureCurrent(ctx context.Context, baseline Snapshot) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capture(ctx, m.Root, baseline)
}
