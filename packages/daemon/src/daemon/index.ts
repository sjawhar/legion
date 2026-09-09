import { randomUUID } from "node:crypto";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type IssueKey, roleToken, roleTopic } from "@legion/contracts";
import { getPluginsNodeModules } from "@oh-my-pi/pi-utils/dirs";
import {
  type CiFetchResult,
  type CommandRunner,
  defaultRunner,
  getCiStatusBatch,
} from "../state/fetch";
import { fetchGitHubProjectItems, type GitHubProjectItemsResult } from "../state/github-fetch";
import type { GitHubPRRef } from "../state/types";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "./api";
import { setApprovalStatus } from "./approval-check";
import { overseerCatchup } from "./catchup";
import { type DaemonConfig, type GitHubAppRole, loadConfig } from "./config";
import {
  createDaemonRunner,
  type DaemonEnvironment,
  type ResolveDaemonEnvironmentDeps,
  resolveDaemonEnvironment,
} from "./environment";
import { type EventPump, type EventPumpDeps, startEventPump } from "./events";
import { buildRoleEnv, TokenManager } from "./github-apps";
import { acquireInstanceLock, type InstanceLock } from "./instance-lock";
import { loadState, saveState } from "./legion-state";
import { createNatsTransport, type NatsTransport } from "./nats-transport";
import { daemonCredentialHelper, ProcessManager, type ProcessManagerDeps } from "./processes";
import { runResync } from "./resync";
import { connectWorkerRpc } from "./worker-rpc";

const LINGER_SWEEP_INTERVAL_MS = 60_000;
const OMP_AGENTS_CAPABILITY_MARKER = "LEGION_OMP_AGENTS=available";
const OMP_AGENTS_CAPABILITY_PROBE = `export default function probeOmpAgents(pi) {
  process.stderr.write(pi.agents ? "LEGION_OMP_AGENTS=available\\n" : "LEGION_OMP_AGENTS=missing\\n");
}
`;

interface DaemonDependencies {
  loadState: typeof loadState;
  saveState: typeof saveState;
  createNatsTransport(config: DaemonConfig): Promise<NatsTransport>;
  acquireInstanceLock(stateDir: string): Promise<InstanceLock>;
  runner: CommandRunner;
  statPrompt: NonNullable<ProcessManagerDeps["statPrompt"]>;
  readProcessCmdline?: ProcessManagerDeps["readProcessCmdline"];
  readPluginManifest(manifestPath: string): Promise<string>;
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  fetchGitHubProjectItems(): Promise<GitHubProjectItemsResult>;
  tokenManager: Pick<TokenManager, "getToken">;
  resolveDaemonEnvironment(
    ompInvocation: string,
    deps: ResolveDaemonEnvironmentDeps
  ): Promise<DaemonEnvironment>;
  setTimeout(callback: () => void, delayMs: number): unknown;
  clearTimeout(timer: unknown): void;
  setInterval(callback: () => void, delayMs: number): unknown;
  clearInterval(timer: unknown): void;
  onSignal(signal: NodeJS.Signals, listener: () => void): void;
  connectWorkerRpc: ProcessManagerDeps["connectWorkerRpc"];
  exit(code: number): void;
  now(): number;
}

export interface DaemonStartOptions {
  deps?: Partial<DaemonDependencies>;
}

export interface DaemonHandle {
  server: LegionApi["server"];
  config: DaemonConfig;
  ready(): Promise<void>;
  drain(): Promise<void>;
  stop(): Promise<void>;
}

function projectBoard(legionId: string): { owner: string; number: number } {
  const [owner, numberText, ...extra] = legionId.split("/");
  const number = Number(numberText);
  if (!owner || extra.length > 0 || !Number.isSafeInteger(number) || number <= 0) {
    throw new Error(`LEGION_ID must match owner/number (got: ${legionId})`);
  }
  return { owner, number };
}

async function resolveConfiguredAppLogins(
  config: DaemonConfig,
  tokenManager: Pick<TokenManager, "getToken">,
  owner: string
): Promise<string[]> {
  const roles = Object.keys(config.githubApps) as GitHubAppRole[];
  if (config.gates.merge === "human" && roles.length === 0) {
    throw new Error("gates.merge=human requires at least one configured GitHub App login");
  }
  const logins = await Promise.all(
    roles.map(async (role) => (await tokenManager.getToken(role, owner)).gitIdentity.name)
  );
  if (config.gates.merge === "human" && logins.some((login) => login.length === 0)) {
    throw new Error("gates.merge=human requires at least one configured GitHub App login");
  }
  return [...new Set(logins)];
}

