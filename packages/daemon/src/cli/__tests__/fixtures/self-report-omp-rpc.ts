#!/usr/bin/env bun
/**
 * Stands in for the real root architect's OMP process for the self-report-deadlock E2E:
 * negotiates the RPC protocol like the real thing, then on stdin EOF (the shim's graceful
 * shutdown signal) mirrors pi-envoy's actual `session_shutdown` hook by POSTing
 * `/legion/v1/process/exit` to the real daemon and AWAITING its response before exiting --
 * exactly the sequence that deadlocks if `/process/exit`'s handler ever joins the very
 * `closeTree` call that is asking this process to exit. Reads its own daemon URL and auth
 * material from the environment, exactly as the real extension does.
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
const daemonUrl = process.env.LEGION_DAEMON_URL;
const tree = process.env.LEGION_TREE;
const generation = process.env.LEGION_GENERATION;
const sessionId = process.env.LEGION_SELF_REPORT_SESSION_ID;
const secretFile = process.env.LEGION_SELF_REPORT_SECRET_FILE;
// Optional: when set, the real HTTP status this process observed is written here before exiting
// so the harness driving this fixture can assert the authenticated round trip actually reached
// `reportRootExit` (a non-200 here — e.g. a 403 from an unauthenticated/stale secret — would
// mean the fixture exited for the wrong reason, proving nothing about the deadlock this fixture
// exists to exercise).
const resultFile = process.env.LEGION_SELF_REPORT_RESULT_FILE;
if (!daemonUrl || !tree || !generation || !sessionId || !secretFile) {
  throw new Error("self-report-omp-rpc.ts is missing required LEGION_* environment");
}
const secret = (await Bun.file(secretFile).text()).trim();

console.error(`[self-report] stdin closed; posting /process/exit before exiting`);
const response = await fetch(`${daemonUrl}/legion/v1/process/exit`, {
  method: "POST",
  headers: { "content-type": "application/json" },
  body: JSON.stringify({
    tree,
    generation: Number(generation),
    sessionId,
    secret,
  }),
});
console.error(`[self-report] /process/exit responded ${response.status}; exiting now`);
if (resultFile) await Bun.write(resultFile, String(response.status));
process.exit(0);
