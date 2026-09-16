# Sandboxing

Polly runs LLM-driven commands — the builtin `bash` tool, shell tools, and
stdio MCP servers — inside an OS-level sandbox by default. This is the
reference: threat model, every configuration field, the workspace Git
protection, and how the Linux and macOS backends differ. The CLI summary is
in [README.md](README.md#sandboxing); library wiring in
[API.md](API.md#sandboxing-in-the-library).

**Contents:** [Intent](#intent) ·
[What gets sandboxed](#what-gets-sandboxed) ·
[Configuration](#configuration) ·
[Workspace Git protection](#workspace-git-protection) ·
[Container backend](#container-backend) ·
[Platform implementations](#platform-implementations) ·
[Observing decisions](#observing-decisions) · [Limitations](#limitations)

## Intent

The commands polly runs are chosen by a language model, often steered by
untrusted input. The sandbox limits the blast radius of a hallucinated,
prompt-injected, or buggy command. By default a sandboxed process sees
nothing of your home directory beyond [what is granted](#the-private-home-directory),
cannot read [credential paths](#credential-paths-denied-by-default) anywhere,
cannot see [credential-shaped environment variables](#environment-filtering),
cannot write outside the [writable set](#the-per-tool-sandbox-object), and
cannot reach the network or host Unix sockets.

The sandbox is **default-on** (tool metadata cannot opt out; only the
caller's `--nosandbox` / `WithUnsafeNoSandbox` can), **fails closed** (no
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
Every member context owns a private scratch directory (`$TMPDIR`, also
`GOCACHE` and `GOTMPDIR`): beside its checkout inside the slot, or, for a member
observing the live tree, one of the `scratch/live-NNNN` directories in the
runtime directory. A read-only member writes there and in host temp, nowhere
else. Siblings can neither read nor write it: the runtime directory is a
private root of every member policy and only the member's own checkout and
scratch are granted back inside it, so a sibling started later is invisible
without any new rule. Slot directories and their scratch are created on
demand. Cleanup restores owner access to read-only directories inside scratch
(including Go module caches) before removing them, without traversing
symlinks to external files.

Member policies hide the parent checkout, the runtime directory, every other
context's root, and the session database as private roots, and deny every
write to common Git metadata and the linked worktree's `.git` entry. The
common Git object store and the user's Git configuration stay readable. A
read-only member's checkout is listed in `denyWritePaths` on top of the
missing write grant. Host temp stays writable for it as for every context:
macOS's bash 3.2 puts here-documents in a system temp directory or, failing
that, the working directory, so withholding host temp would break them inside
the read-only checkout. `$TMPDIR` users and Go builds land in the scratch,
which is removed with the context.
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
roots. `write_file`
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

Every preset also carries the [default home grants](#the-private-home-directory).

The default is **`workspace+net+git`**. `workspace` canonicalizes the
working directory at startup and refuses roots it cannot safely protect
(the filesystem root, your home directory, mounted-volume roots); change
into a project directory or select `--sandbox base`. Under `base` or
`readonly` the working directory is exposed read-only so tools still see
the project inside the private home; from your home directory itself
nothing is exposed and polly prints a notice.

### Global flags

`--writepath`, `--readpath`, `--denypath`, and `--allownet` (env
`POLLYTOOL_WRITEPATHS`, `POLLYTOOL_READPATHS`, `POLLYTOOL_DENYPATHS`,
`POLLYTOOL_ALLOWNET`) overlay every sandboxed tool's policy; `--nosandbox`
(`POLLYTOOL_NOSANDBOX`) disables sandboxing. When no-sandbox mode is
effective, an explicitly supplied `--sandbox`, `--denypath`, `--writepath`,
`--readpath`, or `--allownet` is rejected rather than silently ignored;
`--nosandbox=false` overrides an ambient `POLLYTOOL_NOSANDBOX=true`. Polly
warns once for each filesystem root left broadly readable or writable after
policies merge; the home directory itself is never a grant and is rejected.

### The private home directory

Your home directory is a **private root** on both platforms: a sandboxed
process sees nothing under it except explicit grants, so credentials,
dotfiles, other projects, the session database, and every swarm member's
workspace are hidden without any rule naming them. Grants are re-bound at
their real paths (Linux) or re-allowed (macOS), so tools see the same paths
inside and outside the sandbox. `$HOME` is passed through unchanged.

Every preset grants these read-only, when they exist:

- your global Git configuration (`~/.gitconfig`, `$XDG_CONFIG_HOME/git` or
  `~/.config/git`, or `$GIT_CONFIG_GLOBAL`), every file it includes through
  `include` and `includeIf` (conditions are not evaluated, so an include
  that is inactive here still works in a member's checkout), and the files
  it names as `core.excludesFile` and `core.attributesFile`;
- every `PATH` entry under your home directory, widened to the install
  prefix above a `bin`, `sbin` or `shims` entry (`~/tools/bin` grants
  `~/tools`; `~/.pyenv/shims` grants `~/.pyenv`), so a toolchain's
  libraries, headers and versioned installs come along; an entry directly
  under your home (`~/bin`) grants only itself.

These grants are computed without running anything but the trusted Git, and
a candidate inside the credential deny list is never granted. A toolchain
that lives under your home but not beneath a `PATH` prefix (a Go module
cache at `~/go/pkg/mod` when only `/usr/local/go/bin` is on `PATH`,
`~/.rustup` behind `~/.cargo/bin` shims) needs a `--readpath` or
`POLLYTOOL_READPATHS` entry.

The CLI adds the skill directories in use, the remote skill cache, and the
attachment cache, plus anything you name with `--readpath`; per-tool
`readPaths` and `writablePaths` add more. A grant whose spelling routes
through a symlink (`~/.aws -> /mnt/c/Users/you/.aws`) keeps that spelling
usable with the target frozen at startup. On Linux, writes under the home
directory outside a grant land in a per-command private tmpfs and are
discarded; on macOS they are denied. Toolchains that must write under your
home directory (`GOTOOLCHAIN` downloads, package-manager caches) need a
`--writepath` there, or an environment variable pointing them at scratch.

### The per-tool `"sandbox"` object

A shell tool schema or MCP server entry may carry a `"sandbox"` field:
`true` means the base policy, an object customizes it, and `false` is
refused unless the caller chose `--nosandbox` / `WithUnsafeNoSandbox`.

| Field | Type | Effect |
|---|---|---|
| `allowNetwork` | bool | allow outbound network access |
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
network, keeps the home directory private, masks the credential deny list,
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
  earlier one; `denyHostTemp`, like `denyWrite`, only ever tightens.
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
`~/.gem/credentials`, `~/.cargo/credentials`, `~/.config/gh`, `~/.netrc`,
`~/.git-credentials`, `~/.local/share/keyrings`, `~/Library/Keychains`

These are masked wherever they resolve. Under the private home directory
they are hidden anyway; the masks matter for homes reached through symlinks
(a WSL home pointing into `/mnt/c`). Credentials outside this list and
outside your home directory — a `.env` in your project, a token in `/etc` —
are readable unless you add them with `--denypath` or `denyPaths`.

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
when the preset is parsed.

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
quietly make an external config or hook target plantable.

## Container backend

The container backend runs every tool of an agent loop inside a container
instead of under the OS sandbox: bash, shell tools, the file tools,
`view_image`, stdio MCP servers and skill scripts. The container is the
whole filesystem a tool can see, so the host home directory, its credential
paths and every other checkout are absent rather than masked. It is the
intended default where an image is configured and a daemon answers; the
native backends remain for everything else.

### Selection

`--sandbox-backend` (`POLLYTOOL_SANDBOX_BACKEND`) is `auto`, `native` or
`docker`; `--sandbox-image` (`POLLYTOOL_SANDBOX_IMAGE`) names the image.
Under `auto`, nothing configured means native tools with no notice; an
image configured but a daemon that does not answer, or a policy the
container cannot honor (`ssh`, `sshkeys`, `allowUnixSockets`), means native
tools with a startup notice naming the reason; an image that is not present
on the daemon fails the start, because polly never pulls. Explicit `docker`
fails closed in every one of those cases, the way `sandbox requested but
unavailable` does for the OS backends. `--nosandbox` never selects a
container. A repository's `.polly/image` file is a hint printed at startup,
never a selection: a checkout could name any image.

The daemon is reached through the Engine API on `DOCKER_HOST`, the active
docker context, or the default socket; `unix://` and `tcp://` (with the
usual TLS variables) work, `ssh://` does not. No `docker` process runs on
the host for tool execution. The one exception is `polly sandbox build`,
an explicit management command that runs the docker CLI to build a
reference image and never runs in a conversation.

### What the container sees

`--sandbox-mode` (`POLLYTOOL_SANDBOX_MODE`) is `bind` for a local daemon
and `copy` otherwise under `auto`:

- **Bind mode** mounts the worktree read-write at its own host path, so
  host tools, the TUI, `@file` references and Git capture agree on what a
  path means. A linked worktree's shared Git directory is mounted read-only
  and its own entry read-write; the metadata the policy protects (`config`,
  `hooks`, the routing pointer) is mounted read-only over itself; the
  scratch directory is mounted read-write and exported as `TMPDIR` and the
  Go cache like the native scratch; skill directories are mounted
  read-only. Nothing else from the host is mounted. A read-only member
  gets a read-only worktree and a writable scratch. A denied read inside a
  mounted tree cannot be honored and fails the start.
- **Copy mode** keeps a self-contained repository in an anonymous volume:
  the start ships a bundle of the base commit (a parentless commit of the
  checkout's tree, so no history travels) and the checkout's uncommitted
  changes. After every call of a tool that can write, the helper collects
  the copy's changed, added, untracked and deleted non-ignored paths and
  the host applies them to the worktree, so host Git capture always sees
  the current tree; ignored files never cross. A host-side write to the
  checkout (an integration, an apply, a snapshot restore) is followed by a
  resync that resets the copy to the new base and reapplies the host's
  changes. A sync that fails returns a `sync_failed` tool error saying the
  container still holds the edit; the next successful sync or resync
  reconciles it.

Containers run as the host user with a read-only root filesystem, a tmpfs
`/tmp` and a tmpfs home under `/run/polly`, every capability dropped,
`no-new-privileges`, a PID limit (4096 by default) and the memory and CPU
limits `POLLYTOOL_SANDBOX_MEMORY`, `POLLYTOOL_SANDBOX_CPUS` and
`POLLYTOOL_SANDBOX_PIDS` set. Network follows the policy: none when
denied, and an empty resolver when allowed without DNS. Git identity
inside is `user.name`, `user.email` and `commit.gpgsign=false` written
from host `git config` values; credential helpers, includes and signing
keys are not copied. The environment a tool receives is the image's plus
the policy's `env` and the values of the names `passEnv` or `allowEnv`
select, sealed into the helper's stream rather than the container's
configuration, so they appear in no `docker inspect`; sensitive-name
stripping still applies inside.

### Lifetime

One container per agent loop: the parent, each swarm member, each
standalone or subagent run. Containers carry labels (`polly.session`,
`polly.root`, `polly.mode`, `polly.image`, `polly.protocol` and the
limits and mount set) and those labels are the only record of them;
nothing is written to session records. An open looks the container up by
session and root, starts it if stopped and reconnects to it, or destroys it
on any label mismatch and creates a fresh one. Closing a standalone run
destroys its container; a swarm member's survives parking, is destroyed
when the coordinator releases its workspace, and is reconnected after a
polly restart. `polly sandbox prune` removes containers whose session no
longer exists; startup never reaps on its own. Anything a tool started in
the background dies with the container.

### Images

An image is named, never built implicitly, and must already be present on
the daemon. `polly sandbox build <variant>` writes one of the reference
Dockerfiles under `docker/` (`base` with git, bash, coreutils and polly;
`go`, `node` and `python` adding a toolchain) to `~/.pollytool/docker/build`
and builds it with the docker CLI as `polly/<variant>:latest`; `--print`
shows the commands instead. The helper inside the image must speak the
same protocol as the host build; `POLLYTOOL_SANDBOX_HELPER` names a host
Linux polly binary to mount as the helper for development or for an image
that predates a protocol change. Images must work as a non-root user and
provide `sleep` from coreutils as the container's init.

### Boundary

The Docker socket is root-equivalent on the host. The container backend
keeps tools away from the host filesystem and environment; it does not
defend the host against the daemon. Unix-socket grants, the `ssh` presets
and agent forwarding are unsupported in this version.

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
  --tmpfs /home/you                      # private home: nothing but grants
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
- **Host runtime state is private.** `/tmp`, `/run`, and the home directory
  are fresh mounts, so D-Bus, Docker, SSH-agent, and Wayland sockets are
  absent, and seccomp denies `socket(AF_UNIX)` for sockets elsewhere.
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
(deny file-read* (subpath "/Users/you"))          ; private home
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
| File reads / credentials | host readable, home private except grants, 17-path credential masks | same, as Seatbelt rules | ✅ |
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

### Containers: the helper's sandbox

With the container tool backend, tools run inside a container that polly's
own helper process hosts: `polly sandbox helper`, a hidden command started
by `docker exec` on the host, serves a native tool registry over its stdin
and stdout, and the host holds one proxy per tool. Inside the container the
helper's registry uses a `containerSandbox` that keeps the environment
discipline of the OS backends (host-selected values sealed into the hello
exchange, ambient filtering by name, explicit per-tool values, the scratch
variables) and starts every command in its own session so cancellation
kills the whole process group, but applies no filesystem or network rules:
the container's mounts, read-only root, dropped capabilities and network
mode are the boundary. It is constructible only after
`sandbox.EnterHelperMode`, which that one command calls, and a registry
built outside helper mode cannot obtain it. This is the one sanctioned path
that runs a child process outside bubblewrap or Seatbelt. The other host
process the backend ever starts is the docker CLI under `polly sandbox
build`, an explicit management command.

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
unsandboxed, or the `ssh` component has no reachable agent. `/set sandbox`
always shows the live state, and `/tools list` marks each sandboxed tool with
a policy summary such as `[sandboxed: net off, temp writes, env filtered]`. The model sees
`[sandboxed]` appended to the bash and shell tools' descriptions.

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
  it runs. Prefer `ssh-add -c` for per-use confirmation.
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
- **Grants are frozen at startup.** A file created later under your home
  directory outside a grant is invisible; `@file` references and context
  files must sit under the working directory or a granted path. Automatic
  `AGENTS.md` discovery stops at the first ungranted ancestor.
- **On Linux, denied paths outside private roots are masked only once they
  exist.** The in-process policy and macOS mask a `--denypath` entry whether
  or not it exists (so creating it is refused too); Linux rebuilds its
  masks every command and covers the entry from the first command after it
  appears. The private home covers the realistic cases.

### Session storage is private to the host

The CLI and managed TUI add the actual session database, its `-wal` and `-shm`
sidecars, and the default disk-promotion destination to ordinary tool deny paths
before shell schema loading or stdio MCP startup. Under the default location the
database sits inside the private home directory and is invisible structurally.
When `--store` points elsewhere the deny entries mask the existing files, and a
sidecar created later is masked from the next command on. Native file tools and
sandboxed processes inherit these restrictions. Host session storage and scoped
workflow/task/artifact inspection remain available. Explicit `--nosandbox`
retains its existing unrestricted process semantics.
