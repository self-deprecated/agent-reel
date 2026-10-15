import { afterEach, expect, test } from "bun:test";
import { EventEmitter } from "node:events";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import * as mod from "./agent-reel.mjs";

const plugin = mod.default;
const SECRET = "SENTINEL-5e1f";
const input = { client: {}, project: { id: SECRET, worktree: SECRET }, directory: `/tmp/${SECRET}`, worktree: `/tmp/${SECRET}`, $: undefined };

// A fake spawner: records argv at spawn time and the stdin JSON at end().
// exit "end" makes a child exit as soon as its stdin is closed (synchronously);
// exit "never" keeps it in flight until the test calls finish().
function fake({ exit = "end" } = {}) {
  const spawns = [];
  const spawner = argv => {
    const child = new EventEmitter();
    const stdin = new EventEmitter();
    const record = { argv, raw: undefined, child };
    stdin.end = data => {
      record.raw = data;
      if (exit === "end") child.emit("exit", 0, null);
    };
    child.stdin = stdin;
    child.unref = () => {};
    spawns.push(record);
    return child;
  };
  return { spawner, spawns, events: () => spawns.map(s => JSON.parse(s.raw)) };
}

async function setup(opts = {}) {
  const f = fake(opts);
  const clock = { t: 1000 };
  const hooks = await plugin(input, { spawn: f.spawner, now: () => clock.t });
  const bus = (type, properties) => hooks.event({ event: { id: "evt", type, properties } });
  return { ...f, clock, hooks, bus };
}

const ev = (event, session, extra = {}) => ({ v: 1, agent: "opencode", event, session, ...extra });
const created = (bus, id, parentID) => bus("session.created", { info: { id, ...(parentID ? { parentID } : {}), directory: `/w/${SECRET}`, title: SECRET, projectID: SECRET } });
const idleBoth = async (bus, sessionID) => {
  await bus("session.status", { sessionID, status: { type: "idle" } });
  await bus("session.idle", { sessionID });
};
const toolPart = (bus, sessionID, callID, status, tool = "bash") =>
  bus("message.part.updated", {
    part: { id: "prt_1", sessionID, messageID: "msg_1", type: "tool", callID, tool, state: { status, input: { command: SECRET }, output: SECRET, title: SECRET, metadata: { output: SECRET } } },
  });

const saved = { ...process.env };
afterEach(() => {
  for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
  Object.assign(process.env, saved);
});

test("module has exactly one export, the default plugin function", () => {
  expect(Object.keys(mod)).toEqual(["default"]);
  expect(typeof mod.default).toBe("function");
});

test("root sessions start a session, child sessions a subagent", async () => {
  const t = await setup();
  await created(t.bus, "ses_root");
  await created(t.bus, "ses_child", "ses_root");
  await created(t.bus, "ses_root"); // repeated: nothing
  expect(t.spawns.map(s => s.argv)).toEqual([["areel", "hook", "generic"], ["areel", "hook", "generic"]]);
  expect(t.events()).toEqual([
    ev("session.start", "ses_root"),
    ev("subagent.start", "ses_child", { subagent: { id: "ses_child", parent: "ses_root" } }),
  ]);
});

test("prompt then both idle events end the turn once; idle without prompt is silent", async () => {
  const t = await setup();
  await created(t.bus, "ses_root");
  await t.hooks["chat.message"]({ sessionID: "ses_root", agent: "build", model: { providerID: SECRET, modelID: SECRET }, messageID: "msg_1" }, { message: { id: "msg_1" }, parts: [{ type: "text", text: SECRET }] });
  await idleBoth(t.bus, "ses_root");
  await idleBoth(t.bus, "ses_root");
  await t.bus("session.status", { sessionID: "ses_root", status: { type: "busy" } });
  expect(t.events()).toEqual([ev("session.start", "ses_root"), ev("prompt", "ses_root"), ev("turn.end", "ses_root")]);
});

test("an unknown session is a root: its prompt and turn are sent", async () => {
  const t = await setup();
  await idleBoth(t.bus, "ses_old");
  await t.hooks["chat.message"]({ sessionID: "ses_old" }, {});
  await idleBoth(t.bus, "ses_old");
  expect(t.events()).toEqual([ev("prompt", "ses_old"), ev("turn.end", "ses_old")]);
});

