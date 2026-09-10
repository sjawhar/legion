import { randomUUID } from "node:crypto";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type IssueKey, parseRoleToken, roleToken, roleTopic } from "@legion/contracts";
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
import { rootForIssue } from "./api/context";
import { EnvoyPublishError } from "./api/http";
import { setApprovalStatus } from "./approval-check";
import { overseerCatchup } from "./catchup";
import { type DaemonConfig, type GitHubAppRole, loadConfig } from "./config";
import {
  createDaemonRunner,
  type DaemonEnvironment,
  type ResolveDaemonEnvironmentDeps,
  resolveDaemonEnvironment,
} from "./environment";
import {
  type EventPump,
  type EventPumpDeps,
  startEventPump,
  type UndeliverableInfo,
} from "./events";
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
    throw new EnvoyPublishError(topic, response.status);
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
    revokeSessionCapability: (sessionId) => api.revokeSessionCapability(sessionId),
    connectWorkerRpc: deps.connectWorkerRpc,
    provisioningToken: async (owner) =>
      (await deps.tokenManager.getToken("implement", owner)).token,
    statPrompt: deps.statPrompt,
    readProcessCmdline: deps.readProcessCmdline,
    workerCatchup: { runner, tokenManager: deps.tokenManager },
    now: deps.now,
  });

  // Awaited (not fire-and-forget): reconnectWorkers is the source of truth
  // runningWorkerCount() relies on, so an admission decision racing ahead of it would risk
  // over-admitting past the configured cap. This runs before `api` is assigned purely because
  // it needs no `api` reference (it only probes existing connections; it never mints a boot
  // token) — but a reconnect's own `get_state` response can still synchronously fire
  // `onIdle` -> `promoteWorkerQueue`, which is why that trigger (and `reconcileWorkerAdmission`)
  // stay gated behind `processManager.enableWorkerPromotion()` below until `api` exists: nothing
  // here is protected by call *ordering*, only by the gate.
  try {
    await processManager.reconnectWorkers();
  } catch (error) {
    console.error(`[legion] worker reconnection failed:`, error);
  }
  const emitOverseerCatchup = async (tree: IssueKey): Promise<void> => {
    const payload = await overseerCatchup(state, tree);
    await deps.envoyPublish(
      roleTopic(roleToken(state.project, tree, "architect")),
      JSON.stringify(payload)
    );
  };

  const onUndeliverable = async (info: UndeliverableInfo): Promise<void> => {
    try {
      if (info.kind === "controller") {
        await processManager.ensureController();
        return;
      }
      const parsed = parseRoleToken(state.project, info.role);
      if (!parsed || "controller" in parsed) return;
      const root = rootForIssue(state, parsed.issue);
      if (!root) return;
      await processManager.resumeWorker(root, parsed.issue, parsed.role);
    } catch (error) {
      console.error(`[legion] onUndeliverable recovery failed for ${info.role}:`, error);
    }
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
    onUndeliverable,
    config,
  };
  const eventPump: EventPump = startEventPump(eventDeps);
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

  const apiDeps: LegionApiDeps = {
    state,
    saveState: save,
    runner,
    tokenManager: deps.tokenManager,
    processManager,
    envoyPublish: deps.envoyPublish,
    onTreeReady: emitOverseerCatchup,
    // The controller is a role holder like any other: its catch-up on ready is the same
    // resync triage re-emission for unadmitted tracked roots that already runs periodically —
    // there is no held-event queue to replay.
    onControllerReady: emitResync,
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
  // `mintControllerCapability` closures, which read `api` by reference. `Bun.serve` above
  // already has the port open and accepting connections by this point — the running-worker
  // and tree-admission counts these two calls converge are correct *before* that happens
  // (computed fresh from `state.roles`/`state.admission`, not accumulated), so an early real
  // request arriving during this window is never over-admitted; it only serializes behind
  // these calls on the same `admissionLock`, which can at most let it jump ahead of a queued
  // tree/worker in FIFO order. `enableWorkerPromotion()` opens the gate `reconcileWorkerAdmission`
  // (and every `onIdle`/`markWorkerDead`/`closeTree` trigger from this point on) requires —
  // see `WorkerAdmission.workerPromotionEnabled`'s doc comment.
  processManager.enableWorkerPromotion();
  await processManager.reconcileAdmission();
  await processManager.reconcileWorkerAdmission();
  const ready = nats.ready();

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
    // A below-threshold launch failure rotates its head to the tail (see
    // `promoteQueuedWorker`) instead of blocking the queue, but nothing else retries a queue
    // with zero live workers on its own — this periodic tick is that retry, mirroring how a
    // worker-cap raise between restarts gets promoted via the same call at boot.
    void processManager.reconcileWorkerAdmission().catch((error) => {
      console.error(`[legion] worker admission reconciliation failed:`, error);
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
    processManager.dispose();
    let failure: unknown;
    try {
      await drain();
    } catch (error) {
      failure = error;
      console.error(
        `[legion] event drain failed: ${error instanceof Error ? error.message : String(error)}`
      );
    } finally {
      // Cancels any watchdog an in-flight handler armed *during* drain (the pre-drain call
      // above only catches what was already armed before this) - dispose() is idempotent, so
      // calling it again here is always safe.
      processManager.dispose();
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
