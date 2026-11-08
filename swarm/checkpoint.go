package swarm

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

func (r *Runtime) bindCheckpoint(session sessions.CoordinationSession, execution string, offset, generation int, cb *llm.AgentCallbacks, structured *structuredResultState, transcript *transcriptBinding) {
	var persisted int
	var sequence *int64
	var workflows workflowDeliveries
	repo, openErr := openRecordRepository(session)
	if repo != nil {
		repo.parentJournal = execution == ""
	}
	if execution == "" {
		prior := cb.OnToolResult
		cb.OnToolResult = func(call messages.ChatMessageToolCall, result messages.ChatMessage) {
			workflows.stage(call, result)
			if prior != nil {
				prior(call, result)
			}
		}
	}
	cb.AdmitInput = func(ctx context.Context) ([]messages.ChatMessage, error) {
		if openErr != nil {
			return nil, openErr
		}
		var batch admissionBatch
		err := repo.Read(ctx, func(reader *recordReader) error {
			if reader.journalOnly {
				return nil
			}
			var err error
			batch, err = reader.Admission(execution, &workflows)
			return err
		})
		return batch.Messages, err
	}
	cb.Checkpoint = func(ctx context.Context, checkpoint llm.AgentCheckpoint) error {
		if len(checkpoint.Generated) < persisted {
			return errors.New("checkpoint prefix regressed")
		}
		var pause *RetryPause
		if checkpoint.Final {
			pause = retryPauseFromError(checkpoint.Err)
		}
		if execution == "" && pause != nil {
			if err := r.promoteRetryPause(ctx); err != nil {
				return err
			}
		}
		// Host projection runs outside the display mutex and transaction. Its
		// durable count can differ from the raw generated acknowledgement.
		project := r.config.DurableMessages
		if project == nil {
			project = llm.StripDeniedExchanges
		}
		appended := project(checkpoint.Generated[persisted:])
		var pauseHistory []messages.ChatMessage
		if execution != "" && pause != nil {
			pauseHistory = project(checkpoint.Generated)
		}
		// The publications this checkpoint commits receipts for, reported as
		// events once the transaction holds.
		var read []string
		actor := ""
		var parentPause *RetryPause
		var committed sessions.CoordinationCommit
		err := transcript.checkpoint(checkpoint, func() (int64, error) {
			if openErr != nil {
				return 0, openErr
			}
			receipts := checkpointReceipts(appended)
			if structured != nil && structured.task != "" {
				receipts.TaskIDs = append(receipts.TaskIDs, structured.task)
			}
			commit, err := repo.Update(ctx, func(tx *recordTx) error {
				d, err := tx.Checkpoint(execution, receipts)
				if err != nil {
					return err
				}
				actor = d.Identity.ActorID
				effects := sessions.CoordinationEffects{Append: appended}
				if sequence == nil {
					// Preserve the FIRST attempt even after a lost commit reply.
					start := d.Identity.Sequence
					sequence = &start
				}
				effects.ExpectedSequence = sequence
				if execution != "" {
					e := d.Execution
					if e == nil || e.Member != actor || e.Generation != generation || e.Status == "paused" || e.Status == "completed" || e.Status == "failed" {
						return errors.New("execution checkpoint was fenced")
					}
					if e.InputSaved {
						effects.Transcript = &sessions.TranscriptIdentity{Execution: execution, Generation: generation}
					}
					e.Iterations = offset + checkpoint.Iterations
					if checkpoint.Request {
						e.Iterations++
					}
					e.Intent = nil
					e.Usage = mergeUsage(e.Usage, usageOf(checkpoint.Generated[persisted:]))
					if checkpoint.Final && errors.Is(checkpoint.Err, ErrYielded) {
						e.Status = "waiting"
					}
					if pause != nil {
						if err := r.checkpointRetryPauseRecords(tx, d, pause, pauseHistory, checkpoint.Err); err != nil {
							return err
						}
					}
					if structured != nil {
						if err := structured.applyCheckpointRecords(d, appended); err != nil {
							return err
						}
					}
					if err := tx.PutExecution(e); err != nil {
						return err
					}
				} else {
					switch {
					case pause != nil:
						d.ParentTurn = &ParentTurn{RetryPause: pause}
						if err := tx.PutParentTurn(actor, d.ParentTurn); err != nil {
							return err
						}
					case checkpoint.Request, d.ParentTurn != nil && d.ParentTurn.RetryPause == nil:
						d.ParentTurn = nil
						if err := tx.Delete(sessions.RecordKey{Kind: "parent_turn", ID: actor}); err != nil {
							return err
						}
					}
					if d.ParentTurn != nil {
						parentPause = d.ParentTurn.RetryPause
					}
				}
				for _, id := range receipts.MailIDs {
					mail := d.Mail[id]
					if mail == nil || mail.To != actor || mail.Delivered {
						return errors.New("mail admission changed")
					}
					mail.Delivered = true
					if execution == "" {
						if err := tx.recordDelivery(d, mail); err != nil {
							return err
						}
					}
					if err := tx.PutMail(mail); err != nil {
						return err
					}
				}
				for _, id := range receipts.PublicationIDs {
					p := d.Publications[id]
					if err := tx.advancePublicationMark(d, p); err != nil {
						return err
					}
					read = append(read, fmt.Sprintf("read %s %s from %s", publicationKind(p), p.ID, p.Author))
				}
				if execution == "" && !tx.journalOnly {
					if err := workflows.recordRecords(tx, actor, appended); err != nil {
						return err
					}
				}
				return tx.Effects(effects)
			})
			if err != nil {
				return 0, err
			}
			// Only the known-success receipt is authoritative; never guess with
			// a postcommit read or publish tentative changed scopes.
			sequence = &commit.Sequence
			persisted = len(checkpoint.Generated)
			committed = commit
			return commit.Sequence, nil
		})
		if err == nil {
			if execution == "" {
				r.parentTurn.setRetryPause(parentPause)
			}
			// Omitted results retain their queued notices; they may be admitted
			// at the next boundary. Failed transactions keep the staged proof.
			workflows.clear()
			for _, text := range read {
				r.event("publication_read", actor, text)
			}
			if execution == "" {
				r.scheduleRelease()
			}
			// Never acquire the Runtime notification lock under the live-tail
			// display mutex held by transcript.checkpoint.
			r.committedRecords(committed)
		}
		return err
	}
	prior := cb.BeforeToolBatch
	cb.BeforeToolBatch = func(ctx context.Context, calls []messages.ChatMessageToolCall) error {
		if prior != nil {
			if err := prior(ctx, calls); err != nil {
				return err
			}
		}
		if execution == "" {
			return nil
		}
		// A batch may start only while this execution still owns its member.
		// JournalToolBatch records the intent itself once the calls are known.
		if openErr != nil {
			return openErr
		}
		return repo.Read(ctx, func(reader *recordReader) error {
			identity, err := reader.Identity()
			if err != nil {
				return err
			}
			e, err := reader.Execution(execution)
			if err != nil {
				return err
			}
			if e == nil || e.Member != identity.ActorID || e.Generation != generation || e.Status != "running" {
				return errors.New("execution intent was fenced")
			}
			return nil
		})
	}
	cb.JournalToolBatch = func(ctx context.Context, c llm.AgentCheckpoint) error {
		if openErr != nil {
			return openErr
		}
		if len(c.Generated) < persisted {
			return errors.New("checkpoint prefix regressed")
		}
		commit, err := repo.Update(ctx, func(tx *recordTx) error {
			identity, err := tx.Identity()
			if err != nil {
				return err
			}
			if execution == "" {
				return tx.PutParentTurn(identity.ActorID, &ParentTurn{Intent: c.Generated[persisted:]})
			}
			e, err := tx.Execution(execution)
			if err != nil {
				return err
			}
			if e == nil || e.Member != identity.ActorID || e.Generation != generation || e.Status != "running" {
				return errors.New("execution journal was fenced")
			}
			e.Intent = c.Generated[persisted:]
			e.Iterations = offset + c.Iterations
			return tx.PutExecution(e)
		})
		if err == nil {
			r.committedRecords(commit)
		}
		return err
	}
}

