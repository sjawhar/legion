import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { tool } from "@opencode-ai/plugin/tool";

const root = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";

async function call(path: string, init?: RequestInit) {
  const res = await fetch(`${root}${path}`, init);
  const text = await res.text();
  if (!res.ok) throw new Error(text || `${res.status}`);
  return text;
}

export default async (input: { serverUrl: URL }) => {
  const shellPid = process.env.OC_SHELL_PID;
  const registryDir = process.env.OC_REGISTRY;
  const file = shellPid && registryDir ? `${registryDir}/${shellPid}.json` : null;
  let activeSessionID: string | null = null;

  const update = (patch: Record<string, unknown>) => {
    if (!file) return;
    try {
      const data = JSON.parse(readFileSync(file, "utf-8"));
      Object.assign(data, patch);
      writeFileSync(file, `${JSON.stringify(data)}\n`);
    } catch {}
  };

  const currentPort = () => {
    const value = Number.parseInt(input.serverUrl.port, 10);
    if (Number.isFinite(value) && value > 0) return value;

    try {
      const output = execFileSync("ss", ["-tlnp"], { encoding: "utf-8" });
      for (const line of output.split("\n")) {
        if (!line.includes(`pid=${process.pid}`)) continue;
        const parts = line.trim().split(/\s+/);
        const local = parts[3];
        const match = local?.match(/:(\d+)$/);
        if (!match) continue;
        const port = Number.parseInt(match[1], 10);
        if (Number.isFinite(port) && port > 0) return port;
      }
    } catch {}

    return null;
  };

  const syncPort = () => {
    const value = currentPort();
    if (!value) return false;
    update({ port: value });
    return true;
  };

  const fetchSession = async (sessionID: string) => {
    try {
      const res = await fetch(`${input.serverUrl}/session/${sessionID}`);
      if (!res.ok) return null;
      const session = await res.json();
      return { id: session.id, title: session.title || "" };
    } catch {
      return null;
    }
  };

  if (file) {
    syncPort();
    const timer = setInterval(() => {
      if (syncPort()) clearInterval(timer);
    }, 1000);
  }

  return {
    event: file
      ? async ({ event }: { event: { type?: string; properties?: Record<string, unknown> } }) => {
          syncPort();

          if (
            event.type === "session.status" &&
            (event.properties?.status as { type?: string } | undefined)?.type === "busy"
          ) {
            const sessionID = event.properties?.sessionID as string | undefined;
            if (sessionID && sessionID !== activeSessionID) {
              activeSessionID = sessionID;
              const session = await fetchSession(sessionID);
              if (session) update({ session });
            }
          }

          if (event.type === "session.updated") {
            const info = event.properties?.info as { id?: string; title?: string } | undefined;
            if (info && info.id === activeSessionID) {
              update({ session: { id: info.id, title: info.title || "" } });
            }
          }

          if (event.type === "session.idle") {
            const sessionID = event.properties?.sessionID as string | undefined;
            if (sessionID && sessionID === activeSessionID) {
              const session = await fetchSession(sessionID);
              if (session) update({ session });
            }
          }
        }
      : undefined,
    tool: {
      envoy_subscribe: tool({
        description: "Subscribe this session to envoy notification topics",
        args: {
          topics: tool.schema
            .array(tool.schema.string())
            .describe("NATS-style topic patterns to subscribe to"),
        },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy subscribe" });
          return call("/v1/interests/subscribe", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              session_id: ctx.sessionID,
              dir: ctx.directory,
              topics: args.topics,
            }),
          });
        },
      }),
      envoy_unsubscribe: tool({
        description: "Unsubscribe this session from envoy topics, or all if omitted",
        args: {
          topics: tool.schema
            .array(tool.schema.string())
            .optional()
            .describe("Topics to remove, or omit to remove all"),
        },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy unsubscribe" });
          return call("/v1/interests/unsubscribe", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              session_id: ctx.sessionID,
              topics: args.topics ?? [],
            }),
          });
        },
      }),
      envoy_list: tool({
        description: "List envoy subscriptions for this session",
        args: {},
        async execute(_args, ctx) {
          ctx.metadata({ title: "Envoy list" });
          return call(`/v1/interests/${ctx.sessionID}`);
        },
      }),
      envoy_send: tool({
        description: "Send an envoy agent-to-agent message to another session",
        args: {
          target_session: tool.schema.string().describe("Target session ID"),
          message: tool.schema.string().describe("Message to deliver"),
        },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy send" });
          return call("/v1/messages/send", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              source_session: ctx.sessionID,
              target_session: args.target_session,
              message: args.message,
            }),
          });
        },
      }),
    },
  };
};
