package swarm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

const recoveryPageLimit = 64
const recoveryPageBytes = 256 * 1024

type recoverySaved struct {
	prepare, worktrees, contexts bool
}

// validateRecovery deliberately reads ALL saved payloads, including completed
// history. Native indexes validate their own fields, not the domain payloads.
// This cold cost is linear in saved bytes, with at most a page and one decoded
// record live (an oversized record is returned alone). It is not a key scan or
// a whole-State cache. No recovery effect precedes successful validation.
func (r *Runtime) validateRecovery(ctx context.Context) (recoverySaved, error) {
	var saved recoverySaved
	access, err := sessions.OpenCoordinationAccess(r.parent)
	if err != nil {
		return saved, err
	}
	err = access.ReadRecords(ctx, func(raw sessions.CoordinationReader) error {
		identity, err := raw.Identity()
		if err != nil {
			return err
		}
		// Native payload queries reject known swarm kinds before returning them
		// when the format is absent or unsupported. Check the domain version
		// first so callers retain ErrUnsupportedFormat and its recovery guidance;
		// never translate a storage or malformed-payload error into that error.
		record, found, err := raw.Get(sessions.RecordKey{Kind: formatKind, ID: formatID})
		if err != nil {
			return err
		}
		if found {
			format, err := decodeRecord[FormatRecord](record, false)
			if err != nil {
				return err
			}
			runtimemetrics.Add(ctx, runtimemetrics.RecordsDecoded, 1)
			if format.Version != swarmFormatVersion {
				return unsupportedFormat(identity.ParentID, format)
			}
		} else {
			// Counts/last keys do not load known payloads on native storage.
			// Only an empty root or its own pre-swarm journal may lack format;
			// the journal is still typed-validated by the streaming pass below.
			page, err := raw.Query(sessions.RecordQuery{CountOnly: true, IncludeTotal: true, IncludeLast: true})
			if err != nil {
				return err
			}
			empty := page.Total == 0 && page.Last == nil
			ownJournal := identity.ActorID == identity.ParentID && page.Total == 1 && page.Last != nil &&
				*page.Last == (sessions.RecordKey{Kind: "parent_turn", ID: identity.ActorID})
			if !empty && !ownJournal {
				return unsupportedFormat(identity.ParentID, nil)
			}
		}
		var stepWorkflow string
		nextStep := 0
		query := sessions.RecordQuery{Limit: recoveryPageLimit, MaxBytes: recoveryPageBytes}
		for {
			page, err := raw.Query(query)
			if err != nil {
				return err
			}
			for _, record := range page.Records {
				if err := ctx.Err(); err != nil {
					return err
				}
				kind := record.Key.Kind
				// Retain only scalar bootstrap facts, never historical payloads.
				switch kind {
				case formatKind:
					continue // the exact format record was validated in this view above
				case "followup":
					_, err = decodeRecord[FollowupCall](record, false)
				case "integration":
					_, err = decodeRecord[IntegrationCandidate](record, false)
				case "apply":
					var value *ApplyRecord
					value, err = decodeRecord[ApplyRecord](record, false)
					saved.worktrees = true
					if value != nil && (value.Status == "applying" || value.Status == "recovery_required") {
						saved.prepare = true
					}
				case "parent_turn":
					_, err = decodeRecord[ParentTurn](record, false)
				case "parent":
					_, err = decodeRecord[ParentRecord](record, false)
				case "run":
					_, err = decodeRecord[Run](record, false)
				case "member":
					var value *Member
					value, err = decodeRecord[Member](record, false)
					if err == nil {
						err = validateSavedSandbox(kind, record.Key.ID, value.SandboxPresets)
					}
					saved.prepare = true
				case "task":
					_, err = decodeRecord[Task](record, false)
				case "mail":
					_, err = decodeRecord[Mail](record, false)
				case "publication":
					_, err = decodeRecord[Publication](record, false)
				case "execution":
					_, err = decodeRecord[Execution](record, false)
					saved.prepare = true
				case "context":
					saved.contexts = true
					var value *ExecutionContext
					value, err = decodeRecord[ExecutionContext](record, false)
					if err == nil {
						err = validateSavedSandbox(kind, record.Key.ID, value.SandboxPresets)
						saved.worktrees = saved.worktrees || value.Checkout != nil
					}
					saved.prepare = true
				case "snapshot":
					_, err = decodeRecord[worktree.Snapshot](record, false)
					saved.worktrees = true
				case "preview":
					_, err = decodeRecord[worktree.Preview](record, false)
				case "workflow":
					_, err = decodeRecord[workflow.Report](record, false)
					saved.prepare = true
				case "workflow_step":
					_, err = decodeRecord[workflow.Step](record, false)
					if err != nil {
						break
					}
					id, number, ok := strings.Cut(record.Key.ID, "/")
					index, parseErr := strconv.Atoi(number)
					if !ok || id == "" || parseErr != nil || index < 0 || strconv.Itoa(index) != number {
						return fmt.Errorf("read swarm workflow_step: unexpected record %q", record.Key.ID)
					}
					// Record ordering groups steps by workflow and numeric index.
					// The header was typed-validated earlier in this same view;
					// one exact existence check per group proves it is not orphaned.
					if id != stepWorkflow {
						_, found, err := raw.Get(sessions.RecordKey{Kind: "workflow", ID: id})
						if err != nil {
							return err
						}
						if !found {
							return fmt.Errorf("read swarm workflow_step: unexpected record %q", record.Key.ID)
						}
						stepWorkflow, nextStep = id, 0
					}
					if index != nextStep {
						return fmt.Errorf("read swarm workflow_step: %s is missing a step before %d", id, index)
					}
					nextStep++
				default:
					continue // unknown kinds remain untouched and round-trippable
				}
				if err != nil {
					return err
				}
				runtimemetrics.Add(ctx, runtimemetrics.RecordsDecoded, 1)
			}
			if page.Next == "" {
				break
			}
			if page.Next == query.After {
				return sessions.ErrCoordinationCursor
			}
			query.After = page.Next
		}
		return nil
	})
	return saved, err
}
