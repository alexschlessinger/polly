package workflow

import (
	"context"
	"encoding/json"
	"time"
)

// DeltaRecorder is an optional alternative to Recorder. When a Host implements
// both, Runner uses only SaveWorkflowDelta. Each call is synchronous: a running
// step's intent must be committed before its host effect starts. Errors abort
// execution without retrying the checkpoint or replaying the effect, including
// errors for which commit success is uncertain. A distinct terminal checkpoint
// is still attempted after draining in-flight host receipts.
//
// Implementations must atomically save the header, changed steps and any related
// delivery receipts or terminal notifications. Merely adapting this method to a
// full-report write does not establish bounded native persistence.
type DeltaRecorder interface {
	SaveWorkflowDelta(context.Context, ReportDelta) error
}

// ReportHeader is a workflow report without its step history. Field meanings
// match Report, including the recorder-owned Run and Acknowledged bookkeeping.
type ReportHeader struct {
	CallID       string    `json:"callID,omitempty"`
	Run          string    `json:"run,omitempty"`
	Acknowledged bool      `json:"acknowledged,omitempty"`
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Source       string    `json:"source"`
	Input        any       `json:"input"`
	Output       any       `json:"output,omitempty"`
	Status       string    `json:"status"`
	Error        *Error    `json:"error,omitempty"`
	Started      time.Time `json:"started"`
	Finished     time.Time `json:"finished,omitempty"`
}

// ReportDelta replaces the header and the specified steps of one report. Steps
// uses zero-based numeric positions in Report.Steps, not operation IDs or lexical
// key order. An omitted step is unchanged, never deleted. TotalSteps is the full
// resulting count, not the number of changed steps; it cannot decrease. Receivers
// must reject negative/out-of-range indexes and gaps in the resulting history.
// The initial checkpoint has zero steps. Header-only terminal updates are valid.
//
// Values are JSON-validated, owned copies: a recorder may retain or mutate them
// without changing the runner's report. Successful writes preserve coordinator
// bookkeeping in Header.Run and Header.Acknowledged as with Recorder. A nil error
// alone establishes commit success; recovery never replays JavaScript or effects.
type ReportDelta struct {
	Header     ReportHeader `json:"header"`
	Steps      map[int]Step `json:"steps"`
	TotalSteps int          `json:"totalSteps"`
}

func cloneReportDelta(r Report, changed map[int]struct{}) (ReportDelta, error) {
	delta := ReportDelta{
		Header: ReportHeader{
			CallID: r.CallID, Run: r.Run, Acknowledged: r.Acknowledged,
			ID: r.ID, Name: r.Name, Source: r.Source, Input: r.Input,
			Output: r.Output, Status: r.Status, Error: r.Error,
			Started: r.Started, Finished: r.Finished,
		},
		Steps: make(map[int]Step, len(changed)), TotalSteps: len(r.Steps),
	}
	for index := range changed {
		delta.Steps[index] = r.Steps[index]
	}
	data, err := json.Marshal(delta)
	if err != nil {
		return ReportDelta{}, err
	}
	var out ReportDelta
	err = json.Unmarshal(data, &out)
	return out, err
}
