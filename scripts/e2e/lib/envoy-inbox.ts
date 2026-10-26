// scripts/e2e/lib/envoy-inbox.ts: a stand-in agent session's serving port, so a live proof can be the
// sender of an Envoy message and read what comes back on its own agent topic. A session registers
// with the listener with this port (POST /v1/interests/subscribe, `port` and `host`), and the
// listener that owns it delivers every message for it as a prompt, POST
// /session/<session>/prompt_async with `{"parts":[{"type":"text","text":…}]}`
// (packages/envoy/internal/session, Deliverer.prompt). This answers each with 204, as a plugin
// does, and appends one JSON line per delivery to --record: when it came, the session it named and
// its text.
//
//   bun scripts/e2e/lib/envoy-inbox.ts --port <port> --record <file.jsonl>
//
// It prints `envoy-inbox listening on 127.0.0.1:<port>` once it serves (--port 0 picks a free port).
import { appendFileSync } from "node:fs";

const promptPath = /^\/session\/([^/]+)\/prompt_async$/;

export function startInbox(port: number, record: string) {
  return Bun.serve({
    hostname: "127.0.0.1",
    port,
    async fetch(request) {
      const match = promptPath.exec(new URL(request.url).pathname);
      if (request.method !== "POST" || !match) return new Response("not found", { status: 404 });
      const body = (await request.json().catch(() => null)) as {
        parts?: { type?: string; text?: string }[];
      } | null;
      const text = (body?.parts ?? [])
        .filter((part) => part.type === "text" && typeof part.text === "string")
        .map((part) => part.text)
        .join("\n");
      appendFileSync(
        record,
        `${JSON.stringify({ at: new Date().toISOString(), session: decodeURIComponent(match[1] ?? ""), text })}\n`
      );
      return new Response(null, { status: 204 });
    },
  });
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const port = args[0] === "--port" ? Number(args[1]) : Number.NaN;
  const record = args[2] === "--record" ? args[3] : undefined;
  if (!Number.isInteger(port) || port < 0 || port > 65535 || !record || args.length !== 4) {
    console.error("usage: bun scripts/e2e/lib/envoy-inbox.ts --port <port> --record <file.jsonl>");
    process.exit(2);
  }
  const server = startInbox(port, record);
  console.log(`envoy-inbox listening on 127.0.0.1:${server.port}`);
}
