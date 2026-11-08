package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// memberWorkspace is a complete, owned dependency bundle for source selection.
// It is not a State: nil means an exact referenced record was absent. Task may
// be a not-yet-persisted refresh assignment, so the loader preserves its value.
type memberWorkspace struct {
	Member          *Member
	Context         *ExecutionContext
	Execution       *Execution
	Task            *taskDependencies
	Snapshot        *worktree.Snapshot
	ContextSnapshot *worktree.Snapshot
	Base, Source    string
}

func (rd *recordReader) MemberWorkspace(m *Member, task *Task) (*memberWorkspace, error) {
	if m == nil {
		return nil, errors.New("unknown member session")
	}
	b := &memberWorkspace{Member: m}
	var err error
	if m.Execution != "" {
		if b.Execution, err = rd.Execution(m.Execution); err != nil {
			return nil, err
		}
	}
	if task != nil {
		b.Task = &taskDependencies{Task: task}
		if task.Owner == m.ID {
			b.Task.Owner = m
		} else if task.Owner != "" {
			if b.Task.Owner, err = rd.Member(task.Owner); err != nil {
				return nil, err
			}
		}
		if task.Execution == m.Execution {
			b.Task.Execution = b.Execution
		} else if task.Execution != "" {
			if b.Task.Execution, err = rd.Execution(task.Execution); err != nil {
				return nil, err
			}
		}
	}
	b.Base, b.Source = continuationSourceRecords(m, b.Task, b.Execution)
	if b.Base != "" {
		if b.Snapshot, err = rd.Snapshot(b.Base); err != nil {
			return nil, err
		}
	}
	if m.Context != "" {
		if b.Context, err = rd.Context(m.Context); err != nil {
			return nil, err
		}
	}
	if b.Context != nil {
		base, _ := workspaceSource(b.Context)
		if base == b.Base {
			b.ContextSnapshot = b.Snapshot
		} else if base != "" {
			if b.ContextSnapshot, err = rd.Snapshot(base); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

func continuationSourceRecords(m *Member, task *taskDependencies, execution *Execution) (snapshot, source string) {
	if task != nil && task.Task != nil {
		t := task.Task
		if t.Status == "done" || task.delivering() {
			return followupSource(t, m.ReadOnly, "")
		}
		if t.StartingSnapshot != "" || t.SourceRoot != "" {
			return t.StartingSnapshot, t.SourceRoot
		}
	}
	if execution != nil {
		return execution.Base, execution.SourceRoot
	}
	return "", ""
}

func sameWorkspaceBaseline(actual, required string, a, b *worktree.Snapshot) bool {
	if actual == "" || required == "" {
		return actual == required
	}
	return validTaskSnapshot(a, actual) && validTaskSnapshot(b, required) && sameCapturedCommit(a, b)
}

func workspaceMismatchRecords(c *ExecutionContext, snapshot, source string, actualSnapshot, requiredSnapshot *worktree.Snapshot) error {
	actual, root := workspaceSource(c)
	actual = capturedCommit(actualSnapshot, actual)
	if actual == "" {
		actual = root
		if actual == "" {
			actual = "an unavailable captured commit"
		}
	}
	required := capturedCommit(requiredSnapshot, snapshot)
	if required == "" {
		required = source
		if required == "" {
			required = "an unavailable captured commit"
		}
	}
	return fail("workspace_mismatch", fmt.Sprintf("member's workspace uses %s; this follow-up requires %s; release the workspace and retry", actual, required))
}

func capturedCommit(snapshot *worktree.Snapshot, id string) string {
	if validTaskSnapshot(snapshot, id) {
		return snapshot.Commit
	}
	return ""
}

// Followup query fanout is confined to an exact reverse relation. No kind-only
// or family query belongs here. Paging also bounds each store payload read.
func followupRecordValues[T any](read func(sessions.RecordQuery) (recordPage[T], error), index sessions.CoordinationIndex, key string) ([]*T, error) {
	var values []*T
	q := sessions.RecordQuery{Index: index, Key: key, Limit: sessions.MaxCoordinationPageLimit}
	for {
		page, err := read(q)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			values = append(values, record.Value)
		}
		if page.Next == "" {
			return values, nil
		}
		if page.Next == q.After {
			return nil, sessions.ErrCoordinationCursor
		}
		q.After = page.Next
	}
}

func (r *Runtime) refreshEligibleRecords(rd *recordReader, m *Member, f *FollowupCall, mailID, ownExecution string) error {
	if m == nil {
		return fail("unknown_member", "refresh requires an existing worker")
	}
	if err := userStopRefusal(m); err != nil {
		return err
	}
	if err := rd.refreshReservation(m.ID, mailID); err != nil {
		return err
	}
	if r.workflowReserved(m.Controller) {
		return fail("session_busy", "worker is reserved by an active workflow; wait for it to settle or cancel it")
	}
	r.mu.Lock()
	active := r.active[m.ID]
	busy := active != nil && active.id != ownExecution
	r.mu.Unlock()
	if busy {
		return fail("session_busy", "refresh requires an idle worker; wait for its execution to finish")
	}
	ok, why, err := memberReleaseEligibleRecords(rd, m)
	if err != nil {
		return err
	}
	if !ok {
		return fail("refresh_refused", "refresh requires a completed, settled assignment: "+why+"; resume, accept, integrate or reconcile the existing work first")
	}
	pending, err := rd.pendingFollowup(m.ID)
	if err != nil {
		return err
	}
	if pending {
		return fail("session_busy", "worker has a pending follow-up; let that work settle before refreshing")
	}
	var t *Task
	if m.Task != "" {
		t, err = rd.Task(m.Task)
		if err != nil {
			return err
		}
	}
	if t == nil || t.Owner != m.ID || t.Status != "done" {
		return fail("refresh_refused", "refresh requires the worker's previous assignment to be done; it does not accept prior work")
	}
	if f != nil && (f.PreviousTask != t.ID || f.PreviousRevision != t.Revision) {
		return fail("stale_task", "assignment changed after the refresh selected its baseline; start a new follow-up")
	}
	for _, status := range []string{"paused", "running"} {
		runs, err := followupRecordValues(rd.Runs, sessions.CoordinationIndexRunStatus, status)
		if err != nil {
			return err
		}
		for _, run := range runs {
			if run.Status == "paused" || run.Status == "running" && run.Starts >= run.Limit {
				return ErrBudget
			}
		}
	}
	return validateRequirement(t, m.ReadOnly)
}

func (r *Runtime) refreshLaunchTaskRecords(rd *recordReader, m *Member, mailID, ownExecution string) (*Task, error) {
	f, err := rd.Followup(mailID)
	if err != nil {
		return nil, err
	}
	mail, err := rd.Mail(mailID)
	if err != nil {
		return nil, err
	}
	if m == nil || f == nil || mail == nil || !f.Refresh || f.Phase != "preparing" || f.Member != m.ID {
		return nil, fail("stale_task", "refresh preparation is no longer available; retry explicitly")
	}
	if err := r.refreshEligibleRecords(rd, m, f, mailID, ownExecution); err != nil {
		return nil, err
	}
	if f.Base != "" {
		base, err := rd.Snapshot(f.Base)
		if err != nil {
			return nil, err
		}
		if !validTaskSnapshot(base, f.Base) {
			return nil, unavailableWorkspace()
		}
	}
	prior, err := rd.Task(f.PreviousTask)
	if err != nil {
		return nil, err
	}
	d := &taskDependencies{Task: prior, Owner: m}
	requirement := d.requirement()
	return &Task{ID: f.Task, Follows: prior.ID, Owner: m.ID, Requirement: requirement, Criteria: criteriaFor(requirement), Description: mail.Text, StartingSnapshot: f.Base, SourceRoot: f.Source, Status: "pending"}, nil
}

// contextCleanupTreeRecords proves the exact current tree against a done task's
// submitted snapshot, without scanning old snapshots from this workspace.
func (r *Runtime) contextCleanupTreeRecords(ctx context.Context, c *ExecutionContext) (string, error) {
	if c.Checkout == nil {
		return "", nil
	}
	manager, err := r.manager(ctx)
	if err != nil {
		return "", err
	}
	if c.Disposable {
		return "", nil
	}
	if c.Release == WorkspaceReleasing {
		present, err := manager.CheckoutPresent(*c.Checkout)
		if err != nil || !present {
			return "", err
		}
	}
	if unchanged, err := manager.Unchanged(ctx, *c.Checkout); err != nil {
		return "", err
	} else if unchanged {
		return c.Checkout.Base.Tree, nil
	}
	current, err := manager.Capture(ctx, c.Root)
	if err != nil {
		return "", err
	}
	if current.Tree == c.Checkout.Base.Tree {
		return current.Tree, nil
	}
	proven := false
	if !c.ReadOnly {
		err = r.readRecords(ctx, func(rd *recordReader) error {
			var err error
			proven, err = releaseCandidate(rd, sessions.CandidateDoneTaskTree, sessions.CoordinationIndexKey(c.Root, current.Tree), func(key sessions.RecordKey) (bool, error) {
				task, err := rd.Task(key.ID)
				if err != nil || task == nil || task.Status != "done" || task.Snapshot == "" {
					return false, err
				}
				snapshot, err := rd.Snapshot(task.Snapshot)
				return snapshot != nil && snapshot.Source == c.Root && snapshot.Tree == current.Tree, err
			})
			return err
		})
	}
	if err != nil {
		return "", err
	}
	if proven {
		return current.Tree, nil
	}
	return "", &workflow.Error{Code: "unintegrated_changes", Message: "context contains unintegrated tree " + current.Tree + "; retained at " + c.Root, Result: map[string]any{"context": c.ID, "root": c.Root}}
}

// Single-context equivalent of finishRelease used by refresh. Durable marking
// precedes removal; tools and child processes close before files disappear.
func (r *Runtime) finishRefreshRelease(c *ExecutionContext, tree string) error {
	ctx, cancel := context.WithTimeout(r.config.Parent.Context(), 2*time.Minute+10*time.Second)
	defer cancel()
	p, err := r.readReleaseProof(ctx, c.ID)
	if err != nil {
		return err
	}
	expected := *c
	expected.Release = WorkspaceReleasing
	if !reflect.DeepEqual(p.context, &expected) {
		return fail("workspace_changed", "workspace changed during refresh release; retry explicitly")
	}
	_, err = r.finishRelease(ctx, []*releaseProof{p}, map[string]string{c.ID: tree})
	return err
}
