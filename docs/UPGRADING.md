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
