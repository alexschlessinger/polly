package sessions

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
)

// Scalar keys below are exact field values, including empty values. Tuple keys
// use CoordinationIndexKey, never string concatenation or SQL fragments.
const (
	CoordinationIndexRecords                 CoordinationIndex = CoordinationIndex(coordindex.Records)          // all records, empty key
	CoordinationIndexRecord                  CoordinationIndex = CoordinationIndex(coordindex.Record)           // tuple(kind, ID)
	CoordinationIndexKind                    CoordinationIndex = CoordinationIndex(coordindex.Kind)             // kind
	CoordinationIndexMailRecipient           CoordinationIndex = CoordinationIndex(coordindex.MailRecipient)    // mail.to
	CoordinationIndexMailPending             CoordinationIndex = CoordinationIndex(coordindex.MailPending)      // mail.to, delivered=false
	CoordinationIndexMemberNamePrefix        CoordinationIndex = CoordinationIndex(coordindex.MemberNamePrefix) // canonical name or ancestor, path boundary
	CoordinationIndexExecutionMember         CoordinationIndex = CoordinationIndex(coordindex.ExecutionMember)
	CoordinationIndexExecutionRun            CoordinationIndex = CoordinationIndex(coordindex.ExecutionRun)
	CoordinationIndexExecutionWorkflow       CoordinationIndex = CoordinationIndex(coordindex.ExecutionWorkflow)
	CoordinationIndexExecutionStatus         CoordinationIndex = CoordinationIndex(coordindex.ExecutionStatus)
	CoordinationIndexTaskOwner               CoordinationIndex = CoordinationIndex(coordindex.TaskOwner)
	CoordinationIndexTaskExecution           CoordinationIndex = CoordinationIndex(coordindex.TaskExecution)
	CoordinationIndexTaskDependency          CoordinationIndex = CoordinationIndex(coordindex.TaskDependency) // each dependencies entry
	CoordinationIndexTaskStatus              CoordinationIndex = CoordinationIndex(coordindex.TaskStatus)
	CoordinationIndexContextOwner            CoordinationIndex = CoordinationIndex(coordindex.ContextOwner)
	CoordinationIndexContextRelease          CoordinationIndex = CoordinationIndex(coordindex.ContextRelease)  // release="" includes unreleased contexts
	CoordinationIndexWorkflowSteps           CoordinationIndex = CoordinationIndex(coordindex.WorkflowSteps)   // report ID; numeric step ordering
	CoordinationIndexMemberLifecycle         CoordinationIndex = CoordinationIndex(coordindex.MemberLifecycle) // active, waiting, paused, idle; current execution only
	CoordinationIndexRunStatus               CoordinationIndex = CoordinationIndex(coordindex.RunStatus)
	CoordinationIndexFollowupMember          CoordinationIndex = CoordinationIndex(coordindex.FollowupMember)
	CoordinationIndexFollowupPreparingMember CoordinationIndex = CoordinationIndex(coordindex.FollowupPreparingMember) // member, refresh=true and phase=preparing

	// Active and Survivor use the same audience keys as PublicationAudience.
	// Active excludes only explicitly superseded records. Survivor chooses the
	// oldest active keyed exact-equivalent claim (retained snapshot commit included).
	CoordinationIndexPublicationActive   CoordinationIndex = CoordinationIndex(coordindex.PublicationActive)
	CoordinationIndexPublicationSurvivor CoordinationIndex = CoordinationIndex(coordindex.PublicationSurvivor)
	// Pending is keyed by an existing member ID or family root/parent ID. It uses
	// that reader's durable mark and current execution run, excludes its author,
	// and retains promoted old duplicates until an exact admittedTo receipt.
	// It is not an arbitrary historical mark/run query or an authorization grant.
	CoordinationIndexPublicationPending CoordinationIndex = CoordinationIndex(coordindex.PublicationPending)
	// AdmissionNews is a revision-only tuple(kind, ID) semantic dependency scope.
	// It excludes labels and queued->running transitions, unlike payload scopes.
	// It is conservative: callers still decide control/visibility and transitions.
	CoordinationIndexAdmissionNews CoordinationIndex = CoordinationIndex(coordindex.AdmissionNews)

	// SemanticNews is a revision-only, empty-key scope over durable semantic
	// heads. Presence, control and canonical value changes invalidate it; raw
	// payloads, labels and iterations do not. This is a dependency hint, not
	// readiness: SemanticChanges still suppresses deletion, control and ABA.
	CoordinationIndexSemanticNews CoordinationIndex = CoordinationIndex(coordindex.SemanticNews)
	// SemanticMail is a revision-only reader-ID scope over semantic pending-mail
	// membership, including followup refresh/phase suppression. Mail text and
	// order changes do not invalidate it; SemanticPendingMail decides readiness.
	CoordinationIndexSemanticMail CoordinationIndex = CoordinationIndex(coordindex.SemanticMail)
	// Display and DisplayFact are revision-only compact display scopes. Display
	// uses an empty key; DisplayFact uses tuple(kind, ID) of a DisplayRecord,
	// not its source record (e.g. status/parent or launch/executionID).
	// Only a changed compact fact, including a deletion, invalidates them.
	// Missing revision rows are valid initial zeroes, including existing v10
	// databases; the next fact/head mutation creates the revision atomically.
	CoordinationIndexDisplay     CoordinationIndex = CoordinationIndex(coordindex.Display)
	CoordinationIndexDisplayFact CoordinationIndex = CoordinationIndex(coordindex.DisplayFact)

	// Audience partitions are candidates, NOT authorization: tuple("machine"),
	// tuple("run", runID), tuple("parent"). Machine publications are global;
	// other publications belong to their run; non-run scopes also reach parent.
	CoordinationIndexPublicationAudience CoordinationIndex = CoordinationIndex(coordindex.PublicationAudience)
	// Claim candidates use tuple(normalizedScope, runID, factKey), with empty
	// runID for machine scope. Candidates include superseded and duplicate rows;
	// callers can use the shared equivalence codec or the derived Survivor index.
	CoordinationIndexPublicationClaim        CoordinationIndex = CoordinationIndex(coordindex.PublicationClaim)
	CoordinationIndexPublicationSupersedes   CoordinationIndex = CoordinationIndex(coordindex.PublicationSupersedes)   // predecessor publication ID
	CoordinationIndexPublicationReceipt      CoordinationIndex = CoordinationIndex(coordindex.PublicationReceipt)      // exact admittedTo reader ID, NOT high-water inference
	CoordinationIndexPublicationExactReceipt CoordinationIndex = CoordinationIndex(coordindex.PublicationExactReceipt) // tuple(readerID, publicationID)
	CoordinationIndexPublicationSnapshot     CoordinationIndex = CoordinationIndex(coordindex.PublicationSnapshot)     // snapshot ID, including historical duplicates
	// OverviewDependency returns records directly referencing tuple(kind, ID).
	// Relations are structural, not transitive or a precomputed presentation.
	// Sources include member context/task/execution/controller; execution member/
	// run/workflow/workspace/base; task owner/execution/run/dependencies/follows/
	// snapshots; mail endpoints/replies/task/execution/workflow; context owner;
	// workflow run, steps' report; publication author/run/snapshot/predecessors;
	// and integration inputs/repairs/pending task+snapshots/run/successor links.
	CoordinationIndexOverviewDependency CoordinationIndex = CoordinationIndex(coordindex.OverviewDependency)
)

