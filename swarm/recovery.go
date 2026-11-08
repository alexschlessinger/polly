package swarm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func recoveryRoot(rd *recordReader) error {
	identity, err := rd.Identity()
	if err != nil {
		return err
	}
	if identity.ActorID != identity.ParentID {
		return errors.New("only a root session can own a swarm")
	}
	return nil
}

// Recovery bypasses prepare recursion, not lease/format checks. A successful
// commit still publishes its exact scopes. Never replay an uncertain callback.
func (r *Runtime) recoveryUpdate(ctx context.Context, fn func(*recordTx) error) error {
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	commit, err := repo.Update(ctx, func(tx *recordTx) error {
		if err := recoveryRoot(tx.recordReader); err != nil {
			return err
		}
		return fn(tx)
	})
	if err == nil {
		r.committedRecords(commit)
	}
	return err
}

// recoveryRecords holds one bounded candidate page outside the read callback,
// allowing each obligation and its receipts to commit together without an
// unbounded transaction write set. Selectors must have stable record ordering;
// mutations may remove the current candidate but cannot move its key backward.
func (r *Runtime) recoveryRecords(ctx context.Context, query sessions.RecordQuery, candidate sessions.CoordinationCandidateSelector, visit func(sessions.CoordinationRecord) error) error {
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	repo.parentJournal = true
	query.Limit, query.MaxBytes = recoveryPageLimit, recoveryPageBytes
	for {
		var page sessions.RecordPage
		err := repo.Read(ctx, func(rd *recordReader) error {
			if rd.journalOnly {
				return nil // no swarm records exist before lazy bootstrap
			}
			var err error
			if candidate != "" {
				page, err = sessions.QueryCoordinationCandidates(rd.raw, sessions.CoordinationCandidateQuery{Selector: candidate, Key: query.Key, After: query.After, Limit: query.Limit, MaxBytes: query.MaxBytes})
			} else {
				page, err = rd.raw.Query(query)
			}
			return err
		})
		if err != nil {
			return err
		}
		for _, record := range page.Records {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(record); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == query.After {
			return sessions.ErrCoordinationCursor
		}
		query.After = page.Next
	}
}

func (r *Runtime) bootstrapRecoveryFormat(ctx context.Context) error {
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	repo.parentJournal = true
	missing := false
	if err := repo.Read(ctx, func(rd *recordReader) error {
		missing = rd.journalOnly
		return recoveryRoot(rd)
	}); err != nil || !missing {
		return err
	}
	commit, err := repo.access.UpdateRecords(ctx, func(raw sessions.CoordinationTx) error {
		rd := &recordReader{raw: raw, ctx: ctx, active: true, forWrite: true}
		defer func() { rd.active = false }()
		if err := recoveryRoot(rd); err != nil {
			return err
		}
		if err := rd.checkFormatFor(true); err != nil {
			return err
		}
		if !rd.journalOnly {
			return nil
		}
		return raw.Put(sessions.RecordKey{Kind: formatKind, ID: formatID}, []byte(`{"version":2}`))
	})
	if err == nil {
		r.committedRecords(commit)
	}
	return err
}

func (r *Runtime) prepareRecoveryRecords(ctx context.Context) error {
	if err := r.bootstrapRecoveryFormat(ctx); err != nil {
		return err
	}
	// Members and contexts are the actual sandbox authority set. Enumerating
	// it is necessary, but unchanged records and arbitrary historical payloads
	// are never rewritten. Context roots remain intact for sibling masks.
	if err := r.recoveryRecords(ctx, sessions.RecordQuery{Kind: "member"}, "", func(record sessions.CoordinationRecord) error {
		return r.recoveryUpdate(ctx, func(tx *recordTx) error {
			m, err := tx.Member(record.Key.ID)
			if err != nil || m == nil {
				return err
			}
			var c *ExecutionContext
			if m.Context != "" {
				c, err = tx.Context(m.Context)
				if err != nil {
					return err
				}
			}
			presets, err := r.restoredAgentSandbox(m, c)
			if err != nil {
				return err
			}
			changed := m.AgentName == "" || !slices.Equal(m.SandboxPresets, presets)
			if m.AgentName == "" {
				m.AgentName = agentName(m)
			}
			m.SandboxPresets = presets
			if changed {
				if err := tx.PutMember(m); err != nil {
					return err
				}
			}
			if c != nil && !slices.Equal(c.SandboxPresets, presets) {
				c.SandboxPresets = presets
				return tx.PutContext(c)
			}
			return nil
		})
	}); err != nil {
		return err
	}
	for _, status := range []string{"running", "waiting", "queued"} {
		if err := r.recoveryRecords(ctx, sessions.RecordQuery{Index: sessions.CoordinationIndexExecutionStatus, Key: status}, "", func(record sessions.CoordinationRecord) error {
			selected, err := decodeRecord[Execution](record, false)
			if err != nil {
				return err
			}
			return r.recoveryUpdate(ctx, func(tx *recordTx) error {
				e, err := tx.Execution(record.Key.ID)
				if err != nil || e == nil || e.Generation != selected.Generation || e.Status != status {
					return err
				}
				e.Generation++
				e.Status = "paused"
				e.Ended = time.Now().UTC()
				e.Error = "process interrupted; explicit resume required"
				return tx.PutExecution(e)
			})
		}); err != nil {
			return err
		}
	}
	if err := r.recoveryRecords(ctx, sessions.RecordQuery{}, sessions.CandidateFollowupPreparing, func(record sessions.CoordinationRecord) error {
		return r.recoveryUpdate(ctx, func(tx *recordTx) error {
			f, err := tx.Followup(record.Key.ID)
			if err != nil || f == nil || !f.Refresh || f.Phase != "preparing" {
				return err
			}
			f.Phase = "interrupted"
			if err := tx.PutFollowup(record.Key.ID, f); err != nil {
				return err
			}
			mail, err := tx.Mail(record.Key.ID)
			if err != nil || mail == nil || !mail.Start {
				return err
			}
			mail.Start = false
			return tx.PutMail(mail)
		})
	}); err != nil {
		return err
	}
	if err := r.recoveryRecords(ctx, sessions.RecordQuery{}, sessions.CandidateWorkflowRunning, func(record sessions.CoordinationRecord) error {
		return r.recoveryUpdate(ctx, func(tx *recordTx) error {
			w, err := tx.WorkflowHeader(record.Key.ID)
			if err != nil || w == nil || w.Status != "running" {
				return err
			}
			w.Status = "interrupted"
			w.Error = &workflow.Error{Code: "interrupted", Message: "JavaScript was interrupted; restart or take over its members explicitly"}
			text, err := recoveryWorkflowText(tx.recordReader, w)
			if err != nil {
				return err
			}
			if err := tx.PutWorkflowHeader(w); err != nil {
				return err
			}
			// Transition and notice are atomic. Reopening after any earlier
			// batch never increments a paused generation or posts this twice.
			return tx.PutMail(&Mail{ID: ids.New(), From: w.ID, To: r.ID, Workflow: w.ID, Kind: "info", Text: text, Posted: time.Now().UTC()})
		})
	}); err != nil {
		return err
	}
	if err := r.recoveryRecords(ctx, sessions.RecordQuery{Index: sessions.CoordinationIndexTaskOpen}, "", func(record sessions.CoordinationRecord) error {
		return r.recoveryUpdate(ctx, func(tx *recordTx) error {
			return r.ensureTaskDeliveryNoticeRecords(tx, record.Key.ID)
		})
	}); err != nil {
		return err
	}
	return nil
}

// pruneRecoveryScratch runs only after all recovery groups succeed, including
// constructor apply recovery. Its keep set is proportional to actual contexts
// and parked scratches, not completed executions, mail, reports, or steps.
func (r *Runtime) pruneRecoveryScratch(ctx context.Context) error {
	live := map[string]bool{}
	if err := r.recoveryRecords(ctx, sessions.RecordQuery{Kind: "context"}, "", func(record sessions.CoordinationRecord) error {
		c, err := decodeRecord[ExecutionContext](record, false)
		if err == nil && c.Checkout == nil && c.Scratch != "" {
			live[c.Scratch] = true
		}
		return err
	}); err != nil {
		return err
	}
	if err := r.recoveryRecords(ctx, sessions.RecordQuery{}, sessions.CandidateFollowupHeldScratch, func(record sessions.CoordinationRecord) error {
		f, err := decodeRecord[FollowupCall](record, false)
		if err == nil && f.HeldScratch != "" {
			live[f.HeldScratch] = true
		}
		return err
	}); err != nil {
		return err
	}
	if err := r.retainUncertainScratch(ctx, live); err != nil {
		return err
	}
	r.pruneLiveScratch(live)
	return nil
}

// recoveryWorkflowText matches workflowTerminalText without loading reports'
// steps or unrelated tasks. The only growing value is the required notice text
// itself (one output record), not a State or a map of historical dependencies.
func recoveryWorkflowText(rd *recordReader, w *workflow.ReportHeader) (string, error) {
	agents, unsettled := 0, 0
	query := sessions.RecordQuery{Index: sessions.CoordinationIndexExecutionWorkflow, Key: w.ID, Limit: recoveryPageLimit, MaxBytes: recoveryPageBytes}
	for {
		page, err := rd.Executions(query)
		if err != nil {
			return "", err
		}
		for _, record := range page.Records {
			agents++
			if record.Value.Status != "completed" {
				unsettled++
			}
		}
		if page.Next == "" {
			break
		}
		query.After = page.Next
	}
	name := clipInspection(w.Name, 512)
	if name == "" {
		name = w.ID
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Workflow %s %s: %d agents", name+" ("+w.ID+")", w.Status, agents)
	if unsettled > 0 {
		fmt.Fprintf(&text, ", %d failed or paused", unsettled)
	}
	text.WriteString(". Inspect: swarm_read({view: \"workflows\", id: \"" + w.ID + "\"}), then recover its unresolved work or report the failure and swarm_control({action: \"acknowledge_workflow\", id: \"" + w.ID + "\", defer: true, note: \"...\"}) to retain it.")
	candidate := sessions.CoordinationCandidateQuery{Selector: sessions.CandidateTaskWorkflowDelivery, Key: w.ID, Limit: recoveryPageLimit, MaxBytes: recoveryPageBytes}
	first := true
	for {
		page, err := sessions.QueryCoordinationCandidates(rd.raw, candidate)
		if err != nil {
			return "", err
		}
		for _, record := range page.Records {
			task, err := decodeRecord[Task](record, false)
			if err != nil {
				return "", err
			}
			if task.Delivery == nil || task.Delivery.Via != "workflow_step" || !strings.HasPrefix(task.Delivery.Ref, w.ID+"/") {
				continue
			}
			if first {
				text.WriteString(". Delivered to the script only: tasks ")
				first = false
			} else {
				text.WriteString(", ")
			}
			text.WriteString(task.ID)
		}
		if page.Next == "" {
			break
		}
		candidate.After = page.Next
	}
	if w.Error != nil {
		text.WriteString(" Reason: " + clipInspection(w.Error.Code+": "+w.Error.Message, 1024))
	}
	return text.String(), nil
}
