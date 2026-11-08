package swarm

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

func changeTestScope(key string) sessions.RevisionScope {
	return sessions.RevisionScope{Index: sessions.CoordinationIndexMailPending, Key: key}
}

func changeTestReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for change test barrier")
		var zero T
		return zero
	}
}

func changeTestQuiet(t *testing.T, s *changeSubscription) {
	t.Helper()
	select {
	case <-s.Changes():
		t.Fatal("unrelated or duplicate hint")
	default:
	}
}

func changeTestCounts(t *testing.T, h *changeHub, subscribers, scopes int) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscriptions) != subscribers || len(h.scopes) != scopes {
		t.Fatalf("retained %d subscribers/%d scopes, want %d/%d", len(h.subscriptions), len(h.scopes), subscribers, scopes)
	}
}

func TestRuntimeStorageScaleChangeHubExactScopesAndCoalescing(t *testing.T) {
	t.Parallel()
	h := newChangeHub()
	defer h.Close()
	a, b := changeTestScope("a"), changeTestScope("b")
	sameKey := sessions.RevisionScope{Index: sessions.CoordinationIndexMailRecipient, Key: a.Key}
	input := []sessions.RevisionScope{a, a}
	onlyA := h.Subscribe(context.Background(), input)
	input[0] = b // subscriptions own dependency sets
	onlyB := h.Subscribe(context.Background(), []sessions.RevisionScope{b})
	both := h.Subscribe(context.Background(), []sessions.RevisionScope{a, b})
	otherIndex := h.Subscribe(context.Background(), []sessions.RevisionScope{sameKey})
	prefix := h.Subscribe(context.Background(), []sessions.RevisionScope{changeTestScope("a/child")})
	empty := h.Subscribe(context.Background(), nil)
	finished := make(chan struct{})
	go func() {
		for range 10000 {
			h.Publish([]sessions.RevisionScope{a, a, changeTestScope("unregistered")})
		}
		close(finished)
	}()
	changeTestReceive(t, finished)
	for _, sub := range []*changeSubscription{onlyA, both} {
		if cap(sub.changes) != 1 || len(sub.changes) != 1 {
			t.Fatalf("slow reader queue has len/cap %d/%d", len(sub.changes), cap(sub.changes))
		}
		changeTestReceive(t, sub.Changes())
		changeTestQuiet(t, sub)
	}
	for _, sub := range []*changeSubscription{onlyB, otherIndex, prefix, empty} {
		changeTestQuiet(t, sub)
	}
	h.Publish([]sessions.RevisionScope{a, b})
	for _, sub := range []*changeSubscription{onlyA, onlyB, both} {
		changeTestReceive(t, sub.Changes())
		changeTestQuiet(t, sub)
	}
	changeTestQuiet(t, otherIndex)
	changeTestQuiet(t, prefix)
	changeTestCounts(t, h, 6, 4)
}

func TestRuntimeStorageScaleChangeHubSubscribeReadWaitBarriers(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"before-subscribe", "before-read", "after-read", "at-wait"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			h := newChangeHub()
			t.Cleanup(h.Close)
			source := newChangeTestSource()
			scope := changeTestScope("recipient")
			start, subscribed, read, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			allowRead, allowWait := make(chan struct{}), make(chan struct{})
			result := make(chan bool, 1)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go func() {
				select {
				case <-start:
				case <-ctx.Done():
					return
				}
				sub := h.Subscribe(ctx, []sessions.RevisionScope{scope})
				defer sub.Close()
				close(subscribed)
				select {
				case <-allowRead:
				case <-ctx.Done():
					return
				}
				// Clear only BEFORE the consistent read, not between read/wait.
				select {
				case <-sub.Changes():
				default:
				}
				ready, _ := source.read(scope)
				close(read)
				select {
				case <-allowWait:
				case <-ctx.Done():
					return
				}
				close(waiting)
				if !ready {
					select {
					case <-sub.Changes():
						ready, _ = source.read(scope)
					case <-sub.Done():
					case <-ctx.Done():
					}
				}
				result <- ready
			}()
			commit := func() {
				source.set(scope, "committed", true)
				h.Publish([]sessions.RevisionScope{scope})
			}
			if phase == "before-subscribe" {
				commit()
			}
			close(start)
			changeTestReceive(t, subscribed)
			if phase == "before-read" {
				commit()
			}
			close(allowRead)
			changeTestReceive(t, read)
			if phase == "after-read" {
				commit()
			}
			close(allowWait)
			changeTestReceive(t, waiting)
			if phase == "at-wait" {
				commit()
			}
			if !changeTestReceive(t, result) {
				t.Fatal("committed readiness was lost")
			}
		})
	}
}

