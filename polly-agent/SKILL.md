---
name: polly-agent
description: Use when you need to delegate a task to a separate LLM sub-agent — spawn a headless, one-shot Polly CLI agent (optionally with its own tools/MCP/skills) and capture its result from stdout. Use for fan-out, isolating a noisy task, or running a task on a different model.
---

## Requirements

```bash
cd /home/alex/workspace/polly && go build -o polly ./cmd/polly/
```

## Keys

```bash
export POLLYTOOL_ANTHROPICKEY="sk-ant-..."
export POLLYTOOL_OPENAIKEY="sk-..."
export POLLYTOOL_GEMINIKEY="..."
export POLLYTOOL_DEEPSEEKKEY="..."
export POLLYTOOL_OPENROUTERKEY="..."
export POLLYTOOL_HUGGINGFACEKEY="..."
export POLLYTOOL_OLLAMAKEY="..."  # optional; ollama is keyless
```

The two scripts (`run-agent.sh`, `fanout.sh`) live in this skill's directory.
Either `cd` here and run them as `./run-agent.sh`, or call them by absolute path.
Either way, point `POLLY_BIN` at the binary you built (the scripts default to `./polly`,
which only exists in the repo root):

```bash
export POLLY_BIN=/home/alex/workspace/polly/polly
```

Results are written to a **run directory**, not the current directory — default
`${TMPDIR:-/tmp}/polly-agent`, override with `POLLY_AGENT_DIR`. Set it once and
reuse it for reads:

```bash
export POLLY_AGENT_DIR=${POLLY_AGENT_DIR:-${TMPDIR:-/tmp}/polly-agent}
```

## Launch one sub-agent

```bash
./run-agent.sh <id> "<prompt>"            # e.g. ./run-agent.sh sumdocs "summarize ./docs"
```

This runs ONE headless sub-agent and writes, under `$POLLY_AGENT_DIR`:

- `out/<id>.txt` — the answer, nothing else
- `err/<id>.log` — tool progress + the `polly-meta` outcome trailer
- `status/<id>.tsv` — this agent's row: `<id> <TAB> <exit> <TAB> ok|fail`
- `status.tsv` — all rows combined, rebuilt as each agent finishes

Read the outcome: `sed -n 's/^polly-meta //p' err/<id>.log`. Keys: `stop_reason`
`model` `iterations` `tool_calls` `tool_errors` `last_tool` `tool_error.N`
(first 10, `name: message`) `input_tokens` `output_tokens` `duration_ms` `error`.

Optional 3rd/4th args set the model and tool: `./run-agent.sh <id> "<prompt>" anthropic/claude-opus-4-8 bash`.
Defaults are `deepseek/deepseek-v4-flash` and the `bash` tool. A `<prompt>` of
`-` reads the prompt from stdin (`./run-agent.sh big - < task.txt`), so huge
prompts don't hit the argv size limit. You can run many of these in parallel —
each agent owns its `status/<id>.tsv` row file, so there is no contention.

The `bash` tool runs in a bubblewrap sandbox (credential dirs like `~/.ssh` are
masked). If the sandbox can't start (e.g. `bwrap` missing or blocked), polly
**fails fast**: the agent exits non-zero before any LLM call with
`Error: sandbox requested but failed to start: ...`. The scripts take no
`--nosandbox` arg; to run without the sandbox, set the env var, which passes
straight through:

```bash
export POLLYTOOL_NOSANDBOX=1   # only when you trust the sub-agent's commands
```

```bash
./run-agent.sh greet "say hello in five words"
cat "$POLLY_AGENT_DIR/out/greet.txt"     # the answer
cat "$POLLY_AGENT_DIR/status.tsv"        # greet  0  ok
```

## Fan out many sub-agents

Put one task per file in a **dedicated, empty** directory (file name = id, `.txt`
optional; contents = prompt), then point `fanout.sh` at it:

```bash
mkdir /tmp/mytasks
echo "summarize ./README.md"        > /tmp/mytasks/readme.txt
echo "list the open TODOs in ./src" > /tmp/mytasks/todos.txt

./fanout.sh /tmp/mytasks
```

