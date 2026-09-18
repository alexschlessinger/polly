# add-dir: extra read-only workspace paths

Status: implemented (phase 3 applied 2026-09-18); see `## Plan` below and the
completion note at the end of this file.

Spec drafted and approved in phases 1–2 of feature-workflow.

## Problem

`polly` sandboxes every session to a single writable workspace (the cwd, under
the default `workspace+net+git` preset). Multi-directory projects — a repo
plus sibling dependency repos, vendored checkouts, adjacent config/data trees —
have no way to let the agent read those other directories. Today the only
options are to open polly from a broad common parent (which the workspace
guardrail may reject and which widens the *writable* root) or to weaken the
sandbox (`--sandbox base`), both of which trade away safety to gain a read.

## Goals

- `--add-dir <path>` CLI flag (repeatable, global — works for one-shot `-p`
  runs too) adds extra directories as **read-only** paths.
- `/add-dir <path>` TUI command adds a directory mid-session; `/add-dir` with
  no argument lists the current extra dirs.
- Added dirs are read-only at **both** layers: sandbox process writes denied
  and polly's own file tools (`write_file`, `edit_file`) refuse writes.
- The list persists on the session record, so a resumed session restores it;
  resuming with `--add-dir` merges new dirs into the persisted list.
- Sub-agents, swarm workers, and worktrees inherit the parent's extra dirs as
  read grants in their own sandbox configs.
- The model is told about the extra read-only paths (session context, next to
  the working-directory description).

## Non-goals

- No writable `--add-dir` variant (possible future work; not designed here).
- No remote/cloud directories.
- No removal (`/remove-dir` or toggle): adding is one-way for the session's
  lifetime.
- No editing of added dirs from the TUI.
- No auto-discovery of "project directories".
- No global config-file surface: extra dirs are per-session, never a global
  grant that widens every session. (Rejected alternative: a config file
  listing dirs for all sessions.)

## Design

- New persisted field on `sessions.Metadata`, e.g.
  `ExtraReadDirs []string`. The session record is the source of truth.
- Sandbox layer: entries append to `sandbox.Config.ReadPaths` of the live
  config. `ReadPaths` is an existing concept (exemptions from the deny list,
  frozen at construction, read-only unless a separate `WritablePaths` grant
  covers them — which we never add). No new policy-engine concepts.
- `--add-dir` resolves/validates each path, appends (dedupe), persists.
- `/add-dir <path>` validates, appends to the live config + session record,
  persists, and reports; empty form lists current dirs.
- Inheritance: spawned members and worktrees receive the extra dirs as
  `ReadPaths` entries in their sandbox configs.
- Context: extra dirs are listed in the session context where the working
  directory is described, marked read-only.

### Validation at add time

- Path must exist; relative paths resolve from cwd; result must be absolute.
- Symlinks resolve to the real path (same treatment as workspace
  canonicalization); targets under a private root or `/tmp`-family roots are
  rejected.
- Reject `$HOME` itself or any ancestor of it (private root — granting it
  unmasks everything), including `/`.
- Reject paths inside the workspace (redundant) and reject/warn when an added
  dir contains masked credential paths (e.g. `~/.ssh`).
  *Superseded:* credential directories are no longer rejected. A directory
  that contains one keeps the deeper mask; one at or inside a credential path
  is the operator's explicit choice to expose it, and the sandbox posture
  names the exposure (see SANDBOX.md, "Credential paths denied by default").
- Adding an ancestor of the workspace is allowed (that is the multi-repo
  case) — read-only grant, documented as also exposing siblings.
- Dedupe: exact duplicates and subsumed nested paths are dropped.

## Edge cases

- **Deleted between sessions**: sandbox construction drops missing read
  grants and they cannot become readable later in that sandbox's lifetime;
  the session record keeps the entry (works again after recreate + resume).
