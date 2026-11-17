package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// lifecycleMCPGate holds exactly one discovery or shutdown request. Other
// sessions stay usable, so the tests can also prove unrelated work progresses.
func lifecycleMCPGate(t *testing.T, method string) (string, <-chan struct{}, func(), *atomic.Bool, *atomic.Int32) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	armed, deleted := new(atomic.Bool), new(atomic.Int32)
	server := mcp.NewServer(&mcp.Implementation{Name: "lifecycle", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "probe", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted.Add(1)
		}
		match := method == "DELETE" && r.Method == http.MethodDelete
		if method == "tools/list" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			match = bytes.Contains(body, []byte(`"tools/list"`))
		}
		if match && armed.Swap(false) {
			close(entered)
			<-release
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { unblock(); hs.CloseClientConnections(); hs.Close() })
	return hs.URL, entered, unblock, armed, deleted
}

func awaitLifecycle[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle operation did not finish")
		var zero T
		return zero
	}
}

func TestMCPRegistryCloseJoinsRetirements(t *testing.T) {
	for _, operation := range []string{"remove", "reload", "restart", "filter", "pending", "pending-replace", "shell", "shell-batch"} {
		t.Run(operation, func(t *testing.T) {
			var opts []RegistryOption
			var script string
			if operation == "shell" || operation == "shell-batch" {
				skipIfWindows(t)
				opts = append(opts, WithUnsafeNoSandbox())
				script = writeShellToolScript(t, t.TempDir(), "srv.sh", `{"title":"probe","type":"object"}`, "true")
			}
			url, entered, unblock, armed, _ := lifecycleMCPGate(t, "DELETE")
			r := NewToolRegistry(nil, opts...)
			defer r.Close()
			defer unblock()
			path := writeRegistryMCPConfig(t, MCPConfig{URL: url, Transport: "streamable"})
			if operation == "pending-replace" {
				records, _, err := r.prepareMCPServerWithNamespacePrefix(context.Background(), path, "")
				if err != nil {
					t.Fatal(err)
				}
				r.stagePreparedTools(records)
			} else if _, err := r.LoadMCPServer(path); err != nil {
				t.Fatal(err)
			}
			armed.Store(true)
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "remove":
					r.Remove("srv__probe")
				case "reload":
					_, err = r.LoadMCPServer(path)
				case "restart":
					_, err = r.RestartMCPServer("srv")
				case "filter":
					err = r.LoadMCPServerWithFilter(path+"#srv", []string{"probe"})
				case "pending", "pending-replace":
					r.stagePreparedTools([]stagedToolRecord{{name: "srv__probe", tool: &testTool{name: "srv__probe"}}})
					r.CommitPendingChanges()
				case "shell":
					_, err = r.LoadShellTool(script)
				case "shell-batch":
					_, err = LoadShellToolsWithRegistry(r, []string{script})
				}
				done <- err
			}()
			awaitLifecycle(t, entered)
			closed, closedAgain := make(chan error, 1), make(chan error, 1)
			go func() { closed <- r.Close() }()
			go func() { closedAgain <- r.Close() }()
			select {
			case err := <-closed:
				t.Fatalf("Close skipped an earlier blocked retirement: %v", err)
			case err := <-closedAgain:
				t.Fatalf("concurrent Close skipped an earlier blocked retirement: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			// Both the map and policy locks must remain free even while Close
			// joins a detached retirement. Independent construction may commit.
			independent := make(chan error, 1)
			go func() {
				r.Get("unrelated")
				r.SandboxPolicyRevision()
				_, err := r.LoadMCPServerWithNamespacePrefix(path, "independent")
				independent <- err
			}()
			if err := awaitLifecycle(t, independent); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				t.Fatalf("retiring operation returned before cleanup: %v", err)
			default:
			}
			unblock()
			for _, ch := range []<-chan error{done, closed, closedAgain} {
				if err := awaitLifecycle(t, ch); err != nil {
					t.Fatal(err)
				}
			}
			r.mu.RLock()
			pending := len(r.retirements)
			r.mu.RUnlock()
			if pending != 0 {
				t.Fatalf("completed retirements retained: %d", pending)
			}
		})
	}
}

