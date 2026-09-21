# Pollytool (polly)

My [LLM](https://en.wikipedia.org/wiki/Stochastic_parrot) harness.
There are many like it. This one is mine.

A terminal assistant with tools, saved conversations, and subagents.
Use the full-screen TUI, pipe it a prompt, or embed it as a Go library.

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [One-shot output](#one-shot-output)
- [TUI](#tui)
- [Sessions](#sessions)
- [Models](#models)
- [Tools](#tools)
- [Skills](#skills)
- [Themes](#themes)
- [Sandboxing](#sandboxing)
- [Documentation](#documentation)

## Install

Build from this checkout with Go 1.27 or later. Git must be on your path.

```bash
go build -o polly ./cmd/polly/
```

Put the binary on your `PATH`, or run `./polly`. Start it from your project
directory.

## Quick start

```bash
polly                                      # open the TUI
polly -m openai/gpt-5.4 -p "Explain this repo"
echo "Hello?" | polly                       # prompt from stdin
polly -f image.jpg -p "What's in this image?"
```

The first launch opens setup: choose a provider, model, and key. It is a form
in the TUI and line-by-line questions elsewhere. Reopen it with `/setup` or
`polly --setup`; `polly --setup` with `--model`, `--effort`, `--theme` and
`--sandbox` or `--nosandbox` saves those without asking. The default model is
`anthropic/claude-opus-5`.

Set your provider's key in the environment, for example
`POLLYTOOL_ANTHROPICKEY`. Keys typed into `/setup` or `/keys` last only for
that process; Polly never saves them. Ollama and custom `--baseurl` endpoints
can run without a key.

Other defaults live in `~/.pollytool/config`. **Flags override environment
variables, which override the config file.** Saved sessions keep their own
settings unless you pass a flag.

[Setup and configuration details →](docs/CLI.md#first-run)

## One-shot output

Use `-p`, `polly ask "…"`, or piped stdin for a single turn.
The answer goes to stdout; live activity goes to stderr.
Redirected answers are raw Markdown.

| Flag | Effect |
|---|---|
| `--stream` | Print text as it arrives |
| `--quiet` | Hide activity |
| `--activity-details` | Include thought, tool, agent, and image summaries |
| `--meta` | Emit a `polly-meta` record |

### Structured output

Pass `--schema person.schema.json` for validated JSON on stdout.
Image attachments work here too.

[Output details →](docs/CLI.md#one-shot-output)

## TUI

Run `polly` without a prompt or piped stdin. Under `TERM=dumb` or a redirect,
Polly uses a line frontend instead.

![polly TUI](.assets/interactive.png)

Click a tool, thought, or agent row to inspect it. Click the status bar's
session name to switch sessions, its model to change models, or its change
count to see the workspace diff.

| Key | Action |
|---|---|
| `Enter` | Send; input during a turn queues for later |
| `Tab` | Accept a completion |
| `Ctrl-C` | Interrupt; again, or while idle, quit |
| `Esc` | Dismiss the popup or inspector; otherwise interrupt |
| `Ctrl-R` / `Ctrl-G` | Search history / pick a session |
| `Ctrl-O` | Expand or collapse inline details |
| `Ctrl-V` | Attach a clipboard image |
| `Alt+1`…`9` | Switch tabs |

Type `@` to attach a workspace file, or start with `/` for commands and skills.
Drag files into the composer to attach them. Use Shift-drag to select text.

A few commands to know:

| Command | Action |
|---|---|
| `/help` | Browse commands |
| `/model`, `/keys`, `/setup` | Change the model, key, or defaults |
| `/new`, `/resume`, `/close` | Open, resume, or close a tab |
| `/inspect` | Open the inspector |
| `/theme` | Preview and switch themes |
| `/sandbox-init` | Set up this project's sandbox |

### Subagents

Ask Polly to delegate, or use `/spawn [--read-only] [--review] <brief>`.
Research agents return findings; editing agents work in isolated Git snapshots
for the parent to integrate. Git 2.40+ is required.

Click **Agents** in the status bar to follow progress or answer an approval request.
Use `/workflow SCRIPT.js INPUT.json` for JavaScript workflows.

[TUI guide →](docs/CLI.md#tui) · [Swarms and workflows →](docs/WORKFLOWS.md)

## Sessions

TUI conversations save automatically. Generated names such as `quiet-otter`
expire after seven idle days; named sessions never expire. One-shot runs are
stateless unless you supply `-c`.

```bash
polly -L                              # resume the last session
polly -c project                      # open a named session
polly -c project -p "Continue"         # continue it from the CLI
cat notes.txt | polly -c project --add # add context without a model call
```

Use `/title` to set a title and `/rename` to change the session's handle.
Sessions live in `~/.pollytool/polly.db`.

[Session settings and commands →](docs/CLI.md#contexts)

## Models

Choose with `-m provider/model`, `POLLYTOOL_MODEL`, or `/model`.

| Provider prefix | API key environment variable |
|---|---|
| `openai/` | `POLLYTOOL_OPENAIKEY` |
| `anthropic/` | `POLLYTOOL_ANTHROPICKEY` |
| `gemini/` | `POLLYTOOL_GEMINIKEY` |
| `qwencloud/` | `POLLYTOOL_QWENCLOUDKEY` |
| `deepseek/` | `POLLYTOOL_DEEPSEEKKEY` |
| `openrouter/` | `POLLYTOOL_OPENROUTERKEY` |
| `ollama/` | `POLLYTOOL_OLLAMAKEY` (optional) |
| `huggingface/` | `POLLYTOOL_HUGGINGFACEKEY` |

The model picker completes discovered names; you can always type one manually.
`--baseurl` selects an OpenAI-compatible or Ollama endpoint.

Reasoning effort defaults to `high`; change it with `--effort` or `/set effort`.
Context limits are detected when possible; use `/set maxcontext` to adjust them.

[Model discovery, routing, and limits →](docs/CLI.md#models)

## Tools

### Built-in tools

Polly can run Bash, read and edit files, delegate to agents, view images, and
recall earlier work. The default tools are:

| Purpose | Tools |
|---|---|
| Shell and files | `bash`, `read_file`, `write_file`, `edit_file`, `list_dir` |
| Agents and session | `spawn_agent`, `set_session_title` |
| Images and themes | `view_image`, `set_theme` (TUI only) |
| Recall | `list_artifacts`, `read_artifact`, `read_transcript` |

Use `--confirm` to approve each tool call. Pass `-t` / `--tool` to choose your
own set; **any `--tool` replaces the defaults**. Repeat it for multiple tools.

### Shell tools

An executable becomes a tool by answering `--schema` with JSON Schema and
`--execute <json-args>` with its result. Load it with `-t ./uppercase.sh`.

[Shell tool example →](docs/CLI.md#shell-tools)

### MCP servers

Load a Claude Desktop-format config with `-t mcp.json`, or select one server
with `-t mcp.json#filesystem`. Tools appear as `server__tool`.
Stdio servers follow the session's sandbox policy; SSE and streamable HTTP
servers run separately.

[Tool behavior and configuration →](docs/CLI.md#tools)

## Skills

Skills are folders containing a `SKILL.md`. Keep yours in
`~/.pollytool/skills`, choose another directory with `--skilldir`, or load one
with `-S <dir|git|url>`. In the TUI, start a prompt with `/name` to activate it.

Four skills ship with Polly:

| Skill | Use it for |
|---|---|
| `feature-workflow` | Plan, research, implement, and review a feature |
| `simplify` | Simplify changes without changing behavior |
| `theme-designer` | Create a theme with the colors you want |
| `sandbox-setup` | Prepare builds and tests through `/sandbox-init` |

Your own skill shadows a built-in with the same name. Use `--listskills` to
list them or `--noskills` to disable skills.

[Skill loading and activation →](docs/CLI.md#skills)

## Themes

Use `/theme` to preview and switch themes, or `--theme <name>` at launch.
The default follows your terminal's palette. Four full themes also ship:
`amber-parrot`, `azure-parrot`, `midnight-parrot`, and `verdant-parrot`.

User themes live in `~/.pollytool/themes/`. Editing the active file reloads it
in the TUI. Ask the `theme-designer` skill to make one, or write the JSON yourself.

[Theme files, colors, and roles →](docs/CLI.md#themes)

## Sandboxing

**Sandboxing is opt-in.** With no sandbox policy from flags, environment, or
config, tool commands run unsandboxed.

```bash
polly --sandbox default                   # workspace + network + Git writes
polly --sandbox readonly                  # no writes or network
polly --sandbox default+private-home      # also hide ungranted home paths
```

Under a sandbox, ordinary home files remain readable. Known credential paths
and Polly's internal storage stay masked; home writes need a specific grant.
Add `private-home` to hide the rest of home.

Use `--add-dir ../shared` for an extra read-only project directory.
In the TUI, `/sandbox-init` prepares isolated build storage, runs the project's
builds and tests, and records verified commands in `AGENTS.md`. `/sandbox` shows
the profile; `/sandbox try <command>` helps diagnose a denied operation.

[Presets and everyday commands →](docs/SANDBOX.md#cli-presets) ·
[Policies and platform details →](docs/SANDBOX.md)

## Documentation

`polly --help` lists all flags. Most also have a `POLLYTOOL_*` environment
variable.

[Documentation index →](docs/README.md)

| Guide | Covers |
|---|---|
| [CLI and TUI](docs/CLI.md) | Setup, commands, attachments, models, tools, and themes |
| [Swarms and workflows](docs/WORKFLOWS.md) | Delegation, review, integration, and JavaScript workflows |
| [Sandboxing](docs/SANDBOX.md) | Permissions, workspace profiles, and platform behavior |
| [Go API](docs/API.md) | Embed Polly in your own program |

## See also

[Soulshack](https://github.com/pkdindustries/soulshack): IRC chatbot built on Polly.

## License

MIT
