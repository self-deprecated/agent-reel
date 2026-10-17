# Recording

AgentReel records a whole Zellij or tmux session, as the session's clients
see it, in the background. It records text, not pixels:

- each screen is stored as a list of terminal rows;
- each distinct row is stored once.

A two-minute agent session is about 190 kB and a busy eight-hour one about
10 MB, and all of it stays searchable and selectable. Recordings land in a
private library (`~/.local/share/agent-reel` by default) and keep running
when the shell that started them exits.

A worker an agent spawns in its own Zellij or tmux session can be recorded
the same way. It links itself to the parent's recording (see [linked
recordings](library.md#linked-recordings)).

## Start, pause, stop

```sh
areel record          # inside Zellij or tmux: record this session
areel record work     # or name a session
areel status          # what is being recorded, and how much
areel pause           # stop capturing for a while
areel resume          # carry on, in a new segment
areel stop            # finish and seal the recording
```

Any number of agents and shells in the session share one recording, and
running `areel record` again does nothing new. The recording follows the
session's size: resize the terminal or attach a smaller client and the
footage resizes with it.

Pausing and resuming keep one recording with several **segments**. Pauses
are left out of the footage. Once you stop, the next `areel record` starts a
new recording.

## Naming a recording (REC)

Commands that take `REC` accept:

| Form | Meaning |
|---|---|
| `@` | the recording of the session you're in (the default for most commands) |
| `latest` | the most recently started recording |
| `3f2a` | a unique prefix of the ID (4+ hex characters) |
| a full 32-hex ID | exactly that recording |

`areel ls` lists the current recording of each session. Add `--json` to any
command for machine-readable output.

## When it starts

**Zellij.** `areel record` needs at least one client attached to the
session, with every client on the same tab (Zellij can only show one tab to
the recorder). If clients are on different tabs, start is refused and tells
you why.

**tmux** (3.2 or newer). Every client of a tmux session shows its current
window, so any session can be recorded, even a detached one. The recorder
attaches its own client:

- it ignores its size, so it never shrinks your session;
- it draws the current window plus the status line;
- it never sends a key, so it can't type into the session.

It uses the `tmux` on your PATH, which must be the same tmux as the server.
From outside tmux, name the session and the backend: `areel record work
--backend tmux`, or set `"backend": "tmux"` in your
[configuration](configuration.md).

Nothing records automatically. You start every recording yourself, or an
integration does it for you: [Pi](marks-and-agents.md#pi) has `/reel start`,
and the [supervisor](integrations.md#supervisor) is a background loop for
allowlisted sessions.

## Finishing

A clean stop seals the recording into one file, `<library>/<id>.areel`, and
verifies it. Sealing also builds the [text search](library.md#search) index.
The file name comes from the `seal.name` template (default `{id}`; see
[configuration](configuration.md)).

If sealing fails (a full disk, a crash), the source is kept and
`areel status`, `areel ls` and `areel doctor` say:

```text
run: areel recover ID
```

`areel recover` seals what was saved and only then removes the source. It
refuses to touch a recording whose recorder processes might still be running.
If there is data it cannot seal, it keeps the source and says so;
`--discard-tail` lets it drop that tail. Long recordings may need bigger
budgets: `--decoded-bytes` and `--work`.

`areel doctor REC` checks a recording without changing it, and
`areel doctor file.areel` checks a standalone file.

## Storage

Nothing is ever deleted, rotated or capped automatically. You can ask for
one warning per recording when it gets big or long, in the library's own
`LIBRARY/config.json` (mode 0600):

```json
{"backend": "zellij", "warnings": {"storedBytes": 1073741824, "footageSeconds": 28800}}
```

Each threshold warns once. The warning shows in `areel status`, as a flag
in `areel ls` and in the status feed, and as a line in the holder's log.
Recording carries on. If storage fails outright, the recording stops as
**interrupted** and keeps everything it had committed. Nothing restarts it
on its own; run `areel recover` once the disk is fixed.

## Keystrokes (`areel attach`)

The recorder only sees output. To show what was typed in a Zellij session
(tmux isn't supported here yet), attach through `areel attach` instead of
`zellij attach`, and opt the recording in:

```sh
areel keys on            # opt this recording in
areel attach work        # instead of: zellij attach work
areel keys pause         # stop capturing keys for a moment
areel keys resume
areel keys off
```

Typed keys show as an overlay in the library viewer (the **Keys** button)
and can be burned into a [composed video](compose.md#keys-overlay). They
never appear in exports or web embeds.

What's kept: labels like `git status`, `Enter`, `Ctrl+C`, `↓ ×5`. A paste is
kept as its size only. Raw input bytes never leave the wrapper.

Password prompts are masked: while a focused pane reads a password (`sudo`,
`passwd`, `ssh-add`), keys are stored as one `••• hidden input`. If in doubt,
it masks. It cannot detect:

- **passwords typed at a remote prompt** inside `ssh`, `mosh` or
  `docker exec`. Run `areel keys pause` first.
- keys typed *before* a password prompt appears.
- clients attached with plain `zellij attach`. Their keys are not seen, so
  the overlay always says "partial".

Make it your default attach with an alias:

```sh
alias za='areel attach'
```

## Platforms

Linux and macOS have native backends for Zellij and tmux. Keystroke
capture, automatic parent links and [Niri highlights](niri.md) are
Zellij-only for now. macOS source builds need cgo and the
macOS SDK. On macOS, protected Apple binaries can hide their environment, so
key capture masks panes it cannot identify.
