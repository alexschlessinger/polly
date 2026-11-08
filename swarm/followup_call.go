package swarm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"sort"
)

// FollowupCall is launch provenance, not completion evidence. It is stored in
// its own record keyed by message ID; Mail.Task/Execution remain delivery-only.
type FollowupCall struct {
	Member           string `json:"member"`
	Refresh          bool   `json:"refresh"`
	Phase            string `json:"phase"`
	PreviousTask     string `json:"previousTask,omitempty"`
	PreviousRevision int    `json:"previousRevision,omitempty"`
	Operation        string `json:"operation,omitempty"`
	Task             string `json:"task,omitempty"`
	Execution        string `json:"execution,omitempty"`
	Base             string `json:"base,omitempty"`
	BaseOrigin       string `json:"baseOrigin,omitempty"`
	Source           string `json:"source,omitempty"`
	ErrorCode        string `json:"errorCode,omitempty"`
	// HeldScratch is where a refresh parked the worker's previous scratch
	// between releasing the old workspace and creating the new one; the
	// launch adopts it into the new scratch and clears this. ScratchCarry
	// records the outcome for the brief: "carried" or "lost".
	HeldScratch  string `json:"heldScratch,omitempty"`
	ScratchCarry string `json:"scratchCarry,omitempty"`
}

// FollowupView deliberately excludes both the brief and internal capture IDs.
type FollowupView struct {
	Member     string `json:"member"`
	Message    string `json:"message"`
	Operation  string `json:"operation,omitempty"`
	Task       string `json:"task,omitempty"`
	Execution  string `json:"execution,omitempty"`
	BaseCommit string `json:"baseCommit,omitempty"`
	BaseOrigin string `json:"baseOrigin,omitempty"`
	Source     string `json:"source,omitempty"`
	Note       string `json:"note"`
}

func (r *Runtime) followupResult(ctx context.Context, mail *Mail) (*FollowupView, error) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var view *FollowupView
	err := r.readRecords(readCtx, func(reader *recordReader) error {
		var err error
		view, err = reader.followupView(mail)
		return err
	})
	return view, err
}

func followupView(s *State, mail *Mail) *FollowupView {
	f := s.Followups[mail.ID]
	commit := ""
	if f != nil {
		commit = snapshotCommit(s, f.Base)
	}
	return followupViewValues(mail, f, commit)
}

func followupViewValues(mail *Mail, f *FollowupCall, commit string) *FollowupView {
	v := &FollowupView{Member: mail.To, Message: mail.ID, Note: "Historical launch provenance is unavailable; inspect list_agents({details:true})."}
	if f != nil {
		v.Operation, v.Task, v.Execution = f.Operation, f.Task, f.Execution
		v.BaseCommit, v.BaseOrigin, v.Source = commit, f.BaseOrigin, f.Source
		switch f.BaseOrigin {
		case "parent":
			v.Note = "Uses this call's parent capture; later parent edits are excluded."
		case "previous_result":
			v.Note = "Uses the worker's previous result. refresh:true selects current parent files after work settles."
		case "original_baseline":
			v.Note = "Uses the original baseline. refresh:true selects current parent files after work settles."
		case "live_source":
			v.Note = "Observes the saved live source; no immutable baseline."
		default:
			v.Note = "Continues the workspace, including worker edits beyond its baseline. Parent changes are not copied."
		}
	}
	return v
}

func followupReplay(s *State, parent, member, message, callID string, refresh bool) (*Mail, error) {
	if callID == "" {
		return nil, nil
	}
	for _, mail := range s.Messages {
		if mail.From != parent || mail.CallID != callID {
			continue
		}
		f := s.Followups[mail.ID]
		if mail.To != member || mail.Text != message || refresh != (f != nil && f.Refresh) {
			return nil, errors.New("follow-up call ID already used with different arguments")
		}
		if mail.StartError != "" {
			if f != nil && f.ErrorCode != "" {
				return nil, fail(f.ErrorCode, mail.StartError)
			}
			return nil, errors.New(mail.StartError)
		}
		return mail, nil
	}
	return nil, nil
}

func refreshReservation(s *State, member, except string) error {
	for id, f := range s.Followups {
		if id != except && f.Member == member && f.Refresh && f.Phase == "preparing" {
			return fail("session_busy", "member has a refresh in preparation; retry after it finishes")
		}
	}
	return nil
}

