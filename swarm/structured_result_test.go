package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

func completion(value string) messages.ChatMessage {
	return iterationTool("complete", completionToolName, `{"value":`+value+`}`)
}

var boolResultSchema = map[string]any{"type": "boolean"}

func TestStructuredCompletionPreservesSchemaReferences(t *testing.T) {
	shape := map[string]any{
		"type": "object", "properties": map[string]any{"name": map[string]any{"$ref": "#/$defs/name"}},
		"required": []string{"name"}, "$defs": map[string]any{"name": map[string]any{"type": "string", "minLength": 1}},
		"default": map[string]any{"$ref": "#/literal-data"},
	}
	state, err := newStructuredResult(&Execution{Request: AgentRequest{Schema: shape}}, "task", true)
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	state.register(registry)
	tool, _ := registry.Get(completionToolName)
	wrapped := &schema.Schema{Raw: tool.GetSchema().Raw}
	if err = wrapped.Validate(`{"value":{"name":"ok"}}`); err != nil {
		t.Fatal(err)
	}
	if err = wrapped.Validate(`{"value":{"name":""}}`); err == nil {
		t.Fatal("nested schema lost constraint")
	}
	if shape["properties"].(map[string]any)["name"].(map[string]any)["$ref"] != "#/$defs/name" {
		t.Fatal("mutated caller's schema")
	}
	if state.toolValueSchema["default"].(map[string]any)["$ref"] != "#/literal-data" {
		t.Fatal("rewrote literal JSON")
	}
}

func TestStructuredWorkflowReadsThenCompletes(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if req.ResponseSchema != nil {
			t.Error("investigation was constrained by response_format")
		}
		found := false
		for _, tool := range req.Tools {
			if tool.GetName() == "swarm_submit" {
				t.Error("ambiguous final submission tool")
			}
			if tool.GetName() == completionToolName {
				found = true
			}
		}
		if !found {
			t.Error("missing completion tool")
		}
		if calls.Add(1) == 1 {
			return iterationTool("read", "read_file", `{"path":"evidence.txt"}`)
		}
		if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "verified fixture") {
			t.Error("read result missing")
		}
		return completion(`{"answer":"verified fixture"}`)
	}), 1, 1)
	if err := os.WriteFile(filepath.Join(r.config.Root, "evidence.txt"), []byte("verified fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.config.Registry.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	source := `polly.defineWorkflow({name:"typed",inputSchema:polly.schema.object({}),async run(){return await polly.agent({task:"Read evidence.txt and report",readOnly:true,schema:polly.schema.object({answer:polly.schema.string()})});}});`
	report, err := r.RunWorkflow(context.Background(), source, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	value := report.Output.(map[string]any)["value"]
	s, _ := r.State(context.Background())
	for _, e := range s.Executions {
		if e.Status != "completed" || e.Completion == nil || e.Iterations != 2 || e.ResultCorrections != 0 {
			t.Fatalf("execution=%+v", e)
		}
		if !reflect.DeepEqual(e.Result.Value, value) || !reflect.DeepEqual(s.Tasks[e.Result.Task].Result, value) || s.Tasks[e.Result.Task].Status != "awaiting_review" {
			t.Fatal("workflow and task results differ")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("extra call: %d", calls.Load())
	}
}

func TestStructuredResultCorrections(t *testing.T) {
	for _, tc := range []struct {
		name        string
		responses   []messages.ChatMessage
		wantErr     bool
		corrections int
	}{
		{"prose", []messages.ChatMessage{answer("done"), completion("true")}, false, 1},
		{"published prose", []messages.ChatMessage{iterationTool("publish", "swarm_publish", `{"text":"finding"}`), answer("see publication"), completion("true")}, false, 1},
		{"invalid then valid", []messages.ChatMessage{completion(`"wrong"`), completion("true")}, false, 1},
		{"two corrections", []messages.ChatMessage{answer("done"), completion(`"wrong"`), completion("true")}, false, 2},
		{"third failure", []messages.ChatMessage{answer("done"), completion(`"wrong"`), answer("still done")}, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if calls >= len(tc.responses) {
					t.Error("unexpected model call")
					return answer("")
				}
				msg := tc.responses[calls]
				calls++
				msg.SetTokenUsage(10, 2)
				return msg
			}), 1, 1)
			result, err := r.Agent(context.Background(), "", AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema})
			if (err != nil) != tc.wantErr {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "after two corrections") {
				t.Fatal(err)
			}
			if !tc.wantErr && result.Value != true {
				t.Fatalf("value=%#v", result.Value)
			}
			s, _ := r.State(context.Background())
			for _, e := range s.Executions {
				if e.ResultCorrections != tc.corrections || e.Iterations != calls || e.Usage.Samples != calls || *e.Usage.InputTokens != calls*10 {
					t.Fatalf("accounting=%+v", e)
				}
				if tc.wantErr && (e.Completion != nil || s.Tasks[result.Task].Status != "blocked") {
					t.Fatal("invalid result was submitted")
				}
			}
			if calls != len(tc.responses) {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestStructuredValuesAndStrictArguments(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      any
		invalid   bool
	}{
		{"object", `{"value":{"a":1}}`, map[string]any{"a": float64(1)}, false},
		{"array", `{"value":[1,"two"]}`, []any{float64(1), "two"}, false},
		{"string", `{"value":"text"}`, "text", false},
		{"number", `{"value":42}`, float64(42), false},
		{"null", `{"value":null}`, nil, false},
		{"duplicate", `{"value":1,"value":2}`, nil, true},
		{"nested duplicate", `{"value":{"a":1,"a":2}}`, nil, true},
		{"trailing", `{"value":1} {}`, nil, true},
		{"missing", `{}`, nil, true},
		{"extra", `{"value":1,"extra":2}`, nil, true},
		{"malformed", `{`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				calls++
				if calls == 1 {
					return iterationTool("finish", completionToolName, tc.raw)
				}
				return completion(`null`)
			}), 1, 1)
			result, err := r.Agent(context.Background(), "", AgentRequest{Task: "transform", ReadOnly: true, Schema: map[string]any{}})
			if err != nil || !reflect.DeepEqual(result.Value, tc.want) {
				t.Fatalf("value=%#v err=%v", result.Value, err)
			}
			wantCalls := 1
			if tc.invalid {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
			s, _ := r.State(context.Background())
			for _, e := range s.Executions {
				if e.Completion == nil {
					t.Fatal("null lost acceptance marker")
				}
			}
		})
	}
}

