# Machine-readable run outcome for headless polly

**Date:** 2026-06-01
**Status:** Approved design, pending implementation plan

## Problem

The `polly-agent` skill delegates work to headless one-shot `polly` runs and must
answer, programmatically: *did this run actually succeed, and how complete is the
answer?* Today it can't. polly writes the answer to stdout and maps **every**
failure mode (API error, tool-load failure, sandbox failure, max-iterations) to a
single exit code `1`, while a run that "completed" by emitting a refusal exits `0`.
The skill reconstructs outcome from exit code + a hand-rolled `status.tsv`, which
is lossy and produced the "silent `ok`" trap we hit in practice: every bash tool
call failed, the model gave up with a refusal, `stop_reason` was `end_turn`, exit
`0` — nothing flagged it.

The rich per-run data already exists inside the agent loop (`StopReason`,
`IterationCount`, token counts, per-tool errors); it just dies in `executeTurn`
instead of being exposed.

## Goals

- Distinguish run outcomes at the exit-code level (normal / truncated /
  iteration-capped / hard error / interrupted) with zero parsing.
- Expose the richer signals — especially `tool_errors`, the one that would have
  caught our incident — without making the answer brittle.
- Keep the answer plain, streamed, and partial-write-safe on stdout.

## Non-goals (explicitly out of scope)

- **JSON-on-stdout envelope.** Rejected as brittle: it swallows the (large,
  multi-line, freeform) answer into an escaped string, is all-or-nothing on a
  crash/signal/OOM (losing even partial output), forces a `jq` dependency on a
  bash-based skill, and buys nothing structurally since every field is a flat
  scalar.
- **`polly batch` subcommand** (JSONL-in/JSONL-out fan-out). Deferred; would
  build on this work later.

## Design

Two independent vehicles, separated by the nature of the data: a big/streaming/
freeform **answer** vs. small/flat/terminal **outcome metadata**.

### A. Granular exit codes

The conversation runner derives the exit code from the final
`resp.Message.StopReason` plus any agent error, and signals it to `main` via a
typed error:

```go
type exitError struct {
    code int
    err  error // wrapped; Error() delegates to it
}
```

`main` resolves it:

```go
if err := command.Run(...); err != nil {
    var ee *exitError
    if errors.As(err, &ee) {
        fmt.Fprintf(os.Stderr, "Error: %v\n", ee)
        cleanupAndExit(ee.code)
    }
    fmt.Fprintf(os.Stderr, "Error: %v\n", err)
    cleanupAndExit(1) // any other error
}
// nil error -> exit 0
```

| Outcome | `stop_reason` | exit |
|---|---|---|
| normal completion | `end_turn` | `0` |
| output truncated (token cap) | `max_tokens` | `2` |
| iteration cap reached | `max_iterations` | `3` |
| hard error (API / tool-load / sandbox) | `error` | `1` |
| interrupted (SIGINT) | — | `130` (already wired) |

Note `max_tokens` currently returns a *nil* error from `executeTurn` (it only
prints a warning), so the runner must emit `exitError{code: 2}` for it even though
the run "succeeded."

### B. `StopReasonMaxIterations`

Add the enum value to `messages/types.go`. At loop exhaustion
(`llm/agent.go` ~line 290), instead of returning a bare
`errors.New("max iterations exceeded")`:

- set the final message's `StopReason = StopReasonMaxIterations`, and
- return an exported sentinel `ErrMaxIterations` alongside the partial
  `AgentResponse`,

so the runner can `errors.Is(err, llm.ErrMaxIterations)` → `exitError{code: 3}`
and distinguish it from a real error.

### C. `--meta-out FILE` sidecar

A new global flag. When set, `executeTurn` writes a small, fixed-shape outcome
record as flat `key=value` lines — **once, atomically** (write temp + `rename`) —
at the end of the turn. It is written for any run that reaches model execution,
including one that hard-errors mid-turn (`stop_reason=error`). Setup failures that
abort *before* `executeTurn` (sandbox probe, tool-load) make no model call, have
no run outcome to describe, and exit `1` with the reason on stderr but no sidecar.
The sidecar never contains the answer, so a malformed write costs one throwaway
file. `key=value` (not JSON) keeps it shell-native: `grep '^tool_errors=' meta`.

