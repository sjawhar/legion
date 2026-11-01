import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { agentSubject, zodSchemaApi } from "@legion/contracts";
import { dispatchFirstSkillFile } from "@legion/envoy-client/dispatch-first";
import { machineID } from "@legion/envoy-client/machine";
import { sessionConfigFile, sessionSettings } from "@legion/envoy-client/session-settings";
import {
  envoyToolSpecs,
  sendConfirmationText,
  type ToolSpec,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract";
import { createEnvoyClient, type EnvoyClient } from "@legion/envoy-client/transport";
import { type ToolDefinition, tool } from "@opencode-ai/plugin/tool";
import { version } from "../package.json" with { type: "json" };
import { logger } from "./log";
import { resolvePort } from "./port";

const moduleDirectory = path.dirname(fileURLToPath(import.meta.url));
// Legion skills ship inside this package so OpenCode carries them without a
// vendor checkout. OpenCode discovers skills only from fixed directories and
// `config.skills.paths` — never from plugin package dirs — so the config hook
// below must inject this path itself.
//   Published tarball: dist/src/server.js → <pkg>/skills (staged at prepack).
//   Repo checkout:     src/server.ts      → <repo>/skills.
const skillsDirectory = [
  path.resolve(moduleDirectory, "../../skills"),
  path.resolve(moduleDirectory, "../../../skills"),
].find((dir) => existsSync(dir));

const PACKAGE_NAME = "@sjawhar/opencode-legion-envoy";

/** The plugin every registration names: this package and the version its manifest carries. */
const PLUGIN = { name: PACKAGE_NAME, version };

/** The package root: the nearest directory above `from` whose package.json names this package.
 *  The packed server.js sits in dist/src/, the repo's server.ts in src/. A manifest on the way up
 *  that is missing, unreadable or not JSON is passed over. */
function packageRoot(from: string): string {
  for (let directory = from; ; directory = path.dirname(directory)) {
    if (manifestName(path.join(directory, "package.json")) === PACKAGE_NAME) return directory;
    if (path.dirname(directory) === directory) {
      throw new Error(`envoy: no ${PACKAGE_NAME} package.json above ${from}`);
    }
  }
}

/** A manifest's `name`, or undefined when the file is missing, unreadable or not JSON. */
function manifestName(manifest: string): unknown {
  try {
    const parsed: unknown = JSON.parse(readFileSync(manifest, "utf8"));
    return typeof parsed === "object" && parsed !== null && "name" in parsed
      ? parsed.name
      : undefined;
  } catch {
    return undefined;
  }
}

/** Holds the `dispatch` shim the `shell.env` hook puts first on every shell command's PATH. */
const PLUGIN_BIN = path.join(packageRoot(moduleDirectory), "bin");

/** A spec by its name, typed so a misspelled name fails `tsc` rather than the plugin's load. */
function toolSpec(name: (typeof envoyToolSpecs)[number]["name"]): ToolSpec {
  const spec = envoyToolSpecs.find((candidate) => candidate.name === name);
  if (spec === undefined) throw new Error(`envoy tool contract has no ${name}`);
  return spec;
}

const subscribeSpec = toolSpec("envoy_subscribe");
const unsubscribeSpec = toolSpec("envoy_unsubscribe");
const listSpec = toolSpec("envoy_list");
const sendSpec = toolSpec("envoy_send");
const publishSpec = toolSpec("envoy_publish");
const roleSetSpec = toolSpec("envoy_role_set");
const whoamiSpec = toolSpec("envoy_whoami");
const sessionsSpec = toolSpec("envoy_sessions");

/** The Envoy tools, on the listener `client` reaches: what OpenCode registers while Envoy is on.
 * `port` and `title` are read at each call, since both arrive after the plugin loads. */
function envoyTools(
  client: EnvoyClient,
  port: () => number | null,
  title: () => string | null
): Record<string, ToolDefinition> {
  return {
    envoy_subscribe: tool({
      description: subscribeSpec.description,
      args: subscribeSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(args, ctx) {
        ctx.metadata({ title: "Envoy subscribe" });
        return JSON.stringify(
          await client.subscribe({
            sessionID: ctx.sessionID,
            directory: ctx.directory,
            topics: args.topics as string[],
            port: port() ?? 0,
            title: title() ?? "",
            driving: true,
            plugin: PLUGIN,
          })
        );
      },
    }),
    envoy_unsubscribe: tool({
      description: unsubscribeSpec.description,
      args: unsubscribeSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(args, ctx) {
        ctx.metadata({ title: "Envoy unsubscribe" });
        await client.unsubscribe({
          sessionID: ctx.sessionID,
          topics: (args.topics as string[] | undefined) ?? [],
        });
        return "ok";
      },
    }),
    envoy_list: tool({
      description: listSpec.description,
      args: listSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(_args, ctx) {
        ctx.metadata({ title: "Envoy list" });
        return JSON.stringify(await client.getInterest(ctx.sessionID));
      },
    }),
    envoy_send: tool({
      description: sendSpec.description,
      args: sendSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(args, ctx) {
        ctx.metadata({ title: "Envoy send" });
        const result = await client.send({
          sourceSessionID: ctx.sessionID,
          targetSessionID: args.session_id as string,
          message: args.message as string,
          ...toMessageMetadata(args as never),
        });
        return JSON.stringify({
          message: sendConfirmationText(result),
          event_id: result.envelope.event_id,
          recipient: result.recipient,
          recipient_state: result.recipientState,
          confirmed: result.confirmed,
        });
      },
    }),
    envoy_publish: tool({
      description: publishSpec.description,
      args: publishSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(args, ctx) {
        ctx.metadata({ title: "Envoy publish" });
        return JSON.stringify(
          await client.publish({
            sourceSessionID: ctx.sessionID,
            topic: args.topic as string,
            message: args.message as string,
            ...toMessageMetadata(args as never),
          })
        );
      },
    }),
    envoy_role_set: tool({
      description: roleSetSpec.description,
      args: roleSetSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(args, ctx) {
        ctx.metadata({ title: "Set Envoy role" });
        const result = await client.setRole({
          sessionID: ctx.sessionID,
          role: args.role as string,
        });
        // A hard claim always lands; the discriminant exists for soft claims.
        if (!result.claimed) throw new Error(`role ${args.role} is held by ${result.holder}`);
        return JSON.stringify(result.interest);
      },
    }),
    envoy_whoami: tool({
      description: whoamiSpec.description,
      args: whoamiSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(_args, ctx) {
        ctx.metadata({ title: "Envoy whoami" });
        return JSON.stringify(
          {
            session_id: ctx.sessionID,
            machine_id: machineID(),
            port: port(),
            dir: ctx.directory,
          },
          null,
          2
        );
      },
    }),
    envoy_sessions: tool({
      description: sessionsSpec.description,
      args: sessionsSpec.arguments(zodSchemaApi(tool.schema)) as never,
      async execute(args, ctx) {
        ctx.metadata({ title: "Envoy sessions" });
        // The stalled sessions are asked for by name: a peer that missed its heartbeats is still
        // running, and a list that leaves it out is read as the peer having died.
        const sessions = await client.listSessions({
          ...(args.dir === undefined ? {} : { directory: args.dir as string }),
          ...(args.title === undefined ? {} : { title: args.title as string }),
          include: "unresponsive",
        });
        const machine = args.machine as string | undefined;
        return JSON.stringify(
          machine ? sessions.filter((session) => session.machine_id === machine) : sessions,
          null,
          2
        );
      },
    }),
  };
}

export default async (input: { serverUrl: URL }) => {
  const cwd = process.cwd();
  // The serve's envoy.json, by OpenCode's rule: `$OPENCODE_CONFIG_DIR/envoy.json`, else the file
  // ENVOY_CONFIG_FILE names, else the neutral file; every shell command is handed the same file
  // (`shell.env`). Dispatch and Envoy are each resolved on their own from it, and why a section is
  // off is logged once. Neither section, nor a file location that cannot be worked out, stops the
  // plugin from loading. OpenCode opens no NATS connection, so no NATS server or seed is resolved.
  const configFile = sessionConfigFile("opencode", process.env);
  const { dispatch, envoy, notice } = sessionSettings(configFile, process.env, {
    cwd,
    nats: false,
  });
  if (notice !== null) logger.warn(`envoy: ${notice}`);
  // With Dispatch configured, every session carries the dispatch-first skill as an instruction
  // file: OpenCode reads instruction files into the main loop's system prompt on every request
  // and leaves them out of title and compaction requests. A package without the skill fails
  // here, naming the file, rather than serving sessions that silently lack it.
  let dispatchFirst: string | undefined;
  if (dispatch.enabled) {
    if (skillsDirectory === undefined) {
      throw new Error(`envoy: no skills directory beside ${moduleDirectory}`);
    }
    dispatchFirst = dispatchFirstSkillFile(skillsDirectory);
    if (!existsSync(dispatchFirst)) {
      throw new Error(`envoy: the dispatch-first skill ${dispatchFirst} is missing`);
    }
  }
  // The listener client exists only while Envoy is on: without it no tool is registered, nothing
  // registers a session, and no listener is reached.
  const envoyOn = envoy.enabled
    ? {
        heartbeatMs: envoy.heartbeatMs,
        client: createEnvoyClient({
          baseUrl: envoy.url,
          fetch: globalThis.fetch,
          token: envoy.token,
        }),
      }
    : undefined;
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

  /**
   * Does the serve still hold this session? `true` when it answers for it, `false` when it
   * answers 404 — the answer for a session OpenCode has deleted — and `undefined` when the read
   * did not answer at all, which is no reason to take a registration away.
   *
   * OpenCode's `session.deleted` never reaches a plugin's `event` hook: the hook is fed from the
   * plain bus (`session.status`, `session.idle`, `session.diff`, `message.part.delta`), and the
   * session aggregate's own events — `session.created`, `session.updated`, `session.deleted` —
   * are published on a stream it does not carry (OpenCode 1.14.46). A serve that waited for that
   * event would heartbeat a deleted session's id for the whole live window and then the whole
   * gone horizon, which is deliveries to a dead id on this port for all of it
   * (dispatch://INFRA-392), so the heartbeat asks the serve itself instead.
   */
  const sessionHeld = async (sessionID: string): Promise<boolean | undefined> => {
    try {
      const res = await fetch(`${input.serverUrl.href}session/${sessionID}`, {
        signal: AbortSignal.timeout(5_000),
      });
      if (res.status === 404) return false;
      return res.ok ? true : undefined;
    } catch {
      return undefined;
    }
  };

  /**
   * The registration on the wire for each served session. A `DELETE /v1/sessions/<id>` that
   * overtakes one is undone when it lands: the listener writes the record again, and a session
   * nothing heartbeats stays registered — live for the live window, then unresponsive for the
   * gone horizon — so peers keep sending to a serve that has stopped (dispatch://INFRA-392).
   */
  const registrationsInFlight = new Map<string, Promise<void>>();

  // driving = this process is the one running the session (it has gone busy
  // here), as opposed to a sibling re-adopted from shared on-disk state after a
  // serve restart. Envoy uses it to keep a live driver's route from being stolen
  // by another process that merely holds the same session.
  const subscribeSession = (
    client: EnvoyClient,
    sessionID: string,
    title: string | null,
    port: number,
    driving: boolean
  ) => {
    const registering = client
      .subscribe({
        sessionID,
        directory: cwd,
        topics: [agentSubject(sessionID)],
        port,
        title: title ?? "",
        driving,
        plugin: PLUGIN,
      })
      .then(
        () => undefined,
        () => undefined
      );
    registrationsInFlight.set(sessionID, registering);
    void registering.then(() => {
      if (registrationsInFlight.get(sessionID) === registering) {
        registrationsInFlight.delete(sessionID);
      }
    });
    return registering;
  };

  /** How long a deregistration waits for that session's registration to leave the wire; past it
   *  the DELETE goes out anyway, since one never sent always leaves the record behind where one
   *  that races a slow registration only may. */
  const REGISTRATION_SETTLE_MS = 5_000;

  /** Deregisters a session this serve no longer heartbeats: one OpenCode deleted, and every
   *  session it still holds when the process is asked to stop. */
  const deregisterSession = async (client: EnvoyClient, sessionID: string): Promise<void> => {
    const registering = registrationsInFlight.get(sessionID);
    if (registering !== undefined) {
      const { promise, resolve } = Promise.withResolvers<void>();
      const timer = setTimeout(resolve, REGISTRATION_SETTLE_MS);
      try {
        await Promise.race([registering, promise]);
      } finally {
        clearTimeout(timer);
      }
    }
    await client.unregisterSession({ sessionID }).catch(() => {});
  };

  /** Stops heartbeating a session this serve no longer holds and takes its registration out of
   *  the registry now rather than leaving it to expire: an entry nothing heartbeats reads as live
   *  for the live window and as unresponsive for the gone horizon after it, which is deliveries
   *  to a dead session id on this port for the whole of it (dispatch://INFRA-392). The DELETE
   *  goes out behind that session's own registration. Both ways a session ends on a running serve
   *  come here: OpenCode's own `session.deleted`, where the host hands it over, and the
   *  heartbeat's own read of the serve, which is the one that answers for a delete (sessionHeld).
   */
  const forgetSession = async (client: EnvoyClient, sessionID: string): Promise<void> => {
    trackedSessions.delete(sessionID);
    if (activeSessionID === sessionID) {
      activeSessionID = null;
      activeSessionTitle = null;
    }
    // Best-effort: drop the session's interests so routing stops.
    await client.unsubscribe({ sessionID, topics: [] }).catch(() => {});
    await deregisterSession(client, sessionID);
  };

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

  // Envoy's background work, started only while Envoy is on, and what stops it.
  let stopEnvoy = (): void => {};
  if (envoyOn !== undefined) {
    // Defer port resolution to background — never block plugin init.
    // The port sync loop retries every second until resolved.
    const portTimer = setInterval(() => {
      syncPort().then((resolved) => {
        if (resolved) clearInterval(portTimer);
      });
    }, 1000);
    portTimer.unref(); // Don't prevent graceful shutdown if port never resolves
    // Fire an immediate async attempt (non-blocking)
    syncPort().catch(() => {});

    // Heartbeat: re-subscribe every tracked session to refresh the envoy_sessions
    // TTL (5-min). Refreshes ALL sessions that have been busy in this serve, not
    // just the most recently active one. Interval is env-tunable for tests/tuning.
    //
    // It is also where a session OpenCode has deleted is noticed, since the host hands the
    // plugin no `session.deleted` (sessionHeld): a session the serve no longer answers for is
    // forgotten and deregistered instead of heartbeated. One pass at a time — a pass that is
    // still reading the serve when the next tick comes is left to finish.
    let heartbeating = false;
    const heartbeatTimer = setInterval(() => {
      const port = currentPort();
      if (!port || heartbeating) return;
      heartbeating = true;
      void (async () => {
        try {
          for (const [sessionID, info] of [...trackedSessions]) {
            if ((await sessionHeld(sessionID)) === false) {
              await forgetSession(envoyOn.client, sessionID);
              continue;
            }
            subscribeSession(envoyOn.client, sessionID, info.title, port, info.driving);
          }
        } finally {
          heartbeating = false;
        }
      })();
    }, envoyOn.heartbeatMs);
    heartbeatTimer.unref?.();
    // A serve is stopped by a signal, and an `exit` handler may not be asynchronous, so the
    // session records it holds were left for the listener to expire: for the whole live window
    // peers read a stopped serve's sessions as live, and for the gone horizon after it as
    // unresponsive (dispatch://INFRA-392). Each signal stops the heartbeat and deregisters every
    // session this serve still holds, each behind that session's own registration, and only then
    // lets the signal take the process: it is re-raised with no listener of ours left, so the
    // default disposition and the exit code are the signal's own.
    const signals: readonly NodeJS.Signals[] = ["SIGTERM", "SIGINT", "SIGHUP"];
    const client = envoyOn.client;
    let stopping = false;
    stopEnvoy = () => {
      stopping = true;
      clearInterval(portTimer);
      clearInterval(heartbeatTimer);
      for (const name of signals) process.off(name, stopOnSignal);
    };
    function stopOnSignal(signal: NodeJS.Signals): void {
      if (stopping) return;
      stopEnvoy();
      const served = [...trackedSessions.keys()];
      trackedSessions.clear();
      void Promise.all(served.map((sessionID) => deregisterSession(client, sessionID))).then(() => {
        process.kill(process.pid, signal);
      });
    }
    for (const name of signals) process.on(name, stopOnSignal);
  }

  return {
    config: (
      cfg: { skills?: { paths?: string[] }; instructions?: string[] } & Record<string, unknown>
    ) => {
      // Serve the bundled legion skills to every session on this serve.
      if (skillsDirectory) {
        cfg.skills ??= {};
        cfg.skills.paths ??= [];
        if (!cfg.skills.paths.includes(skillsDirectory)) {
          cfg.skills.paths.push(skillsDirectory);
        }
      }
      if (dispatchFirst !== undefined) {
        cfg.instructions ??= [];
        if (!cfg.instructions.includes(dispatchFirst)) cfg.instructions.push(dispatchFirst);
      }
    },
    // The `dispatch` command in every shell command: first on PATH, handed the serve's envoy.json,
    // and told which OpenCode session runs it and that session's title.
    "shell.env": async (
      input: { cwd: string; sessionID?: string; callID?: string },
      output: { env: Record<string, string> }
    ) => {
      output.env.PATH = `${PLUGIN_BIN}${path.delimiter}${output.env.PATH ?? process.env.PATH ?? ""}`;
      output.env.DISPATCH_HOST = "opencode";
      if (configFile.path !== null) output.env.ENVOY_CONFIG_FILE = configFile.path;
      // OpenCode spreads output.env over its own process.env, so an absent key keeps the inherited value.
      output.env.DISPATCH_SESSION_ID = "";
      output.env.DISPATCH_SESSION_TITLE = "";
      if (input.sessionID) {
        output.env.DISPATCH_SESSION_ID = input.sessionID;
        const title = trackedSessions.get(input.sessionID)?.title;
        if (title) output.env.DISPATCH_SESSION_TITLE = title;
      }
    },
    event: async ({
      event,
    }: {
      event: { type?: string; properties?: Record<string, unknown> };
    }) => {
      if (envoy.enabled && activeSessionID) syncPort().catch(() => {});

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
          // The title names the session to the `dispatch` command (`shell.env`) and, while Envoy
          // is on, in its registration. Best-effort, and the first registration does not wait on it.
          const titlePromise = fetchTitle(sessionID).then((title) => {
            if (!title) return null;
            // Update tracked metadata even if this session is no longer the
            // active one (another session may have become busy meanwhile).
            const tracked = trackedSessions.get(sessionID);
            if (tracked !== undefined) {
              trackedSessions.set(sessionID, { title, driving: tracked.driving });
            }
            // Guard: only update if this session is still active (prevents race on fast switches)
            if (activeSessionID === sessionID) activeSessionTitle = title;
            return title;
          });
          if (envoyOn !== undefined) {
            await syncPort();
            const port = currentPort();
            if (port) {
              await subscribeSession(envoyOn.client, sessionID, activeSessionTitle, port, true);
              // After the title arrives, send one follow-up subscribe with it.
              titlePromise.then((title) => {
                const tracked = trackedSessions.get(sessionID);
                if (title && tracked !== undefined) {
                  subscribeSession(
                    envoyOn.client,
                    sessionID,
                    title,
                    currentPort() ?? 0,
                    tracked.driving
                  );
                }
              });
            }
          }
        }
      }

      if (event.type === "session.deleted") {
        const props = event.properties ?? {};
        const deletedID =
          (props.sessionID as string | undefined) ??
          (props.info as { id?: string } | undefined)?.id;
        if (deletedID && envoyOn !== undefined) await forgetSession(envoyOn.client, deletedID);
      }
    },
    // Cleanup hook (used by tests; production relies on the signal handlers above).
    dispose: stopEnvoy,
    tool:
      envoyOn === undefined
        ? {}
        : envoyTools(envoyOn.client, currentPort, () => activeSessionTitle),
  };
};