export function createBoardProjectItemsFetcher(
  board: { owner: string; number: number },
  tokenManager: Pick<TokenManager, "getToken">,
  runner: CommandRunner = defaultRunner
): () => Promise<GitHubProjectItemsResult> {
  return () =>
    fetchGitHubProjectItems(board.owner, board.number, runner, async (owner) => {
      const lease = await tokenManager.getToken("implement", owner);
      return {
        env: buildRoleEnv(lease.token, lease.gitIdentity, process.env),
      };
    });
}

export function createCiStatusFetcher(
  tokenManager: Pick<TokenManager, "getToken">,
  runner: CommandRunner = defaultRunner
): (prRefs: Record<string, GitHubPRRef>) => Promise<Record<string, CiFetchResult>> {
  return (prRefs) =>
    getCiStatusBatch(prRefs, runner, async (owner) => {
      const lease = await tokenManager.getToken("implement", owner);
      return {
        env: buildRoleEnv(lease.token, lease.gitIdentity, process.env),
      };
    });
}

/** An Envoy publish failure carrying the HTTP status, so callers can distinguish "no holder" (404) from other rejections. */
export interface EnvoyPublishError extends Error {
  status?: number;
}

async function publishToEnvoy(
  config: DaemonConfig,
  topic: string,
  payloadJson: string
): Promise<void> {
  const response = await fetch(`${config.envoyUrl}/v1/messages/publish`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ topic, message: payloadJson, payload: payloadJson }),
  });
  if (!response.ok) {
    const error = new Error(`Envoy publish to ${topic} failed with status ${response.status}`);
    (error as EnvoyPublishError).status = response.status;
    throw error;
  }
}
async function verifyOmpAgentsCapability(
  ompInvocation: string,
  runner: CommandRunner
): Promise<void> {
  const probeDir = await mkdtemp(path.join(os.tmpdir(), "legion-omp-probe-"));
  const probePath = path.join(probeDir, "probe.mjs");
  try {
    await writeFile(probePath, OMP_AGENTS_CAPABILITY_PROBE, "utf8");
    const result = await runner([
      "sh",
      "-c",
      `${ompInvocation} models --no-extensions --extension "$1" --json >/dev/null`,
      "sh",
      probePath,
    ]);
    if (
      result.exitCode === 0 &&
      (result.stderr.includes(OMP_AGENTS_CAPABILITY_MARKER) ||
        result.stdout.includes(OMP_AGENTS_CAPABILITY_MARKER))
    ) {
      return;
    }

    const detail = [result.stderr.trim(), result.stdout.trim()].filter(Boolean).join("\n");
    throw new Error(
      `[legion] Configured OMP invocation does not expose pi.agents${detail ? `: ${detail}` : ""}`
    );
  } finally {
    await rm(probeDir, { recursive: true, force: true });
  }
}

// Read by legion.ts (packages/pi-envoy/extensions/legion.ts) on load: proves the
// extension actually loaded through OMP's own extension pipeline, not merely that
// its manifest file exists on disk. A manifest-only check would pass even when the
// plugin is disabled (`omp plugin disable`) or unregistered, in which case OMP's
// ambient discovery silently skips it and every spawned session is Legion-less.
const LEGION_LOADED_MARKER = "LEGION_PLUGIN_LOADED=yes";
const LEGION_LOAD_PROBE = `export default function probeLegionPluginLoaded(pi) {
  const loaded = globalThis[Symbol.for("legion.pi-envoy.legion-loaded")];
  process.stderr.write(loaded ? "LEGION_PLUGIN_LOADED=yes\\n" : "LEGION_PLUGIN_LOADED=no\\n");
}
`;

