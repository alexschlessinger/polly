# Sandbox validation

Run native checks on macOS or Linux with the sandbox backend available.
A skipped test is missing coverage, not a passing sandbox check.

## Contents

- [Prerequisites](#prerequisites)
- [Repository checks](#repository-checks)
- [Build recipes](#build-recipes)
- [Live setup checks](#live-setup-checks)
- [What to verify](#what-to-verify)

[Documentation index](README.md) · [Sandbox guide](SANDBOX.md) ·
[Local CI](../.github/local-ci/README.md)

## Prerequisites

| Check | Needs |
|---|---|
| Native sandbox | macOS Seatbelt or Linux `bubblewrap`; permission to launch that backend |
| Linux namespaces | Unprivileged user namespaces; on AppArmor hosts, `kernel.apparmor_restrict_unprivileged_userns=0` |
| Recipe fixtures | Existing toolchains on `PATH`, Git, dependency network access |
| Live `/sandbox-init` | Polly, model credentials, disposable fixture projects/homes |

An outer sandbox can prevent nested Seatbelt or bubblewrap from starting. That
run does not establish native enforcement. Use a host or the documented CI worker
that can run the backend. `cmd/polly` tests create a temporary home;
`POLLYTOOL_TEST_HOME` can select its parent directory when needed.

## Repository checks

From the repository root:

```sh
CGO_ENABLED=0 go build ./...
go vet ./...
POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 CGO_ENABLED=0 go test ./...
gofmt -l $(git ls-files -co --exclude-standard '*.go')
git diff --check
```

The opt-in flag requires the native sandbox suite. For focused enforcement checks:

```sh
POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 go test -count=1 ./tools/sandbox
```

[`.github/ci.sh`](../.github/ci.sh) bundles Python supervisor checks, build, vet,
and required native tests in `test`. `race` uses CGO; `cross` builds Linux/macOS
amd64/arm64 and Windows amd64. Cross-compilation proves compilation, not native
runtime behavior.

## Build recipes

The [shipped recipes](../skills/builtin/sandbox-setup/recipes) describe storage
preparation for these fixture names:

| Family | Recipe names |
|---|---|
| Go / Rust | `go`, `cargo` |
| Node | `npm`, `pnpm`, `yarn-classic`, `yarn-modern` |
| Python | `uv`, `pip` |
| JVM | `gradle`, `maven` |
| Other native builds | `dotnet`, `zig`, `c-cpp` |

Use all available toolchains, or select a comma-separated subset:

```sh
POLLYTOOL_SANDBOX_RECIPE_TESTS=1 \
  go test ./cmd/polly -run '^TestSandboxNativeRecipes$' -count=1 -v

POLLYTOOL_SANDBOX_RECIPE_TESTS=go,uv \
  go test ./cmd/polly -run '^TestSandboxNativeRecipes$' -count=1 -v
```

Each fixture prepares storage, bootstraps dependencies, runs cold and warm checks,
reopens the profile, creates a real linked Git worktree, and checks again after
reset. Project-local dependencies must be restored in the new checkout. Commands
run through ordinary sandboxed Bash, not a trial overlay.

Missing tools produce explicit skips. Only one Yarn major can occupy `yarn` on
`PATH`; select each variant with the matching installation. Recipe declarations
record supported versions; test output records the versions actually exercised.

## Live setup checks

[The fixture generator](../.github/sandbox-init-evals.py) creates disposable
projects and isolated homes. It makes no model calls and copies no credentials.
Existing fixture directories are refused.

```sh
python3 .github/sandbox-init-evals.py \
  --workspaces /tmp/polly-init-evals \
  --homes "$PWD/.tmp-native-init-homes"
```

Use each printed home only for its fixture's Polly process, from the matching
project. Polly refuses homes under `/tmp`; never use a real home as the fixture
root. Supply model credentials separately and run with an explicit sandbox policy.
Use `default+private-home` for the hidden-SDK scenario so its extra read actually
requires a grant. Then run `/sandbox-init`.

| Fixture | Expected check |
|---|---|
| `mixed` | Both Go and Node bootstrap/build/test commands run |
| `unfamiliar` | Custom state uses managed storage and the tool's own flag |
| `host-read` | Review only the synthetic `.hidden-sdk/version.txt` read |
| `failing-test` | Keep the assertion failure visible; source/tests remain intact; setup is incomplete |
| `sandbox-test` | Keep the full command and evidence for a nested-sandbox exclusion; also run the supported subset |

Reopen the session and repeat setup to check saved grants, storage reuse, and
one maintained `AGENTS.md` section. Inspect actual tool results, proposals, and
commands. A successful trial does not establish final verification.

## What to verify

| Boundary | Evidence |
|---|---|
| Home access | Ordinary reads work; private-home reads do not; home writes and credential/runtime paths stay denied |
| Profile updates | Explicit/session settings win; failed saves or tool rebuilds leave effective policy intact |
| Derived tools | Loaded and staged Bash/shell tools receive changes; MCP retains its documented base policy |
| Workspace storage | Mutable state is separate per checkout; read-only members use scratch; only declared shared caches are shared |
| Cleanup | Busy leases refuse cleanup; reset preserves configuration, grants, checkout dependencies, and host files |
| Path safety | Replaced allocations, symlink escapes, loader/credential variables, and private roots are refused |
| Setup result | Commands ran under the final profile; ordinary test failures are not filtered out |

Relevant coverage lives in [sandbox tests](../tools/sandbox),
[profile tests](../cmd/polly/sandbox_profile_test.go),
[preparation tests](../cmd/polly/sandbox_prepare_test.go),
[storage-command tests](../cmd/polly/repl_sandbox_storage_test.go),
[derived-policy tests](../tools/registry_policy_derived_test.go), and
[storage lifecycle tests](../internal/envstorage).