func TestStructuredToolsDisabled(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "inherited"}[inherited], func(t *testing.T) {
			calls := 0
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if len(req.Tools) != 0 || req.ResponseSchema == nil {
					t.Error("changed disabled-tool authority")
				}
				calls++
				if calls == 1 {
					return answer("not json")
				}
				return answer("true")
			}), 1, 1)
			req := AgentRequest{Task: "transform", ReadOnly: true, Schema: boolResultSchema}
			if inherited {
				r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 5, DisableTools: true}, nil)
			} else {
				req.Tools = []string{}
			}
			result, err := r.Agent(context.Background(), "", req)
			if err != nil || result.Value != true || calls != 2 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestStructuredBudgetDenialAndExclusiveBatch(t *testing.T) {
	for _, kind := range []string{"budget", "denied", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				if kind == "budget" {
					return answer("premature")
				}
				msg := completion("true")
				if kind == "mixed" {
					msg.ToolCalls = append(msg.ToolCalls, messages.ChatMessageToolCall{ID: "effect", Name: "effect", Arguments: `{}`})
				}
				return msg
			}), 1, 1)
			r.config.Registry.Register(&tools.Func{Name: "effect", Run: func(context.Context, tools.Args) (string, error) { effects.Add(1); return "ok", nil }})
			if kind == "denied" {
				r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
					return &llm.AgentCallbacks{ApproveToolCalls: func(c []messages.ChatMessageToolCall) []bool { return make([]bool, len(c)) }}
				}
			}
			result, err := r.Agent(context.Background(), "", AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema, MaxIterations: 1})
			if err == nil || effects.Load() != 0 {
				t.Fatalf("result=%+v err=%v effects=%d", result, err, effects.Load())
			}
			if kind == "budget" && !llm.IsIterationLimit(err) {
				t.Fatal(err)
			}
			s, _ := r.State(context.Background())
			for _, e := range s.Executions {
				wantCorrections := 0
				if kind == "budget" {
					wantCorrections = 1
					if e.PendingResultCorrection == "" {
						t.Fatal("lost correction at budget boundary")
					}
				}
				if e.Completion != nil || e.ResultCorrections != wantCorrections {
					t.Fatalf("unearned completion/correction: %+v", e)
				}
			}
		})
	}
}