// A daemon and the OMP sessions it spawns share one ambient environment (Legion
// never sets `--profile`/`OMP_PROFILE` for spawned sessions), so this probe's
// invocation — no `--extension` beyond the probe's own — matches the daemon's real
// spawn shape closely enough that ambient discovery resolves the same plugin root
// a spawned session will load from.
//
// Known gap: this probe runs from the daemon's own cwd, not a spawned root's
// `workspace.workspaceDir`. A target repo that commits `.omp/plugin-overrides.json`
// disabling `pi-legion-envoy` passes this boot gate but still launches a
// Legion-less session. That is caught at runtime instead: such a session never
// calls `/process/started` or `/worker/started`, and the boot handshake treats an
// unclaimed boot token as a launch failure (see T5/T9).
async function verifyLegionPluginLoaded(
  ompInvocation: string,
  runner: CommandRunner,
  readPluginManifest: (manifestPath: string) => Promise<string>
): Promise<void> {
  const probeDir = await mkdtemp(path.join(os.tmpdir(), "legion-plugin-probe-"));
  const probePath = path.join(probeDir, "probe.mjs");
  try {
    await writeFile(probePath, LEGION_LOAD_PROBE, "utf8");
    const result = await runner([
      "sh",
      "-c",
      `${ompInvocation} models --extension "$1" --json >/dev/null`,
      "sh",
      probePath,
    ]);
    if (
      result.exitCode === 0 &&
      (result.stderr.includes(LEGION_LOADED_MARKER) || result.stdout.includes(LEGION_LOADED_MARKER))
    ) {
      return;
    }
    // The manifest read is a best-effort version hint for the error message only —
    // it is not part of the pass/fail gate above.
    const manifestPath = path.join(
      getPluginsNodeModules(),
      "@sjawhar",
      "pi-legion-envoy",
      "package.json"
    );
    const version = await readPluginManifest(manifestPath)
      .then((raw) => {
        const manifest: { readonly version?: string } = JSON.parse(raw);
        return manifest.version;
      })
      .catch(() => undefined);
    throw new Error(
      `[legion] pi-legion-envoy${version ? ` ${version}` : ""} is installed but not loaded by omp (disabled or unregistered); run omp plugin list`
    );
  } finally {
    await rm(probeDir, { recursive: true, force: true });
  }
}

function defaultDependencies(config: DaemonConfig): DaemonDependencies {
  const board = projectBoard(config.legionId);
  const tokenManager = new TokenManager(config.githubApps);
  return {
    loadState,
    saveState,
    createNatsTransport,
    acquireInstanceLock,
    runner: defaultRunner,
    resolveDaemonEnvironment,
    statPrompt: stat,
    readPluginManifest: (manifestPath) => readFile(manifestPath, "utf8"),
    envoyPublish: (topic, payloadJson) => publishToEnvoy(config, topic, payloadJson),
    fetchGitHubProjectItems: createBoardProjectItemsFetcher(board, tokenManager),
    tokenManager,
    setTimeout: (callback, delayMs) => setTimeout(callback, delayMs),
    clearTimeout: (timer) => clearTimeout(timer as number),
    setInterval: (callback, delayMs) => setInterval(callback, delayMs),
    clearInterval: (timer) => clearInterval(timer as number),
    onSignal: (signal, listener) => {
      process.on(signal, listener);
    },
    connectWorkerRpc,
    exit: (code) => {
      process.exit(code);
    },
    now: Date.now,
  };
}

export async function startDaemon(
  config: DaemonConfig,
  options: DaemonStartOptions = {}
): Promise<DaemonHandle> {
  const board = projectBoard(config.legionId);
  const deps = { ...defaultDependencies(config), ...options.deps };
  // At most one daemon runs per project: two sharing a durable JetStream
  // consumer would split its messages and race saves to the same state
  // file. Released on clean shutdown (stop(), below) or on any startup
  // failure past this point.
  const instanceLock = await deps.acquireInstanceLock(config.stateDir);
  try {
    return await startDaemonLocked(config, deps, board, instanceLock);
  } catch (error) {
    await instanceLock.release();
    throw error;
  }
}

