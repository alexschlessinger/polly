---
name: polly-tui
description: Capture the polly TUI as PNGs — play a scenario of typed input, keys and shots through polly's own off-screen renderer, then read the images
---

# Capturing the polly TUI

`driver.sh` builds `./polly` and plays a scenario through polly's own
off-screen renderer, so a frame can be captured with no terminal, pty
host, window or screen-recording permission, and with a fixture, no
provider key. Runs use an isolated `HOME`
(`~/.cache/polly-tui-home`) and leave PNGs in `/tmp/polly-shots/`
(override with `POLLY_SHOT_DIR`).

```bash
D=.agents/skills/polly-tui/driver.sh   # run from the repo root
$D hshot scenario.txt                  # prints each PNG path it writes
$D hshot scenario.txt 160x50           # at a wider virtual terminal
POLLY_SHOT_FIXTURE=fixture.json $D hshot scenario.txt   # seeded state, scripted model
```

```text
# scenario.txt — :shot paths are $VAR- and ~-expanded
:shot $POLLY_SHOT_DIR/splash.png
/help
:wait "Navigate" 5
:shot $POLLY_SHOT_DIR/help.png
:size 160x50
:settle
:shot $POLLY_SHOT_DIR/help-wide.png
```

A bare line is typed and submitted; `:` lines are steps: `:key <name>`,
`:type <text>`, `:submit <text>`, `:shot <path>`, `:size WxH`,
`:wait <pattern> [sec]`, `:settle [sec]`, `:ready [sec]`, `:sleep <ms>`,
`:quit`. Key names: `enter`, `esc`, `tab`, `up`, `down`, `left`, `right`,
`pgup`, `pgdn`, `home`, `end`, `insert`, `delete`, `backspace`, `space`,
`c-a`…`c-z`. The full reference is
[docs/SCREENSHOTS.md](../../../docs/SCREENSHOTS.md).

A fixture (`POLLY_SHOT_FIXTURE=fixture.json`) gives the run its state with no
provider key: `sessions` seed the store (history, titles, child sessions) and
`turns` script what the model streams when a bare line is typed. A turn's
`gate` holds the stream until the scenario runs `:release <gate>`; a `mark`
lets the scenario wait with `:at <mark>` for exactly the emit it wants to
capture. Scripted tool calls run the real tools. `polly --export <context>` writes a
fixture from a stored session; `fixtures/` holds ready-made pairs (a
`.json` fixture and the `.txt` scenario that plays it). The format is in
[docs/SCREENSHOTS.md § Fixtures](../../../docs/SCREENSHOTS.md#fixtures-seeded-sessions-and-scripted-turns).

Read the PNGs with the Read tool. A capture is the frame painted after
the step before it, so no shot contains the step that asked for it, and a
step that never happens (a `:wait` pattern that never appears, a path that
cannot be written) fails the run with its script line instead of silently
capturing something else.

A capture is polly's own frame: cells in theme colors, plus every image
polly placed (thumbnails, the masthead logo) at one pixel per screen
pixel. What a terminal would then do with those pixels — image scaling or
palette quantization, the hardware cursor, window chrome — is not
reproduced. A scenario makes a real API call only if it types a prompt, no
fixture is given, and a provider key is exported; commands, keys and
`/attach` are free.

## Gotchas

- The transcript is bottom-pinned: a long output (`/help`) scrolls its first
  lines out of a 40-row frame. `:wait` for a pattern that is visible, or use
  a taller size.
- The first typed line waits out the startup workspace baseline (the Git diff
  behind the first frame) so it runs instead of queueing. Input typed while a
  turn runs still queues, exactly as for a fast typist — that is what `:wait`,
  `:settle` and `:ready` are for.
- `git` must be on `PATH`, cwd must not be the real `$HOME`, and `HOME` must
  not be under `/tmp` (a gitconfig inside a writable sandbox path). The
  driver's isolated HOME and the repo root satisfy both.