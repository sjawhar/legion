#!/usr/bin/env bun
// Runs one native Dispatch tool through `executeDispatchTool`, the function every host's registered
// `dispatch_*` tool calls (pi-envoy `extensions/envoy.ts`, claude-envoy
// `src/envoy-channel-server.ts`, envoy-plugin `src/server.ts`), and prints what the model would
// see: the result text on success (exit 0), the failure text on a refusal (exit 1). Dispatch is
// resolved as the hosts resolve it (`activeDispatchConfig`: envoy.json, then `DISPATCH_URL` with
// `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`), so to drive a stand-in set both `DISPATCH_URL` and
// `DISPATCH_TOKEN`, or the configured bearer is sent to the stand-in. Every request is traced on
// stderr as `METHOD URL -> status content-type`. A write tool against a real Dispatch writes there,
// as the session `ENVOY_SESSION_ID` names (a fresh id when unset).
//   bun bin/dispatch-tool.ts <dispatch_tool> '<json arguments>'
import { activeDispatchConfig } from "../src/dispatch-config";
import { executeDispatchTool } from "../src/dispatch-execute";
import { messageFor } from "../src/errors";

const [tool, rawArguments] = Bun.argv.slice(2);
if (tool === undefined || rawArguments === undefined) {
  console.error("usage: bun bin/dispatch-tool.ts <dispatch_tool> '<json arguments>'");
  process.exit(2);
}
const config = activeDispatchConfig(process.env, { cwd: process.cwd() });
if (config === null) {
  console.error("dispatch-tool: Dispatch is not configured; set DISPATCH_URL and DISPATCH_TOKEN");
  process.exit(2);
}
const traced: typeof fetch = Object.assign(
  async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const response = await fetch(input, init);
    const contentType = response.headers.get("content-type") ?? "(no content type)";
    console.error(`${init?.method ?? "GET"} ${String(input)} -> ${response.status} ${contentType}`);
    return response;
  },
  { preconnect: fetch.preconnect }
);
try {
  const result = await executeDispatchTool({
    tool,
    args: JSON.parse(rawArguments),
    cwd: process.cwd(),
    host: "omp",
    sessionId: process.env.ENVOY_SESSION_ID ?? `dispatch-tool-${crypto.randomUUID()}`,
    config,
    env: process.env,
    fetchImpl: traced,
  });
  console.log(result.text);
  console.error(`details: ${JSON.stringify(result.details)}`);
} catch (error) {
  console.log(messageFor(error));
  process.exit(1);
}
