# Control backends

Polly normally runs every turn in its own process. With `--backend URL` it
runs them in a daemon that speaks the JSON-RPC 2.0 *control contract*
instead (the reference daemon is `polly-zig serve`), and reads every session
through that daemon. The TUI, the one-shot CLI and the line REPL work the
same way. Polly reads none of the backend's files, so a daemon on another
machine, reached through a forwarded socket, works just like a local one.

Without `--backend` (or `POLLYTOOL_BACKEND`), nothing changes: polly runs its
own engine and store, as always.

## Contents

- [Choosing a backend](#choosing-a-backend)
- [What runs where](#what-runs-where)
- [Following and stopping a turn from elsewhere](#following-and-stopping-a-turn-from-elsewhere)
- [What a backend session does not do](#what-a-backend-session-does-not-do)
- [How it works](#how-it-works)

## Choosing a backend

| URL | Meaning |
|---|---|
| `exec:///abs/path/to/polly-zig` | Find the daemon of that binary, or start it |
| `exec://polly-zig` | The same, with the binary found on `PATH` |
| `unix:///path/to/serve.sock` | Dial a daemon's socket that is already there, local or forwarded |

```bash
polly --backend exec://polly-zig                          # the TUI
polly --backend exec://polly-zig -p "summarize this repo"  # one shot
export POLLYTOOL_BACKEND=exec://polly-zig                  # every launch
```

An `exec://` launch finds the binary's daemon the way the binary's own CLI
does, so `polly --backend exec://polly-zig` and `polly-zig` share one daemon
and, for the same setup, the same worker. The daemon's socket lives in a
directory of the user's own: on macOS under the system's per-user temporary
directory (`getconf DARWIN_USER_TEMP_DIR`, which `$TMPDIR` does not move), on
Linux under `$XDG_RUNTIME_DIR`, else `/tmp/polly-zig-<uid>`. The daemon's store
is `POLLYTOOL_RUNTIME_DIR`, else `~/.polly-zig`.

A `unix://` URL skips all of that: polly dials the socket and asks the daemon
to start the setup if it has to. For a daemon on another machine, give the
paths that are valid there as query parameters: `runtime=` (its store),
`dir=` (where the setup works) and `home=`.

```bash
ssh -N -L /tmp/far.sock:/run/user/1000/polly-zig/1-<key>.sock box &
polly --backend 'unix:///tmp/far.sock?runtime=/home/me/.polly-zig&dir=/home/me/src/app&home=/home/me'
```

The backend serves `anthropic`, `openai`, `openrouter`, `deepseek` and
`huggingface` models. Each provider's key comes from its usual
`POLLYTOOL_<PROVIDER>KEY` variable. It crosses once, in the line that starts
the provider's worker. A `/model` switch to another provider reaches a second
worker on the same daemon.

`--workspace TARGET` (`POLLYTOOL_WORKSPACE`) runs the backend's tools somewhere
else: `ssh:HOST[:DIR]`, `docker:CONTAINER[:DIR]`, or `local`.

## What runs where

The model calls, the tools, child agents, compaction and the transcript all
belong to the backend. Polly shows what the backend reports as it happens:
streamed reasoning and text, tool calls with their arguments, tool results
and images, compaction notices, retries, child agents, and token and context
usage. Each turn's settings come from polly: the model, the system prompt,
the iteration, token and context limits, reasoning effort, tool timeout and
`--tool` selections. They are not saved with the backend's session, so a
resumed session runs with the settings of the launch that resumes it.

| Feature | With `--backend` |
|---|---|
| `--list`, `--show`, `--export` | Read through the backend |
| `/sessions`, Ctrl-S, `/new` | Backend sessions |
| `/model`, `/set`, `/effort`, `/context`, `/copy`, `/theme`, `/help` | Work as usual |
| Child agents | Shown on their `spawn_agent` rows and in the Agents panel; a child can be stopped on its own |
| Images | Composer images go to the backend inline; tool images are read back through it |
| Ctrl-C / Esc in a turn | Stops the turn in the backend; what it finished is kept |

## Following and stopping a turn from elsewhere

A turn belongs to the backend, not to the launch that asked for it. If the
launch quits or loses its connection, the turn keeps running. A launch that
loses its connection reconnects, catches up from the session and goes on.

```bash
polly --backend exec://polly-zig --attach fix-tests   # follow its running turn
polly --backend exec://polly-zig --stop fix-tests     # stop it
```

Leave out the name and `--attach` and `--stop` use the last session. Ctrl-C
detaches an attach and leaves the turn running. `--stop` stops that one turn,
named by where its lines begin, so a stop that arrives late never stops the
session's next turn.

Opening a session in the TUI while its turn runs elsewhere follows that turn
live, as if it had been asked in that tab. Esc stops it, and the composer
frees up when it ends.

## What a backend session does not do

Polly fails closed on what only its own engine provides:

- **Sandboxing.** Polly's sandbox does not reach the backend's tools, so a
  sandbox policy (`--sandbox`, `--writepath`, `--readpath`, `--denypath`,
  `--allownet`, `POLLYTOOL_SANDBOX`, or a saved sandbox setting) refuses the
  launch. The masthead says that tools run outside polly's sandbox.
- **Approvals.** `--confirm` is refused: the control contract has no tool
  approvals.
- **Other providers.** `gemini`, `ollama`, `qwencloud` and `codex` models
  are refused with the list of providers the backend serves.
- **In-process commands.** On a backend session `/plan`, `/todo`, `/swarm`,
  `/workflow`, `/compact`, `/sandbox`, `/sandbox-init`, `/add-dir`, `/clear`,
  `/reset`, `/rename`, `/title`, `/spawn`, `/fast`, `/speed`, `/login`,
  `/logout` and `/setup` reply with a notice and do nothing.
- **Store writes.** `--delete`, `--reset`, `--purge`, `--create` and `--add`
  are refused: the backend's sessions are its own.

## How it works

The `control` package holds the client side: the JSON-RPC connection, the
daemon's discovery and start, the setup and build keys, the session store
that reads over the connection, and the conversion between the backend's
transcript lines and polly's messages. Its live tests run against a real
backend binary:

```bash
POLLY_BACKEND_BIN=/abs/path/to/polly-zig go test ./control -run Live
```

The contract's requests are `hello`, `watch`, `prompt`, `stop` and `agents`.
On top of those come the daemon's `join` and `start`, and four store reads:
`sessions`, `info`, `history` (the transcript's lines exactly as stored) and
`output` (a saved tool output, in byte ranges).