func followupOrigin(m *Member, task *Task) string {
	if task != nil && task.Status == "done" {
		if !m.ReadOnly && task.Snapshot != "" {
			return "previous_result"
		}
		return "original_baseline"
	}
	return "existing_workspace"
}

// Bind pending ordinary follow-ups in the same transaction as assignment or
// resume, and hand them back in posting order so the launch can put their
// text into the brief. A receipt already bound to an execution never follows
// later work.
func bindFollowups(s *State, m *Member, t *Task, e *Execution) []*Mail {
	var bound []*Mail
	for id, f := range s.Followups {
		mail := s.Messages[id]
		if f.Refresh || f.Member != m.ID || f.Phase != "pending" || mail == nil || !mail.Start || mail.Delivered {
			continue
		}
		f.Task, f.Execution, f.Base, f.Source = t.ID, e.ID, e.Base, e.SourceRoot
		f.Phase = "launched"
		if t.ID != f.PreviousTask {
			f.Operation = "new_task"
		} else {
			f.Operation = "resume"
			f.BaseOrigin = "existing_workspace"
		}
		if f.Source != "" {
			f.BaseOrigin = "live_source"
		}
		bound = append(bound, mail)
	}
	sort.Slice(bound, func(i, j int) bool {
		if bound[i].Posted.Equal(bound[j].Posted) {
			return bound[i].ID < bound[j].ID
		}
		return bound[i].Posted.Before(bound[j].Posted)
	})
	return bound
}

func refreshBrief(s *State, f *FollowupCall) string {
	return refreshBriefValues(f, snapshotCommit(s, f.Base))
}

func refreshBriefValues(f *FollowupCall, commit string) string {
	var text string
	if f.Source != "" {
		text = "Your new assignment continues observing the same live source " + f.Source + ". Earlier conversation may describe older file contents."
	} else {
		baseline := "the parent capture selected for this refresh"
		if commit != "" {
			baseline = "parent commit " + commit + ", captured for this refresh"
		}
		text = fmt.Sprintf("Your new assignment starts from %s. This baseline supersedes earlier file descriptions in the conversation; inspect the files in your assigned worktree.", baseline)
	}
	switch f.ScratchCarry {
	case "carried":
		text += " Your previous scratch directory was carried over into this workspace's scratch, so the private helpers and caches you kept there are still available."
	case "lost":
		text += " Your previous scratch directory could not be carried over and was deleted; this workspace's scratch starts empty, so recreate any private helpers you need."
	}
	return text + "\n\n"
}

// These selectors contain only this call/member's dependencies. They do not
// turn missing records in an invented partial State into lifecycle authority.
func (reader *recordReader) followupView(mail *Mail) (*FollowupView, error) {
	f, err := reader.Followup(mail.ID)
	if err != nil {
		return nil, err
	}
	commit := ""
	if f != nil {
		commit, err = reader.lifecycleSnapshotCommit(f.Base)
		if err != nil {
			return nil, err
		}
	}
	return followupViewValues(mail, f, commit), nil
}

func (reader *recordReader) lifecycleSnapshotCommit(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	snapshot, err := reader.Snapshot(id)
	if err != nil {
		return "", err
	}
	if validTaskSnapshot(snapshot, id) {
		return snapshot.Commit, nil
	}
	return "", nil
}

func (reader *recordReader) followupReplay(parent, member, message, callID string, refresh bool) (*Mail, error) {
	if callID == "" {
		return nil, nil
	}
	page, err := reader.Mails(sessions.RecordQuery{Index: sessions.CoordinationIndexMailFromCall, Key: sessions.CoordinationIndexKey(parent, callID), Limit: 1})
	if err != nil || len(page.Records) == 0 {
		return nil, err
	}
	mail := page.Records[0].Value
	f, err := reader.Followup(mail.ID)
	if err != nil {
		return nil, err
	}
	if mail.To != member || mail.Text != message || refresh != (f != nil && f.Refresh) {
		return nil, errors.New("follow-up call ID already used with different arguments")
	}
	if mail.StartError != "" {
		if f != nil && f.ErrorCode != "" {
			return nil, fail(f.ErrorCode, mail.StartError)
		}
		return nil, errors.New(mail.StartError)
	}
	return mail, nil
}

func (reader *recordReader) refreshReservation(member, except string) error {
	// At most the exempt reservation and one conflicting reservation are needed.
	page, err := queryRecords[FollowupCall](reader, "followup", sessions.RecordQuery{Index: sessions.CoordinationIndexFollowupPreparingMember, Key: member, Limit: 2})
	if err != nil {
		return err
	}
	for _, record := range page.Records {
		if record.Key.ID != except {
			return fail("session_busy", "member has a refresh in preparation; retry after it finishes")
		}
	}
	return nil
}

