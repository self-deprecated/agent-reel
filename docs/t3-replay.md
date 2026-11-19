# T3 Code replays

```sh
areel t3-replay ~/.t3/userdata/statev2.sqlite --thread 5d83 -o work.json   # take the stretch
areel t3-replay work.json -o work.html                                     # play it in a browser
areel t3-replay work.json -o work.mp4                                      # film it for a slide
```

`areel t3-replay` animates a stretch of T3 Code work as T3 Code's own window.
The middle shows one thread's conversation, the **focus**. The sidebar shows
the threads around it, and they change as they really did: they start working,
finish, get looked at and settle. It is made for slides and talks about how you
work with agents.

Unlike `areel import`, the output is a web page or a video, not a library
recording.

## Two steps

1. **Take the stretch from the database.** `-o FILE.json` writes a *timeline*:
   the threads, their state changes and the focus thread's conversation, with
   real timestamps. You can edit it before it is drawn: rename a thread, drop
   one, shorten a reply or change `playback`.
2. **Draw it.** Pass the timeline (or the database directly) with
   `-o FILE.html` or `-o FILE.mp4`.

The page is one self-contained file that loads nothing from the network. It
loops. Space pauses it and the arrow keys seek. Hover for the controls: the
progress bar, and a **Working shelf** button (or the W key) that turns the
Working section on and off while it plays.
`?t=12` opens it paused at 12 seconds. The video is filmed frame by frame
from that page in headless Chromium and encoded with FFmpeg as H.264, so it is
exact even on a slow machine. It is 1920×1080 at T3 Code's own scale;
`--scale 2` films at twice the pixels. The page carries its own fonts (subsets
of Inter and JetBrains Mono), so it looks the same everywhere.

## Choosing the stretch

| Flag | |
|---|---|
| `--thread ID` | the focus thread: an ID, a unique prefix or `latest`; `areel import DB --list` lists them |
| `--from TIME` · `--to TIME` | the window, as RFC 3339, `YYYY-MM-DD HH:MM` or `HH:MM` (today, local time). The default is the focus thread's first and last event |
| `--threads ID,...` | exactly these other sidebar threads |
| `--skip-projects NAMES` | leave these projects' threads out of the sidebar (comma-separated project names) |
| `--max-threads N` | otherwise, take up to N threads (default 12). Threads that changed state in the window come first, busiest first. Then come threads from the six hours before it. Sub-agents, archived and deleted threads are left out |
| `--working-shelf on\|off` | working threads in a shelf at the bottom of the sidebar, or in the list (default: your T3 Code setting) |
| `--history N` | how many conversation items from before the window are already on screen (default 6) |

## What it shows

T3 Code's dark window.

- **Sidebar cards** show the project's icon and name, the thread title, and
  its provider (Claude or Codex); a card for a thread with a branch has a
  third line for it. The top right shows **Working**
  and how long it has been at it, **Done** (green) for a finished run you
  haven't opened yet, **Failed** (red), or else how long ago the thread last
  changed. Like T3 Code, threads you have seen and threads working in the
  background are dimmed.
- **The list** is most recently changed first. With T3 Code's Working section
  on, working threads gather in a **Working** shelf at the bottom, above
  Settled; off, they stay in the list. The replay follows your setting
  (`sidebarWorkingShelfEnabled` in `client-settings.json` beside the
  database); `--working-shelf on|off` overrides it. Cards slide when they
  change place.
- **Settled** threads drift out of the list, and the **Settled** count in the
  footer goes up.
- **The conversation** is the focus thread's. A prompt is typed into the
  composer, then sent. While the agent works, its replies stream in, tool
  calls appear one by one (running ones shimmer) and "Working for …" ticks
  at the bottom. When the run finishes, the work folds into
  "Worked for 6m 34s ›" above the final reply, as in T3 Code. A run the agent
  starts without a prompt (a wake) folds the same way.

## The timeline file

The timeline is strict JSON. Unknown keys and bad values are refused, so a
typo in an edited file is an error.

```json
{
  "replay": 1,
  "title": "Build Agent Task Cockpit",
  "from": "2026-11-19T05:50:00Z",
  "to": "2026-11-19T06:20:00Z",
  "focus": "a880bcac-…",
  "model": "Opus 5.5 High",
  "settledBefore": 463,
  "playback": { "speed": 60, "gapCap": 1.2, "hold": 3, "theme": "dark",
                "width": 1920, "height": 1080, "clock": false, "workingShelf": true },
  "projects": [{ "id": "989f…", "title": "t3code" }],
  "threads": [
    { "id": "a880bcac-…", "title": "Build Agent Task Cockpit", "project": "989f…",
      "branch": "t3/35014dd6", "agent": "claude",
      "created": "2026-11-19T05:50:00Z",
      "changes": [
        { "at": "2026-11-19T05:50:00Z", "status": "done", "settled": false },
        { "at": "2026-11-19T05:54:24Z", "status": "working" },
        { "at": "2026-11-19T06:00:59Z", "status": "done" }
      ] }
  ],
  "items": [
    { "id": "…", "at": "2026-11-19T05:54:24Z", "kind": "user", "text": "I answered two questions…" },
    { "id": "…", "at": "2026-11-19T05:54:46Z", "done": "2026-11-19T05:54:47Z",
      "kind": "tool", "verb": "Ran", "text": "cat apps/web/src/threadWaitResolves.ts" },
    { "id": "…", "at": "2026-11-19T05:55:06Z", "kind": "assistant", "text": "I'm sending all the answers…" }
  ]
}
```

- A thread's `changes` are in time order. Each sets only the fields it names.
  `status` is `idle`, `working`, `done` (finished, not yet looked at) or
  `failed`. `settled` moves the thread to the Settled footer or back. The card
  appears at `created`. `branch` and `agent` (`claude` or `codex`) are shown
  on the card.
- A project's `icon` is an emoji, `folder`, or left out for T3 Code's
  two-letter monogram. `model` is the composer's model label, and
  `settledBefore` the footer's count before the replay's own threads settle.
- `items` are the focus thread's conversation. `kind` is `user`, `assistant`
  (Markdown: paragraphs, lists, headings, code), `tool`, `question`, `error`
  or `notice`. A tool call has a `verb` (Ran, Read, Edited, ...), a `text`
  (the command or path), an optional `detail` (`+12 -3` is drawn as a diff
  count), and `done` when it finished. Reasoning is not included.

### Playback

Real time plays at `speed` (seconds of work per second), but no pause between
two events plays longer than `gapCap` seconds. A half-hour stretch usually
plays in 20 to 40 seconds. `duration` then stretches or squeezes the whole
replay to that many seconds; `--duration` sets it from the command line.
`hold` keeps the last frame before the replay loops. `theme` is `dark` or
`light` (`--theme`; the light look is rougher). `width` × `height` is the
window in CSS pixels, scaled to fit the browser and used as the video size.
`clock` adds a small real time of day at the top, which T3 Code doesn't have.
`workingShelf` puts working threads in the Working shelf at the bottom of the
sidebar.

Every motion is timed from the replay clock, so the page and the video show
the same frames.

## Privacy

A replay shows thread titles, prompts, replies, commands and file paths from
your T3 Code database, which is opened read-only. Edit the timeline to remove
what shouldn't be seen, and review the page or video before you share it.
