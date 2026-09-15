# AGENTS.md

`polly` is an LLM harness: a CLI + TUI (`cmd/polly`) on a Go library. Single module `github.com/alexschlessinger/pollytool`, Go 1.27, stdlib `testing` only, no Makefile.

## Verify before declaring done

```bash
CGO_ENABLED=0 go build ./...   # sqlite is modernc.org/sqlite; cgo stays off
go vet ./...
go test ./...
gofmt -l .                     # must print nothing; no hook or CI step enforces formatting
```

- `.github/ci.sh [test|race|cross|docker|all]` is the shared CI entry point (`docker` is developer-run against a local daemon; `all` does not include it). `test` = local-ci Python unit tests + build + vet + `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 go test ./...`; `race` needs `CGO_ENABLED=1`; `cross` builds 5 GOOS/GOARCH targets including windows/amd64.
- Sandbox security tests are opt-in: `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 go test ./tools/sandbox` (macOS/Linux only). Linux needs `bubblewrap` and `kernel.apparmor_restrict_unprivileged_userns=0`.
- Use `go test -count=1` when re-running after a change you expect to flip a result; nearly all tests are serial.
- Docs are `README.md` (CLI/TUI user guide), `API.md` (Go library reference), `SANDBOX.md` (sandboxing), `WORKFLOWS.md` (swarm/workflow guide; format-2 details in `docs/swarm-state-model.md`). Local CI (Docker/OrbStack + Tart VMs) is documented in `.github/local-ci/README.md` and is specific to one Apple Silicon setup.

## Layout

Flat top-level domain packages, one concept per file. `cmd/polly` holds the CLI and the entire tcell/gotui TUI (`repl_*.go`; `line_*.go` is the dumb-terminal frontend). `llm` is the agent loop and provider router with one package per provider under `llm/<provider>/`, sharing `llm/internal/contract` and `llm/internal/catalog`. `tools` holds `Tool`/`ToolRegistry` and the builtins; `tools/sandbox` is the policy engine (bubblewrap on Linux, Seatbelt on macOS, heavily platform-split). `sessions` is the SQLite store; `swarm`, `workflow` (goja JS), `worktree`, `subagent` are the multi-agent stack; `messages`, `schema`, `skills`, `artifacts`, `images`, `internal/*` are support.

Request flow: `main` → provider router (`llm.NewMultiPass`) → `llm.NewAgent` → agent loop. Each iteration `llm.Prepare` adapts the request to the model's capabilities, then `ChatCompletionStream` routes on the `provider/` prefix; each provider package builds the wire request, converts messages, parses stream events through its adapter, and fetches its own model catalog and embeddings. Tool calls run in parallel via `tools.ToolRegistry` and results are fed back until done or `ErrMaxIterations`.

Key types: `llm.LLM`/`Agent`/`AgentCallbacks`, `messages.ChatMessage`/`StreamEvent`, `tools.Tool`/`ToolRegistry`/`ToolError`, `schema.ToolSchema`, `sessions.Store`, `subagent.Runner`.

`tools/docker` is the container tool backend: `tools/docker/protocol` is the wire format, `tools/docker/helper` the in-container side, and the package root the host side (session, proxies, mirror, copy-mode sync, prune). `docker/` holds the reference Dockerfiles `polly sandbox build` embeds. `experiments/textfx` and `experiments/windowfx` are throwaway TUI experiments. `.agents/skills/polly-tui/` (SKILL.md + `driver.sh`) is the sanctioned way to drive and screenshot the TUI (tmux headless, or WezTerm for real pixel captures).

## Style that differs from Go defaults

- Errors: `fmt.Errorf` with `%w`. Tool failures return `*tools.ToolError` (structured, JSON-serialized).
- Functional options (`With*`) for registries and clients.
- Tests: `t.Fatalf`, `t.TempDir()`, plain assertions. No testify, no mocks frameworks, no golden-file framework.
- Platform-split files (`*_darwin.go`, `*_linux.go`, `*_windows.go`, `*_unix.go`, `*_other.go`): a change usually belongs in every variant; check all of them.
- Only `defaultProviders()` in `llm/multipass.go` may name a provider; every routing rule is a `providerSpec` field there.

