package tools

import (
	"errors"
	"fmt"
)

// ErrNativeToolsRequired reports a registry without native tool setup asked
// to bind an execution context or to build native tools.
var ErrNativeToolsRequired = errors.New("registry has no native tool setup; construct it with WithNativeTools or supply tools through OpenTools")

// WithNativeTools installs polly's built-in tools: constructors for bash and
// the file tools, loaded by name, and view_image, registered at once and
// marked built-in. Without it a registry serves only the tools registered on
// it, which is what an independently supplied toolset needs: derivation and
// execution binding never add native tools of their own.
func WithNativeTools() RegistryOption {
	return func(o *registryOptions) {
		o.native = true
	}
}

// installNativeTools is the one place native tools enter a registry.
func installNativeTools(r *ToolRegistry) {
	r.native = true

	r.nativeTools["bash"] = func(registry *ToolRegistry) (Tool, error) {
		bt := newBashTool(registry.executionRoot)
		bt.siblingLoaded = registry.hasVisibleTool
		if err := registry.requireProcessSandbox("bash"); err != nil {
			return nil, err
		}
		if registry.sandboxFactory == nil {
			return bt, nil
		}
		// Fail closed: bash without its sandbox must not load.
		sb, cfg, err := registry.newSandboxFor("bash", nil)
		if err != nil {
			return nil, fmt.Errorf("sandbox for bash: %w", err)
		}
		return bt.withSandboxConfig(sb, cfg), nil
	}

	// read_file applies the base read policy in-process when sandboxing is
	// active and reads unrestricted otherwise, like view_image. The writing
	// tools fail closed without a sandbox: an in-process write grants the
	// model the caller's ambient host access exactly like an unsandboxed
	// command, so they require WithUnsafeNoSandbox to load without one.
	r.nativeTools["read_file"] = func(registry *ToolRegistry) (Tool, error) {
		return NewReadFileTool(registry), nil
	}
	r.nativeTools["list_dir"] = func(registry *ToolRegistry) (Tool, error) {
		return NewListDirTool(registry), nil
	}
	r.nativeTools["write_file"] = func(registry *ToolRegistry) (Tool, error) {
		if err := registry.requireProcessSandbox("write_file"); err != nil {
			return nil, err
		}
		return NewWriteFileTool(registry), nil
	}
	r.nativeTools["edit_file"] = func(registry *ToolRegistry) (Tool, error) {
		if err := registry.requireProcessSandbox("edit_file"); err != nil {
			return nil, err
		}
		return NewEditFileTool(registry), nil
	}
	r.nativeTools["view_image"] = func(registry *ToolRegistry) (Tool, error) {
		return NewViewImageTool(registry), nil
	}

	// view_image is present on every native registry, whatever a selection
	// names: it reads under the same policy as the file tools and is how the
	// model sees an image a command produced.
	viewer := NewViewImageTool(r)
	r.Register(viewer)
	r.MarkBuiltin(viewer.GetName())
}