func TestRuntimeStorageScaleChangeHubDependencyReplacement(t *testing.T) {
	t.Parallel()
	h := newChangeHub()
	defer h.Close()
	source := newChangeTestSource()
	a, b := changeTestScope("old"), changeTestScope("discovered")
	s := h.Subscribe(context.Background(), []sessions.RevisionScope{a})
	if s.Replace([]sessions.RevisionScope{a, a}) {
		t.Fatal("unchanged dependencies requested a reread")
	}
	ready, _ := source.read(b) // discover dependency in the consistent read
	if ready {
		t.Fatal("fixture unexpectedly ready")
	}
	source.set(b, "changed-before-registration", true)
	h.Publish([]sessions.RevisionScope{b}) // no subscriber yet
	changeTestQuiet(t, s)
	if !s.Replace([]sessions.RevisionScope{b}) {
		t.Fatal("new dependency did not require registration-race reread")
	}
	if ready, _ := source.read(b); !ready {
		t.Fatal("reread lost pre-registration change")
	}
	h.Publish([]sessions.RevisionScope{a})
	changeTestQuiet(t, s)
	h.Publish([]sessions.RevisionScope{b})
	changeTestReceive(t, s.Changes())
	changeTestCounts(t, h, 1, 1)

	// Publish and replacement contend on the same lock. Whichever linearizes
	// first, the added flag plus reread covers the new dependency's commit.
	c := changeTestScope("concurrent")
	source.set(c, "during-replacement", true)
	h.mu.Lock()
	entered := make(chan struct{}, 2)
	replaced, published := make(chan bool, 1), make(chan struct{})
	go func() { entered <- struct{}{}; replaced <- s.Replace([]sessions.RevisionScope{c}) }()
	go func() { entered <- struct{}{}; h.Publish([]sessions.RevisionScope{c}); close(published) }()
	changeTestReceive(t, entered)
	changeTestReceive(t, entered)
	h.mu.Unlock()
	if !changeTestReceive(t, replaced) {
		t.Fatal("concurrent new dependency did not require reread")
	}
	changeTestReceive(t, published)
	if ready, _ := source.read(c); !ready {
		t.Fatal("replacement reread lost commit")
	}
	if s.Replace(nil) {
		t.Fatal("removal alone should not require reread")
	}
	changeTestCounts(t, h, 1, 0)
}

func TestRuntimeStorageScaleChangeHubCloseCancelAndChurn(t *testing.T) {
	t.Parallel()
	h := newChangeHub()
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := h.Subscribe(ctx, []sessions.RevisionScope{changeTestScope("cancel")})
	cancel()
	changeTestReceive(t, cancelled.Done())
	changeTestCounts(t, h, 0, 0)
	alreadyCancelled := h.Subscribe(ctx, []sessions.RevisionScope{changeTestScope("never")})
	changeTestReceive(t, alreadyCancelled.Done())
	if alreadyCancelled.Replace([]sessions.RevisionScope{changeTestScope("ignored")}) {
		t.Fatal("closed subscription installed dependencies")
	}
	var wg sync.WaitGroup
	for i := range 64 {
		ctx, cancel := context.WithCancel(context.Background())
		scope := changeTestScope(fmt.Sprint(i))
		s := h.Subscribe(ctx, []sessions.RevisionScope{scope})
		wg.Add(3)
		go func() { defer wg.Done(); h.Publish([]sessions.RevisionScope{scope}); s.Close(); s.Close() }()
		go func() { defer wg.Done(); cancel(); s.Replace([]sessions.RevisionScope{changeTestScope("shared")}) }()
		go func() { defer wg.Done(); h.Publish([]sessions.RevisionScope{scope}); <-s.Done() }()
	}
	wg.Wait()
	changeTestCounts(t, h, 0, 0)
	s := h.Subscribe(context.Background(), nil)
	for i := range 2000 {
		s.Replace([]sessions.RevisionScope{changeTestScope(fmt.Sprint(i))})
		changeTestCounts(t, h, 1, 1)
	}
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				h.Publish([]sessions.RevisionScope{changeTestScope("1999")})
			}
		}()
	}
	close(start)
	h.Close()
	h.Close()
	wg.Wait()
	changeTestReceive(t, s.Done())
	changeTestCounts(t, h, 0, 0)
	select {
	case _, open := <-s.Changes():
		if !open {
			t.Fatal("Close closed the producer's hint channel")
		}
	default:
	}
	afterClose := h.Subscribe(context.Background(), []sessions.RevisionScope{changeTestScope("after-close")})
	changeTestReceive(t, afterClose.Done())
	h.Publish([]sessions.RevisionScope{changeTestScope("after-close")})
	changeTestCounts(t, h, 0, 0)
}
