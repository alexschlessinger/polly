package swarm

import "context"

type workflowCallIDKey struct{}

// WithWorkflowCallID attaches display provenance for a workflow launched by a
// host tool call. Typed and library launches may omit it. It grants no authority.
func WithWorkflowCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, workflowCallIDKey{}, id)
}
