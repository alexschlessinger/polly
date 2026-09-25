# Replaceable tools and stores

Tool construction is available for direct agents, standalone children, managed
members, and workflows. Storage/workspace assembly changes below remain proposed.
Applications choose implementations through Go construction while keeping Polly's
agent loop, registry, and coordination rules. See the [library reference](API.md).

## Contents

- [Scope](#scope)
- [Tool construction](#tool-construction)
- [Shared execution rules](#shared-execution-rules)
- [Application assembly](#application-assembly)
- [Sessions and artifacts](#sessions-and-artifacts)
- [Workspaces and recovery](#workspaces-and-recovery)
- [Implementation and verification](#implementation-and-verification)

[Documentation index](README.md)

## Scope

| Replaceable part | Existing boundary to keep |
|---|---|
| Model client | `llm.LLM` and optional metadata interfaces |
| Tools | `tools.Tool`, `OutputTool`, and the concrete `ToolRegistry` |
| Session database | `SessionStore`, `Session`, `CoordinationSession` |
| Artifact bytes | `artifacts.Store`, scoped through `Session.ArtifactStore()` |
| Native sandbox | `WithSandboxFactory` and `sandbox.Sandbox` |
| Git workspace construction | An application-supplied `*worktree.Manager` |

Keep one `llm.NewAgent` / `Agent.Run` path for direct, standalone-child, and
managed execution. The application process continues to own scheduling, messaging,
leases, integration, and recovery. Managed workspaces remain local Git workspaces.

Replacing the loop, registry dispatch/policy, scheduler, or Git implementation is
outside this proposal. So are remote workers, transport protocols, backend IDs in
saved records, and general filesystem/process/network service APIs.

## Tool construction

The `tools` package exposes:

```go
type OpenTools func(context.Context, ToolScope) (ToolBinding, error)

type ToolScope struct {
    Root         string
    SourceRoot   string
    Grant        ExecutionGrant
    ReadPaths    []string
    AllowedTools []string
    Gate         *ExecutionGate
}

type ToolBinding struct {
    Registry         *ToolRegistry
    Instructions     string
    ToolInstructions string
    Omitted          []string
    Close            func() error
}
```

The coordinator calculates scope from current authority. The application's
function returns ordinary tools ready for the shared registry. Native construction
uses `ExecutionPolicy` and `BindExecutionContext`; another implementation enforces
the same supplied restrictions or fails. Each binding owns a fresh registry.

### Separate native setup

Generic registry construction and `Derive` install no native factories or prepare
filesystem policy. `WithNativeTools` installs native tools, including
`view_image`; sandbox options and `NativeOpenTools` prepare and bind authority.
Shell/MCP loading and skill reads remain explicit native operations. An independent toolset must not be silently rebound to
native tools or have its image implementation replaced by the agent.

Keep agent-owned `read_transcript`, `read_artifact`, and `list_artifacts` in the
private registry; `AgentConfig.Builtins` chooses which of them an application
installs. Session/member registration hooks and `Agent.ToolRegistry()` remain
available. No second registry interface is needed.

### Authority and guidance

`ExecutionGrant` carries read-only state, denied roots, and scratch access.
`ReadPaths` supplies approved Git/configuration reads. Parent integration uses its
shared gate; isolated member workspaces keep independent execution behavior.
Never reuse source-bound closures with authority over another workspace.

Preserve nil/inherit and empty/disable tool selections, selector patterns, and
built-in exceptions. Validate selections after member, private, and result tools
are registered. Disabled tools suppress private helpers too. Later skill loads
remain inside the same bounds.

`Instructions` holds general/repository guidance; `ToolInstructions` holds tool
and skill guidance. Repository guidance remains available with tools disabled.
Tool guidance follows disabled-tools and structured-output rules. Clear inherited
`CompletionRequest.Skills` when the constructor has already composed its guidance.

### Ownership

A successful binding returns a nonnil registry and idempotent `Close`. Failure
releases partial resources. The binding owns all its resources, including clients
shared by several tools; application clients and borrowed templates stay with
their owner.

Close the agent before the binding. `Agent.Close` releases only its private view;
`ToolRegistry.Close` alone cannot account for arbitrary backend resources.
Workflow owners close bindings after active calls finish. Cached bindings reopen
when scope or the parent registry's committed sandbox-policy revision changes. Parking closes the
binding; resumption constructs another. Closing tools does not settle tasks,
delete workspaces, or close the session store.

## Shared execution rules

Keep argument parsing, approvals, batch exclusivity, journaling, parallelism,
result order, artifacts, callbacks, and checkpoints in the existing loop.
Dispatch through `ToolRegistry.ExecuteTool` and preserve its rich output on error,
including whether invocation began and whether its context was interrupted.

Resolve a tool before approval, then invoke that handle with the approved
arguments. Recheck allowance after waiting for the gate. Reject foreign/replaced
handles rather than substituting another tool under the same name.

Preserve `ExclusiveTool`, `UntimedTool`, `RecallTool`, `CoordinationTool`, and
whole-batch `CommitPendingChanges`, including inherited skill activation.
Tool implementations do not get a separate loop or checkpoint sequence.

## Application assembly

`swarm.Config` takes `OpenTools`. The additional `OpenWorktrees` field below
remains proposed:

```go
// OpenWorktrees remains proposed; other fields are available.
type Config struct {
    Store         sessions.SessionStore
    Parent        sessions.Session
    Client        llm.LLM
    Registry      *tools.ToolRegistry
    OpenTools     tools.OpenTools
    OpenWorktrees func(context.Context) (*worktree.Manager, error)
}
```

The host constructs the model, store, and parent binding, then supplies
constructors for member/workflow tools and Git workspaces. Backend settings stay
with those constructors. Missing required dependencies fail before execution.

The standalone runner accepts the same `OpenTools` function. Both standalone and
managed children still create an agent and call `Run`. Preparation hooks may
register tools; native discovery belongs in native construction.

CLI/TUI assembly supplies `NativeOpenTools` and the repository-instruction
loader. Native Git administration and repository reads remain native consumers;
custom model tools do not replace those operations. Storage and workspace
constructor injection remains future work.

## Sessions and artifacts

Use the existing [session interfaces](../sessions/interface.go),
[coordination interface](../sessions/coordination.go), and
[artifact store](../artifacts/store.go). A replacement must preserve:

- Exclusive ownership and cancellation on lease loss.
- Atomic transcript/coordination updates and generation checks.
- Stable identity across rename, release, and reopen.
- Read-only inspection without acquiring a write lease or repairing state.
- Session-scoped artifact access, publication, and collection.

Managed sessions need `CoordinationSession`; CLI/TUI adapters also need the view
and title capabilities they consume. Check these at acquisition and recovery.
Conversation storage alone does not establish swarm support.

A database adapter may run the `UpdateCoordination` callback inside its transaction.
It must not transparently retry the callback or replay effects after an uncertain
commit. Test the actual database's connection and commit-failure behavior.

Move SQLite location/WAL/SHM discovery out of `swarm.New`. Application assembly
supplies `PrivatePaths` and `Promote`, covering the active database, promotion
destination, sidecars, and canonical aliases before registry/MCP construction.
Tools, workspaces, and coordination use the same exclusions.

Separate artifact bytes can use `artifacts.Store`, but the session adapter still
owns reference authorization, retention, and publication. Bytes must be ready
before publishing a durable reference; failed commits may leave only collectable,
unreferenced bytes. Reopening saved data requires a compatible store.

## Workspaces and recovery

Proposed workspace injection keeps lazy, single-instance construction through
`OpenWorktrees`.
A missing constructor disables Git-dependent work and clearly refuses recovery
that needs it. Parent integration and members must agree on root, capacity,
scratch, and private paths. Only `worktree.ErrNotRepository` permits non-Git
read-only fallback; propagate broken-repository and sandbox failures.

The native Git manager may use its own explicitly supplied native registry for
administration. Custom editing tools must affect the assigned local workspace,
or synchronize there before returning, so capture/integration sees their edits.
Reject incompatible combinations. In-memory tools can still serve standalone or
non-editing work.

Use the same configured `OpenTools` for first runs, follow-ups, parked resumptions,
and recovery. Reconstruct authority from coordinator state and require compatible
application configuration after restart. Preserve exact revisions, workspace
ownership, the integration gate, uncertain-write handling, and bounded outcome
recording.

Changing only the sandbox wrapper confines native processes; it does not replace
file, image, or skill reads. Those need their own implementations when relocated.

## Implementation and verification

Deliver one complete path at a time:

| Stage | Change | Proof |
|---|---|---|
| Direct agent and standalone child | Separate generic/native setup; add bindings; move `view_image` | Scripted model and independent in-memory tools run through the shared loop with native effects denied |
| Managed child and recovery | Construct scoped tools under the current lease | Native and independent tools survive follow-up, parking, recovery, and release; edits appear in real snapshots |
| Parent and workflows | Use bindings with the existing integration gate | Preserve hooks, reservations, teardown, and partial receipts |
| Storage and assembly | Inject storage/Git choices; validate capabilities | SQLite and an independent reopenable map store pass ownership and persistence contracts |

Shared contract tests cover selection, disabled tools, approved arguments,
changed handles/policy, exclusive batches, rich output, cancellation, partial
results, staged refresh, and cleanup. Include generic construction, derivation,
and private helpers in the no-native-effects check.

Exercise failed bindings, close order, repeated cleanup, borrowed templates,
shared clients, and native reload cleanup. Storage checks cover leases, competing
updates, atomic persistence, read-only views, and artifact ownership. A successful
checkpoint followed by a failed read must not append that checkpoint again.

Finish with build, vet, full tests, native sandbox tests, race checks, and the
cross-platform matrix. Document consumers that still require native tools.
