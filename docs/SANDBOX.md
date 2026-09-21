# Sandboxing

Polly can restrict tool commands and file access with native Linux and macOS
sandboxes. **Sandboxing is opt-in. Home is readable by default; writes and known
credentials are restricted.**

```sh
polly --sandbox default               # workspace + network + Git
polly --sandbox workspace             # workspace writes only
polly --sandbox readonly              # no writes or network
polly --sandbox default+private-home  # hide ungranted home paths
```

## Contents

- [When sandboxing is enabled](#when-sandboxing-is-enabled)
- [Presets](#cli-presets)
- [Paths and home access](#paths-and-home-access)
- [Build setup](#build-setup)
- [Workspace profiles](#workspace-profiles)
- [Storage and cleanup](#storage-and-cleanup)
- [Credentials and environment](#credentials-and-environment)
- [Tool configuration](#tool-configuration)
- [How policies merge](#how-policies-merge)
- [What gets sandboxed](#what-gets-sandboxed)
- [Agents and snapshots](#agents-and-snapshots)
- [Git protection](#workspace-git-protection)
- [Platform differences](#platform-implementations)
- [Trials and diagnostics](#observing-denials-in-a-trial)
- [Limits](#limitations)
- [Implementation and tests](#implementation-and-tests)

[Documentation index](README.md) · [CLI guide](CLI.md) ·
[Library setup](API.md#sandboxing-in-the-library)

## When sandboxing is enabled

A preset, path grant, path denial, or network grant asks for a sandbox. These can
come from flags, `POLLYTOOL_*` environment variables, or `~/.pollytool/config`.
With none of them, commands run unsandboxed.

| Input | Result |
|---|---|
| `--sandbox default` | `workspace+net+git` |
| `--sandbox base` | Temporary writes only; no network |
| `--sandbox=` or an empty `POLLYTOOL_SANDBOX` | `workspace+net+git` |
| A lone `--readpath`, `--writepath`, `--denypath`, or enabled `--allownet` | `workspace+net+git`, plus that setting |
| `--add-dir` alone | Records extra project context; does **not** enable sandboxing |
| A saved workspace profile alone | Does **not** enable sandboxing |
| `--nosandbox` | Explicitly disables sandboxing; conflicts with supplied sandbox policy flags/settings |

`--sandbox` requires a value. `/setup` saves the choice for later launches; it
cannot replace the process policy already wired for the current launch.
`--nosandbox=false` can override an exported `POLLYTOOL_NOSANDBOX=true`.

Once requested, sandboxing fails closed: an unavailable backend, invalid policy,
or failed startup probe is an error. A tool's metadata cannot opt out.
Other platforms can run unsandboxed, but cannot satisfy a sandbox request.

**Library distinction:** `sandbox.DefaultConfig()` and `sandbox.ParsePreset("")`
produce the minimal `base` policy. The CLI expands its empty preset to
`workspace+net+git` before calling the library.

## CLI presets

Join names with `+`. Order does not matter.

| Preset | Effect |
|---|---|
| `default` | Shorthand for `workspace+net+git` |
| `base` | Temporary-directory writes; no network |
| `readonly` | Deny all file writes, including temporary files |
| `workspace` | Make the working directory writable; protect discovered Git metadata |
| `git` | With `workspace`, permit Git data writes while protecting hooks, config, and routing |
| `net` | Allow network access |
| `private-home` | Hide home except explicitly granted paths and discovered toolchains/configuration |
| `ssh` | Expose the SSH agent socket, SSH config, and `known_hosts`; keep private keys masked |
| `sshkeys` | Expose all of `~/.ssh`, including private keys; grant no writes |

`git` requires `workspace`. `readonly+net` allows networking while denying writes.
The `workspace` preset rejects filesystem roots, home itself, mounted-volume
roots, and layouts whose Git metadata cannot be protected.

## Paths and home access

### Global flags

| Flag | Environment variable | Purpose |
|---|---|---|
| `--readpath` | `POLLYTOOL_READPATHS` | Read a path otherwise hidden by policy |
| `--writepath` | `POLLYTOOL_WRITEPATHS` | Add a writable path |
| `--denypath` | `POLLYTOOL_DENYPATHS` | Add a read/write denial |
| `--allownet` | `POLLYTOOL_ALLOWNET` | Allow network access |
| `--nosandboxprofile` | `POLLYTOOL_NOSANDBOXPROFILE` | Omit this workspace's saved profile for one launch |
| `--add-dir` | None | Add a read-only project directory to this session |

Path flags are repeatable and accept `~`. Relative paths resolve against Polly's
working directory. Grants must exist when prepared; a missing grant is dropped
and cannot silently become active later. Replaced or redirected grants fail closed.

Home itself cannot be a grant. A writable ancestor of home does not make home
writable. Policies that expose writes to Polly's own configuration are refused.

### The private home directory

Without `private-home`, ordinary home files, installed toolchains, and other
projects are readable. Home writes still need a grant inside home. Known
credential paths, Polly runtime storage, managed environments, and other members'
scratch remain protected in either mode.

`private-home` hides everything in home except grants. Presets retain read access
to discovered Git configuration and its includes, plus install prefixes reached
through home-based `PATH` entries. Shared roots such as `~/.local` are not exposed
wholesale. A toolchain or cache outside those grants needs an explicit read grant.

The CLI also grants required skill and attachment paths. In a linked worktree it
exposes the worktree's Git directories read-only so Git can inspect the checkout.
That does not make a common gitdir outside the writable workspace writable.

`HOME` itself is not replaced. On Linux, `private-home` uses a temporary filesystem:
writes outside grants may succeed there and disappear when the command exits.
On macOS, those writes are denied. See [platform differences](#platform-implementations).

### Extra project directories

```sh
polly --sandbox default --add-dir ../shared --add-dir ../data
```

`/add-dir <path>` adds one during a session; `/add-dir` lists them. Directories
persist with the session, merge on resume, and pass to child contexts as read
grants. They do not widen the writable workspace.

The directory must exist. Root, home and its ancestors, temporary directories,
workspace-internal paths, and paths containing or inside credential masks are
refused. An ancestor of the workspace is allowed; its siblings become readable.
Use `--readpath` for a credential grant.

A live add updates file tools and future Bash/shell-tool calls. Existing stdio
MCP servers keep their old policy; `/tools restart <server>` applies the new base
policy. Failed restarts leave the old server running. With sandboxing disabled,
the directories remain useful context, but an unsandboxed process retains host access.

## Build setup

Run `/sandbox-init [notes]` in a sandboxed root session. It activates the shipped
`sandbox-setup` skill, reads project instructions and CI, and uses existing native
toolchains. It prepares isolated build storage before dependency bootstrap.

| Setup tool | What it can do |
|---|---|
| `sandbox_prepare` | Allocate workspace-owned cache, state, and configuration; bind path-valued variables and managed links; save the profile without a permission review |
| `sandbox_trial` | Run a diagnostic command and report output, observed denials, and candidate grants |
| `sandbox_propose` | Ask you to review additional host access; every proposed grant starts unselected |

Preparation runs no installer and cannot grant arbitrary host roots, credentials,
sockets, shell hooks, or changes to host installations. Ordinary sandboxed tools
write non-secret configuration and restore dependencies. Existing explicit
settings win over automatic defaults. Recipes guide setup; their names grant no
extra authority.

After setup, the agent must run the actual build and test commands through
ordinary sandboxed Bash. **A successful trial is not final verification.** It then
writes the successful commands, working directory, platform, tool versions, and
bootstrap steps into `AGENTS.md` under `Build and test in Polly's sandbox`.

| Result | Meaning |
|---|---|
| **Verified** | Recorded commands ran successfully under the saved policy |
| **Verified with sandbox exclusions** | Named tests were shown to require an unavailable sandbox operation; the filtered command ran tests and passed |
| **Incomplete** | Setup, verification, persistence, or the instructions update still needs work |

Assertion failures, compiler errors, missing dependencies/services, and fixable
permissions are failures, not reasons to filter tests. Keep the full CI commands
and unrelated project instructions. Required session-only settings make reopening
incomplete.

### `/sandbox-init` iteration limit

The turn has at most 20 model calls, or the agent's smaller configured limit.
At the limit, completed history and saved settings remain; setup is incomplete.
This is not a wall-clock or per-command limit.

Setup tools are restricted to that top-level turn, require skills to be enabled,
and never pass to children. Two canceled permission reviews end their use until
the next `/sandbox-init`.

## Workspace profiles

Standing exceptions live in `~/.pollytool/workspaces/<repository-key>/sandbox.json`.
Linked worktrees and subdirectories share the repository's profile; non-Git
folders have their own. The profile is loaded for both TUI and one-shot launches
when sandboxing is enabled.

| Command | Purpose |
|---|---|
| `/sandbox` or `/sandbox show` | List active, inactive, automatic, and session-only settings |
| `/sandbox allow read ~/src/protos` | Grant a host read |
| `/sandbox allow write ~/.foo/cache` | Grant a host write |
| `/sandbox allow env GOCACHE=@cache/go-build` | Redirect a path-valued setting into workspace-owned storage |
| `/sandbox allow passenv NPM_TOKEN` | Pass a filtered environment variable |
| `/sandbox forget <number\|path\|NAME\|all>` | Remove settings |
| `/sandbox try [command]` | Diagnose a command, then review candidate grants |

Add `--members` to a `passenv` item to share it with swarm members. Credential
reads and passed credentials are tied to the repository's recorded `origin`;
a changed origin requires renewed approval.

Changes rebuild the current session's loaded and staged Bash/shell tools,
including derived registries. Other open sessions keep their current settings
until reopened. A session-only item overrides the matching saved kind/path/name;
forgetting its number restores the saved item. Forgetting the path or name removes
both. A failed rebuild or save leaves the previous effective settings intact.

### Profile restrictions

Profiles cannot grant root, home, Polly state, sibling scratch, or workspace
paths governed by the preset. Host-write exceptions cannot cover credential
locations, executable/install directories, startup files, Git configuration,
workspace metadata, or other repositories. Explicit CLI grants still undergo the
sandbox's own validation and Git audit.

Path-valued `env` items use `@workspace` or declared managed allocations.
They cannot replace `HOME`, `PATH`, temporary-directory policy,
`POLLYTOOL_*`, code-loading hooks, service addresses, or credential variables.
Use `passenv` for permitted credential names. Socket access requires its own
policy; passing a socket variable alone is not a socket grant.

Files must be owned, regular, non-symlink files of at most 256 KiB, inside an
owned directory, with no other-user writes. Invalid items are skipped with a
notice; an unreadable or unsupported profile applies nothing.

## Storage and cleanup

Managed allocations have stable names, a purpose, recipe provenance, and one of
three cleanup categories:

| Kind | Lifetime | Clean caches | Reset environment |
|---|---|---|---|
| `@cache/name` | Disposable downloads/build caches | Clear | Clear |
| `@state/name` | Restorable dependencies and mutable tool state | Keep | Clear |
| `@config/name` | Non-secret configuration | Keep | Keep |

`/sandbox storage` shows paths, sizes, sharing, purposes, and unmanaged
entries. `/sandbox clean caches` clears tracked caches. `/sandbox reset environment`
also clears tracked state, preserving configuration, declarations, and explicit
grants. Neither command removes checkout `node_modules`, `.venv`, build outputs,
or host installations. Reset does not run bootstrap; restore dependencies and
verify again afterward.

Cache allocations live under the platform cache directory's
`pollytool/ws/<repository>/managed/`. State/configuration live under the platform
data directory's `pollytool/environments/<repository>/<checkout>/`. Absolute
`XDG_CACHE_HOME` and `XDG_DATA_HOME` override the platform bases. Permission and
ownership records stay under `~/.pollytool/workspaces/`, outside sandbox writes.

Worktrees get separate mutable state and configuration. Only caches declared
safe for concurrent writers are shared. New checkouts copy the initial non-secret
configuration without overwriting local changes. Read-only members use private
scratch copies; contexts without writable scratch get no managed write grants.
Untracked cache redirects are excluded from managed cleanup.

Cleanup requires an idle invoking session, stopped members, and an exclusive
storage lease. Another live session can make the environment busy. Both frontends
run storage work in the background and cancel it on shutdown. Deletion stays
inside owned allocations, does not follow external links, and retains ownership
records after partial failure for safe retry.

`/sandbox forget @state/name` disables the allocation's automatic grant while
retaining its cleanup record. A later setup can prepare it again.

## Credentials and environment

### Credential paths denied by default

The current default masks are:

| Category | Paths |
|---|---|
| SSH and GPG | `~/.ssh`, `~/.gnupg`, `~/.gpg` |
| Cloud and clusters | `~/.aws`, `~/.azure`, `~/.config/gcloud`, `~/.kube` |
| Package registries | `~/.npmrc`, `~/.pypirc`, `~/.gem/credentials`, `~/.cargo/credentials`, `~/.cargo/credentials.toml` |
| Other credentials | `~/.docker/config.json`, `~/.config/gh`, `~/.netrc`, `~/.git-credentials` |
| Key stores | `~/.local/share/keyrings`, `~/Library/Keychains` |

Masks follow resolved paths, including symlinks. An explicit read grant at or
inside a masked path can expose it; deeper denials still apply. These exposures
are named in the sandbox posture. Arbitrary `.env` files and secrets outside the
list are not detected. Add explicit denials or use `private-home` as appropriate.

### Environment filtering

By default, filtering removes these names case-insensitively:

- `POLLYTOOL_*`, `AWS_*`, and credential suffixes such as `_API_KEY`, `_APIKEY`,
  `_TOKEN`, `_SECRET`, `_SECRET_KEY`, `_ACCESS_KEY`, `_PASSWORD`, `_PASSPHRASE`,
  `_CREDENTIALS`, and `_PRIVATE_KEY`, including their bare forms.
- `PGPASSWORD`, `PGPASSFILE`, `MYSQL_PWD`, `REDISCLI_AUTH`, and `DATABASE_URL`.
- SSH/GPG agent variables, D-Bus addresses, `DOCKER_HOST`, `CONTAINER_HOST`,
  `XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY`, and `PULSE_SERVER`.

`passEnv` exempts exact names from the default filter. A nonempty `allowEnv`
passes only those exact names and ignores `passEnv`. These are name heuristics,
not secret detection: a variable named `CREDS` is not automatically removed.
Policy `env` values apply last; they are configuration, never a place to store secrets.

The launcher receives an empty environment. Filtered target values are installed
only after containment, using an anonymous descriptor on Linux and pipes on macOS.

## Tool configuration

A shell tool schema or MCP server entry can declare `"sandbox": true` for the
base policy, or an object for additions. Sandboxed registries refuse `false`;
an unsandboxed CLI launch or `WithUnsafeNoSandbox` permits it. A declaration
cannot enable sandboxing for an unsandboxed CLI launch.

### The per-tool `"sandbox"` object

| Field | Effect |
|---|---|
| `allowNetwork` | Allow networking |
| `privateHome` | Hide ungranted home paths |
| `denyDNS` | Restrict DNS when networking is allowed; platform limits apply |
| `writablePaths` | Add read/write grants |
| `readPaths` | Add read grants inside hidden or denied roots |
| `denyPaths` | Deny reads and writes; deeper read grants can expose descendants |
| `denyWritePaths` | Protect existing paths inside writable trees |
| `allowEnv` | Strict ambient environment allowlist |
| `passEnv` | Exempt names from the default environment filter |
| `env` | Explicit target variables; later overlays win by name |
| `allowUnixSockets` | Grant existing absolute Unix-socket paths |
| `denyWrite` | Deny all file writes, overriding write grants |
| `denyHostTemp` | Withhold automatic host-temp write grants; explicit write grants remain |

```json
{
  "sandbox": {
    "allowNetwork": true,
    "writablePaths": ["~/.cache/fetcher"],
    "passEnv": ["SERVICE_TOKEN"]
  }
}
```

Create the cache directory before launching. This example adds to the caller's
policy; it does not replace existing grants or restrictions.

## How policies merge

For Bash and shell tools, the order is **base → named layers → tool declaration**.
Named layers apply in name order. The workspace profile is one such layer.

- Boolean fields OR; lists append. `env` merges by name, with the later value
  winning. An overlay cannot unset an earlier boolean or remove a path.
- `denyWrite` wins over write grants. `denyDNS` matters only with network access.
- The deepest path rule wins. A read grant tied with a denial permits reads,
  but not writes. `denyWritePaths` keeps read access and blocks writes.
- Grants freeze filesystem identities during policy preparation. Missing grants
  are dropped; replacing the object or its route does not grant new authority.
- `denyPaths` masks are reevaluated for commands. On Linux, a nonexistent denied
  path outside a private root is covered only once it exists for a later command.
- Removing/replacing a named layer rebuilds affected tools transactionally. A
  running call keeps its old sandbox. Swarm members keep their bound launch policy.

| Consumer | Workspace-profile layer |
|---|---|
| Bash, shell tools, native file checks | Applied |
| Ordinary subagents | Inherited |
| Swarm members | Filtered member part; writes follow the member's authority; `passenv` needs `--members` |
| Stdio MCP servers | Not applied, including after restart |
| Shell-tool schema discovery | Not applied |
| Runtime Git administration | Not applied |

MCP servers use the base policy plus their own declaration. Schema discovery has
its own restricted policy. This distinction matters when a command works in Bash
but the same program fails as an MCP server.

## What gets sandboxed

| Path | Enforcement |
|---|---|
| Built-in Bash and shell tools | Native process sandbox |
| Stdio MCP | The server process is sandboxed for its lifetime |
| Remote HTTP/SSE MCP | The remote process is outside local containment |
| Native file/image tools and automatic `AGENTS.md` reads | In-process path checks |
| Custom in-process Go tools | The host/tool implementation must enforce its own effects |
| Workflow JavaScript | No direct process/filesystem/network APIs; host calls use bound tools |

File checks test both the written path and its resolved route, then open without
following links. Reads outside private/denied roots remain allowed. Writes need a
grant and must pass all write restrictions. In-process image URL fetches need
network permission; with `denyDNS`, the host must be an IP literal.

Shell `--schema` discovery runs before the tool is trusted: temporary writes,
no network or workspace writes, and only safe interpreter/toolchain read grants.
It does not inherit credential exceptions or workspace-profile settings.

### Session storage is private to the host

The CLI protects the session database, its WAL/SHM sidecars, the default promotion
destination, and runtime roots before loading process tools. Linux's missing-path
limitation still applies to sidecars outside private roots. Host storage APIs
remain available.
Explicitly unsandboxed processes retain ambient host authority.

## Agents and snapshots

Each member binds native tools, processes, skills, instructions, and local MCP
servers to its assigned root. Editing uses an isolated Git checkout; read-only
members cannot write the checkout. Each member gets private scratch under
`$TMPDIR/polly-<uid>/`, with `TMPDIR`, `TMP`, and `TEMP` pointing there.

Policies hide the parent checkout, sibling workspaces/scratch, and runtime data;
common Git history and approved Git configuration remain readable. Scratch is
released with the context. macOS can grant a separate `nested/` scratch root for
Polly launched inside a sandboxed parent command. `POLLYTOOL_SCRATCH_ROOT` is the
runtime's explicit scratch-root override.

Context rebinding drops overlays that could restore parent write authority.
Required incompatible tools fail launch; optional ones are omitted and reported.
Remote MCP needs an operator's `contextIndependent: true` declaration for member
use. Native file tools keep context restrictions even with process sandboxing
disabled; an unsandboxed shell does not provide that isolation.

### Swarm snapshot limits

Snapshots include tracked edits and non-ignored new files through private Git
indexes, preserving the parent's index, HEAD, and branch. Git-backed research also
uses snapshots; only non-Git research uses live files. Git setup failure does not
fall back to the live checkout.

Capture rejects conflicted, sparse, or split indexes; submodules; content filters
including LFS; unsafe Git metadata routes; and special files. New-file guards are
32 MiB per file and 256 MiB total. Runtime-private paths are excluded before
staging, even if tracked; this does not erase historical Git objects.

Runtime Git gets narrow host-owned administration grants through the sandbox
factory. Agent tools never receive those grants. Only the parent can integrate;
application preserves its branch and index and records an intent and receipt.
Snapshots and publications are evidence, not additional filesystem access.

See [workspaces](WORKFLOWS.md#workspace-release-and-restoration),
[integration](WORKFLOWS.md#integrating-editing-results), and
[recovery](WORKFLOWS.md#settlement-and-recovery).

## Workspace Git protection

| Policy | Git behavior |
|---|---|
| `workspace` | Discovered Git metadata is read-only |
| `workspace+git` / `default` | Data paths can be written; hooks, config, and routing remain protected |

Discovery covers nested repositories, submodules, and linked-worktree routing.
An empty `.git` marker is pinned and skipped; malformed routing fails. With `git`,
missing inert protection entries may be created. If that cannot be done, the
repository falls back to whole-tree protection. Dormant or unvisited metadata
stays wholly protected.

A main checkout can commit, rebase, and fetch where its grants allow. Config-writing
commands remain blocked. A common gitdir outside a linked checkout is not made
writable merely by adding `git`. New metadata below writable `modules/` or
`worktrees/` can still be created; inspect tool-created repositories before using
them on the host.

Bare working directories, symlinked/hard-linked metadata, unsafe hook layouts,
and unsupported repository-local configuration routes are refused. The audit
also checks global/system hooks and configuration includes against the **final**
writable grants, including per-tool additions.

Polly uses a trusted Git executable for this audit: the fixed system Git, or
validated standard Homebrew Git routes on macOS. Fixed audit commands run on the
host; model-selected Git commands remain inside their tool policy.

TUI change tracking uses private indexes and object storage under
`~/.pollytool/changes/`, through the runtime sandbox. It grants agents no new
access and does not modify the repository's index, refs, or objects. The session's
saved baseline survives transcript reset and change-cache cleanup.

## Platform implementations

Both backends filter environments and freeze grants, but their isolation differs.

| Behavior | Linux: bubblewrap | macOS: Seatbelt |
|---|---|---|
| Ordinary home reads | Allowed by default | Allowed by default |
| `private-home` | Private tmpfs with grants mounted back | Deny reads, then allow grants |
| Ungranted private-home writes | Disposable tmpfs writes, unless `denyWrite` | Denied |
| Temporary storage | Private `/tmp` and `/run`; configured grants can expose host paths | Shared host temp unless withheld |
| Missing/hidden reads | Missing paths, empty directories, or empty masked files | Usually permission errors; metadata probes can report absence |
| No network | Separate network namespace | Network deny rules |
| `denyDNS` with network | Hides the default resolver; hardcoded DNS remains possible | Blocks port 53 and the system resolver socket; not all DNS-over-HTTP traffic |
| Other processes | Hidden by PID namespace | Enumeration remains possible; signaling is restricted |
| Host IPC | Private IPC namespace; Unix sockets restricted | Unix sockets restricted; Mach services remain available |
| A granted Unix socket | Enables broader AF_UNIX access to reachable paths | Exact socket-path grant |

### Linux: bubblewrap (`bwrap`)

Polly requires the fixed, trusted `/usr/bin/bwrap` and a host that permits its
namespaces. The launcher uses a read-only root, ordered mounts, PID/IPC namespaces,
dropped capabilities, seccomp, parent-death teardown, and a new session.

Without socket grants, the filter blocks filesystem/abstract Unix sockets,
AF_VSOCK, and io_uring socket setup. Private anonymous stream and sequenced-packet
pairs remain available for child IPC; datagram pairs are blocked.

`TMPDIR`, `TMP`, and `TEMP` normally become `/tmp`; policy `env` can set the final
values. `denyHostTemp` withholds implicit **host** writes; it does not remove the
private per-command tmpfs. `denyWrite` makes private writable mounts read-only too.

### macOS: Seatbelt (`sandbox-exec`)

Polly requires trusted `/usr/bin/sandbox-exec` and `/usr/bin/perl`. The generated
profile starts from allow-by-default and restricts file writes, selected reads,
networking, and signals. The command gets its own process session.

Both literal and resolved paths are checked. Ancestors of read grants receive
metadata-only access for traversal, not directory listings or sibling reads.
Automatic host-temp grants freeze when the policy is prepared.

This backend shares the host filesystem and kernel. It cannot provide Linux's
mount, PID, or IPC namespace isolation.

## Observing denials in a trial

`/sandbox try <command>` runs once under a candidate policy without changing the
session. With no command, it offers recent failed Bash calls. Trial output and
denials help identify needed access; they do not authorize it.

| Platform/mode | What a trial can observe |
|---|---|
| macOS | Tagged sandbox denials from a live system-log stream, when available |
| Linux, readable home | Command output and an explicit observation limitation; no automatic denial list |
| Linux, `private-home` | New writes in the disposable home tmpfs; no hidden reads or denied writes elsewhere |
| Other platforms | No native observation |

macOS brackets commands with canary denials to check observation. If the stream
cannot be confirmed, the trial says denials are unavailable; an empty list is not
proof of unrestricted success. The fixed trusted `log stream` observer runs on the
host with a minimal environment and bounded lifetime.

Linux never scans the real readable home to infer denials. A successful
private-home trial can have discarded writes, so repeat final verification under
the saved ordinary policy.

Every grant proposal starts unselected. Credentials need explicit selection;
retargeted paths lose their selection. Network/socket denials and unexplained
operations are reported without automatic grants. Read proposals may group nearby
files; writes target a tool's own directory, not an entire shared config/cache root.

Select only required items, rerun, then save or keep them for the session.
The line frontend accepts `tick`, `try`, `save`, `session`, `output`, and `cancel`;
without an interactive terminal it grants nothing. During setup, the model sees
the review outcome, not output from a user-approved credential trial.

### Observing decisions

Use `/set sandbox` for live posture, `/tools list` for per-tool policy, and
`/sandbox show` for profile items. `--debug` logs configuration, wrapping, and
stripped variable **names**, never their values.

Every model request gets a bounded `<sandbox_context>` describing its bound
policy. It refreshes after policy changes and is not saved as transcript text.
Custom prompts and structured output retain it. It is guidance; tools enforce
permissions independently.

## Limitations

- This is same-user, shared-kernel containment, not a VM. There are no CPU, memory,
  or process-count limits.
- Readable home includes arbitrary personal files and unrecognized secrets.
  `private-home` narrows that view but still exposes granted toolchains and files.
- Linux Unix-socket grants are broader than macOS grants. An SSH agent grant lets
  a command request signatures; it does not reveal the key bytes.
- SSH presets do not grant `known_hosts` writes. First contact with an unknown
  host needs host-side setup. GPG-signed commits need separate agent access or
  signing outside the sandbox.
- Existing stdio MCP processes keep their launch policy. Remote servers are
  outside local containment. Custom in-process tools must enforce their own effects.
- Cancellation stops owned process groups/namespaces, but deliberately detached
  processes can escape process-group cancellation where the OS allows them.
  Output capture has a one-second drain bound and reports incomplete capture.
- Frozen grants need rebuilding after legitimate replacement. Missing Linux
  masks outside private roots take effect only on a later command after creation.

## Implementation and tests

These files define the behavior above:

| Area | Source and coverage |
|---|---|
| CLI activation and presets | [config.go](../cmd/polly/config.go), [preset.go](../tools/sandbox/preset.go) |
| Config, merging, and filters | [sandbox.go](../tools/sandbox/sandbox.go) |
| Home visibility | [readable_home_test.go](../tools/sandbox/readable_home_test.go) |
| Profile rules and preparation | [sandbox_profile_check.go](../cmd/polly/sandbox_profile_check.go), [sandbox_prepare_test.go](../cmd/polly/sandbox_prepare_test.go) |
| Storage and cleanup | [envstorage](../internal/envstorage), [repl_sandbox_storage_test.go](../cmd/polly/repl_sandbox_storage_test.go) |
| Native enforcement | [Linux](../tools/sandbox/sandbox_linux.go), [macOS](../tools/sandbox/sandbox_darwin.go) |
| Trial observation | [Linux](../tools/sandbox/observe_linux.go), [macOS](../tools/sandbox/observe_darwin.go) |

See [sandbox validation](SANDBOX_ENVIRONMENT_VALIDATION.md) for native checks,
build recipes, and live setup fixtures.
