# AGENTS.md

`polly` is an LLM harness: a CLI + TUI (`cmd/polly`) on a Go library. Single module `github.com/alexschlessinger/pollytool`, Go 1.27, stdlib `testing` only, no Makefile.

## Iterate narrowly; verify fully before declaring done

```bash
# During edits: rerun the relevant tests, not the entire repository.
python3 -B .github/test.py focus ./cmd/polly --run 'TestStatusRow'
# Broaden to changed packages and their reverse dependencies (cache allowed).
python3 -B .github/test.py affected
# Final checkpoint after all edits/integration:
python3 -B .github/test.py full
```

- The full mode runs Python tooling tests, `CGO_ENABLED=0 go build ./...`, `go vet ./...`, `go test ./...`, and a read-only `gofmt -l` check of existing tracked/untracked Go files. Formatting must be clean; deleted paths are excluded. All Go commands use `CGO_ENABLED=0` (sqlite is modernc.org/sqlite).
- Workers run focused checks for their assignment; the parent runs the full checkpoint once after integration. Do not repeat full uncached sweeps after each small edit or in every worker. Relevant integration/security checks still belong with changes that affect them.
- `focus` always uses `-count=1`; `affected` and `full` allow Go's test cache. Use `--fresh` for deliberate uncached verification, and `--timings` for a slow-test report. `affected --list` previews selection; configuration or unowned paths conservatively select full verification. See [Testing](docs/TESTING.md) for scope and limitations.

- `.github/ci.sh [test|race|cross|warm|all]` is the shared CI entry point. `test` = local-ci Python unit tests + build + vet + `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 go test ./...`; `race` needs `CGO_ENABLED=1` and the workflow samples it on about one run in five; `cross` builds 5 GOOS/GOARCH targets including windows/amd64; `warm` is the compile-only cache pass the Docker image build runs.
- Sandbox security tests are opt-in: `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 go test ./tools/sandbox` (macOS/Linux only). Linux needs `bubblewrap` and `kernel.apparmor_restrict_unprivileged_userns=0`.
- Use `go test -count=1` when re-running after a change you expect to flip a result. Tests are serial except in `swarm`, where every top-level test calls `t.Parallel()` unless it needs `t.Setenv` or `t.Chdir`; new swarm tests follow the same rule.
- Docs start at `docs/README.md`. Main guides: `README.md` (overview and quick start), `docs/CLI.md` (CLI/TUI), `docs/API.md` (Go library), `docs/SANDBOX.md` (sandboxing), `docs/WORKFLOWS.md` (swarms/workflows). Keep the README brief and guides indexed; document current behavior without implementation histories. Local CI (Docker/OrbStack + Tart VMs) is documented in `.github/local-ci/README.md` and is specific to one Apple Silicon setup.

## Layout

Flat top-level domain packages, one concept per file. `cmd/polly` holds the CLI and the entire tcell/gotui TUI (`repl_*.go`; `line_*.go` is the dumb-terminal frontend). `llm` is the agent loop and provider router with one package per provider under `llm/<provider>/`, sharing `llm/internal/contract` and `llm/internal/catalog`. `tools` holds `Tool`/`ToolRegistry` and the builtins; `tools/sandbox` is the policy engine (bubblewrap on Linux, Seatbelt on macOS, heavily platform-split). `sessions` is the SQLite store; `swarm`, `workflow` (goja JS), `worktree`, `subagent` are the multi-agent stack; `messages`, `schema`, `skills`, `artifacts`, `images`, `internal/*` are support.

Request flow: `main` → provider router (`llm.NewMultiPass`) → `llm.NewAgent` → agent loop. Each iteration `llm.Prepare` adapts the request to the model's capabilities, then `ChatCompletionStream` routes on the `provider/` prefix; each provider package builds the wire request, converts messages, parses stream events through its adapter, and fetches its own model catalog and embeddings. Tool calls run in parallel via `tools.ToolRegistry` and results are fed back until done or `ErrMaxIterations`.

Key types: `llm.LLM`/`Agent`/`AgentCallbacks`, `messages.ChatMessage`/`StreamEvent`, `tools.Tool`/`ToolRegistry`/`ToolError`, `schema.ToolSchema`, `sessions.Store`, `subagent.Runner`.

`experiments/textfx` and `experiments/windowfx` are throwaway TUI experiments. `.agents/skills/polly-tui/` (SKILL.md + `driver.sh`) is the sanctioned way to capture the TUI off-screen (scenarios played through `polly --shot-script`). `skills/builtin/` holds skills embedded in the binary (`go:embed` in `skills/builtin.go`), synced to `~/.pollytool/builtin-skills` at startup and shadowed by same-named user skills; they (including `theme-designer`, which drives the `set_theme` tool) must not reference this repository — they run against arbitrary projects.

