import { agentSubject } from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import { machineID } from "@legion/envoy-client/machine";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { createEnvoyClient } from "@legion/envoy-client/transport";
import { tool } from "@opencode-ai/plugin/tool";
import { loadEnvoyConfig } from "./config";
import { buildDispatchMcpEntry, injectEnvoyMcp } from "./dispatch-mcp";
import { dispatchSubscriptionTopic } from "./dispatch-subscribe";
import { logger } from "./log";
import { resolvePort } from "./port";

const [
  subscribeSpec,
  unsubscribeSpec,
  listSpec,
  sendSpec,
  publishSpec,
  roleSetSpec,
  whoamiSpec,
  sessionsSpec,
] = envoyToolSpecs;

export default async (input: { serverUrl: URL }) => {
  const cwd = process.cwd();
  const config = await loadEnvoyConfig(cwd);
  const envoyDefaults = envoyDefaultsFromEnvironment(process.env);
  const envoy = createEnvoyClient({ baseUrl: envoyDefaults.envoyUrl, fetch: globalThis.fetch });
  let activeSessionID: string | null = null;
  let activeSessionTitle: string | null = null;
  // All sessions that have become busy in this serve instance. The heartbeat
  // refreshes the envoy_sessions TTL for ALL of them — a single serve hosts many
  // sessions, so tracking only the most-recently-active one lets idle siblings
  // expire out of the registry and become undeliverable.
  const trackedSessions = new Map<string, { title: string | null; driving: boolean }>();
  /** Cached port — resolved asynchronously, null until first successful resolution. */
  let resolvedPort: number | null = null;

  let portWarningLogged = false;
  const refreshPort = async (): Promise<number | null> => {
    const port = await resolvePort(input.serverUrl);
    if (!port && !portWarningLogged) {
      portWarningLogged = true;
      logger.error(
        [
          `[envoy-plugin] Could not resolve serve port: serverUrl=${input.serverUrl.href},`,
          `pid=${process.pid}`,
        ].join(" ")
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

  /** Fetch session title from the OpenCode serve API. Best-effort — returns null on failure. */
  const fetchTitle = async (sessionID: string): Promise<string | null> => {
    try {
      const res = await fetch(`${input.serverUrl.href}session/${sessionID}`, {
        signal: AbortSignal.timeout(5_000),
      });
      if (!res.ok) return null;
      const data = (await res.json()) as { title?: string };
      return data.title ?? null;
    } catch {
      return null;
    }
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

  // driving = this process is the one running the session (it has gone busy
  // here), as opposed to a sibling re-adopted from shared on-disk state after a
  // serve restart. Envoy uses it to keep a live driver's route from being stolen
  // by another process that merely holds the same session.
  const subscribeSession = (
    sessionID: string,
    title: string | null,
    port: number,
    driving: boolean
  ) =>
    envoy
      .subscribe({
        sessionID,
        directory: cwd,
        topics: [agentSubject(sessionID)],
        port,
        title: title ?? "",
        driving,
      })
      .catch(() => {});

  // A process registers ONLY the sessions it has actually run (see the busy
  // handler below). It must never claim a route for a session it merely has
  // loaded from shared on-disk state: doing so re-points that session's route
  // here, envoy delivers here, and this process starts a second model loop on a
  // session another process is driving.
  //
  // That means a session whose process is gone stays unreachable until it runs
  // again. Keeping dispatched-but-idle workers reachable is the daemon's job — it
  // knows the serve port and the session IDs it dispatched — not something a
  // stranger process may arrange by adopting routes.

  // Heartbeat: re-subscribe every tracked session to refresh the envoy_sessions
  // TTL (5-min). Refreshes ALL sessions that have been busy in this serve, not
  // just the most recently active one. Interval is env-tunable for tests/tuning.
  const heartbeatMs = envoyDefaults.heartbeatMs;
  const heartbeatInterval = setInterval(() => {
    const port = currentPort();
    if (!port) return;
    for (const [sessionID, info] of trackedSessions) {
      subscribeSession(sessionID, info.title, port, info.driving);
    }
  }, heartbeatMs);
  heartbeatInterval.unref?.();

  process.on("exit", () => {
    clearInterval(timer);
    clearInterval(heartbeatInterval);
  });

  return {
    config: (cfg: { mcp?: Record<string, unknown> } & Record<string, unknown>) => {
      // Inject the envoy MCP entry into the OpenCode config when
      // dispatch is enabled. Centralizing this in the plugin (instead of
      // each user's opencode.json) means:
      //   1. Registration is gated by `dispatch.enabled`
      //   2. The bearer token is sourced per-CWD via the user's gh shim
      //      (no env coordination needed)
      //   3. Token rotation happens transparently inside the shim
      //      subprocess — OpenCode never sees an expired token
      const entry = buildDispatchMcpEntry({
        dispatch: config.dispatch,
      });
      if (!entry) return;
      const { warning } = injectEnvoyMcp(cfg, entry);
      if (warning) logger.warn(warning);
    },
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
          activeSessionTitle = null;
          if (!trackedSessions.has(sessionID)) {
            trackedSessions.set(sessionID, { title: null, driving: true });
          }
          await syncPort();
          const port = currentPort();
          // Fetch title — best-effort, non-blocking for initial subscribe
          const titlePromise = fetchTitle(sessionID).then((t) => {
            // Guard: only update if this session is still active (prevents race on fast switches)
            if (t && activeSessionID === sessionID) activeSessionTitle = t;
            return t;
          });
          if (port) {
            await subscribeSession(sessionID, activeSessionTitle, port, true);
            // After the title arrives, send one follow-up subscribe with it.
            titlePromise.then((title) => {
              if (!title) return;
              // Update tracked metadata even if this session is no longer the
              // active one (another session may have become busy meanwhile).
              if (trackedSessions.has(sessionID)) {
                const driving = trackedSessions.get(sessionID)?.driving ?? true;
                trackedSessions.set(sessionID, { title, driving });
                subscribeSession(sessionID, title, currentPort() ?? 0, driving);
              }
              if (activeSessionID === sessionID) activeSessionTitle = title;
            });
          }
        }
      }

      if (event.type === "session.deleted") {
        const props = event.properties ?? {};
        const deletedID =
          (props.sessionID as string | undefined) ??
          (props.info as { id?: string } | undefined)?.id;
        if (deletedID) {
          // Stop heartbeating a session that no longer exists, so its 5-min
          // envoy_sessions entry expires instead of being kept alive (which would
          // cause delivery attempts to a dead session id on the current port).
          trackedSessions.delete(deletedID);
          if (activeSessionID === deletedID) {
            activeSessionID = null;
            activeSessionTitle = null;
          }
          // Best-effort: drop the deleted session's interests so routing stops.
          envoy.unsubscribe({ sessionID: deletedID, topics: [] }).catch(() => {});
        }
      }
    },
    "tool.execute.after": async (
      input: { tool: string; sessionID: string; callID: string; args: unknown },
      output: { title: string; output: string; metadata: unknown }
    ) => {
      // When this session opens a Dispatch thread via the envoy_dispatch MCP
      // tool, auto-subscribe it to the thread's GitHub topic so the human's
      // reply is delivered back through Envoy. Best-effort — a subscribe
      // failure must never surface to the model or fail the tool call.
      const topic = dispatchSubscriptionTopic(input.tool, output.output);
      if (!topic) return;
      try {
        await envoy.subscribe({
          sessionID: input.sessionID,
          directory: cwd,
          topics: [topic],
          port: currentPort() ?? 0,
          title: activeSessionTitle ?? "",
          driving: true,
        });
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        logger.warn(`[envoy-plugin] dispatch auto-subscribe failed: ${message}`);
      }
    },
    // Cleanup hook (used by tests; production relies on process 'exit').
    dispose: () => {
      clearInterval(timer);
      clearInterval(heartbeatInterval);
    },
    tool: {
      envoy_subscribe: tool({
        description: subscribeSpec.description,
        args: { topics: tool.schema.array(tool.schema.string()) },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy subscribe" });
          return JSON.stringify(
            await envoy.subscribe({
              sessionID: ctx.sessionID,
              directory: ctx.directory,
              topics: args.topics,
              port: currentPort() ?? 0,
              title: activeSessionTitle ?? "",
              driving: true,
            })
          );
        },
      }),
      envoy_unsubscribe: tool({
        description: unsubscribeSpec.description,
        args: { topics: tool.schema.array(tool.schema.string()).optional() },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy unsubscribe" });
          await envoy.unsubscribe({ sessionID: ctx.sessionID, topics: args.topics ?? [] });
          return "ok";
        },
      }),
      envoy_list: tool({
        description: listSpec.description,
        args: {},
        async execute(_args, ctx) {
          ctx.metadata({ title: "Envoy list" });
          return JSON.stringify(await envoy.getInterest(ctx.sessionID));
        },
      }),
      envoy_send: tool({
        description: sendSpec.description,
        args: { session_id: tool.schema.string(), message: tool.schema.string() },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy send" });
          return JSON.stringify(
            await envoy.send({
              sourceSessionID: ctx.sessionID,
              targetSessionID: args.session_id,
              message: args.message,
            })
          );
        },
      }),
      envoy_publish: tool({
        description: publishSpec.description,
        args: { topic: tool.schema.string(), message: tool.schema.string() },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy publish" });
          return JSON.stringify(
            await envoy.publish({
              sourceSessionID: ctx.sessionID,
              topic: args.topic,
              message: args.message,
            })
          );
        },
      }),
      envoy_role_set: tool({
        description: roleSetSpec.description,
        args: { role: tool.schema.string() },
        async execute(args, ctx) {
          ctx.metadata({ title: "Set Envoy role" });
          return JSON.stringify(await envoy.setRole({ sessionID: ctx.sessionID, role: args.role }));
        },
      }),
      envoy_whoami: tool({
        description: whoamiSpec.description,
        args: {},
        async execute(_args, ctx) {
          ctx.metadata({ title: "Envoy whoami" });
          const sessionID = ctx.sessionID;
          const port = currentPort();
          return JSON.stringify(
            {
              session_id: sessionID,
              machine_id: machineID(),
              port,
              dir: ctx.directory,
            },
            null,
            2
          );
        },
      }),
      envoy_sessions: tool({
        description: sessionsSpec.description,
        args: { machine: tool.schema.string().optional() },
        async execute(args, ctx) {
          ctx.metadata({ title: "Envoy sessions" });
          const sessions = await envoy.listSessions();
          return JSON.stringify(
            args.machine
              ? sessions.filter((session) => session.machine_id === args.machine)
              : sessions,
            null,
            2
          );
        },
      }),
    },
  };
};
