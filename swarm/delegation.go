package swarm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/sessions"
)

var taskNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func agentName(m *Member) string {
	if m.AgentName != "" {
		return m.AgentName
	}
	return "/root/agent_" + m.ID
}

func validateAgentName(s *State, name string) error {
	if !taskNamePattern.MatchString(name) {
		return errors.New("task_name must be 1-64 lowercase letters, digits or underscores, starting with a letter")
	}
	for _, m := range s.Members {
		if agentName(m) == "/root/"+name {
			return errors.New("task_name already exists; use followup_task to continue that worker")
		}
	}
	return nil
}

// resolveTarget accepts stable session IDs and canonical or sibling names.
// There are no nested workers in Polly; all names belong to this root family.
func (r *Runtime) resolveTarget(s *State, actor, target string) (string, error) {
	if err := member(s, r.ID, actor); err != nil {
		return "", err
	}
	if target == r.ID || target == "/root" || actor != r.ID && target == ".." {
		return r.ID, nil
	}
	if m := s.Members[target]; m != nil {
		return m.ID, nil
	}
	name := target
	if !strings.HasPrefix(name, "/") {
		name = "/root/" + name
	}
	for _, m := range s.Members {
		if agentName(m) == name {
			return m.ID, nil
		}
	}
	return "", errors.New("unknown agent target; use list_agents for names and IDs")
}

func (reader *recordReader) validateAgentName(name string) error {
	if !taskNamePattern.MatchString(name) {
		return errors.New("task_name must be 1-64 lowercase letters, digits or underscores, starting with a letter")
	}
	page, err := reader.Members(sessions.RecordQuery{Index: sessions.CoordinationIndexMemberName, Key: "/root/" + name, Limit: 1})
	if err != nil {
		return err
	}
	if len(page.Records) != 0 {
		return errors.New("task_name already exists; use followup_task to continue that worker")
	}
	return nil
}

