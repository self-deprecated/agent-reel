import { randomUUID } from "node:crypto";
import { isAbsolute } from "node:path";
import type { Invocation, Runner } from "./runner.ts";

export const START_TIMEOUT = 35000;
// Unbound start: the binary is a bare command name resolved from PATH (default
// "areel") or an absolute path; the library is passed only when explicitly
// configured in Pi. Otherwise the CLI resolves it from its own settings (flag >
// env > user config file > default) and the adapter binds to the library the
// CLI reports in its `record --json` output.
// autoBind enables proof-based automatic binding (see Adapter.probe): the same
// binary/library selection asks `areel activity target --json` whether a live
// holder records the terminal Pi runs in. Off in the bare seam, on in the
// real extension entrypoint (defaultStartConfig).
export interface StartConfig { binary: string; library?: string; autoBind?: true; }
export function startConfig(config: Partial<StartConfig> = {}): StartConfig {
 const binary = config.binary ?? "areel";
 if (!(validPath(binary) || /^[A-Za-z0-9._-]{1,64}$/.test(binary)) || /^\.\.?$/.test(binary) || (config.library !== undefined && !validPath(config.library)) || (config.autoBind !== undefined && config.autoBind !== true)) throw new Error("invalid-start-config");
 return { binary, ...(config.library === undefined ? {} : { library: config.library }), ...(config.autoBind ? { autoBind: true as const } : {}) };
}
export function defaultStartConfig(): StartConfig { return startConfig({ autoBind: true }); }
const validPath = (x: string) => isAbsolute(x) && Buffer.byteLength(x) <= 4096 && !/[\x00-\x1f\x7f]/.test(x);
// Sender work is bounded by queue/bytes/in-flight and a token bucket (rate events
// per second sustained, burst deep), never by a lifetime count: the receiver owns
// retention. Everything refused locally is dropped and counted by reason.
export const LIMITS = { queue: 32, bytes: 16384, eventBytes: 2048, rate: 8, burst: 32, attempts: 2, timeout: 5000, retry: 250, refresh: 60000, visualAge: 15000, autoInterval: 20000 } as const;
export interface Clock { now(): number; later(fn: () => void, ms: number): () => void; }
export const realClock: Clock = { now: () => performance.now(), later(fn, ms) { const timer = setTimeout(fn, ms); timer.unref(); return () => clearTimeout(timer); } };
// auto marks a binding selected by the proof query rather than an explicit
// /reel bind or /reel start; only auto bindings are replaced or dropped by it.
export interface Binding { binary: string; library: string; recording: string; producer: string; auto?: true; }
export interface Descriptor { producer: string; agentInstance: string; conversation: string | null; parent: null; location: null; }
interface Ticket { generation: string; descriptor: Descriptor; }
// report marks the coalesced pi:adapter drop report: the drop total it carries.
// replied: some attempt got a well-formed receiver reply (accepted, not yet durable).
interface Task { call: Invocation; kind: "registration" | "activity" | "control"; attempts: number; descriptor?: Descriptor; eventID?: string; generation?: string; report?: number; replied?: boolean; }
export const DROP_REASONS = ["queue", "bytes", "size", "rate", "registration", "invalidated", "receiver"] as const;
export type DropReason = typeof DROP_REASONS[number];
// Saturating per-binding drop counters (reset by bind, retained across reload so
// invalidation drops of a replaced adapter are reported by its successor);
// reported is the total carried by the last receiver-accepted drop report.
export type Drops = { dropped: number; reported: number } & Record<DropReason, number>;
export interface ProcessState {
 // admitted is a saturating per-binding count of admitted SDK events; never a gate.
 // receiverFull latches (per binding) once the receiver refuses activity at its record cap.
 // autoOptOut: /reel unbind suppresses auto-binding for this Pi process until /reel bind or /reel start.
 instance: string; binding?: Binding; ticket?: Ticket; uncertain: boolean; admitted: number; drops: Drops; receiverFull: boolean; autoOptOut: boolean;
 flight?: { abort: AbortController; binding: Binding; kind: Task["kind"] };
 controlFlight?: AbortController;
 // One auto-bind proof query child per process; the slot is kept until actual close.
 probeFlight?: AbortController;
 active?: Adapter;
}
export function newProcessState(): ProcessState { return { instance: randomUUID(), uncertain: false, admitted: 0, receiverFull: false, autoOptOut: false, drops: { dropped: 0, reported: 0, queue: 0, bytes: 0, size: 0, rate: 0, registration: 0, invalidated: 0, receiver: 0 } }; }
// CLI command words per operation: two-word integration commands, one-word controls.
export const OPERATIONS = {
 register: ["producer", "register"], refresh: ["producer", "refresh"], replace: ["producer", "replace"],
 activity: ["activity", "send"], marker: ["activity", "mark"],
} as const;
type Operation = keyof typeof OPERATIONS;
const isOperation = (call: Invocation, operation: Operation) => call.args[0] === OPERATIONS[operation][0] && call.args[1] === OPERATIONS[operation][1];
// Refusal text of legacy-format libraries once a holder's record/dedup table is
// full. Only matched to stop sending; never forwarded or shown.
export const RECEIVER_FULL = "activity record/dedup limit reached";
// Proof-query reasons that do not show the covering recording ended: an
// auto binding survives them (the next probe decides).
const PROBE_INCONCLUSIVE = new Set(["holder-unknown"]);
export function conversationID(value: unknown): string | null {
 return typeof value === "string" && /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(value) ? value : null;
}
export function parseBinding(text: string): Binding {
 if (text.length > 8192 || Buffer.byteLength(text) > 8192) throw new Error("invalid-binding");
 const args: unknown = JSON.parse(text);
 if (!Array.isArray(args) || args.length !== 3 || !args.every(x => typeof x === "string")) throw new Error("invalid-binding");
 const [binary, library, recording] = args as string[];
 if (![binary, library].every(validPath) || !/^[a-f0-9]{32}$/.test(recording)) throw new Error("invalid-binding");
 return { binary, library, recording, producer: randomUUID() };
}
function object(v: unknown): Record<string, unknown> {
 if (!v || typeof v !== "object" || Array.isArray(v)) throw new Error("invalid-response");
 return v as Record<string, unknown>;
}
function sameDescriptor(v: unknown, d: Descriptor): boolean {
 const r = object(v);
 return r.producer === d.producer && r.agentInstance === d.agentInstance && r.conversation === d.conversation && r.parent === null && r.location === null;
}
// A /reel mark label: control characters and whitespace runs become one space,
// trimmed, cut to the receiver's 256-byte label bound on a character boundary.
export const MARK_LABEL_BYTES = 256;
export function markLabel(text: string): string {
 const clean = text.replace(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/g, "\uFFFD").replace(/[\p{Cc}\s]+/gu, " ").trim();
 let out = ""; let bytes = 0;
 for (const ch of clean) { const n = Buffer.byteLength(ch); if (bytes + n > MARK_LABEL_BYTES) break; out += ch; bytes += n; }
 return out.trimEnd();
}
const increment = (n: number) => Math.min(n + 1, Number.MAX_SAFE_INTEGER);
const add = (n: number, k: number) => Math.min(n + k, Number.MAX_SAFE_INTEGER);

