# AGENTS.md

Guidance for coding agents working in this repo. README.md is the CLI/TUI user guide; API.md is the Go library reference; SANDBOX.md covers sandboxing. Consult and update them as noted below.

## Overview

`polly` is an LLM harness: CLI + TUI (`cmd/polly`) built on a Go library (`llm`, `tools`, `messages`, `schema`, `sessions`, `skills`, `subagent`, `artifacts`). Module: `github.com/alexschlessinger/pollytool`, Go 1.27.

Request flow: `main` → provider client (`llm.New*Client`) → `llm.NewAgent` → agent loop (`ChatCompletionStream`, stream events parsed per-provider in `llm/adapters/`) → tool calls run in parallel via `tools.ToolRegistry` → results fed back until done or `ErrMaxIterations`.

Key types: `llm.LLM`/`Agent`/`AgentCallbacks`, `messages.ChatMessage`/`StreamEvent`, `tools.Tool`/`ToolRegistry`/`ToolError`, `schema.ToolSchema`, `sessions.Store`, `subagent.Runner`.

## Build & test

```bash
CGO_ENABLED=0 go build ./...     # no cgo: sqlite is modernc.org/sqlite
go vet ./...
go test ./...                    # tests are *_test.go colocated with code
```

CI (`.github/workflows/test.yml`) runs build + vet + tests on linux/macOS/windows, cross-compiles 5 GOOS/GOARCH targets, and runs `-race` on `./sessions ./cmd/polly ./llm`. Verify all three commands above before declaring done.

- Sandbox security tests are opt-in: `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 go test ./tools/sandbox` (macOS/Linux only).
- POSIX-only tests skip via a per-package `skipIfWindows(t)` helper (see `tools/skip_test.go`), not build tags. Use it for anything needing POSIX shell/sandboxing so the Windows CI leg passes.
- `POLLYTOOL_*` env vars configure everything (`POLLYTOOL_ANTHROPICKEY`, `MODEL`, `SANDBOX`, etc.); test-only ones: `POLLYTOOL_REQUIRE_SANDBOX_TESTS`, `POLLYTOOL_CLIPBOARD_TEST`.

## Conventions

- Commits: `area: lowercase imperative summary`, no period. Areas match package/topic: `llm`, `tools`, `sessions`, `mcp`, `repl`, `tests`, `skills`, `sandbox`, …
- Packages are flat top-level domains; one concept per file; stdlib `testing` with `t.Fatalf`, `t.TempDir()`, plain assertions.
- Errors: `fmt.Errorf` with `%w`; tool failures return `*tools.ToolError` (structured, serialized as JSON).
- Functional options for registries/clients (`With*`).
- Platform-split files (`*_darwin.go`, `*_windows.go`, `*_unix.go`, `*_other.go`) — check whether a change belongs in all variants.

## Adding things

- **Provider**: client in `llm/<provider>/`, wrapper in `llm/<provider>.go`, factory registered in `llm/defaultProviderFactories()` (`llm/multipass.go`), env key `POLLYTOOL_<PROVIDER>KEY` via `getEnvVarNameForProvider`. Update API.md §Providers and README §Models.
- **Builtin tool**: implement `tools.Tool` in `tools/<name>.go` (or a declarative `tools.Func`); register in `NewToolRegistry` (`tools/registry.go`) or via `RegisterNative`. Rich output → `OutputTool`; long-running exemption → `UntimedTool`. If it spawns processes it must go through the sandbox factory; if it touches paths it must policy-check like the builtin file tools. Update README §Built-in tools.
- **Sandbox preset/config**: update SANDBOX.md (see its "How policies merge" section).

## Sandbox rules

Sandboxing is default-on for bash, shell tools, and stdio MCP servers (bubblewrap on Linux, Seatbelt on macOS); it fails closed, tool metadata cannot opt out, and credential paths are denied by default. Never add a code path that runs a child process outside the sandbox factory, and never weaken deny-list behavior without updating SANDBOX.md.

## Gotchas

- Do not commit the `polly` binary (34 MB, gitignored at repo root) or runtime data.
- `.gopath/` (~1.4 GB) is a local GOPATH cache, gitignored — not repo source. `.claude/worktrees/`, `.polly-shots/`, `.zvec-grep/`, `.tmux-tmp/` are scratch; work from the repo root or greps will hit duplicate trees. Agent-facing skills live in `.agents/skills/`.
- `skills.Message` is deliberately field-order-compatible with `messages.ChatMessage` to break an import cycle — do not reorder fields.
- Tool wrappers must preserve media output: use `NamespacedTool.ExecuteOutput` when wrapping; don't drop `OutputTool` semantics.
- Provider quirks are intentional: reasoning models reject `temperature`; OpenAI reasoning items are model-locked and dropped on model switch; Anthropic has a legacy vs adaptive thinking split (`legacyThinkingPrefixes`).