// TaskOpen excludes done/canceled; ApplyUncertain selects applying/recovery_required.
// Both are existing semantic partitions with an empty key.
const (
	CoordinationIndexTaskOpen       CoordinationIndex = "semantic.open-task"
	CoordinationIndexApplyUncertain CoordinationIndex = "semantic.uncertain-apply"
)

const (
	MaxCoordinationPageLimit      = 1000
	MaxCoordinationRevisionScopes = 256
)

var (
	ErrCoordinationSelector = coordindex.ErrSelector
	ErrCoordinationFormat   = coordindex.ErrFormat
)

// CoordinationIndexKey encodes a compound selector key without ambiguity.
func CoordinationIndexKey(parts ...string) string { return coordindex.Key(parts...) }

func isRevisionOnlyCoordinationIndex(index CoordinationIndex) bool {
	switch index {
	case CoordinationIndexAdmissionNews, CoordinationIndexSemanticNews, CoordinationIndexSemanticMail, CoordinationIndexDisplay, CoordinationIndexDisplayFact:
		return true
	}
	return false
}

func validateCoordinationQuery(query RecordQuery) error {
	if query.IncludeLast && !query.CountOnly {
		return fmt.Errorf("%w: IncludeLast requires CountOnly", ErrCoordinationRequest)
	}
	if query.CountOnly {
		if !query.IncludeTotal || query.Limit != 0 || query.Offset != 0 || query.After != "" || query.MaxBytes != 0 {
			return fmt.Errorf("%w: count-only requires IncludeTotal and no paging options", ErrCoordinationRequest)
		}
		return nil
	}
	if query.Limit <= 0 || query.Limit > MaxCoordinationPageLimit || query.Offset < 0 || query.MaxBytes < 0 || query.After != "" && query.Offset != 0 {
		return fmt.Errorf("%w: page bounds", ErrCoordinationRequest)
	}
	return nil
}