// bindParent adds mail admission, progressive persistence and settlement
// while preserving the UI's first-input gate. RunParent applies it to a copy
// of the host's callbacks; the host persists only response.AllMessages after
// response.PersistedMessages at turn end.
func (r *Runtime) bindParent(cb *llm.AgentCallbacks, allowed func() bool) {
	priorToolContext := cb.BeforeToolExecute
	cb.BeforeToolExecute = func(ctx context.Context, call messages.ChatMessageToolCall, args map[string]any) context.Context {
		if priorToolContext != nil {
			ctx = priorToolContext(ctx, call, args)
		}
		if call.Name == "workflow_run" {
			ctx = WithWorkflowCallID(ctx, call.ID)
		}
		return subagent.WithCallID(ctx, call.ID)
	}
	r.bindCheckpoint(r.parent, "", 0, 0, cb, nil, nil)
	checkpoint := cb.Checkpoint
	cb.Checkpoint = func(ctx context.Context, c llm.AgentCheckpoint) error {
		if allowed != nil && !allowed() {
			return errors.New("parent turn persistence was fenced")
		}
		return checkpoint(ctx, c)
	}
	var last [32]byte
	var prompted bool
	priorFinal := cb.ContinueAfterFinal
	cb.ContinueAfterFinal = func(ctx context.Context, response *messages.ChatMessage) ([]messages.ChatMessage, error) {
		if priorFinal != nil {
			input, err := priorFinal(ctx, response)
			if err != nil || len(input) != 0 {
				return input, err
			}
		}
		r.parentTurn.setSettling(true)
		settleErr := r.Settle(ctx)
		r.parentTurn.setSettling(false)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		if len(s.Runs) == 0 {
			return nil, nil
		}
		if settleErr == nil {
			err = r.update(ctx, func(s *State) error {
				for _, run := range s.Runs {
					if run.Status == "running" || run.Status == "paused" && runDeferred(s, run.ID) {
						run.Status = "completed"
					}
				}
				return nil
			})
			return nil, err
		}
		fingerprint := sha256.Sum256([]byte(coordinationFingerprint(s)))
		if prompted && last == fingerprint {
			return nil, &settlementBlockedError{settleErr}
		}
		prompted = true
		last = fingerprint
		return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: nudgeText(settleErr, Present(s, r.ID, r.ID)), Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}}}, nil
	}
}

// nudgeText leads with the settlement blocker, lists every decision with its
// action (bounded, the rest reachable through swarm_read), and asks for the
// restated answer.
func nudgeText(settleErr error, p Presentation) string {
	var b strings.Builder
	b.WriteString("Coordination is still outstanding: " + settleErr.Error() + ". Resolve it, then answer.")
	shown := 0
	for _, item := range p.Decisions {
		line := "\n- " + item.Label + ": " + item.Action
		if item.Why != "" {
			line = "\n- " + item.Label + ": " + item.Why + ". " + item.Action
		}
		if shown == 10 || b.Len()+len(line) > 4096 {
			break
		}
		b.WriteString(line)
		shown++
	}
	if rest := len(p.Decisions) - shown; rest > 0 {
		fmt.Fprintf(&b, "\n… and %d more; swarm_read lists them.", rest)
	}
	b.WriteString("\n\nYour previous answer was provisional. Once coordination settles, reply with the complete answer for the user, restated in full and reflecting what you accepted or acknowledged, not a description of the coordination steps.")
	return b.String()
}
