# One engine, replaceable tools and stores

**Status: design for implementation.** Applications select implementations through
ordinary Go construction. Polly keeps one agent loop and one tool registry with
shared execution rules. The constructor types below are proposed; the model,
tool, registry, session, and artifact APIs they compose already exist.

## Scope

Replace the model client, tool implementations, session database, and artifact
storage. A different native process sandbox remains a smaller configuration
change. The agent loop and the registry's lookup, authorization, dispatch, and
refresh behavior stay shared.

Agent loops run in the application process. Managed workspaces remain local Git
workspaces. Scheduling, messaging, completion, parking, and recovery keep their
current implementations and session records.

Both [subagent.AgentRunner](../subagent/subagent.go) and
[swarm.executeSlice](../swarm/runtime.go) already construct an agent and call
[llm.Agent.Run](../llm/agent.go). Keep that loop and the existing `llm.NewAgent`
constructor. There is no new execution interface or alternate agent API.

## Reuse the existing boundaries

| Component | Replaceable implementation | Shared mechanism |
| --- | --- | --- |
| Model | `llm.LLM`, including existing optional metadata interfaces | Request projection, streaming, and the agent loop. |
| Tools | `tools.Tool` and `OutputTool` implementations | `ToolRegistry`, approvals, timeout/gate handling, and result processing. |
| Sessions/database | `sessions.SessionStore`, `Session`, `CoordinationSession` | Coordination transitions and recovery rules. |
| Artifacts | `artifacts.Store`, returned by `Session.ArtifactStore()` | References, publication, and ownership rules. |
| Native process sandbox | Existing `WithSandboxFactory` / `sandbox.Sandbox` | Native tool behavior and confinement requirements. |
| Git workspaces | Application-supplied construction of `*worktree.Manager` | Existing local workspace lifecycle. |

The useful distinction is **how a tool performs work** versus **how Polly admits
and records that work**. A file tool may use local files or an in-memory tree;
its approval and result handling still use the same registry and loop. Replacing
Polly's dispatch or policy semantics is outside this design.

Use the existing packages. No `runtimeapi`, host, backend resolver, service
container, or second tool descriptor hierarchy is needed.

## Make the registry independent of native setup

[Tool](../tools/interface.go) already supplies schema, metadata, and execution.
[ToolRegistry.ExecuteTool](../tools/execution.go) already handles the common
per-tool timeout, execution gate, and rich-output dispatch. Keep those as the
shared mechanism instead of adding `tools.Execution` around them.

The registry should own tool registration, lookup, logical allow/deny policy,
staged changes, and derived views. Native construction must be explicit:

- Make generic registry initialization and `Derive` operate only on registry
  state. They must not install native factories, resolve filesystem paths, or
  call `sandbox.PrepareConfig`. Derivation still narrows the parent's tool policy.
- Move implicit native factory installation from `newRegistry` into native
  setup. Native setup registers the existing tools and sandbox settings. Migrate
  callers that depend on implicit factories to that explicit setup; preserve
  named native convenience wrappers where compatibility requires them.
- Keep native `ExecutionPolicy`, `BindExecutionContext`, loader, and discovery
  behavior behind the native tool constructor. Generic consumers do not invoke
  them to prepare an independently supplied toolset.
- Keep the existing native MCP reload and orphan-client cleanup behavior. A
  binding's cleanup also accounts for resources beyond the registry's current
  MCP client tracking.

This separation is necessary work: merely retaining the current registry under
a new name would leave native effects hidden in construction and derivation.
The shared collection can still be the concrete `*tools.ToolRegistry`; callers
need to replace its tools, not implement a different registry.

## Construct bound tools with one function

Ordinary callers can supply an already-populated registry. Child and workflow
callers need tools bound to the current workspace and authority. Add these
constructor types in `tools`:

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

This is constructor input and output, not a runtime service interface. The
application's function selects concrete tool implementations and returns a
registry ready for ordinary registration and dispatch.

`ExecutionGrant` and `ExecutionGate` are existing types. The grant carries
read-only state, denied reads/writes, and scratch access. `ReadPaths` carries
additional read access such as the owning Git directory; deny rules still win.
Root and scratch are local workspace paths. Supply `Gate` for executions that
currently share the parent's integration gate; isolated member workspaces keep
their existing independent execution behavior.

The coordinator computes the scope from existing state. The native constructor
uses the current `ExecutionPolicy` and `BindExecutionContext` behavior, creating
tools for the destination registry and workspace. Sandbox preparation, path
canonicalization, credential filtering, shell/MCP loading, and skill discovery
remain native implementation details. Never reuse source-bound tool closures
with authority over another workspace.

