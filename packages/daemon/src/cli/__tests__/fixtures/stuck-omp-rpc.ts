#!/usr/bin/env bun
/**
 * Stands in for a worker's `omp --mode rpc` process that is HUNG and never reacts to its stdin
 * being closed (the shim's graceful-shutdown signal) — proves the timeout-then-kill fallback in
 * a real E2E, alongside `fake-omp-rpc.ts`'s graceful-exit case. Negotiates protocol like the
 * real thing, then just keeps running forever.
 */
const decoder = new TextDecoder();
let buffer = "";

export {};

for await (const chunk of Bun.stdin.stream()) {
  buffer += decoder.decode(chunk, { stream: true });
  let index = buffer.indexOf("\n");
  while (index !== -1) {
    const line = buffer.slice(0, index).trim();
    buffer = buffer.slice(index + 1);
    index = buffer.indexOf("\n");
    if (!line) continue;
    const command = JSON.parse(line) as { id?: string; type: string };
    if (command.type === "negotiate_protocol") {
      console.log(
        JSON.stringify({
          id: command.id,
          type: "response",
          command: "negotiate_protocol",
          success: true,
          data: { protocolVersion: 2 },
        })
      );
    }
  }
}
// Deliberately never exits here (stdin EOF reached above but no process.exit call): mirrors a
// hung OMP process that never completes session_shutdown, so the shim never sees its own
// process exit and its socket never closes on its own.
await new Promise(() => {});
