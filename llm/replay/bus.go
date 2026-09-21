package replay

import (
	"context"
	"sync"
)

// Bus is the in-process signal exchange between a fixture's turns and the
// shot script playing against them. A gate is released by the script and
// awaited by a turn; a mark is reached by a turn and awaited by the script.
// Both latch: a release before the turn arrives, or a mark reached before the
// script asks, is remembered, so neither side depends on the other's timing.
type Bus struct {
	mu       sync.Mutex
	released map[string]chan struct{}
	reached  map[string]chan struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{released: map[string]chan struct{}{}, reached: map[string]chan struct{}{}}
}

// signal returns the latch for name in m, creating it on first use.
func (b *Bus) signal(m map[string]chan struct{}, name string) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := m[name]
	if !ok {
		ch = make(chan struct{})
		m[name] = ch
	}
	return ch
}

// fire latches name in m; firing twice is harmless.
func (b *Bus) fire(m map[string]chan struct{}, name string) {
	ch := b.signal(m, name)
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func wait(ctx context.Context, ch chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Release lets every turn past gate.
func (b *Bus) Release(gate string) { b.fire(b.released, gate) }

// AwaitRelease blocks until gate has been released.
func (b *Bus) AwaitRelease(ctx context.Context, gate string) error {
	return wait(ctx, b.signal(b.released, gate))
}

// Reach reports that the turns have reached mark.
func (b *Bus) Reach(mark string) { b.fire(b.reached, mark) }

// AwaitMark blocks until mark has been reached.
func (b *Bus) AwaitMark(ctx context.Context, mark string) error {
	return wait(ctx, b.signal(b.reached, mark))
}