// The persistent status slot is visible in the terminal and therefore inside the
// recording itself, so it carries only this indicator, never the diagnostics.
export const RECORDING_INDICATOR = "🔴";
export class Adapter {
 private state: ProcessState; private runner: Runner; private clock: Clock; private paint: (text: string | undefined) => void;
 private epoch = {}; private alive = false; conversation: string | null = null;
 private queue: Task[] = []; private queuedBytes = 0; private pending?: Task; private retryAt = 0;
 private stopTick?: () => void; private stopDeadline?: () => void; private stopControlDeadline?: () => void;
 private registration = "unbound"; private delivery = "idle"; private visual = "unknown"; private visualUntil = 0; private refreshAt = 0;
 private durable = 0; private unconfirmed = 0;
 // Token bucket refilled from the injected clock; the drop report bypasses it.
 private tokens: number = LIMITS.burst; private refilled: number; private reporting = false;
 private startOutcome = ""; private notice: (text: string) => void; private startWith: StartConfig;
 // Auto-bind: periodic proof timer, a generation that binding changes
 // invalidate, and the last probe outcome (a reason code, shown on demand only).
 private stopProbe?: () => void; private probeGeneration = 0; private probeOutcome = "";
 constructor(state: ProcessState, runner: Runner, clock: Clock, paint: (text: string | undefined) => void, notice: (text: string) => void = () => {}, start: StartConfig = startConfig()) {
  this.startWith = startConfig(start);
  state.active?.dispose(); this.state = state; this.runner = runner; this.clock = clock; this.paint = paint; this.notice = notice; state.active = this;
  this.refilled = clock.now();
 }
 private current(epoch = this.epoch): boolean { return this.alive && this.state.active === this && epoch === this.epoch; }
 // Fresh visual health only; expired or unknown knowledge reads as unknown.
 private visualNow(): string { return this.clock.now() < this.visualUntil ? this.visual : "unknown"; }
 // On-demand diagnostics for an explicit /reel status notification; never painted persistently.
 status(): string {
  const registration = this.registration === "registered" && this.clock.now() >= this.refreshAt ? "unknown-refresh-overdue" : this.registration;
  return `reel visual:${this.visualNow()} registration:${registration} metadata:${this.delivery} durable:${this.durable} unconfirmed:${this.unconfirmed} dropped:${this.droppedText()} anchor:unknown${this.startOutcome ? ` start:${this.startOutcome}` : ""} ${this.bindingText()}`;
 }
 // binding: none | auto | explicit; auto: off (not enabled) | on | opted-out, plus the last proof outcome.
 private bindingText(): string {
  const b = this.state.binding; const auto = !this.startWith.autoBind ? "off" : this.state.autoOptOut ? "opted-out" : "on";
  return `binding:${b ? b.auto ? "auto" : "explicit" : "none"} auto:${auto}${this.startWith.autoBind && this.probeOutcome ? ` probe:${this.probeOutcome}` : ""}`;
 }
 private droppedText(): string {
  const d = this.state.drops; const reasons = DROP_REASONS.filter(r => d[r] > 0).map(r => `${r}:${d[r]}`);
  return `${d.dropped}${reasons.length ? `(${reasons.join(",")})` : ""}`;
 }
 private drop(reason: DropReason, count = 1): void {
  if (count <= 0) return;
  const d = this.state.drops; d.dropped = add(d.dropped, count); d[reason] = add(d[reason], count);
 }
 // Persistent status slot content: the indicator while reported recording is fresh, otherwise cleared.
 indicator(): string | undefined { return this.visualNow() === "reported-recording" ? RECORDING_INDICATOR : undefined; }
 private show(): void { if (this.current()) { try { this.paint(this.indicator()); } catch { /* Stale UI never changes delivery or capture. */ } } }
 private report(): void { try { this.notice(this.status()); } catch { /* UI is not capture authority. */ } }
 private invalidate(): void {
  this.startOutcome = "";
  this.epoch = {}; this.stopTick?.(); this.stopTick = undefined; this.stopDeadline?.(); this.stopDeadline = undefined;
  this.stopControlDeadline?.(); this.stopControlDeadline = undefined;
  // Discarded SDK events count as invalidated; a registration or the drop report is not an event.
  const event = (t?: Task) => t?.kind === "activity" && t.report === undefined;
  this.drop("invalidated", this.queue.filter(t => event(t)).length + (event(this.pending) ? 1 : 0));
  this.reporting = false; this.queue = []; this.queuedBytes = 0; this.pending = undefined; this.visualUntil = 0;
  // Late shutdown from a replaced SDK closure must not cancel its successor.
  if (this.state.active !== this) return;
  this.state.controlFlight?.abort();
  const flight = this.state.flight;
  if (flight) {
   if (flight.kind === "registration" && flight.binding === this.state.binding) this.state.uncertain = true;
   flight.abort.abort(); // Keep the process-wide slot until actual runner settlement.
  }
 }
 start(conversation: string | null): void {
  if (this.state.active !== this) return;
  this.invalidate(); this.alive = true; this.conversation = conversation; this.delivery = "idle";
  if (this.state.binding) this.register(); else this.registration = "unbound";
  this.tick(); this.scheduleProbe(0);
 }
 dispose(): void { this.invalidate(); this.alive = false; this.stopProbe?.(); this.stopProbe = undefined; }
 bind(binding: Binding): void {
  if (!this.current()) return;
  this.invalidate(); this.probeGeneration++; this.state.binding = binding; this.state.ticket = undefined; this.state.uncertain = false;
  // Drops and the receiver-full latch are per binding: a new recording never inherits them.
  this.state.drops = newProcessState().drops; this.state.receiverFull = false;
  this.state.admitted = 0; this.durable = 0; this.unconfirmed = 0; this.delivery = "idle";
  this.register(); this.tick();
 }
 unbind(): void {
  if (!this.current()) return;
  this.invalidate(); this.probeGeneration++; this.state.binding = undefined; this.state.ticket = undefined; this.state.uncertain = false;
  this.registration = "unbound"; this.delivery = "idle"; this.show();
 }
 private descriptor(): Descriptor {
  return { producer: this.state.binding!.producer, agentInstance: this.state.instance, conversation: this.conversation, parent: null, location: null };
 }
 // Integration commands (hidden CLI groups): explicit library and recording, stdin JSON.
 private invocation(operation: Operation, input?: unknown, generation?: string): Invocation {
  const b = this.state.binding!;
  return { binary: b.binary, args: [...OPERATIONS[operation], "--library", b.library, "--recording", b.recording, ...(generation ? ["--generation", generation] : [])], ...(input === undefined ? {} : { input: JSON.stringify(input) + "\n" }) };
 }
 // Bound controls: REC positional, explicit library, machine output.
 private controlInvocation(operation: "status" | "pause" | "resume" | "stop"): Invocation {
  const b = this.state.binding!;
  return { binary: b.binary, args: [operation, b.recording, "--library", b.library, "--json"] };
 }
 private register(refresh = false): void {
  if (this.state.uncertain) { this.registration = "unknown-rebind-required"; return; }
  const descriptor = this.descriptor(); const generation = this.state.ticket?.generation;
  const operation: Operation = generation ? refresh ? "refresh" : "replace" : "register";
  this.registration = "pending";
  this.pending = { kind: "registration", attempts: 0, descriptor, generation, call: this.invocation(operation, { descriptor, ...(generation ? { generation } : {}) }) };
  this.retryAt = 0;
 }
 private tick(): void {
  this.stopTick?.(); this.stopTick = undefined;
  if (!this.current()) return;
  if (this.state.binding && this.registration === "registered" && this.clock.now() >= this.refreshAt && !this.pending) this.register(true);
  this.reportDrops(); this.pump(); this.show();
  if (this.state.binding) this.stopTick = this.clock.later(() => this.tick(), 1000);
 }
 // marker selects the receiver's navigation-worthy hint (CLI "marker"); label is
 // an optional fixed envelope label (never SDK text); the envelope, bounds,
 // identity and delivery accounting are otherwise identical.
 emit(kind: string, data: { phase: string; tool?: string; role?: string }, marker = false, label?: string): void {
  if (!this.current() || !this.state.binding) return;
  const reason = this.admit(`pi:${kind}`, data, marker, false, undefined, label);
  if (reason) { this.drop(reason); this.show(); return; }
  this.state.admitted = increment(this.state.admitted); this.pump(); this.show();
 }
 private refill(): void {
  const now = this.clock.now();
  if (now > this.refilled) this.tokens = Math.min(LIMITS.burst, this.tokens + (now - this.refilled) * LIMITS.rate / 1000);
  this.refilled = Math.max(this.refilled, now);
 }
 // /reel mark [label]: one navigation mark at now through the same bounded,
 // registered delivery as SDK events: kind pi:mark, marker:true, the user's
 // bounded label in the envelope label (the record shape of `areel mark`) and
 // no data. The receiver places it at admission. Returns "queued", "unbound"
 // or the drop reason.
 markNow(text: string): "queued" | "unbound" | DropReason {
  if (!this.current() || !this.state.binding) return "unbound";
  const label = markLabel(text);
  const reason = this.admit("pi:mark", undefined, true, false, undefined, label || undefined);
  if (reason) { this.drop(reason); this.show(); return reason; }
  this.state.admitted = increment(this.state.admitted); this.pump(); this.show();
  return "queued";
 }
 // Queues one envelope or returns the reason it was refused. Rate is checked last
 // so a refused event never spends a token; reports bypass the bucket only.
 private admit(kind: string, data: object | undefined, marker: boolean, report: boolean, total?: number, label?: string): DropReason | undefined {
  if (this.state.receiverFull) return "receiver";
  if (this.registration !== "registered" || this.clock.now() >= this.refreshAt || !this.state.ticket) return "registration";
  const d = this.descriptor(); const id = randomUUID(); const generation = this.state.ticket.generation;
  const call = this.invocation(marker ? "marker" : "activity", { id, ...d, source: "pi:agent-reel", kind, ...(label === undefined ? {} : { label }), ...(data === undefined ? {} : { data }) }, generation);
  const size = Buffer.byteLength(call.input!);
  if (size > LIMITS.eventBytes) return "size";
  if (this.queue.length >= LIMITS.queue) return "queue";
  if (this.queuedBytes + size > LIMITS.bytes) return "bytes";
  if (!report) { this.refill(); if (this.tokens < 1) return "rate"; this.tokens -= 1; }
  this.queuedBytes += size;
  this.queue.push({ call, kind: "activity", attempts: 0, eventID: id, generation, descriptor: d, ...(report ? { report: total } : {}) });
  return undefined;
 }
 // Durable drop provenance: at most one outstanding coalesced pi:adapter report,
 // integers only, built from local counters (never from an SDK event). When it
 // cannot be queued it simply stays due and coalesces; it is never itself a drop.
 private reportDrops(): void {
  const d = this.state.drops;
  if (this.reporting || d.dropped <= d.reported || !this.state.binding || this.state.receiverFull) return;
  const data = { phase: "dropped", dropped: d.dropped, queue: d.queue, bytes: d.bytes, size: d.size, rate: d.rate, registration: d.registration, invalidated: d.invalidated, receiver: d.receiver };
  if (!this.admit("pi:adapter", data, false, true, d.dropped)) this.reporting = true;
 }
 startRecording(target?: string): boolean {
  if (!this.current() || this.state.controlFlight) return false;
  const binding = this.state.binding;
  if (target !== undefined && (binding || !target || Buffer.byteLength(target) > 256 || /[\x00-\x1f\x7f]/.test(target))) throw new Error("invalid-start-target");
  const binary = binding ? binding.binary : this.startWith.binary;
  // Bound restart names its recording and library; an unbound start lets the CLI
  // discover the target (TARGET after "--" so it is never parsed as a flag).
  const call: Invocation = binding ? { binary, args: ["record", "--recording", binding.recording, "--library", binding.library, "--json"] } : {
   binary, args: ["record", ...(this.startWith.library === undefined ? [] : ["--library", this.startWith.library]), "--json", ...(target === undefined ? [] : ["--", target])], discovery: true,
  };
  return this.operate(call, START_TIMEOUT, value => {
   const r = object(value); const selected = object(r.target);
   const text = (v: unknown, max: number) => typeof v === "string" && v.length > 0 && Buffer.byteLength(v) <= max && !/[\x00-\x1f\x7f]/.test(v);
   if (this.state.binding !== binding || typeof r.id !== "string" || !/^[a-f0-9]{32}$/.test(r.id) || r.status !== "recording" || r.suppressed !== false || (r.error !== undefined && r.error !== "") || (r.pending !== undefined && r.pending !== "") || !text(selected.backend, 128) || !text(selected.instance, 512) || !text(selected.name, 256) || typeof selected.address !== "string" || !validPath(selected.address) || typeof r.library !== "string" || !validPath(r.library) || (target !== undefined && selected.name !== target)) throw new Error("invalid-start-response");
   // The explicitly invoked CLI owns binding/target authority, including a new
   // recording ID after stop. Same-ID starts must not reset metadata identity,
   // but an explicit start takes ownership of an auto binding.
   if (!binding || r.id !== binding.recording) this.bind({ binary, library: binding ? binding.library : r.library as string, recording: r.id, producer: randomUUID() });
   else if (binding.auto) { delete binding.auto; this.probeGeneration++; }
   this.visual = "reported-recording"; this.visualUntil = this.clock.now() + LIMITS.visualAge;
  }, true);
 }
 control(operation: "status" | "pause" | "resume" | "stop"): boolean {
  if (!this.current() || !this.state.binding || this.state.controlFlight) return false;
  const task: Task = { kind: "control", attempts: 1, call: this.controlInvocation(operation) };
  // Explicit status is the on-demand diagnostics path: its settlement (or deadline) is reported as a notice.
  return this.operate(task.call, LIMITS.timeout, value => { this.consume(task, value); }, false, operation === "status");
 }
 private startNotice(known: boolean): void {
  this.startOutcome = known ? "reported-recording" : "outcome-unknown-check-areel-status";
  try { this.notice(known ? "reel: CLI reports recording; binding selected. Capture fidelity remains unverified." : "reel: start outcome unknown; recording may exist. Check standalone areel status with the explicit library/target before retrying. No automatic retry or stop."); } catch { /* UI is not capture authority. */ }
 }
 private operate(call: Invocation, timeout: number, consume: (value: unknown) => void, starting = false, reporting = false): boolean {
  // One separate bounded operator lane: a wedged metadata child cannot occupy it.
  const epoch = this.epoch; const abort = new AbortController(); this.state.controlFlight = abort;
  this.visualUntil = 0; this.startOutcome = starting ? "pending" : "";
  let timedOut = false;
  this.stopControlDeadline = this.clock.later(() => { timedOut = true; abort.abort(); if (this.current(epoch)) { this.visualUntil = 0; if (starting) this.startNotice(false); this.show(); if (reporting) this.report(); } }, timeout);
  const complete = (value: unknown, failed: boolean) => {
   if (this.state.controlFlight === abort) this.state.controlFlight = undefined;
   if (!this.current(epoch)) return;
   this.stopControlDeadline?.(); this.stopControlDeadline = undefined;
   if (!failed && !timedOut) { try { consume(value); } catch { failed = true; this.visualUntil = 0; } }
   if (starting && !timedOut) this.startNotice(!failed);
   this.show();
   if (reporting && !timedOut) this.report();
  };
  let run: Promise<unknown>;
  try { run = this.runner(call, abort.signal); } catch { run = Promise.reject(new Error("unavailable")); }
  void run.then(value => complete(value, false), () => complete(undefined, true)); this.show(); return true;
 }
 private consume(task: Task, value: unknown): boolean {
  const r = object(value);
  if (task.kind === "activity" && typeof r.error === "string" && r.error.includes(RECEIVER_FULL)) this.latchReceiverFull();
  if (r.error) throw new Error("unavailable");
  if (task.kind === "registration") {
   const registration = object(r.registration);
   if (r.accepted !== false || r.durable !== false || r.failed !== false || typeof registration.expiresAt !== "string" || !Number.isFinite(Date.parse(registration.expiresAt)) || registration.durability !== "volatile-holder-lifetime" || typeof registration.generation !== "string" || !/^[a-f0-9]{32}$/.test(registration.generation) || !sameDescriptor(registration.descriptor, task.descriptor!)) throw new Error("invalid-registration");
   if ((isOperation(task.call, "refresh") && registration.generation !== task.generation) || (isOperation(task.call, "replace") && registration.generation === task.generation)) throw new Error("invalid-generation-transition");
   this.state.ticket = { generation: registration.generation, descriptor: task.descriptor! };
   this.state.uncertain = false; this.registration = "registered"; this.refreshAt = this.clock.now() + LIMITS.refresh; return true;
  }
  if (task.kind === "activity") {
   const receipt = object(r.receipt); const context = object(r.registeredContext);
   // The receiver owns its record cap; any positive safe integer sequence is a
   // well-formed receipt. An upper bound here would misreport durable admissions.
   if (r.accepted !== true || r.failed !== false || receipt.id !== task.eventID || !Number.isSafeInteger(receipt.sequence) || (receipt.sequence as number) < 1 || context.generation !== task.generation || !sameDescriptor(context.descriptor, task.descriptor!)) throw new Error("invalid-receipt");
   if (r.durable === true) { this.durable = increment(this.durable); this.delivery = "durable"; return true; }
   if (r.durable !== false) throw new Error("invalid-durability");
   task.replied = true; this.delivery = "pending"; return false;
  }
  if (r.id !== this.state.binding!.recording || r.pending || !["recording", "paused", "stopped", "interrupted"].includes(String(r.status))) throw new Error("invalid-status");
  this.visual = `reported-${r.status}`; this.visualUntil = this.clock.now() + LIMITS.visualAge; return true;
 }
 private pump(): void {
  if (!this.current() || !this.state.binding || this.state.flight || this.clock.now() < this.retryAt) return;
  if (!this.pending && this.registration === "registered" && this.clock.now() >= this.refreshAt) this.register(true);
  if (!this.pending && this.registration === "registered") {
   this.pending = this.queue.shift(); if (this.pending) this.queuedBytes -= Buffer.byteLength(this.pending.call.input!);
  }
  const task = this.pending; if (!task) return;
  const epoch = this.epoch; const binding = this.state.binding;
  const flight = { abort: new AbortController(), binding, kind: task.kind }; this.state.flight = flight; task.attempts++;
  let timedOut = false;
  this.stopDeadline = this.clock.later(() => {
   timedOut = true; flight.abort.abort();
   if (this.current(epoch)) { if (task.kind === "registration") this.registration = "unknown"; if (task.kind === "activity") this.delivery = "unknown"; this.visualUntil = 0; this.show(); }
  }, LIMITS.timeout);
  let run: Promise<unknown>;
  try { run = this.runner(task.call, flight.abort.signal); } catch { run = Promise.reject(new Error("unavailable")); }
  const complete = (value: unknown, failed: boolean) => {
   if (this.state.flight === flight) this.state.flight = undefined;
   if (!this.current(epoch)) { this.state.active?.wake(); return; }
   this.stopDeadline?.(); this.stopDeadline = undefined;
   let done = false;
   if (!failed && !timedOut) { try { done = this.consume(task, value); } catch { failed = true; } }
   else failed = true;
   // A drop report the receiver accepted (durable, or pending when exhausted) is
   // reported; one whose every attempt failed stays due and is re-queued by a later tick.
   const settle = () => {
    if (task.report === undefined) return;
    this.reporting = false;
    if (done || task.replied) this.state.drops.reported = Math.max(this.state.drops.reported, task.report);
   };
   if (done) { this.pending = undefined; this.retryAt = 0; settle(); }
   else if (task.attempts < (task.kind === "control" ? 1 : LIMITS.attempts)) { this.retryAt = this.clock.now() + LIMITS.retry; }
   else {
    this.pending = undefined; this.retryAt = 0; settle();
    if (task.kind === "registration") { this.registration = "unknown-rebind-required"; this.state.uncertain = true; }
    // A capped event the receiver refused is a receiver drop, not an unconfirmed delivery.
    if (task.kind === "activity" && this.state.receiverFull && !task.replied) { if (task.report === undefined) this.drop("receiver"); this.delivery = "receiver-full"; }
    else if (task.kind === "activity") { this.unconfirmed = increment(this.unconfirmed); this.delivery = failed ? "unknown" : "pending-unconfirmed"; }
    if (task.kind === "control") this.visualUntil = 0;
   }
   this.show(); this.pump();
  };
  void run.then(value => complete(value, false), () => complete(undefined, true));
 }
 // Latch: stop sending activity for this binding. Queued events become receiver
 // drops; a queued drop report is discarded and stays due (never itself a drop).
 // Registration and control lanes are unaffected.
 private latchReceiverFull(): void {
  if (this.state.receiverFull) return;
  this.state.receiverFull = true;
  this.drop("receiver", this.queue.filter(t => t.kind === "activity" && t.report === undefined).length);
  if (this.queue.some(t => t.report !== undefined)) this.reporting = false;
  this.queue = this.queue.filter(t => t.kind !== "activity"); this.queuedBytes = this.queue.reduce((n, t) => n + Buffer.byteLength(t.call.input ?? ""), 0);
 }
 wake(): void { this.pump(); this.show(); }
 // Explicit /reel unbind: unbind and suppress auto-binding for this process
 // until an explicit /reel bind or /reel start clears it.
 optOut(): void { if (!this.current()) return; this.state.autoOptOut = true; this.probeOutcome = "opted-out"; this.unbind(); }
 // Explicit /reel bind or /reel start re-enables auto-binding.
 optIn(): void { if (this.current()) this.state.autoOptOut = false; }
 private scheduleProbe(ms: number): void {
  this.stopProbe?.(); this.stopProbe = undefined;
  if (!this.startWith.autoBind || !this.current()) return;
  this.stopProbe = this.clock.later(() => this.probe(), ms);
 }
 // Auto-bind proof: the CLI answers whether a live holder records the target
 // containing this terminal (caller environment only, never AREEL_RECORDING).
 // Skipped while opted out, while explicitly bound, or while a previous probe
 // child is still open. Output is never shown; the outcome is a reason code.
 private probe(): void {
  this.stopProbe = undefined;
  if (!this.current()) return;
  this.scheduleProbe(LIMITS.autoInterval);
  if (this.state.autoOptOut || this.state.probeFlight || (this.state.binding && !this.state.binding.auto)) return;
  const generation = this.probeGeneration; const abort = new AbortController(); this.state.probeFlight = abort;
  const call: Invocation = { binary: this.startWith.binary, args: ["activity", "target", "--json", ...(this.startWith.library === undefined ? [] : ["--library", this.startWith.library])], discovery: true };
  let timedOut = false;
  const stopDeadline = this.clock.later(() => { timedOut = true; abort.abort(); }, LIMITS.timeout);
  const complete = (value: unknown, failed: boolean) => {
   if (this.state.probeFlight === abort) this.state.probeFlight = undefined;
   stopDeadline();
   if (!this.current() || generation !== this.probeGeneration) return;
   if (failed || timedOut) { this.probeOutcome = "unknown"; return; }
   try { this.applyProbe(value); } catch { this.probeOutcome = "unknown"; }
  };
  let run: Promise<unknown>;
  try { run = this.runner(call, abort.signal); } catch { run = Promise.reject(new Error("unavailable")); }
  void run.then(value => complete(value, false), () => complete(undefined, true));
 }
 private applyProbe(value: unknown): void {
  const r = object(value); const text = (v: unknown, max: number) => typeof v === "string" && v.length > 0 && Buffer.byteLength(v) <= max && !/[\x00-\x1f\x7f]/.test(v);
  if (this.state.autoOptOut) return;
  const binding = this.state.binding;
  if (binding && !binding.auto) return; // An explicit binding is never replaced automatically.
  if (r.covered === true) {
   const target = object(r.target);
   if (typeof r.recording !== "string" || !/^[a-f0-9]{32}$/.test(r.recording) || typeof r.library !== "string" || !validPath(r.library) || r.reason !== "covered" || !text(r.status, 64) || !text(target.backend, 128) || !text(target.instance, 512) || !text(target.name, 256)) throw new Error("invalid-probe");
   this.probeOutcome = "covered";
   if (binding && binding.recording === r.recording && binding.library === r.library) return;
   // A new covering recording replaces the ended one: bind() drops the old binding's queue and ticket.
   this.bind({ binary: this.startWith.binary, library: r.library, recording: r.recording, producer: randomUUID(), auto: true });
   return;
  }
  if (r.covered !== false || r.recording !== null || typeof r.reason !== "string" || !/^[a-z][a-z-]{0,63}$/.test(r.reason) || r.reason === "covered") throw new Error("invalid-probe");
  this.probeOutcome = r.reason;
  if (binding?.auto && !PROBE_INCONCLUSIVE.has(r.reason)) this.unbind();
 }
}
