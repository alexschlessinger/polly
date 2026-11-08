# Replaceable tools and stores

Tool construction is available for direct agents, standalone children, managed
members, and workflows. Hosts also supply Git workspace construction, storage
exclusions, and promotion. Shared storage contracts run against SQLite and an
independent reopenable file store used by tests.
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

Native MCP construction carries the binding context through connection,
initialization and tool discovery. Context-aware loading, restart, discovery and
binding APIs are additive; existing convenience methods retain their
background-context behavior. Failed or canceled owned stdio construction
terminates and reaps its child. Final installation rechecks source closure,
affected tool selection and policy after construction/instruction callbacks;
stale candidates cannot resurrect removed tools or publish stale authority.

Independent registry names can construct concurrently. Retired owned MCP clients
close outside registry-map and policy locks, but `Close` still joins previously
detached cleanup before returning. A slow successful SDK shutdown or synchronous
custom/native/shell constructor can block its own caller; this does not promise
that arbitrary host callbacks are cancellable. Borrowed clients/templates remain
caller-owned. Close the agent before its binding, and finish binding cleanup
before workspace reuse.

### Separate native setup

Generic registry construction and `Derive` install no native factories or prepare
filesystem policy. `WithNativeTools` installs native tools, including
`view_image`; sandbox options and `NativeOpenTools` prepare and bind authority.
Shell/MCP loading and skill reads remain explicit native operations. An independent toolset must not be silently rebound to
native tools or have its image implementation replaced by the agent.

Keep agent-owned recall helpers and `restore_artifact` in the private registry;
`AgentConfig.Builtins` chooses which of them an application installs.
`restore_artifact` obeys tool/skill selections rather than recall's always-allowed
exemption. Session/member registration hooks and `Agent.ToolRegistry()` remain
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

`swarm.Config` takes explicit tool and workspace constructors:

```go
type Config struct {
    Store         sessions.SessionStore
    Parent        sessions.Session
    Client        llm.LLM
    Registry      *tools.ToolRegistry
    OpenTools     tools.OpenTools
    OpenWorktrees func(context.Context, worktree.Config) (*worktree.Manager, error)
    PrivatePaths  []string
    Promote       func(context.Context) error
}
```

The host constructs the model, store, and parent binding, then supplies
constructors for member/workflow tools and Git workspaces. The workspace
constructor receives the runtime's resolved root, directory, capacity and a copy
of its private paths; it supplies the administrative registry while honoring
those settings. Backend settings stay with the constructors. Missing required
dependencies fail before execution.

The standalone runner accepts the same `OpenTools` function. Both standalone and
managed children still create an agent and call `Run`. Preparation hooks may
register tools; native discovery belongs in native construction.

CLI/TUI assembly supplies `NativeOpenTools` and the repository-instruction
loader. Native Git administration and repository reads remain native consumers;
custom model tools do not replace those operations. Git's administrative registry
is borrowed from the host and must outlive the runtime; it can differ from the
registry of model-facing tools.

## Sessions and artifacts

Use the existing [session interfaces](../sessions/interface.go),
[coordination interface](../sessions/coordination.go), and
[artifact store](../artifacts/store.go). A replacement must preserve:

- Exclusive ownership and cancellation on lease loss.
- Atomic transcript/coordination updates and generation checks.
- Stable identity across rename, release, and reopen.
- Read-only inspection without acquiring a write lease or repairing state.
- Session-scoped artifact access, publication, and collection.

Managed sessions need `CoordinationSession` on both parent and child handles;
the runtime checks each child acquisition, including recovery after a rename,
and closes incompatible handles before returning an error. CLI/TUI adapters also
need the view and title capabilities they consume.
Conversation storage alone does not establish swarm support.

A database adapter may run the `UpdateCoordination` callback inside its transaction.
It must not transparently retry the callback or replay effects after an uncertain
commit. Records, transcript appends, member retention and artifact publication
pins form one atomic commit. An error alone cannot establish whether a remote
commit happened; inspect durable receipts before deciding what to resume.

### Optional record and paging access

`CoordinationRecords` adds callback-local point reads, declared-index queries,
revision reads and atomic record updates without changing required `Session`,
`SessionStore` or `CoordinationSession` methods. Use `OpenCoordinationAccess` on
the supplied handle. The trusted-host `OpenCoordinationViewAccess` similarly
selects root-only, lease-free display access. Neither discovers a database path
or bypasses an application's selected backend.

Readers cannot outlive their callback, be shared concurrently, or make nested
store calls. They return owned payload bytes and see staged record writes.
`CoordinationTx.Effects` joins transcript append/expected sequence/identity,
artifact pins and member retention to the same transaction. Callbacks run at most
once; errors veto the transaction, but a backend error can still represent an
uncertain commit. `CoordinationCommit.Sequence` and `Changed` are authoritative
only on nil error. Revision tokens are equality hints, never lease, execution,
authority or commit-success proof.

