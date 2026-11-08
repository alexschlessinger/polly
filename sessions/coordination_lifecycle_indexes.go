package sessions

import "github.com/alexschlessinger/pollytool/internal/coordindex"

// Lifecycle selectors retain exact field values, including empty values. They
// identify candidate records, not replay compatibility or start authorization.
const (
	// CoordinationIndexExecutionRunCall uses tuple(execution.run, request.callID),
	// ordered by bytewise record ID.
	CoordinationIndexExecutionRunCall CoordinationIndex = CoordinationIndex(coordindex.ExecutionRunCall)
	// CoordinationIndexExecutionCallID uses exact request.callID, ordered by
	// bytewise record ID. Call IDs do not establish workflow binding authority.
	CoordinationIndexExecutionCallID CoordinationIndex = CoordinationIndex(coordindex.ExecutionCallID)
	// CoordinationIndexWorkflowStepID uses tuple(reportID, step.id), ordered by
	// numeric step position within that report, not by the stable display ID.
	CoordinationIndexWorkflowStepID CoordinationIndex = CoordinationIndex(coordindex.WorkflowStepID)
	// CoordinationIndexMailFromCall uses tuple(mail.from, mail.callID), ordered
	// by posting instant then bytewise record ID.
	CoordinationIndexMailFromCall CoordinationIndex = CoordinationIndex(coordindex.MailFromCall)
	// CoordinationIndexMailPendingStartRecipient uses exact mail.to where
	// start=true and delivered=false, ordered by posting instant then record ID.
	CoordinationIndexMailPendingStartRecipient CoordinationIndex = CoordinationIndex(coordindex.MailPendingStartRecipient)
	// CoordinationIndexMemberName uses the exact agentName, or /root/agent_ + ID
	// when absent, like MemberNamePrefix; it does not trim or match descendants.
	// Matches are ordered by bytewise record ID.
	CoordinationIndexMemberName CoordinationIndex = CoordinationIndex(coordindex.MemberName)
	// CoordinationIndexTaskFollowupCall uses tuple(task.follows, followupCallID),
	// ordered by bytewise record ID.
	CoordinationIndexTaskFollowupCall CoordinationIndex = CoordinationIndex(coordindex.TaskFollowupCall)
	// CoordinationIndexSnapshotSource uses exact snapshot.source, without path
	// cleaning or resolution, ordered by bytewise record ID.
	CoordinationIndexSnapshotSource CoordinationIndex = CoordinationIndex(coordindex.SnapshotSource)
)
