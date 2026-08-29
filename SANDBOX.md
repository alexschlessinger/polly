# Sandboxing

Polly runs LLM-driven commands — the builtin `bash` tool, schema-defined
shell tools, and stdio MCP servers — inside an OS-level sandbox by default.
This document is the sandbox reference: the threat model, every
configuration field, the workspace Git protection, and how the Linux and
macOS implementations differ. The CLI-facing summary lives in
[README.md](README.md#sandboxing); library wiring in
[API.md](API.md#sandboxing-in-the-library).

**Contents:** [Intent](#intent) ·
[What gets sandboxed](#what-gets-sandboxed) ·
[Configuration](#configuration) ·
[Workspace Git protection](#workspace-git-protection) ·
[Platform implementations](#platform-implementations) ·
[Observing decisions](#observing-decisions) ·
[Limitations](#limitations)

## Intent

The commands polly executes are chosen by a language model, often steered by
untrusted input (web pages, file contents, MCP tool output). The sandbox
limits the blast radius of a hallucinated, prompt-injected, or simply buggy
command. By default a sandboxed process cannot:

- **Read credentials.** `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.kube`, and the
  rest of the [built-in deny list](#credential-paths-denied-by-default) are
  blocked from reads, plus anything added with `--denypath`, unless an
  effective `readPaths` grant exempts an entry.
- **See secrets in the environment.** Credential-shaped variables
  (`POLLYTOOL_*`, `AWS_*`, `*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`,
  ...) and agent sockets (`SSH_AUTH_SOCK`, `GPG_AGENT_INFO`) are stripped
  before exec unless explicitly delivered or allowlisted. Agent sockets
  matter: with `SSH_AUTH_SOCK` intact, a process can use your SSH keys
  without ever reading `~/.ssh`. The `ssh` preset re-admits exactly
  `SSH_AUTH_SOCK` (via `passEnv`) and the one agent socket it names (via
  `allowUnixSockets`), so agent-based SSH works while private keys stay
  masked.
- **Write beyond the effective writable set.** The filesystem is read-only
  except the OS temp dir and configured `writablePaths`. The CLI default
  adds the current workspace; the library `DefaultConfig` does not. Git
  metadata under a writable workspace is carved back out — see
  [Workspace Git protection](#workspace-git-protection).
- **Reach TCP/UDP unless the effective policy allows it.** Library
  `DefaultConfig` denies network access. The CLI deliberately defaults to
  `workspace+net+git`, so CLI tools can use outbound TCP/UDP unless the
  operator selects a tighter preset. Host filesystem Unix sockets remain
  blocked even when TCP/UDP is enabled, except any socket an
  `allowUnixSockets` grant names.

Design principles:

- **Default-on.** Tools don't opt in. Tool metadata cannot opt out by
  itself; the caller must explicitly select `--nosandbox` /
  `WithUnsafeNoSandbox`.
- **Fail closed.** If no backend exists, or a sandbox fails to construct,
  the tool errors instead of silently running unsandboxed. At startup polly
  probes the backend with a trivial command and aborts with a pointer to
  `--nosandbox` if it can't actually start processes.
- **Track the live filesystem.** Deny lists and profiles are rebuilt on
  every command, so a credential dir created mid-session (`aws configure` in
  another terminal) is masked by the next command. On Linux, denied
  directory entries are also reserved when a long-lived process starts, so
  later host-side creation or replacement cannot appear inside that existing
  namespace.
- **Observable.** Every decision is visible under `--debug` (variable and
  path names only — never values), and the REPL reports posture at startup
  and via `/get sandbox`.

This is containment for accidents and injection, not a hostile-binary jail:
the process runs as your user on a shared kernel. See
[Limitations](#limitations).

## What gets sandboxed

| Execution path | Sandboxed | Opt-out |
|---|---|---|
| Builtin `bash` tool | yes | `--nosandbox` |
| Shell tools (`-t ./tool.sh`) | yes | global `--nosandbox` only |
| Stdio MCP servers | yes (the whole server process) | global `--nosandbox` only |
| Remote MCP servers (HTTP/SSE) | no — the process runs elsewhere | n/a |
| Skill helper / function tools | no — in-process, nothing to wrap | n/a |
| Builtin file tools (`view_image`, `read_file`, `list_dir`, `search_files`, `write_file`, `edit_file`) | policy-checked in-process | `--nosandbox` |

The builtin file tools run in-process, so there is no child to wrap; instead
they check every path against the base sandbox config before touching it —
reads against the read policy (deny list minus `readPaths` exemptions),
writes against the write policy (writable paths minus `denyWritePaths`
islands and the credential deny list) — so they cannot see or change what a
sandboxed command could not. `write_file` and `edit_file` refuse to load at
all when sandboxing is unavailable unless the registry opts out with
`WithUnsafeNoSandbox` (the CLI's `--nosandbox`).

Shell-tool `--schema` discovery is deliberately stricter than execution.
Before the schema can be trusted, discovery runs with private-temp writes
only and no network, workspace, read-path, or environment grants. Base
`denyPaths`, `denyWritePaths`, and `denyWrite` restrictions are retained.

## Configuration

### CLI presets

`--sandbox <spec>` (`POLLYTOOL_SANDBOX`) picks the base policy every
sandboxed tool starts from. A spec is one or more preset names joined with
`+`:

| Preset | Policy |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | `denyWrite` — nothing writable, not even temp |
| `workspace` | working directory writable; discovered Git routing entries and metadata trees pinned read-only |
| `git` | with `workspace`: pin only the dangerous Git metadata leaves instead of whole trees, so commit/rebase/fetch work; requires `workspace` |
| `net` | `allowNetwork` |
| `ssh` | agent-based SSH: pass `SSH_AUTH_SOCK`, allow connecting to that socket, read `~/.ssh/config` and `~/.ssh/known_hosts`; private keys stay masked |
| `sshkeys` | read all of `~/.ssh`, private keys included (agentless setups); `~/.ssh` writes stay denied |

The default is **`workspace+net+git`** — agentic work on the current project
with network access and working Git, while credentials stay masked and the
rest of the filesystem stays read-only. Tighten with `--sandbox base` or
`--sandbox readonly` when tools only need to compute or inspect.

`git` selects *how* the workspace protects Git metadata and does nothing on
its own — `--sandbox git` is rejected with a pointer to `workspace+git`.

`workspace` canonicalizes the working directory at startup and refuses roots
it cannot safely protect: the filesystem root, the user's home directory,
exact mounted-volume roots on Linux and macOS, and Linux's exact private
temp/runtime sandbox roots — recursively protecting Git metadata there
cannot safely use a partial scan. Descendants of mounted volumes remain
valid bounded workspaces; otherwise change into a project directory or
select `--sandbox base`.

### Global flags

| Flag | Env | Effect |
|---|---|---|
| `--sandbox <spec>` | `POLLYTOOL_SANDBOX` | select the base preset |
| `--writepath <dir>` (repeatable) | `POLLYTOOL_WRITEPATHS` | extra writable paths for all sandboxed tools |
| `--denypath <path>` (repeatable) | `POLLYTOOL_DENYPATHS` | extra read-blocked paths for all sandboxed tools |
| `--allownet` | `POLLYTOOL_ALLOWNET` | allow outbound network for all sandboxed tools |
| `--nosandbox` | `POLLYTOOL_NOSANDBOX` | disable sandboxing entirely |

For a conversation run, when no-sandbox mode is effective, polly rejects an
explicitly supplied `--sandbox`, `--denypath`, `--writepath`, or
`--allownet` instead of silently ignoring that policy. Pass
`--nosandbox=false` to override an ambient `POLLYTOOL_NOSANDBOX=true` and
restore sandboxing.

After global and per-tool policies are merged, polly visibly warns once for
each home directory or filesystem root that remains a broad writable grant.
The warning points to the global and per-tool settings to inspect; read-only
policies and matching `denyWritePaths` do not trigger it.

### The per-tool `"sandbox"` object

A shell tool schema or MCP server entry may carry a `"sandbox"` field:
`true` means "sandbox with the base policy", and an object customizes it. A
`"sandbox": false` request is refused unless the caller also made the
explicit global unsafe choice (`--nosandbox` / `WithUnsafeNoSandbox`) — tool
metadata cannot disable the sandbox by itself.

| Field | Type | Default | Effect |
|---|---|---|---|
| `allowNetwork` | bool | `false` | allow outbound network access |
| `denyDNS` | bool | `false` | with `allowNetwork`: block DNS on macOS; on Linux, suppress the default resolver only (best effort) |
| `writablePaths` | string[] | `[]` | directories where writes are allowed (supports `~`) |
| `readPaths` | string[] | `[]` | paths exempted from the read deny list (supports `~`) |
| `denyPaths` | string[] | `[]` | extra read-blocked paths on top of the built-in deny list (supports `~`) |
| `denyWritePaths` | string[] | `[]` | paths kept read-only even inside a `writablePaths` entry (supports `~`) |
| `allowEnv` | string[] | unset | strict allowlist: if set, *only* these env vars pass through |
| `passEnv` | string[] | `[]` | additive exemptions from the sensitive-var stripping (ignored when `allowEnv` is set) |
| `allowUnixSockets` | string[] | `[]` | absolute Unix-socket paths the process may connect to while broad Unix-socket access stays blocked (supports `~`) |
| `denyWrite` | bool | `false` | deny all file writes, even temp; overrides `writablePaths` |

**Base policy** (library `sandbox.DefaultConfig()`, CLI preset `base`):

- Writes: denied everywhere except the sandbox temp dir (`/tmp` is a private
  tmpfs on Linux)
- Network: denied
- Reads: all files accessible except the credential deny list
- Env: all vars passed through except sensitive ones
- Linux extras: private `/tmp` and `/run`, inherited capabilities dropped,
  filesystem Unix sockets denied, own PID and IPC namespaces, own session

### How policies merge

The effective config for a tool is the caller's base config (a CLI preset,
or `DefaultConfig` in the library) merged with the global overlays
(`--denypath`, `--writepath`, `--allownet`) and the tool's own `"sandbox"`
object. Merging is monotonic: booleans OR and path lists append, so an
overlay may add grants or restrictions but can never remove an earlier
entry. Grant fields such as `allowNetwork`, `writablePaths`, and `readPaths`
widen access; `denyWrite`, `denyDNS`, `denyPaths`, `denyWritePaths`, and a
strict `allowEnv` add restrictions.

Interactions worth knowing:

- `denyWrite: true` silently overrides `writablePaths` (and makes
  `denyWritePaths` redundant).
- `denyDNS` only matters when `allowNetwork` is true (no network ⊃ no DNS).
  macOS blocks the resolver service and direct port 53; Linux masks the
  default resolver configuration, but a process that hardcodes a resolver
  (`dig @8.8.8.8`) still resolves names.
- `allowEnv` is a mode switch, not an addition: when set, *only* the listed
  variables pass through, and `passEnv` is ignored so a strict allowlist
  stays strict. Prefer `passEnv` when you only want to add one variable
  (`"passEnv": ["SSH_AUTH_SOCK"]`) rather than re-enumerating `HOME`/`PATH`
  under `allowEnv`.
- An `allowUnixSockets` entry that is not a live socket at command time is
  dropped, never failing the command — a dead agent degrades to "cannot
  reach the agent", not a broken sandbox — and never lifts a credential deny
  that covers it.
- A missing or unresolvable `denyWritePaths` entry fails sandbox
  construction and is checked again before every command; neither backend
  can reliably reserve a nonexistent protected object. On both platforms,
  writable ancestors of a protected entry are pinned against relocation, so
  moving an ancestor cannot expose a replacement at the guarded path.

### Path semantics

Policy paths are normalized when the sandbox is constructed:

- `~` expands to the user's home directory, relative entries resolve against
  polly's working directory at that moment, and empty entries are rejected.
- Existing `writablePaths` and `readPaths` grants are canonicalized and
  **frozen to their filesystem identities**. Missing grants are dropped
  rather than becoming active after a later creation, and a grant that is
  later replaced or rerouted fails closed before the backend runs.
- A `readPaths` entry may name a child of a denied directory (for example
  `~/.ssh/config`): only that child is restored, and the rest of the denied
  parent remains hidden. An entry that traverses a symlink keeps its
  approved lexical route, but both the route and the canonical target are
  revalidated before each command, so replacement or retargeting fails
  closed.
- `denyWritePaths` entries carve read-only islands out of writable trees;
  `denyPaths` blocks reads *and* writes.
- Deny paths are re-checked on every command, and symlinked entries are
  resolved to their real targets before masking.
- `PrepareConfig` carries the approved filesystem identities privately
  through `Config.Merge`. A tool registry prepares its base before any
  process-backed tool or schema sandbox starts (`WithSandboxFactory`
  snapshots it when the option is created) and reuses those identities for
  later lazy tool construction, so replacing or rerouting an approved object
  fails before the sandbox factory is called.

### Environment filtering

Sensitive variables are always stripped, even without `allowEnv`, matched
case-insensitively:

- the prefixes `POLLYTOOL_*` and `AWS_*`;
- names ending in `_API_KEY`, `_APIKEY`, `_TOKEN`, `_SECRET`, `_SECRET_KEY`,
  `_ACCESS_KEY`, `_PASSWORD`, `_PASSPHRASE`, `_CREDENTIALS`, `_PRIVATE_KEY`
  — and those same names bare (`TOKEN`, `PASSWORD`, `API_KEY`, ...);
- database credential carriers: `PGPASSWORD`, `PGPASSFILE`, `MYSQL_PWD`,
  `REDISCLI_AUTH`, `DATABASE_URL`;
- agent sockets and host runtime handles: `SSH_AUTH_SOCK`, `SSH_AGENT_PID`,
  `GPG_AGENT_INFO`, `DBUS_SESSION_BUS_ADDRESS`, `DBUS_SYSTEM_BUS_ADDRESS`,
  `DOCKER_HOST`, `CONTAINER_HOST`, `XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY`,
  `PULSE_SERVER`.

To pass one through, add it to `passEnv` (additive — everything else still
flows) or list it in `allowEnv` (strict — *only* the listed names flow). The
`ssh` preset uses `passEnv` for `SSH_AUTH_SOCK`. Remember the heuristics are
heuristics: a secret in a variable named `CREDS` passes the suffix patterns,
so use `allowEnv` for tools that should see almost nothing.

### Credential paths denied by default

`~/.ssh`, `~/.gnupg`, `~/.gpg`, `~/.aws`, `~/.azure`, `~/.config/gcloud`,
`~/.kube`, `~/.docker/config.json`, `~/.npmrc`, `~/.pypirc`,
`~/.gem/credentials`, `~/.cargo/credentials`, `~/.config/gh`, `~/.netrc`,
`~/.git-credentials`, `~/.local/share/keyrings`, `~/Library/Keychains`

`--denypath` (env `POLLYTOOL_DENYPATHS`) adds global entries for all
sandboxed tools; per-tool `denyPaths` adds local ones. Credentials living
outside this list — a `.env` in your project, a token in a config file the
list doesn't know — are readable unless you add them.

### Examples

Sandbox a shell tool with the base policy (in the tool's `--schema` output):

```json
{
  "title": "my_tool",
  "type": "object",
  "sandbox": true,
  "properties": { ... }
}
```

A fetcher that needs the network and a cache directory. The overlay adds to
whatever base the caller selected: with `--sandbox base` the effective
writable set is temp plus this cache; with the CLI default it also includes
the workspace:

```json
"sandbox": {
  "allowNetwork": true,
  "writablePaths": ["~/.cache/fetcher"]
}
```

Network with DNS blocked on macOS and the default resolver suppressed on
Linux (best effort — a hard-coded resolver remains reachable on Linux):

```json
"sandbox": { "allowNetwork": true, "denyDNS": true }
```

A deploy tool that needs AWS credentials, specific env vars, and little
else:

```json
"sandbox": {
  "allowNetwork": true,
  "writablePaths": ["/tmp/deploy"],
  "readPaths": ["~/.aws"],
  "allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"]
}
```

Pass one variable through without a strict allowlist, and let the tool reach
one agent socket while others stay blocked:

```json
"sandbox": { "passEnv": ["SSH_AUTH_SOCK"], "allowUnixSockets": ["~/.ssh/agent.sock"] }
```

A read-only analysis tool that may read your SSH config but can't write at
all, not even temp:

```json
"sandbox": { "denyWrite": true, "readPaths": ["~/.ssh/config"] }
```

A stdio MCP server with one credential explicitly passed through:

```json
{
  "mcpServers": {
    "github": {
      "command": "github-mcp-server",
      "sandbox": {
        "allowNetwork": true,
        "allowEnv": ["GITHUB_TOKEN", "HOME", "PATH"]
      }
    }
  }
}
```

Block a project's secrets from every sandboxed tool:

```sh
polly --denypath ~/work/secrets --denypath ~/.config/myapp ...
```

## Workspace Git protection

A writable workspace almost always contains a Git repository, and Git
metadata is an execution vector: a command that can write `.git/hooks/` or
`.git/config` (`core.hooksPath`, `core.fsmonitor`) gets to run code on the
*host* the next time you type `git status` outside the sandbox. So the
`workspace` preset carves Git metadata back out of the writable tree, in one
of two modes.

Discovery is recursive: regular repositories, nested repositories,
submodules, and linked worktrees are all found, following `.git` routing
files and `commondir` pointers to the real metadata. It runs once, when the
workspace preset is parsed.

### Whole-tree mode (`workspace` without `git`)

Every discovered `.git` routing entry, per-worktree gitdir, `commondir`
pointer, and common gitdir is pinned read-only. This closes replacement and
alternate-config paths at the cost of blocking *all* Git metadata writes:
working-tree files stay editable, but `git commit`, rebase, and fetch fail
with `EPERM`. The LLM-facing bash description says `.git is read-only` so
the model doesn't mistake the failure for a transient error. Missing leaves
such as `config.worktree` are covered by the whole-tree pin.

### Leaf mode (`workspace+git` — the default)

Leaf mode keeps `.git` writable and pins only the metadata that can select
host-executed code or reroute the repository:

- `config` and `config.worktree`
- `hooks/`
- the `.git` routing file and the `commondir`/`gitdir` pointers

— per repository, submodule gitdir, and linked worktree. `index`,
`objects/`, `refs/`, `logs/`, and `COMMIT_EDITMSG` stay writable, so
commit/rebase/fetch work while hook-planting and `core.hooksPath` /
`core.fsmonitor` rewrites stay blocked.

Because `denyWritePaths` entries must exist on disk, leaf mode **creates the
inert leaves it pins when they are absent**: an empty `.git/config` and
`.git/hooks/` (exactly what `git init` leaves behind), and an empty
`.git/config.worktree` only when `extensions.worktreeConfig` is effectively
enabled (Git ignores the file otherwise). If a leaf cannot be created — a
read-only `.git`, say — that one repository falls back to the whole-tree pin
and logs `sandbox_git_leaf_fallback`; the sandbox still starts and is never
weaker than whole-tree mode.

Metadata the workspace walk never enters stays whole-tree pinned: dormant
(registered but not checked-out) submodule gitdirs under `modules/`, stale
or external `worktrees/<id>` entries, and a common gitdir outside the
workspace. Bare repositories inside the workspace are always whole-tree
pinned. Ancestor pinning makes the pinned leaves freeze `.git` itself
against rename or recreation on both platforms.

Leaf mode's honest residuals:

- The `modules/` and `worktrees/` container directories stay writable so
  `git worktree add` and `git submodule update` work — which means a
  sandboxed command can create *new* metadata subtrees there, just as it can
  already create a fake `anydir/.git/` in the writable worktree. Don't run
  Git inside a directory a sandboxed tool created without inspecting it.
- Data-level paths (`objects/`, `refs/`, `packed-refs`, `logs/`, `HEAD`,
  `index`, `info/attributes`, `objects/info/alternates`) are unpinned:
  tampering there corrupts history or content but cannot execute host code,
  and the equivalent is already reachable through writable worktree files
  such as `.gitattributes`.
- The reverse `worktrees/<id>/gitdir` pointer *is* pinned because a
  legitimate `git worktree repair` writes through it. A retargeted pointer
  does not yield an arbitrary-file clobber in current Git (repair validates
  the pointer and refuses), so the pin is defense-in-depth against routing
  corruption rather than a known write primitive.
- Config-writing commands (`git config`, `git remote add`, `git submodule
  init/deinit`, `git maintenance start`) are blocked by the `config` pin —
  by design.
- An existing effective `core.hooksPath` or config include that targets an
  unpinned location *inside* `.git` fails closed under leaf mode where
  whole-tree mode accepted it (the target is no longer covered). Drop the
  `git` component to fall back to `workspace+net`.

### Refused layouts

Some repository shapes cannot be pinned portably, so the workspace preset
refuses them up front rather than protecting them halfway:

- **Bare-repository working directories.**
- **Symlinked Git metadata** — a symlinked `.git`, config,
  `config.worktree`, hooks directory, or hook file: neither backend can pin
  the link if a merged policy later makes its target writable.
- **Hard-linked protected files** — routing, config, or hook files with
  hard-link aliases: protecting the pathname would not protect the writable
  alias.
- **Repository-local `core.hooksPath` and config includes** — the effective
  redirected target cannot be pinned without evaluating Git's full
  configuration environment. (`/dev/null` remains a supported
  `core.hooksPath` value for disabling hooks.)

### The trusted Git and the config audit

To evaluate hooks and config safely, polly only runs a Git it trusts:

- PATH-selected Git is accepted when it reaches the fixed `/usr/bin/git`
  through a stable non-symlink route outside writable paths.
- On Darwin, the standard Homebrew `/opt/homebrew/bin/git` and
  `/usr/local/bin/git` leaf symlinks are also accepted when they resolve
  directly to a non-writable, single-link `Cellar/git/<version>/bin/git`
  target outside every sandbox-writable path.
- Polly executes the *resolved* selected Git with repository-routing
  variables removed (including the `git config`-only `GIT_CONFIG` override),
  preserving that Git's compiled config-prefix semantics so its answers
  match your next host Git invocation. Relevant global/system config
  selectors and ordinary-command overrides remain in place.

The audit checks effective and overridden global/system `core.hooksPath`
values and recursively inspects config includes, even inactive `includeIf`
branches. Before containment, it rejects:

- hook targets, selected config sources, and include targets that land in
  host-visible writable content outside protected Git metadata — including
  absent targets, and the macOS host temp trees that stay writable under
  Seatbelt;
- existing config sources with hard-link aliases;
- symlinked or hard-linked entries in configured hook directories.

Discovery's private policy record survives `Config.Merge`, and the
config/include/hook audit **runs again when each sandbox is constructed**,
against the final merged set of host-visible writable roots — so a later CLI
`--writepath` or per-tool `writablePaths` overlay cannot quietly make an
external config, include, or hook target plantable. (Linux's exact private
`/tmp` and `/run` mounts are excluded from that host-persistence check;
explicitly rebound descendants and wider roots remain covered.)

## Platform implementations

Both platforms share the same policy surface (the `Config` fields) and
ambient environment filtering in Go. The pre-containment wrapper receives an
empty environment; the filtered target environment, including explicitly
configured values, is installed only after containment is active. The
enforcement mechanisms are otherwise very different.

(Library callers wrapping an `exec.Cmd` themselves must use the managed wrap
API — `WrapCmdManaged` and its cleanup contract are covered in
[API.md](API.md#sandboxing-in-the-library).)

### Linux: bubblewrap (`bwrap`)

Each command is re-written to run under [bubblewrap](https://github.com/containers/bubblewrap)
in a fresh mount + PID namespace. The default config renders roughly:

```
bwrap \
  --ro-bind / /                          # entire filesystem read-only
  --tmpfs /tmp                           # private writable temp
  --tmpfs /run --remount-ro /run         # hide host runtime sockets
  --tmpfs /home/you/.ssh                 # denied dirs masked with empty tmpfs
  --ro-bind /dev/null /home/you/.netrc   # denied files masked with /dev/null
  ...                                    # one mask per denied path
  --dev /dev                             # fresh devtmpfs
  --proc /proc                           # proc scoped to the new PID namespace
  --unshare-pid                          # own PID namespace
  --unshare-ipc                          # own SysV/POSIX IPC namespace
  --unshare-net                          # no network (omitted when allowNetwork)
  --seccomp FD                           # deny AF_UNIX sockets + io_uring setup
  --cap-drop ALL                         # discard inherited launcher capabilities
  --die-with-parent
  --new-session                          # detach from the controlling tty
  -- /proc/self/fd/BOOTSTRAP_FD ...      # pinned post-containment bootstrap
```

Properties worth knowing:

- **The launcher has a fixed trusted route.** Polly executes only the
  regular, root-owned, non-user-writable `/usr/bin/bwrap`; a workspace or
  temp-directory `bwrap` earlier on `PATH` is never selected. Construction
  fails closed if the fixed backend is unavailable or mutable.
- **Target environment is installed only after containment.** bwrap itself
  gets a non-nil empty environment. The parent passes a pinned descriptor
  for its own executable plus a sealed anonymous NUL-delimited environment
  descriptor; after namespaces, mounts, and seccomp are active, the internal
  bootstrap closes those descriptors and directly `exec`s the original
  target with its exact argument vector and filtered environment. Target
  environment values never appear in bwrap's environment or argv.
- **Writes are physically impossible** outside private temp and writable
  binds — the root is a read-only mount, not a policy check. Inherited
  capabilities are dropped, so a capability-bearing root launcher cannot
  remount those binds writable.
- **Host runtime state is private.** `/tmp` and `/run` are fresh mounts, so
  D-Bus, Docker/Podman, SSH-agent, Wayland, and similar host sockets are
  absent. Private roots are installed after writable ancestor binds, so a
  workspace or explicit write grant cannot cover them and reveal the host
  temp/runtime tree. A seccomp rule also denies `socket(AF_UNIX)` so sockets
  elsewhere in the broad read-only filesystem view cannot be reached.
  Private AF_UNIX stream `socketpair()` remains available for descendant
  IPC; reconnectable datagram and seqpacket pairs are denied. With
  networking disabled, socket creation is denied for every address family
  except that private stream pair.
- **Denied paths read as empty**, not as errors: a masked directory lists
  nothing (tmpfs), a masked file reads zero bytes (`/dev/null`). Tools
  probing for credentials see "not configured" rather than "blocked".
- **`--unshare-pid`** means `/proc` only shows the sandboxed process tree.
  Without it, any same-UID process's `/proc/<pid>/environ` is readable —
  including polly's own `POLLYTOOL_*` API keys, which would defeat the env
  stripping.
- **`--new-session`** detaches the controlling terminal, closing the classic
  TIOCSTI escape (a sandboxed process injecting keystrokes into your shell).
- **Symlinked deny paths are resolved** to their real targets before masking
  (bwrap can't mount over the link itself, and masking the link wouldn't
  cover the target). Resolved duplicates are masked once.
- **Child read exemptions are bind-backed read-only.** For example,
  `readPaths: ["~/.ssh/config"]` keeps the parent `~/.ssh` mask and restores
  only `config`; sibling keys remain hidden.
- **Missing deny paths are reserved before they are masked.** bwrap cannot
  create a mount destination below the read-only host root, so polly first
  gives the nearest existing parent a private, snapshotted directory view
  with the denied entry omitted, then creates the empty-tmpfs or `/dev/null`
  mask inside that private view. A host process cannot make a later
  creation, replacement, or symlink retarget appear inside the
  already-running sandbox. Existing denied leaves receive the same masks
  directly. Unexpected resolution or snapshot errors fail the command
  closed. When the reserved parent is itself writable, existing allowed
  children remain host-backed, but new direct siblings are created in the
  private snapshot rather than on the host.
- A `writablePaths` entry that doesn't exist is dropped when the sandbox is
  constructed, rather than bound — bwrap aborts on a missing bind source,
  and failing construction would brick session restore over one stale path.
  A path created later does not acquire authority on a subsequent command.
- `denyDNS` (with `allowNetwork`) is implemented by masking
  `/etc/resolv.conf` with `/dev/null`. **This is weaker than the macOS
  equivalent and is best-effort only:** bubblewrap has no port-level network
  filtering (the net namespace is all-or-nothing, and seccomp-BPF can't
  inspect the destination port behind the `connect()` sockaddr pointer), so
  a process that hardcodes a resolver (`dig @8.8.8.8`) still resolves names.
  It stops the libc default resolver, not a determined one. True DNS egress
  control on Linux would need a userspace network proxy (netns +
  slirp/pasta), which is out of scope. On macOS the port-53 block below is
  enforced by the kernel policy.

### macOS: Seatbelt (`sandbox-exec`)

Each command runs under `sandbox-exec` with a generated profile. A fixed,
root-owned `/usr/bin/perl` bootstrap reads the filtered target environment
from anonymous, prefilled pipe descriptors, clears its own environment,
closes the descriptors, and directly `exec`s the original command. Large
environments are split across nonblocking pipe shards whose writers are
closed before wrapping returns, so no named filesystem object or background
writer exists. Target values therefore never appear in the pre-containment
wrapper environment or any intermediate process argv. Framed target
environments larger than 1 MiB are rejected before pipe allocation. The
default config renders:

```scheme
(version 1)
(allow default)                          ; allow-by-default policy
(deny file-write*)                       ; ...except writes are denied
(allow file-write* (literal "/dev/null"));  char devices re-allowed
(allow file-write* (literal "/dev/zero")); (+ random, urandom, stdout, stderr)
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/var/folders/.../T"))  ; your real $TMPDIR
(deny file-read* (subpath "/Users/you/.ssh"))
; ... one deny rule per denied path, 17 built-in ...
(deny signal)                            ; can't signal unrelated processes...
(allow signal (target self))             ; ...but a script can manage its own
(allow signal (target same-sandbox))     ;    descendants (anything in this sandbox)
(deny network*)
```

The command also runs with `setsid()` (via `SysProcAttr.Setsid`), giving it
its own session and detached terminal — see the terminal note below.

Properties worth knowing:

- **Allow-by-default.** Unlike the Linux side, Seatbelt here is a targeted
  deny: writes, credential reads, network, and cross-process signaling are
  blocked; the rest (spawning processes, *enumerating* other processes, mach
  services) is permitted. Note this mostly affects process and IPC
  operations, **not** file reads — Linux's read-only root bind also exposes
  the whole filesystem readable, and both platforms deny the same credential
  list, so the read surface matches.
- **Signaling is denied for unrelated processes.** `(deny signal)` plus a
  self/`same-sandbox` re-allow lets a script manage its own descendants
  (timeouts, background workers, even children it detaches into their own
  session) but blocks it from `kill`-ing or `SIGSTOP`-ing your other
  processes. `same-sandbox` scopes by sandbox membership, so it covers
  descendants regardless of process group and doesn't depend on the
  `setsid()` below. It's the macOS approximation of the isolation Linux gets
  for free from the PID namespace (where other processes are simply
  invisible).
- **Own session, no controlling terminal.** `setsid()` is the macOS
  counterpart to bwrap's `--new-session`: it detaches the controlling tty,
  closing terminal-injection vectors.
- **Denied reads fail loudly**: `cat ~/.ssh/config` returns
  `Operation not permitted`, where Linux would return empty.
- **Denied paths are also write-blocked.** Each credential path gets a
  `deny file-write*` rule after the `writablePaths` allows, so a broad
  `writablePaths` (e.g. `["~"]`) can't re-open write access to `~/.ssh` or
  `~/.aws` — a sandboxed process can neither read nor plant credentials
  there. On Linux this is structural instead: the tmpfs/`/dev/null` mask
  sits over the writable bind, so writes land on the ephemeral overlay, not
  the real file. `readPaths` re-allows reads for exempted paths but never
  writes.
- **Rules are emitted for both the literal path and its symlink-resolved
  target.** Seatbelt matches resolved vnode paths, so a rule on a
  dotfiles-managed symlink (`~/.npmrc -> ~/dotfiles/npmrc`) would never fire
  for the real file; naming both closes the hole.
- **Last-match-wins** is how `readPaths` exemptions work: they're emitted as
  `allow file-read*` rules after the denies.
- Deny rules are emitted whether or not the path exists (harmless, unlike
  bwrap), so no existence filtering is needed.
- A missing `writablePaths` entry is dropped when the sandbox is
  constructed, so it is tolerated without leaving an inert grant that could
  activate after a later creation.
- Automatic writable roots, including the construction-time `TMPDIR`, are
  also frozen; changing the parent process environment before a later
  command does not add a new write grant.
- `allowNetwork` enables TCP/UDP but still denies outbound Unix-domain
  sockets, preventing access to host Docker, VM, agent, and service
  endpoints. The one exception is macOS's fixed `mDNSResponder` socket,
  re-allowed only when DNS is enabled; `denyDNS` removes that exception and
  also blocks direct port 53.
- `sandbox-exec` is deprecated by Apple but remains functional and is what
  Bazel, Nix, and Chromium-family tooling ride on; there is no supported
  replacement API for ad-hoc profiles.
- The Darwin backend executes only fixed `/usr/bin/sandbox-exec` and
  `/usr/bin/perl` routes. Both must be regular, root-owned executables that
  non-root users cannot modify. Construction fails closed if either trusted
  component is unavailable.

### Differences at a glance

| | Linux (bwrap) | macOS (Seatbelt) | Unified? |
|---|---|---|---|
| File reads / credentials | whole fs readable, 17-path deny list | whole fs readable, same deny list | ✅ effect matches |
| Writes | read-only root + temp binds | `deny file-write*` + temp allows | ✅ effect matches |
| Network | denied (`--unshare-net`) | denied (`deny network*`) | ✅ effect matches |
| Env handling | shared filtering; pinned in-namespace bootstrap reads a sealed anonymous env FD and directly `exec`s the target | shared filtering; fixed in-profile bootstrap reads anonymous pipe shards and directly `exec`s the target | ✅ effect matches |
| Cross-process env read | blocked by PID namespace | blocked by the OS (`KERN_PROCARGS2` truncates) | ✅ effect matches |
| Signal other processes | invisible, can't signal | `deny signal` + self/same-sandbox re-allow | ✅ effect matches |
| Controlling terminal | detached (`--new-session`) | detached (`setsid()`) | ✅ effect matches |
| Missing `writablePaths` | dropped at construction | dropped at construction | ✅ tolerated, cannot activate later |
| Granted Unix socket (`allowUnixSockets`) | seccomp admits `AF_UNIX` stream + bind into private root; sockets outside private roots also become reachable | profile authorizes the exact socket path only | ⚠️ effect matches for the grant; Linux is broader (see below) |
| **Denied-read failure mode** | reads as empty | `Operation not permitted` | ❌ inherent (masking vs policy) |
| **Process enumeration** | invisible (PID namespace) | visible (`KERN_PROC` sysctl, ungated) | ❌ inherent |
| **Host IPC** | private IPC namespace; host Unix sockets blocked | host Unix sockets blocked; Mach services allowed (allow-default) | ❌ platform gap |
| Mechanism / backend | namespaces, maintained | kernel policy, `sandbox-exec` deprecated | ❌ inherent |

The bold rows are the only genuinely operator-visible gaps that remain, and
each is inherent to the platform rather than a config choice:

- **Denied-read failure mode** is the one that can actually bite
  portability: a tool doing `[ -f ~/.aws/credentials ]` sees *absent* on
  Linux but *present-but-unreadable* on macOS. Unifying it would mean
  changing one platform's masking mechanism.
- **Process enumeration** isn't gateable on macOS — `KERN_PROC` sysctl isn't
  covered by `process-info*`. A sandboxed process can *list* your processes
  (but no longer signal or read the env of them).
- **Host IPC** Unix sockets are blocked on both platforms, including when
  TCP/UDP is enabled. Mach services remain visible on macOS. Flipping
  Seatbelt to `(deny default)` would break most tools because every syscall,
  file read, and Mach service would need an allowlist, so it remains
  allow-default.

## Observing decisions

A masked directory reads as empty and a stripped variable surfaces as a
mystifying auth failure deep inside a tool, so the sandbox logs what it
does. With `--debug`, one `sandbox_config` line when a tool is loaded (the
effective merged config) and one `sandbox_wrap` line per command:

```
... DBG sandbox_config tool=bash network=false deny_dns=false deny_write=false
        writable_paths="[/tmp]" read_paths=[] deny_paths=[] allow_env=[]
        pass_env=[] allow_unix_sockets=[]
... DBG sandbox_wrap command="bash -c" network=false deny_write=false
        writable_paths="[/tmp]" env_stripped="[OPENAI_API_KEY SSH_AUTH_SOCK]"
        denied_paths=17 unix_sockets=0
```

A dropped socket grant (dead or replaced agent) logs a
`sandbox_unix_socket_grant_dropped` line naming the path and reason.

`env_stripped` and all path fields log **names only, never values**. Linux
also strips host-runtime hints such as `DBUS_SESSION_BUS_ADDRESS`,
`DOCKER_HOST`, and `XDG_RUNTIME_DIR`. `command` is truncated to the first
two argv entries.

In the REPL:

- Startup prints a posture line:
  `sandbox: active (base; 3 tools sandboxed; not sandboxed: weather)` — or
  `sandbox: disabled (--nosandbox)`.
- `/get sandbox` shows the live state:
  `active (preset: base; denypaths: 2; tools: 3 sandboxed, 1 not: weather)`.
- `/tools list` marks each sandboxed tool with a policy summary such as
  `[sandboxed: net off, temp writes, env filtered]`, and
  `/tools show <name>` includes a `sandboxed: true/false` line.

The LLM also sees it: sandboxed shell tools get `[sandboxed]` appended to
their description, so the model knows writes and network may be restricted.

## Limitations

Know what this does *not* do:

- **Same user, same kernel.** This is not a VM or a container with its own
  user namespace. The kernel attack surface is fully exposed, and anything
  your user can do that isn't explicitly denied, the sandbox can do.
- **macOS is allow-by-default.** A sandboxed process can still read every
  non-denied file you can, *enumerate* your other processes, and talk to
  mach services — though it can no longer signal them, read their
  environments, or hold your terminal. Treat the macOS sandbox as
  credential/write/network containment with partial process isolation, not
  the full PID-namespace isolation Linux provides.
- **macOS temp is shared.** Linux uses a private tmpfs for `/tmp`; Seatbelt
  cannot provide a mount namespace, so `/private/tmp` remains shared on
  macOS.
- **The deny list is a list.** Credentials living outside the 17 built-in
  paths (a `.env` in your project, a token in a config the list doesn't
  know) are readable unless you add them with `--denypath` or `denyPaths`.
- **No resource limits.** CPU, memory, disk-in-temp, and process count are
  unconstrained; a sandboxed fork bomb is still a fork bomb.
- **Env heuristics are heuristics.** A secret in a variable named `CREDS`
  passes the suffix patterns. Use `allowEnv` for tools that should see
  almost nothing.
- **A granted agent socket is a signing oracle.** `allowUnixSockets` (and
  the `ssh` preset) let a sandboxed process *use* your SSH agent while it
  runs — it never reads the key, but a prompt-injected command can sign with
  it. Prefer `ssh-add -c` (per-use confirmation) if that matters, and
  remember the grant only exists while an agent is running.
- **Linux socket grants are broader than macOS.** `connect()` cannot be
  path-filtered in seccomp, so once any `allowUnixSockets` grant is active,
  Unix sockets that sit *outside* the private `/tmp`/`/run` roots (e.g. a
  Docker socket under `~/.docker/run`) also become connectable. macOS
  authorizes only the exact granted path. Grant sockets deliberately.
- **The `ssh` preset does not grant `known_hosts` writes.** First contact
  with an unknown host fails host-key verification inside the sandbox;
  connect once outside, or manage `known_hosts` on the host. `ControlMaster`
  sockets and other `~/.ssh` writes are likewise denied.
- **`git commit` needs the `git` component.** Under plain `workspace` the
  whole `.git` tree is read-only and commits fail with `EPERM`; the default
  `workspace+net+git` fixes this. GPG-signed commits still fail (no
  gpg-agent socket is granted) — disable signing in the sandbox or sign on
  the host.
