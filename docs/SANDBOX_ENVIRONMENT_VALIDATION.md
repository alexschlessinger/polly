# Native environment validation

Validated on 2026-09-19 against the combined `sandbox/init` implementation.
The existing PR commits were retained; implementation and validation changes are
additional commits on that branch.

## Runtime and lifecycle coverage

The automated tests exercise:

- Version-1 reads without rewrites, successful version-2 upgrades, legacy cache
  semantics, explicit/session precedence, repeated preparation and reopened sessions.
- Rejected host roots, escaping references, credential/loader variables, replaced
  allocations, symlinked ancestors, automatic host grants and denied member paths.
- Serialized concurrent profile updates, staged policy publication after persistence,
  factory/persistence failures, and loaded/staged tools in nested derived registries.
- Separate mutable worktree state, checkout-subdirectory reuse, read-only scratch,
  shared caches, configuration copies and contexts without writable scratch.
- Cross-process lifetime leases and cleanup admission, cancellation, partial cleanup
  retries, preserved configuration and root identities, read-only dependency files,
  and unchanged external/project files. Trials and new context binding are gated
  during cleanup too.
- Private managed roots outside home, using both native and in-process file checks.

Both the TUI and line frontend were exercised. Storage inspection showed paths,
sizes and provenance. Reset retained checkout files and reported that bootstrap
and verification were required. A second live session caused a clear busy result;
cache cleanup succeeded after that session closed.

## Real recipes

`TestSandboxNativeRecipes` uses the shipped recipe declarations and ordinary
sandboxed execution. Each fixture covers initial setup, cold build/test, warm
reuse, reopened profile, a real Git linked worktree, and build/test after reset.
Fixture sources and generated lockfiles are committed before creating the new
checkout; project-local dependencies must be restored there. Missing tools are explicit
skips and do not count as recipe coverage.

