package swarm

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

const commitArgumentHelp = "use commit with a full retained Git commit from baseCommit, resultCommit, or candidate.merged.commit"

func rejectSnapshotArgument(args map[string]any) error {
	for key := range args {
		if strings.EqualFold(key, "snapshot") {
			return fail("invalid_args", "snapshot arguments are no longer supported; "+commitArgumentHelp)
		}
	}
	return nil
}

// snapshotForCommit resolves content identity to retained runtime provenance.
// Git object existence alone is not admission, and duplicate captures remain
// separate records with their own source and reference lifetime.
func (r *Runtime) snapshotForCommit(ctx context.Context, commit string) (string, error) {
	if _, err := hex.DecodeString(commit); err != nil || len(commit) != 40 && len(commit) != 64 {
		return "", fail("invalid_args", commitArgumentHelp)
	}
	commit = strings.ToLower(commit)
	s, err := r.read(ctx)
	if err != nil {
		return "", err
	}
	var selected *worktree.Snapshot
	for _, id := range sortedInspectionIDs(s.Snapshots) {
		snapshot := s.Snapshots[id]
		if snapshot == nil || snapshot.Commit != commit {
			continue
		}
		if !validTaskSnapshot(snapshot, id) || selected != nil && selected.Tree != snapshot.Tree {
			return "", fail("invalid_commit", "retained captures disagree about this commit; inspect the saved coordination state")
		}
		if selected == nil {
			selected = snapshot
		}
	}
	if selected == nil {
		return "", fail("unknown_commit", "commit is not retained in this runtime; "+commitArgumentHelp)
	}
	manager, err := r.manager(ctx)
	if err != nil {
		return "", err
	}
	if err := manager.ValidateSnapshot(ctx, *selected); err != nil {
		return "", fail("workspace_unavailable", "retained commit is unavailable or does not match its recorded tree: "+err.Error())
	}
	return selected.ID, nil
}

func (r *Runtime) commitArgument(ctx context.Context, args map[string]any) (string, error) {
	if err := rejectSnapshotArgument(args); err != nil {
		return "", err
	}
	value, present := args["commit"]
	if !present {
		return "", nil
	}
	commit, ok := value.(string)
	if !ok {
		return "", fail("invalid_args", commitArgumentHelp)
	}
	return r.snapshotForCommit(ctx, commit)
}

func (r *Runtime) decodeCommitRequest(ctx context.Context, args map[string]any) (AgentRequest, error) {
	req, err := decodeRequest(args)
	if err != nil {
		return req, err
	}
	req.Snapshot, err = r.commitArgument(ctx, args)
	return req, err
}

func (r *Runtime) decodeCommitFollowup(ctx context.Context, args map[string]any) (FollowupRequest, error) {
	var req FollowupRequest
	if err := rejectSnapshotArgument(args); err != nil {
		return req, err
	}
	if err := delegationArgs(tools.Args(args), "task", "question", "commit", "label", "background", "callID"); err != nil {
		return req, err
	}
	copy := make(map[string]any, len(args))
	for key, value := range args {
		if key != "commit" {
			copy[key] = value
		}
	}
	if err := strictRequest(copy, &req); err != nil {
		return req, err
	}
	var err error
	req.Snapshot, err = r.commitArgument(ctx, args)
	return req, err
}

// Baseline compatibility compares captured code, never submission ownership,
// acceptance, or candidate identity. Callers retain those separate checks.
func sameCapturedCommit(a, b *worktree.Snapshot) bool {
	return a != nil && b != nil && validTaskSnapshot(a, a.ID) && validTaskSnapshot(b, b.ID) && a.Commit == b.Commit && a.Tree == b.Tree
}

func sameBaseline(s *State, a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	x, y := s.Snapshots[a], s.Snapshots[b]
	return validTaskSnapshot(x, a) && validTaskSnapshot(y, b) && sameCapturedCommit(x, y)
}

func snapshotCommit(s *State, id string) string {
	if snapshot := s.Snapshots[id]; validTaskSnapshot(snapshot, id) {
		return snapshot.Commit
	}
	return ""
}
