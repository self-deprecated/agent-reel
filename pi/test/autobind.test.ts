import assert from "node:assert/strict";
import { test } from "node:test";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { spawn } from "node:child_process";
import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { install } from "../extensions/index.ts";
import { defaultStartConfig, startConfig, LIMITS, RECORDING_INDICATOR, newProcessState, type Clock, type ProcessState, type StartConfig } from "../extensions/adapter.ts";
import { createRunner, type Invocation, type Runner } from "../extensions/runner.ts";
const op = (call: { args: string[] }) => ["activity", "producer", "service"].includes(call.args[0]) ? `${call.args[0]} ${call.args[1]}` : call.args[0];

const first = "a".repeat(32); const second = "b".repeat(32);
const sessionA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa";
const library = "/reported/library";
const manual = JSON.stringify(["/manual/areel", "/manual/library", "c".repeat(32)]);
class Time implements Clock {
 value = 0; next = 0; jobs = new Map<number, { at: number; fn: () => void }>();
 now() { return this.value; }
 later(fn: () => void, ms: number) { const id = ++this.next; this.jobs.set(id, { at: this.value + ms, fn }); return () => { this.jobs.delete(id); }; }
 advance(ms: number) {
  const end = this.value + ms;
  for (let count = 0; ; count++) {
   assert.ok(count < 2000);
   const next = [...this.jobs].filter(([, j]) => j.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
   if (!next) break; this.value = next[1].at; this.jobs.delete(next[0]); next[1].fn();
  }
  this.value = end;
 }
}
const flush = async () => { for (let i = 0; i < 60; i++) await Promise.resolve(); };
const covered = (recording: string) => ({ covered: true, recording, library, status: "recording", target: { backend: "zellij", instance: "named:opaque", name: "likable-elephant" }, reason: "covered" });
const none = (reason: string) => ({ covered: false, recording: null, library, target: null, reason });
function ack(call: Invocation): unknown {
 const body = JSON.parse(call.input ?? "{}");
 if (op(call).startsWith("producer ")) return { accepted: false, failed: false, durable: false, registration: { generation: "1".repeat(32), descriptor: body.descriptor, expiresAt: "2030-01-01T00:00:00Z", durability: "volatile-holder-lifetime" } };
 if (op(call) === "activity send" || op(call) === "activity mark") return { accepted: true, failed: false, durable: true, receipt: { id: body.id, sequence: 1 }, registeredContext: { generation: call.args.at(-1), descriptor: { producer: body.producer, agentInstance: body.agentInstance, conversation: body.conversation, parent: null, location: null } } };
 return { id: call.args[1], status: "recording" };
}
// probe answers the proof query; everything else is acknowledged normally.
function fixture(probe: (call: Invocation) => Promise<unknown>, state: ProcessState = newProcessState(), time = new Time(), start: StartConfig = defaultStartConfig()) {
 const calls: Invocation[] = []; const shown: (string | undefined)[] = [];
 const handlers = new Map<string, (event: any, ctx: ExtensionCommandContext) => unknown>(); let command!: (args: string, ctx: ExtensionCommandContext) => Promise<void>;
 const pi = { on(name: string, fn: (event: any, ctx: ExtensionCommandContext) => unknown) { handlers.set(name, fn); }, registerCommand(_name: string, value: { handler: typeof command }) { command = value.handler; } } as unknown as ExtensionAPI;
 const ctx = { hasUI: true, sessionManager: { getSessionId: () => sessionA }, ui: { setStatus(_key: string, value: string | undefined) { assert.ok(value === undefined || value === RECORDING_INDICATOR); shown.push(value); }, notify(value: string) { shown.push(value); } } } as unknown as ExtensionCommandContext;
 const runner: Runner = call => { calls.push(call); return op(call) === "activity target" ? probe(call) : Promise.resolve(ack(call)); };
 const adapter = install(pi, state, runner, time, start);
 const probes = () => calls.filter(c => op(c) === "activity target");
 return { adapter, state, time, calls, shown, probes, emit: (name: string, value: unknown = {}) => handlers.get(name)?.(value, ctx), command: (args: string) => command(args, ctx) };
}

test("the real entrypoint enables auto-bind; the bare seam stays inert", () => {
 assert.deepEqual(defaultStartConfig(), { binary: "areel", autoBind: true });
 assert.deepEqual(startConfig(), { binary: "areel" });
 assert.throws(() => startConfig({ autoBind: false as unknown as true }));
 const f = fixture(async () => covered(first), newProcessState(), new Time(), startConfig());
 f.emit("session_start"); f.time.advance(120000); assert.equal(f.calls.length, 0); assert.match(f.adapter.status(), /binding:none auto:off$/); f.adapter.dispose();
});

test("auto-bind: session start proves the covering live recording and binds it; activity flows", async () => {
 const f = fixture(async () => covered(first));
 f.emit("session_start"); f.time.advance(0); await flush();
 assert.deepEqual(f.calls[0], { binary: "areel", args: ["activity", "target", "--json"], discovery: true });
 assert.equal(f.state.binding!.recording, first); assert.equal(f.state.binding!.library, library); assert.equal(f.state.binding!.auto, true);
 assert.equal(op(f.calls[1]), "producer register"); assert.deepEqual(f.calls[1].args.slice(2, 6), ["--library", library, "--recording", first]);
 f.emit("message_end", { message: { role: "user" } }); await flush();
 const mark = f.calls.find(c => op(c) === "activity mark"); assert.ok(mark); assert.ok(mark.args.includes(first)); assert.equal(JSON.parse(mark.input!).kind, "pi:prompt");
 assert.match(f.adapter.status(), /binding:auto auto:on probe:covered$/);
 // Periodic re-proof of the same recording keeps the binding and its producer.
 const producer = f.state.binding!.producer; f.time.advance(LIMITS.autoInterval); await flush();
 assert.equal(f.probes().length, 2); assert.equal(f.state.binding!.producer, producer);
 assert.equal(f.calls.filter(c => op(c) === "producer register").length, 1);
 // No capture control was ever issued and nothing but the indicator slot was painted.
 assert.ok(f.calls.every(c => !["record", "ensure", "resume", "pause", "stop", "status"].includes(op(c))));
 assert.ok(f.shown.every(s => s === undefined || s === RECORDING_INDICATOR));
 f.adapter.dispose(); assert.equal(f.time.jobs.size, 0);
});

test("no recording: quiet periodic probes, no binding, no UI output", async () => {
 const f = fixture(async () => none("no-recording-for-target"));
 f.emit("session_start"); f.time.advance(0); await flush();
 for (let i = 0; i < 3; i++) { f.time.advance(LIMITS.autoInterval); await flush(); }
 assert.equal(f.probes().length, 4); assert.equal(f.calls.length, 4); assert.equal(f.state.binding, undefined);
 assert.ok(f.shown.every(s => s === undefined), "no notice while nothing records");
 assert.match(f.adapter.status(), /binding:none auto:on probe:no-recording-for-target$/);
 f.emit("agent_start"); await flush(); assert.equal(f.calls.length, 4); f.adapter.dispose();
});

test("switching recordings rebinds; an ended recording unbinds; inconclusive and malformed replies keep the binding", async () => {
 let reply: unknown = covered(first);
 const f = fixture(async () => reply);
 f.emit("session_start"); f.time.advance(0); await flush();
 const old = f.state.binding!; assert.equal(old.recording, first);
 reply = covered(second); f.time.advance(LIMITS.autoInterval); await flush();
 assert.equal(f.state.binding!.recording, second); assert.equal(f.state.binding!.auto, true); assert.notEqual(f.state.binding!.producer, old.producer);
 const register = f.calls.filter(c => op(c) === "producer register").at(-1)!; assert.ok(register.args.includes(second));
 f.emit("agent_start"); await flush(); assert.ok(f.calls.filter(c => op(c) === "activity send").every(c => c.args.includes(second)));
 for (const kept of [none("holder-unknown"), { covered: true, recording: "bad" }, { covered: "yes" }, none("Bad Reason"), { ...none("recording-ended"), recording: second }]) {
  reply = kept; f.time.advance(LIMITS.autoInterval); await flush(); assert.equal(f.state.binding?.recording, second, JSON.stringify(kept));
 }
 assert.match(f.adapter.status(), /probe:unknown$/);
 reply = none("recording-ended"); f.time.advance(LIMITS.autoInterval); await flush();
 assert.equal(f.state.binding, undefined); assert.match(f.adapter.status(), /binding:none auto:on probe:recording-ended$/);
 f.emit("agent_start"); await flush(); const count = f.calls.length; f.emit("agent_end"); await flush(); assert.equal(f.calls.length, count);
 reply = covered(first); f.time.advance(LIMITS.autoInterval); await flush(); assert.equal(f.state.binding!.recording, first);
 f.adapter.dispose();
});

test("/reel unbind opts out until /reel bind or /reel start; explicit bindings are never replaced", async () => {
 let reply: unknown = covered(first);
 const f = fixture(async () => reply);
 f.emit("session_start"); f.time.advance(0); await flush(); assert.equal(f.state.binding!.recording, first);
 await f.command("unbind"); await flush();
 assert.equal(f.state.binding, undefined); assert.equal(f.state.autoOptOut, true); assert.match(f.shown.at(-1)!, /binding:none auto:opted-out/);
 const probes = f.probes().length; f.time.advance(LIMITS.autoInterval * 5); await flush();
 assert.equal(f.probes().length, probes, "opted out: no probe child at all"); assert.equal(f.state.binding, undefined);
 // Unbound /reel status reports local diagnostics instead of a refusal.
 await f.command("status"); assert.match(f.shown.at(-1)!, /^reel .* binding:none auto:opted-out/);
 // Opt-out survives reload of the extension (process state).
 f.emit("session_shutdown", { reason: "reload" });
 const next = fixture(async () => reply, f.state, f.time); next.emit("session_start", { reason: "reload" }); next.time.advance(LIMITS.autoInterval); await flush();
 assert.equal(next.probes().length, 0); assert.equal(next.state.binding, undefined);
 // Explicit bind re-enables auto-binding but its explicit binding wins: probes are skipped while it lasts.
 await next.command("bind " + manual); await flush();
 assert.equal(next.state.autoOptOut, false); assert.equal(next.state.binding!.auto, undefined); assert.match(next.adapter.status(), /binding:explicit auto:on/);
 next.time.advance(LIMITS.autoInterval * 3); await flush(); assert.equal(next.probes().length, 0); assert.equal(next.state.binding!.recording, "c".repeat(32));
 // Unbind again: opted out; /reel start clears the opt-out (and binds what it started).
 await next.command("unbind"); assert.equal(next.state.autoOptOut, true);
 await next.command("start"); await flush(); assert.equal(next.state.autoOptOut, false);
 next.adapter.dispose();
});

test("an explicit start on an auto binding takes ownership; a stale probe cannot override an explicit bind", async () => {
 const f = fixture(async () => covered(first));
 f.emit("session_start"); f.time.advance(0); await flush(); const binding = f.state.binding!; assert.equal(binding.auto, true);
 const record = f.calls.length; await f.command("start"); await flush();
 assert.equal(f.calls[record].args.join(" "), `record --recording ${first} --library ${library} --json`);
 f.adapter.dispose();
 let finish!: (value: unknown) => void;
 const g = fixture(() => new Promise(resolve => { finish = resolve; }));
 g.emit("session_start"); g.time.advance(0); assert.ok(g.state.probeFlight);
 await g.command("bind " + manual); await flush(); const explicit = g.state.binding;
 finish(covered(first)); await flush(); assert.equal(g.state.binding, explicit); assert.equal(g.state.binding!.auto, undefined); assert.equal(g.state.probeFlight, undefined);
 g.adapter.dispose();
});

test("a hung probe keeps its single process slot across reload; its deadline aborts it", async () => {
 const state = newProcessState(); const time = new Time();
 const g = fixture(() => new Promise(() => {}), state, time);
 g.emit("session_start"); g.time.advance(0); const flight = state.probeFlight; assert.ok(flight);
 g.time.advance(LIMITS.timeout); assert.equal(flight.signal.aborted, true); assert.equal(state.probeFlight, flight);
 g.emit("session_shutdown", { reason: "reload" });
 const h = fixture(() => new Promise(() => {}), state, time); h.emit("session_start"); h.time.advance(LIMITS.autoInterval * 3); await flush();
 assert.equal(h.probes().length, 0, "the hung child still owns the probe slot"); h.adapter.dispose(); assert.equal(time.jobs.size, 0);
});

function child() { return Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), kill() { return true; } }); }
test("the proof query uses the discovery allowlist and never forwards AREEL_RECORDING", async () => {
 const allowed = { PATH: "/tools", HOME: "/home/user", XDG_RUNTIME_DIR: "/runtime", ZELLIJ_SESSION_NAME: "likable-elephant" };
 const env = { ...allowed, AREEL_RECORDING: second, AREEL_LIBRARY: "/wrong", AREEL_CONFIG: "/redirect.json", ZELLIJ_PANE_ID: "3", SECRET_TOKEN: "x" };
 const spawned: { binary: string; args: string[]; options: any; child: ReturnType<typeof child> }[] = [];
 const fakeSpawn = ((binary: string, args: string[], options: any) => { const value = { binary, args, options, child: child() }; spawned.push(value); return value.child; }) as unknown as typeof spawn;
 const runner = createRunner(fakeSpawn, env);
 const promise = runner({ binary: "areel", args: ["activity", "target", "--json"], discovery: true }, new AbortController().signal);
 assert.deepEqual(spawned[0].options.env, allowed); assert.deepEqual(spawned[0].args, ["activity", "target", "--json"]);
 spawned[0].child.stdout.emit("data", Buffer.from(JSON.stringify(none("no-caller-terminal")))); spawned[0].child.emit("close", 0);
 assert.deepEqual(await promise, none("no-caller-terminal"));
 for (const args of [["activity", "target", "--recording", first], ["activity", "send"], ["activity", "status"]]) await assert.rejects(runner({ binary: "areel", args, discovery: true }, new AbortController().signal), /reel-unavailable/);
 assert.equal(spawned.length, 1);
 // Configured explicit library is passed through to the proof query.
 const f = fixture(async () => none("no-library"), newProcessState(), new Time(), startConfig({ binary: "/opt/areel", library: "/explicit/library", autoBind: true }));
 f.emit("session_start"); f.time.advance(0); await flush();
 assert.deepEqual(f.calls[0], { binary: "/opt/areel", args: ["activity", "target", "--json", "--library", "/explicit/library"], discovery: true }); f.adapter.dispose();
});
