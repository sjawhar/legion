import { awaitShutdown, type Locator, type Runtime, type SpawnSpec } from "../runtime";
import type { WorkerRpcClient } from "../worker-rpc";

export type FakeWorkerRpcClient = WorkerRpcClient & {
  prompts: string[];
  negotiated: boolean;
  getStateCalls: number;
  getStateImpl?: () => Promise<Record<string, unknown>>;
  idleFireCount: number;
  emitRunState(state: "running" | "idle"): void;
  /** Sets `runState` directly, bypassing the idle trigger entirely — models a real client's
   * post-rejection restore (an undo, never a transition; see `WorkerRpcClient.prompt`'s doc
   * comment), as opposed to `emitRunState`, which fires `onIdle` on a genuine idle transition. */
  setRunStateSilently(state: "unknown" | "running" | "idle"): void;
};

/** The in-memory `WorkerRpcClient` every daemon test drives a fake worker through. */
export function fakeWorkerRpcClient(): FakeWorkerRpcClient {
  const closed = Promise.withResolvers<void>();
  let idleCallback: (() => void) | undefined;
  let runState: "unknown" | "running" | "idle" = "unknown";
  const client = {
    closed: closed.promise,
    get runState() {
      return runState;
    },
    prompts: [] as string[],
    negotiated: false,
    getStateCalls: 0,
    getStateImpl: undefined as (() => Promise<Record<string, unknown>>) | undefined,
    idleFireCount: 0,
    async negotiate() {
      client.negotiated = true;
    },
    async prompt(message: string) {
      runState = "running";
      client.prompts.push(message);
    },
    async getState() {
      client.getStateCalls += 1;
      return client.getStateImpl ? client.getStateImpl() : {};
    },
    shutdown() {
      // Mirrors the real shim: the frame alone never closes the socket — the shim closes it only
      // once OMP actually exits, asynchronously relative to receiving the frame. Deferred by a
      // microtask (never synchronous) so a test asserting the graceful path is decided by real
      // promise ordering against `stopProcess`'s timeout race, not by `closed` already having
      // settled before that race was even built.
      queueMicrotask(() => client.close());
    },
    close() {
      const wasIdle = runState === "idle";
      runState = "idle";
      closed.resolve();
      if (!wasIdle) {
        client.idleFireCount += 1;
        idleCallback?.();
      }
    },
    onIdle(callback: () => void) {
      idleCallback = callback;
    },
    emitRunState(state: "running" | "idle") {
      const wasIdle = runState === "idle";
      runState = state;
      if (state === "idle" && !wasIdle) {
        client.idleFireCount += 1;
        idleCallback?.();
      }
    },
    setRunStateSilently(state: "unknown" | "running" | "idle") {
      runState = state;
    },
  };
  return client;
}

interface FakeProcess {
  kind: "root" | "worker" | "controller";
  spec: SpawnSpec;
  locator: Locator;
  client?: WorkerRpcClient;
}

/**
 * An in-memory `Runtime`: spawned processes live in a table keyed by a counter id and are
 * "alive" while they are in it. Locators are the kubernetes union member on purpose — nothing
 * tmux-shaped can leak through a test built on this fake.
 */
export class FakeRuntime implements Runtime {
  readonly spawned: Array<{ kind: "root" | "worker" | "controller"; spec: SpawnSpec }> = [];
  readonly stopped: Array<{
    locator: Locator;
    timeoutMs: number;
    options: { skipGraceful?: boolean } | undefined;
  }> = [];
  readonly reconciled: Array<{ known: ReadonlySet<string>; graceMs: number }> = [];
  readonly connects: Locator[] = [];
  private readonly processes = new Map<string, FakeProcess>();
  private nextId = 1;

  constructor(
    private readonly options: {
      clientFactory?: () => WorkerRpcClient;
      sleep?: (ms: number) => Promise<void>;
    } = {}
  ) {}

  async spawn(kind: "root" | "worker" | "controller", spec: SpawnSpec): Promise<Locator> {
    const id = this.nextId;
    this.nextId += 1;
    const locator: Locator = {
      runtime: "kubernetes",
      namespace: "fake",
      podName: `${spec.role}-${id}`,
      podUid: `uid-${id}`,
      pvcName: "fake-pvc",
    };
    this.spawned.push({ kind, spec });
    this.processes.set(locator.podUid, { kind, spec, locator });
    return locator;
  }

  async probe(locator: Locator): Promise<{ status: "alive" | "dead" | "unknown"; pid?: number }> {
    return { status: this.processes.has(this.uid(locator)) ? "alive" : "dead" };
  }

  async connect(locator: Locator): Promise<WorkerRpcClient> {
    this.connects.push(locator);
    const process = this.processes.get(this.uid(locator));
    if (!process) throw new Error(`fake runtime: no process for ${this.uid(locator)}`);
    process.client ??= (this.options.clientFactory ?? fakeWorkerRpcClient)();
    return process.client;
  }

  async stop(
    locator: Locator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean }
  ): Promise<void> {
    this.stopped.push({ locator, timeoutMs, options });
    const process = this.processes.get(this.uid(locator));
    if (process?.client && !options?.skipGraceful) {
      await awaitShutdown(process.client, timeoutMs, this.options.sleep);
    }
    this.processes.delete(this.uid(locator));
  }

  async reconcileOrphans(known: ReadonlySet<string>, graceMs: number): Promise<void> {
    this.reconciled.push({ known, graceMs });
    for (const [uid, process] of this.processes) {
      if (process.locator.runtime === "kubernetes" && !known.has(process.locator.podName)) {
        this.processes.delete(uid);
      }
    }
  }

  private uid(locator: Locator): string {
    if (locator.runtime !== "kubernetes") {
      throw new Error("fake runtime only operates kubernetes-shaped locators");
    }
    return locator.podUid;
  }
}