async function startDaemonLocked(
  config: DaemonConfig,
  deps: DaemonDependencies,
  board: { owner: string; number: number },
  instanceLock: InstanceLock
): Promise<DaemonHandle> {
  const environment = await deps.resolveDaemonEnvironment(config.ompInvocation, {
    run: deps.runner,
  });
  const runner = createDaemonRunner(environment, deps.runner);
  await verifyOmpAgentsCapability(environment.ompInvocation, runner);
  await verifyLegionPluginLoaded(environment.ompInvocation, runner, deps.readPluginManifest);
  config.appLogins = await resolveConfiguredAppLogins(config, deps.tokenManager, board.owner);
  await deps.tokenManager.getToken("implement", board.owner);
  const stateFile = path.join(config.stateDir, "state.json");
  const state = await deps.loadState(stateFile, {
    project: config.project,
    cap: config.admissionCap,
  });
  let saving: Promise<void> | undefined;
  const save = () => {
    const write = saving
      ? saving.catch(() => {}).then(() => deps.saveState(stateFile, state))
      : deps.saveState(stateFile, state);
    saving = write;
    void write.then(
      () => {
        if (saving === write) saving = undefined;
      },
      () => {
        if (saving === write) saving = undefined;
      }
    );
    return write;
  };
  const nats = await deps.createNatsTransport(config);
  let api: LegionApi;

  const processManager = new ProcessManager({
    state,
    saveState: save,
    config,
    ompInvocation: environment.ompInvocation,
    panePath: environment.paneEnv.PATH,
    credentialHelper: daemonCredentialHelper(),
    run: runner,
    natsPublish: (subject, data) => nats.publish(subject, data),
    natsRequest: (subject, data) => nats.request(subject, data),
    mintControllerCapability: async () => api.mintControllerCapability(),
    mintBootToken: (tree, generation) => api.mintBootToken(tree, generation),
    mintWorkerBootToken: (tree, issue, role, generation, expectedSessionId) =>
      api.mintWorkerBootToken(tree, issue, role, generation, expectedSessionId),
    connectWorkerRpc: deps.connectWorkerRpc,
    provisioningToken: async (owner) =>
      (await deps.tokenManager.getToken("implement", owner)).token,
    statPrompt: deps.statPrompt,
    readProcessCmdline: deps.readProcessCmdline,
    workerCatchup: { runner, tokenManager: deps.tokenManager },
    now: deps.now,
  });

  // reconcileAdmission is now awaited further down (see the comment near its call), once
  // `api` is assigned: its promotion cascade calls back into `mintBootToken`/
  // `mintControllerCapability` closures that read `api` by reference.
  void processManager.reconnectWorkers().catch((error) => {
    console.error(`[legion] worker reconnection failed:`, error);
  });
  const emitOverseerCatchup = async (tree: IssueKey): Promise<void> => {
    const payload = await overseerCatchup(state, tree);
    await deps.envoyPublish(
      roleTopic(roleToken(state.project, tree, "architect")),
      JSON.stringify(payload)
    );
  };

  const eventDeps: EventPumpDeps = {
    nats,
    envoyPublish: deps.envoyPublish,
    state,
    saveState: save,
    onException: (exception) => processManager.handleException(exception),
    onLinger: (tree) => processManager.beginLinger(tree),
    onProbe: async (tree) => {
      if ((await processManager.probe(tree)) === "dead") await processManager.resurrect(tree);
    },
    onApprovalStatus: (effect) =>
      setApprovalStatus(effect, {
        runner,
        tokenManager: deps.tokenManager,
        appLogins: config.appLogins,
        gatesMerge: config.gates.merge,
      }),
    config,
  };
  const eventPump: EventPump = startEventPump(eventDeps);
  const apiDeps: LegionApiDeps = {
    state,
    saveState: save,
    runner,
    tokenManager: deps.tokenManager,
    processManager,
    envoyPublish: deps.envoyPublish,
    onTreeReady: emitOverseerCatchup,
    onControllerReady: () => eventPump.redeliverControllerEvents(),
    onControllerEvent: (payload) =>
      eventPump.publishControllerEvent(payload, {
        event_id: `api-controller:${randomUUID()}`,
        issued_at: deps.now(),
      }),
  };
  api = startLegionApi(
    {
      port: config.port,
      hostname: "127.0.0.1",
      gates: config.gates,
      appLogins: config.appLogins,
    },
    apiDeps
  );

  // Awaited only now that `api` is assigned: the promotion cascade this can
  // trigger calls back into `processManager`'s `mintBootToken`/
  // `mintControllerCapability` closures, which read `api` by reference.
  await processManager.reconcileAdmission();
  const ready = nats.ready();
  const fetchCiStatusBatch = createCiStatusFetcher(deps.tokenManager, deps.runner);

  const emitResync = async (): Promise<void> => {
    // Serialized against durable GitHub messages: resync reads/writes the
    // same PrState CI fields a durable checks settlement does, so the two
    // must not interleave (see events.ts's queue doc comment).
    const payload = await eventPump.runExclusive(() =>
      runResync({
        state,
        config,
        fetchGitHubProjectItems: deps.fetchGitHubProjectItems,
        fetchCiStatusBatch,
        applyEffects: eventPump.applyEffects,
        now: deps.now,
      })
    );
    console.log(
      `[legion] resync complete: anomalies=${payload.anomalies.length} healed=${payload.healed} reconciled-labels=${payload.reconciledLabels} excluded-null-content-items=${payload.excludedNullContentItems} ciFetchFailures=${payload.ciFetchFailures}${
        payload.ciFetchFailureDetails.length === 0
          ? ""
          : ` ciFetchFailureDetails=${payload.ciFetchFailureDetails
              .map(({ owner, error }) => `owner=${owner} error=${error}`)
              .join(" ")}`
      }`
    );
    await eventPump.publishControllerEvent(payload, {
      event_id: `resync:${randomUUID()}`,
      issued_at: deps.now(),
    });
  };

  let stopped = false;
  let resyncTimer: unknown;
  const scheduleResync = (): void => {
    resyncTimer = deps.setTimeout(async () => {
      try {
        await emitResync();
      } catch (error) {
        console.error(
          `[legion] resync failed: ${error instanceof Error ? error.message : String(error)}`
        );
      }
      if (!stopped) scheduleResync();
    }, config.resyncIntervalMs);
  };
  scheduleResync();

  const lingerTimer = deps.setInterval(() => {
    const now = deps.now();
    for (const tree of Object.values(state.trees)) {
      if (tree.status !== "lingering" || !tree.lingerUntil) continue;
      if (Date.parse(tree.lingerUntil) <= now) {
        void processManager.expireLinger(tree.root).catch((error) => {
          console.error(`[legion] linger cleanup failed for ${tree.root}:`, error);
        });
      }
    }
    void processManager.reconcileTmuxWindows().catch((error) => {
      console.error(`[legion] tmux reconciliation failed:`, error);
    });
  }, LINGER_SWEEP_INTERVAL_MS);

  const drain = async () => {
    await eventPump.drain();
    // A spawn fired by `admit`'s promotion (never awaited at its call site)
    // or an in-flight `advancePromotionSweep` may still be running its own
    // `saveState`; without this, the final `save()` below can capture state
    // older than what that spawn is about to persist.
    await processManager.drainSpawns();
    await saving;
  };

  const stop = async (): Promise<void> => {
    if (stopped) return;
    stopped = true;
    if (resyncTimer !== undefined) deps.clearTimeout(resyncTimer);
    deps.clearInterval(lingerTimer);
    eventPump.stop();
    let failure: unknown;
    try {
      await drain();
    } catch (error) {
      failure = error;
      console.error(
        `[legion] event drain failed: ${error instanceof Error ? error.message : String(error)}`
      );
    } finally {
      try {
        await save();
      } catch (error) {
        failure ??= error;
        console.error(
          `[legion] final state save failed: ${error instanceof Error ? error.message : String(error)}`
        );
      }
      try {
        api.stop();
      } catch (error) {
        failure ??= error;
        console.error(
          `[legion] API shutdown failed: ${error instanceof Error ? error.message : String(error)}`
        );
      }
      try {
        await nats.close();
      } catch (error) {
        failure ??= error;
        console.error(
          `[legion] NATS shutdown failed: ${error instanceof Error ? error.message : String(error)}`
        );
      }
      try {
        await instanceLock.release();
      } catch (error) {
        failure ??= error;
        console.error(
          `[legion] instance lock release failed: ${error instanceof Error ? error.message : String(error)}`
        );
      }
    }
    if (failure) throw failure;
  };
  const stopForSignal = (): void => {
    void stop()
      .catch((error) => {
        console.error(
          `[legion] shutdown failed: ${error instanceof Error ? error.message : String(error)}`
        );
      })
      .then(() => deps.exit(0));
  };
  deps.onSignal("SIGTERM", stopForSignal);
  deps.onSignal("SIGINT", stopForSignal);

  console.log(`legion daemon listening on 127.0.0.1:${api.server.port}`);
  return { server: api.server, config, ready: () => ready, drain, stop };
}

if (import.meta.main) {
  void startDaemon(loadConfig()).catch((error) => {
    console.error(
      `[legion] daemon failed: ${error instanceof Error ? error.message : String(error)}`
    );
    process.exitCode = 1;
  });
}
