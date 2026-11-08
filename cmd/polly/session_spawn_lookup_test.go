package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

// These supplied capabilities use the real legacy store's atomic spawn query.
// The header fixture deliberately reads full views internally: it verifies host
// selection/fencing, not native SQL payload bounds or header efficiency.
type atomicSpawnViews struct {
	*countedHostSummaries
	reads      int
	beforeRead func(context.Context) error
	afterRead  func()
	readErr    error
}

func (s *atomicSpawnViews) lookup(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
	if s.beforeRead != nil {
		if err := s.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	if s.readErr != nil {
		return nil, s.readErr
	}
	view, err := s.SessionStore.(sessions.ViewStore).ReadView(ctx, target, known)
	if s.afterRead != nil {
		s.afterRead()
	}
	return view, err
}
func (s *atomicSpawnViews) ReadView(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
	s.reads++
	s.views = append(s.views, target)
	return s.lookup(ctx, target, known)
}

type atomicSpawnHeaders struct {
	*atomicSpawnViews
	headers []sessions.ViewTarget
}

func (s *atomicSpawnHeaders) ReadViewHeader(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionViewHeader, error) {
	s.headers = append(s.headers, target)
	view, err := s.lookup(ctx, target, known)
	if err != nil {
		return nil, err
	}
	return &sessions.SessionViewHeader{ID: view.ID, Revision: view.Revision, Epoch: "fixture", Sequence: view.Sequence, ParentID: view.ParentID, Metadata: view.Metadata, Artifacts: view.Artifacts, InUse: view.InUse, Unchanged: view.Unchanged}, nil
}

func hostSpawnChild(t *testing.T, store sessions.SessionStore, parent, name, callID string) string {
	t.Helper()
	ctx := context.Background()
	child, err := store.Acquire(ctx, name, sessions.AcquireOptions{Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	md, err := child.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md.SpawnCallID = callID
	if err := child.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}
	id := child.(sessions.ViewIdentity).ViewID()
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHostSpawnLookupAtomicAcrossLiveReordering(t *testing.T) {
	for _, header := range []bool{false, true} {
		t.Run(fmt.Sprint("header=", header), func(t *testing.T) {
			ctx := context.Background()
			base := testOpenMemoryStore(t, nil)
			parent := testAcquireSession(t, base, "parent")
			parentID := parent.(sessions.ViewIdentity).ViewID()
			if err := parent.Close(); err != nil {
				t.Fatal(err)
			}
			for n := range hostSummaryPageSize + 2 {
				call := fmt.Sprintf("call-%d", n)
				if n == 0 || n == hostSummaryPageSize+1 {
					call = "duplicate"
				}
				hostSpawnChild(t, base, "parent", fmt.Sprintf("child-%03d", n), call)
			}
			views := &atomicSpawnViews{countedHostSummaries: countedSummaries(t, base)}
			moved := false
			move := func(context.Context) error {
				moved = true
				child, err := base.Acquire(ctx, "child-000", sessions.AcquireOptions{ExistingOnly: true})
				if err != nil {
					return err
				}
				md, err := child.GetMetadata(ctx)
				if err == nil {
					md.Description = "move before live cursor"
					err = child.SetMetadata(ctx, md)
				}
				return errors.Join(err, child.Close())
			}
			// The old multi-page implementation skipped the moved duplicate.
			// The repaired path instead sees both in one atomic supplied read.
			views.beforePage = func(ctx context.Context) error {
				if len(views.queries) == 2 {
					return move(ctx)
				}
				return nil
			}
			views.beforeRead = move
			headers := &atomicSpawnHeaders{atomicSpawnViews: views}
			var supplied sessions.SessionStore = views
			if header {
				supplied = headers
			}
			got, err := launchedAgentSummary(ctx, supplied, parentID, "duplicate")
			if got != nil || err == nil || !strings.Contains(err.Error(), "ambiguous") || !moved {
				t.Fatalf("duplicate accepted: got=%+v err=%v moved=%v", got, err, moved)
			}
			if len(views.queries) != 0 || views.listed != 0 {
				t.Fatalf("spawn uniqueness used listing: pages=%d lists=%d", len(views.queries), views.listed)
			}
			if header && (len(headers.headers) != 1 || views.reads != 0) || !header && views.reads != 1 {
				t.Fatalf("wrong capability: headers=%d views=%d", len(headers.headers), views.reads)
			}
		})
	}
}

func TestHostSpawnLookupIdentityOwnershipAndCancellation(t *testing.T) {
	for _, header := range []bool{false, true} {
		for _, mode := range []string{"unchanged", "rename before lookup", "rename and reuse", "delete and reuse", "canceled before", "canceled during", "canceled after", "read error"} {
			t.Run(fmt.Sprint("header=", header)+"/"+mode, func(t *testing.T) {
				base := testOpenMemoryStore(t, nil)
				parent := testAcquireSession(t, base, "parent")
				parentID := parent.(sessions.ViewIdentity).ViewID()
				if err := parent.Close(); err != nil {
					t.Fatal(err)
				}
				childID := hostSpawnChild(t, base, "parent", "original-child", "launch")
				views := &atomicSpawnViews{countedHostSummaries: countedSummaries(t, base)}
				headers := &atomicSpawnHeaders{atomicSpawnViews: views}
				var supplied sessions.SessionStore = views
				if header {
					supplied = headers
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				rename := func() {
					p := testAcquireSession(t, base, "parent")
					if err := p.Rename(context.Background(), "renamed"); err != nil {
						t.Fatal(err)
					}
					if err := p.Close(); err != nil {
						t.Fatal(err)
					}
				}
				sentinel := errors.New("supplied lookup error")
				switch mode {
				case "rename before lookup":
					rename()
				case "rename and reuse", "delete and reuse":
					views.beforeRead = func(context.Context) error {
						if mode == "rename and reuse" {
							rename()
						} else if err := base.Delete(context.Background(), "parent"); err != nil {
							return err
						}
						if err := testAcquireSession(t, base, "parent").Close(); err != nil {
							return err
						}
						hostSpawnChild(t, base, "parent", "replacement-child", "launch")
						return nil
					}
				case "canceled before":
					cancel()
				case "canceled during":
					views.beforeRead = func(context.Context) error { cancel(); return nil }
				case "canceled after":
					views.afterRead = cancel
				case "read error":
					views.readErr = sentinel
				}
				got, err := launchedAgentSummary(ctx, supplied, parentID, "launch")
				switch mode {
				case "unchanged", "rename before lookup":
					if err != nil || got == nil || got.ID != childID || got.ParentID != parentID {
						t.Fatalf("identity=%+v err=%v", got, err)
					}
					got.Metadata.Name = "caller-owned mutation"
					fresh, err := base.(sessions.ViewStore).ReadView(context.Background(), sessions.ViewTarget{ID: childID}, "")
					if err != nil || fresh.Metadata.Name != "original-child" {
						t.Fatalf("lookup did not return owned metadata: %+v %v", fresh, err)
					}
				case "rename and reuse", "delete and reuse":
					if got != nil || !errors.Is(err, sessions.ErrSessionNotFound) {
						t.Fatalf("reused name redirected: %+v %v", got, err)
					}
				case "read error":
					if got != nil || !errors.Is(err, sentinel) {
						t.Fatalf("error was retried/hidden: %+v %v", got, err)
					}
				default:
					if got != nil || !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation=%+v %v", got, err)
					}
				}
				if len(views.queries) != 0 || views.listed != 0 {
					t.Fatal("atomic lookup enumerated summaries")
				}
				wantReads := 1
				if mode == "canceled before" {
					wantReads = 0
				}
				if header && (len(headers.headers) != wantReads || views.reads != 0) || !header && views.reads != wantReads {
					t.Fatalf("extra lookup/fallback: headers=%d views=%d", len(headers.headers), views.reads)
				}
				if len(views.ids) != wantReads || wantReads > 0 && views.ids[0] != parentID {
					t.Fatalf("parent was not selected by exact identity: %v", views.ids)
				}
				targets := views.views
				if header {
					targets = headers.headers
				}
				wantParent := "parent"
				if mode == "rename before lookup" {
					wantParent = "renamed"
				}
				for _, target := range targets {
					if target.ID != "" || target.Name != "" || target.Parent != wantParent || target.SpawnCallID != "launch" {
						t.Fatalf("unexpected atomic selector: %+v", target)
					}
				}
			})
		}
	}
}

func TestHostSpawnLookupRequiresSuppliedAtomicCapability(t *testing.T) {
	base := testOpenMemoryStore(t, nil)
	parent := testAcquireSession(t, base, "parent")
	id := parent.(sessions.ViewIdentity).ViewID()
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	hostSpawnChild(t, base, "parent", "child", "launch")
	counted := countedSummaries(t, base)
	// Deliberately hide the backend's ViewStore: a host may restrict capability.
	supplied := struct {
		sessions.SessionStore
		sessions.SummaryPageStore
	}{base, counted}
	got, err := launchedAgentSummary(context.Background(), supplied, id, "launch")
	if got != nil || !errors.Is(err, sessions.ErrPagingUnavailable) || len(counted.queries) != 0 {
		t.Fatalf("atomic capability bypass: %+v %v pages=%d", got, err, len(counted.queries))
	}
}
