import assert from "node:assert/strict";
import { test } from "node:test";
import type { ExtensionAPI, ExtensionCommandContext } from "@earendil-works/pi-coding-agent";
import { install } from "../extensions/index.ts";
import { LIMITS, MARK_LABEL_BYTES, RECORDING_INDICATOR, markLabel, newProcessState, parseBinding, type Clock, type ProcessState } from "../extensions/adapter.ts";
import type { Invocation, Runner } from "../extensions/runner.ts";
// CLI operation words: two-word integration groups, one-word lifecycle commands.
const op = (call: { args: string[] }) => ["activity", "producer", "service"].includes(call.args[0]) ? `${call.args[0]} ${call.args[1]}` : call.args[0];

const sessionA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa";
const sessionB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb";
const record = "a".repeat(32);
const binding = JSON.stringify(["/private/areel", "/private/library", record]);
const secret = "SENTINEL_PRIVATE_BODY_AUTH_PATH";
class Time implements Clock {
 value = 0; next = 0; jobs = new Map<number, { at: number; fn: () => void }>();
 now() { return this.value; }
 later(fn: () => void, ms: number) { const id = ++this.next; this.jobs.set(id, { at: this.value + ms, fn }); return () => { this.jobs.delete(id); }; }
 advance(ms: number) {
  const end = this.value + ms; let count = 0;
  while (true) {
   const next = [...this.jobs].filter(([, job]) => job.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
   if (!next) break;
   assert.ok(++count < 2000, "unbounded timer loop"); this.value = next[1].at; this.jobs.delete(next[0]); next[1].fn();
  }
  this.value = end;
 }
}
const flush = async () => { for (let i = 0; i < 100; i++) await Promise.resolve(); };
function ack(call: Invocation, generation?: string, durable = true): unknown {
 const body = JSON.parse(call.input ?? "{}");
 generation ??= op(call) === "producer replace" ? (BigInt("0x" + body.generation) + 1n).toString(16).padStart(32, "0") : body.generation ?? "1".repeat(32);
 if (op(call).startsWith("producer ")) return { accepted: false, durable: false, failed: false, registration: { generation, descriptor: body.descriptor, expiresAt: "2000-01-01T00:00:00Z", durability: "volatile-holder-lifetime" } };
 if (op(call) === "activity send" || op(call) === "activity mark") return { accepted: true, failed: false, durable, receipt: { id: body.id, sequence: 1 }, registeredContext: { generation: call.args.at(-1), descriptor: { producer: body.producer, agentInstance: body.agentInstance, conversation: body.conversation, parent: null, location: null } } };
 return { id: record, status: "recording", pending: "" };
}
function fixture(options: { state?: ProcessState; time?: Time; run?: Runner } = {}) {
 const state = options.state ?? newProcessState(); const time = options.time ?? new Time();
 const calls: Invocation[] = []; const signals: AbortSignal[] = []; const shown: (string | undefined)[] = [];
 const handlers = new Map<string, (event: any, ctx: ExtensionCommandContext) => unknown>();
 let command!: (args: string, ctx: ExtensionCommandContext) => Promise<void>;
 const pi = { on(name: string, fn: (event: any, ctx: ExtensionCommandContext) => unknown) { handlers.set(name, fn); }, registerCommand(name: string, value: { handler: typeof command }) { assert.equal(name, "reel"); command = value.handler; } } as unknown as ExtensionAPI;
 let session = sessionA;
 // The persistent slot may only ever carry the recording indicator or be cleared.
 const ctx = { hasUI: true, sessionManager: { getSessionId: () => session, getHeader() { throw new Error(secret); }, getSessionFile() { throw new Error(secret); } }, ui: { setStatus(_key: string, value: string | undefined) { assert.ok(value === undefined || value === RECORDING_INDICATOR, `persistent status ${value}`); shown.push(value); }, notify(value: string) { shown.push(value); } } } as unknown as ExtensionCommandContext;
 const runner: Runner = (call, signal) => { calls.push(call); signals.push(signal); return options.run ? options.run(call, signal) : Promise.resolve(ack(call)); };
 const adapter = install(pi, state, runner, time);
 const emit = (name: string, data: unknown = {}) => handlers.get(name)?.(data, ctx);
 return { state, time, calls, signals, shown, handlers, adapter, ctx, emit, command: (s: string) => command(s, ctx), session: (s: string) => { session = s; } };
}

test("load is inert, explicit binding only, privacy allowlist includes errors, slash bypass and continuation", async () => {
 const f = fixture(); assert.equal(f.calls.length, 0); assert.equal(f.time.jobs.size, 0);
 f.emit("session_start", { reason: "startup", previousSessionFile: secret }); f.emit("agent_start"); assert.equal(f.calls.length, 0);
 assert.equal(f.handlers.has("input"), false); assert.equal(f.handlers.has("before_agent_start"), false); assert.equal(f.handlers.has("message_update"), false);
 assert.equal(f.handlers.has("tool_execution_update"), false); assert.equal(f.handlers.has("before_provider_request"), false); assert.equal(f.handlers.has("context"), false);
 await f.command("bind " + binding); await flush(); assert.equal(op(f.calls[0]), "producer register");
 const sensitive = { prompt: secret, text: secret, images: secret, args: secret, result: secret, messages: secret, errorMessage: secret, title: secret, toolCallId: secret, input: secret, environment: secret, auth: secret, path: secret };
 f.emit("agent_start", sensitive); f.emit("turn_start", sensitive); f.emit("tool_execution_start", { ...sensitive, toolName: "bash" });
 f.emit("tool_execution_end", { ...sensitive, toolName: secret, isError: true }); f.emit("agent_end", sensitive);
 f.emit("agent_start", sensitive); f.emit("agent_end", sensitive); await flush();
 // agent_end is not the end of the agent's turn: no pi:turn row before agent_settled.
 assert.equal(f.calls.filter(c => c.input?.includes('"kind":"pi:turn"')).length, 0);
 f.emit("agent_settled", sensitive); f.emit("ui_prompt_start", sensitive); f.emit("session_compact_failed", sensitive); await flush();
 const payload = f.calls.filter(c => op(c) === "activity send" || op(c) === "activity mark").map(c => c.input).join("\n");
 assert.ok(!payload.includes(secret)); assert.ok(payload.includes('"tool":"other"')); assert.ok(payload.includes('"tool":"bash"'));
 assert.ok(payload.includes('"kind":"pi:step","data":{"phase":"start"}')); assert.ok(!payload.includes('"phase":"settled"'));
 const settled = f.calls.filter(c => c.input?.includes('"kind":"pi:turn"'));
 assert.equal(settled.length, 1); assert.equal(op(settled[0]), "activity mark"); assert.ok(settled[0].input!.includes('"kind":"pi:turn","data":{"phase":"end","role":"assistant"}'));
 assert.ok(!JSON.stringify(f.shown).includes(secret));
 assert.ok(f.calls.every(c => !["record", "ensure", "resume", "pause", "stop"].includes(op(c))));
 const envelope = JSON.parse(f.calls.find(c => op(c) === "activity send")!.input!);
 assert.equal(envelope.parent, null); assert.equal(envelope.location, null); assert.equal(envelope.conversation, sessionA);
 await f.command("invalid " + secret); assert.ok(!f.shown.at(-1)!.includes(secret)); f.adapter.dispose(); assert.equal(f.time.jobs.size, 0);
});

test("stable process instance, receiver replacement after reload/switch/fork/resume; separate agent/process identity", async () => {
 const first = fixture(); first.emit("session_start"); await first.command("bind " + binding); await flush();
 const producer = first.state.binding!.producer; const instance = first.state.instance;
 for (const [reason, session] of [["reload", sessionA], ["new", sessionB], ["fork", sessionA], ["resume", sessionB]] as const) {
  const previous = first.state.active!; const expectedGeneration = first.state.ticket!.generation; previous.dispose();
  const next = fixture({ state: first.state }); next.session(session); next.emit("session_start", { reason }); await flush();
  const call = next.calls[0]; assert.equal(op(call), "producer replace");
  const body = JSON.parse(call.input!); assert.equal(body.generation, expectedGeneration); assert.notEqual(next.state.ticket!.generation, expectedGeneration); assert.equal(body.descriptor.agentInstance, instance); assert.equal(body.descriptor.producer, producer); assert.equal(body.descriptor.conversation, session);
  previous.emit("agent", { phase: "late" }); assert.equal(next.calls.length, 1);
 }
 const other = fixture(); other.session(sessionB); other.emit("session_start"); await other.command("bind " + binding); await flush();
 assert.notEqual(other.state.instance, instance); assert.notEqual(other.state.binding!.producer, producer);
 assert.equal(JSON.parse(other.calls[0].input!).descriptor.conversation, sessionB);
 first.state.active!.dispose(); other.adapter.dispose();
});

test("late registration completion cannot rebind; hanging work retains one process slot across reloads", async () => {
 let finish!: (value: unknown) => void;
 const old = fixture({ run: () => new Promise(resolve => { finish = resolve; }) }); old.emit("session_start"); await old.command("bind " + binding);
 old.time.advance(LIMITS.timeout); assert.equal(old.calls.length, 1); assert.equal(old.signals[0].aborted, true); assert.match(old.adapter.status(), /registration:unknown/);
 old.emit("session_shutdown", { reason: "reload" });
 const next = fixture({ state: old.state }); next.session(sessionB); next.emit("session_start");
 assert.equal(next.calls.length, 0); assert.match(next.adapter.status(), /rebind-required/);
 await next.command("bind " + binding); next.time.advance(10000); assert.equal(next.calls.length, 0);
 finish(ack(old.calls[0], "f".repeat(32))); await flush();
 assert.equal(next.calls.length, 1); assert.equal(op(next.calls[0]), "producer register");
 assert.equal(next.state.ticket!.descriptor.conversation, sessionB); assert.notEqual(next.state.ticket!.generation, "f".repeat(32)); next.adapter.dispose();
});

test("bounded exact retries: pending is not durable; failure diagnostics never forwarded", async () => {
 const f = fixture({ run: async call => ack(call, "1".repeat(32), false) }); f.emit("session_start"); await f.command("bind " + binding); await flush();
 f.emit("agent_start"); await flush(); assert.match(f.adapter.status(), /metadata:pending durable:0/);
 f.time.advance(1000); await flush(); const events = f.calls.filter(c => op(c) === "activity send");
 assert.equal(events.length, 2); assert.equal(events[0].input, events[1].input); assert.deepEqual(events[0].args, events[1].args);
 assert.match(f.adapter.status(), /pending-unconfirmed durable:0 unconfirmed:1/); f.time.advance(5000); await flush(); assert.equal(f.calls.filter(c => op(c) === "activity send").length, 2); f.adapter.dispose();
 const bad = fixture({ run: async () => { throw new Error(secret); } }); bad.emit("session_start"); await bad.command("bind " + binding); await flush(); bad.time.advance(1000); await flush();
 assert.equal(bad.calls.length, 2); assert.match(bad.adapter.status(), /unknown-rebind-required/); assert.ok(!JSON.stringify(bad.shown).includes(secret));
 bad.time.advance(60000); await flush(); assert.equal(bad.calls.length, 2); bad.adapter.dispose();
});

test("queue/bytes/rate caps, immutable queued generation, dropped late callbacks", async () => {
 let release!: (value: unknown) => void;
 const f = fixture({ run: call => op(call) === "activity send" ? new Promise(resolve => { release = resolve; }) : Promise.resolve(ack(call)) });
 f.emit("session_start"); await f.command("bind " + binding); await flush();
 for (let i = 0; i < 1000; i++) f.emit("tool_execution_start", { toolName: "bash" });
 assert.equal(f.calls.length, 2); assert.ok(f.state.admitted <= LIMITS.queue + 1); assert.match(f.adapter.status(), /dropped:[1-9]/); assert.ok(f.time.jobs.size <= 2);
 const oldInput = f.calls[1].input; f.session(sessionB); f.emit("session_tree"); f.emit("agent_end"); release(ack(f.calls[1])); await flush();
 assert.equal(f.calls.filter(c => op(c) === "activity send").length, 1); assert.equal(f.calls[1].input, oldInput); assert.match(f.adapter.status(), /durable:0/);
 f.adapter.dispose();
});

test("refresh uses receiver generation, visual status ages to unknown and controls are explicit", async () => {
 const f = fixture(); f.emit("session_start"); await f.command("bind " + binding); await flush();
 f.time.advance(LIMITS.refresh); await flush(); assert.equal(op(f.calls.at(-1)!), "producer refresh");
 assert.equal(JSON.parse(f.calls.at(-1)!.input!).generation, "1".repeat(32));
 assert.match(f.adapter.status(), /visual:unknown/); await f.command("status"); await flush(); assert.match(f.adapter.status(), /visual:reported-recording/);
 f.time.advance(LIMITS.visualAge); assert.match(f.adapter.status(), /visual:unknown/);
 for (const action of ["pause", "resume", "stop"]) { await f.command(action); await flush(); assert.equal(op(f.calls.at(-1)!), action); }
 await f.command("unbind"); const count = f.calls.length; f.emit("agent_start"); f.time.advance(60000); assert.equal(f.calls.length, count); assert.equal(f.time.jobs.size, 0); f.adapter.dispose();
});

test("malformed/stale durable receipts do not earn durable ticks; no session files/paths used", async () => {
 const f = fixture({ run: async call => {
  const response = ack(call) as any;
  if (op(call) === "activity send") response.registeredContext.generation = "f".repeat(32);
  return response;
 } });
 f.session(secret); f.emit("session_start"); await f.command("bind " + binding); await flush();
 assert.equal(JSON.parse(f.calls[0].input!).descriptor.conversation, null);
 f.emit("agent_start"); await flush(); f.time.advance(1000); await flush(); assert.match(f.adapter.status(), /metadata:unknown durable:0/); assert.ok(!JSON.stringify(f.calls).includes(secret)); f.adapter.dispose();
 for (const text of ["[]", '{}', '["relative","/lib","' + record + '"]', JSON.stringify(["/bin", "/lib", "bad"]), "x".repeat(8193)]) assert.throws(() => parseBinding(text));
});


test("operator lane stays independent of a wedged metadata child; no overlapping controls", async () => {
 let releaseMetadata!: (value: unknown) => void; let releaseControl!: (value: unknown) => void;
 const f = fixture({ run: call => op(call).startsWith("producer ") ? Promise.resolve(ack(call)) : new Promise(resolve => { if (op(call) === "activity send") releaseMetadata = resolve; else releaseControl = resolve; }) });
 f.emit("session_start"); await f.command("bind " + binding); await flush(); f.emit("agent_start");
 await f.command("stop"); assert.equal(op(f.calls.at(-1)!), "stop"); const stopCall = f.calls.at(-1)!;
 const count = f.calls.length; await f.command("resume"); assert.equal(f.calls.length, count); assert.match(f.shown.at(-1)!, /control not dispatched/);
 f.time.advance(LIMITS.timeout); assert.ok(f.state.flight); assert.ok(f.state.controlFlight); assert.ok(f.time.jobs.size <= 3);
 releaseControl({ ...ack(stopCall) as object, status: "stopped" }); await flush(); assert.match(f.adapter.status(), /visual:unknown/); // deadline is not late success
 assert.equal(f.state.controlFlight, undefined); assert.ok(f.state.flight);
 releaseMetadata(ack(f.calls[1])); await flush(); f.adapter.dispose();
});


test("serialized byte/envelope limits are enforced independently of queue count", async () => {
 let release!: (value: unknown) => void;
 const f = fixture({ run: call => op(call) === "activity send" ? new Promise(resolve => { release = resolve; }) : Promise.resolve(ack(call)) });
 f.emit("session_start"); await f.command("bind " + binding); await flush();
 f.adapter.emit("test", { phase: "x".repeat(LIMITS.eventBytes) }); assert.equal(f.calls.length, 1);
 for (let i = 0; i < 100; i++) f.adapter.emit("test", { phase: "x".repeat(1200) });
 const queue = (f.adapter as unknown as { queue: { call: Invocation }[] }).queue;
 assert.ok(queue.length > 0 && queue.length < LIMITS.queue);
 assert.ok(queue.reduce((sum, item) => sum + Buffer.byteLength(item.call.input!), 0) <= LIMITS.bytes);
 f.adapter.dispose(); release(ack(f.calls[1])); await flush();
 assert.match(f.adapter.status(), /dropped:\d+\(bytes:\d+,size:1/);
 const busy = fixture(); busy.emit("session_start"); await busy.command("bind " + binding); await flush();
 for (let i = 0; i < 100; i++) { busy.emit("agent_start"); await flush(); busy.time.advance(125); await flush(); }
 const reload = fixture({ state: busy.state }); reload.emit("session_start", { reason: "reload" }); await flush(); reload.emit("agent_start"); await flush();
 // No lifetime budget survives reload either: the successor keeps admitting.
 assert.equal(reload.calls.length, 2); assert.equal(op(reload.calls[0]), "producer replace"); assert.equal(op(reload.calls[1]), "activity send");
 assert.equal(reload.state.drops.dropped, 0); reload.adapter.dispose();
});

test("receiver refresh/replace generation invariants are checked, not silently rebound", async () => {
 for (const operation of ["producer refresh", "producer replace"]) {
  const f = fixture({ run: async call => {
   if (op(call) !== operation) return ack(call);
   const previous = JSON.parse(call.input!).generation;
   return ack(call, operation === "producer refresh" ? "f".repeat(32) : previous);
  } });
  f.emit("session_start"); await f.command("bind " + binding); await flush();
  if (operation === "producer refresh") f.time.advance(LIMITS.refresh); else f.emit("session_tree");
  await flush(); f.time.advance(1000); await flush();
  assert.match(f.adapter.status(), /registration:unknown-rebind-required/);
  assert.equal(f.state.ticket!.generation, "1".repeat(32)); f.emit("agent_start");
  assert.equal(f.calls.filter(c => op(c) === "activity send").length, 0); f.adapter.dispose();
 }
});


test("late shutdown from an old SDK closure cannot abort successor work", async () => {
 const old = fixture(); old.emit("session_start"); await old.command("bind " + binding); await flush();
 let release!: (value: unknown) => void;
 const next = fixture({ state: old.state, run: call => op(call) === "activity send" ? new Promise(resolve => { release = resolve; }) : Promise.resolve(ack(call)) });
 next.emit("session_start", { reason: "reload" }); await flush(); next.emit("agent_start");
 old.emit("session_shutdown", { reason: "reload" }); old.emit("agent_end");
 assert.equal(next.signals.at(-1)!.aborted, false); assert.equal(next.state.uncertain, false);
 release(ack(next.calls.at(-1)!)); await flush(); assert.match(next.adapter.status(), /durable:1/); next.adapter.dispose();
});

// Proxy events record every property touched (dotted for nesting) and throw on
// anything outside the allowlist. Handlers swallow errors, so the exact touched
// list plus the emitted envelope together prove which getters ran.
function spy(allow: string[], values: Record<string, unknown>, touched: string[], prefix = ""): unknown {
 const forbidden = (what: string) => { throw new Error(`forbidden ${what} ${secret}`); };
 return new Proxy({}, {
  get(_target, key) {
   const name = prefix + String(key); touched.push(name);
   if (!allow.includes(name)) forbidden("get " + name);
   const value = values[name];
   return value && typeof value === "object" ? spy(allow, values, touched, name + ".") : value;
  },
  has: () => forbidden("has"), ownKeys: () => forbidden("enumerate"), getOwnPropertyDescriptor: () => forbidden("describe"),
 });
}

test("hooks touch exactly the allowlisted keys: message roles, tool name/error, model source, nothing for thinking", async () => {
 const f = fixture(); f.emit("session_start"); await f.command("bind " + binding); await flush();
 const messageKeys = ["message", "message.role"];
 const cases: [string, string[], Record<string, unknown>, string, string][] = [
  ["message_start", messageKeys, { message: {}, "message.role": "user" }, "activity send", '"kind":"pi:message","data":{"phase":"start","role":"user"}'],
  ["message_end", messageKeys, { message: {}, "message.role": "user" }, "activity mark", '"kind":"pi:prompt","data":{"phase":"submit","role":"user"}'],
  ["message_end", messageKeys, { message: {}, "message.role": "assistant" }, "activity send", '"kind":"pi:message","data":{"phase":"end","role":"assistant"}'],
  ["message_start", messageKeys, { message: {}, "message.role": "toolResult" }, "activity send", '"data":{"phase":"start","role":"toolResult"}'],
  ["message_end", messageKeys, { message: {}, "message.role": "toolResult" }, "activity send", '"kind":"pi:message","data":{"phase":"end","role":"toolResult"}'],
  ["message_end", messageKeys, { message: {}, "message.role": "custom" }, "activity send", '"kind":"pi:message","data":{"phase":"end","role":"other"}'],
  ["message_end", messageKeys, { message: {}, "message.role": secret }, "activity send", '"kind":"pi:message","data":{"phase":"end","role":"other"}'],
  ["tool_execution_start", ["toolName"], { toolName: "read" }, "activity send", '"kind":"pi:tool","data":{"phase":"start","tool":"read"}'],
  ["tool_execution_end", ["isError", "toolName"], { toolName: secret, isError: false }, "activity send", '"data":{"phase":"end","tool":"other"}'],
  ["tool_execution_end", ["isError", "toolName"], { toolName: "bash", isError: true }, "activity send", '"data":{"phase":"failed","tool":"bash"}'],
  ["model_select", ["source"], { source: "cycle" }, "activity send", '"kind":"pi:model","data":{"phase":"cycle"}'],
  ["model_select", ["source"], { source: "restore" }, "activity send", '"data":{"phase":"restore"}'],
  ["model_select", ["source"], { source: "set" }, "activity send", '"data":{"phase":"set"}'],
  ["model_select", ["source"], { source: secret }, "activity send", '"data":{"phase":"other"}'],
  ["thinking_level_select", [], {}, "activity send", '"kind":"pi:thinking","data":{"phase":"changed"}'],
  ["agent_start", [], {}, "activity send", '"kind":"pi:agent","data":{"phase":"start"}'],
  ["agent_end", [], {}, "activity send", '"kind":"pi:agent","data":{"phase":"run-end"}'],
  ["turn_start", [], {}, "activity send", '"kind":"pi:step","data":{"phase":"start"}'],
  ["turn_end", [], {}, "activity send", '"kind":"pi:step","data":{"phase":"end"}'],
  ["agent_settled", [], {}, "activity mark", '"kind":"pi:turn","data":{"phase":"end","role":"assistant"}'],
  ["ui_prompt_start", [], {}, "activity send", '"kind":"pi:ui","data":{"phase":"waiting"}'],
  ["session_compact", [], {}, "activity mark", '"kind":"pi:compaction","label":"Compaction","data":{"phase":"complete"}'],
  ["session_compact_failed", [], {}, "activity send", '"kind":"pi:compaction","data":{"phase":"failed"}'],
 ];
 for (const [name, allow, values, operation, fragment] of cases) {
  const touched: string[] = []; const before = f.calls.length;
  f.emit(name, spy(allow, values, touched)); await flush();
  assert.deepEqual(touched, allow, name);
  assert.equal(f.calls.length, before + 1, name); const call = f.calls.at(-1)!;
  assert.equal(op(call), operation, name); assert.ok(call.input!.includes(fragment), `${name}: ${call.input}`);
  assert.ok(!call.input!.includes(secret), name);
 }
 assert.ok(!JSON.stringify(f.shown).includes(secret)); assert.match(f.adapter.status(), new RegExp(`durable:${cases.length} unconfirmed:0 dropped:0`));
 // Another session in the same process: nothing is read, nothing is sent.
 const count = f.calls.length; f.session(sessionB);
 for (const [name, allow, values] of cases) { const touched: string[] = []; f.emit(name, spy(allow, values, touched)); assert.deepEqual(touched, [], name); }
 await flush(); assert.equal(f.calls.length, count); f.session(sessionA);
 // A replaced owner (reload) with the original session: still nothing read or sent.
 const next = fixture({ state: f.state }); next.emit("session_start", { reason: "reload" }); await flush(); const replaced = next.calls.length;
 for (const [name, allow, values] of cases) { const touched: string[] = []; f.emit(name, spy(allow, values, touched)); assert.deepEqual(touched, [], name); }
 await flush(); assert.equal(next.calls.length, replaced); assert.equal(f.calls.length, count);
 const touched: string[] = []; next.emit("message_end", spy(messageKeys, { message: {}, "message.role": "user" }, touched)); await flush();
 assert.deepEqual(touched, messageKeys); assert.equal(op(next.calls.at(-1)!), "activity mark"); assert.equal(JSON.parse(next.calls.at(-1)!.input!).kind, "pi:prompt"); next.adapter.dispose();
});

test("marker policy: prompt, settled turn and compaction only; no marker carries a tool; prompt/turn markers are unlabelled", async () => {
 const f = fixture(); f.emit("session_start"); await f.command("bind " + binding); await flush();
 // One realistic run: a prompt, two LLM steps with a tool between them, UI and model changes, compaction, settlement.
 const events: [string, unknown][] = [
  ["agent_start", {}], ["message_start", { message: { role: "user" } }], ["message_end", { message: { role: "user" } }],
  ["turn_start", {}], ["message_start", { message: { role: "assistant" } }], ["message_end", { message: { role: "assistant" } }],
  ["tool_execution_start", { toolName: "bash" }], ["tool_execution_end", { toolName: "read", isError: true }],
  ["message_start", { message: { role: "toolResult" } }], ["message_end", { message: { role: "toolResult" } }], ["turn_end", {}],
  ["turn_start", {}], ["message_end", { message: { role: "custom" } }], ["turn_end", {}],
  ["ui_prompt_start", {}], ["ui_prompt_end", {}], ["model_select", { source: "set" }], ["thinking_level_select", {}],
  ["session_compact", {}], ["session_compact_failed", {}], ["agent_end", {}], ["agent_settled", {}],
 ];
 for (const [name, event] of events) f.emit(name, event);
 await flush();
 const sent = f.calls.filter(c => op(c) === "activity send" || op(c) === "activity mark").map(c => ({ marker: op(c) === "activity mark", envelope: JSON.parse(c.input!) }));
 assert.equal(sent.length, events.length); assert.equal(f.state.drops.dropped, 0);
 const markers = sent.filter(s => s.marker).map(s => s.envelope);
 assert.deepEqual(markers.map(e => e.kind), ["pi:prompt", "pi:compaction", "pi:turn"]);
 for (const e of markers) assert.ok(e.kind === "pi:tool" || e.data?.tool === undefined, `${e.kind} marker carries a tool`);
 const [prompt, compaction, turn] = markers;
 assert.equal("label" in prompt, false); assert.deepEqual(prompt.data, { phase: "submit", role: "user" });
 assert.equal("label" in turn, false); assert.deepEqual(turn.data, { phase: "end", role: "assistant" });
 assert.equal(compaction.label, "Compaction"); assert.deepEqual(compaction.data, { phase: "complete" });
 // Plain rows: tools keep their tool, messages/steps/agent/ui/model/thinking are never markers, and only compaction is labelled.
 const plain = sent.filter(s => !s.marker).map(s => s.envelope);
 assert.deepEqual([...new Set(plain.map(e => e.kind))].sort(), ["pi:agent", "pi:compaction", "pi:message", "pi:model", "pi:step", "pi:thinking", "pi:tool", "pi:ui"]);
 assert.deepEqual(plain.filter(e => e.kind === "pi:tool").map(e => e.data), [{ phase: "start", tool: "bash" }, { phase: "failed", tool: "read" }]);
 assert.ok(plain.every(e => e.kind === "pi:tool" || e.data.tool === undefined));
 assert.ok(plain.every(e => !("label" in e))); assert.ok(plain.every(e => e.data.phase !== "settled"));
 assert.deepEqual(plain.filter(e => e.kind === "pi:message" && e.data.phase === "end").map(e => e.data.role), ["assistant", "toolResult", "other"]);
 f.adapter.dispose();
});

test("receipt sequence: any positive safe integer is durable without retry; invalid values are rejected", async () => {
 const accepted = [1025, 8192, 100000, Number.MAX_SAFE_INTEGER]; let index = 0;
 const f = fixture({ run: async call => { const r = ack(call) as any; if (op(call) === "activity send") r.receipt.sequence = accepted[index++]; return r; } });
 f.emit("session_start"); await f.command("bind " + binding); await flush();
 for (let n = 1; n <= accepted.length; n++) {
  f.emit("agent_start"); await flush(); f.time.advance(1000); await flush();
  assert.equal(f.calls.filter(c => op(c) === "activity send").length, n);
  assert.match(f.adapter.status(), new RegExp(`metadata:durable durable:${n} unconfirmed:0 dropped:0`));
 }
 f.adapter.dispose();
 for (const invalid of [0, -1, -1025, 1.5, 1025.5, 2 ** 53, Number.MAX_SAFE_INTEGER + 1, NaN, Infinity, "1025", null, undefined, true]) {
  const b = fixture({ run: async call => { const r = ack(call) as any; if (op(call) === "activity send") r.receipt.sequence = invalid; return r; } });
  b.emit("session_start"); await b.command("bind " + binding); await flush();
  b.emit("agent_start"); await flush(); b.time.advance(1000); await flush();
  const events = b.calls.filter(c => op(c) === "activity send");
  assert.equal(events.length, 2, String(invalid)); assert.equal(events[0].input, events[1].input);
  assert.match(b.adapter.status(), /metadata:unknown durable:0 unconfirmed:1/, String(invalid)); b.adapter.dispose();
 }
});

type Internals = { queue: { call: Invocation }[]; queuedBytes: number };
const activity = (calls: Invocation[], kind: string) => calls.filter(c => op(c) === "activity send" && JSON.parse(c.input!).kind === kind);

test("volume: 10,000 events are all admitted under bounded queue/bytes with no lifetime drop", async () => {
 let sequence = 0;
 const f = fixture({ run: async call => { const r = ack(call) as any; if (op(call) === "activity send") r.receipt.sequence = ++sequence; return r; } });
 f.emit("session_start"); await f.command("bind " + binding); await flush();
 const internals = f.adapter as unknown as Internals; let maxQueue = 0; let maxBytes = 0; let sent = 0;
 while (sent < 10000) {
  // One full burst first, then the sustained rate each simulated second.
  const batch = Math.min(sent === 0 ? LIMITS.burst : LIMITS.rate, 10000 - sent);
  for (let i = 0; i < batch; i++) {
   f.emit("tool_execution_start", { toolName: "bash" }); sent++;
   maxQueue = Math.max(maxQueue, internals.queue.length); maxBytes = Math.max(maxBytes, internals.queuedBytes);
   assert.ok(internals.queue.length <= LIMITS.queue); assert.ok(internals.queuedBytes <= LIMITS.bytes);
  }
  await flush(); f.time.advance(1000); await flush();
 }
 assert.equal(f.state.admitted, 10000); assert.equal(f.state.drops.dropped, 0);
 assert.ok(maxQueue >= LIMITS.burst - 1, `burst exercised the queue (${maxQueue})`); assert.ok(maxBytes > 0);
 assert.equal(activity(f.calls, "pi:tool").length, 10000); assert.equal(activity(f.calls, "pi:adapter").length, 0);
 assert.equal(sequence, 10000);
 assert.match(f.adapter.status(), /metadata:durable durable:10000 unconfirmed:0 dropped:0 anchor/);
 f.adapter.dispose();
});

test("rate: a burst beyond the bucket drops with reason rate and recovers after refill", async () => {
 const f = fixture(); f.emit("session_start"); await f.command("bind " + binding); await flush();
 for (let i = 0; i < LIMITS.burst + 5; i++) f.emit("agent_start");
 assert.equal(f.state.admitted, LIMITS.burst); assert.equal(f.state.drops.rate, 5); assert.equal(f.state.drops.queue, 0);
 assert.match(f.adapter.status(), /dropped:5\(rate:5\) /);
 await flush(); f.time.advance(1000 / LIMITS.rate); await flush();
 f.emit("agent_start"); f.emit("agent_start"); assert.equal(f.state.admitted, LIMITS.burst + 1); assert.equal(f.state.drops.rate, 6);
 await flush(); f.time.advance(1000); await flush();
 for (let i = 0; i < LIMITS.rate; i++) f.emit("agent_start");
 assert.equal(f.state.admitted, LIMITS.burst + 1 + LIMITS.rate); assert.equal(f.state.drops.rate, 6);
 // Refill saturates at the burst depth, however long the idle gap.
 await flush(); f.time.advance(60 * 1000); await flush();
 for (let i = 0; i < LIMITS.burst + 1; i++) f.emit("agent_start");
 assert.equal(f.state.admitted, 2 * LIMITS.burst + 1 + LIMITS.rate); assert.equal(f.state.drops.rate, 7);
 await flush(); assert.equal(activity(f.calls, "pi:agent").length, f.state.admitted); f.adapter.dispose();
});

test("drop report: exact integer-only pi:adapter envelope, coalesced, one outstanding, never a drop itself", async () => {
 const held: { call: Invocation; resolve: (value: unknown) => void }[] = [];
 let register!: () => void;
 const f = fixture({ run: call => op(call) === "activity send" ? new Promise(resolve => { held.push({ call, resolve }); }) : op(call) === "producer register" ? new Promise(resolve => { register = () => resolve(ack(call)); }) : Promise.resolve(ack(call)) });
 f.emit("session_start"); await f.command("bind " + binding);
 // Registration pending: three drops, then a tick while unregistered sends nothing.
 f.emit("agent_start"); f.emit("agent_start"); f.emit("agent_start"); assert.equal(f.state.drops.registration, 3);
 f.time.advance(1000); await flush(); assert.equal(held.length, 0);
 register(); await flush(); assert.equal(held.length, 0, "the report waits for the maintenance tick");
 f.time.advance(1000); await flush();
 assert.equal(held.length, 1); const first = JSON.parse(held[0].call.input!);
 assert.equal(op(held[0].call), "activity send"); assert.deepEqual(held[0].call.args.slice(-2), ["--generation", f.state.ticket!.generation]);
 assert.deepEqual(Object.keys(first), ["id", "producer", "agentInstance", "conversation", "parent", "location", "source", "kind", "data"]);
 assert.deepEqual({ ...first, id: "x" }, { id: "x", producer: f.state.binding!.producer, agentInstance: f.state.instance, conversation: sessionA, parent: null, location: null, source: "pi:agent-reel", kind: "pi:adapter", data: { phase: "dropped", dropped: 3, queue: 0, bytes: 0, size: 0, rate: 0, registration: 3, invalidated: 0, receiver: 0 } });
 assert.deepEqual(Object.keys(first.data), ["phase", "dropped", "queue", "bytes", "size", "rate", "registration", "invalidated", "receiver"]);
 assert.ok(Object.entries(first.data).every(([k, v]) => k === "phase" || Number.isSafeInteger(v)));
 assert.ok(Buffer.byteLength(held[0].call.input!) <= LIMITS.eventBytes);
 // Outstanding report in flight: more drops across ticks coalesce, no second report.
 for (let i = 0; i < LIMITS.queue + 2; i++) f.emit("agent_start"); // the report holds the flight: 32 queued, then two queue drops
 f.adapter.emit("test", { phase: "x".repeat(LIMITS.eventBytes) });
 f.time.advance(500); f.emit("agent_start"); f.emit("agent_start"); // tokens remain, the queue is still full
 assert.equal((f.adapter as unknown as Internals).queue.length, LIMITS.queue);
 f.time.advance(2000); await flush(); assert.equal(held.length, 1, "one outstanding report");
 assert.deepEqual({ ...f.state.drops }, { dropped: 8, reported: 0, queue: 4, bytes: 0, size: 1, rate: 0, registration: 3, invalidated: 0, receiver: 0 });
 // Settling the report re-arms it, but a full queue leaves the next one pending (not dropped).
 held[0].resolve(ack(held[0].call)); await flush(); assert.equal(f.state.drops.reported, 3); assert.equal(held.length, 2);
 f.emit("agent_start"); assert.equal((f.adapter as unknown as Internals).queue.length, LIMITS.queue);
 f.time.advance(1000); await flush(); assert.equal(f.state.drops.dropped, 8, "a report that cannot be queued is not a drop");
 assert.equal(activity(f.calls, "pi:adapter").length, 1);
 // Draining one slot lets the next tick queue exactly one coalesced cumulative report.
 held[1].resolve(ack(held[1].call)); await flush(); f.time.advance(1000); await flush();
 while (held.length < 40 && activity(held.map(h => h.call), "pi:adapter").length < 2) { held.at(-1)!.resolve(ack(held.at(-1)!.call)); await flush(); }
 const reports = activity(f.calls, "pi:adapter"); assert.equal(reports.length, 2);
 assert.deepEqual(JSON.parse(reports[1].input!).data, { phase: "dropped", dropped: 8, queue: 4, bytes: 0, size: 1, rate: 0, registration: 3, invalidated: 0, receiver: 0 });
 held.at(-1)!.resolve(ack(held.at(-1)!.call)); await flush(); assert.equal(f.state.drops.reported, 8);
 const count = f.calls.length; f.time.advance(3000); await flush(); assert.equal(activity(f.calls.slice(count), "pi:adapter").length, 0, "nothing new to report");
 assert.ok(!JSON.stringify(f.calls).includes(secret)); f.adapter.dispose();
});

test("invalidation: session_tree, rebind and reload discards count as invalidated, never the drop report", async () => {
 const hang = () => new Promise<unknown>(() => {});
 const f = fixture({ run: call => op(call) === "activity send" ? hang() : Promise.resolve(ack(call)) });
 f.emit("session_start"); await f.command("bind " + binding); await flush();
 f.emit("agent_start"); f.emit("agent_start"); f.emit("agent_start"); // one in flight, two queued
 f.emit("session_tree"); assert.equal(f.state.drops.invalidated, 3); assert.match(f.adapter.status(), /dropped:3\(invalidated:3\) /);
 f.adapter.dispose();
 // A pending drop report discarded by invalidation is not itself counted.
 let register!: () => void;
 const g = fixture({ run: call => op(call) === "activity send" ? hang() : op(call) === "producer register" ? new Promise(resolve => { register = () => resolve(ack(call)); }) : Promise.resolve(ack(call)) });
 g.emit("session_start"); await g.command("bind " + binding); g.emit("agent_start"); register(); await flush(); // registration drop
 g.time.advance(1000); await flush(); assert.equal(activity(g.calls, "pi:adapter").length, 1);
 // Rebinding starts a fresh per-binding drop account: the old registration drop is not carried over.
 await g.command("bind " + binding); await flush(); assert.equal(g.state.drops.invalidated, 0); assert.equal(g.state.drops.dropped, 0); assert.equal(g.state.drops.registration, 0);
 g.adapter.dispose(); assert.equal(g.state.drops.invalidated, 0);
 // Reload: the replaced adapter's discards are counted in process state and shown by its successor.
 const h = fixture({ run: call => op(call) === "activity send" ? hang() : Promise.resolve(ack(call)) });
 h.emit("session_start"); await h.command("bind " + binding); await flush();
 h.emit("agent_start"); h.emit("agent_start"); h.emit("agent_start");
 const reload = fixture({ state: h.state }); reload.emit("session_start", { reason: "reload" });
 assert.equal(h.state.drops.invalidated, 3); assert.match(reload.adapter.status(), /dropped:3\(invalidated:3\) /);
 reload.adapter.dispose(); h.adapter.dispose();
});

const recordB = "b".repeat(32);
const bindingB = JSON.stringify(["/private/areel", "/private/library", recordB]);

test("drops are per binding: recording B's first report carries only B's drops under B's descriptor", async () => {
 const f = fixture(); f.emit("session_start"); await f.command("bind " + binding); await flush();
 for (let i = 0; i < LIMITS.burst + 3; i++) f.emit("agent_start"); // three rate drops, the rest queued
 assert.equal(f.state.drops.rate, 3); assert.equal(activity(f.calls, "pi:adapter").length, 0, "no tick yet");
 const producerA = f.state.binding!.producer;
 await f.command("bind " + bindingB); // queued A events are invalidated, then the account resets
 assert.deepEqual({ ...f.state.drops }, { dropped: 0, reported: 0, queue: 0, bytes: 0, size: 0, rate: 0, registration: 0, invalidated: 0, receiver: 0 });
 await flush(); assert.equal(f.state.ticket!.descriptor.producer, f.state.binding!.producer); assert.notEqual(f.state.binding!.producer, producerA);
 f.adapter.emit("test", { phase: "x".repeat(LIMITS.eventBytes) }); assert.equal(f.state.drops.size, 1);
 f.time.advance(1000); await flush();
 const reports = activity(f.calls, "pi:adapter"); assert.equal(reports.length, 1);
 assert.ok(reports[0].args.includes(recordB));
 const body = JSON.parse(reports[0].input!);
 assert.equal(body.producer, f.state.binding!.producer);
 assert.deepEqual(body.data, { phase: "dropped", dropped: 1, queue: 0, bytes: 0, size: 1, rate: 0, registration: 0, invalidated: 0, receiver: 0 });
 assert.equal(f.state.drops.reported, 1); f.adapter.dispose();
});

test("drop report: a report whose every attempt failed stays due and is re-queued", async () => {
 for (const mode of ["reject", "error-reply", "timeout"] as const) {
  let failures = 0;
  const f = fixture({ run: (call, signal) => {
   if (op(call) !== "activity send" || JSON.parse(call.input!).kind !== "pi:adapter" || failures >= 2) return Promise.resolve(ack(call));
   failures++;
   if (mode === "reject") return Promise.reject(new Error(secret));
   if (mode === "error-reply") return Promise.resolve({ accepted: false, durable: false, failed: false, error: "activity sink unavailable" });
   return new Promise((_resolve, reject) => { signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true }); });
  } });
  f.emit("session_start"); await f.command("bind " + binding); await flush();
  for (let i = 0; i < LIMITS.burst + 2; i++) f.emit("agent_start");
  await flush(); assert.equal(f.state.drops.rate, 2, mode);
  for (let i = 0; i < 30 && failures < 2; i++) { f.time.advance(1000); await flush(); }
  const failed = activity(f.calls, "pi:adapter"); assert.equal(failed.length, 2, mode); assert.equal(failed[0].input, failed[1].input, mode);
  assert.equal(f.state.drops.reported, 0, `${mode}: a failed report is not reported`);
  for (let i = 0; i < 30 && f.state.drops.reported === 0; i++) { f.time.advance(1000); await flush(); }
  const reports = activity(f.calls, "pi:adapter"); assert.equal(reports.length, 3, mode);
  assert.notEqual(JSON.parse(reports[2].input!).id, JSON.parse(reports[0].input!).id, mode);
  assert.deepEqual(JSON.parse(reports[2].input!).data, JSON.parse(reports[0].input!).data, mode);
  assert.equal(f.state.drops.reported, 2, mode); assert.equal(f.state.drops.dropped, 2, `${mode}: a failed report is never itself a drop`);
  f.time.advance(3000); await flush(); assert.equal(activity(f.calls, "pi:adapter").length, 3, mode); f.adapter.dispose();
 }
});