func waitStructuredExecution(t *testing.T, r *Runtime, member string) {
	t.Helper()
	r.mu.Lock()
	i := r.active[member]
	r.mu.Unlock()
	if i != nil {
		select {
		case <-i.done:
		case <-time.After(10 * time.Second):
			t.Fatal("resume stalled")
		}
	}
}

func TestStructuredAcceptedRecoveryWithoutModelCall(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return completion("true")
	}), 1, 1)
	result, err := r.Agent(context.Background(), "", AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema, MaxIterations: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the final receipt checkpoint but before finish.
	if err = r.update(context.Background(), func(s *State) error {
		m := s.Members[result.Session]
		e := s.Executions[m.Execution]
		e.Status = "paused"
		e.Result = nil
		task := s.Tasks[result.Task]
		task.Status = "running"
		task.Result = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = r.Resume(context.Background(), result.Session, 0); err != nil {
		t.Fatal(err)
	}
	waitStructuredExecution(t, r, result.Session)
	s, _ := r.State(context.Background())
	e := s.Executions[s.Members[result.Session].Execution]
	if calls.Load() != 1 || e.Status != "completed" || e.Result.Value != true || e.Iterations != 1 {
		t.Fatalf("calls=%d execution=%+v", calls.Load(), e)
	}
}

func TestStructuredCorrectionsPersistAcrossResume(t *testing.T) {
	calls := 0
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		calls++
		if calls > 1 && !strings.Contains(req.Messages[len(req.Messages)-1].Content, "Invalid final result:") {
			t.Error("model retry did not receive corrective feedback")
		}
		return answer("premature")
	}), 1, 1)
	result, err := r.Agent(context.Background(), "", AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema, MaxIterations: 2})
	if !llm.IsIterationLimit(err) {
		t.Fatal(err)
	}
	if err = r.ResumeWithIterations(context.Background(), result.Session, 3); err != nil {
		t.Fatal(err)
	}
	waitStructuredExecution(t, r, result.Session)
	s, _ := r.State(context.Background())
	e := s.Executions[s.Members[result.Session].Execution]
	if e.Status != "failed" || e.ResultCorrections != 2 || calls != 3 || e.PendingResultCorrection != "" || !strings.Contains(e.Error, "after two corrections") {
		t.Fatalf("calls=%d execution=%+v", calls, e)
	}
}

func TestStructuredCorrectionsSurviveSingleCallGrants(t *testing.T) {
	for _, toolFree := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion tool", true: "direct JSON"}[toolFree], func(t *testing.T) {
			var calls atomic.Int32
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if calls.Add(1) > 1 && !strings.Contains(req.Messages[len(req.Messages)-1].Content, "Invalid final result:") {
					t.Error("lost pending corrective input")
				}
				return answer("premature")
			}), 1, 1)
			req := AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema, MaxIterations: 1}
			if toolFree {
				req.Tools = []string{}
			}
			result, err := r.Agent(context.Background(), "", req)
			if !llm.IsIterationLimit(err) {
				t.Fatal(err)
			}
			for attempt := 2; attempt <= 3; attempt++ {
				if err = r.ResumeWithIterations(context.Background(), result.Session, 1); err != nil {
					t.Fatal(err)
				}
				waitStructuredExecution(t, r, result.Session)
				s, _ := r.State(context.Background())
				e := s.Executions[s.Members[result.Session].Execution]
				if int(calls.Load()) != attempt || e.ResultCorrections != 2 {
					t.Fatalf("calls=%d execution=%+v", calls.Load(), e)
				}
				if attempt == 2 && (e.Status != "paused" || e.PendingResultCorrection == "") {
					t.Fatalf("lost pending correction: %+v", e)
				}
				if attempt == 3 && (e.Status != "failed" || !strings.Contains(e.Error, "after two corrections")) {
					t.Fatalf("third invalid final did not fail: %+v", e)
				}
			}
		})
	}
}

func TestStructuredContinuationDoesNotReuseCompletion(t *testing.T) {
	calls := 0
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls++
		if calls == 1 {
			return completion("true")
		}
		return completion(`"new"`)
	}), 1, 2)
	first, err := r.Agent(context.Background(), "", AgentRequest{Task: "first", ReadOnly: true, Schema: boolResultSchema})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Agent(context.Background(), "", AgentRequest{Task: "second", Session: first.Session, Schema: map[string]any{"type": "string"}})
	if err != nil || second.Value != "new" || calls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", second, err, calls)
	}
}