test("a child session sends no prompt and one subagent.end", async () => {
  const t = await setup();
  await created(t.bus, "ses_child", "ses_root");
  await t.hooks["chat.message"]({ sessionID: "ses_child" }, { parts: [{ type: "text", text: SECRET }] });
  await idleBoth(t.bus, "ses_child");
  await idleBoth(t.bus, "ses_child");
  expect(t.events()).toEqual([
    ev("subagent.start", "ses_child", { subagent: { id: "ses_child", parent: "ses_root" } }),
    ev("subagent.end", "ses_child", { subagent: { id: "ses_child", parent: "ses_root" } }),
  ]);
});

test("tool parts: pending/running/running/completed send start and end once", async () => {
  const t = await setup();
  for (const status of ["pending", "running", "running", "completed", "completed"]) await toolPart(t.bus, "ses_root", "call_1", status);
  await toolPart(t.bus, "ses_root", "call_2", "running", "read");
  await toolPart(t.bus, "ses_root", "call_2", "error", "read");
  await t.bus("message.part.updated", { part: { type: "text", sessionID: "ses_root", text: SECRET } });
  expect(t.events()).toEqual([
    ev("tool.start", "ses_root", { tool: { name: "bash" } }),
    ev("tool.end", "ses_root", { tool: { name: "bash" } }),
    ev("tool.start", "ses_root", { tool: { name: "read" } }),
    ev("tool.error", "ses_root", { tool: { name: "read" } }),
  ]);
});

test("tool parts of child sessions are dropped", async () => {
  const t = await setup();
  await created(t.bus, "ses_child", "ses_root");
  for (const status of ["running", "completed"]) await toolPart(t.bus, "ses_child", "call_c", status);
  expect(t.events()).toEqual([ev("subagent.start", "ses_child", { subagent: { id: "ses_child", parent: "ses_root" } })]);
});

test("the call dedupe map is bounded to 512 entries", async () => {
  const t = await setup();
  for (let i = 0; i < 513; i++) {
    t.clock.t += 1000; // keep the bucket full
    await toolPart(t.bus, "ses_root", `call_${i}`, "running");
  }
  t.clock.t += 1000;
  await toolPart(t.bus, "ses_root", "call_0", "running"); // forgotten: sent again
  await toolPart(t.bus, "ses_root", "call_512", "running"); // remembered: not sent
  expect(t.spawns.length).toBe(514);
});

test("permission asked and replied", async () => {
  const t = await setup();
  await t.bus("permission.asked", { id: "per_1", sessionID: "ses_root", permission: "bash", patterns: [SECRET], metadata: { command: SECRET }, always: [SECRET], tool: { messageID: "msg_1", callID: "call_1" } });
  await t.bus("permission.replied", { sessionID: "ses_root", requestID: "per_1", reply: "reject" });
  await t.bus("permission.replied", { sessionID: "ses_root", requestID: "per_1", reply: "once" });
  await t.bus("permission.replied", { sessionID: "ses_root", permissionID: "per_1", response: "deny" });
  await t.bus("permission.replied", { sessionID: "ses_root", permissionID: "per_1", response: "always" });
  await t.bus("permission.replied", { sessionID: "ses_root", requestID: "per_1" });
  await t.bus("permission.asked", { id: "per_2", sessionID: "ses_root" });
  expect(t.events()).toEqual([
    ev("permission.request", "ses_root", { tool: { name: "bash" } }),
    ev("permission.reply", "ses_root", { status: "denied" }),
    ev("permission.reply", "ses_root", { status: "ok" }),
    ev("permission.reply", "ses_root", { status: "denied" }),
    ev("permission.reply", "ses_root", { status: "ok" }),
    ev("permission.reply", "ses_root"),
    ev("permission.request", "ses_root"),
  ]);
});

test("compaction start and end", async () => {
  const t = await setup();
  const output = { context: [], prompt: undefined };
  await t.hooks["experimental.session.compacting"]({ sessionID: "ses_root" }, output);
  await t.bus("session.compacted", { sessionID: "ses_root" });
  expect(output).toEqual({ context: [], prompt: undefined });
  expect(t.events()).toEqual([ev("compaction.start", "ses_root"), ev("compaction.end", "ses_root")]);
});

