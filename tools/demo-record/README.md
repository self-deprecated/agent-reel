# Demo recordings

The README's footage is real: each agent gets the same two prompts in a
fresh clone of this repository, typed by a script, and records itself with
the installed `areel`.

```sh
python3 tools/demo-record/record.py --agent claude   # also: codex, pi
python3 tools/demo-record/record.py --agent codex --no-record   # dry run
nix develop -c sd areel docs-assets                  # docs/demo/*.html and docs/images/*
```

`record.py`:

1. Clones HEAD to `/tmp/agent-reel` and creates a throwaway Zellij session,
   `demo-<agent>`. The session has a plain `agent-reel $` prompt and the
   compact layout.
2. Attaches to it with `areel attach` on a 110×32 PTY that the script owns,
   so its typing is real keystrokes.
3. Records into the demo library `/tmp/areel-rec/lib`, adding chapter marks
   as it goes.
4. Launches the agent:
   - Claude Code with the repo's hook plugin and auto permissions, and no
     subagents;
   - Codex with the workspace-write sandbox;
   - Pi as configured.
5. Types the two prompts and waits for each reply by watching the screen
   settle.
6. Opens a floating pane that runs `git diff --stat` and exports the live
   recording, then stops and seals the recording.

Screens it waited on are saved in `/tmp/areel-rec/screens/<agent>/`.

`visuals.py` writes:

- the per-agent web exports for `docs/demo/`;
- the composed GIF for the compose guide;
- the side-by-side still.

`screenshots/` captures the library, the viewer and a web embed with
Chromium. `hero/` plays the Claude Code web export in Chromium (jumping
between chapters with a real mouse) into the README's `hero.gif`. It also
writes the README's text excerpt from the same opening frame.

Requirements:

- each agent must be logged in;
- the first run in a new folder answers the agent's trust prompt the way a
  user would.
