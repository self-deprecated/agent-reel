// agent-reel: OpenCode plugin shim for `areel hook generic`.
//
// OpenCode has no command hooks; it loads JS plugins from
// ~/.config/opencode/plugins/ or a path under "plugin" in opencode.json. This
// shim maps OpenCode's session, prompt, tool, permission and compaction events
// to the generic hook schema v1 (docs/marks-and-agents.md, "Agent hooks")
// and pipes each one as JSON to a fresh `areel hook generic`, which proves the
// covering recording and delivers it (or silently drops it).
//
// Privacy: only session/call identifiers, tool and permission names and a
// permission verdict are read. Message text, tool input/output, titles,
// directories, paths, patterns and model names never reach a payload.
//
// Cost: nothing is ever awaited or thrown into OpenCode. At most 4 children are
// in flight and a token bucket admits 8 events/s (burst 32); anything over is
// dropped and only counted. Every payload is under 2 KiB by construction.
//
// Environment: plugins run in the TUI's server worker with the terminal's
// environment, so areel sees ZELLIJ_SESSION_NAME and finds the recording.
// Under `opencode serve` / `opencode attach` they run with the server's
// environment instead, which usually proves no recording (every event is then
// dropped by areel). AREEL_HOOK=off (or 0, false) disables the shim entirely.
//
// Only the default export may exist: OpenCode's loader calls every export as
// a plugin (and refuses the module if one is not a function).
import { spawn } from "node:child_process";

const ARGV = ["areel", "hook", "generic"];
const LIMITS = { inFlight: 4, rate: 8, burst: 32, calls: 512, sessions: 512, bytes: 2048, tool: 64 };
// Identifiers sent in a payload (OpenCode's are ASCII like ses_...): bounded
// so that a payload cannot grow past LIMITS.bytes.
const ID = /^[A-Za-z0-9._:-]{1,128}$/;
const TOOL_EVENTS = new Map([["running", "tool.start"], ["completed", "tool.end"], ["error", "tool.error"]]);

const spawnAreel = argv => spawn(argv[0], argv.slice(1), { stdio: ["pipe", "ignore", "ignore"] });
const id = v => (typeof v === "string" && ID.test(v) ? v : undefined);
const name = v => (typeof v === "string" && v !== "" ? v.slice(0, LIMITS.tool) : undefined);
const byteLength = s => new TextEncoder().encode(s).length;
// Insertion-ordered map that forgets its oldest entry beyond max.
function remember(map, key, value, max) {
  map.set(key, value);
  if (map.size > max) map.delete(map.keys().next().value);
  return value;
}