test("drop report: a receiver-accepted report (pending-unconfirmed) is reported and not resent", async () => {
 for (const second of ["pending", "reject"] as const) {
  let attempts = 0;
  const f = fixture({ run: call => {
   if (op(call) !== "activity send" || JSON.parse(call.input!).kind !== "pi:adapter") return Promise.resolve(ack(call));
   attempts++;
   return attempts === 2 && second === "reject" ? Promise.reject(new Error("unavailable")) : Promise.resolve(ack(call, undefined, false));
  } });
  f.emit("session_start"); await f.command("bind " + binding); await flush();
  for (let i = 0; i < LIMITS.burst + 2; i++) f.emit("agent_start");
  await flush();
  for (let i = 0; i < 30 && attempts < 2; i++) { f.time.advance(1000); await flush(); }
  assert.equal(attempts, 2, second); assert.equal(f.state.drops.reported, 2, second);
  f.time.advance(5000); await flush(); assert.equal(activity(f.calls, "pi:adapter").length, 2, `${second}: not re-queued`);
  f.adapter.dispose();
 }
});

test("legacy receiver cap: latch stops activity, counts receiver drops, keeps registration/control, clears on bind", async () => {
 const cap = { accepted: false, durable: false, failed: false, accounting: {}, error: "activity record/dedup limit reached" };
 let full = true; let hold!: (value: unknown) => void; let held = false;
 const f = fixture({ run: call => {
  if (op(call) !== "activity send" && op(call) !== "activity mark") return Promise.resolve(ack(call));
  if (!full) return Promise.resolve(ack(call));
  if (!held) { held = true; return new Promise(resolve => { hold = resolve; }); }
  return Promise.resolve(cap);
 } });
 f.emit("session_start"); await f.command("bind " + binding); await flush();
 f.emit("agent_start"); f.emit("turn_start"); f.emit("turn_end"); f.emit("agent_end"); // one in flight, three queued
 assert.equal(activity(f.calls, "pi:agent").length, 1);
 hold(cap); await flush();
 assert.equal(f.state.receiverFull, true); assert.equal(f.state.drops.receiver, 3, "queued events become receiver drops");
 assert.equal((f.adapter as unknown as Internals).queue.length, 0);
 f.time.advance(1000); await flush(); // the capped event's normal retry
 const sent = () => f.calls.filter(c => op(c) === "activity send" || op(c) === "activity mark").length;
 assert.equal(sent(), LIMITS.attempts, "the first capped event makes exactly the normal attempts");
 assert.equal(f.state.drops.receiver, 4); assert.equal(f.state.drops.dropped, 4);
 assert.match(f.adapter.status(), /metadata:receiver-full durable:0 unconfirmed:0 dropped:4\(receiver:4\) /);
 const before = f.calls.length;
 for (let i = 0; i < 5; i++) f.emit("agent_start");
 f.emit("message_end", { message: { role: "user" } });
 assert.equal(f.calls.length, before, "later emits spawn nothing"); assert.equal(f.state.drops.receiver, 10);
 f.time.advance(5000); await flush(); assert.equal(sent(), LIMITS.attempts, "no drop report is sent while latched");
 // Registration and control keep working.
 for (let t = 0; t < LIMITS.refresh; t += 1000) { f.time.advance(1000); await flush(); }
 assert.equal(f.calls.filter(c => op(c) === "producer refresh").length, 1); assert.equal(sent(), LIMITS.attempts); assert.match(f.adapter.status(), /registration:registered/);
 await f.command("status"); await flush(); assert.equal(op(f.calls.at(-1)!), "status");
 // Reload keeps the per-binding latch.
 const reload = fixture({ state: f.state, run: async call => ack(call) }); reload.emit("session_start", { reason: "reload" }); await flush();
 reload.emit("agent_start"); await flush(); assert.equal(reload.calls.filter(c => op(c) === "activity send").length, 0); assert.equal(reload.state.drops.receiver, 11);
 // A new binding clears the latch and the drop account.
 full = false; await reload.command("bind " + bindingB); await flush();
 assert.equal(reload.state.receiverFull, false); assert.equal(reload.state.drops.dropped, 0);
 reload.emit("agent_start"); await flush(); assert.equal(reload.calls.filter(c => op(c) === "activity send").length, 1);
 assert.match(reload.adapter.status(), /durable:1 unconfirmed:0 dropped:0 /); reload.adapter.dispose(); f.adapter.dispose();
});

