package swarm

import (
	"context"

	"github.com/alexschlessinger/pollytool/tools"
)

// contextPolicy is the native sandbox policy for a context's scope: what
// the parent's registry would bind natively. Workflow bindings and tests
// inspect it; members are opened through Config.OpenTools instead.
func (r *Runtime) contextPolicy(ctx context.Context, s *State, c *ExecutionContext) (tools.ExecutionContext, error) {
	scope, err := r.contextScope(ctx, s, c)
	if err != nil {
		return tools.ExecutionContext{}, err
	}
	ec, err := r.config.Registry.ExecutionPolicy(scope.Root, scope.Grant)
	if err != nil {
		return ec, err
	}
	ec.Sandbox.ReadPaths = append(ec.Sandbox.ReadPaths, scope.ReadPaths...)
	return ec, nil
}