func TestMCPLoadAbsentNameMutation(t *testing.T) {
	for _, mutation := range []string{"remove-absent", "install-remove", "replace-remove", "filter-remove"} {
		t.Run(mutation, func(t *testing.T) {
			url, entered, unblock, armed, deleted := lifecycleMCPGate(t, "tools/list")
			r := NewToolRegistry(nil)
			defer r.Close()
			defer unblock()
			path := writeRegistryMCPConfig(t, MCPConfig{URL: url, Transport: "streamable"})
			armed.Store(true)
			done := make(chan error, 1)
			go func() { _, err := r.LoadMCPServer(path); done <- err }()
			awaitLifecycle(t, entered)
			switch mutation {
			case "install-remove", "filter-remove":
				if _, err := r.LoadMCPServer(path); err != nil {
					t.Fatal(err)
				}
			case "replace-remove":
				r.Register(&testTool{name: "srv__probe"})
			}
			if mutation == "filter-remove" {
				if err := r.LoadMCPServerWithFilter(path+"#srv", nil); err != nil {
					t.Fatal(err)
				}
			} else {
				r.Remove("srv__probe")
			}
			independent := make(chan error, 1)
			go func() { _, err := r.LoadMCPServerWithNamespacePrefix(path, "independent"); independent <- err }()
			if err := awaitLifecycle(t, independent); err != nil {
				t.Fatalf("unrelated load blocked or rejected: %v", err)
			}
			before := deleted.Load()
			unblock()
			if err := awaitLifecycle(t, done); err == nil {
				t.Fatal("stale absent-name load was installed")
			}
			if _, exists := r.Get("srv__probe"); exists || deleted.Load() != before+1 {
				t.Fatalf("stale tool resurrected=%t; candidate shutdowns=%d", exists, deleted.Load()-before)
			}
			if len(r.mcpLoads) != 0 || len(r.toolVersions) != len(r.tools) {
				t.Fatalf("completed reservations or tombstones retained: loads=%d versions=%d tools=%d", len(r.mcpLoads), len(r.toolVersions), len(r.tools))
			}
		})
	}
}

func TestMCPLoadReservationPrunesObsoleteAbsence(t *testing.T) {
	r := NewToolRegistry(nil)
	defer r.Close()
	old := r.snapshotMCPLoad()
	r.Remove("absent")
	newer := r.snapshotMCPLoad()
	r.releaseMCPLoad(old)
	if len(r.toolVersions) != 0 || len(r.mcpLoads) != 1 {
		t.Fatalf("old tombstone retained solely for a newer reservation: %v", r.toolVersions)
	}
	if err := r.commitMCPLoad(context.Background(), newer, []stagedToolRecord{{name: "absent", tool: &testTool{name: "absent"}}}, ""); err != nil {
		t.Fatal(err)
	}
	r.releaseMCPLoad(newer)
	r.Remove("absent")
	for i := range 100 {
		r.Remove(fmt.Sprintf("absent-%d", i))
	}
	if len(r.toolVersions) != 0 || len(r.mcpLoads) != 0 {
		t.Fatal("idle registry retained mutation history")
	}
	// Preparation failures (including pre-cancellation) must release their
	// reservations, not pin all subsequent absent-name mutations indefinitely.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.LoadMCPServerContext(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load: %v", err)
	}
	if _, err := r.RestartMCPServer("missing"); err == nil {
		t.Fatal("restart unexpectedly succeeded")
	}
	if err := r.LoadMCPServerWithFilter("missing", nil); err == nil {
		t.Fatal("filtered load unexpectedly succeeded")
	}
	if len(r.mcpLoads) != 0 {
		t.Fatal("failed preparation leaked reservations")
	}
}

type lifecycleContextTool struct {
	Tool
	bind func(*ToolRegistry) Tool
}

func (t *lifecycleContextTool) BindExecutionContext(r *ToolRegistry, _ ExecutionContext) (Tool, error) {
	return t.bind(r), nil
}