func (reader *recordReader) pendingFollowup(member string) (bool, error) {
	page, err := reader.Mails(sessions.RecordQuery{Index: sessions.CoordinationIndexMailPendingStartRecipient, Key: member, Limit: 1})
	return len(page.Records) != 0, err
}

func (tx *recordTx) bindFollowups(m *Member, t *Task, e *Execution) ([]*Mail, error) {
	var bound []*Mail
	query := sessions.RecordQuery{Index: sessions.CoordinationIndexMailPendingStartRecipient, Key: m.ID, Limit: 100}
	for {
		page, err := tx.Mails(query)
		if err != nil {
			return nil, err
		}
		for _, record := range page.Records {
			mail := record.Value
			f, err := tx.Followup(mail.ID)
			if err != nil {
				return nil, err
			}
			if f == nil || f.Refresh || f.Member != m.ID || f.Phase != "pending" {
				continue
			}
			f.Task, f.Execution, f.Base, f.Source = t.ID, e.ID, e.Base, e.SourceRoot
			f.Phase = "launched"
			if t.ID != f.PreviousTask {
				f.Operation = "new_task"
			} else {
				f.Operation, f.BaseOrigin = "resume", "existing_workspace"
			}
			if f.Source != "" {
				f.BaseOrigin = "live_source"
			}
			if err := tx.PutFollowup(mail.ID, f); err != nil {
				return nil, err
			}
			bound = append(bound, mail)
		}
		if page.Next == "" {
			break
		}
		query.After = page.Next
	}
	// Backends agree on posting order; retain the tie-break explicitly for
	// compatibility stores implementing the selector themselves.
	sort.Slice(bound, func(i, j int) bool {
		if bound[i].Posted.Equal(bound[j].Posted) {
			return bound[i].ID < bound[j].ID
		}
		return bound[i].Posted.Before(bound[j].Posted)
	})
	return bound, nil
}

func (reader *recordReader) followupProvenance(mail *Mail) (string, error) {
	f, err := reader.Followup(mail.ID)
	if err != nil || f == nil || f.Phase != "launched" {
		return "", err
	}
	commit, err := reader.lifecycleSnapshotCommit(f.Base)
	if err != nil {
		return "", err
	}
	view := followupViewValues(mail, f, commit)
	text := fmt.Sprintf("Follow-up %s: %s, task %s, execution %s; baseline origin: %s.", mail.ID, view.Operation, view.Task, view.Execution, view.BaseOrigin)
	if view.BaseCommit != "" {
		text += " Baseline commit: " + view.BaseCommit + "."
	}
	if view.Source != "" {
		text += " Live source: " + view.Source + "."
	}
	return text + " " + view.Note, nil
}

func (reader *recordReader) refreshBrief(f *FollowupCall) (string, error) {
	commit, err := reader.lifecycleSnapshotCommit(f.Base)
	if err != nil {
		return "", err
	}
	return refreshBriefValues(f, commit), nil
}

func (r *Runtime) failFollowup(ctx context.Context, id string, cause error) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	held := ""
	err := r.updateRecords(finishCtx, func(tx *recordTx) error {
		mail, err := tx.Mail(id)
		if err != nil {
			return err
		}
		f, err := tx.Followup(id)
		if err != nil {
			return err
		}
		if mail == nil || f != nil && f.Phase == "launched" && f.Operation != "steer" {
			return nil
		}
		mail.Start = false
		if f != nil && f.Refresh && (errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)) {
			// An interrupted refresh keeps its parked scratch for the retry.
			f.Phase = "interrupted"
		} else {
			mail.StartError = cause.Error()
			if f != nil {
				f.Phase = "failed"
				var detail *workflow.Error
				if errors.As(cause, &detail) {
					f.ErrorCode = detail.Code
				}
				held, f.HeldScratch = f.HeldScratch, ""
			}
		}
		if err := tx.PutMail(mail); err != nil {
			return err
		}
		if f != nil {
			return tx.PutFollowup(id, f)
		}
		return nil
	})
	if held != "" {
		// A failed refresh has no workspace to adopt the parked scratch.
		if e := scratch.Release(held); e != nil {
			slog.Warn("refresh_held_scratch_retained", "path", held, "error", e)
		}
	}
	return err
}