func (reader *recordReader) resolveTarget(parent, actor, target string) (string, error) {
	if actor != parent {
		m, err := reader.Member(actor)
		if err != nil {
			return "", err
		}
		if m == nil {
			return "", fail("unknown_member", "caller is not a member of this swarm")
		}
	}
	if target == parent || target == "/root" || actor != parent && target == ".." {
		return parent, nil
	}
	if target != "" {
		m, err := reader.Member(target)
		if err != nil {
			return "", err
		}
		if m != nil {
			return m.ID, nil
		}
	}
	name := target
	if !strings.HasPrefix(name, "/") {
		name = "/root/" + name
	}
	page, err := reader.Members(sessions.RecordQuery{Index: sessions.CoordinationIndexMemberName, Key: name, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(page.Records) != 0 {
		return page.Records[0].Value.ID, nil
	}
	return "", errors.New("unknown agent target; use list_agents for names and IDs")
}

func launchRefusalRecords(reader *recordReader, m *Member, resume, retry bool) error {
	if m == nil {
		return errors.New("unknown member session")
	}
	if m.Control == MemberControlStopped && !resume {
		return errors.New("member is stopped; explicit resume required")
	}
	if m.Execution == "" {
		return nil
	}
	e, err := reader.Execution(m.Execution)
	if err != nil || e == nil {
		return err
	}
	if err := e.RetryPause.resumeError(); err != nil {
		return err
	}
	switch e.Status {
	case "queued", "running", "waiting":
		return fail("session_busy", "member already has an active execution")
	case "paused":
		if !resume {
			return errors.New("member is paused; explicit resume or takeover required")
		}
	case "failed":
		if !resume && !retry {
			return errors.New("member's last execution failed; explicit resume or takeover required, or a continuation by the workflow that launched it")
		}
	}
	return nil
}

func pendingFollowup(s *State, memberID string) bool {
	for _, mail := range s.Messages {
		if mail.To == memberID && mail.Start && !mail.Delivered {
			return true
		}
	}
	return false
}

// FollowupTask steers an active execution or explicitly starts an idle member.
// The start intent and the message share a durable receipt, but reading it
// cannot consume either. Only an input checkpoint marks the receipt delivered.
func (r *Runtime) FollowupTask(ctx context.Context, target, message, callID string) (*Mail, error) {
	return r.followupTask(ctx, target, message, callID, false)
}

func (r *Runtime) followupTask(ctx context.Context, target, message, callID string, refresh bool) (*Mail, error) {
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("message is required")
	}
	if refresh {
		return r.refreshFollowup(ctx, target, message, callID)
	}
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if r.closing || r.ctx.Err() != nil {
		return nil, context.Canceled
	}
	if err := r.prepare(ctx); err != nil {
		return nil, err
	}
	var id string
	var replay *Mail
	err := r.readRecords(ctx, func(reader *recordReader) error {
		var err error
		id, err = reader.resolveTarget(r.ID, r.ID, target)
		if err != nil {
			return err
		}
		if id == r.ID {
			return errors.New("followup_task requires a child target")
		}
		replay, err = reader.followupReplay(r.ID, id, message, callID, false)
		if replay != nil || err != nil {
			return err
		}
		if err := reader.refreshReservation(id, ""); err != nil {
			return err
		}
		m, err := reader.Member(id)
		if err != nil {
			return err
		}
		if err := userStopRefusal(m); err != nil {
			return err
		}
		if err := reader.unresolvedTaskApply(m.Task); err != nil {
			return err
		}
		if r.workflowReserved(m.Controller) {
			return fail("session_busy", "member is reserved by an active workflow; continue it through that workflow or wait for it to settle")
		}
		return nil
	})
	if replay != nil || err != nil {
		return replay, err
	}
	mail := &Mail{ID: ids.New(), From: r.ID, To: id, Kind: "info", Text: message, Start: true, CallID: callID, Posted: time.Now().UTC()}
	r.mu.Lock()
	if i := r.active[id]; i != nil && i.interrupted.Load() {
		mail.ResumeExecution = i.id
	}
	r.mu.Unlock()
	err = r.updateRecords(ctx, func(tx *recordTx) error {
		m, err := tx.Member(id)
		if err != nil {
			return err
		}
		if m == nil {
			return errors.New("unknown member")
		}
		var task *Task
		if m.Task != "" {
			task, err = tx.Task(m.Task)
			if err != nil {
				return err
			}
		}
		f := &FollowupCall{Member: id, Phase: "pending", PreviousTask: m.Task, Operation: "resume", BaseOrigin: followupOrigin(m, task)}
		if task != nil {
			f.PreviousRevision = task.Revision
		}
		r.mu.Lock()
		active := r.active[id]
		r.mu.Unlock()
		var activeExecution, previous *Execution
		if active != nil {
			activeExecution, err = tx.Execution(active.id)
			if err != nil {
				return err
			}
		}
		if m.Execution != "" {
			previous, err = tx.Execution(m.Execution)
			if err != nil {
				return err
			}
		}
		// The terminal checkpoint can win just before the invocation leaves
		// the active map. It can no longer consume steering input. Do not
		// issue a receipt claiming that completed execution will do this work.
		if active != nil {
			if e := activeExecution; e != nil && (e.Status == "completed" || e.Status == "failed" || e.Status == "paused" && !active.interrupted.Load()) {
				return fail("session_busy", "worker is finishing its execution; retry followup_task once it is idle")
			}
		}
		if active != nil && !active.interrupted.Load() {
			f.Phase, f.Operation, f.Task, f.Execution = "launched", "steer", m.Task, active.id
			f.BaseOrigin = "existing_workspace"
			if e := activeExecution; e != nil {
				f.Base, f.Source = e.Base, e.SourceRoot
			}
		} else if e := previous; e != nil {
			f.Task, f.Execution, f.Base, f.Source = m.Task, e.ID, e.Base, e.SourceRoot
		}
		if f.Source != "" {
			f.BaseOrigin = "live_source"
		}
		if err := tx.PutFollowup(mail.ID, f); err != nil {
			return err
		}
		return tx.PutMail(mail)
	})
	if err == nil {
		r.mu.Lock()
		i := r.active[id]
		r.mu.Unlock()
		if i == nil {
			err = r.startFollowupLocked(ctx, id)
		}
	}
	if err != nil {
		// Keep the information, but a refused launch must not spring to life
		// after an unrelated later event. Another explicit call can retry.
		return nil, errors.Join(err, r.failFollowup(ctx, mail.ID, err))
	}
	return mail, nil
}

func (r *Runtime) startFollowupLocked(ctx context.Context, memberID string) error {
	var m *Member
	var e *Execution
	var pending bool
	err := r.readRecords(ctx, func(reader *recordReader) error {
		var err error
		m, err = reader.Member(memberID)
		if err != nil || m == nil {
			return err
		}
		pending, err = reader.pendingFollowup(memberID)
		if err != nil || !pending {
			return err
		}
		if err := userStopRefusal(m); err != nil {
			return err
		}
		if err := reader.refreshReservation(memberID, ""); err != nil {
			return err
		}
		if m.Execution != "" {
			e, err = reader.Execution(m.Execution)
		}
		return err
	})
	if err != nil || m == nil || !pending {
		return err
	}
	if r.workflowReserved(m.Controller) {
		return fail("session_busy", "member is reserved by an active workflow")
	}
	req := AgentRequest{Session: memberID, Task: "Continue your assignment with the follow-up from your parent below. Retain relevant prior findings and report the resulting work."}
	if e != nil {
		if e.Status == "paused" {
			return r.continueExecution(ctx, memberID, e.ID, 0, 0, false)
		}
		// Managed follow-ups retain the worker's typed completion contract.
		// Direct Agent callers still choose the schema for each execution.
		req.Schema = e.Request.Schema
	}
	_, err = r.startLocked(ctx, "", req, launchIntent{resume: true})
	return err
}

// InterruptAgent cancels this turn without stopping the member or canceling
// its task. The interrupt flag orders concurrent follow-ups and fences late
// finals. Never join a worker while holding a lock its host callbacks may use.
func (r *Runtime) InterruptAgent(ctx context.Context, target string) (any, error) {
	r.launchMu.Lock()
	locked := true
	defer func() {
		if locked {
			r.launchMu.Unlock()
		}
	}()
	var id string
	var m *Member
	var e *Execution
	err := r.readRecords(ctx, func(reader *recordReader) error {
		var err error
		id, err = reader.resolveTarget(r.ID, r.ID, target)
		if err != nil {
			return err
		}
		if id == r.ID {
			return errors.New("interrupt_agent requires a child target")
		}
		m, err = reader.Member(id)
		if err != nil {
			return err
		}
		if m.Execution != "" {
			e, err = reader.Execution(m.Execution)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if r.workflowReserved(m.Controller) {
		return nil, fail("session_busy", "member is reserved by an active workflow; cancel the workflow before interrupting its workers")
	}
	previous := delegationStatusExecution(e)
	r.mu.Lock()
	i := r.active[id]
	if i != nil {
		i.interrupted.Store(true)
		i.cancel()
	}
	r.mu.Unlock()
	r.launchMu.Unlock()
	locked = false
	if i != nil {
		select {
		case <-i.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]any{"previous_status": previous}, nil
}

// recordFollowupFailure makes a queued launch refusal visible and prevents an
// unrelated future event from retrying it. Information stays in the mailbox.
func (r *Runtime) recordFollowupFailure(memberID string, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Second)
	defer cancel()
	_ = r.updateRecords(ctx, func(tx *recordTx) error {
		found := false
		query := sessions.RecordQuery{Index: sessions.CoordinationIndexMailPendingStartRecipient, Key: memberID, Limit: 100}
		for {
			page, err := tx.Mails(query)
			if err != nil {
				return err
			}
			for _, record := range page.Records {
				mail := record.Value
				mail.Start, mail.StartError = false, cause.Error()
				f, err := tx.Followup(mail.ID)
				if err != nil {
					return err
				}
				if f != nil && f.Phase != "launched" {
					f.Phase = "failed"
					if err := tx.PutFollowup(mail.ID, f); err != nil {
						return err
					}
				}
				if err := tx.PutMail(mail); err != nil {
					return err
				}
				found = true
			}
			if page.Next == "" {
				break
			}
			query.After = page.Next
		}
		if found {
			notice := &Mail{ID: ids.New(), From: r.ID, To: r.ID, Kind: "info", Text: "Follow-up for " + memberID + " could not start: " + cause.Error() + ". Resolve the cause before retrying with followup_task.", Posted: time.Now().UTC()}
			return tx.PutMail(notice)
		}
		return nil
	})
}

func delegationStatus(s *State, m *Member) any {
	return delegationStatusExecution(s.Executions[m.Execution])
}

func delegationStatusExecution(e *Execution) any {
	if e != nil {
		switch e.Status {
		case "queued":
			return "pending_init"
		case "running", "waiting":
			return "running"
		case "paused":
			return "interrupted"
		case "failed":
			return map[string]any{"errored": clipInspection(e.Error, 512)}
		case "completed":
			return map[string]any{"completed": nil}
		}
	}
	return "pending_init"
}
