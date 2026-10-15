import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Adapter, conversationID, defaultStartConfig, newProcessState, parseBinding, realClock, startConfig, type Clock, type ProcessState, type StartConfig } from "./adapter.ts";
import { runCLI, type Runner } from "./runner.ts";

const builtins = new Set(["bash", "powershell", "read", "write", "edit", "grep", "find", "ls"]);
const tool = (value: string) => builtins.has(value) ? value : "other";
// Documented SDK message roles only (session-format.md); anything else is "other".
const roles = new Set(["user", "assistant", "toolResult"]);
const role = (value: unknown) => typeof value === "string" && roles.has(value) ? value : "other";
// ModelSelectSource enum from types.d.ts ("set" | "cycle" | "restore"); anything else is "other".
const sources = new Set(["set", "cycle", "restore"]);
const source = (value: unknown) => typeof value === "string" && sources.has(value) ? value : "other";
const processKey = Symbol.for("mono-agent.agent-reel.process");
function processState(): ProcessState {
 const root = globalThis as unknown as Record<symbol, ProcessState>;
 const state = root[processKey] ??= newProcessState();
 // A process state created by an earlier package version (live reload) lacks the V4 counters.
 const fresh = newProcessState(); state.drops ??= fresh.drops; state.drops.receiver ??= 0; state.admitted ??= 0; state.receiverFull ??= false; state.autoOptOut ??= false;
 return state;
}
// Exported seam installs real SDK-shaped hooks against fake API/CLI/clock tests;
// it never imports collector code or inspects terminal/backend/environment identity.
// start: the unbound-start binary (default "areel" from PATH) and an optional
// explicit library; without one the CLI's own settings choose the library.
// start.autoBind enables the CLI proof query for automatic binding (the real
// entrypoint enables it; the bare seam leaves it off).
export function install(pi: ExtensionAPI, state: ProcessState, runner: Runner, clock: Clock, start: StartConfig = startConfig()): Adapter {
 let context: ExtensionContext | undefined;
 // The persistent slot receives only the adapter indicator (🔴 or cleared); diagnostics go through notify.
 const adapter = new Adapter(state, runner, clock, text => { if (context?.hasUI) context.ui.setStatus("agent-reel", text); }, text => { if (context?.hasUI) context.ui.notify(text, "info"); }, start);
 function unavailable(ctx: ExtensionContext, text: string): void {
  try { if (ctx.hasUI) { ctx.ui.setStatus("agent-reel", undefined); ctx.ui.notify(text, "warning"); } } catch { /* stale UI */ }
 }
 function sid(ctx: ExtensionContext): string | null { return conversationID(ctx.sessionManager.getSessionId()); }
 // Ownership and session freshness gate: a replaced owner or another session
 // never reads the SDK event, not even its allowlisted keys.
 const stale = (ctx: ExtensionContext) => state.active !== adapter || sid(ctx) !== adapter.conversation;
 type Data = { phase: string; tool?: string; role?: string };
 // read runs only after the gate, so no event property is touched when stale.
 // It returns the kind, data, marker flag and an optional fixed label.
 function record(ctx: ExtensionContext, read: () => [kind: string, data: Data, marker?: boolean, label?: string]) {
  try {
   if (stale(ctx)) return;
   const [kind, data, marker = false, label] = read();
   adapter.emit(kind, data, marker, label);
  } catch { /* No raw SDK/error/context data enters diagnostics or activity. */ }
 }
 const mark = (ctx: ExtensionContext, kind: string, read: () => Data, marker = false, label?: string) => record(ctx, () => [kind, read(), marker, label]);
 // Shared agent vocabulary (the same shape as the other agents' hook
 // adapters): prompt submit is the pi:prompt marker {submit, role user}; the
 // end of the agent's turn is the pi:turn marker {end, role assistant} at
 // agent_settled (turn_end is one LLM step: pi:step); a completed compaction is
 // a marker labelled "Compaction". Everything else is a plain row. Markers
 // carry role only, never a tool, and prompt/turn markers carry no label.
 // Message boundaries read message.role only; content, usage, model, provider
 // and tool bodies are never read.
 function messageRole(e: unknown): string {
  const payload = e && typeof e === "object" ? (e as { message?: unknown }).message : undefined;
  return role(payload && typeof payload === "object" ? (payload as { role?: unknown }).role : undefined);
 }
 pi.on("session_start", (_event, ctx) => {
  if (state.active !== adapter) return;
  context = ctx;
  try { adapter.start(sid(ctx)); }
  catch { adapter.dispose(); unavailable(ctx, "reel unavailable: update Pi to >=0.85.1 and retest"); }
 });
 pi.on("session_shutdown", event => {
  const ownsUI = state.active === adapter;
  if (event.reason === "quit") adapter.unbind();
  adapter.dispose();
  try { if (ownsUI && context?.hasUI) context.ui.setStatus("agent-reel", undefined); } catch { /* stale UI */ }
  context = undefined;
 });
 pi.on("session_tree", (_event, ctx) => {
  if (state.active !== adapter) return;
  try { context = ctx; adapter.start(sid(ctx)); }
  catch { adapter.dispose(); unavailable(ctx, "reel unavailable: session unavailable; visual, registration and metadata are unknown"); }
 });
 pi.on("agent_start", (_event, ctx) => mark(ctx, "agent", () => ({ phase: "start" })));
 pi.on("agent_end", (_event, ctx) => mark(ctx, "agent", () => ({ phase: "run-end" })));
 // agent_settled: Pi will not continue on its own (after retries, compaction
 // and queued follow-ups), so it is the end of the agent's turn.
 pi.on("agent_settled", (_event, ctx) => mark(ctx, "turn", () => ({ phase: "end", role: "assistant" }), true));
 pi.on("turn_start", (_event, ctx) => mark(ctx, "step", () => ({ phase: "start" })));
 pi.on("turn_end", (_event, ctx) => mark(ctx, "step", () => ({ phase: "end" })));
 pi.on("tool_execution_start", (e, ctx) => mark(ctx, "tool", () => ({ phase: "start", tool: tool(e.toolName) })));
 pi.on("tool_execution_end", (e, ctx) => mark(ctx, "tool", () => ({ phase: e.isError ? "failed" : "end", tool: tool(e.toolName) })));
 pi.on("ui_prompt_start", (_event, ctx) => mark(ctx, "ui", () => ({ phase: "waiting" })));
 pi.on("ui_prompt_end", (_event, ctx) => mark(ctx, "ui", () => ({ phase: "closed" })));
 pi.on("session_compact", (_event, ctx) => mark(ctx, "compaction", () => ({ phase: "complete" }), true, "Compaction"));
 pi.on("session_compact_failed", (_event, ctx) => mark(ctx, "compaction", () => ({ phase: "failed" })));
 pi.on("message_start", (e, ctx) => mark(ctx, "message", () => ({ phase: "start", role: messageRole(e) })));
 // A user message_end is the prompt submit marker; every other role is a plain row.
 pi.on("message_end", (e, ctx) => record(ctx, () => {
  const r = messageRole(e);
  return r === "user" ? ["prompt", { phase: "submit", role: r }, true] : ["message", { phase: "end", role: r }];
 }));
 // model_select: phase is the validated source enum only (set/cycle/restore);
 // model and previousModel objects are never read. Observer only, no return.
 pi.on("model_select", (e, ctx) => mark(ctx, "model", () => ({ phase: source(e?.source) })));
 // thinking_level_select: constant phase; level/previousLevel are never read.
 pi.on("thinking_level_select", (_event, ctx) => mark(ctx, "thinking", () => ({ phase: "changed" })));
 // Deliberately no input/message_update/tool_execution_update/provider hooks:
 // slash commands are not prompts, agent_end is not agent_settled, and streaming
 // bodies are never copied. Message hooks contribute only a role label.
 pi.registerCommand("reel", {
  description: 'Recording: auto-binds to a live recording of this terminal; /reel start [target]; mark [label]; status|pause|resume|stop|unbind (opts out of auto-bind); bind ["/absolute/areel","/absolute/library","recording-id"]',
  handler: async (args, ctx) => {
   try {
    if (sid(ctx) !== adapter.conversation || state.active !== adapter) return;
    if (args === "start" || args.startsWith("start ")) {
     adapter.optIn();
     if (!adapter.startRecording(args === "start" ? undefined : args.slice(6))) {
      if (ctx.hasUI) ctx.ui.notify("reel: start not dispatched (busy or inactive); no start queued. Standalone areel controls remain independent.", "warning");
      return;
     }
    }
    else if (args === "mark" || args.startsWith("mark ")) {
     // The label is the user's own text; the notice never echoes it (the
     // terminal is inside the recording) and says only what happened locally.
     const outcome = adapter.markNow(args.slice(5));
     if (ctx.hasUI) {
      if (outcome === "queued") ctx.ui.notify("reel: mark queued; /reel status shows its delivery.", "info");
      else if (outcome === "unbound") ctx.ui.notify("reel: mark not sent: no bound recording. Start or bind one first.", "warning");
      else ctx.ui.notify(`reel: mark not sent (${outcome}).`, "warning");
     }
     return;
    }
    else if (args.startsWith("bind ")) { const binding = parseBinding(args.slice(5)); adapter.optIn(); adapter.bind(binding); }
    else if (args === "unbind") adapter.optOut();
    else if (args === "status" && !state.binding) { if (ctx.hasUI) ctx.ui.notify(adapter.status(), "info"); return; }
    else if (args === "status" || args === "pause" || args === "resume" || args === "stop") {
     if (!adapter.control(args)) {
      if (ctx.hasUI) ctx.ui.notify("reel: control not dispatched (unbound or busy); standalone areel controls remain independent.", "warning");
      return;
     }
    }
    else throw new Error("invalid-command");
    // /reel status reports its diagnostics when the CLI reply settles (see Adapter.control).
    if (args !== "status" && ctx.hasUI) ctx.ui.notify(adapter.status(), "info");
   } catch {
    try { if (ctx.hasUI) ctx.ui.notify("reel: invalid command or unavailable. Use /reel start [target] when unbound, /reel start for the existing binding, or an explicit absolute /reel bind. No automatic capture action.", "warning"); } catch { /* stale UI */ }
   }
  },
 });
 return adapter;
}
export default function agentReel(pi: ExtensionAPI): void { install(pi, processState(), runCLI, realClock, defaultStartConfig()); }
