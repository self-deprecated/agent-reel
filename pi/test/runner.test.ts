import assert from "node:assert/strict";
import { test } from "node:test";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { spawn } from "node:child_process";
import { createRunner, OUTPUT_BYTES } from "../extensions/runner.ts";

function childFixture() {
 const child = Object.assign(new EventEmitter(), { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(), kills: 0, kill() { this.kills++; return true; } });
 let observed: unknown[] = [];
 const start = ((...args: unknown[]) => { observed = args; return child; }) as unknown as typeof spawn;
 return { child, runner: createRunner(start), observed: () => observed };
}
const call = { binary: "/explicit/areel", args: ["activity", "--recording", "a".repeat(32)], input: "{}\n" };
test("native runner shape: no shell, only PATH environment, bounded JSON; no real child spawned", async () => {
 const f = childFixture(); const result = f.runner(call, new AbortController().signal);
 f.child.stdout.emit("data", Buffer.from('{"version":2}\n')); f.child.emit("close", 0);
 assert.deepEqual(await result, { version: 2 });
 const args = f.observed(); assert.equal(args[0], call.binary); assert.deepEqual(args[1], call.args);
 const options = args[2] as { shell: boolean; env: object }; assert.equal(options.shell, false); assert.deepEqual(Object.keys(options.env), ["PATH"]);
});
test("overflow/abort/error remain generic; ownership held until close", async () => {
 for (const mode of ["stdout", "stderr", "abort", "error", "invalid-json", "bad-utf8", "exit"] as const) {
  const f = childFixture(); const controller = new AbortController(); let settled = false;
  const result = f.runner(call, controller.signal).then(() => { settled = true; return "unexpected"; }, error => { settled = true; return String(error); });
  if (mode === "stdout") f.child.stdout.emit("data", Buffer.alloc(OUTPUT_BYTES + 1, 65));
  if (mode === "stderr") f.child.stderr.emit("data", Buffer.from("PRIVATE_SECRET".repeat(2000)));
  if (mode === "abort") controller.abort();
  if (mode === "error") f.child.emit("error", new Error("PRIVATE_SECRET"));
  if (mode === "invalid-json") f.child.stdout.emit("data", Buffer.from("PRIVATE_SECRET"));
  if (mode === "bad-utf8") f.child.stdout.emit("data", Buffer.from([0xff]));
  await Promise.resolve(); assert.equal(settled, false, mode);
  f.child.emit("close", mode === "exit" ? 1 : 0);
  assert.equal(await result, "Error: reel-unavailable", mode);
  if (["stdout", "stderr", "abort"].includes(mode)) assert.equal(f.child.kills, 1);
 }
});
test("pre-aborted or oversized input never starts even a fake child", async () => {
 const f = childFixture(); const abort = new AbortController(); abort.abort();
 await assert.rejects(f.runner(call, abort.signal), /reel-unavailable/);
 await assert.rejects(f.runner({ ...call, input: "x".repeat(8193) }, new AbortController().signal), /reel-unavailable/);
 assert.equal(f.observed().length, 0);
});
test("a nonzero exit surfaces only a well-formed error reply; anything else stays generic", async () => {
 const f = childFixture(); const result = f.runner(call, new AbortController().signal);
 f.child.stdout.emit("data", Buffer.from('{"accepted":false,"error":"activity record/dedup limit reached"}\n')); f.child.emit("close", 1);
 assert.deepEqual(await result, { accepted: false, error: "activity record/dedup limit reached" });
 for (const body of ['{"accepted":false}', '{"error":""}', '{"error":7}', '[{"error":"x"}]']) {
  const g = childFixture(); const pending = g.runner(call, new AbortController().signal);
  g.child.stdout.emit("data", Buffer.from(body)); g.child.emit("close", 1);
  await assert.rejects(pending, /reel-unavailable/, body);
 }
});