`fanout.sh` runs **every** regular file in the dir as an agent — don't reuse a dir
that holds other files (e.g. the repo's own `tasks/`). Prompts are streamed to
each agent over stdin, so task files of any size work. If stripping `.txt`
would collide two ids (files `report` and `report.txt`), the second keeps its
full file name as the id.

`fanout.sh` runs all tasks in parallel, prints the run directory (stderr) and
`status.tsv` (stdout), and exits `0` only when every agent succeeded (`1`
otherwise, `64` on usage errors):

```
readme	0	ok
todos	0	ok
```

## Read the results

`status.tsv` always has one row per agent (written by `run-agent.sh`, started
fresh by `fanout.sh`), under `$POLLY_AGENT_DIR`:

```bash
cd "$POLLY_AGENT_DIR"                            # or prefix the paths below with it
cat status.tsv                                  # id <TAB> exit <TAB> ok|fail
awk -F'\t' '$3=="fail"{print $1}' status.tsv    # ids that failed
sed -n 's/^polly-meta //p' err/<id>.log         # an agent's outcome record
cat out/<id>.txt                                # an agent's answer
```

- Exit codes (also in `status.tsv`): `0` ok · `2` truncated (`max_tokens`) ·
  `3` iteration cap (`max_iterations`) · `1` hard error · `130` interrupted ·
  `64` script usage error (never recorded in `status.tsv` — the agent never ran).
  Find iteration-capped agents with `awk -F'\t' '$2==3' status.tsv`.
- **Exit 0 ≠ correct.** Check the trailer: `tool_errors>0` means the agent hit
  tool failures (and may have thrashed); `tool_error.N` says which and why (even
  under `--quiet`); `last_tool` is where it stopped. Then read `out/<id>.txt`.
- A `fail` row may still have useful **partial** output in `out/<id>.txt` — e.g. a
  max-iterations run returns its work so far before exiting non-zero.

## Raw CLI (when the scripts aren't enough)

The scripts wrap a one-shot `polly` call with the safe defaults baked in. To do
anything they don't cover (MCP servers, skills, structured output, system prompt),
call `polly` directly. **Headless, never pass `--confirm`** — there is no TTY to
answer it and the agent would hang. The answer goes to stdout, progress to stderr.

Use `"$POLLY_BIN"` (set above) so these work from any directory.

```bash
# capture the answer directly
result=$("$POLLY_BIN" -p "summarize this repo" --tool bash --quiet)

# feed a file/image/URL into the prompt instead of inlining it (repeatable)
"$POLLY_BIN" -p "summarize this" --file ./big-report.md --quiet

# structured output: --schema prints ONLY JSON to stdout (standard JSON Schema;
# `required` and `additionalProperties` are honored)
"$POLLY_BIN" -m anthropic/claude-sonnet-4-6 -p "extract title and author" \
  --schema ./schema.json --quiet > result.json

# MCP server via a JSON config file (optionally #server to pick one)
"$POLLY_BIN" -p "do X" --tool ./mcp.json
# mcp.json: {"mcpServers": {"name": {"command": "cmd", "args": ["a"]}}}

# load a skill (dir/repo/archive; auto-activated)
"$POLLY_BIN" -p "do X" --skilldir ./skills --skill myskill
```

A `--maxtokens` truncation exits `2` (`stop_reason=max_tokens`); the warning goes
to stderr, so `out/<id>.txt` stays clean.

## Key flags

| Flag | Default | Notes |
|------|---------|-------|
| `-m, --model` | `anthropic/claude-sonnet-4-6` | `provider/model` (prefix required) |
| `-p, --prompt` | — | One-shot task; reads stdin if omitted |
| `-f, --file` | — | Include a file, image, or URL in the prompt (repeatable) |
| `-s, --system` | terse-terminal default | System prompt |
| `-t, --tool` | — (none) | Repeatable: `bash`, a script path, or an MCP `.json` |
| `--tooltimeout` | `30s` | Per-tool timeout |
| `--confirm` | false | Prompt before each tool call; **TTY only** (ignored headless, where tools auto-run) |
| `--temp` | `1.0` | Temperature (0–2) |
| `--maxtokens` | `64000` | Max output tokens |
| `--maxiterations` | `250` | Max agent iterations (LLM calls) |
| `--timeout` | `2m` | Per-request timeout |
| `--maxcontext` | `100000` | Max history tokens (0 = unlimited) |
| `--thinkingeffort` | `off` | `off`/`low`/`medium`/`high` |
| `--schema` | — | Path to JSON Schema file → structured JSON output |
| `--skilldir` | — | Skill directory (repeatable) |
| `-S, --skill` | — | Load + auto-activate a skill (dir/repo/archive) |
| `--baseurl` | — | OpenAI-compatible / Ollama endpoint |
| `--quiet` | false | Suppress status and tool-progress display |
| `-d, --debug` | false | Debug logging |
| `--nosandbox` | false | Disable bash sandbox |

## Providers

Models must be prefixed with the provider, e.g. `anthropic/claude-sonnet-4-6`.

| Provider (prefix) | Model examples |
|----------|---------------|
| `anthropic` | `claude-sonnet-4-6`, `claude-opus-4-8`, `claude-haiku-4-5` |
| `openai` | `gpt-5.5`, `gpt-5.4-mini` |
| `gemini` | `gemini-3.5-flash`, `gemini-3.1-flash-lite-preview` |
| `deepseek` | `deepseek-v4-pro`, `deepseek-v4-flash` |
| `openrouter` | provider-specific (OpenAI-compatible) |
| `huggingface` | e.g. `zai-org/GLM-5.1` (OpenAI-compatible) |
| `ollama` | local model names (keyless) |

