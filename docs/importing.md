# Importing conversations

```sh
areel import ~/.claude/projects/-home-me-app/1c0e2a.jsonl      # a Claude Code session
areel import ~/.t3/userdata/statev2.sqlite --thread latest      # a T3 Code thread
areel import rollout.jsonl --style pi --cast talk.cast          # just write a cast
areel import ~/.t3/userdata/statev2.sqlite --thread 5d83 --follow   # live, until areel stop
```

`areel import FILE` turns an agent's conversation log into a library
recording: a minimal fake terminal session in the look of Claude Code, Codex
or Pi, timed from the log's own timestamps. The result is an ordinary
recording, sealed like any other, so the viewer, search, marks and exports all
work on it. An asciicast file can be imported the same way.

## What it reads

The format is read from the file itself; `--format` overrides it.

| Format | Where it usually is | |
|---|---|---|
| `t3` | `~/.t3/userdata/statev2.sqlite` | every T3 Code thread; pick one with `--thread` |
| `claude-code` | `~/.claude/projects/<project>/<session>.jsonl` | one session |
| `codex` | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` | one session |
| `pi` | `~/.pi/agent/sessions/*.jsonl` | one session |
| `asciicast` | any `.cast` (v2 or v3) | footage as it is; input events are dropped |

A T3 database holds many threads. `--list` prints them (`--json` for
machines), and `--thread` takes an ID, a unique prefix or `latest`.

From a conversation, the recording shows prompts, replies, reasoning (if the
style shows it), tool calls with their output, file diffs, sub-agents, the
agent's working status and its footer. Prompts are marked `You` and finished
turns `Agent` on the player's timeline. Files are opened read-only.

## Styles

`--style` picks the look:

- `claude-code`, `codex` or `pi`, the built-in looks. The default is the
  agent that wrote the log (for T3, the provider of the thread).
- `NAME`: the style file `NAME.json` in `styles/` beside the
  [user config](configuration.md), normally `~/.config/agent-reel/styles/`.
- A path to a style file.

A style file is strict JSON. `base` names the built-in look it starts from;
every other key is optional and keeps the base's value when left out:

```json
{
  "base": "pi",
  "size": { "cols": 140, "rows": 40 },
  "timing": { "idleCap": 1.5, "busyCap": 0.6, "tick": 0.25, "speed": 1 },
  "animate": { "typePrompts": true, "streamReplies": true,
               "typingCps": 45, "streamCps": 400, "maxWindow": 4, "fps": 15 },
  "show": { "welcome": true, "reasoning": "collapsed", "toolOutput": 8,
            "fileDiff": "full", "diffLines": 20, "subagents": true, "markers": true },
  "colors": { "userBg": "#2d2a3e" }
}
```

| Key | |
|---|---|
| `size` | terminal size: 40–512 columns, 10–512 rows, at most 32768 cells (default 120×36) |
| `timing.idleCap` | the longest gap kept while waiting for the user, in seconds (default 3) |
| `timing.busyCap` | the longest gap kept while the agent works (default 1.5) |
| `timing.tick` | spinner redraw interval while working; 0 for none |
| `timing.speed` | playback multiplier |
| `animate` | typing prompts and streaming replies, at these characters per second, within at most `maxWindow` seconds |
| `show.reasoning` | `hidden`, `collapsed` or `full` (Claude Code and Codex hide it, Pi shows it) |
| `show.toolOutput` · `show.diffLines` | rows of tool output and of a diff |
| `show.fileDiff` | `hidden`, `stat` or `full` |
| `colors` | `#rrggbb`, a terminal colour name (`red`, `bright-blue`, ...) or `""` for the terminal's default |

Colour names belong to the base look:

- **claude-code**: `text`, `white`, `inactive`, `claude`, `shimmer`, `ok`,
  `err`, `accept`, `code`, `rule`, `thinking`, `userBg`, `logoBg`, `diffFg`,
  `addBg`, `addMark`, `delBg`, `delMark`
- **codex**: `text`, `accent`, `ok`, `err`, `dim`, `web`, `code`, `model`,
  `cwd`, `title`, `addBg`, `delBg`, `diffText`, and the shell colours
  `shCommand`, `shOperator`, `shDash`, `shFlag`, `shString`, `shNumber`,
  `shKeyword`, `shText`
- **pi**: `text`, `accent`, `border`, `ok`, `err`, `warn`, `muted`, `dim`,
  `thinking`, `userBg`, `pendingBg`, `successBg`, `errorBg`, `heading`,
  `code`, the editor border per thinking level `thinkOff` ... `thinkMax`,
  and the logo's `logoCoral`, `logoBlue`, `logoYellow`

Unknown keys, unknown colour names and out-of-range values are refused, with
a suggestion for near misses. Flags override the style for one import:
`--size`, `--idle-cap`, `--busy-cap`, `--speed`, `--reasoning`,
`--tool-output`, `--diff-lines`, `--exact`, `--no-animate` and
`--no-welcome`. `areel import --help` lists them.

The looks were matched against the real tools: Claude Code 2.1.301 and Codex
0.163 recordings (the [demo pages](demo/)), and Pi 1.2's source and theme.
They don't reproduce syntax highlighting in code blocks, Claude Code's tips,
or anyone's personal setup (vim mode, Pi footer extensions).

## Timing

Events keep their order and their real spacing, except that long gaps are
cut: a gap keeps at most `idleCap` seconds while the agent waits for you and
`busyCap` while it works, and loses its middle. A four-hour session becomes
about half an hour. Elapsed counters and turn summaries ("Worked for 3m 12s")
still show real durations. `--exact` keeps real time.

Typing a prompt and streaming a reply are the only invented motion. Both
happen inside the gap before the event and end exactly at it.

`areel ls` shows how long the import ran in its DURATION column;
`areel inspect REC` gives the footage length.

## Live imports

With `--follow` the command starts the recording and returns. The recording
draws everything so far, then each new event as the log grows (polled every
second), until `areel stop REC`. Gaps are cut the same way, so the footage
doesn't grow while nothing happens. The viewer shows its latest saved screen,
updated every couple of seconds, rather than a live stream.

An import can be stopped but not paused or resumed. Importing the same file
(or T3 thread) again starts a new recording for it.

## For scripts and agents

Everything can be stated rather than detected: `--format`, `--thread`,
`--style` and the style flags. `--list --json` prints
`{"format", "conversations": [{"id", "title", "updated"}]}` (no `id` for
one-conversation files). `--json` prints
`{"recording", "name", "format", "events", "library", "live", "status"}`;
without `--follow` it returns once `status` is `stopped` and the file is
sealed. With `--follow`, end the recording with `areel stop RECORDING`.

## Limits

- GitHub Copilot and other agents' logs are not read.
- Claude Code sub-agent internals are left out; the sub-agent call shows.
- Codex logs without typed item events (older versions) are read from their
  raw messages and tool calls.
- Tool calls still open when a turn ends show as failed.

## Privacy

The footage shows what the conversation contains: prompts, replies, tool
arguments and output. Treat it like a screen recording, and review an export
before sharing it.
