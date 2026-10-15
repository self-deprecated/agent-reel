import { spawn } from "node:child_process";

export interface Invocation { binary: string; args: string[]; input?: string; discovery?: true; }
// Settlement means the owned child has closed, NOT just that a deadline elapsed.
export type Runner = (call: Invocation, signal: AbortSignal) => Promise<unknown>;
export const OUTPUT_BYTES = 64 * 1024;
export const INPUT_BYTES = 8192;
export const DISCOVERY_BYTES = 16384;
// Transport only: CLI owns precedence, ambiguity and namespace interpretation.
// HOME, XDG_CONFIG_HOME and XDG_DATA_HOME let the CLI locate its standard user
// config file and default library (areel config); the adapter never reads them.
// AREEL_LIBRARY, AREEL_CONFIG (whose file can set the library) and
// AREEL_RECORDING are never forwarded. Discovery transport is allowed only for
// an unbound `record` and for the read-only `activity target` proof query.
export const DISCOVERY_KEYS = ["AREEL_BACKEND", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "ZELLIJ_SESSION_NAME", "TMUX", "ZELLIJ_SOCKET_DIR", "XDG_RUNTIME_DIR", "TMPDIR"] as const;

// SDK pi.exec has neither stdin nor bounded output options (Pi0.85.1).
// Follow the native single-child pattern in pi/collect, without importing it.
export function createRunner(start: typeof spawn = spawn, environment: NodeJS.ProcessEnv = process.env): Runner { return (call, signal) => {
 if (signal.aborted || Buffer.byteLength(call.input ?? "") > INPUT_BYTES) return Promise.reject(new Error("reel-unavailable"));
 return new Promise((resolve, reject) => {
  const env: NodeJS.ProcessEnv = { PATH: environment.PATH ?? "" };
  if (call.discovery) {
   const discovery = call.args[0] === "record" || (call.args[0] === "activity" && call.args[1] === "target");
   if (!discovery || call.args.includes("--recording")) { reject(new Error("reel-unavailable")); return; }
   let bytes = 0;
   for (const key of DISCOVERY_KEYS) {
    const value = environment[key];
    if (value !== undefined) {
     bytes += Buffer.byteLength(value);
     if (bytes > DISCOVERY_BYTES || value.includes("\x00")) { reject(new Error("reel-unavailable")); return; }
     env[key] = value;
    }
   }
  }
  const child = start(call.binary, call.args, { shell: false, env, stdio: ["pipe", "pipe", "pipe"] });
  let failed = false; let stopping = false; let size = 0; let errors = 0; const chunks: Buffer[] = [];
  const stop = () => { failed = true; if (!stopping) { stopping = true; child.kill("SIGKILL"); } };
  signal.addEventListener("abort", stop, { once: true });
  child.on("error", () => { failed = true; });
  child.stdin.on("error", stop);
  child.stdout.on("data", (chunk: Buffer) => { size += chunk.length; if (size > OUTPUT_BYTES) stop(); else if (!failed) chunks.push(chunk); });
  child.stderr.on("data", (chunk: Buffer) => { errors += chunk.length; if (errors > 16384) stop(); }); // Never retain/forward diagnostics.
  child.once("close", code => {
   signal.removeEventListener("abort", stop);
   if (failed) { reject(new Error("reel-unavailable")); return; }
   try {
    const text = new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks));
    const value: unknown = JSON.parse(text);
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error();
    // The CLI prints its reply and then exits nonzero when the reply carries an
    // error. Such a well-formed error reply is surfaced (every consumer treats a
    // non-empty error as failure) so a receiver refusal can be recognized; any
    // other nonzero exit stays generic.
    const error = (value as { error?: unknown }).error;
    if (code !== 0 && (typeof error !== "string" || error === "")) throw new Error();
    resolve(value);
   } catch { reject(new Error("reel-unavailable")); }
  });
  child.stdin.end(call.input);
  if (signal.aborted) stop();
 });
}; }
export const runCLI = createRunner();
