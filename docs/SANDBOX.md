# Sandboxing

Polly runs LLM-driven commands — the builtin `bash` tool, shell tools, and
stdio MCP servers — inside an OS-level sandbox when something asks for one.
Sandboxing is opt-in: `--sandbox`, a path grant or `--allownet`, from a flag,
the environment or the configuration file, is what asks, and a launch nothing
asked runs those commands unsandboxed. This is the reference: threat model, every configuration field, the workspace Git
protection, and how the Linux and macOS backends differ. The CLI summary is
in [README.md](README.md#sandboxing); library wiring in
[API.md](API.md#sandboxing-in-the-library).

**Contents:** [Intent](#intent) ·
[What gets sandboxed](#what-gets-sandboxed) ·
[Configuration](#configuration) ·
[Workspace Git protection](#workspace-git-protection) ·
[Platform implementations](#platform-implementations) ·
[Observing decisions](#observing-decisions) · [Limitations](#limitations)

## Intent

The commands polly runs are chosen by a language model, often steered by
untrusted input. The sandbox limits the blast radius of a hallucinated,
prompt-injected, or buggy command; without one, a command polly runs can do
whatever your user can. Under a sandbox a process sees
ordinary home files, toolchains and configuration, with an optional
[private-home mode](#the-private-home-directory),
cannot read [credential paths](#credential-paths-denied-by-default) anywhere,
cannot see [credential-shaped environment variables](#environment-filtering),
cannot write outside the [writable set](#the-per-tool-sandbox-object), and
cannot reach host Unix sockets without a grant. The base policy denies
network; the preset a policy gets when it names none, `workspace+net+git`,
includes it.

A sandbox that was asked for is **not optional** (tool metadata cannot opt
out; only the caller's `--nosandbox` / `WithUnsafeNoSandbox` can), **fails
closed** (no
backend, or a sandbox that fails to construct, is an error, never a silent
unsandboxed run), **frozen at startup** (a grant is bound to the filesystem
object it named when the sandbox was built, and nothing created later inside
a private root becomes visible), and is **observable** under `--debug`. It is
containment for accidents and injection, not a hostile-binary jail: the
process runs as your user on a shared kernel. See [Limitations](#limitations).

## What gets sandboxed

Swarm members use a registry-level execution root and a separately prepared
sandbox for that root. No process-wide `chdir` is used. Editing worktrees are
outside the parent's checkout; native file tools, bash, shell tools, images,
skills, and repository instructions resolve there. Local stdio MCP servers
relaunch there. Context-specific binding discards tool/server sandbox overlays
that could restore the parent's write grants. Required incompatible tools fail
launch; optional ones are omitted and reported. Later skill activation cannot
expand the member's inherited tool capabilities.
Typed `/spawn`, model delegation, and scripted `polly.agent` all enter this same
runtime. A TUI tab does not grant a child the parent's bound tools or filesystem
access. `/spawn --read-only` uses the same research policy as `read_only:true`.
Every member context owns a private scratch directory (`$TMPDIR`, `TMP` and
`TEMP`), named for its slot inside the **scratch root**
(`$TMPDIR/polly-<uid>`; the names are short so that a Unix socket path inside
a scratch, such as tmux's, fits the 104-byte limit on macOS). A read-only
member writes there and in host temp, nowhere else. Siblings can neither read nor write it: the scratch
root is a private root of every policy and only the member's own scratch is
granted back inside it, so a sibling started later is invisible without any
new rule. Slot directories and their scratch are created on demand. Cleanup
restores owner access to read-only directories inside scratch (such as a
module cache a build left read-only) before removing them, without traversing symlinks to external
files.

The scratch root sits in the OS temp area rather than under your home for one
reason: **no ancestor of a scratch may be denied**. A tool that opens each
component of a path in turn — what polly's own `internal/safefile` does to
refuse a symlinked route, and what any careful tool may do — fails on the
first ancestor the policy hides, however the leaf is granted. A scratch under
the private home made every such suite unrunnable in a member, with the open
of the home itself reported as the error. For the same reason the scratch
root's own directory entry stays readable while everything inside it needs a
grant: listing it shows polly's slot names and nothing of yours, and no slot
inside it can be read or written without a grant. It is created `0700` and
refused if an entry already standing at that name is not a directory this user
owns exclusively. Each scratch is released with its context; because it no
longer sits inside the directory that owns it, polly also records that owner
beside it and reclaims, on startup, any scratch whose owner was removed
wholesale rather than released. A scratch whose owner is still on disk is
never reclaimed, so a second polly running at the same time is unaffected.

A polly, or a test suite of polly's own, started inside a sandboxed command
needs a scratch root it may write. On Linux the command's `$TMPDIR` is its
private `/tmp`, so the root it computes is there. On macOS the command keeps
the host temp directory, so a command without a scratch of its own (your
bash, an MCP server) is granted `nested/` inside the scratch root and told,
through `POLLYTOOL_SCRATCH_ROOT`, to claim scratch there; every other policy
still denies it. A member computes its root under its own scratch and needs
nothing. `POLLYTOOL_SCRATCH_ROOT` relocates the scratch root for any polly
that sees it set.

Member policies hide the parent checkout, the runtime directory, the scratch
root, every other context's root, and the session database as private roots,
and deny every write to common Git metadata and the linked worktree's `.git`
entry. The
common Git object store and the user's Git configuration stay readable. A
read-only member's checkout is listed in `denyWritePaths` on top of the
missing write grant. Host temp stays writable for it as for every context:
macOS's bash 3.2 puts here-documents in a system temp directory or, failing
that, the working directory, so withholding host temp would break them inside
the read-only checkout. `$TMPDIR` users land in the scratch, as do tool
caches a member points there, and it is removed with the context. Managed environment bindings redirect mutable allocations into that scratch.
Writable worktree contexts instead receive their own persistent state and
configuration; only recipe-declared concurrent caches are shared. Contexts without
writable scratch acquire no managed write authority. Ordinary subagents inherit
the session environment. Configuration copies are bounded and never follow links.
On macOS, approved `readPaths` also permit metadata checks on their exact
ancestor directories so Git can resolve linked worktrees from a main checkout.
This permits neither ancestor directory listings nor reads of sibling files,
and grants no writes. This is filesystem isolation, not confidentiality of
repository history. Native file tools retain their context checks even with an explicit process sandbox
opt-out. An unsandboxed shell necessarily retains ambient host authority.

Runtime-private paths inside the source checkout are omitted from snapshot
indexes and excluded from Git staging before file contents are read, including
tracked entries and private directories. This prevents new snapshots from
copying session databases; it does not remove objects already in Git history.

Runtime snapshot, worktree administration, and integration commands use the
same sandbox factory with a trusted Git executable and narrow runtime grants.
For these fixed runtime commands only, preset-generated whole-directory Git
pins are replaced with protections for configuration, hooks, parent indexes,
HEAD, and branch refs. This allows snapshots and worktree administration when
the parent is itself a linked checkout with a common gitdir outside its write
root. The parent and member tool policies retain their original Git pins.
Explicit deny rules, including an extra deny for the same common gitdir, and
global write denial remain in force; this grant is never exposed to a shell,
MCP server, or model-selected command. Read-only reviewers inside Git also use
this runtime snapshot path. Setup errors do not silently switch to live files.
Parent integration authority is bound by the host; children cannot accept or apply
editing changes. `swarm_integrate` accepts and integrates editing work under a runtime-owned exclusive gate against parent tool execution,
preserves its index/branch, rechecks source versions, and records a durable intent
and receipt. Default touched-path preconditions include rename
endpoints, existence, type, Git mode, and content identity; ancestors are checked
without following symlinks. Whole-tree matching is an optional stricter policy.
Integration candidates allocate no checkout and confer no filesystem authority:
resolver/reviewer copies use the ordinary isolated context policy. Cancellation after the write boundary finishes the
apply; lease loss still fences it. Uncertain outcomes require reconciliation. Git 2.40+ and a supported process sandbox (macOS/Linux), or an
explicit unsafe acknowledgment, are required for editing. See
[integration and recovery](WORKFLOWS.md#integrating-editing-results).

Runtime Git and read-only members explicitly expose their selected checkout
paths as frozen read-only grants inside private roots. A denied path deeper
than such a grant still wins, and the grants never make the source checkout
writable. The same grants apply when a member's scratch is its only write
grant; the checkout itself is never writable.

Workflow JavaScript has no direct process/filesystem/network APIs. The workflow
host and agent loop share the Go tool invoker for timeouts, execution gates, and
rich output; each caller checks tool availability and approvals before invocation.
Remote MCP processes cannot be
contained locally and require the operator's explicit `contextIndependent: true`
declaration in the server configuration before being exposed to a member.
Parent workflow integration uses the host's existing parent authority. Scripts
cannot provide an identity or broaden filesystem/tool policy, and generic
`polly.tool` remains bound to isolated contexts. `polly.release` verifies ownership,
per-context inactivity, and unchanged/integrated contents before cleanup. An
unchanged copy is recognized with a cheap Git check (HEAD tree, no
assume-unchanged or skip-worktree flags, empty status); any other copy is captured
in full before removal. The content checks and durable workspace release routine are
shared with ordinary cleanup and automatic reclamation of settled research and
editing workspaces. An active workflow keeps its member reservations; explicit
release can reclaim its own idle copies or settled member workspaces without
changing those reservations. Unintegrated edits or repeated cleanup failures retain
the workspace with a reason; failed release never changes a completed task.
Snapshot refs survive workspace release until `/swarm forget`, after safe cleanup
and resolved integration obligations. Published artifacts follow parent retention;
parent deletion/TTL does not remove Git workspaces or refs. Restoration requires
retained task/execution provenance, never just the member's current context pointer.
Check-copy edits require explicit adoption through an editing task before integration.
Tool metadata and peer messages do not grant additional user authorization.

The bash tool distinguishes sandbox launcher failure from an ordinary command
exit with a target-start acknowledgment descriptor. Only the latter can be
recovered by workflow `exec(check: false)` after output capture completes;
incomplete capture, timeout, cancellation, approval and policy-construction
failures still reject. OS access denials inside a running
command remain that command's exit status; stderr is never parsed to infer an
error category.

Finite commands (bash, shell tools, schema discovery, and indexed search) stop
immediately on cancellation. On macOS, Polly kills the command's private session
process group; on Linux, it kills the owned bubblewrap process and relies on its
PID namespace and parent-death teardown. Unsandboxed and legacy-wrapped Unix
commands get a private process group. Other platforms stop the direct process.
This does not change the lifecycle of long-lived stdio MCP transports.

Capture has one shared one-second drain deadline after cancellation or observed
foreground exit, including the bash startup acknowledgment. A retained pipe
returns partial output with an incomplete-capture error, even when the shell
exits nonzero. Cancellation and deadlines retain their context errors and any
partial output. After foreground exit, Polly closes overdue capture readers
without signaling background jobs. Such jobs can receive `SIGPIPE` on later
writes; jobs with redirected output retain existing behavior where the sandbox
permits it. Deliberately detached sessions can escape process-group cancellation,
but cannot prolong capture beyond the drain limit. Output-size truncation is a
separate memory bound and does not stop draining the command.

Under a sandbox — a launch a policy asked for — the coverage is:

| Execution path | Sandboxed | Opt-out |
|---|---|---|
| Builtin `bash` tool | yes | `--nosandbox` |
| Shell tools (`-t ./tool.sh`) | yes | global `--nosandbox` only |
| Stdio MCP servers | yes (the whole server process) | global `--nosandbox` only |
| Remote MCP servers (HTTP/SSE) | no — the process runs elsewhere | n/a |
| Skill helper / function tools | no — in-process, nothing to wrap | n/a |
| Builtin file tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `view_image`) | policy-checked in-process | `--nosandbox` |
| Automatic `AGENTS.md` loading for default CLI coding turns | policy-checked in-process | `--nosandbox` |

The in-process file operations check every path against the base config
with the same rules: outside a private root a path is readable unless a
denied path covers it; inside one (your home directory, a `denyPaths`
directory) only granted paths are readable; and the deepest rule containing
the path decides. The private `/tmp` and `/run` of the Linux backend are not
in-process roots: file tools read the host's temp and runtime directories
unless a denied path covers them. Writes must land inside the writable
paths and outside `denyWritePaths`, denied paths, and ungranted private
roots. Loops that check many paths against one config (workspace captures,
retained commits, context-file discovery) compile the read policy once with
`sandbox.CompileReadPolicy` and query it per path; the compiled policy
captures route identities and the private roots at compile time, so it is
built at the start of each loop and dropped afterwards, and a frozen grant
replaced while it is in use still fails every query closed. `write_file`
and `edit_file` refuse to load when sandboxing is unavailable unless the
registry opts out. Shell-tool `--schema` discovery is stricter than
execution: private-temp writes only, no network, workspace, or environment
grants; it keeps the read grants that make interpreters under your home
directory reachable.

## Swarm snapshot limits

The runtime captures tracked changes and non-ignored new files through a private
index, preserving the parent's index, index timestamp, HEAD, and branch. Source
roots must be checkouts of the same Git repository. Read-only live roots are used
only outside Git; a denied or broken Git setup fails instead of falling back.

Capture refuses conflicted, sparse, or split indexes, submodules, content filters
(including LFS), symlinked top-level Git metadata, and special files. Repository
and global ignore rules apply. New-file guards default to 32 MiB per file and
256 MiB total in the published tree. Runtime-private paths are excluded before
staging, including tracked private files; existing history is unaffected. External
writers cannot be locked by Polly, so detected inconsistent snapshots are refused.

Member registries omit indexed semantic search. Tools must rebind to the assigned
root or explicitly declare safe context independence; otherwise required tools
fail launch and optional tools are omitted. A snapshot or retained publication is
evidence, not an additional filesystem grant. See
[workspace restoration](WORKFLOWS.md#workspace-release-and-restoration) for resource
lifetime and [follow-ups](WORKFLOWS.md#follow-ups) for source selection.

## Configuration

### CLI presets

`--sandbox <spec>` (`POLLYTOOL_SANDBOX`) picks the base policy from one or
more preset names joined with `+`. The presets and what they mean are
tabulated in [README.md](README.md#sandboxing); in terms of the fields
below they are:

- `base` — `DefaultConfig()`: temp-dir writes only, no network.
- `readonly` — `denyWrite`.
- `workspace` — the working directory in `writablePaths`, with its Git
  metadata in `denyWritePaths` ([whole-tree or leaf
  mode](#workspace-git-protection)).
- `git` — with `workspace`: leaf mode instead of whole-tree. It does
  nothing on its own; `--sandbox git` is rejected with a pointer to
  `workspace+git`.
- `net` — `allowNetwork`.
- `ssh` — `passEnv: ["SSH_AUTH_SOCK"]`, that socket in `allowUnixSockets`,
  and `~/.ssh/config` and `~/.ssh/known_hosts` in `readPaths`.
- `sshkeys` — `~/.ssh` in `readPaths`; writes there stay denied.
- `private-home` — hide home except explicit grants, including the
  [default toolchain grants](#the-private-home-directory).

Ordinary presets permit home reads while keeping credential masks and write limits.

A policy that asks for a sandbox without naming a preset — `--sandbox=` or
`POLLYTOOL_SANDBOX=` with an empty value, a lone `--writepath`, `--allownet` —
gets **`workspace+net+git`**; an empty value never means "no sandbox", which
is what omitting the policy already says. `workspace` canonicalizes the
working directory at startup and refuses roots it cannot safely protect
(the filesystem root, your home directory, mounted-volume roots); change
into a project directory or select `--sandbox base`. Under `base` or
`readonly` the working directory is exposed read-only so tools still see
the project inside private roots. With `private-home`, running from home itself
adds no grant and prints a notice.

### Global flags

`--writepath`, `--readpath`, `--denypath`, and `--allownet` (env
`POLLYTOOL_WRITEPATHS`, `POLLYTOOL_READPATHS`, `POLLYTOOL_DENYPATHS`,
`POLLYTOOL_ALLOWNET`) each ask for a sandbox and overlay its policy;
`--nosandbox` (`POLLYTOOL_NOSANDBOX`) is the explicit spelling of what a
launch does untold. The two refuse to coexist: with no-sandbox mode
effective, an explicitly supplied `--sandbox`, `--denypath`, `--writepath`,
`--readpath`, or `--allownet` is rejected rather than silently ignored;
`--nosandbox=false` overrides an ambient `POLLYTOOL_NOSANDBOX=true`. Polly
warns once for each filesystem root left broadly readable or writable after
policies merge; the home directory itself is never a grant and is rejected.
A policy whose writable paths cover polly's own configuration file
(`~/.pollytool/config`, for example `--writepath ~/.pollytool`) is refused:
the `POLLYTOOL_SANDBOX` line there is what asks for the sandbox, so a command
that edits the file could drop it — or plant `POLLYTOOL_NOSANDBOX` — and the
next start would run unsandboxed. `--nosandbox` is the open way to run
without one, and `/setup`'s Sandbox field saves the choice for later
launches.

`--add-dir <path>` (repeatable) adds an extra read-only directory outside
the workspace — a sibling dependency repo, a vendored checkout, an adjacent
data tree — as a `readPaths` grant at sandbox startup and for the session's
own file tools. It has **no `POLLYTOOL_*` environment default**: extra
read dirs are a per-session surface, never an ambient grant that widens
every session, and the list persists on the session record (`/add-dir` adds
a directory mid-session and lists them with no argument; a resume with
`--add-dir` merges into the stored list). Each entry must exist as a
directory; the filesystem root, the home directory or an ancestor of it,
temp-family roots, paths inside the workspace, and directories that contain
a masked [credential path](#credential-paths-denied-by-default) or lie at or
inside one are rejected, while an ancestor of the workspace is allowed (it
also exposes the workspace's siblings, read-only). Extra directories are for
projects; a credential is granted with `--readpath` or a preset. `--add-dir`
is deliberately accepted with `--nosandbox`: platforms without a sandbox
backend can only run `--nosandbox`, and the entries must still persist and
reach model context there — nothing is enforced, as with all sandboxing on
those platforms.

`--nosandboxprofile` (`POLLYTOOL_NOSANDBOXPROFILE`) leaves the
[workspace profile](#workspace-profiles) out of one launch.

### Workspace profiles

A workspace profile holds your standing sandbox exceptions for one
workspace, so a build that needs a cache outside the project, a token, or
a sibling directory works in every session without flags. It lives at
`~/.pollytool/workspaces/<key>/sandbox.json`, outside the repository and
out of reach of every sandboxed command. The key hashes the repository's
common Git directory, found without running Git, so every worktree and
subdirectory of one repository shares a profile; a directory outside Git
has its own.

`/sandbox` (or `/sandbox show`) lists the items, numbered, with the
credentials they expose and why an item does not apply, such as a path that
no longer exists. `/sandbox allow` adds one:

- `read <path>`: a read grant outside the working directory.
- `write <path>`: a write grant outside the working directory.
- `env NAME=VALUE`: a variable whose value names a path under `@workspace`,
  polly's working directory, or `@cache`, the workspace's own cache
  directory (`~/Library/Caches/pollytool/ws/<key>` on macOS,
  `~/.cache/pollytool/ws/<key>` on Linux, both under `$XDG_CACHE_HOME` when
  it is set). A path you type is stored in that form. An item under `@cache`
  also makes that directory writable, so a tool whose cache the private home
  hides gets one of its own: `/sandbox allow env GOCACHE=@cache/go-build`.
- `passenv NAME [--members]`: a credential-shaped variable the sandbox
  otherwise [strips](#environment-filtering), such as `NPM_TOKEN`.

`/sandbox forget <n|path|NAME|all>` removes items. A change applies to the
session at once, rebuilding its loaded bash and shell tools, and is saved;
other open sessions pick it up when they next open. The model cannot run
slash commands. Only you add host access; during `/sandbox-init`,
`sandbox_prepare` can also persist managed storage and path-valued automatic defaults. You may also edit the file
by hand.

**Trying a command.** `/sandbox try <command>` runs the command as a
[trial](#observing-denials-in-a-trial) and proposes an item for each path the
sandbox denied it; `/sandbox try` alone offers the session's recent failed
bash commands. A proposal names:

- For a read, the path itself, or the program's directory once four reads
  fall inside it.
- For a write, the program's own directory: the entry directly inside the
  home directory, or inside a shared directory (`~/.cache`, `~/.config`,
  `~/.local/share`, `~/.local/state`, `~/Library/Caches`,
  `~/Library/Application Support`, and the like), that holds the path. A
  shared directory is never proposed whole. The sandbox drops a grant of a
  missing path, so a directory that does not exist yet is created, mode
  0700, when you try or allow it. Where the trial cannot tell whether the
  command meant a file or a directory (a new entry directly in the home
  directory, on macOS), it asks you to create it first.

Network access, Unix sockets and denials the policy model cannot explain are
listed with the reason, never proposed. The rules of `/sandbox allow` judge
each proposal, and one they refuse shows why and cannot be ticked.

Every proposal starts unticked: the command is the workspace's own code,
which can draw a denial of any path on purpose, so a trial's proposals are
evidence to weigh, never grants. A credential needs a tick of its own, and a
write carries a warning. If a path's symlink target or credential status
changes after it is proposed, its tick is withdrawn and it needs a new review.
Tick what the workspace needs, run the command
again with the ticked items, and allow them: saved to the profile, or for
this session only, which `/sandbox show` marks and which ends with the
session. Nothing is allowed while a turn is running. The TUI reviews a trial
in a dialog whose keys count only once it is on screen, never inside a
paste, with the Cancel button focused; the plain-text frontend prints the review,
reads answers (`tick 1,3`, `try`, `save`, `session`, `output`, `cancel`), and
allows nothing without a terminal to ask. Linux trials see writes only, so a
Linux review proposes write grants; allow reads with `/sandbox allow read`.

A session-only item overrides a saved item with the same kind and path or
variable name, leaving the file unchanged. Forgetting the session item's
number restores the saved setting; forgetting the path or name removes
both. Saving an item replaces any matching session override too.

**Setting up with `/sandbox-init`.** `/sandbox-init [notes]` activates the
builtin `sandbox-setup` skill. Versioned recipe files carry ecosystem recognition,
version hints, relocation mechanisms, bootstrap guidance and cleanup semantics;
the policy engine contains no ecosystem-specific permission rules. The model
reads project instructions, manifests, lockfiles and CI, inspects existing
toolchains, and prepares predictable state before attempting a build.

- `sandbox_prepare` accepts named `allocations` (name, kind, purpose, recipe,
  optional shared cache flag), path-valued `env` bindings and managed `links`.
  The host chooses paths, validates ownership and authority, creates dedicated
  directories, rebuilds the live policy and persists the profile without a
  review dialog. It executes no installer. Ordinary non-secret configuration is
  written through existing sandboxed file tools. Explicit profile/session
  settings take precedence; conflicts are reported. Recipe names confer no
  authority, and caller-selected host roots are impossible.
- `sandbox_trial` runs a command as a trial and returns its exit code, the
  end of its output, the denials, and the items `/sandbox try` would
  propose. For that one trial it may add `env` items whose value is under
  `@cache` or `@workspace`, since those reach nothing of yours. Every other
  item needs you.
- `sandbox_propose` opens the `/sandbox try` review with the model's items,
  each shown with the model's reason in its own words, beside polly's own
  explanation and warnings. The rules of `/sandbox allow` judge every item,
  every row starts unticked, and you tick, try and allow as with
  `/sandbox try`; the turn waits on your answer, so allowing works while it
  runs. What you allow reaches every sandboxed command, the model's own bash
  included. The model learns what you allowed, left unticked, or polly
  refused, and how each of your trials ended, never their output, which a
  ticked credential may have let the command fill with a secret.

Once the settings are settled, the model runs necessary dependency bootstrap and the final build and test commands
through ordinary sandboxed bash, without trial-only grants, and updates a
`Build and test in Polly's sandbox` section in the workspace's `AGENTS.md`,
creating the file if necessary. It records the exact successful commands,
their working directory, platform, tested tool versions and required settings.
Bootstrap commands are recorded for fresh worktrees; placeholders must be valid
shell syntax with no user-specific absolute paths. A necessary session-only
setting makes reopening incomplete.
Tests that inherently cannot run inside the sandbox are skipped with the test
runner's own filters, and the filtered command must pass with tests actually
executed. Every exclusion names the test and observed limitation; unrelated
failures are reported, never hidden. Other project instructions and full CI
commands are preserved. If no working command is found or the file cannot be
written, setup reports **Incomplete**. Otherwise it reports **Verified** or
**Verified with sandbox exclusions**. Each exclusion requires observed failure
evidence and inspection of the test; compiler errors, assertion failures,
fixable permissions, missing dependencies and absent services never qualify.

The tools exist only in a top-level session after you run `/sandbox-init`, and
never reach sub-agents or swarm members. Two cancelled reviews end the run, and
the tools refuse until the next `/sandbox-init`. They also refuse after the
setup turn ends. `/sandbox-init` needs a session whose sandbox is on, and
polly's skills, which `--noskills` turns off.

The profile is read at every start, TUI and one-shot alike, and applied as
the `workspace-profile` [sandbox layer](#how-policies-merge). Bash, shell
tools, the file tools and sub-agents get all of it. Swarm members get it
too, with three differences: a `passenv` item reaches them only with
`--members`, write grants only when the member may write, and an
`@workspace` value names the member's own worktree. Legacy @cache redirects retain their existing sharing behavior. Managed
allocations carry context metadata: writable members get checkout-specific
state/configuration and only explicitly concurrent caches are shared; read-only
members get scratch-local copies and caches. Ownership metadata stays outside
the sandbox write boundary. Stdio MCP servers and
shell-tool schema discovery get none of it, and under `--nosandbox` it does
not apply at all.

**What an item may not do.** The same rules run when `/sandbox allow` adds
an item and at every start, so a hand-edited profile gets no more than a
typed command. An item that breaks one is refused when added and skipped
with a notice at start. Skipping only narrows the policy, so a profile never
stops polly from starting. A read inside the working directory is skipped
without a notice: every worktree of the repository shares the profile, so a
read one worktree needed is merely redundant in another. `/sandbox show`
still lists it as not applied.

- A path may not be the filesystem root, your home directory or an ancestor
  of it, or reach polly's own state: `~/.pollytool`, polly's cache directory
  apart from the workspace's own, and swarm scratch. A path inside the
  working directory is refused too, since the `--sandbox` preset decides
  those.
- A write may not be at, inside or around a masked credential path, whose
  files name commands the host runs, or a place the host runs code from:
  every `PATH` directory and the install prefix behind it, shell startup
  files, `~/Library/LaunchAgents`, systemd user units and autostart entries,
  and your Git configuration. Nor may it reach the workspace's Git metadata
  or another Git repository. `/sandbox allow write` also looks three levels
  into the directory for a repository, and refuses a directory too large to
  check; that look does not repeat at start. `--writepath` stays your own
  escape hatch for all of these.
- An `env` item may not set a credential-shaped name (pass it with
  `passenv`), polly's own `POLLYTOOL_*` configuration, the shell's own
  environment (`PATH`, `HOME`, ...), the temp directory the sandbox sets, a
  variable that runs startup code or loads code into other programs
  (`BASH_ENV`, `LD_*`, `DYLD_*`, `NODE_OPTIONS`, `PYTHONPATH`, ...), one that
  changes how Git, SSH or GPG run, one that points at a host service
  (`DOCKER_*`, ...), or `XDG_CONFIG_HOME`.
- A `passenv` item may not pass `POLLYTOOL_*` or a socket address
  (`SSH_AUTH_SOCK`, `DOCKER_HOST`, ...; the `ssh` preset is how the agent
  reaches the sandbox). A variable the sandbox does not strip needs none.

**Credentials.** A read at or inside a
[masked credential path](#credential-paths-denied-by-default) and every
`passenv` item expose a credential. That is allowed, since the profile is
yours, and the posture names it like any other exposure. `/sandbox allow`
marks such an item as a credential and records the workspace's `origin`
remote, read from the repository's config file. The item applies only while
the origin stays the same, so a different repository cloned to the same path
does not inherit your token; allow it again to keep it. An item that exposes
a credential without the mark does not apply either, so a directory a
sandboxed command swaps for a link to one never becomes a grant of it.

The file is JSON:

```json
{"version": 1, "workspace": "/Users/you/src/api/.git", "items": [
  {"kind": "read", "path": "~/src/protos"},
  {"kind": "env", "name": "GOCACHE", "value": "@cache/go-build"},
  {"kind": "passenv", "name": "NPM_TOKEN", "members": true, "credential": true,
   "origin": "git@github.com:acme/api.git"}
]}
```

It must be a regular file, never a symlink, that you own and no one else
may write, at most 256 KiB, in a directory that is likewise yours alone. A
file with unknown fields or an unsupported version applies nothing. Version 1
is read unchanged and upgrades to version 2 only on a successful write. Version 2
adds `storage.allocations`, `storage.links`, an `automatic` mark on env items,
and `managed` for explicit bindings to allocated storage. This distinguishes a
legacy `@cache/name` redirect from an allocation with the same name.
`configuration_checkout` identifies the initial configuration seed; new checkout
environments copy its non-secret configuration without overwriting local edits.
The repository identity and existing grants/redirects are preserved. Legacy
storage remains unclassified and excluded from cleanup.

**Managed storage.** @cache allocations live in the existing workspace cache
area under `managed/`, with shared caches separated from checkout caches. @state
and @config live under `$XDG_DATA_HOME/pollytool/environments/<repo>/<checkout>`,
or the platform user-data directory (`~/Library/Application Support` on macOS,
`~/.local/share` on Linux). Subdirectories of one checkout share its environment.
These roots are private even when XDG points outside home. Sandbox writes reach
only allocated directories. Profile/ownership/lock records remain protected
under `~/.pollytool/workspaces/<repo>/`. Records bind allocations to their original
filesystem identities; replaced roots, symlink redirection and existing unowned
directories are rejected. Links connect only managed state and configuration.

Read/modify/write operations use a cross-process profile lock and the existing
transactional policy rebuild, including loaded/staged nested-derived tools.
Validation, rebuild or persistence failure retains the previous effective
settings. Repeated preparation reuses stable names. Configuration is separate
from disposable cache/dependency data, including links for mixed tool homes.

**Storage controls.** Both frontends provide `/sandbox storage`,
`/sandbox clean caches` and `/sandbox reset environment`. Inspection lists paths,
bytes, purposes, recipe provenance, sharing and cleanup categories, identifying
legacy/unmanaged directories separately. Cache cleanup empties only tracked
cache allocations. Reset also empties tracked dependency state; configuration,
declarations and explicit grants survive. Neither removes project node_modules,
.venv, build outputs or existing host installations. Bootstrap is never run by
reset; dependency restoration and verification are required again.

Inspection/cleanup uses background work in both frontends with cancellation on
shutdown. Session lifetime leases block
cross-process cleanup until other sessions close. Cleanup requires an idle
invoking session with its members stopped and gates new local tool execution.
Deletion is confined to owned opened directories, repairs only internal directory
permissions, removes links without following them and preserves root identities.
After cancellation or partial failure, ownership records remain for safe retry.
Forgetting an allocation (or all settings) disables its automatic grant while
retaining cleanup tracking; it can be prepared again by a later `/sandbox-init`.

### The private home directory

Home is **readable by default**. Existing toolchains and ordinary configuration
need no discovery grants. `$HOME` is unchanged, and home writes still require a
grant inside it; a writable ancestor does not grant home writes. Known credential
paths and sensitive environment variables remain filtered. Other projects,
personal files and secrets outside known credential locations are readable.
This is a write restriction and known-credential policy, not home confidentiality.

Polly's `~/.pollytool` runtime directory, managed environment storage roots and
member scratch stay private independently of home. Only explicitly granted
subdirectories (such as the current member worktree, skills or allocated storage)
are visible. Session databases, saved permission records and sibling worktrees
there remain hidden. Custom runtime paths retain their explicit deny rules.
Native commands and in-process file tools apply the same policy.

Use `--sandbox workspace+net+git+private-home` (or `privateHome: true` in a
sandbox config) to retain the stricter policy. Home then becomes a **private
root**: nothing under it is visible except grants. Grants are re-bound at their
real paths on Linux or re-allowed on macOS. Polly storage roots remain traversable
at their own directory entries so tools can reach granted descendants.

With `private-home`, presets grant these read-only, when they exist:

- your global Git configuration (`~/.gitconfig`, `$XDG_CONFIG_HOME/git` or
  `~/.config/git`, or `$GIT_CONFIG_GLOBAL`), every file it includes through
  `include` and `includeIf` (conditions are not evaluated, so an include
  that is inactive here still works in a member's checkout), and the files
  it names as `core.excludesFile` and `core.attributesFile`;
- every `PATH` entry under your home directory, widened to the install
  prefix above a `bin`, `sbin` or `shims` entry (`~/tools/bin` grants
  `~/tools`; `~/.pyenv/shims` grants `~/.pyenv`), so a toolchain's
  libraries, headers and versioned installs come along. A shared root is
  never widened to: not your home (`~/bin` grants only itself), and not a
  directory holding an XDG base directory (`~/.local` holds `~/.local/share`
  and `~/.local/state`, where programs of every kind keep data, history
  and tokens). Such an entry grants itself plus, for each executable in it
  that is a symlink, the install prefix of the link's target
  (`~/.local/bin/python3.13` →
  `~/.local/share/uv/python/cpython-3.13…/bin/python3.13` grants that
  `cpython-3.13…` directory).

These grants are computed without running anything but the trusted Git, and
a candidate inside the credential deny list is never granted. No toolchain's
cache is granted by name in private-home mode: one under home outside a `PATH`
prefix (a module cache such as `~/go/pkg/mod`, `~/.rustup` behind
`~/.cargo/bin` shims, `~/.npm/_cacache`) needs a `--readpath` or
`POLLYTOOL_READPATHS` entry.

The CLI adds the skill directories in use, the remote skill cache, and the
attachment cache, plus anything you name with `--readpath`; per-tool
`readPaths` and `writablePaths` add more. A grant whose spelling routes
through a symlink (`~/.aws -> /mnt/c/Users/you/.aws`) keeps that spelling
usable with the target frozen at startup. With `private-home` on Linux, writes outside a grant land in a per-command
private tmpfs and are discarded; otherwise home writes outside grants are denied. Toolchains that must write under your
home directory (toolchain downloads, package-manager and build caches) need a
`--writepath` there, or an environment variable pointing them at scratch.

When polly runs at the top of a linked worktree or a submodule checkout, the
CLI also exposes, read-only, the Git directories its `.git` file routes to:
the worktree's gitdir and the repository's common directory. Without them no
Git command works in a worktree whose main checkout is inside your home. With private-home, the
main checkout's own files stay hidden, and the `workspace` preset keeps Git
metadata outside the workspace unwritable, so commits from such a worktree
still fail. A denied path covering that metadata wins, and polly prints a
notice that Git fails there. A main checkout needs nothing, since its `.git`
is part of the working directory, and swarm members are granted their own.

### The per-tool `"sandbox"` object

A shell tool schema or MCP server entry may carry a `"sandbox"` field:
`true` means the base policy, an object customizes it, and `false` is
refused unless the caller chose `--nosandbox` / `WithUnsafeNoSandbox`.

| Field | Type | Effect |
|---|---|---|
| `allowNetwork` | bool | allow outbound network access |
| `privateHome` | bool | hide home except granted paths; false by default |
| `denyDNS` | bool | with `allowNetwork`: block DNS on macOS; suppress the default resolver on Linux (best effort) |
| `writablePaths` | string[] | directories where writes are allowed; also readable inside private roots |
| `readPaths` | string[] | paths granted read-only inside private roots (the home directory, `denyPaths` directories); a denied path deeper than the grant still wins |
| `denyPaths` | string[] | read-blocked paths: an existing directory becomes a private root, an existing file is masked; an entry that does not exist yet is still masked in-process and on macOS, and on Linux from the first command after it exists |
| `denyWritePaths` | string[] | paths kept read-only even inside a `writablePaths` entry |
| `allowEnv` | string[] | strict allowlist: if set, *only* these env vars pass through |
| `passEnv` | string[] | additive exemptions from sensitive-var stripping (ignored when `allowEnv` is set) |
| `env` | object | explicit target variables (name → value), applied after filtering, per-call explicit env, and the Linux temp rewrite; a later overlay's value wins per name |
| `allowUnixSockets` | string[] | absolute Unix-socket paths the process may connect to |
| `denyWrite` | bool | deny all writes, even temp; overrides `writablePaths` |
| `denyHostTemp` | bool | withhold the implicit host temp write grant; only `writablePaths` stay writable (Linux keeps its private per-command tmpfs) |

Path fields support `~`. The **base policy** (`sandbox.DefaultConfig()`,
preset `base`) denies writes everywhere except the sandbox temp dir, denies
network, permits home reads, masks Polly storage and the credential deny list,
and strips sensitive env vars. On Linux it also gives the process a private
`/tmp` and `/run`, its own PID and IPC namespaces, dropped capabilities, and
no filesystem Unix sockets.

### How policies merge

The effective config is the base (a preset, or `DefaultConfig`) merged with
the global overlays and the tool's own `"sandbox"` object. Merging is
monotonic: booleans OR and path lists append, so an overlay can add grants
or restrictions but never remove one. Details:

- `denyWrite: true` overrides `writablePaths`. `denyDNS` only matters with
  `allowNetwork`.
- `env` merges per variable name and the later overlay's value replaces the
  earlier one; `privateHome`, `denyHostTemp` and `denyWrite` only ever tighten (boolean OR).
- `allowEnv` is a mode switch: when set, only the listed variables flow and
  `passEnv` is ignored. Prefer `passEnv` to add one variable.
- `~` expands to your home and relative entries resolve against polly's
  working directory. Existing `writablePaths` and `readPaths` grants are
  canonicalized and **frozen to their filesystem identities** when
  `WithSandboxFactory` is created; missing grants are dropped rather than
  activating on a later creation, and a grant later replaced or rerouted
  fails closed.
- Rules are path-scoped and the deepest rule containing a path decides. A
  `readPaths` child of a denied directory (`~/.ssh/config`) is readable
  while its siblings stay hidden; a `denyPaths` entry inside a writable tree
  stays masked; a grant at exactly a denied path wins the tie for reads
  while writes there stay denied. The home directory itself is never a
  grant: `--writepath ~` and `readPaths: ["~"]` are rejected, a temp
  directory equal to it grants nothing, and a writable ancestor
  (`--writepath /Users`) does not make it writable; only grants inside it do.
- `denyWritePaths` entries carve read-only islands out of writable trees
  and must exist on disk; `denyPaths` blocks reads *and* writes and its
  masks are rebuilt every command with symlinks resolved. Writable ancestors
  of a protected entry are pinned against relocation.
- An `allowUnixSockets` entry that isn't a live socket at command time is
  dropped rather than failing the command.
- The extra read directories from `--add-dir` and `/add-dir` append to
  `readPaths` and freeze with the rest. A mid-session `/add-dir` reaches the
  file tools and every later sandbox at once, and rebuilds the loaded bash
  and shell tools under the widened policy; a call already running
  finishes under the old one. A stdio MCP server already running keeps its
  load-time policy: `/add-dir` names such servers, and `/tools restart
  <server>` starts one again under the current policy. The new process
  starts before the old one stops, so a restart that fails leaves the
  running server in place.
  A rebuild the sandbox refuses cancels the whole change, so the tools and
  the policy never disagree.
  A persisted entry whose directory no longer exists is dropped by the
  freeze and grants nothing, but stays on the session record and works
  again after the directory is recreated and the session resumed. Sub-agents,
  swarm members, and worktrees receive the extra dirs as `readPaths` entries
  in their own sandbox configs, so the read-only grant is inherited.
- A **sandbox layer** is a named overlay merged after the base and before a
  tool's own object, in name order, into the sandboxes of bash, shell tools,
  and sub-agents' tools, and into the file tools' own checks, so a file
  tool reaches what a command reaches. It is the one part of the merge that
  can be taken back: replacing or removing a layer mid-session rebuilds the
  loaded and staged bash and shell tools, including tools loaded by derived
  registries, the way `/add-dir` does. A rebuild failure in any registry
  leaves the policy and every tool unchanged. A layer reaches a
  swarm member only through its member part, which the member policy judges
  like the parent's grants; a member keeps the policy it started with. A
  layer never reaches stdio MCP servers (a running server could not be
  narrowed again), shell-tool schema discovery, or the Git that polly runs
  for worktrees, and a credential a layer exposes is named like any other.
  Polly's one layer is the [workspace profile](#workspace-profiles).

### Environment filtering

Sensitive variables are always stripped, matched case-insensitively:

- the prefixes `POLLYTOOL_*` and `AWS_*`;
- names ending in `_API_KEY`, `_APIKEY`, `_TOKEN`, `_SECRET`, `_SECRET_KEY`,
  `_ACCESS_KEY`, `_PASSWORD`, `_PASSPHRASE`, `_CREDENTIALS`, `_PRIVATE_KEY`,
  and those names bare (`TOKEN`, `PASSWORD`, `API_KEY`, ...);
- database credentials: `PGPASSWORD`, `PGPASSFILE`, `MYSQL_PWD`,
  `REDISCLI_AUTH`, `DATABASE_URL`;
- agent sockets and host runtime handles: `SSH_AUTH_SOCK`, `SSH_AGENT_PID`,
  `GPG_AGENT_INFO`, `DBUS_SESSION_BUS_ADDRESS`, `DBUS_SYSTEM_BUS_ADDRESS`,
  `DOCKER_HOST`, `CONTAINER_HOST`, `XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY`,
  `PULSE_SERVER`.

Pass one through with `passEnv` (additive) or `allowEnv` (strict). Agent
sockets are stripped for a reason: with `SSH_AUTH_SOCK` intact a process
can use your SSH keys without ever reading `~/.ssh`. The heuristics are
heuristics: a secret in a variable named `CREDS` passes, so use `allowEnv`
for tools that should see almost nothing.

### Credential paths denied by default

`~/.ssh`, `~/.gnupg`, `~/.gpg`, `~/.aws`, `~/.azure`, `~/.config/gcloud`,
`~/.kube`, `~/.docker/config.json`, `~/.npmrc`, `~/.pypirc`,
`~/.gem/credentials`, `~/.cargo/credentials`, `~/.cargo/credentials.toml`,
`~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.local/share/keyrings`,
`~/Library/Keychains`

These are masked wherever they resolve. Under the private home directory
they are hidden anyway; the masks matter for homes reached through symlinks
(a WSL home pointing into `/mnt/c`) and inside grants of a directory that
contains one. Credentials outside this list — a token in a home dotfile, a `.env` in your
project, a token in `/etc` — are readable by default unless you add
them with `--denypath` or `denyPaths`.

The masks are defaults, not a prohibition. A grant at or inside a masked
path — `--readpath ~/.aws`, the `ssh` and `sshkeys` presets, a tool's
`readPaths` — exposes that credential, because the deeper
rule wins, and `passEnv` or `allowEnv` lets a credential-shaped variable
through. Polly's own automatic grants never do either. Subagents and swarm
members inherit such grants from the parent like any other, and the
parent's `allowUnixSockets` (the `ssh` preset's agent socket) with them,
unless a path the member may not read covers one; shell-tool `--schema`
discovery, which runs a script before it is trusted, gets neither. Every
exposure is named wherever the posture is shown: the TUI masthead, the line
frontends' startup notice, `/set sandbox`, and the tool's `/tools` summary.

### Examples

In a shell tool's `--schema` output, or an MCP server entry:

```jsonc
// Base policy
"sandbox": true

// A fetcher: network plus a cache dir (added to whatever base is selected)
"sandbox": { "allowNetwork": true, "writablePaths": ["~/.cache/fetcher"] }

// A deploy tool: AWS credentials, a strict env allowlist, little else
"sandbox": {
  "allowNetwork": true,
  "writablePaths": ["/tmp/deploy"],
  "readPaths": ["~/.aws"],
  "allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"]
}

// One variable and one agent socket, everything else still filtered
"sandbox": { "passEnv": ["SSH_AUTH_SOCK"], "allowUnixSockets": ["~/.ssh/agent.sock"] }

// Read-only analysis that may read your SSH config
"sandbox": { "denyWrite": true, "readPaths": ["~/.ssh/config"] }
```

## Workspace Git protection

Git metadata is an execution vector: a command that can write `.git/hooks/`
or `.git/config` (`core.hooksPath`, `core.fsmonitor`) runs code on the
*host* the next time you type `git status`. So the `workspace` preset
carves Git metadata back out of the writable tree. Discovery is recursive —
nested repositories, submodules, and linked worktrees are found by
following `.git` routing files and `commondir` pointers — and runs once,
when the preset is parsed. An empty `.git` file (the marker tools such as
`uv` drop in cache directories to stop Git discovery) routes nowhere: it is
pinned read-only like any routing file and otherwise skipped. Any other
malformed routing file still fails the preset.

**Whole-tree mode** (`workspace` without `git`) pins every discovered
`.git` routing entry, gitdir, and common gitdir read-only. Working-tree
files stay editable, but `git commit`, rebase, and fetch fail with `EPERM`;
the LLM-facing bash description says `.git is read-only` so the model
doesn't mistake that for a transient error.

**Leaf mode** (`workspace+git`, the default) keeps `.git` writable and pins
only the metadata that can select host-executed code or reroute the
repository: `config` and `config.worktree`, `hooks/`, and the `.git`
routing file plus `commondir`/`gitdir` pointers — per repository, submodule,
and linked worktree. `index`, `objects/`, `refs/`, and `logs/` stay
writable, so commit/rebase/fetch work while hook-planting and
`core.hooksPath` rewrites stay blocked. Because pinned entries must exist,
leaf mode creates the inert leaves it pins when absent (an empty
`.git/config` and `.git/hooks/`, exactly what `git init` leaves behind); if
it can't, that repository falls back to the whole-tree pin. Metadata the
walk never enters — dormant submodule gitdirs, stale `worktrees/<id>`
entries, bare repositories — stays whole-tree pinned.

Leaf mode's residuals: `modules/` and `worktrees/` stay writable so
`git worktree add` and `git submodule update` work, which means a sandboxed
command can create *new* metadata subtrees there — don't run Git inside a
directory a sandboxed tool created without inspecting it. Data paths
(`objects/`, `refs/`, `HEAD`, `index`) are unpinned; tampering there
corrupts content but cannot execute host code. Config-writing commands
(`git config`, `git remote add`) are blocked by the `config` pin, by design.

**Change tracking.** The file-change diffs the TUI shows for Bash commands come
from `worktree.ChangeTracker`, which runs the trusted Git under the same runtime
posture as worktree administration (`RuntimeGitReadConfig`: the checkout and its
Git routing visible, no network) with a single write grant, its own directory under
`~/.pollytool/changes/`. Snapshots stage into a private index there and write
objects through `GIT_OBJECT_DIRECTORY` with the repository's objects as a read-only
alternate, so no snapshot touches the repository's index, refs, or objects. This
adds no grant to any agent tool. Each observer has a separate mutable index;
object caches are shared under process-held leases, removed when the final
observer closes, and expired after seven unused days following a crash. An
active observer's files are never pruned. Blob reads stream with a 16 MiB total
retained-input limit; larger inputs have explicitly unknown counts. The CLI's
tracked-file baseline is a self-contained pack (at most 64 MiB) owned by the
session artifact store, together with the latest net report. Those artifacts
survive cache cleanup and transcript resets; session deletion or expiry releases
them. The baseline is captured on first open, excludes untracked files, and
never writes to the real Git index. Workspace reports therefore include all
non-ignored untracked files as well as changes to tracked files since baseline.

**Refused layouts.** Some shapes cannot be pinned portably, so the preset
refuses them up front: bare-repository working directories; symlinked Git
metadata (`.git`, config, hooks directory, or a hook file); hard-linked
routing, config, or hook files; and repository-local `core.hooksPath` or
config includes, whose effective target cannot be pinned without evaluating
Git's full configuration (`/dev/null` remains a supported `core.hooksPath`).

**The trusted Git and the config audit.** To evaluate hooks and config,
polly only runs a Git it trusts — the fixed `/usr/bin/git`, or on macOS the
standard Homebrew symlinks when they resolve to a non-writable
`Cellar/git/<version>/bin/git` — with repository-routing variables removed
so its answers match your next host invocation. The audit checks effective
and overridden global/system `core.hooksPath` values and recursively
inspects config includes (even inactive `includeIf` branches), rejecting
hook, config, and include targets that land in host-visible writable
content outside protected Git metadata, config sources with hard-link
aliases, and symlinked or hard-linked entries in hook directories. It
**runs again when each sandbox is constructed**, against the final merged
writable roots, so a later `--writepath` or per-tool `writablePaths` cannot
quietly make an external config or hook target plantable. The trusted Git runs
on the host, as does one other fixed binary: the macOS `log stream` that
[observes a trial's denials](#observing-denials-in-a-trial).

## Platform implementations

Both backends share the policy surface (the `Config` fields) and the
environment filtering, done in Go. The pre-containment wrapper receives an
empty environment; the filtered target environment is installed only after
containment is active. The enforcement mechanisms differ.

### Linux: bubblewrap (`bwrap`)

Each command runs under [bubblewrap](https://github.com/containers/bubblewrap)
in a fresh mount + PID namespace. The default config renders roughly:

```
bwrap \
  --ro-bind / /                          # host read-only
  --tmpfs /home/you                      # with private-home: nothing but grants
  --tmpfs /run                           # hide host runtime sockets
  --tmpfs /tmp                           # private writable temp
  --ro-bind /proc/self/fd/N /home/you/.gitconfig     # read grants, pinned sources
  --ro-bind /proc/self/fd/N /home/you/go/pkg/mod
  --bind /proc/self/fd/N /home/you/src/project       # writable grant
  --ro-bind /proc/self/fd/N /home/you/src/project/.git/config  # deny-write island
  --symlink /mnt/c/Users/you/.aws /home/you/.aws     # granted spelling recreated
  --tmpfs /srv/secrets --ro-bind /dev/null /etc/token # masks outside private roots
  --remount-ro /srv/secrets --remount-ro /run
  --dev /dev --proc /proc
  --unshare-pid --unshare-ipc
  --unshare-net                          # omitted when allowNetwork
  --seccomp FD                           # deny AF_UNIX sockets + io_uring setup
  --cap-drop ALL
  --die-with-parent --new-session
  -- /proc/self/fd/BOOTSTRAP_FD ...      # pinned post-containment bootstrap
```

Mounts are emitted in path-depth order, so every mount lands on top of the
one that contains it and the deepest rule wins.

- **Fixed launcher.** Only the root-owned, non-user-writable
  `/usr/bin/bwrap` is executed; construction fails closed if it's
  unavailable or mutable.
- **Environment after containment.** bwrap gets an empty environment. A
  pinned bootstrap reads a sealed anonymous env descriptor after
  namespaces, mounts, and seccomp are active, then `exec`s the target with
  its exact argv. Target values never appear in bwrap's environment or argv.
  `TMPDIR`, `TMP`, and `TEMP` are rewritten to the private `/tmp`; a policy
  `env` value, such as a member's scratch directory, is applied after that
  rewrite.
- **Writes are physically impossible** outside private temp, the private
  home tmpfs, and writable binds: the root is a read-only mount, not a
  policy check, and capabilities are dropped so a root launcher cannot
  remount. Writes into the private home outside a grant are discarded with
  the namespace.
- **Host runtime state is private.** `/tmp`, `/run`, Polly storage, and (with `private-home`) the home directory
  are fresh mounts, so D-Bus, Docker, SSH-agent, and Wayland sockets are
  absent, and seccomp denies `socket(AF_UNIX)` for sockets elsewhere.
  Anonymous stream and sequenced-packet `socketpair` calls remain available for
  private child IPC (including Rust's process-spawn handshake). These pairs cannot
  disconnect or reconnect to another endpoint. Datagram pairs, AF_VSOCK and
  io_uring socket creation remain denied; creating a Unix socket still needs the
  existing explicit stream-socket grant.
- **Hidden paths read as absent or empty**, not as errors. Nothing under a
  private root exists unless granted, so a host-side creation there cannot
  appear inside the running sandbox. Denied paths outside private roots are
  masked where they exist, with a read-only tmpfs or `/dev/null`, and the
  masks are rebuilt every command. A grant inside a masked directory is
  bound back in; a mask inside a grant sits on top of it.
- **A cwd inside a private root** that no grant covers starts the command
  at `/`; the CLI grants the working directory read-only where needed.
- **`--unshare-pid`** hides other processes' `/proc/<pid>/environ`,
  including polly's own API keys; **`--new-session`** detaches the
  controlling terminal, closing the TIOCSTI keystroke-injection escape.
- `denyDNS` masks `/etc/resolv.conf` and is best-effort only: bubblewrap
  has no port-level filtering, so a hardcoded resolver (`dig @8.8.8.8`)
  still works.

### macOS: Seatbelt (`sandbox-exec`)

Each command runs under `sandbox-exec` with a generated profile, launched
through a fixed root-owned `/usr/bin/perl` bootstrap that reads the
filtered environment from anonymous pipes (over 1 MiB is rejected) and
`exec`s the command, so target values never appear in any intermediate
process. The default config renders:

```scheme
(version 1)
(allow default)                          ; allow-by-default policy
(deny file-write*)                       ; ...except writes are denied
(allow file-write* (literal "/dev/null")); char devices re-allowed (+ zero, random, stdout, stderr)
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/var/folders/.../T"))  ; your real $TMPDIR
(allow file-write* (subpath "/Users/you/src/project"))
(deny file-write* (subpath "/Users/you/src/project/.git/config")) ; deny-write island
(deny file-write-unlink (literal "/Users/you/src/project"))       ; routing pins
(deny file-read* (subpath "/Users/you"))          ; with private-home
(deny file-read* (subpath "/Users/you/.ssh"))     ; ... one deny per denied path
(allow file-read* (subpath "/Users/you/.gitconfig"))   ; grants re-allow, by depth
(allow file-read-metadata (literal "/Users/you"))      ; ancestors: stat only
(allow file-read* (subpath "/Users/you/src/project"))
(deny signal)                            ; can't signal unrelated processes...
(allow signal (target self))             ; ...but a script can manage its own
(allow signal (target same-sandbox))     ;    descendants
(deny network*)
```

Path rules are emitted in path-depth order, so with Seatbelt's
last-match-wins evaluation the deepest rule containing a path decides,
exactly as the Linux mount order does. A working directory the policy hides
(inside the private home with no grant, or masked) starts the command at
`/`, as on Linux; the CLI grants the working directory read-only where
needed.

The command also runs under `setsid()`: its own session, no controlling
terminal.

- **Allow-by-default.** Writes, home-directory and credential reads,
  network, and cross-process signaling are denied; spawning processes,
  enumerating other processes, and Mach services are permitted. The file
  *read* surface matches Linux: the host outside the home directory is
  readable, the home directory only where granted.
- **Denied paths are also write-blocked**, so a broad `writablePaths` such
  as `["~/.local"]` cannot re-open write access to `~/.local/share/keyrings`.
  `readPaths` re-allows reads, never writes. Rules cover both the literal
  path and its symlink-resolved target, since Seatbelt matches resolved
  vnode paths. Ancestors of a grant get metadata-only access so a path can
  be traversed; listing them stays denied.
- **Automatic writable roots**, including the construction-time `TMPDIR`,
  are frozen like configured grants. `denyHostTemp` omits these automatic
  roots, leaving only `writablePaths`.
- `allowNetwork` enables TCP/UDP but still denies outbound Unix-domain
  sockets, except macOS's fixed `mDNSResponder` socket when DNS is enabled;
  `denyDNS` removes that exception and blocks port 53.
- `sandbox-exec` is deprecated by Apple but remains functional (Bazel, Nix,
  and Chromium ride on it); there is no replacement API for ad-hoc
  profiles. Only the fixed root-owned `/usr/bin/sandbox-exec` and
  `/usr/bin/perl` are executed; construction fails closed otherwise.

### Differences at a glance

| | Linux (bwrap) | macOS (Seatbelt) | Unified? |
|---|---|---|---|
| File reads / credentials | host readable, home private except grants, 18-path credential masks | same, as Seatbelt rules | ✅ |
| Writes | read-only root + temp binds | `deny file-write*` + temp allows | ✅ |
| Network | `--unshare-net` | `deny network*` | ✅ |
| Env handling | sealed env FD read by in-namespace bootstrap | anonymous pipes read by in-profile bootstrap | ✅ |
| Cross-process env read | blocked by PID namespace | blocked by the OS (`KERN_PROCARGS2` truncates) | ✅ |
| Signal other processes | invisible | `deny signal` + self/same-sandbox re-allow | ✅ |
| Controlling terminal | detached (`--new-session`) | detached (`setsid()`) | ✅ |
| Granted Unix socket | seccomp admits `AF_UNIX` | exact socket path only | ⚠️ Linux is broader ([why](#limitations)) |
| **Denied-read failure mode** | reads as empty | `Operation not permitted` | ❌ inherent |
| **Process enumeration** | invisible (PID namespace) | visible (`KERN_PROC` sysctl, ungated) | ❌ inherent |
| **Host IPC** | private IPC namespace; host Unix sockets blocked | host Unix sockets blocked; Mach services allowed | ❌ platform gap |

The bold rows are inherent to the platform. The denied-read failure mode
is the one that bites portability, but only for reads and listings: on
macOS `(deny file-read* (subpath ...))` covers metadata too, so an
existence probe such as `[ -f ~/.aws/credentials ]` reports *absent* there
as well (`stat` fails with `Operation not permitted`). The platform
difference is the failure mode of a read or listing — empty on Linux,
`Operation not permitted` on macOS — not whether the path looks present.
Flipping Seatbelt to `(deny default)`
would break most tools, since every syscall, file read, and Mach service
would need an allowlist.

## Observing decisions

A masked directory reads as empty and a stripped variable surfaces as a
mystifying auth failure deep inside a tool, so the sandbox logs what it
does. With `--debug`: one `sandbox_config` line when a tool is loaded and
one `sandbox_wrap` line per command, **names only, never values**:

```
... DBG sandbox_config tool=bash network=false deny_dns=false deny_write=false
        writable_paths="[/tmp]" read_paths=[] deny_paths=[] allow_env=[]
        pass_env=[] allow_unix_sockets=[]
... DBG sandbox_wrap command="bash -c" network=false deny_write=false
        writable_paths="[/tmp]" env_stripped="[OPENAI_API_KEY SSH_AUTH_SOCK]"
        private_roots=3 grants=4 masks=0 unix_sockets=0
```

In the REPL, startup prints a posture line only when the posture is
exceptional: the sandbox is disabled or unavailable, a capable tool runs
unsandboxed, the `ssh` component has no reachable agent, or the policy
exposes a credential (`credentials: ~/.aws/sso, NPM_TOKEN`). `/set sandbox`
always shows the live state, and `/tools list` marks each sandboxed tool with
a policy summary such as `[sandboxed: net off, temp writes, env filtered]`. The model sees
`[sandboxed]` appended to the bash and shell tools' descriptions.

Before every model request, Polly also adds a bounded `<sandbox_context>` summary
of the registry's effective policy, including active layers. It describes home
visibility, filesystem grants and masks, process network/DNS access, Git write
restrictions, and policy-supplied environment variable names, never values.
Lists are sorted and omitted entries are counted. MCP servers and tools with
their own policies may differ from this summary. Explicit unsafe mode is named.
The context is regenerated for the next request after a policy change, including
changes during `/sandbox-init`; it is not stored in conversation history.
Custom system prompts and structured output retain it. Children receive their bound policy,
not a copy of the parent's summary. The context inspector includes its token
cost. This is operational guidance; enforcement remains in the tools and sandbox.

### Observing denials in a trial

A trial (`ToolRegistry.RunTrial` in the library) runs one command with bash
under a candidate policy, the one bash starts from with the candidate merged
over it. It reports what the sandbox denied the command: each path or
address, whether it was a read, a write or network access, and why the
policy refused it. The reasons are a masked credential path, the private
home, a missing write grant, network being off, or a reason Polly does not
model. The candidate reaches only that command, and the session's policy
does not change.

**macOS.** Every deny rule of the trial's profile except the signal rule
carries a random tag, `(with message "polly-<nonce>")`, which changes no
decision. While the trial runs, Polly streams the kernel's reports of that
tag from the system log:

```
/usr/bin/log stream --style ndjson --timeout <n>m \
    --predicate 'sender == "Sandbox" AND eventMessage CONTAINS "polly-<nonce>"'
```

Like the [trusted Git](#workspace-git-protection) of the configuration audit,
this process runs on the host, not in a sandbox. `log` refuses to run under
Seatbelt, and only a live stream sees these reports (`log show` does not keep
them). It is bounded as follows:

- `/usr/bin/log` must be root-owned and not writable by other users.
- It runs with a fixed argument vector, `PATH=/usr/bin:/bin` as its whole
  environment, no input, and its own process group.
- It ends itself a minute after the trial's deadline, or after an hour when
  the trial has none.
- Its output is parsed as data.

A trial's profile also lets the command read the metadata of the home
directory and its shared directories (`~/.cache`, `~/.config`,
`~/Library/Caches`, ...): each entry alone, never its listing or contents,
and never a denied one. A tool that stats `~/.cache` before making
`~/.cache/tool` is otherwise denied the stat, and the denial names
`~/.cache` instead of the directory the tool wanted. A grant of that
directory lets its ancestors be stat'ed anyway, so the trial then fails
where the real command would.

Two canary writes bracket the command. Each is a write into the home
directory through the trial's sandbox, and so always denied:

- Before the command, the stream must report a canary within three seconds.
  Otherwise the trial runs the command anyway and says its denials are
  unavailable. Non-admin accounts are untested, and this is how an account
  whose stream stays silent shows up.
- After the command, a second canary marks the end of its reports.

Denials every process draws are dropped: dyld's DTrace helper, the missing
controlling terminal, and CoreFoundation's text encoding and Apple preference
files.

**Linux.** bubblewrap keeps no record of what it refused. With readable home,
trials return command output and an observation limitation, without scanning
home. With `private-home`, a command's writes into the private home succeed into its tmpfs and are discarded when the
command ends. The trial therefore lists the home directory before and after
the command, from inside the sandbox and on the tmpfs only, so grants bound
into it are skipped. It reports each new tree as one discarded write, at the
tree's root or at the directory that a chain of single new directories leads
to (a tool's new `~/.cache/tool`, not `~/.cache`), and says whether that is
a directory or a file. The listing reaches Polly
through a descriptor the command does not inherit.

Linux trials do not observe reads of hidden paths, which fail as missing
files, or writes refused elsewhere.

**Other platforms** observe nothing.

**Evidence, not authority.** The command runs with the trial policy's full
reach, so it can draw a denial on purpose or avoid one. A trial shows what a
command reached for. Anything that proposes grants from a trial must still
judge each grant on its own.

## Limitations

- **Same user, same kernel.** Not a VM or a user-namespaced container.
  Anything your user can do that isn't explicitly denied, the sandbox can
  do.
- **macOS is allow-by-default.** Process enumeration and Mach services stay
  reachable ([differences](#differences-at-a-glance)), and `/private/tmp`
  is shared because Seatbelt has no mount namespace; `denyHostTemp` withholds
  it for policies that grant explicit writable paths only.
- **No resource limits.** A sandboxed fork bomb is still a fork bomb.
- **A granted agent socket is a signing oracle.** `allowUnixSockets` and the
  `ssh` preset let a prompt-injected command sign with your SSH agent while
  it runs, in the session and in every swarm member it starts. Prefer
  `ssh-add -c` for per-use confirmation.
- **Linux socket grants are broader than macOS.** `connect()` cannot be
  path-filtered in seccomp, so once any grant is active, Unix sockets
  outside the private `/tmp`/`/run` roots (a Docker socket under
  `~/.docker/run`, say) also become connectable. Grant sockets
  deliberately.
- **The `ssh` preset does not grant `known_hosts` writes.** First contact
  with an unknown host fails inside the sandbox; connect once outside, or
  manage `known_hosts` on the host.
- **GPG-signed commits fail** even under `workspace+git`, since no
  gpg-agent socket is granted; disable signing in the sandbox or sign on
  the host.
- **Grants are frozen at startup.** With `private-home`, a file created later under home
  outside a grant is invisible; `@file` references and context
  files must sit under the working directory or a granted path. Automatic
  `AGENTS.md` discovery stops at the first ungranted ancestor.
- **On Linux, denied paths outside private roots are masked only once they
  exist.** The in-process policy and macOS mask a `--denypath` entry whether
  or not it exists (so creating it is refused too); Linux rebuilds its
  masks every command and covers the entry from the first command after it
  appears. Private-home mode additionally hides ungranted home paths.

### Session storage is private to the host

The CLI and managed TUI add the actual session database, its `-wal` and `-shm`
sidecars, and the default disk-promotion destination to ordinary tool deny paths
before shell schema loading or stdio MCP startup. Under the default location the
database sits inside the explicitly private `~/.pollytool` directory, in both
home modes.
When `--store` points elsewhere the deny entries mask the existing files, and a
sidecar created later is masked from the next command on. Native file tools and
sandboxed processes inherit these restrictions. Host session storage and scoped
workflow/task/artifact inspection remain available. Explicit `--nosandbox`
retains its existing unrestricted process semantics.

### `/sandbox-init` iteration limit

`/sandbox-init` runs for at most 20 model calls (or the agent's smaller
configured limit). At the limit it stops as Incomplete, persists completed
messages and tool results, and retains settings already saved. Commands or AGENTS.md may still need work.
Ordinary conversations retain their usual limit. This bounds loop iterations,
not elapsed time or the number of commands in one tool call.

On Linux with readable home, trials run normally but cannot automatically observe
denials. They return command output and an explicit observation limitation. The
private-home write report remains available only in private-home mode; Polly
never scans the real readable home to construct that report.
