package swarm

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// openRecorder wraps an OpenTools, recording every scope it is asked for
// and the order of opens and closes next to other events a test records.
type openRecorder struct {
	mu     sync.Mutex
	inner  tools.OpenTools
	scopes []tools.ToolScope
	events []string
	fail   error
}

func (o *openRecorder) record(event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *openRecorder) recorded() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func (o *openRecorder) open(ctx context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
	o.mu.Lock()
	o.scopes = append(o.scopes, scope)
	fail := o.fail
	o.mu.Unlock()
	if fail != nil {
		return tools.ToolBinding{}, fail
	}
	binding, err := o.inner(ctx, scope)
	if err != nil {
		return binding, err
	}
	o.record("open")
	closeBinding := binding.Close
	binding.Close = func() error {
		o.record("close")
		return closeBinding()
	}
	return binding, nil
}

// recordingStore records every session acquisition next to the recorder's
// open and close events.
type recordingStore struct {
	sessions.SessionStore
	rec *openRecorder
}

func (s *recordingStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	session, err := s.SessionStore.Acquire(ctx, name, opts)
	if err == nil {
		s.rec.record("acquire:" + name)
	}
	return session, err
}

// runtimeWithOpen rebuilds r so that its bindings open through the
// recorder, which wraps the runtime's configured OpenTools.
func runtimeWithOpen(t *testing.T, r *Runtime, o *openRecorder) *Runtime {
	t.Helper()
	return rebuildRuntime(t, r, func(c *Config) {
		o.inner = c.OpenTools
		c.OpenTools = o.open
		c.Store = &recordingStore{SessionStore: c.Store, rec: o}
	})
}

func TestMemberToolsOpenAfterTheLeaseAndCloseWithTheSlice(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 1)
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "answer", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := s.Members[result.Session].Name
	events := rec.recorded()
	open := slices.Index(events, "open")
	lease := slices.Index(events, "acquire:"+name)
	if open < 0 || lease < 0 || lease > open {
		t.Fatalf("tools opened before the member lease: %v", events)
	}
	if closed := slices.Index(events, "close"); closed < open {
		t.Fatalf("binding not closed with the slice: %v", events)
	}
	if len(rec.scopes) != 1 || rec.scopes[0].AllowedTools != nil || !rec.scopes[0].Grant.ReadOnly {
		t.Fatalf("scopes = %+v", rec.scopes)
	}
}

func TestMemberScopeCarriesTheContextAuthority(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	if _, err := r.config.Registry.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "edit", Tools: []string{"read_file"}}); err != nil {
		t.Fatal(err)
	}
	if len(rec.scopes) != 1 {
		t.Fatalf("scopes = %+v", rec.scopes)
	}
	scope := rec.scopes[0]
	manager, err := r.manager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := r.runtimeDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(scope.Root, dir) || scope.Root == r.config.Root {
		t.Fatalf("root %q is not a checkout under %q", scope.Root, dir)
	}
	if scope.Grant.SourceRoot != r.config.Root {
		t.Fatalf("source root = %q, want %q", scope.Grant.SourceRoot, r.config.Root)
	}
	for _, denied := range []string{dir, r.config.Root} {
		if !slices.Contains(scope.Grant.DeniedReads, denied) {
			t.Fatalf("denied reads %v lack %q", scope.Grant.DeniedReads, denied)
		}
	}
	if want := []string{manager.GitDir, scope.Root + "/.git"}; !reflect.DeepEqual(scope.Grant.DeniedWrites, want) {
		t.Fatalf("denied writes = %v, want %v", scope.Grant.DeniedWrites, want)
	}
	if len(scope.ReadPaths) == 0 || scope.ReadPaths[0] != manager.GitDir {
		t.Fatalf("read paths = %v, want the Git directory first", scope.ReadPaths)
	}
	if scope.Grant.ReadOnly || scope.Grant.Scratch == "" || filepath.Dir(scope.Grant.Scratch) != scratch.Root() {
		t.Fatalf("grant = %+v", scope.Grant)
	}
	if !slices.Equal(scope.AllowedTools, []string{"read_file"}) {
		t.Fatalf("allowed tools = %v", scope.AllowedTools)
	}
}

func TestOpenToolsFailureFailsTheExecutionBeforeAnyModelCall(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return answer("never")
	}), 1, 1)
	rec := &openRecorder{fail: errors.New("tool backend unreachable")}
	r = runtimeWithOpen(t, r, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "answer", ReadOnly: true})
	if err == nil || !strings.Contains(err.Error(), "tool backend unreachable") {
		t.Fatalf("spawn = %v, want the binding failure", err)
	}
	if calls.Load() != 0 {
		t.Fatal("the model was called without tools")
	}
	if events := rec.recorded(); slices.Contains(events, "open") || slices.Contains(events, "close") {
		t.Fatalf("events = %v", events)
	}
}

func TestParkAndResumeOpenFreshTools(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "ask", Name: "send_message", Arguments: tools.Result(map[string]any{"target": r.ID, "message": "which option?"})}, {ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		return answer("completed")
	})
	r = runtimeTest(t, model, 1, 1)
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "ask parent", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("%+v %v", result, err)
	}
	if events := rec.recorded(); !slices.Equal(filterEvents(events, "open", "close"), []string{"open", "close"}) {
		t.Fatalf("parked member kept its binding: %v", events)
	}
	if _, err = r.Send(ctx, r.ID, result.Session, "info", "", "choose A"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal("member failed to resume")
	}
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open", "close", "open", "close"}) {
		t.Fatalf("resume did not reopen tools: %v", events)
	}
	if len(rec.scopes) != 2 || !reflect.DeepEqual(rec.scopes[0], rec.scopes[1]) {
		t.Fatalf("resumed scope differs: %+v", rec.scopes)
	}
}

func TestRecoveryReopensThroughTheSameOpenTools(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "ask", Name: "send_message", Arguments: tools.Result(map[string]any{"target": r.ID, "message": "which option?"})}, {ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		return answer("recovered")
	})
	r = runtimeTest(t, model, 1, 1)
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "wait then recover", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("%+v %v", result, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Resume(ctx, result.Session, 0); err != nil {
		t.Fatal(err)
	}
	for {
		recovered.mu.Lock()
		i := recovered.active[result.Session]
		recovered.mu.Unlock()
		if i == nil {
			break
		}
		select {
		case <-i.done:
		case <-ctx.Done():
			t.Fatal("resume did not finish")
		}
	}
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open", "close", "open", "close"}) {
		t.Fatalf("recovery did not reopen tools: %v", events)
	}
	if len(rec.scopes) != 2 || !reflect.DeepEqual(rec.scopes[0], rec.scopes[1]) {
		t.Fatalf("recovered scope differs: %+v", rec.scopes)
	}
	s, err := recovered.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e := s.Executions[s.Members[result.Session].Execution]; e.Status != "completed" {
		t.Fatalf("execution: %+v", e)
	}
}

func filterEvents(events []string, keep ...string) []string {
	var out []string
	for _, event := range events {
		if slices.Contains(keep, event) {
			out = append(out, event)
		}
	}
	return out
}
