package tools

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// CoordinationTool identifies trusted orchestration that may wait for other
// executions. It must acquire any filesystem exclusivity inside its runtime
// operation, rather than holding a shared permit while waiting for that work.
// A timeout exemption (UntimedTool) does not imply this exemption.
type CoordinationTool interface {
	Tool
	Coordinates() bool
}

const executionGateCapacity int64 = 1 << 62

// ExecutionGate serializes a runtime's integration writes against parent tool
// executions. Ownership belongs to the runtime, not to an individual turn.
type ExecutionGate struct{ semaphore *semaphore.Weighted }

func NewExecutionGate() *ExecutionGate {
	return &ExecutionGate{semaphore: semaphore.NewWeighted(executionGateCapacity)}
}

func (g *ExecutionGate) acquire(ctx context.Context, weight int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := g.semaphore.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	return func() { g.semaphore.Release(weight) }, nil
}

func (g *ExecutionGate) Shared(ctx context.Context) (func(), error) {
	return g.acquire(ctx, 1)
}

func (g *ExecutionGate) Exclusive(ctx context.Context) (func(), error) {
	return g.acquire(ctx, executionGateCapacity)
}

// SetExecutionGate attaches a runtime's gate to its parent registry. Bound
// execution contexts have independent registries and do not inherit this gate.
func (r *ToolRegistry) SetExecutionGate(gate *ExecutionGate) {
	r.mu.Lock()
	r.executionGate = gate
	r.mu.Unlock()
}

// GuardExecution must surround the actual tool invocation with a deferred
// release. Callers invoking tools outside llm.Agent can use the same boundary.
func (r *ToolRegistry) GuardExecution(ctx context.Context, tool Tool) (func(), error) {
	if coordinator, ok := tool.(CoordinationTool); ok && coordinator.Coordinates() {
		return func() {}, ctx.Err()
	}
	r.mu.RLock()
	gate, parent := r.executionGate, r.parent
	r.mu.RUnlock()
	if gate == nil && parent != nil {
		return parent.GuardExecution(ctx, tool)
	}
	if gate == nil {
		return func() {}, ctx.Err()
	}
	return gate.Shared(ctx)
}
