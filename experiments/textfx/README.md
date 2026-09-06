# Text lab

Run from the repository root:

```sh
go run ./experiments/textfx
```

A standalone playground using Polly's pinned gotui/tcell dependencies and
the same custom `Draw` / `ui.Cell` rendering approach as its transcript.
No providers, credentials, or session data. Nothing is wired into Polly.

Use a terminal at least 88 columns by 35 rows for the full descriptions.
Left/right arrows, Tab, or `1` / `2` / `3` / `4` switch between the four collections.
Space pauses; `+` / `-` adjust speed; `c` switches between RGB and terminal
palette colors; `r` restarts the sequence; `q` exits. The palette version
uses dim/bold/reverse steps instead of smooth RGB interpolation.

Collection 1: light sweep, breathing brightness, flowing gradient,
decode reveal, vertical wave, and background scanner. Start by trying the
first two at half speed for plausible everyday UI treatments.

Collection 2: typewriter with an erasing caret, comet trail, scattered
twinkles, outward ripple, amber/mint color wipe, and a split-flap board.
Launch directly into it with `go run ./experiments/textfx --page 2`.

Collection 3 turns the effects into little REPL scenes:

- **Prompt gravity:** scattered characters gather into a prompt ready to send.
- **Agent circuit:** colored signals branch into search/build/test agents and return as reports.
- **Patch stitch:** a needle weaves a deleted line into its replacement.
- **Tool sonar:** a search pulse illuminates matching output.
- **Context fold:** old turns collapse into a memory capsule.
- **Soft landing:** activity resolves into a quiet receipt and a fresh prompt.

Run `go run ./experiments/textfx --page 3`. These are scripted six-second
loops with sample text, counts, and timings, not live Polly events. The
three-row scenes keep their labels and captions still while they animate.

Collection 4 studies subtle feedback on specific Polly affordances:

- **Disclosure:** light the focused chevron, then reveal a tool row on opening.
- **Count settle:** briefly tint the changed completed count as agent reports arrive.
- **Caller beacon:** send a short glint left along the divider toward `← Back to caller`.
- **Context edge:** accent only a newly filled meter cell; usage numbers update immediately.
- **Queue receipt:** briefly warm `(queued)` after submission and fade it when the turn starts.
- **Ready cursor:** slowly breathe the idle cursor while keeping the `>` steady.

Run `go run ./experiments/textfx --page 4`. Labels and placements are based
on the current activity controls, parent divider, context status, transcript
queue marker, and composer. Focus, click, report, and queue events are scripted;
this is still an isolated experiment. Except for the idle cursor, accents
are short reactions to those events, with quiet holds between them.

Export the same gotui cell buffer without opening a terminal:

```sh
go run ./experiments/textfx --snapshot /tmp/textfx.png
go run ./experiments/textfx --gif /tmp/textfx.gif
go run ./experiments/textfx --page 2 --gif /tmp/textfx-2.gif
go run ./experiments/textfx --page 3 --gif /tmp/textfx-3.gif
go run ./experiments/textfx --page 4 --gif /tmp/textfx-4.gif
```

Exports rasterize the actual gotui cell buffer with local font fallback;
your terminal's font and theme will look different. The live terminal is
the authority for modifiers. RGB
gradients depend on terminal color capability, and vertical movement uses
whole character rows. This prototype redraws at 30 fps; integration should
stop the timer when idle or hidden and animate only transient UI text.