test("dispose ends every root session, once", async () => {
  const t = await setup();
  await created(t.bus, "ses_a");
  await created(t.bus, "ses_b");
  await created(t.bus, "ses_child", "ses_a");
  await t.hooks["chat.message"]({ sessionID: "ses_c" }, {});
  const before = t.spawns.length;
  await t.hooks.dispose();
  await t.hooks.dispose();
  expect(t.events().slice(before)).toEqual([ev("session.end", "ses_a"), ev("session.end", "ses_b"), ev("session.end", "ses_c")]);
});

test("rate limit: 33 events in one tick make 32 spawns, then 8 per second", async () => {
  const t = await setup();
  for (let i = 0; i < 33; i++) await t.hooks["chat.message"]({ sessionID: "ses_root" }, {});
  expect(t.spawns.length).toBe(32);
  t.clock.t += 125; // one token
  await t.hooks["chat.message"]({ sessionID: "ses_root" }, {});
  await t.hooks["chat.message"]({ sessionID: "ses_root" }, {});
  expect(t.spawns.length).toBe(33);
  t.clock.t += 60_000; // refills to the burst, not beyond
  for (let i = 0; i < 40; i++) await t.hooks["chat.message"]({ sessionID: "ses_root" }, {});
  expect(t.spawns.length).toBe(65);
});

test("in-flight cap: at most 4 children until one exits or fails", async () => {
  const t = await setup({ exit: "never" });
  for (let i = 0; i < 6; i++) await t.hooks["chat.message"]({ sessionID: "ses_root" }, {});
  expect(t.spawns.length).toBe(4);
  t.spawns[0].child.emit("exit", 0, null);
  t.spawns[0].child.emit("exit", 0, null); // counted once
  t.spawns[1].child.emit("error", new Error("ENOENT"));
  t.spawns[1].child.emit("exit", 1, null); // the same child: still one slot
  t.spawns[2].child.stdin.emit("error", new Error("EPIPE")); // not an exit
  for (let i = 0; i < 4; i++) await t.hooks["chat.message"]({ sessionID: "ses_root" }, {});
  expect(t.spawns.length).toBe(6);
});

test("no hook is awaited on the child and nothing throws out of a hook", async () => {
  const throwing = await plugin(input, { spawn: () => { throw new Error("spawn failed"); } });
  const broken = await plugin(input, { spawn: () => ({}) }); // no on/stdin
  for (const hooks of [throwing, broken]) {
    await hooks["chat.message"]({ sessionID: "ses_root" }, {});
    await hooks["experimental.session.compacting"]({ sessionID: "ses_root" }, {});
    await hooks.dispose();
    for (const arg of [undefined, null, {}, { event: null }, { event: { type: "session.created" } }, { event: { type: "session.created", properties: { info: null } } }, { event: { type: "message.part.updated", properties: { part: { type: "tool", state: null } } } }, { event: { type: "permission.replied", properties: null } }]) {
      await hooks.event(arg);
    }
    await hooks["chat.message"](undefined, undefined);
    await hooks["experimental.session.compacting"](null, null);
  }
  // A never-exiting child does not delay any hook.
  const t = await setup({ exit: "never" });
  const started = performance.now();
  await t.hooks["chat.message"]({ sessionID: "ses_root" }, {});
  await t.hooks["experimental.session.compacting"]({ sessionID: "ses_root" }, {});
  await t.hooks.dispose();
  expect(performance.now() - started).toBeLessThan(50);
  expect(t.spawns.length).toBe(3);
});