Fixture source permissions match ordinary Git checkouts. The real-worktree run
caught a fixture helper's private file modes changing Yarn's local-package archive
hash after checkout; correcting those modes preserved `yarn install --immutable`.
Yarn hashes the [local package archive](https://github.com/yarnpkg/berry/blob/master/packages/plugin-file/sources/FileResolver.ts).

All 13 recipes passed on macOS 27.2 arm64 and Linux arm64. Linux used a disposable
Debian toolchain container on an OrbStack Linux kernel, with real bubblewrap and
seccomp enforcement inside it. This container is validation infrastructure, not
the product's execution strategy. Toolchains needed only for validation were
installed into disposable test storage; existing host installations were not changed.

| Recipe | macOS tool versions | Linux tool versions |
| --- | --- | --- |
| Go | 1.27.1 | 1.27.0 |
| npm | Node 26.8.2, npm 12.0.2 | Node 20.19.2, npm 9.2.0 |
| pnpm | 11.19.0 | 10.32.1 |
| Yarn classic | 1.22.22 | 1.22.22 |
| Yarn modern | 4.9.2 | 4.9.2 |
| uv | 0.12.13, Python 3.14.7 | 0.12.17, Python 3.13.5 |
| pip | 26.2.1, Python 3.14.7 | 25.1.1, Python 3.13.5 |
| Cargo / rustc | 1.98.1 | 1.85.1 |
| Gradle | 8.14.3, Corretto JDK 21.0.12.1 | 8.14.3, Debian JDK 21.0.12.1 |
| Maven | 3.9.9, Corretto JDK 21.0.12.1 | 3.9.9, Debian JDK 21.0.12.1 |
| .NET | SDK 10.0.401 | SDK 10.0.401 |
| Zig | 0.17.0-dev.1476+91a29d707 | 0.15.2 |
| C/C++ | CMake 4.4.3, Apple Clang 21.0.0 | CMake 3.31.6, GCC 14.2.0 |

Dependency fixtures include UUID for Go, `itoa` for Cargo, a registry package plus
a local package for Node, `idna` for Python, JUnit for Java, and xUnit for .NET.
C/C++ and Zig fixtures compile and execute local tests.

The cold fixtures exposed and led to these corrections:

- Go needs isolated `GOPATH` state for checksum-database checkpoints, in addition
  to `GOCACHE` and `GOMODCACHE`. Checksum verification stays enabled.
- .NET needs explicit NuGet HTTP/plugin cache locations as well as package state.
- Zig's global cache must be per checkout: the tested development version retained
  source paths that another checkout's sandbox could not read.
- Linux Cargo's child-process startup needs anonymous sequenced-packet pairs.
  The filter now allows those private pairs, while retaining restrictions on
  endpoint creation, datagram pairs, AF_VSOCK and io_uring. Kernel tests verify
  that the pairs cannot disconnect/reconnect or redirect messages to listening
  filesystem or abstract endpoints. The startup mechanism is visible in
  [Rust's process implementation](https://github.com/rust-lang/rust/blob/1.85.1/library/std/src/sys/pal/unix/process/process_unix.rs).

To reproduce with the required existing tools on PATH:

```sh
POLLYTOOL_SANDBOX_RECIPE_TESTS=1 go test ./cmd/polly -run '^TestSandboxNativeRecipes$' -count=1 -v
```

Only one Yarn major can occupy `yarn` on PATH at once. Run the other variant with
its installation on PATH and `POLLYTOOL_SANDBOX_RECIPE_TESTS=yarn-classic` or
`yarn-modern`. The variable also accepts a comma-separated subset. Recipe JSON
describes supported version ranges; the table above records versions actually tested.

## Live `/init` evaluations

The macOS TUI evaluations used `anthropic/claude-sonnet-4-6`. The fixture generator
is [sandbox-init-evals.py](../.github/sandbox-init-evals.py); it creates projects
and isolated fixture homes without making model calls or copying credentials.

| Scenario | Initial permission reviews | Preparation / trials / proposals | Observed outcome |
| --- | ---: | --- | --- |
| Mixed Go + Node project | 0 | 1 / 0 / 0 | Verified; both workflows executed |
| Unfamiliar `widget.py` tool | 0 | 1 / 0 / 0 | Verified; custom state allocation and command flag |
| Required hidden SDK read | 1 | 0 / 1 / 1 | Verified after approving exactly the synthetic SDK file |
| Ordinary assertion failure | 0 | 0 / 0 / 0 | Incomplete; failing source and test preserved |
| Nested Seatbelt test | 0 | 0 / 0 / 0 | Verified with sandbox exclusions; full command retained and the filtered command ran one passing test |

Ordinary preparation produced zero permission reviews and zero failed build
attempts before preparing storage. Two unnecessary inspection failures occurred:
one missing AGENTS.md read and one directory passed to `read_file`. Expected
failure evidence was retained for the hidden SDK trial, the assertion failure,
and the nested-sandbox test; none was relabeled as a preparation success.

Repeated runs of the mixed project, host-read case and sandbox-test case reused
saved settings without further reviews and retained one dedicated AGENTS.md
section. The mixed project switched its recorded bootstrap to `npm ci` once its
lockfile existed. Initial evaluations also exposed a checkout-specific path and
missing working-directory/OS fields in existing instructions. The final brief
explicitly supplies the host platform and requires a portability check; live
reruns repaired those fields. The ordinary failing test was never excluded.

Example fixture creation (use fresh directories):

```sh
python3 .github/sandbox-init-evals.py \
  --workspaces /tmp/polly-init-evals \
  --homes "$PWD/.tmp-native-init-homes"
```

Use each printed fixture home only for its test Polly process; run `/init` from
the corresponding project. The host-read case's sole reviewed file is the
generated `.hidden-sdk/version.txt`. Check proposals/tool results in the session
transcript, commands actually executed, unchanged test sources, the saved section,
and reopened-session behavior. Fixture HOME isolation is test harness setup;
managed build preparation never replaces HOME.

## Repository checks

- CGO-free build, vet and full tests passed on macOS and Linux through
  `.github/ci.sh test`, including `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1`.
- Focused race coverage passed for storage, profile/policy changes, derived and
  bound contexts, cleanup and trials; the entire envstorage package also ran
  under the race detector.
- Cross-builds passed for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
  and windows/amd64. Native execution was tested on the two arm64 platforms above.
- `gofmt -l` over tracked and untracked Go sources produced no output;
  `git diff --check` passed.

Native recipe tests and model evaluations are opt-in because they need installed
toolchains, dependency network access or model credentials. Missing prerequisites
must remain visible as incomplete coverage.