test("/reel mark sends one labelled navigation mark through the bound CLI and never echoes the label", async () => {
 const f = fixture(); f.emit("session_start");
 await f.command("mark unbound " + secret); await flush();
 assert.equal(f.calls.length, 0); assert.ok(f.shown.some(x => typeof x === "string" && /no bound recording/.test(x))); assert.ok(!JSON.stringify(f.shown).includes(secret));
 await f.command("bind " + binding); await flush();
 await f.command("mark  plan\tapproved\u0007 " + secret); await flush();
 const marks = f.calls.filter(c => op(c) === "activity mark");
 assert.equal(marks.length, 1);
 const call = marks[0];
 assert.deepEqual(call.args.slice(0, 6), ["activity", "mark", "--library", "/private/library", "--recording", record]);
 assert.equal(call.args[6], "--generation"); assert.ok(!call.discovery);
 const envelope = JSON.parse(call.input!);
 assert.equal(envelope.kind, "pi:mark"); assert.equal(envelope.source, "pi:agent-reel"); assert.equal(envelope.label, "plan approved " + secret);
 assert.equal("data" in envelope, false); assert.equal(envelope.conversation, sessionA);
 assert.ok(f.shown.some(x => typeof x === "string" && /mark queued/.test(x))); assert.ok(!JSON.stringify(f.shown).includes(secret));
 // No label: the mark still goes out, with no label key.
 await f.command("mark"); await flush();
 const bare = JSON.parse(f.calls.filter(c => op(c) === "activity mark").at(-1)!.input!);
 assert.equal("label" in bare, false); assert.equal(bare.kind, "pi:mark");
 f.adapter.dispose();
});

test("mark labels are bounded, control-free and well-formed", () => {
 assert.equal(markLabel("  a\n\tb\u0000c\u007f  "), "a b c");
 assert.equal(markLabel(""), "");
 assert.equal(markLabel("x\ud800y"), "x\ufffdy");
 const long = markLabel("é".repeat(300));
 assert.equal(Buffer.byteLength(long), MARK_LABEL_BYTES); assert.equal(long, "é".repeat(128));
 assert.equal(markLabel("a".repeat(255) + "é"), "a".repeat(255));
 assert.equal(markLabel("a".repeat(255) + " b"), "a".repeat(255));
});