The legacy adapter performs one whole-family read or exactly one
`UpdateCoordination` callback, forwarding all effects atomically. It reports
`NativeIndexed:false` and `DurableRevisions:false`; conservative tokens invalidate
between operations rather than pretending unchanged polling is efficient.
Publication/overview indexes identify candidates and dependencies; domain
authorization, coalescing and transitions still belong to swarm. Unknown record
kinds remain round-trippable and unsupported selectors fail explicitly.

SQLite supplies native indexed records and durable revisions. Its legacy
`UpdateCoordination` entry point maintains the same indexes and revision scopes,
so mixing the two APIs in one supported binary does not bypass invalidation.
That is not mixed-binary compatibility: stop all older processes before the
schema-10 migration, as described in [upgrading](UPGRADING.md). Promotion preserves
stable identities, transcript epochs and revision tokens.

`CountOnly` queries return totals without selecting an arbitrary payload. They
require `IncludeTotal` and zero paging fields. Optional `IncludeLast` adds an
owned final key under the same filter/order/snapshot, nil when empty; it requires
`CountOnly`. Both modes avoid payload reads on SQLite, while the legacy adapter
still loads the family. The native last-key lookup is an indexed reverse seek;
this does not imply that every kind-filtered total is constant work or that the
result validates payload integrity. See [record-scoped coordination](API.md#record-scoped-coordination)
for the complete query contract.

Ordinary swarm lifecycle and follow-up operations use record access on both parent
and leased child handles: assignment/transcript identity, execution transitions,
control, delivery and follow-up provenance remain atomic. Renamed-member recovery
uses stable-ID summary lookup with `ExpectedID` fencing. The first-assignment roster
uses maintained lifecycle totals and renders at most 32 working members. Record update
callbacks preserve exact JSON number tokens in opaque saved inputs/results; display
reads keep their existing representation.

Optional callback-local `CoordinationStatusReader` and
`CoordinationSemanticReader` add semantic status pages/totals and exact baseline
comparison. Detect them independently from `NativeIndexed`; use
`SemanticReader(reader)` for a supplied-reader news fallback. Only native history
advertises `SemanticCapabilities().NativeHistory`. The legacy news adapter carries
family-sized baseline values; status falls back to full-state presentation if its
capability is absent or historical workflow ownership is ambiguous. Do not
advertise stub methods or use a read failure to discover another backend.

SQLite eagerly maintains per-item status facts, ordered memberships/totals and
canonical news history in both native and legacy write transactions. Staged reads
see the completed derivation; source writes, history, revisions and transcript/pin
effects commit or roll back together. The complete unreleased schema 10 includes
these tables: schema 9 upgrades atomically with bounded-memory backfill, while an
incomplete intermediate schema 10 is refused, never silently repaired or erased.
Default integrity, WAL/FULL, connection and lease policies are unchanged.

Native marks bind root, rules version and a durable family-creation epoch. History
and tombstones survive reopen/promotion and are retained until family deletion;
this is transition-proportional storage without a history-GC scheduler. Compare
against the original baseline after every false observation. Parent-news mode
suppresses deletion-only and current workflow-control changes and preserves A→B→A
equality; full-coordination mode includes absent/controlled entries. See
[semantic coordination reads](API.md#semantic-coordination-reads) for continuation,
paging, oversized-item and ownership contracts. Tokens are observations, not
commit-success or mutation authority.

Native status, timeout summaries and unchanged parent-news checks avoid unrelated
retained family payloads. Actual dependency/run/global-gate fanout, exact batch
`Next` output and deep offsets are not constant work. Parent wait uses scoped
semantic-news/mail hints plus actual in-memory activity; delivery repair on return
and settlement load current obligations and their authority dependencies. TUI
consumers use optional display projections and scoped invalidation as described below.

Optional `CoordinationCandidateReader` provides callback-local candidate pages;
`QueryCoordinationCandidates` falls back through that same reader. SQLite uses
additive source indexes created by the schema-10 migration.
Candidates are observations, not mutation authority: writes recheck source records
inside the leased transaction. Automatic release selects bounded candidate batches
and retains its two-phase proof/mark/filesystem/receipt protocol.

These are selected-record guarantees, not whole-runtime constant-work guarantees.
Cold recovery still validates retained history once with bounded memory, before
record-scoped recovery groups. Explicit full-state exports remain broad. All saved
sibling context roots remain security dependencies; actual task/apply/follow-up
fanout and selected conversation history can also be large.
Legacy backends remain correct but materialize the family. Native successful
commits publish their exact changed revision scopes. Optional
`CoordinationCommitSession` supplies equivalent receipts for whole-state callbacks;
wrappers must explicitly forward it. Other whole-state writers conservatively
invalidate live subscribers after success. One shared poller per owner covers
remote/uncertain commits with bounded revision batches; hosts lacking durable or
requested revision capabilities conservatively recheck. Hints never acknowledge
delivery or confer mutation authority.
Capacity defaults and startup integrity policy are unchanged.

Summary and transcript access use the supplied backend in the same way. Legacy
summary pages materialize `ListSummaries`; transcript pages materialize `ReadView`
and invalidate their conservative epochs on any view change. They preserve
correctness, not indexed efficiency. Read-only fallbacks never acquire a write
lease to emulate a missing view. See [paged storage capabilities](API.md#paged-storage-capabilities)
for live cursor, oversized-item and ownership rules. There is no success-returning
maintenance fallback: a backend advertises `MaintenanceStore` only when all its
operations are functional.

Native TUI display access is also opt-in. Compact coordination display/usage
readers remain callback-local; `swarm.DisplaySnapshot` carries precomputed semantic
observations rather than masquerading as a partial `State`. Display changes and
tombstones are maintained atomically by both writer APIs. Selected inspector
pages load only the chosen records and their formatting dependencies.

`TranscriptDisplayStore`, together with `NativeDisplay`, selects recent real-user
turns and sparse prefix facts. A `SessionView` with `Display` set contains a partial
display history, never a complete model request or export. `SessionTranscriptReader`
adds optional paging through the supplied session handle for incremental context
statistics. Missing capabilities retain correct broad adapters; wrappers must
explicitly forward capabilities, not probe for another database. Cold reads,
exact search and selected oversized values remain proportional to what they
request.

Application assembly supplies `PrivatePaths` and `Promote`, covering the active
database, promotion destination, sidecars, and canonical aliases before
registry/MCP construction. Swarm does not discover store locations or assume
SQLite sidecar names. The CLI resolves exclusions and promotion once per
conversation; tools, snapshot tracking, workspaces, and coordination use that
same configuration. Library hosts must supply all their storage exclusions.

Separate artifact bytes can use `artifacts.Store`, but the session adapter still
owns reference authorization, retention, and publication. Bytes must be ready
before publishing a durable reference; failed commits may leave only collectable,
unreferenced bytes. Reopening saved data requires a compatible store.

## Workspaces and recovery

`OpenWorktrees` is lazy and serialized; a successful manager is reused for the
runtime's lifetime, and failed construction can be retried. A nil manager without
an error is rejected. A missing constructor returns `swarm.ErrWorktreesUnavailable`
when workspace construction is requested and refuses recovery of saved Git
workspaces before changing their coordination state. Parent integration and
members share root, capacity, scratch, and private paths. Only an explicit
`worktree.ErrNotRepository` from the constructor permits a new non-Git read-only
workspace; missing constructors, broken repositories and sandbox failures do not
silently fall back to a live tree. Existing live-workspace continuations preserve
their source kind.

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
| Storage and workspace assembly | Supply Git construction, private paths and promotion | Independent Git administration, scoped exclusions, lazy reuse, constructor failure and recovery checks |
| Independent storage verification | Validate storage capabilities and ownership | SQLite and an independent JSON file store pass the same contracts and runtime recovery test |

[`TestStorageContract`](../sessions/storage_contract_test.go) runs the shared
[`sessiontest.Run`](../internal/sessiontest/contract.go) suite against both stores:

- Exclusive leases, takeover, cancellation, stale writes and already-open readers.
- Stable session and cache identity across rename and reopen, including rejection
  of a deleted identity after its name is reused.
- Read-only snapshots, revision checks, independent returned values and family
  display access without taking a lease.
- Atomic transcript, coordination, publication and member-retention updates;
  callback vetoes, stale sequences, concurrent family updates and failed commits.
- Publication authorization, persistence after author deletion, private ownership
  acquired by readers, and collection after the final owner and pin disappear.
- Parent and member retention across expiry and reopen.

SQLite commit failures use a deferred foreign-key constraint that fails at
`COMMIT`, after the callback and checkpoint statements have succeeded. The suite
then reopens and inspects the committed receipt and transcript rather than
retrying the callback.
[`TestStorageReopenRecoversExecutionWithoutReplayingEffects`](../swarm/storage_contract_test.go)
closes the runtime, parent and store, then resumes with fresh handles. It checks
stable identity through rename and name reuse, one logical execution and task
brief, preserved publication bytes, and no replay of an uncertain tool effect.

These paths need no additional public storage interface. The test file store
decodes backing JSON for each operation and commits by file replacement; it does
not call SQLite storage methods. It intentionally coordinates leases within one
fixture and has no heartbeat or power-loss durability guarantees. It is evidence
for the interface boundary, not a supported application backend. Backend-specific
migrations, cross-process lease timing and connection failures still need their
own tests.

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
