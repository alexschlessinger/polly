package workflow

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/dop251/goja"
)

//go:embed bootstrap.js
var bootstrap string

type Runner struct {
	Host   Host
	Config Config
}

type completion struct {
	index int
	value any
	err   error
}
type pending struct{ resolve, reject func(any) error }

// Run creates a fresh VM. Only this goroutine touches it, including promise
// resolution; goroutines doing host work return JSON through a mailbox.
func (r *Runner) Run(ctx context.Context, source string, input any) (report *Report, runErr error) {
	if r.Host == nil {
		return nil, errors.New("workflow host is required")
	}
	caller := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	state := Report{ID: ids.New(), Source: source, Input: input, Status: "running", Steps: []Step{}, Started: time.Now().UTC()}
	if r.Config.RunID != "" {
		state.ID = r.Config.RunID
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(512)
	// Capture intrinsics before user code can replace them. Every call that
	// can invoke a getter or toJSON runs under the same interrupt budget.
	jsonObject := vm.Get("JSON").ToObject(vm)
	parseJSON, _ := goja.AssertFunction(jsonObject.Get("parse"))
	stringifyJSON, _ := goja.AssertFunction(jsonObject.Get("stringify"))
	budget := r.Config.JSBudget
	if budget <= 0 {
		budget = DefaultJSBudget
	}
	limit := r.Config.MaxHostCalls
	if limit <= 0 {
		limit = DefaultMaxHostCalls
	}
	mail := make(chan completion, limit)
	waiting := map[int]pending{}
	activated := false
	save := func(c context.Context) error {
		copy, err := cloneReport(state)
		if err != nil {
			return fmt.Errorf("workflow report must be JSON: %w", err)
		}
		if recorder, ok := r.Host.(Recorder); ok {
			return recorder.SaveWorkflow(c, copy)
		}
		return nil
	}
	defer func() {
		interrupted := caller.Err() != nil
		cancel(nil)
		state.Finished = time.Now().UTC()
		if runErr != nil {
			state.Status = "failed"
			state.Error = errorValue(runErr)
			if interrupted {
				state.Status = "interrupted"
			}
		} else {
			state.Status = "completed"
		}
		for i := range state.Steps {
			if state.Steps[i].Status == "running" {
				state.Steps[i].Status = "interrupted"
			}
		}
		c, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if err := save(c); err != nil {
			runErr = errors.Join(runErr, err)
			state.Status = "failed"
			state.Error = errorValue(runErr)
		}
		copy, copyErr := cloneReport(state)
		if copyErr != nil {
			runErr = errors.Join(runErr, copyErr)
			copy = Report{ID: state.ID, Source: state.Source, Name: state.Name, Status: "failed", Error: errorValue(runErr), Started: state.Started, Finished: state.Finished}
		}
		report = &copy
		if runErr != nil {
			e := errorValue(runErr)
			e.Report = report
			runErr = e
		}
	}()
	// Timers interrupt only an uninterrupted JS slice, never host waiting.
	// The durability write a host call makes before it starts is host work
	// too, so the budget clock stops around it (pauseBudget/resumeBudget run
	// on the VM goroutine, inside the slice that owns the timer).
	var (
		budgetTimer   *time.Timer
		budgetStarted time.Time
		budgetLeft    time.Duration
		budgetPaused  bool
	)
	pauseBudget := func() {
		if budgetTimer == nil || budgetPaused {
			return
		}
		if budgetTimer.Stop() {
			budgetLeft -= time.Since(budgetStarted)
			budgetPaused = true
		}
	}
	resumeBudget := func() {
		if !budgetPaused {
			return
		}
		budgetPaused = false
		budgetStarted = time.Now()
		budgetTimer.Reset(max(budgetLeft, 0))
	}
	runJS := func(fn func() error) (err error) {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		timedOut := make(chan struct{})
		timer := time.AfterFunc(budget, func() {
			defer close(timedOut)
			cancel(&Error{Code: "execution_budget", Message: "JavaScript execution budget exceeded"})
			vm.Interrupt(context.Cause(ctx))
		})
		budgetTimer, budgetStarted, budgetLeft, budgetPaused = timer, time.Now(), budget, false
		interrupted := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { defer close(interrupted); vm.Interrupt(context.Cause(ctx)) })
		defer func() {
			budgetTimer = nil
			if !timer.Stop() {
				<-timedOut
			}
			if !stop() {
				<-interrupted
			}
			if cause := context.Cause(ctx); cause != nil {
				err = cause
			}
		}()
		defer func() {
			if value := recover(); value != nil {
				switch e := value.(type) {
				case *goja.InterruptedError:
					err = e
				case *goja.StackOverflowError:
					err = e
				case *goja.Exception:
					err = e
				default:
					panic(value)
				}
			}
		}()
		if exception := vm.Try(func() { err = fn() }); exception != nil {
			return exception
		}
		return err
	}
	jsonValue := func(value any) (goja.Value, error) {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		// JSON.parse returns JS-owned data; Go maps and reflect wrappers never
		// enter the VM, so scripts cannot call host methods through results.
		return parseJSON(goja.Undefined(), vm.ToValue(string(data)))
	}
	jsonExport := func(v goja.Value) (any, error) {
		encoded, err := stringifyJSON(goja.Undefined(), v)
		if err != nil {
			return nil, err
		}
		if goja.IsUndefined(encoded) {
			return nil, errors.New("workflow value is not JSON")
		}
		return schema.DecodeJSON(encoded.String())
	}
	_ = vm.Set("__host", func(call goja.FunctionCall) goja.Value {
		p, resolve, reject := vm.NewPromise()
		rejectError := func(err error) goja.Value {
			value, _ := jsonValue(errorValue(err))
			_ = reject(value)
			return vm.ToValue(p)
		}
		if ctx.Err() != nil {
			return rejectError(context.Cause(ctx))
		}
		if !activated {
			return rejectError(errors.New("host calls are allowed only inside workflow.run"))
		}
		if len(state.Steps) >= limit {
			err := &Error{Code: "execution_budget", Message: "workflow host-call budget exhausted"}
			cancel(err)
			return rejectError(err)
		}
		var args map[string]any
		decoded, err := schema.DecodeJSON(call.Argument(1).String())
		if err != nil {
			return rejectError(err)
		}
		args, ok := decoded.(map[string]any)
		if !ok {
			return rejectError(errors.New("host arguments must be an object"))
		}
		index := len(state.Steps)
		op := Operation{ID: fmt.Sprintf("%s/%d", state.ID, index+1), Kind: call.Argument(0).String(), Args: args}
		state.Steps = append(state.Steps, Step{Operation: op, Status: "running", Started: time.Now().UTC()})
		// Intent must be durable before a command or agent can start.
		pauseBudget()
		err = save(ctx)
		resumeBudget()
		if err != nil {
			cancel(err)
			return rejectError(err)
		}
		waiting[index] = pending{resolve, reject}
		go func() {
			value, err := r.Host.Call(ctx, op)
			select {
			case mail <- completion{index, value, err}:
			case <-ctx.Done():
			}
		}()
		return vm.ToValue(p)
	})
	var controller goja.Value
	if err := runJS(func() error { var e error; controller, e = vm.RunString(bootstrap); return e }); err != nil {
		return nil, err
	}
	if err := runJS(func() error { _, e := vm.RunScript("workflow.js", source); return e }); err != nil {
		return nil, err
	}
	getDef, _ := goja.AssertFunction(controller.ToObject(vm).Get("definition"))
	var def goja.Value
	if err := runJS(func() error { var e error; def, e = getDef(goja.Undefined()); return e }); err != nil {
		return nil, err
	}
	if goja.IsUndefined(def) {
		return nil, errors.New("script did not define a workflow")
	}
	var schemaValue any
	err := runJS(func() error {
		d := def.ToObject(vm)
		state.Name = d.Get("name").String()
		var e error
		schemaValue, e = jsonExport(d.Get("inputSchema"))
		return e
	})
	if err != nil {
		return nil, err
	}
	schemaData, err := json.Marshal(schemaValue)
	if err != nil {
		return nil, err
	}
	s := schema.SchemaFromBytes(schemaData)
	if s == nil {
		return nil, errors.New("invalid inputSchema")
	}
	inputData, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if err := s.Validate(string(inputData)); err != nil {
		return nil, fmt.Errorf("workflow input: %w", err)
	}
	if err := save(ctx); err != nil {
		return nil, err
	}
	activated = true
	var result goja.Value
	if err := runJS(func() error {
		value, e := jsonValue(input)
		if e != nil {
			return e
		}
		run, _ := goja.AssertFunction(controller.ToObject(vm).Get("run"))
		result, e = run(goja.Undefined(), value)
		return e
	}); err != nil {
		return nil, err
	}
	promise, ok := result.Export().(*goja.Promise)
	if !ok {
		return nil, errors.New("workflow run did not return a promise")
	}
	for promise.State() == goja.PromiseStatePending {
		if len(waiting) == 0 {
			return nil, errors.New("workflow is waiting on a promise with no host operation")
		}
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case completed := <-mail:
			entry := waiting[completed.index]
			delete(waiting, completed.index)
			step := &state.Steps[completed.index]
			step.Finished = time.Now().UTC()
			if completed.err != nil {
				step.Status = "failed"
				step.Error = errorValue(completed.err)
			} else {
				step.Status = "completed"
				step.Value = completed.value
			}
			if err := save(ctx); err != nil {
				return nil, err
			}
			if err := runJS(func() error {
				if completed.err != nil {
					v, e := jsonValue(errorValue(completed.err))
					if e != nil {
						return e
					}
					return entry.reject(v)
				}
				v, e := jsonValue(completed.value)
				if e != nil {
					return e
				}
				return entry.resolve(v)
			}); err != nil {
				return nil, err
			}
		}
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if len(waiting) > 0 {
		return nil, errors.New("workflow returned with unawaited host operations")
	}
	if promise.State() == goja.PromiseStateRejected {
		v := promise.Result()
		e := &Error{Code: "workflow_failed"}
		if err := runJS(func() error {
			e.Message = v.String()
			if obj, ok := v.(*goja.Object); ok {
				if m := obj.Get("message"); m != nil && !goja.IsUndefined(m) {
					e.Message = m.String()
				}
				if code := obj.Get("code"); code != nil && !goja.IsUndefined(code) {
					e.Code = code.String()
				}
				if value := obj.Get("result"); value != nil && !goja.IsUndefined(value) {
					var err error
					e.Result, err = jsonExport(value)
					if err != nil {
						return err
					}
				}
				if session := obj.Get("session"); session != nil && !goja.IsUndefined(session) {
					e.Session = session.String()
				}
				if usage := obj.Get("usage"); usage != nil && !goja.IsUndefined(usage) {
					var err error
					e.Usage, err = jsonExport(usage)
					if err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		return nil, e
	}
	if err := runJS(func() error { var e error; state.Output, e = jsonExport(promise.Result()); return e }); err != nil {
		return nil, fmt.Errorf("workflow output must be JSON: %w", err)
	}
	return &state, nil
}