## Adding things

- Provider: `llm/<provider>/` exporting `NewProvider`, `ListModels` (on `llm/internal/catalog`), optionally `Embed` and `DefaultBaseURL`; one row in `defaultProviders()` wires it and carries every routing rule as a `providerSpec` field (base URL scoping, keyless access, host routing, catalog shape); env key `POLLYTOOL_<PROVIDER>KEY` via `getEnvVarNameForProvider`. Update `API.md` §Providers and `README.md` §Models.
- Builtin tool: implement `tools.Tool` in `tools/<name>.go` (or a declarative `tools.Func`); register in `installNativeTools` (`tools/native_tools.go`, reached only through `WithNativeTools`) or via `RegisterNative`; a generic registry and `Derive` never install native tools. Rich output implements `OutputTool`; long-running exemption is `UntimedTool`. Anything that spawns a process goes through the sandbox factory; anything that touches paths policy-checks like the builtin file tools. Update `README.md` §Built-in tools.
- Sandbox preset or config: update `SANDBOX.md` ("How policies merge").

## Sandbox invariants

Sandboxing is default-on for bash, shell tools, and stdio MCP servers, and fails closed. Tool metadata cannot opt out; home is private except for explicit grants; credential paths are masked everywhere. Default preset is `workspace+net+git`. Never add a code path that runs a child process outside the sandbox factory, and never widen a grant set or weaken a mask without updating `SANDBOX.md`. The one sanctioned exception is the container sandbox (`tools/sandbox/container_unix.go`): it runs children directly because the container is the boundary, and it is constructible only in helper mode, entered solely by the hidden `polly sandbox helper` command (`cmd/polly/sandbox_cmd.go`). The Docker daemon is reached through its Engine API, never by running the docker CLI; the only host process that runs docker is the explicit `polly sandbox build` command. Polly refuses to start when cwd is the real `$HOME` or when `HOME` is under `/tmp`, and needs `git` on PATH.

## Gotchas

- Tool wrappers must preserve media output: wrap with `NamespacedTool.ExecuteOutput`; do not drop `OutputTool` semantics.
- Provider quirks are intentional: reasoning models reject `temperature`; OpenAI reasoning items are model-locked and dropped on model switch; Anthropic has a legacy vs adaptive thinking split (`legacyThinkingPrefixes`).
- Never commit the `polly` binary at the repo root, the `textfx`/`windowfx` binaries that `go build ./experiments/...` drops there, or runtime data. Runtime state lives in `~/.pollytool/` (`polly.db`, `skills/`, `worktrees/`).
- Scope searches to the repo root and exclude gitignored paths
- `POLLYTOOL_*` env vars configure everything at runtime; the test-only ones are `POLLYTOOL_REQUIRE_SANDBOX_TESTS`, `POLLYTOOL_REQUIRE_DOCKER_TESTS` (with `POLLYTOOL_DOCKER_TEST_IMAGE` and `POLLYTOOL_DOCKER_TEST_GIT_IMAGE`; developer-run against a local daemon, the local CI workers have no socket), `POLLYTOOL_CLIPBOARD_TEST`, `POLLYTOOL_OPENROUTER_LIVE_TEST`, `POLLYTOOL_TEST_LOCK_DATABASE`.

## Repo etiquette

- Commits: `area: lowercase imperative summary`, no period, subject line only. Areas match package or topic (`llm`, `tools`, `sandbox`, `swarm`, `repl`, `sessions`, `tests`, `docs`, `ci`, ...); multiple areas as a comma list (`sandbox, worktree: ...`).
- Branches: `area/topic` (`swarm/workflow-api-shorthands`, `sandbox/private-home`, `ci/linux-worker-pool`). PRs merge to `main` as merge commits, not squashes.
