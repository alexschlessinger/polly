#!/usr/bin/env bash
# run-agent.sh <id> <prompt> [model] [tool]
#
# Launch ONE headless polly sub-agent and record its result + status.
# Writes under the run directory POLLY_AGENT_DIR (default ${TMPDIR:-/tmp}/polly-agent):
#   <run-dir>/out/<id>.txt   the answer (agent stdout)
#   <run-dir>/err/<id>.log   tool progress + errors (agent stderr)
#   <run-dir>/status.tsv     one row per agent: <id> <TAB> <exit> <TAB> ok|fail  (race-safe upsert)
#   <run-dir>/meta/<id>.txt  outcome sidecar: stop_reason, tool_calls, tool_errors, tokens, duration_ms
# Prints the run directory to stderr. Exits with the agent's own exit code:
#   0 ok | 2 truncated (max_tokens) | 3 max-iterations | 1 error | 130 interrupted.
#
# Defaults: model deepseek/deepseek-v4-flash, tool bash.
# Override the polly binary with POLLY_BIN (default: ./polly).
# Override the run directory with POLLY_AGENT_DIR.
#
# Safe to run many of these in parallel (each upserts its own row under a lock),
# so you can either call this directly in a loop or use fanout.sh.
set -uo pipefail

if [ $# -lt 2 ]; then
  echo "usage: run-agent.sh <id> <prompt> [model] [tool]" >&2
  exit 2
fi

id=$1
prompt=$2
model=${3:-deepseek/deepseek-v4-flash}
tool=${4:-bash}
polly=${POLLY_BIN:-./polly}
dir=${POLLY_AGENT_DIR:-${TMPDIR:-/tmp}/polly-agent}

mkdir -p "$dir/out" "$dir/err" "$dir/meta"

# NEVER pass --confirm: headless there is no TTY to answer it, so it would hang.
# --quiet drops progress chrome; the answer still goes to stdout (-> out/<id>.txt).
printf '%s' "$prompt" | "$polly" -m "$model" --tool "$tool" --quiet \
  --meta-out "$dir/meta/$id.txt" \
  > "$dir/out/$id.txt" 2> "$dir/err/$id.log"
rc=$?

verdict=$([ "$rc" -eq 0 ] && echo ok || echo fail)
# Lock so parallel agents never corrupt status.tsv; upsert by id so a re-run
# replaces that agent's row instead of duplicating it.
(
  flock 9
  if [ -f "$dir/status.tsv" ]; then
    awk -F'\t' -v id="$id" '$1 != id' "$dir/status.tsv" > "$dir/status.tsv.tmp"
  else
    : > "$dir/status.tsv.tmp"
  fi
  printf '%s\t%s\t%s\n' "$id" "$rc" "$verdict" >> "$dir/status.tsv.tmp"
  mv "$dir/status.tsv.tmp" "$dir/status.tsv"
) 9> "$dir/.status.lock"

echo "$dir" >&2
exit "$rc"