test("no content text reaches any payload", async () => {
  const t = await setup();
  const bus = t.bus;
  await created(bus, "ses_root");
  await created(bus, "ses_child", "ses_root");
  await t.hooks["chat.message"]({ sessionID: "ses_root", agent: SECRET, model: { providerID: SECRET, modelID: SECRET }, messageID: "msg_1", variant: SECRET }, { message: { id: "msg_1", summary: { title: SECRET } }, parts: [{ type: "text", text: SECRET }, { type: "file", url: `file:///${SECRET}`, filename: SECRET }] });
  await bus("message.updated", { info: { id: "msg_1", sessionID: "ses_root", role: "user", path: { cwd: SECRET, root: SECRET } } });
  await bus("message.part.updated", { part: { type: "text", sessionID: "ses_root", text: SECRET } });
  for (const status of ["pending", "running", "completed"]) await toolPart(bus, "ses_root", "call_1", status);
  await toolPart(bus, "ses_root", "call_2", "error");
  await bus("permission.asked", { id: "per_1", sessionID: "ses_root", permission: "edit", patterns: [`/src/${SECRET}.ts`], metadata: { filepath: SECRET, diff: SECRET }, always: [SECRET], tool: { messageID: "msg_1", callID: "call_1" } });
  await bus("permission.replied", { sessionID: "ses_root", requestID: "per_1", reply: "once", message: SECRET });
  await t.hooks["experimental.session.compacting"]({ sessionID: "ses_root" }, { context: [SECRET] });
  await bus("session.compacted", { sessionID: "ses_root" });
  await bus("session.error", { sessionID: "ses_root", error: { name: "UnknownError", data: { message: SECRET } } });
  await bus("session.updated", { info: { id: "ses_root", title: SECRET, directory: SECRET } });
  await idleBoth(bus, "ses_root");
  await idleBoth(bus, "ses_child");
  await t.hooks.dispose();
  expect(t.spawns.length).toBe(13);
  for (const s of t.spawns) {
    expect(s.raw).not.toContain(SECRET);
    expect(s.argv.join(" ")).not.toContain(SECRET);
  }
});

test("payloads stay under 2 KiB with hostile ids and names", async () => {
  const t = await setup();
  const huge = "\u0001\ud800".repeat(4096);
  const long = "a".repeat(128);
  const root = "b".repeat(128);
  await created(t.bus, long, root);
  await created(t.bus, "x".repeat(129)); // unusable id: nothing
  await t.bus("session.created", { info: { id: "ses_c2", parentID: huge } }); // child, parent not sent
  await toolPart(t.bus, root, "c".repeat(256), "running", huge);
  await toolPart(t.bus, root, "c".repeat(257), "running", huge); // unbounded call id: dropped
  await t.bus("permission.asked", { id: "p", sessionID: root, permission: huge });
  for (const s of t.spawns) expect(new TextEncoder().encode(s.raw).length).toBeLessThan(2048);
  expect(t.events().map(e => e.event)).toEqual(["subagent.start", "subagent.start", "tool.start", "permission.request"]);
  expect(t.events()[1].subagent).toEqual({ id: "ses_c2" });
  expect(t.events()[2].tool.name.length).toBe(64);
});

test("AREEL_HOOK off, 0 or false disables every hook", async () => {
  for (const value of ["off", "0", "false", " FALSE ", "Off"]) {
    process.env.AREEL_HOOK = value;
    const f = fake();
    expect(await plugin(input, { spawn: f.spawner })).toEqual({});
  }
  process.env.AREEL_HOOK = "on";
  expect(Object.keys(await plugin(input, {})).sort()).toEqual(["chat.message", "dispose", "event", "experimental.session.compacting"]);
});

test("the default spawner pipes the JSON to areel hook generic on PATH", async () => {
  const dir = mkdtempSync(join(tmpdir(), "areel-shim-"));
  try {
    const out = join(dir, "out");
    writeFileSync(join(dir, "areel"), `#!/bin/sh\nprintf '%s\\n' "$*" > "${out}.tmp"\ncat >> "${out}.tmp"\nmv "${out}.tmp" "${out}"\necho noise\necho noise >&2\n`);
    chmodSync(join(dir, "areel"), 0o755);
    process.env.PATH = `${dir}:${process.env.PATH}`;
    const hooks = await plugin(input, JSON.parse('{"some":"user options"}'));
    await hooks["chat.message"]({ sessionID: "ses_root" }, {});
    for (let i = 0; i < 200 && !existsSync(out); i++) await Bun.sleep(10);
    const [argv, json] = readFileSync(out, "utf8").split("\n");
    expect(argv).toBe("hook generic");
    expect(JSON.parse(json)).toEqual(ev("prompt", "ses_root"));
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("a missing areel binary is swallowed", async () => {
  const dir = mkdtempSync(join(tmpdir(), "areel-shim-"));
  try {
    process.env.PATH = dir; // empty: spawn fails with ENOENT
    const hooks = await plugin(input);
    for (let i = 0; i < 6; i++) await hooks["chat.message"]({ sessionID: "ses_root" }, {});
    await Bun.sleep(100);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
