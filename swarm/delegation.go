package swarm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
)

// DelegationContract pins the ordinary coordination contract. Workspace,
// authority, durable task review and budget policy remain Polly's own.
const DelegationContract = "openai/codex@b979d4f1f04538ba5a5fcc434d499c007bfe1b8c"

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
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	id, err := r.resolveTarget(s, r.ID, target)
	if err != nil {
		return nil, err
	}
	if id == r.ID {
		return nil, errors.New("followup_task requires a child target")
	}
	if mail, err := followupReplay(s, r.ID, id, message, callID, false); mail != nil || err != nil {
		return mail, err
	}
	if err := refreshReservation(s, id, ""); err != nil {
		return nil, err
	}
	m := s.Members[id]
	if err := unresolvedTaskApply(s, m.Task); err != nil {
		return nil, err
	}
	if r.workflowReserved(m.Controller) {
		return nil, fail("session_busy", "member is reserved by an active workflow; continue it through that workflow or wait for it to settle")
	}
	mail := &Mail{ID: ids.New(), From: r.ID, To: id, Kind: "info", Text: message, Start: true, CallID: callID, Posted: time.Now().UTC()}
	r.mu.Lock()
	if i := r.active[id]; i != nil && i.interrupted.Load() {
		mail.ResumeExecution = i.id
	}
	r.mu.Unlock()
	if err = r.update(ctx, func(s *State) error {
		m := s.Members[id]
		if m == nil {
			return errors.New("unknown member")
		}
		f := &FollowupCall{Member: id, Phase: "pending", PreviousTask: m.Task, Operation: "resume", BaseOrigin: followupOrigin(m, s.Tasks[m.Task])}
		if task := s.Tasks[m.Task]; task != nil {
			f.PreviousRevision = task.Revision
		}
		r.mu.Lock()
		active := r.active[id]
		r.mu.Unlock()
		// The terminal checkpoint can win just before the invocation leaves
		// the active map. It can no longer consume steering input. Do not
		// issue a receipt claiming that completed execution will do this work.
		if active != nil {
			if e := s.Executions[active.id]; e != nil && (e.Status == "completed" || e.Status == "failed" || e.Status == "paused" && !active.interrupted.Load()) {
				return fail("session_busy", "worker is finishing its execution; retry followup_task once it is idle")
			}
		}
		if active != nil && !active.interrupted.Load() {
			f.Phase, f.Operation, f.Task, f.Execution = "launched", "steer", m.Task, active.id
			f.BaseOrigin = "existing_workspace"
			if e := s.Executions[active.id]; e != nil {
				f.Base, f.Source = e.Base, e.SourceRoot
			}
		} else if e := s.Executions[m.Execution]; e != nil {
			f.Task, f.Execution, f.Base, f.Source = m.Task, e.ID, e.Base, e.SourceRoot
		}
		if f.Source != "" {
			f.BaseOrigin = "live_source"
		}
		s.Followups[mail.ID] = f
		s.Messages[mail.ID] = mail
		return nil
	}); err != nil {
		return nil, errors.Join(err, r.failFollowup(ctx, mail.ID, err))
	}
	r.mu.Lock()
	i := r.active[id]
	r.mu.Unlock()
	if err == nil && i == nil {
		err = r.startFollowupLocked(ctx, id)
	}
	if err != nil {
		// Keep the information, but a refused launch must not spring to life
		// after an unrelated later event. Another explicit call can retry.
		rollback := r.failFollowup(ctx, mail.ID, err)
		return nil, errors.Join(err, rollback)
	}
	r.changed()
	return mail, nil
}

func (r *Runtime) startFollowupLocked(ctx context.Context, memberID string) error {
	s, err := r.read(ctx)
	if err != nil {
		return err
	}
	m := s.Members[memberID]
	if m == nil || !pendingFollowup(s, memberID) {
		return nil
	}
	if err := refreshReservation(s, memberID, ""); err != nil {
		return err
	}
	if r.workflowReserved(m.Controller) {
		return fail("session_busy", "member is reserved by an active workflow")
	}
	if e := s.Executions[m.Execution]; e != nil && e.Status == "paused" {
		return r.continueExecution(ctx, memberID, e.ID, 0, 0)
	}
	_, err = r.startLocked(ctx, "", AgentRequest{Session: memberID, Task: "Continue your assignment using the explicitly requested follow-up in your addressed input. Retain relevant prior findings and report the resulting work."}, launchIntent{resume: true})
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
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	id, err := r.resolveTarget(s, r.ID, target)
	if err != nil {
		return nil, err
	}
	if id == r.ID {
		return nil, errors.New("interrupt_agent requires a child target")
	}
	m := s.Members[id]
	if r.workflowReserved(m.Controller) {
		return nil, fail("session_busy", "member is reserved by an active workflow; cancel the workflow before interrupting its workers")
	}
	previous := delegationStatus(s, m)
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
	_ = r.update(ctx, func(s *State) error {
		found := false
		for _, mail := range s.Messages {
			if mail.To == memberID && mail.Start && !mail.Delivered {
				mail.Start, mail.StartError = false, cause.Error()
				if f := s.Followups[mail.ID]; f != nil && f.Phase != "launched" {
					f.Phase = "failed"
				}
				found = true
			}
		}
		if found {
			notice := &Mail{ID: ids.New(), From: r.ID, To: r.ID, Kind: "info", Text: "Follow-up for " + memberID + " could not start: " + cause.Error() + ". Resolve the cause before retrying with followup_task.", Posted: time.Now().UTC()}
			s.Messages[notice.ID] = notice
		}
		return nil
	})
}

func delegationStatus(s *State, m *Member) any {
	if e := s.Executions[m.Execution]; e != nil {
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
