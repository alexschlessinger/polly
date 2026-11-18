# AGENTS.md

`polly` is an LLM harness: a CLI + TUI (`cmd/polly`) on a Go library, one module, Go 1.27, stdlib `testing` only, no Makefile.

## Checks

```bash
CGO_ENABLED=0 go build ./...   # sqlite is modernc.org/sqlite; cgo stays off
go vet ./...
go test ./...
gofmt -l $(git ls-files -co --exclude-standard '*.go')   # must print nothing; no hook or CI step enforces formatting
```

That is the full pass; scale it to the change, since a docs-only edit needs none of it.

- `.github/ci.sh [test|race|cross|warm|all]` is the shared CI entry point: `test` adds the local-ci Python tests, `race` needs `CGO_ENABLED=1` and runs about one workflow in five.
- Sandbox security tests are opt-in: `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 go test ./tools/sandbox` (macOS/Linux only). Linux needs `bubblewrap` and `kernel.apparmor_restrict_unprivileged_userns=0`.
- `-count=1` when re-running after an expected flip.

## Layout

Flat top-level domain packages, one concept per file. `cmd/polly` holds the CLI and the whole tcell/gotui TUI (`repl_*.go`, plus the `line_*.go` dumb-terminal frontend). `llm` is the agent loop and provider router; `llm/<provider>/` packages share `llm/internal/contract` and `catalog`. `tools` holds `Tool`/`ToolRegistry` and the builtins; `tools/sandbox` is the policy engine, platform-split by backend. `sessions` is the SQLite store; `swarm`, `workflow` (goja JS), `worktree`, `subagent` are the multi-agent stack.

Request flow: `main` → `llm.NewMultiPass` (routes on the `provider/` prefix) → `llm.NewAgent` → agent loop, where `llm.Prepare` adapts each request and tool calls run in parallel until `ErrMaxIterations`. Details in `docs/API.md`.

`skills/builtin/` holds `go:embed`ded skills (`skills/builtin.go`) that must not reference this repository — they run against arbitrary projects.

Docs start at `docs/README.md`; local CI is `.github/local-ci/README.md`.

## Style that differs from Go defaults

- Errors: `fmt.Errorf` with `%w`. Tool failures return `*tools.ToolError` (structured, JSON-serialized).
- Functional options (`With*`) for registries and clients.
- Tests: `t.Fatalf`, `t.TempDir()`, plain assertions. No testify, mock frameworks, or golden files. They run serially except in `swarm`, where every top-level test calls `t.Parallel()` unless it needs `t.Setenv` or `t.Chdir`.
- Platform-split files (`*_darwin.go`, `*_linux.go`, `*_windows.go`, `*_unix.go`, `*_other.go`): a change usually belongs in every variant; check all of them.

## Adding things

- Provider: `llm/<provider>/` exporting `NewProvider`, `ListModels` (returning `contract.ModelCatalog`), optionally `Embed` and `DefaultBaseURL`; one row in `defaultProviders()` (`llm/multipass.go`) carries every routing rule as a `providerSpec` field and is the only place a provider may be named; env key `POLLYTOOL_<PROVIDER>KEY` via `getEnvVarNameForProvider`. Update `docs/API.md` §Providers and `README.md` §Models.
- Builtin tool: implement `tools.Tool` in `tools/<name>.go` (or a declarative `tools.Func`); register in `installNativeTools` (`tools/native_tools.go`, enabled by `WithNativeTools`) or via `RegisterNative` — generic registry construction and `Derive` install none. Rich output implements `OutputTool`; long-running exemption is `UntimedTool`. Processes go through the sandbox factory; path access policy-checks like the builtin file tools. Update `README.md` §Built-in tools.
- Sandbox preset or config: update `docs/SANDBOX.md` ("How policies merge").

## Sandbox invariants

Sandboxing is opt-in: a policy (`--sandbox`, a path grant, `--allownet`, from a flag, env or config) is what asks for one; a launch nothing asked runs bash, shell tools and stdio MCP servers unsandboxed. Once asked for it fails closed: tool metadata cannot opt out, home is readable with restricted writes, `private-home` hides home, credential paths and runtime storage stay masked, and a policy naming no preset gets `workspace+net+git`. Never run a child process outside the sandbox factory; never widen a grant set or weaken a mask without updating `docs/SANDBOX.md`. A sandbox policy rejects a workspace that is the home directory, a filesystem or volume root, or a private sandbox root; the git preset accepts only the system or Homebrew `git`.

## Gotchas

- Tool wrappers must preserve media output: wrap with `NamespacedTool.ExecuteOutput`; do not drop `OutputTool` semantics.
- Provider quirks are intentional: reasoning models reject `temperature`; OpenAI reasoning items are model-locked and dropped on model switch; Anthropic has a legacy vs adaptive thinking split (`legacyThinkingPrefixes`).
- Never commit the `polly` binary at the repo root, the `textfx`/`windowfx` binaries `go build ./experiments/...` drops there, or runtime data. Runtime state lives in `~/.pollytool/` (`polly.db`, `polly.db-log/`, `skills/`, `themes/`, `worktrees/`); member scratch defaults to `/private/tmp/polly-<uid>/` on macOS and `$TMPDIR/polly-<uid>/` elsewhere, outside the private home. `set_theme` is the sanctioned writer of `themes/`, which is not a home read grant.

## Repo etiquette

- Commits: `area: lowercase imperative summary`, no period, subject line only; areas match package or topic, comma-listed when multiple.
- Branches: `area/topic`. PRs merge to `main` as merge commits, not squashes.
