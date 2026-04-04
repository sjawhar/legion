import { execFileSync } from "node:child_process";
import { readFileSync, unlinkSync, writeFileSync } from "node:fs";
import { tool } from "@opencode-ai/plugin/tool";

const root = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";

async function call(path: string, init?: RequestInit) {
  const res = await fetch(`${root}${path}`, init);
  const text = await res.text();
  if (!res.ok) throw new Error(text || `${res.status}`);
  return text;
}

export default async (input: { serverUrl: URL }) => {
  const registryDir = process.env.OC_REGISTRY;
  let activeSessionID: string | null = null;
  let _activeFile: string | null = null;

  const registryFile = (sessionID: string) =>
    registryDir ? `${registryDir}/${sessionID}.json` : null;

  const update = (sessionID: string, patch: Record<string, unknown>) => {
    const file = registryFile(sessionID);
    if (!file) return;
    try {
      let data: Record<string, unknown> = {};
      try {
        data = JSON.parse(readFileSync(file, "utf-8"));
      } catch {
        data = { pid: process.pid, dir: process.cwd(), started: new Date().toISOString() };
      }
      Object.assign(data, patch);
      writeFileSync(file, `${JSON.stringify(data)}\n`);
      registryFiles.add(file);
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

  const syncPort = (sessionID?: string) => {
    const value = currentPort();
    if (!value) return false;
    if (sessionID) update(sessionID, { port: value });
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

  // Track all registry files this process created, clean up on exit
  const registryFiles = new Set<string>();

  if (registryDir) {
    syncPort();
    const timer = setInterval(() => {
      if (syncPort(activeSessionID ?? undefined)) clearInterval(timer);
    }, 1000);

    // Heartbeat: re-subscribe every 2 minutes to refresh envoy_sessions TTL (5-min)
    const heartbeatInterval = setInterval(
      () => {
        if (!activeSessionID) return;
        const port = currentPort();
        if (!port) return;
        call("/v1/interests/subscribe", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            session_id: activeSessionID,
            dir: process.cwd(),
            topics: [`notifications.agent.${activeSessionID}`],
            port,
          }),
        }).catch(() => {});
      },
      2 * 60 * 1000
    );

    process.on("exit", () => {
      clearInterval(heartbeatInterval);
      for (const f of registryFiles) {
        try {
          unlinkSync(f);
        } catch {}
      }
    });
  }

  return {
    event: registryDir
      ? async ({ event }: { event: { type?: string; properties?: Record<string, unknown> } }) => {
          if (activeSessionID) syncPort(activeSessionID);

          if (
            event.type === "session.status" &&
            (event.properties?.status as { type?: string } | undefined)?.type === "busy"
          ) {
            const sessionID = event.properties?.sessionID as string | undefined;
            if (sessionID && sessionID !== activeSessionID) {
              activeSessionID = sessionID;
              _activeFile = registryFile(sessionID);
              const session = await fetchSession(sessionID);
              if (session) update(sessionID, { session });
              syncPort(sessionID);
              call("/v1/interests/subscribe", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({
                  session_id: sessionID,
                  dir: process.cwd(),
                  topics: [`notifications.agent.${sessionID}`],
                  port: currentPort() ?? 0,
                }),
              }).catch(() => {});
            }
          }
          if (event.type === "session.updated") {
            const info = event.properties?.info as { id?: string; title?: string } | undefined;
            if (info && info.id === activeSessionID) {
              update(info.id, { session: { id: info.id, title: info.title || "" } });
            }
          }

          if (event.type === "session.idle") {
            const sessionID = event.properties?.sessionID as string | undefined;
            if (sessionID && sessionID === activeSessionID) {
              const session = await fetchSession(sessionID);
              if (session) update(sessionID, { session });
            }
          }
        }
      : undefined,
    tool: {
      envoy_subscribe: tool({
        description:
          "Subscribe this session to Envoy notification topics. Use exact NATS-style topic strings such as notifications.agent.<session_id>, notifications.github.<owner>.<repo>.pr, notifications.github.<owner>.<repo>.issue, notifications.github.<owner>.<repo>.comment, notifications.github.<owner>.<repo>.ci, notifications.slack.<team_id>.<channel_id>.message, or notifications.slack.<team_id>.<channel_id>.mention. Use this when a session should RECEIVE future events.",
        args: {
          topics: tool.schema
            .array(tool.schema.string())
            .describe(
              "NATS-style topic patterns to subscribe to. Examples: notifications.agent.ses_123, notifications.github.trajectory-labs-pbc.agent-c.pr, notifications.slack.T09FRELLTS8.C0A0DHVU8HE.mention"
            ),
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
              port: Number.parseInt(input.serverUrl.port, 10) || 0,
            }),
          });
        },
      }),
      envoy_unsubscribe: tool({
        description:
          "Unsubscribe this session from Envoy topics, or remove all current subscriptions if topics are omitted.",
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
        description:
          "List the current Envoy topic subscriptions for this session so you can confirm the exact topic shapes that are active.",
        args: {},
        async execute(_args, ctx) {
          ctx.metadata({ title: "Envoy list" });
          return call(`/v1/interests/${ctx.sessionID}`);
        },
      }),
      envoy_send: tool({
        description:
          "Send an Envoy agent-to-agent message directly to another session by session ID. Use this for coordination between agents or to notify a known controller/worker session. This is for SEND, not subscription.",
        args: {
          target_session: tool.schema
            .string()
            .describe("Target OpenCode session ID, e.g. ses_2e6ca3034ffejVikSZ8mDwk0mR"),
          message: tool.schema
            .string()
            .describe("Message body to deliver to that session as a new user turn/notification"),
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
      envoy_publish: tool({
        description:
          "Publish an Envoy message to any topic. Use for broadcast to named topics like notifications.legion.controller, team channels, or custom routing. Subscribers matching the topic will receive the message. This is for BROADCAST, not session-targeted delivery (use envoy_send for that).",
        args: {
          topic: tool.schema
            .string()
            .describe("NATS-style topic to publish to, e.g. notifications.legion.controller"),
          message: tool.schema.string().describe("Message body to broadcast"),
        },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy publish" });
          return call("/v1/messages/publish", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              source_session: ctx.sessionID,
              topic: args.topic,
              message: args.message,
            }),
          });
        },
      }),
    },
  };
};
