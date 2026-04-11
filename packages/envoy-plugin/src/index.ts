import { tool } from "@opencode-ai/plugin/tool";
import { resolvePort } from "./port";

const root = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";

/** HTTP timeout for Envoy calls — prevent hanging when NATS/Envoy is unavailable. */
const CALL_TIMEOUT_MS = 5_000;

async function call(path: string, init?: RequestInit) {
  const res = await fetch(`${root}${path}`, {
    ...init,
    signal: init?.signal ?? AbortSignal.timeout(CALL_TIMEOUT_MS),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(text || `${res.status}`);
  return text;
}

export default async (input: { serverUrl: URL }) => {
  let activeSessionID: string | null = null;
  /** Cached port — resolved asynchronously, null until first successful resolution. */
  let resolvedPort: number | null = null;

  let portWarningLogged = false;
  const refreshPort = async (): Promise<number | null> => {
    const port = await resolvePort(input.serverUrl);
    if (!port && !portWarningLogged) {
      portWarningLogged = true;
      console.error(
        `[envoy-plugin] Could not resolve serve port: serverUrl=${input.serverUrl.href}, pid=${process.pid}`
      );
    }
    if (port) {
      portWarningLogged = false;
      resolvedPort = port;
    }
    return port;
  };

  /** Return cached port synchronously — tools need a port value inline. */
  const currentPort = () => resolvedPort;

  const syncPort = async (): Promise<boolean> => {
    const value = await refreshPort();
    return value !== null;
  };

  // Defer port resolution to background — never block plugin init.
  // The port sync loop retries every second until resolved.
  const timer = setInterval(() => {
    syncPort().then((resolved) => {
      if (resolved) clearInterval(timer);
    });
  }, 1000);
  timer.unref(); // Don't prevent graceful shutdown if port never resolves
  // Fire an immediate async attempt (non-blocking)
  syncPort().catch(() => {});

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
    clearInterval(timer);
    clearInterval(heartbeatInterval);
  });

  return {
    event: async ({
      event,
    }: {
      event: { type?: string; properties?: Record<string, unknown> };
    }) => {
      if (activeSessionID) syncPort().catch(() => {});

      if (
        event.type === "session.status" &&
        (event.properties?.status as { type?: string } | undefined)?.type === "busy"
      ) {
        const sessionID = event.properties?.sessionID as string | undefined;
        if (sessionID && sessionID !== activeSessionID) {
          activeSessionID = sessionID;
          await syncPort();
          const port = currentPort();
          if (port) {
            call("/v1/interests/subscribe", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({
                session_id: sessionID,
                dir: process.cwd(),
                topics: [`notifications.agent.${sessionID}`],
                port,
              }),
            }).catch(() => {});
          }
        }
      }
    },
    tool: {
      envoy_subscribe: tool({
        description:
          "Subscribe this session to Envoy notification topics. GitHub topics are resource-scoped: notifications.github.<owner>.<repo>.pr.<number>, notifications.github.<owner>.<repo>.issue.<number>.comment, etc. Use NATS wildcards for broad subscriptions: notifications.github.<owner>.<repo>.pr.> (all PR events). Other topics: notifications.agent.<session_id>, notifications.slack.<team_id>.<channel_id>.message, notifications.slack.<team_id>.<channel_id>.mention. Use this when a session should RECEIVE future events.",
        args: {
          topics: tool.schema
            .array(tool.schema.string())
            .describe(
              "NATS-style topic patterns to subscribe to. GitHub topics include resource number: notifications.github.owner.repo.pr.123 (PR state), notifications.github.owner.repo.pr.123.comment (PR comments), notifications.github.owner.repo.issue.456.> (all events on issue). Use > wildcard for broad matching. Other examples: notifications.agent.ses_123, notifications.slack.T09FRELLTS8.C0A0DHVU8HE.mention"
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
              port: currentPort() ?? 0,
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
          "Publish an Envoy message to any topic. Use for broadcast to named topics like notifications.role.legion-controller, team channels, or custom routing. Subscribers matching the topic will receive the message. This is for BROADCAST, not session-targeted delivery (use envoy_send for that).",
        args: {
          topic: tool.schema
            .string()
            .describe("NATS-style topic to publish to, e.g. notifications.role.legion-controller"),
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
      envoy_role_set: tool({
        description:
          "Set the current session as the holder of a named role. Messages published to notifications.role.<role> will route to this session. Only one session holds a role at a time — claiming it removes it from the previous holder.",
        args: {
          role: tool.schema
            .string()
            .describe(
              "Role name to claim (lowercase alphanumeric, hyphens, underscores). E.g. opencode-dev, legion-controller, legion-po"
            ),
        },
        async execute(args, ctx) {
          ctx.metadata({ title: "Set Envoy role" });
          return call("/v1/roles/set", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              session_id: ctx.sessionID,
              role: args.role,
            }),
          });
        },
      }),
      envoy_whoami: tool({
        description:
          "Returns this session's Envoy identity: session ID, machine ID, port, directory, and current topic subscriptions.",
        args: {},
        async execute(_args, ctx) {
          ctx.metadata({ title: "Envoy whoami" });
          const sessionID = ctx.sessionID;
          const port = currentPort();
          let topics: string[] = [];
          try {
            const res = await call(`/v1/interests/${sessionID}`);
            const data = JSON.parse(res);
            topics = data.topics || [];
          } catch {}
          return JSON.stringify(
            {
              session_id: sessionID,
              machine_id: process.env.HOSTNAME || "unknown",
              port,
              dir: ctx.directory,
              topics,
            },
            null,
            2
          );
        },
      }),
      envoy_sessions: tool({
        description:
          "List all live sessions registered with Envoy. Returns session ID, machine ID, port, directory, topics, and last-seen timestamp for each. Use the optional machine filter to show only sessions on a specific host.",
        args: {
          machine: tool.schema
            .string()
            .optional()
            .describe(
              "Filter to sessions on this machine ID (e.g. hostname). Omit to list all machines."
            ),
        },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy sessions" });
          const res = await call("/v1/sessions");
          if (!args.machine) return res;
          const sessions = JSON.parse(res) as Array<{
            machine_id: string;
          }>;
          return JSON.stringify(
            sessions.filter((s) => s.machine_id === args.machine),
            null,
            2
          );
        },
      }),
    },
  };
};
