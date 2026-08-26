import { randomUUID } from "node:crypto";
import { mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type IssueKey, roleToken, roleTopic } from "@legion/contracts";
import { connect, StringCodec, type Subscription } from "nats";
import type { CommandRunner } from "../state/fetch";
import { defaultRunner } from "../state/fetch";
import { fetchGitHubProjectItems, type GitHubProjectItemsResult } from "../state/github-fetch";
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
import { loadState, saveState } from "./legion-state";
import { daemonCredentialHelper, ProcessManager, type ProcessManagerDeps } from "./processes";
import { runResync } from "./resync";

const LINGER_SWEEP_INTERVAL_MS = 60_000;
const OMP_AGENTS_CAPABILITY_MARKER = "LEGION_OMP_AGENTS=available";
const OMP_AGENTS_CAPABILITY_PROBE = `export default function probeOmpAgents(pi) {
  process.stderr.write(pi.agents ? "LEGION_OMP_AGENTS=available\\n" : "LEGION_OMP_AGENTS=missing\\n");
}
`;

export interface NatsTransport {
  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void;
  publish(subject: string, data: string): void;
  request(subject: string, data: string): Promise<string>;
  ready(): Promise<void>;
  close(): Promise<void>;
}

interface DaemonDependencies {
  loadState: typeof loadState;
  saveState: typeof saveState;
  createNatsTransport(config: DaemonConfig): Promise<NatsTransport>;
  runner: CommandRunner;
  statPrompt: NonNullable<ProcessManagerDeps["statPrompt"]>;
  readProcessCmdline?: ProcessManagerDeps["readProcessCmdline"];
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

async function createNatsTransport(config: DaemonConfig): Promise<NatsTransport> {
  const connection = await connect({
    servers: config.natsUrls,
    name: `legion-daemon-${config.project}`,
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
  });
  const codec = StringCodec();
  const subscriptions = new Set<Subscription>();

  return {
    subscribe(subject, callback) {
      const subscription = connection.subscribe(subject);
      subscriptions.add(subscription);
      void (async () => {
        for await (const message of subscription) {
          callback(message.subject, codec.decode(message.data));
        }
      })();
      return () => {
        subscriptions.delete(subscription);
        subscription.unsubscribe();
      };
    },
    publish(subject, data) {
      connection.publish(subject, codec.encode(data));
    },
    async request(subject, data) {
      const reply = await connection.request(subject, codec.encode(data), {
        timeout: 10_000,
      });
      return codec.decode(reply.data);
    },
    ready() {
      return connection.flush();
    },
    async close() {
      for (const subscription of subscriptions) subscription.unsubscribe();
      subscriptions.clear();
      await connection.drain();
    },
  };
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
    throw new Error(`Envoy publish to ${topic} failed with status ${response.status}`);
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

function defaultDependencies(config: DaemonConfig): DaemonDependencies {
  const board = projectBoard(config.legionId);
  const tokenManager = new TokenManager(config.githubApps);
  return {
    loadState,
    saveState,
    createNatsTransport,
    runner: defaultRunner,
    resolveDaemonEnvironment,
    statPrompt: stat,
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
  config.appLogins = await resolveConfiguredAppLogins(config, deps.tokenManager, board.owner);
  const environment = await deps.resolveDaemonEnvironment(config.ompInvocation, {
    run: deps.runner,
  });
  const runner = createDaemonRunner(environment, deps.runner);
  await verifyOmpAgentsCapability(environment.ompInvocation, runner);
  await deps.tokenManager.getToken("implement", board.owner);
  const stateFile = path.join(config.stateDir, "state.json");
  const state = await deps.loadState(stateFile, {
    project: config.project,
    cap: config.admissionCap,
  });
  state.admission.cap = config.admissionCap;
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
    provisioningToken: async (owner) =>
      (await deps.tokenManager.getToken("implement", owner)).token,
    statPrompt: deps.statPrompt,
    readProcessCmdline: deps.readProcessCmdline,
    workerCatchup: { runner, tokenManager: deps.tokenManager },
    now: deps.now,
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
    onLinger: async (tree) => {
      processManager.beginLinger(tree);
    },
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
    dispatch: { url: config.dispatchUrl, bearer: config.dispatchBearer },
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
  const ready = nats.ready();

  const emitResync = async (): Promise<void> => {
    const payload = await runResync({
      state,
      config,
      fetchGitHubProjectItems: deps.fetchGitHubProjectItems,
      applyEffects: eventPump.applyEffects,
      now: deps.now,
    });
    console.log(
      `[legion] resync complete: anomalies=${payload.anomalies.length} healed=${payload.healed} reconciled-labels=${payload.reconciledLabels} excluded-null-content-items=${payload.excludedNullContentItems}`
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
