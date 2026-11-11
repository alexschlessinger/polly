package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// observedMCPSandbox delegates to the native sandbox, retaining only the
// command and its descriptor cleanup for assertions after Wait. This legacy
// wrapper owns those descriptors until construction returns; the done channel
// synchronizes cleanup with exec.Cmd.Start's descriptor reads.
type observedMCPSandbox struct {
	inner   sandbox.Sandbox
	started chan<- observedMCPCommand
}

type observedMCPCommand struct {
	cmd     *exec.Cmd
	cleanup func() error
}

func (s observedMCPSandbox) Wrap(cmd *exec.Cmd) error {
	cleanup, err := sandbox.WrapCmdManaged(s.inner, cmd)
	if err != nil {
		return err
	}
	s.started <- observedMCPCommand{cmd: cmd, cleanup: cleanup}
	return nil
}

func waitMCPFixtureReady(t *testing.T, path string, done <-chan error) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("setup returned before stalled fixture was ready: %v", err)
		case <-timeout.C:
			t.Fatal("stalled fixture did not become ready")
		case <-ticker.C:
		}
	}
}

// This is an explicit native-security check, not a successful skip reported as
// cancellation coverage. Each fixture ignores EOF and has a 10-second watchdog.
func TestRuntimeStorageMCPStalledCancellation(t *testing.T) {
	skipUnlessSandboxTests(t)
	for _, phase := range []string{"server/discover", "initialize", "tools/list"} {
		for _, binding := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/binding=%t", phase, binding), func(t *testing.T) {
				dir := realTempDir(t)
				ready := filepath.Join(dir, "ready")
				path := writeRegistryMCPConfig(t, stalledMCPFixtureConfig(t, phase, ready))
				observed := make(chan observedMCPCommand, 1)
				factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
					inner, err := sandbox.New(cfg)
					if err != nil {
						return nil, err
					}
					return observedMCPSandbox{inner: inner, started: observed}, nil
				}
				registry := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(factory, sandbox.Config{WritablePaths: []string{dir}}))
				defer registry.Close()
				// A caller-owned template supplies the selection, not a process
				// to pool or close. NativeOpenTools must build its own server.
				template := &MCPClient{}
				if binding {
					registry.Register(&NamespacedTool{Tool: &MCPTool{client: template, tool: &mcp.Tool{Name: "probe"}, Source: path}, namespacedName: "srv__probe"})
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				finished := make(chan struct{})
				defer func() {
					cancel()
					<-finished
					select {
					case command := <-observed:
						_ = command.cleanup()
					default:
					}
				}()
				start := time.Now()
				go func() {
					defer close(finished)
					if binding {
						bound, err := NativeOpenTools(registry)(ctx, ToolScope{Root: dir, AllowedTools: []string{"srv__probe"}})
						if err == nil {
							bound.Close()
						}
						done <- err
					} else {
						_, err := registry.LoadMCPServerContext(ctx, path)
						done <- err
					}
				}()
				waitMCPFixtureReady(t, ready, done)
				setup := time.Since(start)
				command := <-observed
				defer func() {
					<-finished
					_ = command.cleanup()
				}()
				cancelled := time.Now()
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation error = %v", err)
					}
				case <-time.After(2 * time.Second):
					// The fixture watchdog bounds this failure path. Wait before
					// inspecting exec.Cmd or releasing its owned descriptors.
					<-done
					_ = command.cleanup()
					t.Fatal("cancellation waited for the SDK shutdown grace period")
				}
				latency := time.Since(cancelled)
				if err := command.cleanup(); err != nil {
					t.Fatal(err)
				}
				if command.cmd.ProcessState == nil {
					t.Fatal("owned child was not waited/reaped before returning")
				}
				if template.Closed() {
					t.Fatal("caller-owned template was closed")
				}
				if len(registry.toolClients) != 0 {
					t.Fatal("cancelled setup installed a client")
				}
				t.Logf("native stalled MCP: setup_to_ready=%s cancel_to_return_and_reap=%s pid=%d", setup, latency, command.cmd.Process.Pid)
			})
		}
	}
}

