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

- `out/<id>.txt` — the answer, and only the answer (clean stdout)
- `err/<id>.log` — tool progress plus the `polly-meta` outcome trailer (stderr)
- a row in `status.tsv` — `<id> <TAB> <exit> <TAB> ok|fail`

The outcome trailer is machine-readable: every line is `polly-meta key=value`.
Extract a run's record with `sed -n 's/^polly-meta //p' err/<id>.log` — fields:
`stop_reason`, `model`, `iterations`, `tool_calls`, `tool_errors`, `last_tool`,
`tool_error.N` (first 10 failures, `name: message`), `input_tokens`,
`output_tokens`, `duration_ms`, and `error` (hard errors only).

Optional 3rd/4th args set the model and tool: `./run-agent.sh <id> "<prompt>" anthropic/claude-opus-4-8 bash`.
Defaults are `deepseek/deepseek-v4-flash` and the `bash` tool. You can run many
of these in parallel — each records its own row in `status.tsv` safely.

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
that holds other files (e.g. the repo's own `tasks/`). Each prompt is passed as a
command-line argument, so a huge task file fails with `Argument list too long`;
keep prompts modest and pass bulk data via `--file` on the raw CLI instead.

`fanout.sh` runs all tasks in parallel, prints the run directory (stderr) and
`status.tsv` (stdout):

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
  `3` iteration cap (`max_iterations`) · `1` hard error · `130` interrupted.
  Find iteration-capped agents with `awk -F'\t' '$2==3' status.tsv`.
- **`ok`/exit 0 still doesn't mean the answer is right.** Read the `polly-meta`
  trailer in `err/<id>.log`: a high `tool_errors` (e.g. every call failed) is the
  machine-readable tell that the agent thrashed and likely gave up — the
  silent-failure signal the exit code can't carry. `tool_error.N` names *which*
  tool failed and why (surfaced even under `--quiet`), and `last_tool` shows what
  it was doing when the turn ended. Then sanity-check `out/<id>.txt`.
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

A `--maxtokens` cap that truncates the answer exits `2` with
`stop_reason=max_tokens` in the trailer; the `Warning: response truncated...`
note goes to stderr, so stdout (`out/<id>.txt`) stays the clean answer.

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
| `--maxtokens` | `50000` | Max output tokens |
| `--maxiterations` | `50` | Max agent iterations (LLM calls) |
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

