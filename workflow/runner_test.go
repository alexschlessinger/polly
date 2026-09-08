package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type hostFunc func(context.Context, Operation) (any, error)

func (f hostFunc) Call(ctx context.Context, op Operation) (any, error) { return f(ctx, op) }
func script(body string) string {
	return `polly.defineWorkflow({name:"test",inputSchema:polly.schema.object({}),async run(input){` + body + `}});`
}

func TestWorkflowParallelOrderAndTypedFailures(t *testing.T) {
	host := hostFunc(func(ctx context.Context, op Operation) (any, error) {
		n := int(op.Args["n"].(float64))
		if n == 2 {
			return nil, &Error{Code: "command_failed", Message: "exit 1", Result: map[string]any{"exitCode": 1}}
		}
		return map[string]any{"value": n}, nil
	})
	r := Runner{Host: host}
	for _, mode := range []string{"collect", "throw_after_all"} {
		t.Run(mode, func(t *testing.T) {
			report, err := r.Run(context.Background(), script(`return polly.parallel([3,2,1], n=>polly.agent({n}),{concurrency:2,errors:"`+mode+`"});`), map[string]any{})
			if mode == "collect" {
				if err != nil {
					t.Fatal(err)
				}
				values := report.Output.([]any)
				if len(values) != 3 || values[1].(map[string]any)["ok"] != false {
					t.Fatalf("result: %#v", values)
				}
			} else {
				var failure *Error
				if !errors.As(err, &failure) || failure.Result == nil {
					t.Fatalf("missing ordered failure result: %v", err)
				}
			}
			if len(report.Steps) != 3 {
				t.Fatalf("did not settle all branches: %+v", report)
			}
		})
	}
}

func TestWorkflowInputAndScopeContracts(t *testing.T) {
	var calls atomic.Int32
	r := Runner{Host: hostFunc(func(ctx context.Context, op Operation) (any, error) {
		calls.Add(1)
		if op.Args["context"] != "ctx" {
			return nil, errors.New("scope lost context")
		}
		return map[string]any{"text": "ok", "data": map[string]any{"answer": 42}}, nil
	})}
	report, err := r.Run(context.Background(), script(`return polly.scope({context:"ctx",label:"verify"},async work=> (await work.tool("check",{})).data);`), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Output.(map[string]any)["answer"] != float64(42) {
		t.Fatal(report.Output)
	}
	_, err = r.Run(context.Background(), script(`await polly.agent({});`), map[string]any{"foreign": true})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("input validation dispatched: %v %d", err, calls.Load())
	}
}

func TestWorkflowBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		config     Config
		want       string
	}{
		{"runaway", `while(true){}`, Config{JSBudget: 10 * time.Millisecond}, "budget"},
		{"unawaited", `polly.agent({}); return 1;`, Config{}, "unawaited"},
		{"hanging_promise", `return new Promise(()=>{});`, Config{}, "no host operation"},
		{"no_host_authority", `return typeof require+":"+typeof process+":"+typeof fetch;`, Config{}, ""},
		{"duplicate_keys", `return polly.schema.keyed(["a","a"],polly.schema.string());`, Config{}, "unique"},
		{"host_budget", `for(let i=0;i<5;i++){try{await polly.agent({});}catch(e){}} return "caught";`, Config{MaxHostCalls: 2}, "budget"},
		{"output_getter", `return {get value(){while(true){}}};`, Config{JSBudget: 10 * time.Millisecond}, "budget"},
		{"error_getter", `throw {get message(){while(true){}}};`, Config{JSBudget: 10 * time.Millisecond}, "budget"},
		{"intrinsic_replacement", `JSON.parse=()=>{throw Error("replaced")};JSON.stringify=()=>"broken";return await polly.agent({});`, Config{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			r := Runner{Config: tc.config, Host: hostFunc(func(ctx context.Context, op Operation) (any, error) {
				calls.Add(1)
				if tc.name == "unawaited" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, nil
			})}
			report, err := r.Run(context.Background(), script(tc.body), map[string]any{})
			if tc.want == "" {
				if err != nil || (tc.name == "no_host_authority" && report.Output != "undefined:undefined:undefined") {
					t.Fatalf("%v %+v", err, report)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			if tc.name == "host_budget" && calls.Load() > 2 {
				t.Fatal("dispatch after exhausted budget")
			}
		})
	}
}

func TestWorkflowCancellationCannotBeSwallowed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	r := Runner{Host: hostFunc(func(ctx context.Context, op Operation) (any, error) {
		calls.Add(1)
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})}
	_, err := r.Run(ctx, script(`try{await polly.agent({});}catch(e){};await polly.agent({});return "ok";`), map[string]any{})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("%v; calls=%d", err, calls.Load())
	}
}

func TestWorkflowHostWaitDoesNotSpendJSBudget(t *testing.T) {
	r := Runner{Config: Config{JSBudget: 20 * time.Millisecond}, Host: hostFunc(func(ctx context.Context, op Operation) (any, error) {
		select {
		case <-time.After(60 * time.Millisecond):
			return "ok", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})}
	report, err := r.Run(context.Background(), script(`return await polly.agent({});`), map[string]any{})
	if err != nil || fmt.Sprint(report.Output) != "ok" {
		t.Fatalf("%+v %v", report, err)
	}
}

type slowRecorder struct {
	hostFunc
	delay time.Duration
	saves atomic.Int32
}

func (r *slowRecorder) SaveWorkflow(context.Context, Report) error {
	r.saves.Add(1)
	time.Sleep(r.delay)
	return nil
}

func TestJSBudgetExcludesDurabilityWrites(t *testing.T) {
	host := &slowRecorder{hostFunc: func(context.Context, Operation) (any, error) { return map[string]any{"ok": true}, nil }, delay: 20 * time.Millisecond}
	// 32 workers each record their intent synchronously inside one JS slice:
	// 640ms of host writes against a 200ms budget for JavaScript itself.
	r := Runner{Host: host, Config: Config{JSBudget: 200 * time.Millisecond}}
	report, err := r.Run(context.Background(), script(`return polly.parallel(Array.from({length:32},(_,i)=>i), n=>polly.agent({n}), {concurrency:32});`), map[string]any{})
	if err != nil {
		t.Fatalf("host writes were charged to the JavaScript budget: %v", err)
	}
	if len(report.Steps) != 32 || host.saves.Load() < 32 {
		t.Fatalf("steps=%d saves=%d", len(report.Steps), host.saves.Load())
	}
	// A genuinely spinning script is still interrupted.
	_, err = r.Run(context.Background(), script(`for(;;){}`), map[string]any{})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "execution_budget" {
		t.Fatalf("spinning script not interrupted: %v", err)
	}
}

func TestScopeRethrowsPrimitiveAndFrozenErrorsIntact(t *testing.T) {
	r := Runner{Host: hostFunc(func(context.Context, Operation) (any, error) { return nil, nil })}
	for _, thrown := range []string{`"lint failed"`, `Object.freeze(new Error("frozen"))`, `42`} {
		_, err := r.Run(context.Background(), script(`return polly.scope({context:"ctx",label:"lint"}, async () => { throw `+thrown+`; });`), map[string]any{})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "workflow_failed" {
			t.Fatalf("throw %s: %v", thrown, err)
		}
		if strings.Contains(failure.Message, "TypeError") {
			t.Fatalf("throw %s became a TypeError: %s", thrown, failure.Message)
		}
	}
}