## Style that differs from Go defaults

- Errors: `fmt.Errorf` with `%w`. Tool failures return `*tools.ToolError` (structured, JSON-serialized).
- Functional options (`With*`) for registries and clients.
- Tests: `t.Fatalf`, `t.TempDir()`, plain assertions. No testify, no mocks frameworks, no golden-file framework.
- Platform-split files (`*_darwin.go`, `*_linux.go`, `*_windows.go`, `*_unix.go`, `*_other.go`): a change usually belongs in every variant; check all of them.
- Only `defaultProviders()` in `llm/multipass.go` may name a provider; every routing rule is a `providerSpec` field there.

## Adding things

- Provider: `llm/<provider>/` exporting `NewProvider`, `ListModels` (on `llm/internal/catalog`), optionally `Embed` and `DefaultBaseURL`; one row in `defaultProviders()` wires it and carries every routing rule as a `providerSpec` field (base URL scoping, keyless access, host routing, catalog shape); env key `POLLYTOOL_<PROVIDER>KEY` via `getEnvVarNameForProvider`. Update `docs/API.md` §Providers and `README.md` §Models.
- Builtin tool: implement `tools.Tool` in `tools/<name>.go` (or a declarative `tools.Func`); register in `installNativeTools` (`tools/native_tools.go`, enabled by `WithNativeTools`) or via `RegisterNative`. Generic registry construction and `Derive` install no native tools. Rich output implements `OutputTool`; long-running exemption is `UntimedTool`. Anything that spawns a process goes through the sandbox factory; anything that touches paths policy-checks like the builtin file tools. Update `README.md` §Built-in tools.
- Sandbox preset or config: update `docs/SANDBOX.md` ("How policies merge").

## Sandbox invariants

Sandboxing is opt-in: a policy — `--sandbox`, a path grant, `--allownet`, from a flag, the environment or the config file — is what asks for one, and a launch nothing asked runs bash, shell tools and stdio MCP servers unsandboxed. Once asked for, it fails closed: tool metadata cannot opt out; home is readable by default with restricted writes; `private-home` opts into hidden home. Known credential paths and Polly runtime storage remain masked. A policy that names no preset gets `workspace+net+git`. Never add a code path that runs a child process outside the sandbox factory, and never widen a grant set or weaken a mask without updating `docs/SANDBOX.md`. Polly refuses to start when cwd is the real `$HOME` or when `HOME` is under `/tmp`, and needs `git` on PATH.

## Gotchas

- Tool wrappers must preserve media output: wrap with `NamespacedTool.ExecuteOutput`; do not drop `OutputTool` semantics.
- Provider quirks are intentional: reasoning models reject `temperature`; OpenAI reasoning items are model-locked and dropped on model switch; Anthropic has a legacy vs adaptive thinking split (`legacyThinkingPrefixes`).
- Never commit the `polly` binary at the repo root, the `textfx`/`windowfx` binaries that `go build ./experiments/...` drops there, or runtime data. Runtime state lives in `~/.pollytool/` (`polly.db`, `skills/`, `themes/`, `worktrees/`), except member scratch, which defaults to `/private/tmp/polly-<uid>/` on macOS (short enough for Unix sockets) and `$TMPDIR/polly-<uid>/` elsewhere, outside the private home. Nested processes receive a scratch root inside their own grant; `themes/` holds user themes, and the `set_theme` tool is the sanctioned writer because the directory is not a home read grant.
- Scope searches to the repo root and exclude gitignored paths
- `POLLYTOOL_*` env vars configure everything at runtime; the test-only ones are `POLLYTOOL_REQUIRE_SANDBOX_TESTS`, `POLLYTOOL_SANDBOX_RECIPE_TESTS`, `POLLYTOOL_CLIPBOARD_TEST`, `POLLYTOOL_OPENROUTER_LIVE_TEST`, `POLLYTOOL_TEST_LOCK_DATABASE`, `POLLYTOOL_TEST_HOME` (base directory `cmd/polly`'s `TestMain` creates its throwaway test `HOME` under, for sandboxed runs that deny home writes).

## Repo etiquette

- Commits: `area: lowercase imperative summary`, no period, subject line only. Areas match package or topic (`llm`, `tools`, `sandbox`, `swarm`, `repl`, `sessions`, `tests`, `docs`, `ci`, ...); multiple areas as a comma list (`sandbox, worktree: ...`).
- Branches: `area/topic` (`swarm/workflow-api-shorthands`, `sandbox/private-home`, `ci/linux-worker-pool`). PRs merge to `main` as merge commits, not squashes.
