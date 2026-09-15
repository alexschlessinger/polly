package tools

import "context"

// OpenTools constructs the tools an agent loop runs with, bound to one
// workspace and one authority. An application supplies the function; the
// coordinator, the standalone runner, and workflow hosts call it for every
// binding they need and never choose an implementation themselves.
// NativeOpenTools builds polly's process-backed tools; another
// implementation supplies Tool values of its own.
type OpenTools func(context.Context, ToolScope) (ToolBinding, error)

// ToolScope is what a binding is opened for: the workspace root the tools
// operate on, the source checkout it was made from (for rebinding
// operator-selected paths), the authority the coordinator grants beyond
// reading the root, extra read access such as the owning Git directory, the
// caller's tool selection, and the execution gate shared with an integrating
// parent. The deepest rule containing a path decides its access.
type ToolScope struct {
	Root string
	// SourceRoot overrides Grant.SourceRoot when nonempty.
	SourceRoot string
	Grant      ExecutionGrant
	ReadPaths  []string
	// AllowedTools narrows the binding: nil inherits every tool, an empty
	// slice disables tools, and patterns select by name or glob. A binding
	// keeps its built-ins whatever the selection names.
	AllowedTools []string
	// Gate, when set, serialises the binding's tool executions with the
	// owner's exclusive operations. Isolated bindings leave it nil.
	Gate *ExecutionGate
}

// ToolBinding is an opened toolset. The caller owns it: agents derive their
// private views from Registry and close them first, then the caller closes
// the binding, which releases every resource the binding opened.
type ToolBinding struct {
	Registry *ToolRegistry
	// Instructions is repository and general guidance for the model;
	// ToolInstructions is guidance about the bound tools and skills. The
	// caller inserts each under its own rules.
	Instructions     string
	ToolInstructions string
	// Omitted names the tools the caller's selection or the binding could
	// not honor, for diagnostics.
	Omitted []string
	// Close releases the binding. It is never nil on success and is safe to
	// call more than once.
	Close func() error
}
