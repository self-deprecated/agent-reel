import assert from "node:assert/strict";
import { test } from "node:test";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { spawn } from "node:child_process";
import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { install } from "../extensions/index.ts";
import { startConfig, LIMITS, RECORDING_INDICATOR, START_TIMEOUT, newProcessState, type Clock, type ProcessState } from "../extensions/adapter.ts";
import { createRunner, DISCOVERY_BYTES, type Invocation, type Runner } from "../extensions/runner.ts";
// CLI operation words: two-word integration groups, one-word lifecycle commands.
const op = (call: { args: string[] }) => ["activity", "producer", "service"].includes(call.args[0]) ? `${call.args[0]} ${call.args[1]}` : call.args[0];

const record = "a".repeat(32); const nextRecord = "b".repeat(32);
const sessionA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"; const sessionB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb";
const secret = "PRIVATE_ENV_RESULT_PATH_SENTINEL";
const manual = JSON.stringify(["/manual/areel", "/manual/library", record]);
class Time implements Clock {
 value = 0; next = 0; jobs = new Map<number, { at: number; fn: () => void }>();
 now() { return this.value; }
 later(fn: () => void, ms: number) { const id = ++this.next; this.jobs.set(id, { at: this.value + ms, fn }); return () => { this.jobs.delete(id); }; }
 advance(ms: number) {
  const end = this.value + ms;
  for (let count = 0; ; count++) {
   assert.ok(count < 1000);
   const next = [...this.jobs].filter(([, j]) => j.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
   if (!next) break; this.value = next[1].at; this.jobs.delete(next[0]); next[1].fn();
  }
  this.value = end;
 }
}
const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
const reported = "/reported/library";
function recording(id = record, name = "current") { return { id, status: "recording", suppressed: false, library: reported, target: { backend: "zellij", instance: "named:opaque", name, address: "/private/namespace" }, segments: [{ number: 1, launched: true }] }; }
function ack(call: Invocation): unknown {
 if (op(call).startsWith("producer ")) return { accepted: false, failed: false, durable: false, registration: { generation: op(call) === "producer replace" ? "2".repeat(32) : "1".repeat(32), descriptor: JSON.parse(call.input!).descriptor, expiresAt: "2030-01-01T00:00:00Z", durability: "volatile-holder-lifetime" } };
 return { id: call.args[1], status: "recording" }; // Bound controls: [operation, REC, --library, L, --json].
}
function fixture(run?: Runner, state: ProcessState = newProcessState(), time = new Time(), start = startConfig()) {
 const calls: Invocation[] = []; const signals: AbortSignal[] = []; const shown: (string | undefined)[] = [];
 const handlers = new Map<string, (event: any, ctx: ExtensionCommandContext) => unknown>(); let command!: (args: string, ctx: ExtensionCommandContext) => Promise<void>;
 const pi = { on(name: string, fn: (event: any, ctx: ExtensionCommandContext) => unknown) { handlers.set(name, fn); }, registerCommand(_name: string, value: { handler: typeof command }) { command = value.handler; } } as unknown as ExtensionAPI;
 let session = sessionA;
 // The persistent slot may only ever carry the recording indicator or be cleared.
 const ctx = { hasUI: true, sessionManager: { getSessionId: () => session }, ui: { setStatus(_key: string, value: string | undefined) { assert.ok(value === undefined || value === RECORDING_INDICATOR, `persistent status ${value}`); shown.push(value); }, notify(value: string) { shown.push(value); } } } as unknown as ExtensionCommandContext;
 const adapter = install(pi, state, (call, signal) => { calls.push(call); signals.push(signal); return run ? run(call, signal) : Promise.resolve(op(call) === "record" ? recording() : ack(call)); }, time, start);
 return { adapter, state, time, calls, signals, shown, emit: (name: string, value: unknown = {}) => handlers.get(name)?.(value, ctx), command: (args: string) => command(args, ctx), session: (value: string) => { session = value; } };
}

test("explicit start only: PATH areel, CLI-chosen library, automatic bind, reported health and existing controls", async () => {
 assert.deepEqual(startConfig(), { binary: "areel" }); assert.deepEqual(startConfig({ binary: "/opt/areel", library: "/explicit/library" }), { binary: "/opt/areel", library: "/explicit/library" });
 for (const bad of [{ binary: "relative/areel" }, { binary: ".." }, { binary: "" }, { library: "relative" }, { library: "/a\nb" }]) assert.throws(() => startConfig(bad));
 const f = fixture(); assert.equal(f.calls.length, 0); assert.equal(f.time.jobs.size, 0);
 f.emit("session_start"); f.emit("agent_start"); f.emit("agent_settled"); f.emit("session_tree"); f.time.advance(60000); assert.equal(f.calls.length, 0);
 await f.command("start"); await flush();
 assert.deepEqual(f.calls[0], { binary: "areel", args: ["record", "--json"], discovery: true });
 assert.equal(f.state.binding!.recording, record); assert.equal(f.state.binding!.library, reported); assert.equal(f.state.binding!.binary, "areel");
 assert.deepEqual(f.calls.map(c => op(c)), ["record", "producer register"]);
 assert.match(f.adapter.status(), /visual:reported-recording registration:registered/);
 for (const action of ["status", "pause", "resume", "stop"]) { await f.command(action); await flush(); assert.deepEqual(f.calls.at(-1)!.args, [action, record, "--library", reported, "--json"]); assert.equal(f.calls.at(-1)!.discovery, undefined); }
 f.emit("session_start", { reason: "reload" }); f.emit("agent_start"); await flush(); assert.equal(f.calls.filter(c => op(c) === "record").length, 1);
 f.adapter.dispose(); assert.equal(f.time.jobs.size, 0);
});

test("explicitly configured start binary and library are passed; the binding uses the CLI-reported library", async () => {
 const f = fixture(undefined, newProcessState(), new Time(), startConfig({ binary: "/opt/areel", library: "/explicit/library" })); f.emit("session_start");
 await f.command("start work"); await flush();
 assert.deepEqual(f.calls[0], { binary: "/opt/areel", args: ["record", "--library", "/explicit/library", "--json", "--", "work"], discovery: true });
 assert.equal(f.state.binding, undefined, "target mismatch: reply names current, not work");
 await f.command("start"); await flush();
 assert.deepEqual(f.calls[1].args, ["record", "--library", "/explicit/library", "--json"]);
 assert.deepEqual([f.state.binding!.binary, f.state.binding!.library], ["/opt/areel", reported]);
 assert.deepEqual(f.calls.at(-1)!.args.slice(0, 6), ["producer", "register", "--library", reported, "--recording", record]); f.adapter.dispose();
});

test("bound start never selects ambient target; same ID retains metadata, new ID after stop rebinds", async () => {
 let id = record;
 const f = fixture(async call => op(call) === "record" ? recording(id) : ["stop", "pause"].includes(op(call)) ? { id: record, status: op(call) === "stop" ? "stopped" : "paused" } : ack(call));
 f.emit("session_start"); await f.command("bind " + manual); await flush();
 const binding = f.state.binding; const ticket = f.state.ticket; f.state.admitted = 17;
 const count = f.calls.length; await f.command("start other"); assert.equal(f.calls.length, count); assert.match(f.shown.at(-1)!, /invalid command/);
 for (const prior of ["recording", "paused"]) {
  if (prior === "paused") { await f.command("pause"); await flush(); }
  await f.command("start"); await flush(); const call = f.calls.at(-1)!;
  assert.deepEqual(call, { binary: "/manual/areel", args: ["record", "--recording", record, "--library", "/manual/library", "--json"] });
  assert.equal(f.state.binding, binding); assert.equal(f.state.ticket, ticket); assert.equal(f.state.admitted, 17);
 }
 await f.command("stop"); await flush(); id = nextRecord; await f.command("start"); await flush();
 assert.equal(f.state.binding!.recording, nextRecord); assert.equal(f.state.binding!.binary, "/manual/areel"); assert.equal(f.state.binding!.library, "/manual/library");
 assert.notEqual(f.state.binding!.producer, binding!.producer); assert.equal(f.state.admitted, 0); assert.equal(op(f.calls.at(-1)!), "producer register");
 assert.ok(f.calls.every(c => c.discovery === undefined)); f.adapter.dispose();
});

test("optional target is one bounded argv value, never shell syntax or metadata", async () => {
 const name = "session with spaces;$(literal)";
 const f = fixture(async call => op(call) === "record" ? recording(record, name) : ack(call)); f.emit("session_start");
 for (const bad of ["start ", "start " + "x".repeat(257), "start a\nsecret", "start a\x00b"]) { await f.command(bad); assert.equal(f.calls.length, 0); }
 await f.command("start " + name); await flush(); assert.deepEqual(f.calls[0].args, ["record", "--json", "--", name]);
 assert.ok(!JSON.stringify(f.calls.slice(1)).includes(name)); assert.ok(!JSON.stringify(f.shown).includes(name)); f.adapter.dispose();
});

test("malformed, failed, pending and mismatched start replies never bind or claim success", async () => {
 const cases: unknown[] = [null, [], {}, { ...recording(), id: "bad" }, { ...recording(), status: "paused" }, { ...recording(), suppressed: true }, { ...recording(), error: secret }, { ...recording(), pending: "stop" }, { ...recording(), pending: false }, { ...recording(), error: null }, { ...recording(), target: null }, { ...recording(), target: { ...recording().target, address: "relative" } }, { ...recording(), target: { ...recording().target, name: "other" } }, { ...recording(), library: undefined }, { ...recording(), library: "relative" }, { ...recording(), library: "/a\nb" }];
 for (const response of cases) {
  const f = fixture(async () => response); f.emit("session_start"); await f.command("start current"); await flush();
  assert.equal(f.state.binding, undefined); assert.equal(f.calls.length, 1); assert.match(f.adapter.status(), /visual:unknown .*start:outcome-unknown/); assert.ok(!JSON.stringify(f.shown).includes(secret));
  f.time.advance(60000); assert.equal(f.calls.length, 1); f.adapter.dispose();
 }
 for (const bound of [false, true]) {
  const f = fixture(call => { if (op(call) === "record") throw new Error(secret); return Promise.resolve(ack(call)); }); f.emit("session_start");
  if (bound) { await f.command("bind " + manual); await flush(); }
  const binding = f.state.binding; await f.command("start"); await flush(); assert.equal(f.state.binding, binding); assert.match(f.adapter.status(), /outcome-unknown/); assert.ok(!JSON.stringify(f.shown).includes(secret)); f.adapter.dispose();
 }
});

test("start-only35s deadline: no retry, no early slot release, no late success; old5s unchanged", async () => {
 let finish!: (value: unknown) => void;
 const f = fixture(() => new Promise(resolve => { finish = resolve; })); f.emit("session_start"); await f.command("start");
 f.time.advance(LIMITS.timeout); assert.equal(f.signals[0].aborted, false); f.time.advance(START_TIMEOUT - LIMITS.timeout - 1); assert.equal(f.signals[0].aborted, false);
 f.time.advance(1); assert.equal(f.signals[0].aborted, true); assert.ok(f.state.controlFlight); assert.match(f.adapter.status(), /outcome-unknown/);
 await f.command("start"); assert.equal(f.calls.length, 1); assert.match(f.shown.at(-1)!, /not dispatched/);
 finish(recording()); await flush(); assert.equal(f.state.binding, undefined); assert.equal(f.state.controlFlight, undefined); assert.match(f.adapter.status(), /visual:unknown/);
 f.time.advance(60000); assert.equal(f.calls.length, 1); f.adapter.dispose();
 const old = fixture(call => op(call) === "status" ? new Promise(() => {}) : Promise.resolve(ack(call))); old.emit("session_start"); await old.command("bind " + manual); await flush(); await old.command("status"); old.time.advance(LIMITS.timeout); assert.equal(old.signals.at(-1)!.aborted, true); assert.equal(LIMITS.timeout, 5000); old.adapter.dispose();
});

test("stale start after bind/unbind/switch/reload cannot replace successor or release successor work", async () => {
 for (const transition of ["bind", "unbind", "tree", "reload"] as const) {
  let finish!: (value: unknown) => void;
  const f = fixture(call => op(call) === "record" ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(ack(call))); f.emit("session_start"); await f.command("start");
  let active = f;
  if (transition === "bind") { await f.command("bind " + manual); await flush(); }
  if (transition === "unbind") await f.command("unbind");
  if (transition === "tree") { f.session(sessionB); f.emit("session_tree"); }
  if (transition === "reload") { f.emit("session_shutdown", { reason: "reload" }); active = fixture(call => op(call) === "record" ? new Promise(() => {}) : Promise.resolve(ack(call)), f.state, f.time); active.emit("session_start", { reason: "reload" }); }
  assert.equal(f.signals[0].aborted, true); const expected = active.state.binding; const shown = active.shown.length;
  await active.command("start"); assert.ok(active.state.controlFlight); // Aborted predecessor still owns lane.
  finish(recording(nextRecord)); await flush(); assert.equal(active.state.binding, expected); assert.equal(active.state.controlFlight, undefined);
  assert.ok(!active.shown.slice(shown).some(s => s?.includes("CLI reports recording")));
  await active.command("start"); await flush(); const signal = active.signals.at(-1)!;
  if (transition === "reload") f.emit("session_shutdown", { reason: "reload" });
  assert.equal(signal.aborted, false); active.adapter.dispose(); f.adapter.dispose();
 }
});

test("metadata flight cannot block operator start and same-ID success leaves its ownership intact", async () => {
 let finishMetadata!: (value: unknown) => void;
 const f = fixture(call => op(call) === "producer register" ? new Promise(resolve => { finishMetadata = resolve; }) : Promise.resolve(recording()));
 f.emit("session_start"); await f.command("bind " + manual); const flight = f.state.flight;
 await f.command("start"); await flush(); assert.equal(f.state.flight, flight); assert.equal(f.signals[0].aborted, false); assert.equal(f.state.binding!.recording, record);
 assert.match(f.adapter.status(), /visual:reported-recording registration:pending/); finishMetadata(ack(f.calls[0])); await flush(); f.adapter.dispose();
});

function child() { return Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), kill() { return true; } }); }
test("SDK command through actual runner: unbound discovery allowlist, absent fallback, privacy and bound PATH-only", async () => {
 const allowed = { PATH: "/tools", AREEL_BACKEND: "configured", HOME: "/home/user", XDG_CONFIG_HOME: "/config", XDG_DATA_HOME: "/data", ZELLIJ_SESSION_NAME: secret, TMUX: "nested", ZELLIJ_SOCKET_DIR: "/namespace", XDG_RUNTIME_DIR: "/runtime", TMPDIR: "/temporary" };
 const env = { ...allowed, AREEL_RECORDING: nextRecord, AREEL_LIBRARY: "/wrong-library", AREEL_CONFIG: "/redirecting-config.json", AREEL_TARGET: "not-forwarded", PI_AUTH: secret, OPENAI_API_KEY: secret, USER: "not-forwarded" };
 const spawned: { binary: string; args: string[]; options: any; child: ReturnType<typeof child> }[] = [];
 const fakeSpawn = ((binary: string, args: string[], options: any) => { const value = { binary, args, options, child: child() }; spawned.push(value); return value.child; }) as unknown as typeof spawn;
 const f = fixture(createRunner(fakeSpawn, env)); f.emit("session_start"); await f.command("start");
 assert.equal(spawned.length, 1); assert.deepEqual(spawned[0].options.env, allowed);
 for (const key of ["AREEL_CONFIG", "AREEL_LIBRARY", "AREEL_RECORDING", "AREEL_TARGET"]) assert.equal(key in spawned[0].options.env, false, key); assert.equal(spawned[0].options.shell, false); assert.equal(spawned[0].args.includes("--recording"), false);
 assert.ok(!JSON.stringify(f.calls).includes(secret)); assert.ok(!JSON.stringify(f.shown).includes(secret));
 const finish = (i: number, value: unknown) => { spawned[i].child.stdout.emit("data", Buffer.from(JSON.stringify(value))); spawned[i].child.emit("close", 0); };
 finish(0, recording()); await flush(); assert.equal(op(spawned[1]), "producer register"); assert.deepEqual(spawned[1].options.env, { PATH: "/tools" }); finish(1, ack(f.calls[1])); await flush();
 await f.command("start"); assert.deepEqual(spawned[2].options.env, { PATH: "/tools" }); assert.ok(spawned[2].args.includes("--recording")); finish(2, recording()); await flush(); f.adapter.dispose();
 const fallback = createRunner(fakeSpawn, { PATH: "/tools", ZELLIJ_SESSION_NAME: "only-session" }); const promise = fallback({ binary: "/fake/areel", args: ["record", "--json"], discovery: true }, new AbortController().signal);
 assert.deepEqual(spawned[3].options.env, { PATH: "/tools", ZELLIJ_SESSION_NAME: "only-session" }); finish(3, recording()); await promise;
});

