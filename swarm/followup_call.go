package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/workflow"
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
	s, err := r.read(readCtx)
	if err != nil {
		return nil, err
	}
	return followupView(s, mail), nil
}

func followupView(s *State, mail *Mail) *FollowupView {
	v := &FollowupView{Member: mail.To, Message: mail.ID, Note: "Historical launch provenance is unavailable; inspect list_agents({details:true})."}
	if f := s.Followups[mail.ID]; f != nil {
		v.Operation, v.Task, v.Execution = f.Operation, f.Task, f.Execution
		v.BaseCommit, v.BaseOrigin, v.Source = snapshotCommit(s, f.Base), f.BaseOrigin, f.Source
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
// resume. A receipt already bound to an execution never follows later work.
func bindFollowups(s *State, m *Member, t *Task, e *Execution) {
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
	}
}

func refreshBrief(s *State, f *FollowupCall) string {
	if f.Source != "" {
		return "Your new assignment continues observing the same live source " + f.Source + ". Earlier conversation may describe older file contents.\n\n"
	}
	baseline := "the parent capture selected for this refresh"
	if commit := snapshotCommit(s, f.Base); commit != "" {
		baseline = "parent commit " + commit + ", captured for this refresh"
	}
	return fmt.Sprintf("Your new assignment starts from %s. This baseline supersedes earlier file descriptions in the conversation; inspect the files in your assigned worktree.\n\n", baseline)
}

func (r *Runtime) failFollowup(ctx context.Context, id string, cause error) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return r.update(finishCtx, func(s *State) error {
		mail, f := s.Messages[id], s.Followups[id]
		if mail == nil || f != nil && f.Phase == "launched" && f.Operation != "steer" {
			return nil
		}
		mail.Start = false
		if f != nil && f.Refresh && (errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)) {
			f.Phase = "interrupted"
			return nil
		}
		mail.StartError = cause.Error()
		if f != nil {
			f.Phase = "failed"
			var detail *workflow.Error
			if errors.As(cause, &detail) {
				f.ErrorCode = detail.Code
			}
		}
		return nil
	})
}
