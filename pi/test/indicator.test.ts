import assert from "node:assert/strict";
import { test } from "node:test";
import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { install } from "../extensions/index.ts";
import { LIMITS, RECORDING_INDICATOR, newProcessState, type Clock } from "../extensions/adapter.ts";
import type { Invocation, Runner } from "../extensions/runner.ts";
// CLI operation words: two-word integration groups, one-word lifecycle commands.
const op = (call: { args: string[] }) => ["activity", "producer", "service"].includes(call.args[0]) ? `${call.args[0]} ${call.args[1]}` : call.args[0];

// The persistent status slot is visible inside the recording, so the paint
// channel (setStatus) is tracked separately from on-demand notifications.
const record = "a".repeat(32);
const session = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa";
const manual = JSON.stringify(["/manual/areel", "/manual/library", record]);
const secret = "PRIVATE_DIAGNOSTIC_SENTINEL";
class Time implements Clock {
 value = 0; next = 0; jobs = new Map<number, { at: number; fn: () => void }>();
 now() { return this.value; }
 later(fn: () => void, ms: number) { const id = ++this.next; this.jobs.set(id, { at: this.value + ms, fn }); return () => { this.jobs.delete(id); }; }
 advance(ms: number) {
  const end = this.value + ms;
  for (let count = 0; ; count++) {
   assert.ok(count < 1000, "bounded fake timer work");
   const next = [...this.jobs].filter(([, job]) => job.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
   if (!next) break; this.value = next[1].at; this.jobs.delete(next[0]); next[1].fn();
  }
  this.value = end;
 }
}
const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
function recording(id = record) { return { id, status: "recording", suppressed: false, library: "/reported/library", target: { backend: "zellij", instance: "named:opaque", name: "current", address: "/private/namespace" } }; }
function ack(call: Invocation): unknown {
 if (op(call).startsWith("producer ")) return { accepted: false, failed: false, durable: false, registration: { generation: "1".repeat(32), descriptor: JSON.parse(call.input!).descriptor, expiresAt: "2030-01-01T00:00:00Z", durability: "volatile-holder-lifetime" } };
 if (op(call) === "record") return recording();
 if (op(call) === "activity send") { const body = JSON.parse(call.input!); return { accepted: true, failed: false, durable: true, receipt: { id: body.id, sequence: 1 }, registeredContext: { generation: call.args.at(-1), descriptor: { producer: body.producer, agentInstance: body.agentInstance, conversation: body.conversation, parent: null, location: null } } }; }
 return { id: record, status: op(call) === "pause" ? "paused" : op(call) === "stop" ? "stopped" : "recording" };
}
function fixture(run?: Runner, override: Record<string, unknown> = {}) {
 const time = new Time(); const calls: Invocation[] = []; const painted: (string | undefined)[] = []; const notices: string[] = [];
 const handlers = new Map<string, (event: unknown, ctx: ExtensionCommandContext) => unknown>(); let command!: (args: string, ctx: ExtensionCommandContext) => Promise<void>;
 const pi = { on(name: string, fn: (event: unknown, ctx: ExtensionCommandContext) => unknown) { handlers.set(name, fn); }, registerCommand(_name: string, value: { handler: typeof command }) { command = value.handler; } } as unknown as ExtensionAPI;
 const ui = { setStatus(key: string, value: string | undefined) { assert.equal(key, "agent-reel"); painted.push(value); }, notify(value: string) { notices.push(value); } };
 const ctx = { hasUI: true, sessionManager: { getSessionId: () => session }, ui, ...override } as unknown as ExtensionCommandContext;
 const adapter = install(pi, newProcessState(), (call, signal) => { calls.push(call); return run ? run(call, signal) : Promise.resolve(ack(call)); }, time);
 const onlyIndicator = () => assert.ok(painted.every(v => v === undefined || v === RECORDING_INDICATOR), `persistent slot received ${JSON.stringify(painted)}`);
 return { adapter, time, calls, painted, notices, onlyIndicator, emit: (name: string, value: unknown = {}) => handlers.get(name)?.(value, ctx), command: (args: string) => command(args, ctx) };
}

test("persistent slot paints exactly the indicator while reported recording is fresh, and clears otherwise", async () => {
 const f = fixture(); f.emit("session_start"); assert.equal(f.painted.length, 1); assert.equal(f.painted[0], undefined);
 await f.command("bind " + manual); await flush(); assert.equal(f.painted.at(-1), undefined); assert.equal(f.adapter.indicator(), undefined);
 await f.command("status"); await flush();
 assert.equal(f.painted.at(-1), RECORDING_INDICATOR); assert.equal(f.adapter.indicator(), "🔴"); assert.match(f.adapter.status(), /visual:reported-recording/);
 f.time.advance(LIMITS.visualAge - 1); assert.equal(f.painted.at(-1), RECORDING_INDICATOR);
 f.time.advance(1); assert.equal(f.painted.at(-1), undefined); assert.equal(f.adapter.indicator(), undefined); assert.match(f.adapter.status(), /visual:unknown/);
 await f.command("status"); await flush(); assert.equal(f.painted.at(-1), RECORDING_INDICATOR);
 await f.command("pause"); assert.equal(f.painted.at(-1), undefined); // dispatch alone clears the indicator
 await flush(); assert.equal(f.painted.at(-1), undefined); assert.match(f.adapter.status(), /visual:reported-paused/);
 await f.command("resume"); await flush(); assert.equal(f.painted.at(-1), RECORDING_INDICATOR);
 await f.command("stop"); await flush(); assert.equal(f.painted.at(-1), undefined); assert.match(f.adapter.status(), /visual:reported-stopped/);
 await f.command("unbind"); assert.equal(f.painted.at(-1), undefined);
 f.emit("session_shutdown", { reason: "quit" }); assert.equal(f.painted.at(-1), undefined);
 f.onlyIndicator(); f.adapter.dispose();
});

test("explicit start paints the indicator; failed, unknown and stale knowledge never paints it", async () => {
 const f = fixture(); f.emit("session_start"); await f.command("start"); await flush();
 assert.equal(f.painted.at(-1), RECORDING_INDICATOR); assert.match(f.adapter.status(), /visual:reported-recording .*start:reported-recording/);
 f.time.advance(LIMITS.visualAge); assert.equal(f.painted.at(-1), undefined); f.onlyIndicator(); f.adapter.dispose();
 const failed = fixture(async () => { throw new Error(secret); }); failed.emit("session_start"); await failed.command("start"); await flush();
 assert.equal(failed.painted.at(-1), undefined); assert.match(failed.adapter.status(), /visual:unknown .*start:outcome-unknown/); failed.onlyIndicator(); failed.adapter.dispose();
 const hanging = fixture(() => new Promise(() => {})); hanging.emit("session_start"); await hanging.command("bind " + manual); await hanging.command("status");
 hanging.time.advance(LIMITS.timeout); assert.equal(hanging.painted.at(-1), undefined); hanging.onlyIndicator(); hanging.adapter.dispose();
});

test("explicit /reel status reports diagnostics as a notification, never through the persistent slot", async () => {
 let release: ((value: unknown) => void) | undefined;
 const f = fixture(call => op(call) === "status" && !release ? new Promise(resolve => { release = resolve; }) : Promise.resolve(ack(call)));
 f.emit("session_start"); await f.command("bind " + manual); await flush();
 const before = f.notices.length; await f.command("status"); await flush(); assert.equal(f.notices.length, before, "diagnostics wait for the settled reply");
 release!(ack(f.calls.at(-1)!)); await flush(); assert.equal(f.notices.length, before + 1);
 assert.match(f.notices.at(-1)!, /^reel visual:reported-recording registration:registered metadata:idle durable:0 unconfirmed:0 dropped:0 anchor:unknown binding:explicit auto:off$/);
 assert.ok(!f.painted.some(v => typeof v === "string" && v.startsWith("reel")), "raw diagnostics must not reach setStatus");
 f.emit("agent_start"); await flush(); await f.command("status"); await flush(); assert.match(f.notices.at(-1)!, /metadata:durable durable:1 /);
 f.onlyIndicator(); f.adapter.dispose();
 const hanging = fixture(call => op(call) === "status" ? new Promise(() => {}) : Promise.resolve(ack(call)));
 hanging.emit("session_start"); await hanging.command("bind " + manual); await flush(); await hanging.command("status");
 const count = hanging.notices.length; hanging.time.advance(LIMITS.timeout);
 assert.equal(hanging.notices.length, count + 1); assert.match(hanging.notices.at(-1)!, /^reel visual:unknown registration:registered /);
 hanging.onlyIndicator(); hanging.adapter.dispose();
});

test("status-display failures stay harmless; an unavailable session clears the slot and warns instead of narrating", async () => {
 let paints = 0;
 const broken = fixture(undefined, { ui: { setStatus() { paints++; throw new Error(secret); }, notify() { throw new Error(secret); } } });
 broken.emit("session_start"); await broken.command("bind " + manual); await flush(); await broken.command("status"); await flush();
 assert.ok(paints > 0); assert.equal(op(broken.calls.at(-1)!), "status"); assert.equal(broken.adapter.indicator(), RECORDING_INDICATOR); broken.adapter.dispose();
 for (const name of ["session_start", "session_tree"]) {
  const f = fixture(undefined, { sessionManager: { getSessionId() { throw new Error(secret); } } });
  f.emit(name); assert.equal(f.painted.length, 1); assert.equal(f.painted[0], undefined);
  assert.match(f.notices.at(-1)!, /^reel unavailable: /); assert.ok(!JSON.stringify(f.notices).includes(secret));
  assert.equal(f.calls.length, 0); f.onlyIndicator(); f.adapter.dispose();
 }
});