func TestNativeBindingRevalidatesContextCallbacks(t *testing.T) {
	url, _ := registryMCPServer(t, "probe")
	path := writeRegistryMCPConfig(t, MCPConfig{URL: url, Transport: "streamable", ContextIndependent: true})
	source := NewToolRegistry(nil, WithNativeTools())
	defer source.Close()
	if _, err := source.LoadMCPServer(path); err != nil {
		t.Fatal(err)
	}
	original := source.toolClients["srv__probe"]
	entered := make(chan *ToolRegistry, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	source.Register(&lifecycleContextTool{Tool: &testTool{name: "zz_gate"}, bind: func(bound *ToolRegistry) Tool {
		entered <- bound
		<-release
		return &testTool{name: "zz_gate"}
	}})
	done := make(chan error, 1)
	root := t.TempDir()
	go func() {
		bound, _, err := source.BindExecutionContextWithContext(context.Background(), ExecutionContext{Root: root}, []string{"srv__probe", "zz_gate"})
		if bound != nil {
			bound.Close()
		}
		done <- err
	}()
	bound := awaitLifecycle(t, entered)
	candidate := bound.toolClients["srv__probe"]
	source.Register(&testTool{name: "zz_gate"})
	unblock()
	if err := awaitLifecycle(t, done); err == nil {
		t.Fatal("context callback published a binding after source replacement")
	}
	if candidate == nil || candidate == original || !candidate.Closed() || original.Closed() {
		t.Fatal("failed context callback binding disposed the wrong client")
	}
}

func TestNativeOpenFinalCloseGenerationWithoutSourceTools(t *testing.T) {
	source := NewToolRegistry(nil, WithNativeTools())
	source.Close() // A reusable source with no names cannot be fenced by name identity alone.
	defer source.Close()
	open := NativeOpenTools(source, WithNativeInstructions(func(*ToolRegistry) string {
		source.Close()
		return "guidance"
	}))
	binding, err := open(context.Background(), ToolScope{Root: t.TempDir()})
	if err == nil {
		binding.Close()
		t.Fatal("Close during instructions was lost for an empty source selection")
	}
}

type lifecycleBorrowedTool struct{ Tool }

func (*lifecycleBorrowedTool) ContextIndependent() bool { return true }

func TestNativeOpenFinalSourceRevalidation(t *testing.T) {
	for _, ownership := range []string{"owned", "borrowed"} {
		for _, mutation := range []string{"remove", "close", "replace", "narrow", "policy", "cancel", "independent", "unselected"} {
			t.Run(ownership+"/"+mutation, func(t *testing.T) {
				url, _ := registryMCPServer(t, "probe")
				path := writeRegistryMCPConfig(t, MCPConfig{URL: url, Transport: "streamable", ContextIndependent: true})
				owner := stubSandboxRegistry(t, sandbox.Config{})
				defer owner.Close()
				if _, err := owner.LoadMCPServer(path); err != nil {
					t.Fatal(err)
				}
				original := owner.toolClients["srv__probe"]
				if ownership == "borrowed" {
					tool, _ := owner.Get("srv__probe")
					owner.Register(&lifecycleBorrowedTool{Tool: tool})
				}
				owner.Register(&testTool{name: "unselected"})
				entered := make(chan *ToolRegistry, 1)
				release := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				open := NativeOpenTools(owner, WithNativeInstructions(func(bound *ToolRegistry) string {
					entered <- bound
					<-release
					return "guidance"
				}))
				type result struct {
					binding ToolBinding
					err     error
				}
				done := make(chan result, 1)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				root := t.TempDir()
				go func() {
					binding, err := open(ctx, ToolScope{Root: root, AllowedTools: []string{"srv__probe"}})
					done <- result{binding, err}
				}()
				bound := awaitLifecycle(t, entered)
				candidate := bound.toolClients["srv__probe"]
				if ownership == "borrowed" && candidate != nil || ownership != "borrowed" && (candidate == nil || candidate == original) {
					t.Fatal("unexpected candidate ownership")
				}
				switch mutation {
				case "remove":
					owner.Remove("srv__probe")
				case "close":
					owner.Close()
				case "replace":
					if ownership == "borrowed" {
						owner.Register(&lifecycleBorrowedTool{Tool: &testTool{name: "srv__probe"}})
					} else if _, err := owner.LoadMCPServer(path); err != nil {
						t.Fatal(err)
					}
				case "narrow":
					owner.stageSkillAllowance([]string{"unselected"}, nil)
					owner.CommitPendingChanges()
				case "policy":
					if _, err := owner.AppendBaseReadPaths(t.TempDir()); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					cancel()
				case "independent":
					owner.Register(&testTool{name: "independent"})
				case "unselected":
					owner.Remove("unselected")
				}
				unblock()
				got := awaitLifecycle(t, done)
				if mutation == "independent" || mutation == "unselected" {
					if got.err != nil {
						t.Fatalf("unrelated source mutation rejected binding: %v", got.err)
					}
					if got.binding.Instructions != "guidance" {
						t.Fatal("instructions lost")
					}
					got.binding.Close()
				} else if got.err == nil {
					got.binding.Close()
					t.Fatal("binding published after source mutation during instructions")
				}
				if mutation == "cancel" && !errors.Is(got.err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", got.err)
				}
				if candidate != nil && !candidate.Closed() || len(bound.toolClients) != 0 || len(bound.All()) != 0 {
					t.Fatal("unpublished/closed binding leaked owned resources")
				}
				if mutation != "close" && mutation != "remove" && (mutation != "replace" || ownership == "borrowed") && original.Closed() {
					t.Fatal("candidate disposal closed a borrowed source client")
				}
			})
		}
	}
}
