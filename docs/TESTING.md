# Testing

Use a small edit/test loop, then broaden verification once the behavior works.
The local runner requires Python 3.8+, Git, and the project's Go toolchain; it uses
only the Python standard library. Commands run from the repository root, even
when the runner is invoked from another directory. Package arguments are relative
to that root. All modes use `CGO_ENABLED=0` and reuse the configured Go caches.

## Focused iteration

```bash
python3 -B .github/test.py focus ./cmd/polly --run 'TestStatusRow'
python3 -B .github/test.py focus ./sessions ./swarm --run 'TestNotification'
```

`focus` runs only the named packages, optionally filtered by Go's `-run` regular
expression, and always uses `-count=1`. It still compiles each package's tests;
a test filter does not avoid compilation. Pass package paths, not individual Go
files, so package context and other source files are preserved.

## Broaden to affected packages

```bash
python3 -B .github/test.py affected --list
python3 -B .github/test.py affected
python3 -B .github/test.py affected --base main
```

The default change set compares the working tree to `HEAD`, including staged,
unstaged, deleted, renamed, and non-ignored untracked files. `--base` compares to
that revision directly, not its merge base; use a merge-base commit explicitly if
that is the comparison you want. A clean tree selects nothing.

The runner maps files to their nearest Go package directory and follows reverse
imports transitively, including imports in internal and external tests. This
covers package-local fixtures and embedded assets as well as Go source. Both old
and new directories of a rename are considered. Module/workspace files, `.github`
changes, and paths without a current owning package (including repository-level
documentation and completely removed packages) fall back to **full verification**.
Package-discovery errors fail rather than silently selecting no tests.

Selection uses `go list` for the current platform and build configuration. It is
an iteration aid, not proof that arbitrary runtime file dependencies or other
platforms are covered. Use focused commands when a large dirty tree selects too
much; always run the full checkpoint before finishing.

## Final checkpoint

```bash
python3 -B .github/test.py full
```

This runs the Python tooling tests, builds all Go packages, runs `go vet`, checks
formatting, and tests all Go packages. Formatting is checked without rewriting
files; existing tracked and non-ignored untracked Go files are checked, while
deleted paths are skipped. A failing check stops the runner with a nonzero exit.

`affected` and `full` preserve Go's test-result caching. Source/dependency changes
invalidate the relevant results automatically. Add `--fresh` only when an uncached
run is needed, such as a reproduction involving external state. Avoid repeatedly
forcing `-count=1 ./...` during edits. Workers should run focused checks; the parent
owns the final full check after integration.

## Find expensive tests

```bash
python3 -B .github/test.py focus ./swarm --timings
python3 -B .github/test.py full --fresh --timings
```

`--timings` streams verbose test output and reports the ten slowest packages and
top-level tests using Go's JSON event stream. Failed tests still fail the command.
Cached packages are excluded from the timing report because Go replays their old
test events. Parallel test durations overlap; do not sum them to estimate elapsed
suite time. Profile the slow tests' setup, execution, and cleanup before changing
fixtures or concurrency. Timing collection does not change the tests selected.

## CI and specialized checks

The local runner does not replace `.github/ci.sh`. Its `test` mode additionally
requires native sandbox security tests; `race` and `cross` cover the race detector
and five target platforms. See [Local CI](../.github/local-ci/README.md) and
[Sandbox validation](SANDBOX_ENVIRONMENT_VALIDATION.md).

Run relevant security/integration checks when changing those boundaries. Do not
assume `go test -short ./...` excludes expensive integration tests: most tests do
not currently consult `testing.Short()`.
