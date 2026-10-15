import assert from "node:assert/strict";
import { test } from "node:test";
import { Adapter, LIMITS, newProcessState, type Clock, type ProcessState } from "../extensions/adapter.ts";
import type { Invocation, Runner } from "../extensions/runner.ts";
// CLI operation words: two-word integration groups, one-word lifecycle commands.
const op = (call: { args: string[] }) => ["activity", "producer", "service"].includes(call.args[0]) ? `${call.args[0]} ${call.args[1]}` : call.args[0];

class Time implements Clock {
 value = 0; next = 0; jobs = new Map<number, { at: number; fn: () => void }>();
 now() { return this.value; }
 later(fn: () => void, ms: number) { const id = ++this.next; this.jobs.set(id, { at: this.value + ms, fn }); return () => { this.jobs.delete(id); }; }
 advance(ms: number) {
  const end = this.value + ms;
  for (let count = 0; ; count++) {
   assert.ok(count < 1000, "bounded fake timer work");
   const next = [...this.jobs].filter(([, job]) => job.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
   if (!next) break;
   this.value = next[1].at; this.jobs.delete(next[0]); next[1].fn();
  }
  this.value = end;
 }
}
const flush = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
function receipt(call: Invocation): unknown {
 const body = JSON.parse(call.input!);
 return { accepted: true, durable: true, failed: false, receipt: { id: body.id, sequence: 1 }, registeredContext: { generation: call.args.at(-1), descriptor: { producer: body.producer, agentInstance: body.agentInstance, conversation: body.conversation, parent: null, location: null } } };
}
function registration(call: Invocation, generation = "1".repeat(32)): unknown {
 return { accepted: false, durable: false, failed: false, registration: { generation, descriptor: JSON.parse(call.input!).descriptor, expiresAt: "2000-01-01T00:00:00Z", durability: "volatile-holder-lifetime" } };
}
function fixture(state: ProcessState = newProcessState(), time = new Time()) {
 const calls: Invocation[] = []; const signals: AbortSignal[] = [];
 const held: { call: Invocation; resolve: (value: unknown) => void }[] = [];
 const runner: Runner = (call, signal) => {
  calls.push(call); signals.push(signal);
  if (op(call) === "producer register") return Promise.resolve(registration(call));
  if (op(call) === "status") return Promise.resolve({ id: "a".repeat(32), status: "recording" });
  return new Promise(resolve => { held.push({ call, resolve }); });
 };
 const adapter = new Adapter(state, runner, time, () => {});
 const queue = () => (adapter as unknown as { queue: { call: Invocation }[]; queuedBytes: number });
 return { state, time, calls, signals, held, adapter, queue };
}
async function bound() {
 const f = fixture(); f.adapter.start(null);
 f.adapter.bind({ binary: "/fake/areel", library: "/fake/library", recording: "a".repeat(32), producer: "freshness-test" });
 await flush(); assert.match(f.adapter.status(), /registration:registered /);
 f.adapter.emit("agent", { phase: "start" }); return f;
}

for (const at of [LIMITS.refresh - 1, LIMITS.refresh, 6 * LIMITS.refresh]) {
 test(`registration freshness with retained aborted metadata flight at ${at}ms`, async () => {
  const f = await bound();
  try {
   const flight = f.state.flight; f.time.advance(at);
   assert.equal(f.state.flight, flight); assert.ok(flight?.abort.signal.aborted);
   assert.deepEqual(f.calls.map(c => op(c)), ["producer register", "activity send"]);
   assert.match(f.adapter.status(), at < LIMITS.refresh ? /registration:registered / : /registration:unknown-refresh-overdue /);
   assert.equal(f.state.uncertain, false); // No invented expiry/rebind scheduling change.
  } finally { f.adapter.dispose(); }
 });
}

test("overdue emits drop/count without queue, budget or dispatch changes; operator lane remains independent", async () => {
 const f = await bound();
 try {
  f.time.advance(LIMITS.refresh - 1);
  f.adapter.emit("turn", { phase: "before-deadline" });
  assert.equal(f.state.admitted, 2); assert.equal(f.queue().queue.length, 1);
  const queued = f.queue().queue[0]; const original = JSON.stringify(queued); const bytes = f.queue().queuedBytes;
  const flight = f.state.flight;
  f.time.advance(1); f.adapter.emit("turn", { phase: "at-deadline" });
  assert.match(f.adapter.status(), /dropped:1\(registration:1\) /, "overdue event must count as dropped");
  f.time.advance(5 * LIMITS.refresh); f.adapter.emit("turn", { phase: "after-five-minutes" });
  assert.match(f.adapter.status(), /dropped:2\(registration:2\) /);
  assert.equal(f.state.admitted, 2, "overdue events must not consume admitted budget");
  assert.equal(f.queue().queue.length, 1); assert.equal(f.queue().queue[0], queued);
  assert.equal(JSON.stringify(queued), original); assert.equal(f.queue().queuedBytes, bytes);
  assert.equal(f.state.flight, flight); assert.ok(flight?.abort.signal.aborted);
  assert.equal(f.calls.length, 2, "no second metadata child or auto-rebind");
  assert.equal(f.adapter.control("status"), true); await flush();
  assert.equal(op(f.calls[2]), "status"); assert.match(f.adapter.status(), /visual:reported-recording registration:unknown-refresh-overdue /);
  assert.equal(f.state.flight, flight); assert.equal(f.state.controlFlight, undefined);
 } finally { f.adapter.dispose(); }
});

test("matching refresh restores freshness after actual settlement, preserving retry and queued contexts", async () => {
 const f = await bound();
 try {
  f.time.advance(LIMITS.refresh - 1); f.adapter.emit("turn", { phase: "queued-before-deadline" });
  const original = f.held[0]; const queued = JSON.stringify(f.queue().queue[0].call);
  f.time.advance(1); assert.match(f.adapter.status(), /registration:unknown-refresh-overdue /);
  original.resolve(receipt(original.call)); await flush(); // Timed-out success is not credited.
  assert.match(f.adapter.status(), /durable:0 /);
  f.time.advance(1000); assert.equal(f.held.length, 2);
  assert.deepEqual(f.held[1].call, original.call, "immutable same-ID/generation retry");
  f.held[1].resolve(receipt(f.held[1].call)); await flush();
  const refresh = f.held[2]; assert.equal(op(refresh.call), "producer refresh");
  assert.equal(JSON.parse(refresh.call.input!).generation, "1".repeat(32));
  assert.equal(refresh.call.args.includes("--generation"), false);
  assert.match(f.adapter.status(), /registration:pending /);
  const budget = f.state.admitted; f.adapter.emit("agent", { phase: "pending-refresh" }); assert.equal(f.state.admitted, budget);
  refresh.resolve(registration(refresh.call)); await flush();
  assert.match(f.adapter.status(), /registration:registered /);
  assert.equal(JSON.stringify(f.held[3].call), queued, "queued context not reassigned during refresh");
  f.held[3].resolve(receipt(f.held[3].call)); await flush();
  assert.match(f.adapter.status(), /durable:2 /);
  f.adapter.emit("agent", { phase: "fresh-after-refresh" }); assert.equal(f.state.admitted, budget + 1);
  assert.deepEqual(f.calls.map(c => op(c)), ["producer register", "activity send", "activity send", "producer refresh", "activity send", "activity send"]);
 } finally { f.adapter.dispose(); }
});

test("late old completion cannot freshen or alter the successor registration", async () => {
 const old = await bound(); old.time.advance(6 * LIMITS.refresh);
 const next = fixture(old.state, old.time); next.adapter.start("bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb");
 try {
  assert.equal(next.calls.length, 0); assert.match(next.adapter.status(), /registration:pending /);
  old.held[0].resolve(receipt(old.held[0].call)); await flush();
  assert.equal(next.calls.length, 1); assert.equal(op(next.calls[0]), "producer replace");
  assert.match(next.adapter.status(), /registration:pending .*durable:0 /);
  assert.equal(next.state.ticket!.descriptor.conversation, null);
  next.held[0].resolve(registration(next.held[0].call, "2".repeat(32))); await flush();
  assert.match(next.adapter.status(), /registration:registered .*durable:0 /);
  assert.equal(next.state.ticket!.generation, "2".repeat(32));
  assert.equal(next.state.ticket!.descriptor.conversation, next.adapter.conversation);
  old.adapter.dispose(); assert.equal(next.state.ticket!.generation, "2".repeat(32));
 } finally { next.adapter.dispose(); old.adapter.dispose(); }
});