Another constructor supplies its own `Tool` implementations directly. It bypasses
native rebinding and either enforces the supplied restrictions or returns an
error. It cannot fall back to native tools. Each successful binding owns a fresh
registry; concurrent bindings must not share mutable workspace authority.

`AllowedTools` preserves nil/inherit, empty/disable, selector patterns, and
built-in exceptions. Apply those bounds to the registry and to later staged skill
tools. Perform final nothing-matches validation after private, member, and
structured-result tools are registered. A selection naming only one of those
tools remains valid. Preserve omitted-tool diagnostics through `Omitted`.
An explicit empty selection and `AgentConfig.DisableTools` suppress private tools
as well; intentionally disabled tools use an empty registry.

`Instructions` contains general/repository guidance; `ToolInstructions` contains
skill/tool guidance. Keep their current insertion and persistence rules:
repository guidance is available with tools disabled, while tool guidance obeys
the existing disabled-tools and structured-output rules. Native construction
renders its catalog and loads instructions; another implementation supplies
plain text. Constructor-based paths clear inherited `CompletionRequest.Skills`
to avoid inserting tool guidance twice. Legacy callers may still supply it.

### Resource ownership

`OpenTools` returns a nonnil registry and a nonnil, idempotent `Close` function on
success. That function closes the owned registry and all binding-owned resources,
including any custom client shared by several tools. A failure releases partial
resources before returning an error. Shared application clients and borrowed
registry templates remain application-owned.

The caller owns the binding. `NewAgent` retains its existing ownership model: it
creates a private derived registry, and `Agent.Close` releases only that view.
Close the agent before the binding. Do not assume `ToolRegistry.Close` alone can
close an arbitrary tool backend; it currently tracks MCP-specific resources.
Workflow owners similarly close their cached bindings after active calls finish.

Closing tools does not delete a workspace, finish a task, close the session
store, or erase durable identity. Parking closes the current binding; resumption
constructs fresh tools through the same application-supplied function.

## Keep tool handling shared

The loop continues owning argument parsing, batch exclusivity, approval,
journaling, parallelism, result ordering, artifact persistence, callbacks, and
checkpoints. Invocation goes through the existing `ToolRegistry.ExecuteTool`.
Keep `ToolExecution` fields and rich output on error, including whether invocation
began and whether its context was interrupted.

Resolve the tool handle before approval and invoke that handle with the approved
arguments. The registry must reject a foreign or replaced handle and recheck
current allowance after waiting for its gate. It must not substitute another
tool under the same name. These are local checks in the shared mechanism, not a
prepared-call lifecycle or durable catalog protocol.

Keep `ExclusiveTool`, `UntimedTool`, `RecallTool`, and `CoordinationTool` behavior.
Preserve whole-batch `CommitPendingChanges`, including commits on both source and
derived registries for inherited skill activation. No tool implementation owns
a second model loop or bypasses the existing journal/checkpoint sequence.

Move `view_image` construction entirely out of generic `llm.NewAgent`. Native
setup supplies the current implementation and its existing availability rules,
including safe file and HTTP access. An independent constructor can supply its
own `view_image`; the agent must not overwrite it with a native helper.

Keep `read_transcript`, `read_artifact`, and `list_artifacts` in the agent's
existing private derived registry. They use the supplied transcript or artifact
store. Keep member, structured-result, and parent tools as ordinary registrations
against the current leased session. Existing registration hooks and
`Agent.ToolRegistry()` remain useful. No new registrar or overlay mechanism is
required; preserve existing tool-name precedence and selection rules.

## Application configuration

Keep the relevant `swarm.Config` fields and add the constructor function:

```go
// Proposed relevant fields; existing lifecycle settings remain on Config.
type Config struct { // swarm.Config
    Store         sessions.SessionStore
    Parent        sessions.Session
    Client        llm.LLM
    Registry      *tools.ToolRegistry // The already-bound parent's tools.
    OpenTools     tools.OpenTools
    OpenWorktrees func(context.Context) (*worktree.Manager, error)

    // Existing Request, Agent, limits, callbacks, paths, Promote, etc.
}
```

The application constructs the store, client, and parent tool binding, supplies
`OpenTools` for child/workflow bindings, and supplies the Git constructor. Backend
settings stay with those constructors. Agent/request defaults still control
model choice, sampling, limits, and response behavior. Missing required objects
or constructors fail before execution rather than selecting a native backend.

