import { randomUUID } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import path from "node:path";
import {
  controllerToken,
  type IssueKey,
  LEGION_DAEMON_API_VERSION,
  ROLE_TOPIC_PREFIX,
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
import {
  type DaemonConfig,
  type KubernetesRuntimeConfig,
  loadConfig,
  ownerForIssue,
  primaryProjectKey,
  projectKeys,
  projectRepos,
  type RuntimeName,
  repoForIssue,
} from "./config";
import { materializeDeploymentInstructions } from "./deployment-instructions";
import { createDispatchClient, type DispatchClient, specArtifactResolver } from "./dispatch-client";
import {
  createDaemonRunner,
  type DaemonEnvironment,
  type DaemonEnvironmentFor,
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
import { createK8sClient, type K8sClient, resolveK8sCredentials } from "./k8s-client";
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
import type { Runtime } from "./runtime";
import { KubernetesRuntime } from "./runtime-kubernetes";
import { TmuxRuntime, type TmuxRuntimeDeps } from "./runtime-tmux";
import { DISPATCH_TOKEN_SECRET, writeSecretFile } from "./secrets";
import { installWorkerGhShim } from "./worker-bin";
import { verifyWorkerImage } from "./worker-image-probe";
import { connectWorkerRpc } from "./worker-rpc";
import { startWorkerStreamListener, type WorkerStreamListener } from "./worker-stream-listener";

const LINGER_SWEEP_INTERVAL_MS = 60_000;

interface DaemonDependencies {
  loadState: typeof loadState;
  saveState: typeof saveState;
  createNatsTransport(config: DaemonConfig): Promise<NatsTransport>;
  acquireInstanceLock(stateDir: string): Promise<InstanceLock>;
  runner: CommandRunner;
  statPrompt: NonNullable<TmuxRuntimeDeps["statPrompt"]>;
  readProcessCmdline?: TmuxRuntimeDeps["readProcessCmdline"];
  readProcessStat?: TmuxRuntimeDeps["readProcessStat"];
  /** Tests inject a client over a fake API server; production resolves credentials
   * (`resolveK8sCredentials`) from `runtime.kubernetes.kubeconfig` or the in-cluster files. */
  k8sClient?: K8sClient;
  readPluginManifest(manifestPath: string): Promise<string>;
  /** Publishes a daemon notice through the listener's `POST /v1/messages/publish`. `dedupeKey`
   * becomes the body's `dedupe_key` (`envoyPublishBody`) — set only by `handleException`'s
   * re-send of a failed copy, so the plugin's dedupe recognises it as the same message. */
  envoyPublish(topic: string, payloadJson: string, dedupeKey?: string): Promise<void>;
  dispatchClient: DispatchClient;
  tokenManager: Pick<TokenManager, "getToken">;
  resolveDaemonEnvironment<R extends RuntimeName>(
    ompInvocation: string,
    deps: ResolveDaemonEnvironmentDeps<R>
  ): Promise<DaemonEnvironmentFor<R>>;
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

/** `baseEnv` is the daemon's `paneEnv`: a `gh` child gets the allow-listed environment plus its
 * minted token and identity (`buildRoleEnv`), never the daemon's own `process.env`. */
export function createCiStatusFetcher(
  tokenManager: Pick<TokenManager, "getToken">,
  runner: CommandRunner,
  baseEnv: NodeJS.ProcessEnv
): (prRefs: Record<string, GitHubPRRef>) => Promise<Record<string, CiFetchResult>> {
  return (prRefs) =>
    getCiStatusBatch(prRefs, runner, async (owner) => {
      const lease = await tokenManager.getToken("implement", owner);
      return {
        env: buildRoleEnv(lease.token, lease.gitIdentity, baseEnv),
      };
    });
}

/** The listener publish body (`POST /v1/messages/publish`) for one daemon notice. `dedupe_key`
 * is present only when a key is given: a LEGION-108 listener uses it verbatim as the envelope's
 * dedupe key (mutually exclusive with `idempotency_key`, which the daemon never sends); an older
 * listener ignores it and mints a fresh key. Exported for the wire-shape test. */
export function envoyPublishBody(
  topic: string,
  payloadJson: string,
  dedupeKey?: string
): { topic: string; message: string; payload: string; dedupe_key?: string } {
  return {
    topic,
    message: payloadJson,
    payload: payloadJson,
    ...(dedupeKey ? { dedupe_key: dedupeKey } : {}),
  };
}

/** One publish to the Envoy listener's `/v1/messages/publish`, with `config.envoyToken` as the
 * bearer when set (a listener bound off loopback requires one: `ENVOY_API_TOKEN`). A non-2xx
 * answer is `EnvoyPublishError` carrying the listener's status verbatim, so the role lane's
 * propagation and the durable lane's fatal stay as they are; a 401/403 additionally logs one line
 * naming the listener and whether a token went out, since nothing else in the daemon's own log
 * would say why every role publish is refused. */
export async function publishToEnvoy(
  config: Pick<DaemonConfig, "envoyUrl" | "envoyToken">,
  topic: string,
  payloadJson: string,
  dedupeKey?: string,
  fetchImpl: (input: string, init?: RequestInit) => Promise<Response> = fetch
): Promise<void> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (config.envoyToken !== undefined) headers.Authorization = `Bearer ${config.envoyToken}`;
  const response = await fetchImpl(`${config.envoyUrl}/v1/messages/publish`, {
    method: "POST",
    headers,
    body: JSON.stringify(envoyPublishBody(topic, payloadJson, dedupeKey)),
  });
  if (!response.ok) {
    if (response.status === 401 || response.status === 403) {
      console.error(
        `[legion] Envoy listener ${config.envoyUrl} refused the publish to ${topic} (${response.status}) ${config.envoyToken === undefined ? "with no bearer token sent" : "with a bearer token sent"}; the daemon's envoy_token_file (or ENVOY_TOKEN_FILE) must hold the listener's ENVOY_API_TOKEN`
      );
    }
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
      project: primaryProjectKey(config),
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
    envoyPublish: (topic, payloadJson, dedupeKey) =>
      publishToEnvoy(config, topic, payloadJson, dedupeKey),
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
    return await startDaemonLocked(config, deps, instanceLock, probeAbort);
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

/** The Kubernetes API client the in-cluster daemon's probe pod and `KubernetesRuntime` share:
 * tests inject one over a fake API; production resolves credentials from
 * `runtime.kubernetes.kubeconfig` or the pod's service-account files. Needs only the
 * `runtime: kubernetes` block, so it runs before any probe or state load. */
async function connectCluster(
  kubernetes: KubernetesRuntimeConfig,
  deps: DaemonDependencies
): Promise<{ readonly kubernetes: KubernetesRuntimeConfig; readonly client: K8sClient }> {
  const client =
    deps.k8sClient ??
    createK8sClient({
      ...(await resolveK8sCredentials({ kubeconfig: kubernetes.kubeconfig, env: process.env })),
      namespace: kubernetes.namespace,
    });
  return { kubernetes, client };
}

async function startDaemonLocked(
  config: DaemonConfig,
  deps: DaemonDependencies,
  instanceLock: InstanceLock,
  probeAbort: AbortController
): Promise<DaemonHandle> {
  // A Kubernetes daemon on the devbox still owns an interactive tmux controller. An in-cluster
  // daemon retains its operator-launched controller because Kubernetes supplies the service-host
  // environment only to pods.
  const resolveEnvironment = <R extends RuntimeName>(runtime: R) =>
    deps.resolveDaemonEnvironment(config.ompInvocation, {
      run: deps.runner,
      stateDir: config.stateDir,
      runtime,
    });
  const controllerOnHost =
    config.runtime.name !== "kubernetes" || process.env.KUBERNETES_SERVICE_HOST === undefined;
  const tmuxEnvironment = controllerOnHost ? await resolveEnvironment("tmux") : undefined;
  const environment = tmuxEnvironment ?? (await resolveEnvironment("kubernetes"));
  const boot =
    config.runtime.name === "kubernetes"
      ? { runtime: "kubernetes" as const, cluster: await connectCluster(config.runtime, deps) }
      : { runtime: "tmux" as const };
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
  // The boot probe(s) (boot-probes.ts; worker-image-probe.ts) start here but are awaited only at
  // the launch hold below, just before the first pane or pod could open: state load, NATS, the API
  // bind, and the worker reconnect all proceed while a probe is still retrying through host load,
  // so an operator can read `/legion/v1/state` and the durable lane keeps acking meanwhile. The
  // no-op `catch` keeps a definitive negative that lands before the hold from becoming an
  // unhandled rejection; the real handling is at the hold.
  const probeOptions = {
    sleep: abortableSleep(deps.sleep, probeAbort.signal),
    timeoutMs: config.slowCommandTimeoutSeconds * 1000,
    retry: DAEMON_PROBE_RETRY,
    signal: probeAbort.signal,
  };
  let probes: Promise<void>;
  const tmuxProbes =
    tmuxEnvironment &&
    (async () => {
      await verifyOmpAgentsCapability(
        tmuxEnvironment.ompInvocation,
        config.ompLaunchPrefix,
        runner,
        probeOptions
      );
      await verifyLegionPluginLoaded(
        tmuxEnvironment.ompInvocation,
        config.ompLaunchPrefix,
        runner,
        deps.readPluginManifest,
        probeOptions
      );
    });
  if (tmuxProbes) await verifyLegionPluginContract(deps.readPluginManifest);
  if (boot.runtime === "kubernetes") {
    const { kubernetes, client } = boot.cluster;
    const workerImageProbe = verifyWorkerImage(
      {
        client,
        project: config.project,
        namespace: kubernetes.namespace,
        image: kubernetes.image,
        resources: kubernetes.resources.small,
        stateDir: config.stateDir,
        daemonApiVersion: LEGION_DAEMON_API_VERSION,
        sessionStore: kubernetes.sessionStore.kind,
        now: deps.now,
        log: (line) => console.error(line),
      },
      probeOptions
    );
    probes = tmuxProbes
      ? Promise.all([workerImageProbe, tmuxProbes()]).then(() => {})
      : workerImageProbe;
  } else {
    probes = tmuxProbes!();
  }
  probes.catch(() => {});
  const owners = new Set(projectRepos(config).map((repo) => repo.split("/")[0] as string));
  await Promise.all(
    [...owners].flatMap((owner) => [
      deps.tokenManager.getToken("implement", owner),
      deps.tokenManager.getToken("review", owner),
    ])
  );
  const stateFile = path.join(config.stateDir, "state.json");
  const state = await deps.loadState(stateFile, {
    project: config.project,
    cap: config.admissionCap,
    resolveSpecArtifact: specArtifactResolver(deps.dispatchClient),
    // LEGION-16 upgrade step 4: the headless controller pane the previous daemon left running
    // probes alive and keeps its claim, so this daemon never replaces it on its own. One line,
    // once, with the exact command; the pane is not touched.
    onHeadlessControllerStripped: ({ tmuxSession, tmuxPaneId }) => {
      const pane = tmuxPaneId ?? "<controllerLocator.tmuxPaneId>";
      console.error(
        `[legion] controller locator carried a headless shim socket (pane ${pane}); after this boot run: tmux -L ${tmuxSession ?? `legion-${config.project}`} kill-pane -t ${pane} so the interactive controller spawns (LEGION-16 upgrade step 4)`
      );
    },
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
  console.log(
    `[legion] dispatch consumer legion-${primaryProjectKey(config)}-dispatch for projects ${projectKeys(config).join(",")}`
  );

  // `state.project`, not `config.project`: every role token, secret-file name, and pane the
  // manager reasons about is keyed by the persisted project (`ProcessManager` reads
  // `deps.state.project` throughout), so the private server and the runtime's secret-file
  // names must come from the same value — exactly what the manager derived before the runtime
  // boundary existed.
  const provisioningToken = async (owner: string): Promise<string> =>
    (await deps.tokenManager.getToken("implement", owner)).token;
  // Assigned once the API is up (below); the Kubernetes runtime reads it lazily on its first
  // connect/probe/stop, and no spawn can run before then: the launch hold (`enableLaunches()`)
  // comes after the listener starts.
  let workerStream: WorkerStreamListener;
  const tmuxRuntime =
    tmuxEnvironment &&
    new TmuxRuntime({
      tmux: { run: runner, socket: `legion-${state.project}` },
      project: state.project,
      stateDir: config.stateDir,
      ompInvocation: tmuxEnvironment.ompInvocation,
      ompLaunchPrefix: config.ompLaunchPrefix,
      deploymentInstructionsFile,
      statPrompt: deps.statPrompt,
      provisioningToken,
      run: runner,
      repoForIssue: (issue) => repoForIssue(config, issue),
      credentialHelper: daemonCredentialHelper(),
      slowCommandTimeoutMs: config.slowCommandTimeoutSeconds * 1000,
      connectWorkerRpc: deps.connectWorkerRpc,
      workerRpcTimeoutMs: () => config.workerRpcTimeoutSeconds * 1000,
      now: deps.now,
      sleep: deps.sleep,
      readProcessCmdline: deps.readProcessCmdline,
      readProcessStat: deps.readProcessStat,
      issueLocators: (issue) => locatorsForIssue(state, issue),
    });
  const runtime: Runtime =
    boot.runtime === "kubernetes"
      ? new KubernetesRuntime({
          project: state.project,
          config: boot.cluster.kubernetes,
          client: boot.cluster.client,
          listener: () => workerStream,
          repo: config.repo,
          provisioningToken,
          daemonUrl: config.daemonUrl,
          workerStreamPort: config.workerStreamPort,
          workerBootTimeoutMs: config.workerBootTimeoutSeconds * 1000,
          workerBootRegistrationDeadlineIntervals: config.workerBootRegistrationDeadlineIntervals,
          workerStopTimeoutMs: config.workerStopTimeoutSeconds * 1000,
          workerRpcTimeoutMs: () => config.workerRpcTimeoutSeconds * 1000,
          deploymentInstructionsFile,
          envoyUrl: config.envoyUrl,
          envoyToken: config.envoyToken,
          now: deps.now,
          sleep: deps.sleep,
        })
      : tmuxRuntime!;
  const processManager = new ProcessManager({
    state,
    saveState: save,
    config,
    runtime,
    controllerRuntime: tmuxRuntime ?? runtime,
    run: runner,
    processPath: environment.paneEnv.PATH,
    rolePromptsDir: environment.rolePromptsDir,
    credentialHelper: daemonCredentialHelper(),
    // Through the listener, never `nats.publish`: a bare payload on a role subject is rejected by
    // the listener's envelope validation and reaches no holder. A direct runtime notice that gets
    // Envoy's normal 404 no-holder response resumes that role with derived catch-up, as the event
    // pump does; it never holds or replays the raw notice.
    publishRole: (topic, json, dedupeKey) => {
      void deps.envoyPublish(topic, json, dedupeKey).catch((error) => {
        if (
          error instanceof EnvoyPublishError &&
          error.status === 404 &&
          topic.startsWith(ROLE_TOPIC_PREFIX)
        ) {
          void recoverUndeliverableRole(topic.slice(ROLE_TOPIC_PREFIX.length));
          return;
        }
        console.error(`[legion] failed to publish a daemon notice to ${topic}:`, error);
      });
    },
    natsRequest: (subject, data) => nats.request(subject, data),
    mintControllerCapability: async () => api.mintControllerCapability(),
    // Rest arguments, never a named list: a wrapper that names fewer parameters than the dep
    // compiles and drops the rest silently — LEGION-186 lost spawnTree's `expectedSessionId`
    // that way, so no resumed root was ever held to the same-agent rule. Spreading pins the
    // dep's and the API's signatures together; a divergence is a compile error. The closure
    // itself stays: `api` is assigned only after this constructor returns.
    mintBootToken: (...args) => api.mintBootToken(...args),
    mintWorkerBootToken: (...args) => api.mintWorkerBootToken(...args),
    revokeSessionCapability: (sessionId) => api.revokeSessionCapability(sessionId),
    workerCatchup: {
      runner,
      tokenManager: deps.tokenManager,
      ownerForIssue: (issue) => ownerForIssue(config, issue),
      baseEnv: environment.paneEnv,
    },
    dispatchClient: deps.dispatchClient,
    now: deps.now,
    sleep: deps.sleep,
  });

  async function recoverUndeliverableRole(role: string): Promise<void> {
    try {
      await processManager.recoverRole(role);
    } catch (error) {
      console.error(`[legion] onUndeliverable recovery failed for ${role}:`, error);
    }
  }

  const emitOverseerCatchup = async (tree: IssueKey): Promise<void> => {
    const payload = await overseerCatchup(state, tree);
    await deps.envoyPublish(
      roleTopic(roleToken(state.project, tree, "architect")),
      JSON.stringify(payload)
    );
  };

  const onUndeliverable = async (info: UndeliverableInfo): Promise<void> => {
    await recoverUndeliverableRole(info.role);
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
    onProbeWorker: (token) => processManager.probeWorkerClaim(token),
    onAdmit: (issue) => {
      processManager.admit(issue);
    },
    onDequeue: (issue) => processManager.dequeue(issue),
    onUndeliverable,
    config,
  };
  const eventPump: EventPump = startEventPump(eventDeps);
  const fetchCiStatusBatch = createCiStatusFetcher(
    deps.tokenManager,
    deps.runner,
    environment.paneEnv
  );

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
          reconcileAdmissionDrift: () => processManager.reconcileAdmissionDrift(),
          isResurrecting: (issue) => processManager.isResurrecting(issue),
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
      projects: config.projects,
      gates: config.gates,
      operatorToken: config.operatorToken,
    },
    apiDeps
  );
  // Bound with the API and torn down with it. A bind failure is startup-fatal: stop the API
  // server it would have partnered so nothing half-listens behind the instance lock's release.
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
  // A pod-runtime daemon needs its host-side controller even with no pending notice. Its launch
  // remains held until both the host and worker-image probes pass. Tmux-only daemons retain the
  // existing demand-driven controller start.
  if (state.controllerPendingNotices.length > 0) {
    if (state.roles[controllerToken(state.project)]) {
      void eventPump.drainControllerNotices().catch((error) => {
        console.error(`[legion] boot-time controller notice drain failed:`, error);
      });
    } else {
      void processManager.ensureController().catch((error) => {
        console.error(`[legion] boot-time controller spawn failed:`, error);
      });
    }
  } else if (boot.runtime === "kubernetes" && tmuxRuntime) {
    void processManager.ensureController().catch((error) => {
      console.error(`[legion] boot-time controller spawn failed:`, error);
    });
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
  //
  // Then, once they pass, the server those panes would open into. The private tmux server
  // survives daemon restarts and hands every new pane two environment tables beneath that pane's
  // `-e` pairs: its global table (the environment it was forked with — an earlier daemon's, not
  // this one's `paneEnv` — plus whatever the operator's `tmux.conf` `set-environment -g`s) and the
  // session table tmux's default `update-environment` fills from every operator attach
  // (`SSH_AUTH_SOCK`, `SSH_CONNECTION`, …). Whatever `paneEnv` would not pass is removed from both
  // here and the session's `update-environment` emptied (names logged, never values); a server
  // this daemon forked itself carries exactly `paneEnv` and removes nothing. A tmux failure here
  // is as fatal as a failed probe: no pane may open into a server this daemon could not inspect.
  try {
    await probes;
    const removed = tmuxRuntime ? await tmuxRuntime.scrubServerEnvironment(environment.paneEnv) : [];
    if (removed.length > 0) {
      console.warn(
        `[legion] removed ${removed.length} variable(s) from the private tmux server environment that panes may not inherit: ${removed.join(", ")}`
      );
    }
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
