package swarm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/workflow"
)

func workflowContext(t *testing.T, h *workflowHost) string {
	t.Helper()
	id, err := h.Call(context.Background(), workflow.Operation{Kind: "context", Args: map[string]any{"readOnly": true}})
	if err != nil {
		t.Fatal(err)
	}
	return id.(string)
}

// registeredWorkflowHost builds a host and registers it the way runWorkflow
// does, so workspace release can unbind it; both are undone at cleanup.
func registeredWorkflowHost(t *testing.T, r *Runtime) *workflowHost {
	t.Helper()
	h := &workflowHost{runtime: r, controller: "workflow"}
	r.mu.Lock()
	r.workflowHosts[h.controller] = h
	r.mu.Unlock()
	t.Cleanup(func() {
		r.mu.Lock()
		delete(r.workflowHosts, h.controller)
		r.mu.Unlock()
		h.close()
	})
	return h
}

func TestWorkflowHostOpensOncePerScopeAndReopensWhenItChanges(t *testing.T) {
	t.Parallel()
	r := scratchRuntime(t, doneModel(), true)
	if _, err := r.config.Registry.LoadToolAuto("list_dir"); err != nil {
		t.Fatal(err)
	}
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	h := registeredWorkflowHost(t, r)
	ctx := context.Background()
	first := workflowContext(t, h)
	list := func(id string) {
		t.Helper()
		value, err := h.Call(ctx, workflow.Operation{ID: "list", Kind: "tool", Args: map[string]any{"context": id, "name": "list_dir", "args": map[string]any{"path": "."}}})
		if err != nil {
			t.Fatal(err)
		}
		if value.(map[string]any)["text"] == "" {
			t.Fatal("empty listing")
		}
	}
	list(first)
	list(first)
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open"}) {
		t.Fatalf("two steps under one scope: %v", events)
	}
	// Another context's checkout becomes a denied read of the first, so its
	// scope changes and the next step reopens after closing the old binding.
	second := workflowContext(t, h)
	list(first)
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open", "close", "open"}) {
		t.Fatalf("scope change did not reopen: %v", events)
	}
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	latest := rec.scopes[len(rec.scopes)-1]
	if !slices.Contains(latest.Grant.DeniedReads, s.Contexts[second].Root) {
		t.Fatalf("reopened scope %+v does not deny the sibling %s", latest.Grant, s.Contexts[second].Root)
	}
	// Releasing a context closes its binding before the checkout goes; the
	// release itself finishes in the background.
	if _, err := h.Call(ctx, workflow.Operation{Kind: "release", Args: map[string]any{"context": first}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		_, bound := h.bound[first]
		h.mu.Unlock()
		events := rec.recorded()
		if !bound && events[len(events)-1] == "close" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("release left the binding open: bound=%v events=%v", bound, events)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkflowStepsRunOverAnIndependentToolset(t *testing.T) {
	t.Parallel()
	set := &independentToolset{}
	r := runtimeWithToolset(t, runtimeTest(t, doneModel(), 1, 4), set, nil)
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	ctx := context.Background()
	id := workflowContext(t, h)
	_, err := h.Call(ctx, workflow.Operation{ID: "exec", Kind: "exec", Args: map[string]any{"context": id, "command": "true"}})
	var failure *workflow.Error
	if !errors.As(err, &failure) || failure.Code != "tool_denied" {
		t.Fatalf("exec without a bash in the toolset = %v, want tool_denied", err)
	}
	value, err := h.Call(ctx, workflow.Operation{ID: "read", Kind: "tool", Args: map[string]any{"context": id, "name": "read_mem", "args": map[string]any{"path": "notes.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := value.(map[string]any)["text"]; got != "IN-MEMORY NOTES" {
		t.Fatalf("tool step = %v", value)
	}
	if len(set.scopes) != 1 || set.scopes[0].AllowedTools != nil || !set.scopes[0].Grant.ReadOnly {
		t.Fatalf("workflow scope = %+v", set.scopes)
	}
}

func TestWorkflowBindingRefreshesSandboxPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	private := filepath.Join(home, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "secret.txt")
	if err := os.WriteFile(path, []byte("permitted only by the layer"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := hookedRegistry(sandbox.Config{PrivateHome: true}, func(*exec.Cmd) error { return nil })
	defer registry.Close()
	if _, err := registry.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SetSandboxLayer("profile", &tools.SandboxLayer{Members: sandbox.Config{ReadPaths: []string{private}}}); err != nil {
		t.Fatal(err)
	}
	r := rebuildRuntime(t, runtimeTest(t, doneModel(), 1, 4), func(c *Config) { useRegistry(c, registry) })
	rec := &openRecorder{}
	r = runtimeWithOpen(t, r, rec)
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	id := workflowContext(t, h)
	ctx := context.Background()
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := func() error {
		bound, err := h.registry(ctx, s, s.Contexts[id])
		if err != nil {
			return err
		}
		tool, ok := bound.Get("read_file")
		if !ok {
			t.Fatal("read_file missing")
		}
		_, err = bound.ExecuteTool(ctx, tool, map[string]any{"path": path}, time.Second)
		return err
	}
	if err := read(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("profile save failed")
	if _, err := registry.SetSandboxLayerAndCommit("profile", nil, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("failed save: %v", err)
	}
	if err := read(); err != nil {
		t.Fatalf("failed save changed authority: %v", err)
	}
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open"}) {
		t.Fatalf("failed save reopened binding: %v", events)
	}
	if _, err := registry.SetSandboxLayer("profile", nil); err != nil {
		t.Fatal(err)
	}
	if err := read(); err == nil {
		t.Fatal("cached binding retained the removed read grant")
	}
	if events := filterEvents(rec.recorded(), "open", "close"); !slices.Equal(events, []string{"open", "close", "open"}) {
		t.Fatalf("committed policy change did not reopen binding: %v", events)
	}
}

func bindingReservationHost(t *testing.T, open tools.OpenTools) (*workflowHost, *State) {
	t.Helper()
	r := runtimeTest(t, doneModel(), 1, 4)
	r.config.OpenTools = open
	h := &workflowHost{runtime: r, controller: "workflow"}
	t.Cleanup(h.close)
	s := &State{Contexts: map[string]*ExecutionContext{}}
	for _, id := range []string{"a", "b", "c"} {
		s.Contexts[id] = &ExecutionContext{ID: id, Root: filepath.Join(t.TempDir(), id), Owner: h.controller}
	}
	return h, s
}

func awaitBindingSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("binding operation did not reach barrier")
	}
}

type bindingResult struct {
	registry *tools.ToolRegistry
	err      error
}

func asyncBinding(h *workflowHost, ctx context.Context, s *State, c *ExecutionContext) <-chan bindingResult {
	result := make(chan bindingResult, 1)
	go func() {
		registry, err := h.registry(ctx, s, c)
		result <- bindingResult{registry, err}
	}()
	return result
}

func awaitBinding(t *testing.T, result <-chan bindingResult) bindingResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("binding operation blocked")
		return bindingResult{}
	}
}

func TestWorkflowBindingConstructionDoesNotBlockOtherContexts(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	h, s := bindingReservationHost(t, func(_ context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
		if filepath.Base(scope.Root) == "a" {
			close(started)
			<-release
		}
		registry := tools.NewToolRegistry(nil)
		return tools.ToolBinding{Registry: registry, Close: registry.Close}, nil
	})
	ctx := context.Background()
	warm, err := h.registry(ctx, s, s.Contexts["b"])
	if err != nil {
		t.Fatal(err)
	}
	blocked := asyncBinding(h, ctx, s, s.Contexts["a"])
	awaitBindingSignal(t, started)
	for _, id := range []string{"b", "c"} {
		got := awaitBinding(t, asyncBinding(h, ctx, s, s.Contexts[id]))
		if got.err != nil || got.registry == nil || (id == "b" && got.registry != warm) || (id == "c" && got.registry == warm) {
			t.Fatalf("context %s did not get its own usable binding: %+v", id, got)
		}
	}
	releaseOnce.Do(func() { close(release) })
	if got := awaitBinding(t, blocked); got.err != nil {
		t.Fatal(got.err)
	}
}

func TestWorkflowBindingSameScopeSharesConstruction(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var opens, closes atomic.Int32
	h, s := bindingReservationHost(t, func(ctx context.Context, _ tools.ToolScope) (tools.ToolBinding, error) {
		if opens.Add(1) == 1 {
			close(started)
		}
		<-release
		registry := tools.NewToolRegistry(nil)
		return tools.ToolBinding{Registry: registry, Close: func() error {
			closes.Add(1)
			if ctx.Err() != nil {
				t.Error("successful construction context was canceled before binding close")
			}
			return registry.Close()
		}}, nil
	})
	ctx := context.Background()
	first := asyncBinding(h, ctx, s, s.Contexts["a"])
	awaitBindingSignal(t, started)
	var results []<-chan bindingResult
	for range 16 {
		results = append(results, asyncBinding(h, ctx, s, s.Contexts["a"]))
	}
	waitCtx, cancel := context.WithCancel(ctx)
	waiter := asyncBinding(h, waitCtx, s, s.Contexts["a"])
	cancel()
	if got := awaitBinding(t, waiter); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled waiter: %+v", got)
	}
	once.Do(func() { close(release) })
	installed := awaitBinding(t, first)
	if installed.err != nil {
		t.Fatal(installed.err)
	}
	for _, result := range results {
		got := awaitBinding(t, result)
		if got.err != nil || got.registry != installed.registry {
			t.Fatalf("same-scope caller did not share binding: %+v", got)
		}
	}
	h.close()
	h.close()
	if opens.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("opens=%d closes=%d", opens.Load(), closes.Load())
	}
}

