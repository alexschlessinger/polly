package tools

import (
	"context"
	"strings"
)

// NativeOption configures NativeOpenTools.
type NativeOption func(*nativeOptions)

type nativeOptions struct {
	instructions func(*ToolRegistry) string
}

// WithNativeInstructions supplies the repository guidance loader a native
// binding renders into ToolBinding.Instructions, given the bound registry so
// it reads under the binding's own policy.
func WithNativeInstructions(load func(*ToolRegistry) string) NativeOption {
	return func(o *nativeOptions) {
		o.instructions = load
	}
}

// NativeOpenTools returns the OpenTools that rebinds source's native tools,
// shell tools, MCP servers, and skills to each scope: the sandbox policy is
// narrowed to the scope's root and grant, every tool is constructed afresh
// against that policy, and the skill catalog is rendered into
// ToolInstructions. The source must have been constructed with
// WithNativeTools. The caller validates its selection once its own tools
// are registered (see ToolRegistry.ValidateToolSelection); the binding only
// reports what it omitted.
func NativeOpenTools(source *ToolRegistry, opts ...NativeOption) OpenTools {
	var o nativeOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx context.Context, scope ToolScope) (ToolBinding, error) {
		if source == nil || !source.native {
			return ToolBinding{}, ErrNativeToolsRequired
		}
		if err := ctx.Err(); err != nil {
			return ToolBinding{}, err
		}
		grant := scope.Grant
		if scope.SourceRoot != "" {
			grant.SourceRoot = scope.SourceRoot
		}
		ec, err := source.ExecutionPolicy(scope.Root, grant)
		if err != nil {
			return ToolBinding{}, err
		}
		ec.Sandbox.ReadPaths = append(ec.Sandbox.ReadPaths, scope.ReadPaths...)
		bound, omitted, err := source.bindExecutionContext(ec, scope.AllowedTools)
		if err != nil {
			return ToolBinding{}, err
		}
		if scope.Gate != nil {
			bound.SetExecutionGate(scope.Gate)
		}
		binding := ToolBinding{Registry: bound, Omitted: omitted}
		if catalog := bound.ExecutionSkills(); catalog != nil && !catalog.IsEmpty() {
			binding.ToolInstructions = strings.TrimSpace(catalog.RuntimeSystemPrompt(""))
		}
		if o.instructions != nil {
			binding.Instructions = strings.TrimSpace(o.instructions(bound))
		}
		binding.Close = bound.Close
		return binding, nil
	}
}