- **Pathological inputs** (`/`, `~`, `.``, empty) all rejected per the rules
  above.
- **Git repos among added dirs**: `.git` is readable but read-only; read-only
  git commands work, anything writing fails. No extra git guardrail needed.
- **Mid-session add**: applies to tool calls and newly spawned
  sandboxes/MCP servers; a running stdio MCP server keeps its old policy
  until restarted. Documented, not auto-restarted.
- **No-sandbox platforms** (Windows / `_other`): dirs persist and appear in
  model context, but read-only is not enforced there (same as all sandboxing
  on those platforms).
- **Concurrency**: the list lives in the session record; parallel tool calls
  and sub-agents read a merged snapshot; `/add-dir` is serialized by the TUI
  input loop.

## Acceptance criteria

- Repeated `--add-dir` flags and `/add-dir` add, dedupe, persist, and
  reappear on resume; resume-with-flag merges.
- Writes into an added dir fail at both layers (sandboxed `bash` write and
  `write_file`/`edit_file` tool policy) on macOS, verified under the opt-in
  sandbox test suite (`POLLYTOOL_REQUIRE_SANDBOX_TESTS=1`).
- Sub-agent sandbox config contains the extra dirs as `ReadPaths`.
- Each rejection (home/ancestor, workspace-interior, nonexistent,
  symlink-to-private, credential-containing) yields a clear error message.
- Model context lists the extra read-only paths.
- Repo checks green: `CGO_ENABLED=0 go build ./...`, `go vet ./...`,
  `go test ./...`, gofmt clean.

## Open questions

- None blocking; research phase may surface implementation details (e.g.
  how mid-session sandbox config rebuild interacts with frozen `ReadPaths`).

## Plan

**Status:** plan approved by the user (phase 2 gate passed, 2026-09-18).
Open questions resolved with the plan's recorded defaults: (1) `--create-context
--add-dir` persists entries into the created context's metadata; (2)
credential-containing dirs are hard-rejected, not warned; (3) `/get sandbox`
output is left unchanged. The user also accepted the plan as-is despite the
placeholder conventions/docs-config lens reports (synthesizer covered those
lenses by direct code inspection).

Verified against commit `7efab709bad1dcee84544fec829194dfc0644a4d`.

### Summary

Two-wave implementation plan. Wave 1 lays groundwork in parallel: (1) a
persisted `ExtraReadDirs []string` field on `sessions.Metadata` (JSON blob,
no SQL migration; `cloneMetadata` must clone the slice) with its docs/API.md
note, and (2) the tools-layer additions: an exported platform-neutral
extra-dir validator in `tools/sandbox` plus `ToolRegistry.AppendBaseReadPaths`,
the only new mutation API needed because the registry freezes the base sandbox
config at `WithSandboxFactory` time. Wave 2 runs concurrently on the merged
result: (3) the whole cmd/polly surface — repeatable global `--add-dir` flag,
launch-time validation, resume merge in `conversationOpener.open` (explicit
merge, NOT a `settingSpecs` row, since those replace on flag), persistence
through the existing `updateContextInfo` write, `--create-context` symmetry,
the `/add-dir` TUI command in the shared command registry, and the
README.md/docs/SANDBOX.md documentation AGENTS.md requires for any widened
grant set; and (4) the model-context line in
`loadRepositoryInstructions`/`composeSessionContracts` listing the extra
read-only paths next to the working-directory description. Inheritance by
sub-agents, swarm members, and worktrees is automatic once the dirs are
`ReadPaths` in the parent registry's base config (verified:
`ExecutionPolicy`/`inheritableReadPaths`, worktree managers read
`SandboxReadPolicy`), so no swarm/worktree edits. Key settled decisions:
`--add-dir` deliberately does NOT join the `--nosandbox` conflict list
(Windows/unsupported platforms require `--nosandbox`, and the spec keeps
context listing there); no `POLLYTOOL_ADDDIRS` env default (spec non-goal: no
ambient grant widening every session); persisted-but-missing dirs survive on
the session record while the sandbox's own freeze drops them;
credential-containing dirs are hard-rejected.

### Checks (every wave)

- `CGO_ENABLED=0 go build ./...`
- `go vet ./...`
- `test -z "$(gofmt -l $(git ls-files -co --exclude-standard '*.go'))"`
- `go test ./sessions ./subagent ./llm ./workflow ./worktree -count=1`
- `go test ./tools -count=1`
- `go test ./tools/sandbox -count=1`
- `HOME="$TMPDIR/polly-add-dir-check-home" USERPROFILE="$TMPDIR/polly-add-dir-check-home" go test ./cmd/polly -count=1`
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/polly`
- `python3 -B -m unittest discover -s .github/local-ci -p 'test_*.py'`

### Final checks (once, after implementation)

- `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 CGO_ENABLED=0 go test ./...`
- `CGO_ENABLED=1 go test -race -count=1 ./tools ./sessions ./cmd/polly ./llm ./subagent ./swarm ./workflow ./worktree`
- `go test ./... -count=1`
- `.github/ci.sh cross`

### Tasks

#### `sessions-extra-read-dirs-metadata` — Persist extra read dirs on sessions.Metadata

Depends on: none.

Paths: `sessions/interface.go`, `sessions/sqlite.go`,
`sessions/sqlite_test.go`, `docs/API.md`.

Acceptance:

- `Metadata.ExtraReadDirs` exists with json tag `extraReadDirs,omitempty` and
  a doc comment naming it the source of truth for `--add-dir`/`/add-dir`
  extra read-only directories
- `cloneMetadata` deep-copies `ExtraReadDirs`; a store round-trip returns an
  equal, detached slice (mutating the returned slice does not affect a
  subsequent `GetMetadata`)
- `Clear` and a `Reset` that passes read-modify-write metadata back both keep
  the stored list (tests assert it)
- `go test ./sessions -count=1` passes and touched files are gofmt-clean;
  docs/API.md gains the `ExtraReadDirs` bullet

Brief:

Add the persisted field the add-dir feature stores its extra read-only directories on. sessions.Metadata (sessions/interface.go:157-198) is stored as a JSON blob in the settings_json column, so NO SQL migration is needed. Conventions: stdlib testing only (t.Fatalf, t.TempDir, no testify), errors via fmt.Errorf with %w, gofmt clean.

1. sessions/interface.go: add `ExtraReadDirs []string `json:"extraReadDirs,omitempty"`` to Metadata next to SkillDirs (line ~193), with a doc comment: canonical absolute real paths of extra read-only directories added with --add-dir / the /add-dir REPL command; the session record is the source of truth; resolved and validated at add time; restored on resume; kept by SetMetadata, Clear, and Reset (those call sites pass read-modify-write metadata back, verified at cmd/polly/storage.go:556 and :624).
2. sessions/sqlite.go cloneMetadata (line ~1761): add `out.ExtraReadDirs = slices.Clone(metadata.ExtraReadDirs)` beside the other slices. Without it a SetMetadata write can alias the caller's slice, since cloneMetadata hand-lists every cloned slice.
3. Tests in sessions/sqlite_test.go following the existing TestSQLiteSessionContract / openTestStore(t, ModeMemory/ModeFile, ...) style: SetMetadata with a nonempty ExtraReadDirs then GetMetadata returns an equal slice that is detached (mutate the returned slice, re-Get, assert unchanged; likewise mutating the caller's Metadata after SetMetadata does not leak in); the field round-trips through both store modes; session.Clear and a Reset-with-same-metadata both preserve the stored list.
4. docs/API.md, sessions section near the `Metadata.Title` bullet (~line 1240): add one bullet documenting Metadata.ExtraReadDirs (extra read-only workspace directories recorded as canonical real paths, restored when the session is opened, preserved by SetMetadata/Reset/Clear, `extraReadDirs` in JSON).

#### `sandbox-extra-dir-validator-registry-append` — tools/sandbox extra-dir validator and ToolRegistry.AppendBaseReadPaths

Depends on: none.

Paths: `tools/sandbox/extra_read_dir.go`,
`tools/sandbox/extra_read_dir_test.go`, `tools/registry.go`,
`tools/registry_test.go`.

Acceptance:

- Each spec rejection case is a distinct, clear error message
  (nonexistent, not-a-directory, filesystem root, home or ancestor,
  temp-family root, workspace-interior, credential-containing); symlinked
  input canonicalizes to the real path; an ancestor of the workspace is
  accepted
- The lenient entry point accepts a deleted dir (kept on the record; sandbox
  `PrepareConfig` freeze drops it) while still rejecting root/home/credential
  spellings
- `MergeExtraReadDirs` is order-stable and drops exact and subsumed
  duplicates; repeated adds converge
- After `AppendBaseReadPaths`, `SandboxReadPolicy().ReadPaths` contains the
  canonical path, a `Derive()`d child registry inherits it, and without a
  sandbox factory the call is a nil no-op; `go test ./tools/sandbox -count=1`
  and `go test ./tools -count=1` show no failures beyond the known
  environmental ones, and touched files are gofmt-clean

Brief:

Build the two tools-layer primitives the cmd/polly surface (a later wave) consumes. Everything here is pure Go, platform-neutral, must compile for windows/_other (no build tags, no cgo; the feature needs no *_darwin/_linux variants). Conventions: stdlib testing (t.Fatalf, t.TempDir), gofmt clean, build with CGO_ENABLED=0.

1. New file tools/sandbox/extra_read_dir.go exporting:
   - `ValidateExtraReadDir(workspace, path string) (string, error)` — canonicalizes and strictly validates one add-dir entry: expand a leading ~ via the home lookup, filepath.Abs against the process working directory, filepath.EvalSymlinks to the real path; require the result to exist and be a directory (reject files, symlinks to files, nonexistent paths — the Claude Code/Gemini precedent). Reject with distinct, clear messages for: the filesystem root (path whose filepath.Dir equals itself); $HOME itself or any ancestor of home (os.UserHomeDir + EvalSymlinks, compare via PathWithin — granting home unmasks everything, including `/`); paths equal to or inside the OS temp-family roots (os.TempDir root; /tmp equal-case); paths inside the workspace (PathWithin(workspace, path) — redundant, the workspace is already readable/writable); and paths containing a masked credential path (for each entry of the exported sandbox.DeniedPaths list (tools/sandbox/sandbox.go:864), reject when it resolves inside the added dir — hard reject, not a warning; matches the credential deny list's fail-closed posture). Adding an ancestor of the workspace is allowed (the multi-repo case). Return the canonical real path.
   - A lenient entry point for persisted entries re-validated on resume, e.g. `CanonicalizeExtraReadDir(path string) (string, error)` or a bool parameter on the validator: same canonicalization and root/home/credential rejections, but skip the existence check — a dir deleted between sessions stays on the session record and the sandbox's own construction freeze (sandbox.PrepareConfig drops missing read grants; see the ReadPaths doc at tools/sandbox/sandbox.go:288) makes it unreadable until recreated.
   - `MergeExtraReadDirs(existing, added []string) []string` — order-stable append that drops exact duplicates and entries subsumed by an already-present entry (use PathWithin for subsumption; an existing ancestor subsumes a new descendant, a new ancestor does not delete existing descendants).
2. tools/registry.go: add `func (r *ToolRegistry) AppendBaseReadPaths(paths ...string) error`. The registry freezes the caller-approved base authority once (preparedBaseSandboxConfig, tools/registry.go:216-228, guarded by sandboxConfigMu, never re-prepared to prevent symlink retargeting), so a mid-session add must: take r.sandboxConfigMu; when no sandbox factory is configured (no factory, or unsafeNoSandbox), return nil as a documented no-op (nothing to grant; the caller still records the list); otherwise build a fresh `sandbox.Config{ReadPaths: paths}`, run it through sandbox.PrepareConfig (freezes identities, drops missing paths, minimizes nested grants), and set `r.baseSandboxCfg = r.baseSandboxCfg.Merge(prepared)` — Config.Merge (tools/sandbox/sandbox.go:917) concatenates ReadPaths/authorityPaths/grantSymlinks, so already-frozen identities are preserved and the new entries are frozen exactly once. After the call, SandboxReadPolicy(), every newSandboxFor per-tool sandbox, and later Derive() subagent registries see the new paths; already-spawned children and running stdio MCP servers keep their snapshots (intended, documented). Note the merged list is not re-minimized: overlapping grants are harmless (deepest rule wins); the persisted session list is deduped by MergeExtraReadDirs, so do not assert live-list uniqueness.
3. Tests: new tools/sandbox/extra_read_dir_test.go (follow TestPrepareConfig* style in tools/sandbox/sandbox_test.go): a table over every rejection case with distinct error text, symlink resolution to the real path (create a symlink with t.TempDir), ancestor-of-workspace accepted, workspace-interior rejected, temp root rejected, and the lenient path accepting a deleted directory. Extend tools/registry_test.go using the stubSandboxRegistry pattern (tools/view_image_test.go:44-48): after AppendBaseReadPaths the path appears in SandboxReadPolicy().ReadPaths; a Derive()d registry's base config also contains it; without a sandbox factory the call returns nil and changes nothing; appending a missing path does not error and grants nothing.

#### `cli-add-dir-flag-and-tui-command` — Wire --add-dir flag, resume merge, and /add-dir TUI command

Depends on: `sessions-extra-read-dirs-metadata`,
`sandbox-extra-dir-validator-registry-append`.

Paths: `cmd/polly/config.go`, `cmd/polly/types.go`,
`cmd/polly/conversation.go`, `cmd/polly/sandbox_wiring.go`,
`cmd/polly/repl_commands.go`, `cmd/polly/storage.go`,
`cmd/polly/config_test.go`, `cmd/polly/repl_commands_test.go`,
`cmd/polly/flag_sources_test.go`,
`cmd/polly/session_private_paths_test.go`, `README.md`, `docs/SANDBOX.md`.

Acceptance:

- Repeated `--add-dir` flags populate `Config.AddDirs`; opening with an
  invalid entry fails before the session runs with an error naming the path
  and the rejection reason (home/ancestor, filesystem root, temp root,
  workspace-interior, nonexistent, not-a-directory, credential-containing)
- Resume merges `--add-dir` entries into the persisted `ExtraReadDirs`
  instead of replacing them (extend the flag_sources_test.go
  `conversationOpener` harness); a stored dir that no longer exists does not
  fail the open, stays on the record, and is granted again after
  recreate+resume
- With the `newSandbox` capture pattern, the opened registry's base config
  `ReadPaths` contain both the stored and the flagged dirs;
  `--nosandbox --add-dir` is accepted (test rows in both conflict tables)
- `--create-context` with `--add-dir` persists the entries into the created
  context's metadata (testable via the created context's `GetMetadata`)
- `/add-dir` with no argument lists the current dirs; with an argument it
  validates, appends to the live registry and the session record, and
  reports the resulting list; invalid paths and a missing active session
  produce clear `replyLine` errors; the add form queues behind an in-flight
  turn (`busySafeWhen` false for args)
- README.md and docs/SANDBOX.md describe `--add-dir` and `/add-dir`
  including the no-env-var and `--nosandbox` coexistence notes and the
  mid-session/MCP scope; build, vet, gofmt, and the cmd/polly tests for the
  touched files are green

Brief:

Wire the whole user-facing surface. Both the one-shot (-p) and REPL paths enter through parseConfig (cmd/polly/config.go:53-101) and conversationOpener.open (cmd/polly/conversation.go:226-352), so one global flag plus one merge point in open covers everything. Depends on the sessions.Metadata.ExtraReadDirs field and the tools/sandbox validator + ToolRegistry.AppendBaseReadPaths from wave 1. Conventions: stdlib testing (t.Fatalf, t.TempDir), go test -count=1, gofmt clean; platform-neutral Go only.

1. cmd/polly/types.go: add `AddDirs []string` to Config next to ReadPaths (line ~65), with a comment: extra read-only directories from --add-dir, per-session, validated and merged into metadata at open.
2. cmd/polly/config.go: add to sandboxConfigFlags() next to --readpath (line ~346): `&cli.StringSliceFlag{Name: "add-dir", Usage: "Extra read-only directory sandboxed tools may read (repeatable; validated; persisted per session and merged on resume)"}`. Do NOT add Sources/envDefault: the spec's non-goal forbids an ambient grant that widens every session, so there is deliberately no POLLYTOOL_ADDDIRS. Read it in parseConfig (line ~66 area) into Config.AddDirs via cmd.StringSlice("add-dir") (dashes in flag names are fine, cf. swarm-apply-timeout).
3. Do NOT add "add-dir" to the --nosandbox conflict list in validateSandboxFlagCombination (config.go:393): unsupported platforms (tools/sandbox/sandbox_other.go returns an error; cmd/polly has no auto-detection, the open fails with 'sandbox requested but unavailable') can only run with --nosandbox, and the spec requires dirs to persist and appear in model context there. Deliberately extend both tables in cmd/polly/config_test.go (TestSandboxFlagsConflictWithEffectiveNoSandbox and TestSandboxFlagsFromEnvConflictWithCLINoSandbox, lines ~294-332) with cases asserting --add-dir does NOT conflict with --nosandbox.
4. cmd/polly/conversation.go open: metadata is read at line 254; before the sandbox wiring at line 278, resolve the merged list: validate each config.AddDirs strictly with sandbox.ValidateExtraReadDir (workspace = the working directory; error out with a clear message naming the offending path — filesystem validation belongs at sandbox startup, not flag parsing, per the validateSandboxPresetSpec comment at config.go:359-362); re-canonicalize each persisted metadata.ExtraReadDirs leniently (missing dirs are tolerated and kept). Merge with sandbox.MergeExtraReadDirs, assign the merged list to metadata.ExtraReadDirs (it is persisted by the existing updateContextInfo SetMetadata write at line 319), and pass the list to sandboxRegistryOptionsWithWarnings. Do NOT add a settingSpecs row for add-dir: standard rows REPLACE on flag-given (applyFlagSettings/updateContextInfo copy flagged rows), while this feature must merge flag dirs into the persisted list — resolve the merge explicitly in open, as this brief specifies. With --nosandbox the wiring returns early with no factory: the list still persists and still reaches model context; nothing is enforced.
5. cmd/polly/sandbox_wiring.go: extend sandboxRegistryOptionsWithWarnings (line 20) with an extraReadDirs []string parameter and append those entries to the ReadPaths in the baseCfg.Merge at lines 32-37 (after homeReadGrants — do not route them through homeReadGrants, which filters for home-interior candidates). Update both call sites: conversation.go:278 (merged list) and cmd/polly/storage.go resolveCreateTools (line ~426): pass the strictly validated CLI dirs so --create-context grants them while resolving tools, and in handleCreateContext (line ~400-416) persist the validated entries into the created context's metadata (info.ExtraReadDirs) so the created context restores them when opened. This is the recorded default answer to the open question; if the user answers differently, reject or ignore the flag there instead.
6. cmd/polly/repl_commands.go: register the command in newDefaultReplCommandRegistry (lines 79-213) following replRenameCommand's handler shape (lines 439-459): name "/add-dir", usage "/add-dir [path]", summary "add a read-only directory, or list current ones", busySafeWhen: func(args []string) bool { return len(args) == 0 } (the no-arg list form is read-only and busy-safe; the mutating add form queues behind an in-flight turn — the TUI input loop then serializes it, satisfying the spec's concurrency note; the registry documents busySafe as reserved for read-only inspection, repl_command_registry.go:25-28). No path completer needed (/attach ships without one). Handler replAddDirCommand: guard ctx.state == nil || ctx.state.session == nil (reply "no active session"); no args → read the session's metadata (GetMetadata) and reply with the current ExtraReadDirs list ("extra read-only dirs: ..." or "no extra read-only dirs"); with a path → workspace = state.toolRegistry.ExecutionRoot() falling back to os.Getwd() (the loadRepositoryInstructions pattern), sandbox.ValidateExtraReadDir strictly, then updateMetadata (cmd/polly/conversation.go:143) appending via sandbox.MergeExtraReadDirs, call state.toolRegistry.AppendBaseReadDirs(canonical) (nil no-op when sandboxing is off), and reply with the resulting list; when the registry has no sandbox (registry.HasSandbox() false / --nosandbox mode) append a note that sandboxing is off so the grant is not enforced. All errors via ctx.replyLine with the validator's message. Registration automatically reserves the name in reference parsing and surfaces it in /help.
7. Tests (all runnable in this repo's sandboxed copies): config_test.go — flag parsing of repeated --add-dir into Config.AddDirs plus the two no-conflict table rows above; cmd/polly/flag_sources_test.go — extend the TestEnvironmentDefaultsDoNotOverrideStoredSession harness (setupSessionStore + updateMetadata + opener.prepare/open) to prove resume MERGES --add-dir into the stored ExtraReadDirs rather than replacing it; cmd/polly/session_private_paths_test.go or a sibling — use the newSandbox package-var capture pattern (lines 31-55: swap newSandbox, restore via t.Cleanup, assert on the captured configs) to assert the opened base config ReadPaths contain the stored and flagged dirs; cmd/polly/repl_commands_test.go — dispatch "/add-dir" (list form) and "/add-dir <path>" (add form, including an invalid-path rejection and the no-active-session guard) via newManagedREPL + r.runCommand + transcriptTexts, modeled on TestGetCommandShowsStableFlagBackedSettings.
8. Documentation (mandatory in this task — AGENTS.md: 'never widen a grant set ... without updating docs/SANDBOX.md'): README.md §Sandboxing (~lines 800-815): add --add-dir as the read-only extra-directory grant — repeatable, validated (must exist as a directory; home/root/tmp/workspace-interior rejected), persisted per session, merged on resume, addable mid-session with /add-dir, inherited read-only by sub-agents/swarm members/worktrees; also add /add-dir to the TUI command list (~lines 260-263). docs/SANDBOX.md §Global flags (~line 277): document --add-dir, explicitly noting it has no POLLYTOOL_* env default (a per-session surface, never an ambient grant) and that it is deliberately allowed with --nosandbox so unsupported platforms keep the context listing while nothing is enforced; §How policies merge (~line 366): extra dirs append to ReadPaths and freeze at construction, a missing persisted dir is dropped by the sandbox but kept on the session record (works again after recreate + resume), a mid-session /add-dir applies to later tool calls and newly spawned sandboxes and stdio MCP servers, and a running stdio MCP server keeps its load-time policy until restarted (documented, not auto-restarted).

#### `model-context-extra-read-dirs` — List extra read-only dirs in the model's session context

Depends on: `sessions-extra-read-dirs-metadata`.

Paths: `cmd/polly/repository_instructions.go`, `cmd/polly/turn.go`,
`cmd/polly/swarm.go`, `cmd/polly/repository_instructions_test.go`.

Acceptance:

- With `ExtraReadDirs` set on the session, the composed contract lists the
  canonical dirs next to the working-directory line, explicitly marked
  read-only
- With no extra dirs, and for a nil/empty list, the composed output is
  unchanged from today (existing repository-instruction tests still pass
  unmodified in their expectations apart from the new-case additions)
- A custom `--system` prompt still suppresses the whole block including the
  extra-dirs line (the existing `SystemPrompt` gate is untouched)
- A metadata change between two `composeSessionContracts` calls is reflected
  on the next call (per-turn re-read, no caching)
- All `loadRepositoryInstructions` call sites compile and pass:
  `cmd/polly/turn.go` passes the session's list, `cmd/polly/swarm.go:67`
  passes nil; gofmt clean on touched files

Brief:

Tell the model about the extra read-only paths, in the session context where the working directory is already described. Depends on the sessions.Metadata.ExtraReadDirs field from wave 1. Conventions: stdlib testing, t.Fatalf, gofmt clean.

1. cmd/polly/repository_instructions.go: loadRepositoryInstructions currently prints `fmt.Fprintf(&b, "Working directory: %s\n\n<repository_instructions>\n", cwd)` at line 68. Change its signature to accept the extra read-only dirs (e.g. `loadRepositoryInstructions(registry *tools.ToolRegistry, extraReadDirs []string)`); when the list is non-empty, print one additional line after the working-directory line and before the blank line, listing the canonical paths and marking them read-only, e.g. `Extra read-only paths (readable but not writable): /a, /b` — exact wording is the editor's choice, but it must (a) name the canonical real paths, (b) mark them read-only, and (c) leave the output byte-identical to today when the list is empty (the existing test asserts a HasPrefix on "Working directory: "+cwd+"\n"). Note the existing read policy in this function (registry.SandboxReadPolicy() + sandbox.ReadAllowed, lines 36-57) already lets AGENTS.md loads succeed from newly granted dirs once they are ReadPaths — no change there.
2. cmd/polly/turn.go composeSessionContracts (lines 101-122): obtain the list from the session record the same way sessionTitleGuidance does (cmd/polly/session_title.go:58-68: state.session.GetMetadata(ctx), guarding nil state/session; on a read error, skip the line and append a warning rather than failing the turn), and pass metadata.ExtraReadDirs to loadRepositoryInstructions at line 110. This stays inside the `settings.SystemPrompt == ""` gate, exactly like the repository instructions today — a custom --system prompt replaces the whole block, unchanged behavior. Re-reading per turn (the function's documented design) means a mid-session /add-dir shows up on the next turn with no caching; the read-only context inspector (repl_context_stats.go:25) shares composeSessionContracts and gets the line for free.
3. Update the other caller: cmd/polly/swarm.go:67 calls loadRepositoryInstructions(registry) — pass nil there (a member's inherited grants come from its sandbox config, and the member has no parent metadata to read; the context line is the parent-session surface).
4. Tests: extend cmd/polly/repository_instructions_test.go — the existing header test (~line 66) gains cases: with extra dirs the output contains the read-only paths line positioned after the working-directory line; with an empty list the output is unchanged (assert equality with the pre-change format); add a composeSessionContracts-level test (or a direct helper test) proving a metadata change between calls is reflected on the next call.

### Docs updates

None beyond the per-task edits above (README.md and docs/SANDBOX.md are
edited by `cli-add-dir-flag-and-tui-command`; docs/API.md by
`sessions-extra-read-dirs-metadata`).

### Risks

- Wave checks run in nested sandboxed copies with known unchanged-baseline
  failures: `go test ./tools/sandbox` (44 Seatbelt tests fail
  `sandbox_apply: Operation not permitted` plus unix-socket bind),
  `go test ./tools` (TestStageMCPServerWithNamespacePrefix, uv cache under
  the private home), and exactly 3 cmd/polly failures under the HOME redirect
  (TestOneShotTerminalContract pty exhaustion, TestCLISignalExitStatusAndStderr
  tty, TestSandboxNoticeReportsMissingSSHAgent socket). Only deltas beyond
  these indicate a regression; the full
  `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1` suite cannot pass in a nested sandbox
  at all and is left to CI/an unsandboxed host (finalChecks).
- The spec's two-layer write-denial acceptance (sandboxed bash write and
  write_file/edit_file into an added dir, verified under
  `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1`) is covered by existing engines —
  `sandbox.Config.ReadPaths` plus `tools/sandbox/write_policy.go`'s
  `WriteAllowed` (writes allowed only under OS temp dirs and WritablePaths,
  which never include added dirs); Linux binds read grants read-only via
  `planLinuxGrants` and macOS re-allows them at depth. No new enforcement
  concept is built, so those acceptance tests are exercised only by CI.
  Verify with stubSandboxRegistry-style tool-policy tests locally
  (tools/write_file_test.go pattern) and REQUIRE-gated end-to-end tests in
  CI.
- `--add-dir` deliberately does NOT conflict with `--nosandbox`
  (validateSandboxFlagCombination), because unsupported platforms
  (tools/sandbox/sandbox_other.go errors, no auto-detection in cmd/polly)
  can only run with `--nosandbox` and the spec requires dirs to persist and
  appear in model context there. Consequence: on sandboxed platforms,
  `--nosandbox --add-dir` is accepted and the dirs are unenforced — a
  documented divergence from every sibling sandbox flag.
- Mid-session `/add-dir` does not affect already-spawned subagents or swarm
  members (they snapshot the parent base config at Derive/spawn time) or a
  running stdio MCP server (config captured at load). Intended per the spec;
  must be documented in docs/SANDBOX.md.
- Swarm members mask inherited read grants against their own denied reads
  (member runtime dir, other members' roots, checkout source root;
  tools/execution_context.go `inheritableReadPaths`). An added dir masked for
  a member stays denied for that member; no new swarm test covers this, the
  existing TestCheckoutReadGrantsHonorOperatorDenials machinery governs it.
- `--add-dir` has no `POLLYTOOL_*` env default, unlike every sibling
  sandbox flag, because the spec's non-goal forbids an ambient grant that
  widens every session. Users who expect the env var pattern may file this
  as a gap.
- session.Clear and sessions Reset call sites pass read-modify-write
  metadata back (cmd/polly/storage.go:556, :624), so ExtraReadDirs survives
  /reset; if a user expects a reset to drop the grants, that is a
  documentation matter, not a bug.
- ToolRegistry.AppendBaseReadPaths merges frozen entries into the base
  without re-minimizing the combined list, so the live policy may carry
  overlapping ReadPaths (harmless: deepest rule wins). The persisted session
  list is deduped by the merge helper; do not assert list uniqueness against
  the live policy, only containment.
- The conventions and docs-config lenses returned placeholder reports (no
  usable content). Conventions (flag patterns, conflict test tables,
  stubSandboxRegistry/newSandbox capture harnesses, stdlib-test style,
  platform-split compilation) and documentation surfaces (README.md sections
  ~260-263 and ~800-815, docs/SANDBOX.md sections ~277 and ~366, docs/API.md
  sessions section ~1240) were established by direct code inspection;
  residual risk that an unstated convention exists that inspection missed.
- gofmt is enforced by no hook or CI step; the gofmt check entry must run
  manually every wave or formatting drift will not be caught.

### Open questions

1. `--create-context`: the plan persists `--add-dir` entries into the created
   context's metadata (symmetry with a directly opened session, and
   resolveCreateTools already builds its sandbox from the same Config).
   Confirm this, or state that management flows should reject/ignore
   `--add-dir` instead.
2. The spec says 'reject/warn' for added dirs containing masked credential
   paths (e.g. ~/.ssh). The plan hard-rejects (matches the sandbox's
   fail-closed posture and the credential deny list; a warned-but-granted
   ~/.ssh read would be a policy hole). Confirm hard rejection is wanted.
3. Should the `/add-dir` no-argument listing also surface in `/get sandbox`
   output (cmd/polly/sandbox_posture.go currently only counts ReadPaths, so
   the count inflates but the dirs are not named)? The plan leaves this out
   as not required by the spec.

## Completion

Implemented by the phase-3 workflow (status `applied`), both waves, zero
repairs, reviewer approved each wave. One post-merge fix by the parent:
`resolveConfigAddDirs` now dedupes repeated/subsumed `--add-dir` flags
(`sandbox.MergeExtraReadDirs`), so `--create --add-dir a --add-dir a` stores
each directory once (the open path already merged; the create path did not),
with a regression case in `TestCreateContextStoresAddDirEntries`.

Verified by the parent on the final tree: `CGO_ENABLED=0 go build ./...`,
`go vet ./...`, gofmt clean, windows/amd64 + `.github/ci.sh cross` builds,
local-ci python tests, `go test ./...` with a failure set byte-identical to
the pristine HEAD baseline run in the same nested sandbox (Seatbelt suite,
uv-cache MCP test, two cmd/polly tty/pty tests, scratch-home and skills
archive tests — all environmental), race suite green except the same known
environmental failures, and end-to-end runs of the built binary: every
rejection path (nonexistent, `~`, `/`, `/tmp`, workspace-interior) fails
with a clear message before the session runs, and `--create --add-dir`
persists canonical deduped `extraReadDirs` in the session record.

Still owed on an unsandboxed host or CI: the
`POLLYTOOL_REQUIRE_SANDBOX_TESTS=1` suite (cannot execute under a nested
Seatbelt sandbox; the new add-dir unit tests pass under the flag) and a
final unsandboxed `go test ./...`.