func TestWorkflowBindingChangedScopeDiscardsLateConstruction(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var opens, closes atomic.Int32
	h, s := bindingReservationHost(t, func(ctx context.Context, _ tools.ToolScope) (tools.ToolBinding, error) {
		n := opens.Add(1)
		if n == 1 {
			close(started)
			<-ctx.Done()
		} else if closes.Load() != 1 {
			t.Error("replacement opened before loser cleanup")
		}
		registry := tools.NewToolRegistry(nil)
		return tools.ToolBinding{Registry: registry, Close: func() error {
			closes.Add(1)
			return registry.Close()
		}}, nil
	})
	ctx := context.Background()
	first := asyncBinding(h, ctx, s, s.Contexts["a"])
	awaitBindingSignal(t, started)
	changed := *s.Contexts["a"]
	changed.ReadOnly = true
	second := asyncBinding(h, ctx, s, &changed)
	if got := awaitBinding(t, first); !errors.Is(got.err, errWorkflowBindingInvalidated) || got.registry != nil {
		t.Fatalf("stale scope installed: %+v", got)
	}
	if got := awaitBinding(t, second); got.err != nil || got.registry == nil {
		t.Fatalf("replacement failed: %+v", got)
	}
	h.close()
	if opens.Load() != 2 || closes.Load() != 2 {
		t.Fatalf("opens=%d closes=%d", opens.Load(), closes.Load())
	}
}

