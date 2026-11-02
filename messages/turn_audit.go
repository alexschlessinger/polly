package messages

import "time"

const (
	// Input metadata is saved before the first provider request. TurnAudit is
	// recorded at run settlement on an internal message, never sent to a model.
	MetadataKeyTurnID        = "turn_id"
	MetadataKeyTurnStartedAt = "turn_started_at"
	MetadataKeyTurnAudit     = "turn_audit"
)

// TurnAudit version 1 records a CLI agent run through model/tool settlement.
// It excludes subsequent output flushing and storage latency. ID identifies
// this run; InputTurnID identifies the persisted user message, which may be
// reused by an explicit retry. Empty InputTurnID denotes older untagged input.
// A saved input without an audit can mean the process died, the session lease
// was lost, persistence failed, or a detached turn was forbidden to append.
type TurnAudit struct {
	Version            int                 `json:"version"`
	ID                 string              `json:"id"`
	InputTurnID        string              `json:"input_turn_id,omitempty"`
	StartedAt          time.Time           `json:"started_at"`
	RunFinishedAt      time.Time           `json:"run_finished_at"`
	ReusedInput        bool                `json:"reused_input,omitempty"`
	Status             string              `json:"status"`
	Error              string              `json:"error,omitempty"`
	InterruptionOrigin string              `json:"interruption_origin,omitempty"`
	Attempts           []ModelAttemptAudit `json:"attempts,omitempty"`
	Retries            []RetryAudit        `json:"retries,omitempty"`
}

type ModelAttemptAudit struct {
	ID                 string     `json:"id"`
	Model              string     `json:"model"`
	Purpose            string     `json:"purpose"`
	Iteration          int        `json:"iteration"`
	Attempt            int        `json:"attempt"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	StopReason         StopReason `json:"stop_reason,omitempty"`
	Error              string     `json:"error,omitempty"`
	InterruptionOrigin string     `json:"interruption_origin,omitempty"`
}

// RetryAudit records both edges of an accepted wait. Error is the failure
// that caused the wait; WaitError describes an interrupted or expired wait.
// A finished wait alone does not prove that another request was made: the
// attempt records establish that. HTTP and stream waits share a retry budget.
type RetryAudit struct {
	At                 time.Time `json:"at"`
	Waiting            bool      `json:"waiting"`
	Source             string    `json:"source,omitempty"`
	RetryCount         int       `json:"retry_count"`
	DelayMillis        int64     `json:"delay_ms"`
	NextRetryAt        time.Time `json:"next_retry_at"`
	Error              string    `json:"error,omitempty"`
	WaitError          string    `json:"wait_error,omitempty"`
	InterruptionOrigin string    `json:"interruption_origin,omitempty"`
}
