package swarm

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestOpenWorktreesSeparatesAdministrationAndSharesOneManager(t *testing.T) {
	t.Parallel()
	r := scratchRuntime(t, doneModel(), true)
	admin := r.config.Registry
	modelTools := tools.NewToolRegistry(nil)
	defer modelTools.Close()
	var calls atomic.Int32
	r = rebuildRuntime(t, r, func(c *Config) {
		c.Registry = modelTools
		c.PrivatePaths = []string{"private.db"}
		c.MaxWorktrees = 3
		c.OpenWorktrees = func(ctx context.Context, config worktree.Config) (*worktree.Manager, error) {
			calls.Add(1)
			if config.Registry != nil || config.Root != c.Root || config.Directory != c.Directory || config.MaxWorktrees != 3 {
				t.Errorf("unexpected workspace configuration: %+v", config)
			}
			if !reflect.DeepEqual(config.PrivatePaths, []string{filepath.Join(c.Root, "private.db")}) {
				t.Errorf("unresolved exclusions: %v", config.PrivatePaths)
			}
			config.Registry = admin
			m, err := worktree.New(ctx, config)
			config.PrivatePaths[0] = "changed by constructor"
			return m, err
		}
	})
	if calls.Load() != 0 {
		t.Fatal("construction was not lazy")
	}
	var wg sync.WaitGroup
	managers := make(chan *worktree.Manager, 8)
	for range 8 {
		wg.Go(func() {
			m, err := r.manager(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			managers <- m
		})
	}
	wg.Wait()
	close(managers)
	for m := range managers {
		if m != r.worktrees || m.Registry != admin {
			t.Fatal("manager was replaced or borrowed model tools for Git")
		}
	}
	if calls.Load() != 1 || r.config.PrivatePaths[0] != filepath.Join(r.config.Root, "private.db") {
		t.Fatal("constructor was repeated or mutated runtime exclusions")
	}
	if _, err := r.worktrees.Capture(context.Background(), r.config.Root); err != nil {
		t.Fatalf("native Git with independent model tools: %v", err)
	}
}

func TestOpenWorktreesErrorsNeverBecomeReadOnlyFallback(t *testing.T) {
	t.Parallel()
	denied := errors.New("host denied workspace construction")
	for _, kind := range []string{"missing", "failure", "nil", "not repository"} {
		t.Run(kind, func(t *testing.T) {
			r := runtimeTest(t, doneModel(), 1, 2)
			var calls int
			r = rebuildRuntime(t, r, func(c *Config) {
				c.OpenWorktrees = nil
				if kind != "missing" {
					c.OpenWorktrees = func(context.Context, worktree.Config) (*worktree.Manager, error) {
						calls++
						switch kind {
						case "failure":
							return nil, denied
						case "not repository":
							return nil, worktree.ErrNotRepository
						default:
							return nil, nil
						}
					}
				}
			})
			_, err := r.Agent(context.Background(), "", AgentRequest{Label: "Research", Task: "inspect", ReadOnly: true})
			if kind == "not repository" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || kind == "missing" && !errors.Is(err, ErrWorktreesUnavailable) || kind == "failure" && !errors.Is(err, denied) {
				t.Fatalf("constructor failure lost: %v", err)
			}
			s, err := r.State(context.Background())
			if err != nil || len(s.Contexts) != 0 || len(s.Executions) != 0 {
				t.Fatalf("failed construction left execution state: %+v %v", s, err)
			}
			if kind != "missing" {
				_, _ = r.manager(context.Background())
				if calls != 2 {
					t.Fatalf("failed constructor was cached: calls=%d", calls)
				}
			}
		})
	}
}

func TestRecoveryRequiresConfiguredWorktrees(t *testing.T) {
	t.Parallel()
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	if _, err := r.Agent(context.Background(), "", AgentRequest{Label: "Research", Task: "inspect", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := r.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	config := r.config
	config.OpenWorktrees = nil
	if recovered, err := New(config); !errors.Is(err, ErrWorktreesUnavailable) {
		if recovered != nil {
			recovered.Close()
		}
		t.Fatalf("recovery accepted missing workspace backend: %v", err)
	}
	after, err := r.read(context.Background())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused recovery changed saved state: %v", err)
	}
	config.OpenWorktrees = r.config.OpenWorktrees
	recovered, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := recovered.manager(context.Background()); err != nil {
		t.Fatalf("recovery with configured manager: %v", err)
	}
}

type hostOwnedStorage struct{ sessions.SessionStore }

func (hostOwnedStorage) Location() (sessions.StoreMode, string) {
	panic("runtime must not discover storage")
}
func (hostOwnedStorage) Promote(context.Context, string) error {
	panic("runtime must use the host promotion callback")
}

func TestRuntimeUsesOnlyHostStorageConfiguration(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, doneModel(), 1, 1)
	var promoted int
	r = rebuildRuntime(t, r, func(c *Config) {
		c.Store = hostOwnedStorage{c.Store}
		c.PrivatePaths = []string{"host-private"}
		c.Promote = func(context.Context) error { promoted++; return nil }
	})
	if promoted != 0 {
		t.Fatal("storage promoted before coordination")
	}
	for range 2 {
		if _, err := r.CreateTask(context.Background(), "hosted task", "", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if promoted != 1 || !reflect.DeepEqual(r.config.PrivatePaths, []string{filepath.Join(r.config.Root, "host-private")}) {
		t.Fatal("host storage configuration was not preserved")
	}
}