The standalone runner accepts the same `OpenTools` function alongside its
existing model/request/agent settings. Its registry-taking convenience entry
point can remain an explicit native wrapper. Both use `llm.NewAgent` and
`Agent.Run`; there is no `NewAgentWithExecution` or `RunSlice`.

The execution part of a caller is unchanged after construction:

```go
func run(
    ctx context.Context,
    client llm.LLM,
    openTools tools.OpenTools,
    scope tools.ToolScope,
    request *llm.CompletionRequest,
    config llm.AgentConfig,
    callbacks *llm.AgentCallbacks,
) (*llm.AgentResponse, error) {
    binding, err := openTools(ctx, scope)
    if err != nil {
        return nil, err
    }
    defer binding.Close()

    // Existing request assembly applies binding guidance and registrations.
    agent := llm.NewAgent(client, binding.Registry, config)
    defer agent.Close()
    return agent.Run(ctx, request, callbacks)
}
```

Request assembly, omitted-tool reporting, and final selector validation stay
with the caller. Native repository/skill reads belong to `OpenTools`, even
when invoked by existing preparation hooks. Those hooks may still register tools
through the concrete registry without requiring it to expose a local sandbox.

Update `cmd/polly` composition to use explicit native setup and keep current CLI
flags and behavior. Choose SQLite paths, defaults, and the workspace location
there. The completion builder and workflows use the same registry dispatch
mechanism; none may silently rebind a custom toolset through native methods.

## Database and artifact replacement

Reuse [the session interfaces](../sessions/interface.go),
[CoordinationSession](../sessions/coordination.go), and
[artifacts.Store](../artifacts/store.go). A database replacement implements the
existing operations and retains these guarantees:

- Exclusive session ownership and cancellation when its lease is lost.
- Atomic coordination/history updates, existing generation checks, and current
  mail, task, usage, and artifact retention behavior.
- Stable identity across rename, release, and reopen.
- Read-only inspection without acquiring a writer lease or repairing state.
- Artifact access, publication, and collection governed by session ownership.

Managed sessions must implement `CoordinationSession`; CLI/TUI adapters also need
the existing view/title capabilities they consume. Validate capabilities when
acquiring handles, including on recovery, and return clear errors for unsupported
operations. A conversation-only store does not automatically support swarms.

A PostgreSQL implementation can execute `UpdateCoordination`'s Go callback in the
application while holding a database transaction. Callbacks do not travel to the
database and must not be transparently rerun. The implementation must provide the
existing transaction guarantees, including handling an uncertain commit without
causing blind replay of effects. Do not add a general commit/reconciliation
framework in advance of an adapter that demonstrates the need.

Move `DurableStore.Location` inspection and SQLite WAL/SHM path derivation out of
`swarm.New`. Application assembly supplies `PrivatePaths` and the existing
`Promote` callback. Compute exclusions before registry/MCP construction, including
the active database, promotion destination, sidecars, and canonical aliases.
Supply the same exclusions to tools, workspaces, and coordination. Generic
coordination should not infer database layout.

Separate artifact bytes can sit behind the existing `artifacts.Store`. The
session adapter coordinates references, authorization, retention, and byte
publication; returning a raw bucket client is insufficient. Byte storage must be
ready before a durable reference is published, and failed commits may leave only
collectable unreferenced bytes. No new public blob interface is required now.

Changing the configured store is not a data migration. Reopening existing data
requires a compatible implementation or a separately planned migration. Preserve
format-2 records as they are; add no backend identities or format-1 migration.

## Keep lifecycle and recovery local

The swarm continues to own leases, concurrency limits, execution generations,
mail admission, parking, completion, snapshots, integration, release, and
recovery. Supply `OpenWorktrees` from application assembly and retain the current
lazy, single-instance initialization. The coordinator invokes that function
instead of choosing an implementation or calling `worktree.New` itself. The
application may return an already-created manager. A missing function disables
Git-dependent operations; recovery of records requiring one fails clearly.

Use one manager and one configuration for parent integration and member
workspaces. Its root, directory, capacity, and private paths must agree with the
coordinator's scratch and deny-list calculations; derive them together and reject
mismatches. Preserve the existing non-Git read-only mode only for
`worktree.ErrNotRepository`; propagate broken-repository and sandbox errors.

