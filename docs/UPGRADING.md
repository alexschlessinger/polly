# Upgrade checklist

For upgrades from **v0.91-alpha** to the current code. See the [CLI guide](CLI.md)
and [Go API reference](API.md) for the complete interfaces.

## CLI and TUI

- **Context compaction:** conversations clear older tool output and summarize
  history when needed. The saved transcript remains intact. `/compact` requests
  a summary now; `--compactmodel` or `/set compactmodel` chooses its model.
  Summarization makes a billable model call. `--maxcontext 0` disables proactive
  compaction, not recovery after a provider rejects an oversized request.
- **Output allowance:** `--maxtokens` defaults to **32,000**. Set an explicit value
  if your workflow needs another allowance; the model's limits still apply.
- **Inspection:** tools and recorded file diffs open inline, rather than through
  `/inspect`. Expanding a tool batch reveals all its recorded calls; long
  outputs have Previous/Next pages. Shift-Tab selects visible controls,
  Left/Right chooses, and Enter activates. The agent panel remains. The
  status-bar Changes count is not a workspace-diff viewer; use Git for the net
  workspace diff.
- **Preferences:** use `/set` for session settings and `/model` for the model/key
  form. `/effort`, `/fast`, and `/keys` remain compatibility shortcuts but are not
  advertised in completion or help. Session preferences persist with the session;
  API keys remain process-only. `/setup` saves launch defaults, and confirming
  `/theme` saves the UI theme for later launches.
- **Clipboard:** `/copy` copies the last completed answer, preserving Markdown.
  It requires a terminal that permits OSC 52 clipboard writes.
- **Response phases:** phase-tagged commentary is retained for provider replay
  but is not displayed as answer text. Final and unphased text remain visible.

## SQLite storage: schema 9 to 10

**Stop every older Polly process and embedded host using the database before the
first schema-10 open.** New opens reject unsupported future schema versions, so
old binaries refuse a migrated database. A version check cannot fence an old
writer that already has the database open. Mixed old/new writers are unsupported.

Before upgrading, make a restorable backup with SQLite's online backup facility
(or stop all users and checkpoint/close the database before copying it). Copying
only a live WAL-mode database file can omit committed data. Keep the pre-upgrade
backup for downgrade: schema 10 has no in-place downgrade path; changing
`user_version` does not convert the database.

The migration validates the existing schema and integrity before deriving indexed
coordination relations, partition counts, durable revision stamps, semantic status
facts, canonical news history, compact display facts/tombstones and transcript
display boundaries/prefixes. Backfill uses bounded key batches and the selected
record's actual dependencies in one transaction; it can take time proportional to
retained history. Failure rolls back both data and schema/version changes. Unknown
record kinds remain opaque JSON records; malformed supported records fail migration
rather than produce incomplete indexes. Schema 10 is unreleased: an incomplete
intermediate-10 shape is refused, not silently repaired or erased.
Swarm record format remains **2**. Incompatible formats cannot be interpreted by
format-dependent native selectors.

Schema 10 also persists transcript replacement epochs and adds summary/parent,
spawn, relation and revision indexes. Clear/Reset advance the epoch atomically;
append, metadata reads and ordinary settings changes do not. Epochs and revision
tokens include stable session/family identities and survive reopen and
memory-to-disk promotion. Promotion preserves live handles, lease owner tokens,
artifact references, epochs and revision tombstones. Record deletion retains its
revision tombstone until the family is deleted; moving a record invalidates both
its old and new selector partitions. Native and legacy coordination writes in
this binary use the same derivation path and atomic effect transaction.

Native summary pages, transcript pages and view headers are lease-free reads:
they do not touch last-used times or repair data. Expired sessions remain visible
when a live lease or swarm pin retains them. Summary continuations are live
keysets, not frozen exports. Transcript continuations must carry the stable ID,
epoch and captured sequence boundary; replacement fails with a stale-epoch error.
A single oversized requested record/message is returned intact. View headers
provide the same metadata revision and read-only artifact handle as full views
without loading history; hosts use them only with the same backend's transcript
paging capability.

Point records and exact partition revisions/counts use indexed selection before
payload decoding. Requested exact totals for a mixed-kind selector additionally
filtered by kind traverse that matching index range; deep offsets, broad exports,
full-history APIs and migration remain potentially large. Default startup checks,
expiry policy, the connection count, lease rules and WAL/FULL durability are
unchanged. Existing whole-result APIs are not replaced by pages automatically.

Schema 10 also adds source-table candidate indexes for settlement, release and
recovery, plus maintenance indexes. The migration creates them after mandatory
integrity checks; their construction can take time proportional to retained
records. A current-schema store missing one is refused, not silently repaired.
This does not add a new swarm format or materialized-table backfill requirement.
Default startup checks and expiry remain unchanged. Go hosts may explicitly
choose `Integrity: IntegrityDeferred` with a positive
`DeferredIntegrityInterval`; structural validation and pre-migration or repair
checks are never deferred.

## Go library hosts

Before starting or recovering a swarm:

1. Supply `swarm.Config.OpenWorktrees` for Git-backed workspaces. It receives a
   `worktree.Config`; set its administrative registry and honor its root,
   directory, capacity, and private paths. A missing constructor disables Git
   workspace construction and refuses recovery of saved Git state with
   `swarm.ErrWorktreesUnavailable`.
2. Supply `PrivatePaths` for active storage, promotion destinations, sidecars,
   and canonical aliases. Swarm does not infer SQLite locations. Resolve these
   exclusions before constructing tools or MCP clients and share them with
   workspace/snapshot construction. The CLI does this assembly automatically.
3. Ensure parent and acquired member handles implement
   `sessions.CoordinationSession`. Incompatible child handles are closed and
   rejected at acquisition, including on resume.

See [runtime construction](API.md#swarms-and-workflows) and
[backend assembly](RUNTIME-BACKENDS.md#application-assembly).

Persist complete `AgentResponse.AllMessages`, including internal compaction
markers, summary usage, metadata, and assistant `TextBlocks`. Keep using
`AllMessages[PersistedMessages:]` when checkpoints already saved a prefix.
`Agent.Compact` returns summary records without running tools or continuing the
conversation; its caller owns persistence. See [context compaction](API.md#context-compaction).

## Workflows

An explicit worker blocker interrupts the whole workflow attempt, drains active
work, and returns `workflow_blocked` to the parent. Completed candidates and
unfinished work remain available. Inspect the report and decide how to recover;
JavaScript is not automatically replayed, and retained edits are not automatically
integrated. Ordinary waits do not interrupt the attempt. See
[workflow delivery](WORKFLOWS.md#delivery-and-shared-findings).