func TestRuntimeStorageMCPCloseDoesNotHoldRegistryLocks(t *testing.T) {
	for _, operation := range []string{"remove", "reload", "filtered", "pending", "close"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			server := mcp.NewServer(&mcp.Implementation{Name: "close-gate", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "probe", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodDelete {
					once.Do(func() { close(entered) })
					<-release
				}
				handler.ServeHTTP(w, req)
			}))
			defer httpServer.Close()
			defer unblock()
			registry := NewToolRegistry(nil, WithNativeTools())
			defer registry.Close()
			defer unblock()
			path := writeRegistryMCPConfig(t, MCPConfig{URL: httpServer.URL, Transport: "streamable"})
			if _, err := registry.LoadMCPServer(path); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "remove":
					registry.Remove("srv__probe")
				case "reload":
					_, err = registry.LoadMCPServer(path)
				case "filtered":
					err = registry.LoadMCPServerWithFilter(path+"#srv", nil)
				case "pending":
					registry.stagePreparedTools([]stagedToolRecord{{name: "srv__probe", tool: &testTool{name: "srv__probe"}}})
					registry.CommitPendingChanges()
				case "close":
					err = registry.Close()
				}
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("operation did not close client: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("close was not reached")
			}
			lookedUp := make(chan time.Duration, 1)
			go func() {
				start := time.Now()
				registry.Get("unrelated")
				registry.SandboxPolicyRevision()
				lookedUp <- time.Since(start)
			}()
			select {
			case elapsed := <-lookedUp:
				t.Logf("lookup plus policy-lock read during blocked shutdown: %s", elapsed)
			case <-time.After(time.Second):
				t.Fatal("external close held an unrelated registry lock")
			}
			select {
			case err := <-done:
				t.Fatalf("shutdown returned before server close was released: %v", err)
			default:
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A small controlled curve: per-session ListTools latency is known, all
// bindings own fresh clients, and lookup latency is measured separately. This
// is not a post-coordination or end-to-end runtime speedup measurement.
func TestRuntimeStorageMCPSetupCurve(t *testing.T) {
	const serverDelay = 15 * time.Millisecond
	for _, concurrency := range []int{1, 4, 16} {
		t.Run(fmt.Sprint(concurrency), func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "setup-curve", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "probe", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			var posts atomic.Int64
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				// Bounded delay on each POST, including initialization and discovery.
				if req.Method == http.MethodPost {
					posts.Add(1)
					time.Sleep(serverDelay)
				}
				handler.ServeHTTP(w, req)
			}))
			defer httpServer.Close()
			path := writeRegistryMCPConfig(t, MCPConfig{URL: httpServer.URL, Transport: "streamable", ContextIndependent: true})
			source := NewToolRegistry(nil, WithNativeTools())
			defer source.Close()
			if _, err := source.LoadMCPServer(path); err != nil {
				t.Fatal(err)
			}
			posts.Store(0)
			root := t.TempDir()
			open := NativeOpenTools(source)
			var wg sync.WaitGroup
			latencies := make([]time.Duration, concurrency)
			errors := make([]error, concurrency)
			start := time.Now()
			for i := range concurrency {
				wg.Go(func() {
					began := time.Now()
					binding, err := open(context.Background(), ToolScope{Root: root})
					latencies[i], errors[i] = time.Since(began), err
					if err == nil {
						binding.Close()
					}
				})
			}
			lookupStart := time.Now()
			for range 100 {
				source.Get("srv__probe")
			}
			lookupTime := time.Since(lookupStart)
			wg.Wait()
			wall := time.Since(start)
			for _, err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
			slices.Sort(latencies)
			t.Logf("bindings=%d completed=%d posts=%d delay_per_POST=%s setup_p50=%s setup_p95=%s setup_max=%s wall_including_close=%s source_100_lookups=%s", concurrency, concurrency, posts.Load(), serverDelay, latencies[(concurrency-1)/2], latencies[(95*concurrency+99)/100-1], latencies[concurrency-1], wall, lookupTime)
		})
	}
}