Keep the existing workspace layout, ownership checks, integration gate, exact
revision rules, uncertain-write handling, and bounded completion after a write
begins. The native Git manager may use its explicitly supplied native registry
for administrative commands. Replacing agent tools does not replace Git's
administrative implementation.

Managed editing tools must operate on the assigned local workspace, or synchronize
their effects there before returning. Otherwise Git capture and integration would
miss the edits. Reject tool/workspace combinations that cannot meet this contract;
a purely in-memory tree is suitable for standalone or non-editing scenarios.

Use the same application-configured `OpenTools` function for initial runs,
follow-ups, parked resumptions, and recovery. Each receives the workspace and
authority reconstructed by the existing coordinator. The application supplies
compatible configuration after restart. There is no persisted `BackendRef`,
selection resolver, per-agent backend migration, or promise to recreate an old
configuration from a saved identifier.

Standalone and managed children remain different ownership arrangements around
`Agent.Run`. Standalone calls return a result; managed calls retain sessions and
coordination state across runs. They do not become separate execution engines.

One `OpenTools` implementation may construct several tools sharing one backend
client and cleanup owner. The loop still sees ordinary tools. Changing only the
sandbox wrapper affects native process confinement; it does not relocate file,
image, or skill reads. Replace those tool implementations when needed.

## Implementation order and proof

Migrate one complete path before broadening the change:

1. **Direct agent and standalone child.** Separate neutral registry initialization
   and derivation from native setup. Add `OpenTools`, move `view_image` into native
   construction, and use the existing `NewAgent`/`Run` path. Preserve partial child
   output and usage on error. Prove the path with a scripted model and independently
   implemented in-memory tools registered in the shared `ToolRegistry`.
2. **Managed child and recovery.** Supply scoped tools through `OpenTools` instead
   of calling native rebinding in the coordinator. Keep member/structured tool
   registration and existing callbacks. Acquire the current lease before binding
   session tools. Test initial execution, follow-up, parking, recovery, and
   workspace release with both native and independent toolsets. For managed
   editing, verify custom tool effects appear in actual snapshots and integration.
3. **Parent and workflows.** Keep the parent's registry injection and integration
   gate. Route workflow construction through `OpenTools`, retaining policy-based
   rebinding and binding ownership. Preserve registration hooks; move their native
   discovery/path work to native construction. Migrate the completion builder and
   library callers without adding another loop.
4. **Storage and assembly.** Remove SQLite discovery from coordination, inject the
   Git constructor, and validate session capabilities. Run storage contract tests
   against SQLite and an independent reopenable map store with separate byte
   storage. Update CLI/TUI assembly and examples to show actual constructor choices.

Test the shared registry/loop contract with both toolsets: selection, disabled
tools, approved arguments, changed handles/policy, exclusive batches, rich output,
cancellation, partial results, staged refresh, and cleanup. Retain the initial
persistence veto and journal/checkpoint ordering.

The independent toolset should exercise file and image calls through the same
registry and loop while native construction, filesystem, process, and network
access would fail. Include construction, `Derive`, and private helper setup in
that check. It must implement its effects independently, not wrap native file
tools. An independent registry implementation is neither needed nor intended.
Managed tests may still use the explicit local Git manager and temporary repo.
Their editing tools must obey the shared local workspace contract; the standalone
in-memory proof alone does not establish managed editing support.

Test failed binding, close ordering, repeated cleanup, borrowed template lifetime,
and multiple tools sharing one client. Preserve native reload/orphan cleanup as
well as end-of-binding cleanup.

For storage, cover reopen, lease loss, competing updates, atomic history and
coordination, read-only views, and artifact ownership. Wrapping SQLite is not an
independent storage implementation. Production database adapters additionally
test their actual transaction and connection-failure behavior. A successful
checkpoint followed by a failed coordination read must not cause fallback
persistence to append the committed suffix again; advance the persisted cursor
from the successful transaction.

After production migration, run build, vet, full tests, native sandbox tests,
race checks, and the repository cross-platform compilation matrix. Document any
consumer that still requires native tools instead of claiming it supports
replacement. Success is changing constructors without editing the loop,
registry dispatch semantics, or coordinator transitions.

## Deferred

Replacing the agent loop, registry policy/dispatch, scheduler, or Git workspace
implementation is outside this delivery. So are remote agent placement, cloud
workers, agent migration, durable external jobs/approvals, transport protocols,
and public filesystem/process/network services. Add no placeholder APIs or
durable fields for them. Introduce another interface only when a concrete
implementation needs behavior that the existing model, tool, or store contracts
cannot express.
