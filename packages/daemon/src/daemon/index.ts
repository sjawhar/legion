import { randomUUID } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import path from "node:path";
import {
  controllerToken,
  type IssueKey,
  parseRoleToken,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import {
  type CiFetchResult,
  type CommandRunner,
  defaultRunner,
  getCiStatusBatch,
} from "../state/fetch";
import type { GitHubPRRef } from "../state/types";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "./api";
import { rootForIssue } from "./api/context";
import { EnvoyPublishError } from "./api/http";
import { publishDesignApproved, publishWakeEffects } from "./api/routes/issues";
import {
  DAEMON_PROBE_RETRY,
  verifyLegionPluginContract,
  verifyLegionPluginLoaded,
  verifyOmpAgentsCapability,
} from "./boot-probes";
import { createCancellableSleep } from "./cancellable-sleep";
import { overseerCatchup } from "./catchup";
import { type DaemonConfig, loadConfig } from "./config";
import { materializeDeploymentInstructions } from "./deployment-instructions";
import { createDispatchClient, type DispatchClient, specArtifactResolver } from "./dispatch-client";
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
import { designGateOpen, loadState, saveState } from "./legion-state";
import { createNatsTransport, type NatsTransport } from "./nats-transport";
import {
  daemonCredentialHelper,
  locatorsForIssue,
  ProcessManager,
  type ProcessManagerDeps,
} from "./processes";
import { childAdopted } from "./reducers";
import { runResync } from "./resync";
import { TmuxRuntime, type TmuxRuntimeDeps } from "./runtime-tmux";
import { DISPATCH_TOKEN_SECRET, writeSecretFile } from "./secrets";
import { installWorkerGhShim } from "./worker-bin";
import { connectWorkerRpc } from "./worker-rpc";
import { startWorkerStreamListener, type WorkerStreamListener } from "./worker-stream-listener";

const LINGER_SWEEP_INTERVAL_MS = 60_000;

interface DaemonDependencies {
  loadState: typeof loadState;
  saveState: typeof saveState;
  createNatsTransport(config: DaemonConfig): Promise<NatsTransport>;
  acquireInstanceLock(stateDir: string): Promise<InstanceLock>;
  runner: CommandRunner;
  statPrompt: NonNullable<ProcessManagerDeps["statPrompt"]>;
  readProcessCmdline?: TmuxRuntimeDeps["readProcessCmdline"];
  readProcessStat?: TmuxRuntimeDeps["readProcessStat"];
  readPluginManifest(manifestPath: string): Promise<string>;
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  dispatchClient: DispatchClient;
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
  connectWorkerRpc: TmuxRuntimeDeps["connectWorkerRpc"];
  sleep?: ProcessManagerDeps["sleep"];
  exit(code: number): void;
  now(): number;
}

export interface DaemonStartOptions {
  deps?: Partial<DaemonDependencies>;
}

export interface DaemonHandle {
  server: LegionApi["server"];
  workerStreamPort: number;
  config: DaemonConfig;
  ready(): Promise<void>;
  drain(): Promise<void>;
  stop(): Promise<void>;
}

/** The GitHub owner whose App installation token every configured role resolves against —
 * `config.repo`'s owner, the single source of repo ownership since B6 (a Dispatch issue key
 * carries no owner/repo of its own). Legacy `projectBoard`/`LEGION_ID must match owner/number`
 * parsed this from `legionId` instead, a leftover from the pre-Dispatch GitHub Projects V2 board
 * design where the project identifier doubled as `owner/number`; `legionId` is now an arbitrary
 * daemon identity (tmux session naming, state dir, role-token namespacing) with no owner
 * embedded in it, so that parse rejected any `legionId` not shaped like `owner/number` even
 * though nothing GitHub-board-related is left to validate. */
function repoOwner(repo: `${string}/${string}`): string {
  const [owner] = repo.split("/") as [string, string];
  return owner;
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

function defaultDependencies(
  config: DaemonConfig,
  overrides: Partial<DaemonDependencies> = {}
): DaemonDependencies {
  const tokenManager = new TokenManager(config.githubApps);
  if (!overrides.dispatchClient && (!config.dispatchUrl || !config.dispatchToken)) {
    throw new Error(
      "dispatch_url and DISPATCH_TOKEN are required to run the Legion daemon (Dispatch is the sole issue lifecycle source)"
    );
  }
  const dispatchClient =
    overrides.dispatchClient ??
    createDispatchClient({
      baseUrl: config.dispatchUrl as string,
      token: config.dispatchToken as string,
      project: config.dispatchProject,
    });
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
    dispatchClient,
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
  // Refused before the instance lock or anything else is acquired. LEGION-24 replaces this with
  // runtime selection; until then only the tmux runtime exists.
  if (config.runtime === "kubernetes") {
    throw new Error("runtime: kubernetes is not implemented yet");
  }
  const owner = repoOwner(config.repo);
  const deps = { ...defaultDependencies(config, options.deps), ...options.deps };
  // At most one daemon runs per project: two sharing a durable JetStream
  // consumer would split its messages and race saves to the same state
  // file. Released on clean shutdown (stop(), below) or on any startup
  // failure past this point.
  const instanceLock = await deps.acquireInstanceLock(config.stateDir);
  // Cancels the boot probes' pending backoff when boot fails for any other reason before the
  // launch hold awaits them (state load, NATS, the API bind): without it the chain would keep
  // spawning OMP probes in the background of a daemon that has already given up.
  const probeAbort = new AbortController();
  try {
    return await startDaemonLocked(config, deps, owner, instanceLock, probeAbort);
  } catch (error) {
    probeAbort.abort();
    await instanceLock.release();
    throw error;
  }
}

/** The probe backoff sleep, cut short the moment `signal` aborts so a probe waiting out a long
 * backoff notices the daemon's teardown at once instead of at the end of the wait. Without an
 * injected `sleep` it is `createCancellableSleep`, whose `cancel` clears the real timer — so a
 * pre-hold boot failure leaves no pending timer keeping the process alive. An injected sleep
 * (tests) is raced against the signal instead. Either way an already-aborted signal returns at
 * once. */
function abortableSleep(
  injected: ((ms: number) => Promise<void>) | undefined,
  signal: AbortSignal
): (ms: number) => Promise<void> {
  if (injected === undefined) {
    const timer = createCancellableSleep();
    signal.addEventListener("abort", () => timer.cancel(), { once: true });
    return (ms) => (signal.aborted ? Promise.resolve() : timer.sleep(ms));
  }
  return (ms) =>
    signal.aborted
      ? Promise.resolve()
      : new Promise<void>((resolve) => {
          const onAbort = () => resolve();
          signal.addEventListener("abort", onAbort, { once: true });
          void injected(ms).then(() => {
            signal.removeEventListener("abort", onAbort);
            resolve();
          });
        });
}

async function startDaemonLocked(
  config: DaemonConfig,
  deps: DaemonDependencies,
  owner: string,
  instanceLock: InstanceLock,
  probeAbort: AbortController
): Promise<DaemonHandle> {
  const environment = await deps.resolveDaemonEnvironment(config.ompInvocation, {
    run: deps.runner,
    stateDir: config.stateDir,
  });
  const runner = createDaemonRunner(environment, deps.runner);
  // The `gh` shim every pane's PATH puts first (`ProcessManager.credentialProcessEnvironment`),
  // installed before any pane can launch. An fs failure refuses startup: no pane may launch with a
  // PATH whose first entry does not exist.
  await installWorkerGhShim(config.stateDir);
  // The one Dispatch bearer every pane shares, delivered as a 0600 file pointer
  // (`DISPATCH_TOKEN_FILE`) rather than a `-e` argv value — see `secrets.ts`. An fs failure here
  // refuses startup exactly like a missing `DISPATCH_TOKEN` does: no pane may launch without it.
  if (config.dispatchToken !== undefined) {
    await writeSecretFile(config.stateDir, DISPATCH_TOKEN_SECRET, config.dispatchToken);
  }
  // The operator's deployment instructions, read and validated exactly once and re-materialized
  // under `<state_dir>` for every pane to `$(cat)` — see `deployment-instructions.ts`. Refuses
  // startup on a missing/unreadable/blank file (naming the resolved path) before the OMP probes
  // below are even started and before state is read: a deployment misconfigured this way must
  // never spawn a probe process or touch its state file.
  const deploymentInstructionsFile =
    config.instructionsPath === undefined
      ? undefined
      : await materializeDeploymentInstructions(
          config.instructionsPath,
          config.stateDir,
          config.legionId
        );
  // The plugin contract check is a local manifest read — no OMP spawn, no runner — so host load
  // cannot make it transient: a skewed plugin is a definitive refusal, made here before any state
  // is loaded or NATS/the API opened, exactly as before. Only the two OMP probes below are
  // load-sensitive and get the hold-and-retry treatment.
  await verifyLegionPluginContract(deps.readPluginManifest);
  // The two boot probes (boot-probes.ts) start here but are awaited only at the launch hold
  // below, just before the first pane could open: state load, NATS, the API bind, and the worker
  // reconnect all proceed while a probe is still retrying through host load, so an operator can
  // read `/legion/v1/state` and the durable lane keeps acking meanwhile. The no-op `catch`
  // keeps a definitive negative that lands before the hold from becoming an unhandled rejection;
  // the real handling is at the hold.
  const probeOptions = {
    sleep: abortableSleep(deps.sleep, probeAbort.signal),
    timeoutMs: config.slowCommandTimeoutSeconds * 1000,
    retry: DAEMON_PROBE_RETRY,
    signal: probeAbort.signal,
  };
  const probes = (async () => {
    await verifyOmpAgentsCapability(
      environment.ompInvocation,
      config.ompLaunchPrefix,
      runner,
      probeOptions
    );
    await verifyLegionPluginLoaded(
      environment.ompInvocation,
      config.ompLaunchPrefix,
      runner,
      deps.readPluginManifest,
      probeOptions
    );
  })();
  probes.catch(() => {});
  await deps.tokenManager.getToken("implement", owner);
  const stateFile = path.join(config.stateDir, "state.json");
  const state = await deps.loadState(stateFile, {
    project: config.project,
    cap: config.admissionCap,
    resolveSpecArtifact: specArtifactResolver(deps.dispatchClient),
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
  // A gate registered while `gates.design` was `root-issues` (or by a daemon predating the
  // gate-off handling) is a document approval nobody may ever give once the operator turns the
  // gate off. Satisfy it here exactly as `handleGatesRegister` would have — approve at the
  // document's latest version plus wake — so an architect still parked on it (its pane outlives a
  // daemon restart) proceeds, and a resumed one's catch-up shows the gate open. Only gates of
  // active trees: a closed or lingering tree has no architect waiting, and a reopen re-registers
  // its gate through the route anyway. Idempotent: only gates that are not open change.
  if (config.gates.design === "off") {
    const approved: IssueKey[] = [];
    for (const [issue, gate] of Object.entries(state.gates)) {
      if (designGateOpen(gate)) continue;
      const tree = rootForIssue(state, issue);
      if (!tree || state.trees[tree]?.status !== "active") continue;
      gate.approvedVersion = gate.latestVersion;
      approved.push(issue);
    }
    if (approved.length > 0) {
      await save();
      for (const issue of approved) {
        await publishDesignApproved(state, issue, deps.envoyPublish);
      }
    }
  }
  const nats = await deps.createNatsTransport(config);
  let api: LegionApi;

  // `state.project`, not `config.project`: every role token, secret-file name, and pane the
  // manager reasons about is keyed by the persisted project (`ProcessManager` reads
  // `deps.state.project` throughout), so the private server and the runtime's secret-file
  // names must come from the same value — exactly what the manager derived before the runtime
  // boundary existed.
  const runtime = new TmuxRuntime({
    tmux: { run: runner, socket: `legion-${state.project}` },
    project: state.project,
    stateDir: config.stateDir,
    connectWorkerRpc: deps.connectWorkerRpc,
    workerRpcTimeoutMs: () => config.workerRpcTimeoutSeconds * 1000,
    now: deps.now,
    sleep: deps.sleep,
    readProcessCmdline: deps.readProcessCmdline,
    readProcessStat: deps.readProcessStat,
    issueLocators: (issue) => locatorsForIssue(state, issue),
  });
  const processManager = new ProcessManager({
    state,
    saveState: save,
    config,
    runtime,
    ompInvocation: environment.ompInvocation,
    processPath: environment.paneEnv.PATH,
    credentialHelper: daemonCredentialHelper(),
    run: runner,
    natsPublish: (subject, data) => nats.publish(subject, data),
    natsRequest: (subject, data) => nats.request(subject, data),
    mintControllerCapability: async () => api.mintControllerCapability(),
    mintBootToken: (tree, generation) => api.mintBootToken(tree, generation),
    mintWorkerBootToken: (tree, issue, role, generation, expectedSessionId) =>
      api.mintWorkerBootToken(tree, issue, role, generation, expectedSessionId),
    revokeSessionCapability: (sessionId) => api.revokeSessionCapability(sessionId),
    provisioningToken: async (owner) =>
      (await deps.tokenManager.getToken("implement", owner)).token,
    statPrompt: deps.statPrompt,
    workerCatchup: { runner, tokenManager: deps.tokenManager, repo: config.repo },
    dispatchClient: deps.dispatchClient,
    now: deps.now,
    sleep: deps.sleep,
    deploymentInstructionsFile,
  });

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
    onAdmit: (issue) => {
      processManager.admit(issue);
    },
    onUndeliverable,
    config,
  };
  const eventPump: EventPump = startEventPump(eventDeps);
  const fetchCiStatusBatch = createCiStatusFetcher(deps.tokenManager, deps.runner);

  const emitResync = async (options?: { force?: boolean }): Promise<void> => {
    // Serialized against the shared durable mutation lane: resync reads/writes the same PrState
    // CI fields a durable Dispatch/GitHub checks-settlement message does, so the two must not
    // interleave (see events.ts's queue doc comment).
    const payload = await eventPump.runExclusive(() =>
      runResync(
        {
          state,
          config,
          dispatchClient: deps.dispatchClient,
          saveState: save,
          fetchCiStatusBatch,
          applyEffects: eventPump.applyEffects,
          now: deps.now,
        },
        options
      )
    );
    console.log(
      `[legion] resync complete: anomalies=${payload.anomalies.length} healed=${payload.healed} ciFetchFailures=${payload.ciFetchFailures}${
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
    dispatchClient: deps.dispatchClient,
    envoyPublish: deps.envoyPublish,
    onTreeReady: emitOverseerCatchup,
    // The controller is a role holder like any other: its catch-up on ready is the same
    // resync triage re-emission for unadmitted tracked roots that already runs periodically.
    // First delivers any Slack mention (or other irreplaceable payload) that arrived while no
    // controller held the role — the one narrow exception to "no held-event queue to replay"
    // (see `ControllerPendingNotice`'s doc comment) — then runs a forced resync: the respawn
    // that got us here may itself be the fallout of the *previous* controller crashing on an
    // anomaly this project's `resyncIntervalMs` throttle would otherwise still be suppressing,
    // so a fresh controller must never trust the interval to have elapsed.
    onControllerReady: async () => {
      await eventPump.drainControllerNotices();
      await emitResync({ force: true });
    },
    onControllerEvent: (payload) =>
      eventPump.publishControllerEvent(payload, {
        event_id: `api-controller:${randomUUID()}`,
        issued_at: deps.now(),
      }),
  };
  api = startLegionApi(
    {
      port: config.port,
      hostname: config.bind,
      repo: config.repo,
      gates: config.gates,
    },
    apiDeps
  );
  // Bound with the API and torn down with it. A bind failure is startup-fatal: stop the API
  // server it would have partnered so nothing half-listens behind the instance lock's release.
  let workerStream: WorkerStreamListener;
  try {
    workerStream = startWorkerStreamListener({
      hostname: config.bind,
      port: config.workerStreamPort,
      rpcTimeoutMs: config.workerRpcTimeoutSeconds * 1000,
      resolveBootToken: api.resolveWorkerBootToken,
      setTimeout: deps.setTimeout,
      clearTimeout: deps.clearTimeout,
    });
  } catch (error) {
    api.stop();
    throw error;
  }

  // Nothing on `processManager` runs before this point. Its `mintControllerCapability`,
  // `mintBootToken`, `mintWorkerBootToken`, and `revokeSessionCapability` deps read `api` by
  // reference, and every path into them — a dead worker's retirement (`retireWorkerLocator` ->
  // `revokeRoleClaim` -> `deps.revokeSessionCapability`), `ensureController`, root and worker
  // launches — is reachable only from here on. The span from `new ProcessManager(...)` to the
  // assignment above contains no `await`, so no callback can interleave with it.
  //
  // `reconnectWorkers` therefore runs here: after `api` because retiring a confirmed-dead worker
  // revokes its capability through it, and awaited before `enableLaunches()` below because it
  // is the source of truth `runningWorkerCount()` relies on — an admission decision racing
  // ahead of it would decide against a count that still holds every unprobed claim as running.
  // A reconnect's own `get_state` response can still synchronously fire `onIdle` ->
  // `promoteWorkerQueue`; that trigger (and `reconcileWorkerAdmission`) stay gated behind
  // `enableLaunches()` — boot's launch hold, released only once the OMP probes pass below.
  // `Bun.serve` is already accepting requests while this runs: safe, because every retirement
  // re-validates the claim it was handed under `mutateClaim(token)`, an unprobed claim counts
  // as running, and promotion stays gated.
  try {
    await processManager.reconnectWorkers();
  } catch (error) {
    console.error(`[legion] worker reconnection failed:`, error);
  }
  // Reaps pane secret files a crash left behind between clearing a locator and its save's prune.
  await processManager.pruneSecretFiles();
  // A crash between `/controller/ready` persisting its own role claim (`ctx.save()`) and that
  // same request finishing its own drain (`onControllerReady`, below) would otherwise strand
  // every notice already recorded in `controllerPendingNotices` forever: the controller session
  // that already claimed the role will never POST `/controller/ready` again this boot, so
  // nothing else would ever trigger a drain for it. Keyed off the durable queue itself, not
  // just a live claim: if no controller role exists at all (its own process died too, or one
  // never existed for this project), nothing would ever reach `/controller/ready` to trigger a
  // drain on its own -- `ensureController` spawns one directly, and its own eventual
  // `/controller/ready` call drains these same notices through the ordinary path once it's
  // live. Both branches are fire-and-forget: `drainControllerNotices` already retries a failed
  // publish with its own bounded backoff, `ensureController` is idempotent, and nothing else in
  // boot depends on either finishing. Kept with the manager's other boot calls, after `api`.
  // Boot's launch hold is still on here, so `ensureController` records the request instead of
  // opening a pane; `replayHeldRecoveries()` below the hold spawns it once the probes pass.
  if (state.controllerPendingNotices.length > 0) {
    if (state.roles[controllerToken(state.project)]) {
      void eventPump.drainControllerNotices().catch((error) => {
        console.error(`[legion] boot-time controller notice drain failed:`, error);
      });
    } else {
      void processManager.ensureController().catch((error) => {
        console.error(`[legion] boot-time controller spawn for pending notices failed:`, error);
      });
    }
  }

  let stopped = false;
  let resyncTimer: unknown;
  let lingerTimer: unknown;
  const drain = async () => {
    await eventPump.drain();
    // A spawn fired by `admit`'s promotion (never awaited at its call site)
    // or an in-flight `advancePromotionSweep` may still be running its own
    // `saveState`; without this, the final `save()` below can capture state
    // older than what that spawn is about to persist.
    await processManager.drainSpawns();
    await saving;
  };
  // `stop` is defined here, above the launch hold, because a definitive probe failure at the
  // hold tears the daemon down through it: the timers it clears are still unset at that point.
  const stop = async (): Promise<void> => {
    if (stopped) return;
    stopped = true;
    probeAbort.abort();
    if (resyncTimer !== undefined) deps.clearTimeout(resyncTimer);
    if (lingerTimer !== undefined) deps.clearInterval(lingerTimer);
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
        workerStream.close();
      } catch (error) {
        failure ??= error;
        console.error(
          `[legion] worker stream shutdown failed: ${error instanceof Error ? error.message : String(error)}`
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

  // THE LAUNCH HOLD. Everything above ran while the probes were still trying; nothing below may
  // open a pane until they pass. A transient probe failure is retried inside `probes` for as
  // long as it takes (unbounded, capped backoff — see `DAEMON_PROBE_RETRY`), the API answering
  // and the durable lane acking throughout; a spawn asked for meanwhile queued through
  // `ProcessManager`'s launch hold and is promoted below. A definitive negative (the wrong OMP,
  // a launch prefix that fails before OMP) still refuses to serve: the daemon closes what it
  // opened — event pump, API, worker stream, NATS, the instance lock — and `startDaemon` rejects
  // with the probe's error, so `legion start` exits 1 exactly as before.
  try {
    await probes;
  } catch (error) {
    try {
      await stop();
    } catch (teardown) {
      console.error("[legion] teardown after a failed boot probe failed:", teardown);
    }
    throw error;
  }

  // The probes passed. First the LEGION-57 boot repair, while the hold is still on: every child
  // tree that holds no process (`queued`, `launch-failed`, `active` without a locator) and whose
  // issue has a live ancestor tree leaves `trees`/`admission` here, before `reconcileAdmission`
  // below could promote it, or demote and relaunch it, as a root. The parents' architects are
  // woken once boot admission has settled (after `replayHeldRecoveries()`).
  const adoptions = processManager.adoptOwnerlessChildTrees();
  // Then release the hold. The promotion cascades below call back into `processManager`'s
  // `mintBootToken`/`mintControllerCapability` closures, which read `api` by reference -- assigned
  // long since. `Bun.serve` has been accepting requests throughout; the running-worker and
  // tree-admission counts these calls converge are correct before that (computed fresh from
  // `state.roles`/`state.admission`, not accumulated), so an early request was never
  // over-admitted -- it queued, and is promoted here in FIFO order. `enableLaunches()` releases
  // the hold every pane-opening path (`admit`, `spawnWorker`, `resurrect`, `ensureController`,
  // and every `onIdle`/`markWorkerDead`/`closeTree` promotion trigger from this point on) waited
  // behind -- see `ProcessManager.launchesEnabled` and `WorkerAdmission.workerPromotionEnabled`.
  processManager.enableLaunches();
  // Before `reconcileAdmission`'s own promotion cascade, which can take a while (spawning
  // multiple queued roots): a restored active-with-a-locator-but-never-confirmed tree must have
  // its registration deadline armed immediately, not only once that cascade finishes, or it sits
  // unwatched for however long promotion takes. `reconnectRoots` is synchronous.
  processManager.reconnectRoots();
  await processManager.reconcileAdmission();
  await processManager.reconcileWorkerAdmission();
  // A resurrection or controller launch requested while the hold was on (the pending-notice
  // `ensureController` above, an exception routed to a dead root, an API call from a live
  // architect) runs now.
  await processManager.replayHeldRecoveries();
  // Each child the boot repair moved back into its parent's tree: wake the parent's architect
  // exactly as `issue.created` would (`childAdopted`, the reducer's own payload), so it spawns the
  // sub-architect. Best-effort like the gate-off wake above, and a 404 no-holder is recovered the
  // ordinary way: the parent root's locator survived boot (or `reconnectRoots`/the resync probe
  // resurrects it), and the resurrected architect's catch-up procedure spawns a sub-architect for
  // any released child without an architect claim.
  for (const { child, parent } of adoptions) {
    await publishWakeEffects(
      childAdopted(state, parent, child),
      deps.envoyPublish,
      (message) =>
        `[legion] the child-adopted wake for ${child} (moved back into ${parent}'s tree at boot) failed to publish; the parent's architect reconciles released children at its next catch-up: ${message}`
    );
  }
  const ready = nats.ready();

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

  lingerTimer = deps.setInterval(() => {
    const now = deps.now();
    for (const tree of Object.values(state.trees)) {
      if (tree.status !== "lingering" || !tree.lingerUntil) continue;
      if (Date.parse(tree.lingerUntil) <= now) {
        void processManager.expireLinger(tree.root).catch((error) => {
          console.error(`[legion] linger cleanup failed for ${tree.root}:`, error);
        });
      }
    }
    void processManager.reconcileOrphans().catch((error) => {
      console.error("[legion] orphan reconciliation failed:", error);
    });
    // A below-threshold launch failure rotates its head to the tail (see
    // `promoteQueuedWorker`) instead of blocking the queue, but nothing else retries a queue
    // with zero live workers on its own — this periodic tick is that retry, mirroring how a
    // worker-cap raise between restarts gets promoted via the same call at boot.
    void processManager.reconcileWorkerAdmission().catch((error) => {
      console.error(`[legion] worker admission reconciliation failed:`, error);
    });
  }, LINGER_SWEEP_INTERVAL_MS);

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

  console.log(`legion daemon listening on ${config.bind}:${api.server.port}`);
  console.log(`legion worker stream listening on ${config.bind}:${workerStream.port}`);
  return {
    server: api.server,
    workerStreamPort: workerStream.port,
    config,
    ready: () => ready,
    drain,
    stop,
  };
}

if (import.meta.main) {
  void startDaemon(loadConfig()).catch((error) => {
    console.error(
      `[legion] daemon failed: ${error instanceof Error ? error.message : String(error)}`
    );
    process.exitCode = 1;
  });
}