// options is OpenCode's per-plugin JSON configuration in production; only
// functions (which JSON cannot carry) are taken from it, by the tests.
export default async function agentReel(_input, options) {
  if (/^(off|0|false)$/i.test(String(process.env.AREEL_HOOK ?? "").trim())) return {};
  const spawner = typeof options?.spawn === "function" ? options.spawn : spawnAreel;
  const now = typeof options?.now === "function" ? options.now : () => performance.now();
  // Session id -> {parent: string|null, prompted, ended}; a child (parent set)
  // is a subagent. Unknown ids are roots.
  const sessions = new Map();
  const calls = new Map(); // "callID status" already sent
  let inFlight = 0, tokens = LIMITS.burst, refilled = now(), dropped = 0;

  function send(event, session, extra) {
    try {
      if (inFlight >= LIMITS.inFlight) return void dropped++;
      const t = now();
      if (t > refilled) tokens = Math.min(LIMITS.burst, tokens + (t - refilled) * LIMITS.rate / 1000);
      refilled = Math.max(refilled, t);
      if (tokens < 1) return void dropped++;
      const json = JSON.stringify({ v: 1, agent: "opencode", event, session, ...extra });
      if (byteLength(json) >= LIMITS.bytes) return void dropped++;
      tokens -= 1;
      const child = spawner(ARGV.slice());
      inFlight++;
      let open = true;
      const settle = () => { if (open) { open = false; inFlight--; } };
      try {
        child.on("error", settle);
        child.on("exit", settle);
        child.stdin.on("error", () => {});
        child.unref?.();
        child.stdin.end(json);
      } catch {
        settle();
        dropped++;
      }
    } catch {
      dropped++;
    }
  }

  const subagent = (sid, s) => ({ subagent: s.parent ? { id: sid, parent: s.parent } : { id: sid } });

  function created(info) {
    const sid = id(info?.id);
    if (!sid || sessions.has(sid)) return;
    const child = typeof info.parentID === "string" && info.parentID !== "";
    // A child whose parent id is unusable keeps "" (still a child, no parent sent).
    const s = remember(sessions, sid, { parent: child ? id(info.parentID) ?? "" : null, prompted: false, ended: false }, LIMITS.sessions);
    if (child) send("subagent.start", sid, subagent(sid, s));
    else send("session.start", sid);
  }

  // session.status idle and session.idle both fire at the end of a turn:
  // clearing prompted (and setting ended) makes the second one a no-op.
  function idle(sessionID) {
    const sid = id(sessionID);
    const s = sid && sessions.get(sid);
    if (!s) return;
    if (s.parent === null) {
      if (!s.prompted) return;
      s.prompted = false;
      send("turn.end", sid);
    } else if (!s.ended) {
      s.ended = true;
      send("subagent.end", sid, subagent(sid, s));
    }
  }

  // Tool parts repeat on every update (with their full input/output); only
  // the first running/completed/error of each call is sent. A subagent's own
  // tools are dropped, as for Claude Code and Codex subagents.
  function toolPart(part) {
    if (part?.type !== "tool") return;
    const sid = id(part.sessionID);
    const call = part.callID;
    const event = TOOL_EVENTS.get(part.state?.status);
    if (!sid || !event || typeof call !== "string" || call === "" || call.length > 256) return;
    if (sessions.get(sid)?.parent != null) return;
    const key = call + " " + part.state.status;
    if (calls.has(key)) return;
    remember(calls, key, true, LIMITS.calls);
    const tool = name(part.tool);
    send(event, sid, tool ? { tool: { name: tool } } : {});
  }

  function permission(type, p) {
    const sid = id(p?.sessionID);
    if (!sid) return;
    if (type === "permission.asked") {
      const tool = name(p.permission);
      return send("permission.request", sid, tool ? { tool: { name: tool } } : {});
    }
    // 1.18 sends reply once|always|reject; older builds named it response.
    const reply = typeof p.reply === "string" ? p.reply : p.response;
    const status = typeof reply !== "string" ? undefined : /reject|deny|denied/i.test(reply) ? "denied" : "ok";
    send("permission.reply", sid, status ? { status } : {});
  }

  function onEvent(e) {
    const p = e?.properties;
    switch (e?.type) {
      case "session.created": return created(p?.info);
      case "session.status": return p?.status?.type === "idle" ? idle(p.sessionID) : undefined;
      case "session.idle": return idle(p?.sessionID);
      case "message.part.updated": return toolPart(p?.part);
      case "permission.asked":
      case "permission.replied": return permission(e.type, p);
      case "session.compacted": {
        const sid = id(p?.sessionID);
        return sid ? send("compaction.end", sid) : undefined;
      }
    }
  }

  return {
    // Called synchronously for every bus event and never awaited.
    event: async arg => {
      try { onEvent(arg?.event); } catch { dropped++; }
    },
    "chat.message": async input => {
      try {
        const sid = id(input?.sessionID);
        if (!sid) return;
        const s = sessions.get(sid) ?? remember(sessions, sid, { parent: null, prompted: false, ended: false }, LIMITS.sessions);
        if (s.parent !== null) return;
        s.prompted = true;
        send("prompt", sid);
      } catch { dropped++; }
    },
    "experimental.session.compacting": async input => {
      try {
        const sid = id(input?.sessionID);
        if (sid) send("compaction.start", sid);
      } catch { dropped++; }
    },
    dispose: async () => {
      try {
        for (const [sid, s] of sessions) if (s.parent === null) send("session.end", sid);
        sessions.clear();
      } catch { dropped++; }
    },
  };
}