```
stop_reason=end_turn
model=deepseek/deepseek-v4-flash
iterations=3
tool_calls=5
tool_errors=0
input_tokens=1234
output_tokens=567
duration_ms=8123
error=                 # present only when stop_reason=error; the message
```

Field sourcing:

- `stop_reason` — `resp.Message.StopReason` (or `error` when the run hard-failed).
- `model` — `config.Model`.
- `iterations` — `resp.IterationCount`.
- `tool_calls` / `tool_errors` — counted in `executeTurn` via the existing
  `OnToolEnd(call, result, dur, err)` callback: increment `tool_calls` each call,
  `tool_errors` when `err != nil`. This is localized to cmd/polly and does **not**
  alter shared history/`IsError` semantics (which denote terminal stream errors).
- `input_tokens` / `output_tokens` — the max-input / summed-output already
  computed in `executeTurn`.
- `duration_ms` — wall-clock measured around `agent.Run`.
- `error` — only emitted when the run hard-failed; the error string.

`truncated` was considered and dropped as redundant with
`stop_reason=max_tokens`.

`tool_errors` is a count of tool calls that returned an error. A run may
legitimately have `tool_errors > 0` and still be correct (the model retried), so
it is a *signal*, not a verdict — but a high ratio (e.g. all calls failed) is
exactly the silent-`ok` tell. No `last_tool_error` field for now (YAGNI).

### D. Scripts + skill integration

- `run-agent.sh`: feed the prompt via **stdin** (`printf '%s' "$prompt" | polly
  ...`, polly already reads stdin when `-p` is omitted) instead of as an argv
  argument → eliminates `Argument list too long` for large prompts. Pass
  `--meta-out "$dir/meta/<id>.txt"` and `mkdir -p "$dir/meta"`.
- `status.tsv` already records the exit code, so granular codes flow through with
  no new script logic: `awk -F'\t' '$2==3' status.tsv` lists iteration-capped
  agents, `$2==2` truncated, `$2==1` hard-failed.
- `SKILL.md`: document the exit-code table, the per-agent `meta/<id>.txt` sidecar
  and its fields, and reaffirm that the answer is still plain stdout in
  `out/<id>.txt`. Update the "Read the results" section to point at `tool_errors`
  as the machine-readable failure signal.

## Testing (TDD)

Write failing tests first for each unit:

- **Exit-code mapping** — table test over (stop_reason, agent-error) → code:
  `end_turn`→0, `max_tokens`→2, `ErrMaxIterations`→3, generic error→1.
- **`exitError` propagation** — `main`'s `errors.As` selects the carried code;
  unknown errors fall back to 1.
- **`StopReasonMaxIterations`** — agent loop exhaustion sets the stop reason and
  returns `ErrMaxIterations` with partial `AllMessages`.
- **Sidecar writer** — correct fields/values for a representative run; flat
  `key=value` format; atomic (temp file renamed; no partial file on error); `error`
  line present iff hard-failed; written on the error path too.
- **`tool_errors` counting** — callback counter increments on tool error, not on
  success.
- **Integration** — `run-agent.sh` produces a correct `meta/<id>.txt`; a forced
  `--maxtokens` truncation yields exit `2`; a large prompt via stdin does not hit
  argv limits.

## Affected files

- `messages/types.go` — `StopReasonMaxIterations`.
- `llm/agent.go` — set stop reason + return `ErrMaxIterations` at loop exhaustion.
- `cmd/polly/polly.go` — `exitError`, exit-code derivation, `OnToolEnd` counters,
  sidecar write in `executeTurn`, `main` mapping.
- `cmd/polly/config.go` — `--meta-out` flag.
- `polly-agent/run-agent.sh` — stdin prompt, `--meta-out`.
- `polly-agent/SKILL.md` — exit-code table, sidecar docs.
- Tests alongside each.