func TestWorkflowBindingPolicyChangeDuringConstructionDiscardsLoser(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var opens, closes atomic.Int32
	h, s := bindingReservationHost(t, func(context.Context, tools.ToolScope) (tools.ToolBinding, error) {
		if opens.Add(1) == 1 {
			close(started)
			<-release
		}
		registry := tools.NewToolRegistry(nil)
		return tools.ToolBinding{Registry: registry, Close: func() error { closes.Add(1); return registry.Close() }}, nil
	})
	registry := hookedRegistry(sandbox.Config{}, func(*exec.Cmd) error { return nil })
	t.Cleanup(func() { registry.Close() })
	h.runtime.config.Registry = registry
	first := asyncBinding(h, context.Background(), s, s.Contexts["a"])
	awaitBindingSignal(t, started)
	if _, err := h.runtime.config.Registry.SetSandboxLayer("changed", &tools.SandboxLayer{Members: sandbox.Config{DenyWrite: true}}); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	if got := awaitBinding(t, first); !errors.Is(got.err, errWorkflowBindingInvalidated) || got.registry != nil {
		t.Fatalf("stale policy installed: %+v", got)
	}
	if closes.Load() != 1 {
		t.Fatal("stale policy binding leaked")
	}
	if _, err := h.registry(context.Background(), s, s.Contexts["a"]); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowBindingCloseDoesNotBlockOtherContexts(t *testing.T) {
	t.Parallel()
	closing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	h, s := bindingReservationHost(t, func(_ context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
		registry := tools.NewToolRegistry(nil)
		return tools.ToolBinding{Registry: registry, Close: func() error {
			if filepath.Base(scope.Root) == "a" {
				close(closing)
				<-release
			}
			return registry.Close()
		}}, nil
	})
	ctx := context.Background()
	if _, err := h.registry(ctx, s, s.Contexts["a"]); err != nil {
		t.Fatal(err)
	}
	warm, err := h.registry(ctx, s, s.Contexts["b"])
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); h.unbind("a") }()
	awaitBindingSignal(t, closing)
	for _, id := range []string{"b", "c"} {
		got := awaitBinding(t, asyncBinding(h, ctx, s, s.Contexts[id]))
		if got.err != nil || (id == "b" && got.registry != warm) {
			t.Fatalf("binding %s blocked by close: %+v", id, got)
		}
	}
	once.Do(func() { close(release) })
	awaitBindingSignal(t, done)
}

