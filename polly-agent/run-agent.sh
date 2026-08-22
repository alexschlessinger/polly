#!/usr/bin/env bash
# run-agent.sh <id> <prompt> [model] [tool]
#
# Launch ONE headless polly sub-agent and record its result + status.
# A <prompt> of "-" reads the prompt from stdin instead of argv, so callers
# can stream arbitrarily large prompts (no ARG_MAX limit):
#   run-agent.sh <id> - < task.txt
# Writes under the run directory POLLY_AGENT_DIR (default ${TMPDIR:-/tmp}/polly-agent):
#   <run-dir>/out/<id>.txt        the answer, and only the answer (agent stdout)
#   <run-dir>/err/<id>.log        tool progress + the polly-meta outcome trailer (agent stderr)
#   <run-dir>/status/<id>.tsv     this agent's row: <id> <TAB> <exit> <TAB> ok|fail
#   <run-dir>/status.tsv          all rows, rebuilt after this agent finishes
# Each agent owns its status/<id>.tsv row file, so parallel runs never contend
# (no flock — it doesn't exist on macOS); a re-run overwrites that agent's row.
# status.tsv is rebuilt (atomic rename) after each agent finishes; when racing
# reads against still-running agents, read status/*.tsv instead.
# Extract a run's outcome record from its err log with:
#   sed -n 's/^polly-meta //p' <run-dir>/err/<id>.log
# (fields: stop_reason, model, iterations, tool_calls, tool_errors, last_tool,
#  tool_error.N, input_tokens, output_tokens, duration_ms, error)
# Prints the run directory to stderr. Exits with the agent's own exit code:
#   0 ok | 2 truncated (max_tokens) | 3 max-iterations | 1 error | 130 interrupted.
# Usage errors exit 64 so they can't be mistaken for a truncated run.
#
# Defaults: model deepseek/deepseek-v4-flash, tool bash.
# Override the polly binary with POLLY_BIN (default: ./polly).
# Override the run directory with POLLY_AGENT_DIR.
set -uo pipefail

if [ $# -lt 2 ]; then
  echo "usage: run-agent.sh <id> <prompt> [model] [tool]  (prompt '-' reads stdin)" >&2
  exit 64
fi

id=$1
prompt=$2
model=${3:-deepseek/deepseek-v4-flash}
tool=${4:-bash}
polly=${POLLY_BIN:-./polly}
dir=${POLLY_AGENT_DIR:-${TMPDIR:-/tmp}/polly-agent}

mkdir -p "$dir/out" "$dir/err" "$dir/status"

# NEVER pass --confirm: headless there is no TTY to answer it, so it would hang.
# --quiet drops progress chrome; the answer still goes to stdout (-> out/<id>.txt).
run_polly() {
  "$polly" -m "$model" --tool "$tool" --quiet --meta \
    > "$dir/out/$id.txt" 2> "$dir/err/$id.log"
}
if [ "$prompt" = "-" ]; then
  run_polly
else
  printf '%s' "$prompt" | run_polly
fi
rc=$?

verdict=$([ "$rc" -eq 0 ] && echo ok || echo fail)
printf '%s\t%s\t%s\n' "$id" "$rc" "$verdict" > "$dir/status/$id.tsv"

# Rebuild the combined view from the per-agent rows. The unique temp name plus
# atomic rename keeps concurrent rebuilds from corrupting the file; the last
# agent to finish writes the complete set.
tmp="$dir/.status.tsv.$$"
sort "$dir/status/"*.tsv > "$tmp" 2>/dev/null && mv "$tmp" "$dir/status.tsv"

echo "$dir" >&2
exit "$rc"