func TestStructuredCancellationNeverCorrects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		cancel()
		return answer("premature")
	}), 1, 1)
	_, err := r.Agent(ctx, "", AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStructuredParallelLargeResults(t *testing.T) {
	large := strings.Repeat("evidence ", 65536)
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		value := "second"
		if strings.Contains(req.Messages[len(req.Messages)-1].Content, "first") {
			value = large
		}
		return completion(tools.Result(value))
	}), 2, 2)
	report, err := r.RunWorkflow(context.Background(), `polly.defineWorkflow({name:"parallel",inputSchema:polly.schema.object({}),run(){return polly.parallel(["first","second"],task=>polly.agent({task,readOnly:true,schema:polly.schema.string()}),{errors:"throw_after_all"})}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows := report.Output.([]any)
	if rows[0].(map[string]any)["value"].(map[string]any)["value"] != large || rows[1].(map[string]any)["value"].(map[string]any)["value"] != "second" {
		t.Fatal("parallel values truncated or crossed")
	}
	s, _ := r.State(context.Background())
	if len(s.Executions) != 2 {
		t.Fatal("wrong executions")
	}
	for _, e := range s.Executions {
		if e.Completion == nil || !reflect.DeepEqual(e.Completion.Value, e.Result.Value) {
			t.Fatal("durable value changed")
		}
	}
}

func TestStructuredCheckpointFencesAcceptance(t *testing.T) {
	for _, kind := range []string{"generation", "owner", "missing receipt"} {
		t.Run(kind, func(t *testing.T) {
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return completion("true") }), 1, 1)
			if kind == "missing receipt" {
				r.config.DurableMessages = func(input []messages.ChatMessage) []messages.ChatMessage {
					var output []messages.ChatMessage
					for _, message := range input {
						if message.Role != messages.MessageRoleTool || message.ToolName != completionToolName {
							output = append(output, message)
						}
					}
					return output
				}
			}
			r.config.Callbacks = func(_ context.Context, m Member) *llm.AgentCallbacks {
				return &llm.AgentCallbacks{OnToolResult: func(call messages.ChatMessageToolCall, receipt messages.ChatMessage) {
					if call.Name != completionToolName || kind == "missing receipt" {
						return
					}
					if err := r.update(context.Background(), func(s *State) error {
						e := s.Executions[s.Members[m.ID].Execution]
						if kind == "generation" {
							e.Generation++
						} else {
							s.Tasks[s.Members[m.ID].Task].Owner = "new-owner"
						}
						return nil
					}); err != nil {
						t.Error(err)
					}
				}}
			}
			_, err := r.Agent(context.Background(), "", AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema})
			if err == nil {
				t.Fatal("stale completion succeeded")
			}
			s, _ := r.State(context.Background())
			for _, e := range s.Executions {
				if e.Completion != nil || e.Status == "completed" {
					t.Fatalf("stale completion persisted: %+v", e)
				}
			}
		})
	}
}

func TestStructuredUncertainCompletionIsNotAccepted(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return completion("true")
	}), 1, 1)
	result, err := r.Agent(context.Background(), "", AgentRequest{Task: "check", ReadOnly: true, Schema: boolResultSchema, MaxIterations: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.update(context.Background(), func(s *State) error {
		m := s.Members[result.Session]
		e := s.Executions[m.Execution]
		e.Completion = nil
		e.Result = nil
		e.Status = "paused"
		s.Tasks[m.Task].Status = "running"
		s.Tasks[m.Task].Result = nil
		e.Intent = []messages.ChatMessage{iterationTool("uncertain", completionToolName, `{"value":false}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = r.Resume(context.Background(), result.Session, 0); err != nil {
		t.Fatal(err)
	}
	waitStructuredExecution(t, r, result.Session)
	s, _ := r.State(context.Background())
	e := s.Executions[s.Members[result.Session].Execution]
	if calls.Load() != 2 || e.Status != "completed" || e.Result.Value != true || e.Completion.CallID == "uncertain" {
		t.Fatalf("uncertain result accepted: calls=%d execution=%+v", calls.Load(), e)
	}
}
