# AgentReel agent plugin

Hook plugin for Claude Code and Codex CLI that runs `areel hook claude` /
`areel hook codex` on every relevant lifecycle event (and an OpenCode plugin
shim that feeds `areel hook generic`), so the live AgentReel
recording covering the agent's terminal gets activity rows (session, tool,
permission, compaction) and marks (your prompts, finished turns, permission
requests, subagents, compactions).

- **Privacy.** Only generic markers are stored: event kind, phase, role, a
  normalized tool name, subagent type and short enum reasons. Prompt, command,
  tool input/output, messages, paths and models are never read or stored. See
  "Agent hooks" in `ACTIVITY.md` (source tree).
- **Never in the way.** `bin/areel-hook` discards all output and always exits
  0, and every hook is async except SessionEnd (and Claude Code's
  SessionStart), which are time-boxed to 1-2 s. Outside a recording every
  event is dropped silently.
  `AREEL_HOOK=off` disables it.

Layout: `.claude-plugin/` (Claude plugin and directory marketplace),
`.codex-plugin/` and `.agents/plugins/` (Codex plugin and marketplace),
`hooks/claude.json`, `hooks/codex.json` (deliberately no `hooks/hooks.json`,
which other agents auto-discover with a different vocabulary),
`bin/areel-hook`, and `opencode/agent-reel.mjs`, the OpenCode plugin shim
(its Bun tests, `opencode/agent-reel.test.mjs`, are not installed).

The Nix package installs this directory at
`share/agent-reel/agent-plugin` next to the `areel` binary; install steps are
in "Agent hooks (Claude Code, Codex, OpenCode)" of the AgentReel README
(`README.md`, installed as `share/doc/agent-reel/README.md`).
