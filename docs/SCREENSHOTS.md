# Headless screenshots

Capture the TUI off-screen for docs, reviews, and visual checks. For everyday
use, see the [CLI guide](CLI.md).

`polly --shot-script <file>` runs the real TUI with no terminal at all. It
paints onto an off-screen screen of `--shot-size` (default `120x40`) and plays a
script of typed input, keys, and captures. Each `:shot` writes a PNG of the
frame Polly rendered, in exact theme colors, and prints its path on stdout. No
terminal capture or screen-recording permission is involved. If the run can't
take the input it was given, it fails and names the script line responsible.

Scenarios that make no model calls (layout, colors, keys, commands) are free and
deterministic:

```text
# scenario.txt
:shot $POLLY_SHOT_DIR/splash.png
/help
:wait "Navigate" 5
:shot $POLLY_SHOT_DIR/help.png
:size 160x50
:settle
:shot $POLLY_SHOT_DIR/help-wide.png
```

```bash
POLLY_SHOT_DIR=/tmp/shots polly --shot-script scenario.txt
```

A script has one step per line. Blank lines and `#` comments are skipped.

| Step | Meaning |
|---|---|
| `<text>` | Type the text into the composer and submit it |
| `:submit <text>` | The same, for text that starts with `:` |
| `:type <text>` | Type text without submitting (multi-line needs `:key c-j`) |
| `:click <X> <Y>` | Click a cell, counted from the top-left corner as 0 0 |
| `:click <text> [sec]` | Wait until the screen contains the text, then click its first cell; the first match reading top to bottom wins (quote text that ends in a number) |
| `:key <name>` | One key: `enter`, `esc`, `tab`, `s-tab`, `up`, `down`, `left`, `right`, `pgup`, `pgdn`, `home`, `end`, `insert`, `delete`, `backspace`, `space`, `c-a`…`c-z` |
| `:shot <path>` | Write a PNG of the current frame; the path is `~`- and `$VAR`-expanded |
| `:size WxH` | Resize the virtual terminal and re-lay out the frame |
| `:wait <pattern> [sec]` | Wait until the screen contains the pattern (quote a pattern that ends in a number) |
| `:settle [sec]` | Wait until two reads of the screen agree and code highlighting has caught up |
| `:ready [sec]` | Wait until input would run rather than queue |
| `:sleep <ms>` | Wait |
| `:release <gate>` | Let the fixture's turn past a gate (needs `--shot-fixture`) |
| `:at <mark> [sec]` | Wait until the fixture reports a mark, then give the frame a moment to land (needs `--shot-fixture`) |
| `:quit` | End the run here |

Timing takes a little care. The first typed line waits until Polly is ready to
run it. After that, input queues just as it would for a fast typist, which is
what `:wait` and `:settle` are for. `:shot` also waits (up to 10 seconds) for
background code highlighting to catch up, so captures don't depend on machine
speed.

## Fixtures: seeded sessions and scripted turns

`--shot-fixture <file>` gives a shot run its state, with no provider key
required. The fixture seeds sessions into the run's store before the TUI opens,
then plays scripted model turns whenever the script types a prompt, so every
frame is reproducible: a resumed transcript, thinking, half-streamed text, a
running tool call, a stream error, child sessions. The fixture names the context it
opens, and its turns *are* the model (`replay/<name>`, named after the file
unless the fixture sets `name`).

```json
{
  "sessions": [
    {
      "name": "flaky-test",
      "metadata": {"title": "Fix the flaky channel test"},
      "history": [
        {"role": "user", "content": "why does TestClose flake?"},
        {"role": "assistant", "content": "`done` is closed twice.",
         "metadata": {"input_tokens": 1540, "output_tokens": 62}}
      ]
    },
    {"name": "flaky-test/scout", "parent": "flaky-test",
     "metadata": {"spawnCallID": "call_0", "spawnOutcome": "finished"}}
  ],
  "turns": [
    {
      "match": "fix it",
      "steps": [
        {"reasoning": "Both closers must share a sync.Once.", "mark": "thought"},
        {"gate": "speak"},
        {"content": "I'll guard the close with ", "delay_ms": 30},
        {"content": "a `sync.Once`.\n", "mark": "explained"},
        {"tool": {"name": "bash", "arguments": {"command": "go test ./..."}}}
      ],
      "usage": {"input": 1610, "output": 48}
    },
    {"steps": [{"content": "Done."}], "mark": "finished"},
    {"error": "429 rate limited: retry after 20s"}
  ]
}
```

```text
:shot $POLLY_SHOT_DIR/resumed.png
fix it
:at thought
:shot $POLLY_SHOT_DIR/thinking.png
:release speak
:at explained
:shot $POLLY_SHOT_DIR/streaming.png
:at finished
:shot $POLLY_SHOT_DIR/finished.png
```

You don't have to write sessions by hand. `polly --export <context>` writes a
fixture from a stored session and the agents it spawned, leaving out
machine-bound paths, so any state you reach in a real run can be replayed. Add
`--artifacts` to embed the images and stored outputs its transcripts reference,
then add `turns` by hand for whatever should happen next.

`sessions` are store records. They're seeded in order, each replacing any
stored session with the same name:

| Field | Meaning |
|---|---|
| `name` | The session's name (required, unique in the fixture) |
| `launch` | The context the run opens; at most one, else the first session |
| `parent` | An earlier session to link this one under, as a spawned agent is |
| `metadata` | Session metadata in its JSON form, laid over the launch defaults: `title`, `thinkingEffort`, `spawnCallID`, `swarmID`, `activeTools`, … |
| `history` | Stored messages, appended verbatim: roles, `tool_calls`, `reasoning`, and the display metadata a resume reads (`thinking_ms`, `tool_ms`, `tool_succeeded`, `input_tokens`) |
| `artifacts` | Payloads the history's parts reference (`kind`, `mime_type`, `name`, `image_token`, `reference`, base64 `data`), stored before the history; ids are content addressed, so a payload lands under the id its part carries |

`turns` are consumed one per model request. Each request takes the first unused
turn whose `match` appears in its last user message, or failing that, the first
unused turn with no `match`. Running out of turns fails the stream.

A turn is either an `error` or a list of `steps`, and each step emits one thing:
`reasoning`, `content`, a `tool` call (`arguments` as an object or a JSON
string), or a `gate`. Any step, and the turn itself, can carry a `mark`.
`delay_ms` waits before a step. `usage` (`input`, `output`, and an optional
billed `cost` in US dollars) and `stop` (`end_turn`, `tool_use`, `max_tokens`,
`content_filter`) finish the turn.

A gate holds the stream until the script runs `:release <gate>`. A mark is
reported once its step has been emitted (or, for a turn, once its stream has
completed), and `:at <mark>` waits for it. Both latch, so their order relative
to the script doesn't matter.

Scripted tool calls run the real tools in the run's sandbox, so tool rows and
file changes come from actual work. For a canned tool result, put it in a
session's `history` instead.