func TestWorkflowBindingCloseDrainsCallsAndRejectsNewOnes(t *testing.T) {
	t.Parallel()
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var closes atomic.Int32
	r := runtimeTest(t, doneModel(), 1, 4)
	r.config.OpenTools = func(context.Context, tools.ToolScope) (tools.ToolBinding, error) {
		registry := tools.NewToolRegistry(nil)
		registry.Register(&tools.Func{Name: "block", Run: func(context.Context, tools.Args) (string, error) {
			close(started)
			<-release
			close(finished)
			return "receipt", nil
		}})
		return tools.ToolBinding{Registry: registry, Close: func() error {
			select {
			case <-finished:
			default:
				t.Error("binding closed while its tool was still running")
			}
			closes.Add(1)
			return registry.Close()
		}}, nil
	}
	h := registeredWorkflowHost(t, r)
	id := workflowContext(t, h)
	call := make(chan error, 1)
	go func() {
		_, err := h.Call(context.Background(), workflow.Operation{Kind: "tool", Args: map[string]any{"context": id, "name": "block", "args": map[string]any{}}})
		call <- err
	}()
	awaitBindingSignal(t, started)
	closed := make(chan struct{})
	go func() { defer close(closed); h.close() }()
	once.Do(func() { close(release) })
	awaitBindingSignal(t, closed)
	if err := <-call; err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(context.Background(), workflow.Operation{Kind: "log"}); !errors.Is(err, errWorkflowHostClosed) || closes.Load() != 1 {
		t.Fatalf("closed host admitted work or leaked binding: err=%v closes=%d", err, closes.Load())
	}
}

func TestWorkflowBindingRetirementWaitsForCleanupWithoutGlobalLock(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"unbind", "close", "cancel"} {
		t.Run(action, func(t *testing.T) {
			started, closing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var closes atomic.Int32
			h, s := bindingReservationHost(t, func(ctx context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
				registry := tools.NewToolRegistry(nil)
				if filepath.Base(scope.Root) != "a" {
					return tools.ToolBinding{Registry: registry, Close: registry.Close}, nil
				}
				close(started)
				<-ctx.Done()
				// A constructor may finish despite cancellation; the caller still
				// owns this late binding and must close it before release returns.
				return tools.ToolBinding{Registry: registry, Close: func() error {
					closes.Add(1)
					close(closing)
					<-release
					return registry.Close()
				}}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := asyncBinding(h, ctx, s, s.Contexts["a"])
			awaitBindingSignal(t, started)
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch action {
				case "close":
					h.close()
				case "unbind":
					h.unbind("a")
				case "cancel":
					cancel()
					h.unbind("a")
				}
			}()
			awaitBindingSignal(t, closing)
			select {
			case <-done:
				t.Fatal("retirement returned before cleanup")
			default:
			}
			other := awaitBinding(t, asyncBinding(h, context.Background(), s, s.Contexts["b"]))
			if action == "close" {
				if !errors.Is(other.err, errWorkflowHostClosed) {
					t.Fatalf("closed host accepted binding: %+v", other)
				}
			} else if other.err != nil {
				t.Fatalf("unrelated binding blocked: %v", other.err)
			}
			once.Do(func() { close(release) })
			awaitBindingSignal(t, done)
			if got := awaitBinding(t, first); got.err == nil || got.registry != nil || closes.Load() != 1 {
				t.Fatalf("loser was not closed exactly once: %+v closes=%d", got, closes.Load())
			}
		})
	}
}