test("discovery budget/operation checks refuse before even fake spawn; raw secrets never escape", async () => {
 let calls = 0; const fakeSpawn = (() => { calls++; return child(); }) as unknown as typeof spawn;
 for (const env of [{ ZELLIJ_SESSION_NAME: "x".repeat(DISCOVERY_BYTES + 1) }, { ZELLIJ_SOCKET_DIR: "x".repeat(9000), TMPDIR: "x".repeat(9000) }, { TMUX: "x\x00" }]) {
  const run = createRunner(fakeSpawn, env); await assert.rejects(run({ binary: "/fake/areel", args: ["record", "--json"], discovery: true }, new AbortController().signal), /reel-unavailable/);
 }
 for (const args of [["activity", "send"], ["start"], ["record", "--recording", record, "--json"]]) await assert.rejects(createRunner(fakeSpawn, {})({ binary: "/fake/areel", args, discovery: true }, new AbortController().signal), /reel-unavailable/);
 assert.equal(calls, 0);
});

test("native start failures and timeout keep unknown outcome and lane until child close", async () => {
 for (const mode of ["exit", "bad-json", "timeout"] as const) {
  const owned = child(); let launches = 0;
  const fakeSpawn = (() => { launches++; return owned; }) as unknown as typeof spawn;
  const f = fixture(createRunner(fakeSpawn, { PATH: "/tools", ZELLIJ_SESSION_NAME: "current" })); f.emit("session_start"); await f.command("start");
  if (mode === "timeout") { f.time.advance(START_TIMEOUT); assert.ok(f.state.controlFlight); await f.command("start"); assert.equal(launches, 1); }
  owned.stdout.emit("data", Buffer.from(mode === "bad-json" ? secret : JSON.stringify(recording())));
  await flush(); assert.ok(f.state.controlFlight); assert.equal(f.state.binding, undefined);
  owned.emit("close", mode === "exit" ? 1 : 0); await flush();
  assert.equal(f.state.controlFlight, undefined); assert.equal(f.state.binding, undefined); assert.equal(launches, 1); assert.match(f.adapter.status(), /outcome-unknown/); assert.ok(!JSON.stringify(f.shown).includes(secret)); f.adapter.dispose();
 }
});
