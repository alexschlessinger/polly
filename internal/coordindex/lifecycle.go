package coordindex

import "encoding/json"

const (
	ExecutionRunCall          Index = "execution.run-call"
	ExecutionCallID           Index = "execution.call-id"
	WorkflowStepID            Index = "workflow.step-id"
	MailFromCall              Index = "mail.from-call"
	MailPendingStartRecipient Index = "mail.pending-start-recipient"
	MemberName                Index = "member.name"
	TaskFollowupCall          Index = "task.followup-call"
	SnapshotSource            Index = "snapshot.source"
)

// requestCallID reads the exact format-2 field name, just like Extract's
// top-level field map. Missing or null request/callID values mean the zero value;
// malformed request objects or call IDs are format errors, not empty matches.
func requestCallID(payload json.RawMessage) (string, error) {
	if payload == nil {
		return "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return "", err
	}
	var callID string
	if field := fields["callID"]; field != nil {
		if err := json.Unmarshal(field, &callID); err != nil {
			return "", err
		}
	}
	return callID, nil
}
