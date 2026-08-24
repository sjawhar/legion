import { stat } from "node:fs/promises";
import path from "node:path";
import { controllerToken, type IssueKey, roleToken, roleTopic } from "@legion/contracts";
import { connect, StringCodec, type Subscription } from "nats";
import type { CommandRunner } from "../state/fetch";
import { defaultRunner } from "../state/fetch";
import { fetchGitHubProjectItems } from "../state/github-fetch";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "./api";
import { setApprovalStatus } from "./approval-check";
import { overseerCatchup } from "./catchup";
import { type DaemonConfig, loadConfig } from "./config";
import { type EventPumpDeps, startEventPump } from "./events";
import { TokenManager } from "./github-apps";
import { loadState, saveState } from "./legion-state";
import { ProcessManager, type ProcessManagerDeps } from "./processes";
import { runResync } from "./resync";

const LINGER_SWEEP_INTERVAL_MS = 60_000;

export interface NatsTransport {
  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void;
  publish(subject: string, data: string): void;
  ready(): Promise<void>;
  close(): Promise<void>;
}

interface DaemonDependencies {
  loadState: typeof loadState;
  saveState: typeof saveState;
  createNatsTransport(config: DaemonConfig): Promise<NatsTransport>;
  runner: CommandRunner;
  statPrompt: NonNullable<ProcessManagerDeps["statPrompt"]>;
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  fetchGitHubProjectItems(): Promise<{ items: Record<string, unknown>[] }>;
  tokenManager: Pick<TokenManager, "getToken">;
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

function defaultDependencies(config: DaemonConfig): DaemonDependencies {
  const board = projectBoard(config.legionId);
  const tokenManager = new TokenManager(config.githubApps);
  return {
    loadState,
    saveState,
    createNatsTransport,
    runner: defaultRunner,
    statPrompt: stat,
    envoyPublish: (topic, payloadJson) => publishToEnvoy(config, topic, payloadJson),
    fetchGitHubProjectItems: () => fetchGitHubProjectItems(board.owner, board.number),
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
  const deps = { ...defaultDependencies(config), ...options.deps };
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
    run: deps.runner,
    natsPublish: (subject, data) => nats.publish(subject, data),
    mintControllerCapability: async () => api.mintControllerCapability(),
    statPrompt: deps.statPrompt,
    now: deps.now,
  });

  const emitOverseerCatchup = async (tree: IssueKey): Promise<void> => {
    const payload = await overseerCatchup(state, tree);
    await deps.envoyPublish(
      roleTopic(roleToken(state.project, tree, "architect")),
      JSON.stringify(payload)
    );
  };

  const apiDeps: LegionApiDeps = {
    state,
    saveState: save,
    runner: deps.runner,
    tokenManager: deps.tokenManager,
    processManager,
    envoyPublish: deps.envoyPublish,
    onTreeReady: emitOverseerCatchup,
  };
  api = startLegionApi({ port: config.port, hostname: "127.0.0.1", gates: config.gates }, apiDeps);

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
        runner: deps.runner,
        tokenManager: deps.tokenManager,
        appLogins: config.appLogins,
        gatesMerge: config.gates.merge,
      }),
    config,
  };
  const eventPump = startEventPump(eventDeps);
  const ready = nats.ready();

  const emitResync = async (): Promise<void> => {
    const payload = await runResync({
      state,
      config,
      fetchGitHubProjectItems: deps.fetchGitHubProjectItems,
      now: deps.now,
    });
    await deps.envoyPublish(roleTopic(controllerToken(state.project)), JSON.stringify(payload));
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
      if (Date.parse(tree.lingerUntil) <= now) processManager.expireLinger(tree.root);
    }
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
