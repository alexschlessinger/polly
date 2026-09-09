// Package workflow executes bounded JavaScript orchestration over a host's
// existing agent runtime. It owns no agents, concurrency pool or session store.
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const DefaultMaxHostCalls = 4096
const DefaultJSBudget = 5 * time.Second

type Operation struct {
	ID   string         `json:"id"`
	Kind string         `json:"kind"`
	Args map[string]any `json:"args"`
}

// Host enforces identity, tool authority, shared execution budgets, member
// reservation and persistence. Results must be JSON values, never Go objects.
type Host interface {
	Call(context.Context, Operation) (any, error)
}

// Recorder checkpoints the source and each completed operation. A runtime
// recovering a nonterminal report must mark it interrupted, never replay JS.
type Recorder interface {
	SaveWorkflow(context.Context, Report) error
}

type Config struct {
	RunID        string
	JSBudget     time.Duration
	MaxHostCalls int
}

type Step struct {
	Operation
	Status   string    `json:"status"`
	Value    any       `json:"value,omitempty"`
	Error    *Error    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

type Report struct {
	// Run and Acknowledged are optional coordinator bookkeeping. A recorder
	// preserves them across the runner's successive checkpoint writes.
	Run          string    `json:"run,omitempty"`
	Acknowledged bool      `json:"acknowledged,omitempty"`
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Source       string    `json:"source"`
	Input        any       `json:"input"`
	Output       any       `json:"output,omitempty"`
	Status       string    `json:"status"`
	Steps        []Step    `json:"steps"`
	Error        *Error    `json:"error,omitempty"`
	Started      time.Time `json:"started"`
	Finished     time.Time `json:"finished,omitempty"`
}

type Error struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Result  any     `json:"result,omitempty"`
	Session string  `json:"session,omitempty"`
	Usage   any     `json:"usage,omitempty"`
	Report  *Report `json:"-"`
	Cause   error   `json:"-"`
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func errorValue(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		copy := *e
		copy.Report = nil
		return &copy
	}
	code := "host_failed"
	if errors.Is(err, context.Canceled) {
		code = "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = "timeout"
	}
	return &Error{Code: code, Message: err.Error(), Cause: err}
}

func cloneReport(r Report) (Report, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return Report{}, err
	}
	var out Report
	err = json.Unmarshal(data, &out)
	return out, err
}
