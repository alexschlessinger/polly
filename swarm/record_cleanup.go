package swarm

import (
	"errors"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Cleanup has always refused any unresolved apply, including one outside its
// selected workspace. Select only those actual obligations, not closed history.
func cleanupIntegrationsRecords(rd *recordReader) error {
	if rd.journalOnly {
		return nil
	}
	q := sessions.RecordQuery{Index: sessions.CoordinationIndexApplyUncertain, Limit: 32}
	for {
		page, err := queryRecords[ApplyRecord](rd, "apply", q)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			if record.Value.Status == "applying" || record.Value.Status == "recovery_required" {
				return errors.New("reconcile or complete interrupted integrations before cleanup")
			}
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == q.After {
			return sessions.ErrCoordinationCursor
		}
		q.After = page.Next
	}
}

func cleanupPausedRecords(rd *recordReader, p *releaseProof) error {
	if p.member == nil || p.execution == nil || p.execution.Status != "paused" {
		return nil
	}
	open, err := ownerHasOpenTaskRecords(rd, p.member.ID)
	if err == nil && open {
		err = errors.New("paused execution has open work; resume or cancel before cleanup")
	}
	return err
}

func cleanupTaskUnchanged(rd *recordReader, t *Task) (bool, error) {
	if t == nil || t.Owner == "" || t.Snapshot == "" || t.StartingSnapshot == "" {
		return false, nil
	}
	owner, err := rd.Member(t.Owner)
	if err != nil || owner == nil || owner.ID != t.Owner || owner.ReadOnly {
		return false, err
	}
	base, err := rd.Snapshot(t.StartingSnapshot)
	if err != nil {
		return false, err
	}
	submitted, err := rd.Snapshot(t.Snapshot)
	return validTaskSnapshot(base, t.StartingSnapshot) && validTaskSnapshot(submitted, t.Snapshot) && base.Tree == submitted.Tree, err
}

// This is validCandidate + completedUnchangedCandidate's release obligation,
// evaluated from exact referenced records, not a fabricated partial State.
func forgetCandidateOutstanding(rd *recordReader, c *IntegrationCandidate) (bool, error) {
	if c == nil || c.Successor != "" || c.Status == "superseded" || c.Status == "applied" {
		return false, nil
	}
	a, err := getRecord[ApplyRecord](rd, "apply", c.ID)
	if err != nil || a != nil && a.Status != "not_applied" {
		return false, err
	}
	completed := coordsem.CompletedCandidate(c.Status, c.Parent.Tree, c.Merged.Tree, len(c.Pending), len(c.Conflicts)) && len(c.references()) != 0
	for _, input := range append(append([]IntegrationInput(nil), c.Inputs...), c.Repairs...) {
		t, err := rd.Task(input.Task)
		if err != nil {
			return false, err
		}
		unchanged, err := cleanupTaskUnchanged(rd, t)
		if err != nil {
			return false, err
		}
		if !coordsem.CandidateTaskValid(semanticTask(t), c.Run, input.Submitted.ID, input.Revision, unchanged) {
			return false, nil
		}
		completed = completed && t != nil && t.Revision == input.Revision && t.Status == "done" && acceptedTaskRevision(t) && unchanged
	}
	return !completed, nil
}

func forgetObligationsRecords(rd *recordReader) error {
	if rd.journalOnly {
		return nil
	}
	outstanding, err := releaseCandidate(rd, sessions.CandidateIntegrationForget, "", func(key sessions.RecordKey) (bool, error) {
		c, err := getRecord[IntegrationCandidate](rd, "integration", key.ID)
		if err != nil {
			return false, err
		}
		return forgetCandidateOutstanding(rd, c)
	})
	if err != nil {
		return err
	}
	if outstanding {
		return errors.New("complete or supersede integration candidates before forgetting snapshots")
	}
	q := sessions.RecordQuery{Index: sessions.CoordinationIndexTaskStatus, Key: "awaiting_review", Limit: 32}
	for {
		page, err := rd.Tasks(q)
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			if record.Value.Status == "awaiting_review" && record.Value.Snapshot != "" {
				return errors.New("resolve submitted task snapshots before forgetting them")
			}
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == q.After {
			return sessions.ErrCoordinationCursor
		}
		q.After = page.Next
	}
}
