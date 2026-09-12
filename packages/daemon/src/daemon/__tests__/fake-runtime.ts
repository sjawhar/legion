import {
  awaitShutdown,
  type Locator,
  type ProbeResult,
  type Runtime,
  type SpawnSpec,
} from "../runtime";
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
 * tmux-shaped can leak through a test built on this fake. `occupyHandle` moves a process out of
 * the alive table into `strangers`: its handle is now held by some other process (tmux: a pane
 * id reissued to another role, or a legacy locator with no identity to verify), so `probe`
 * answers `dead`/`not-recorded-process` and `stop` never destroys what is there — the recorded
 * process is still reachable over its own socket for the graceful ask, mirroring a still-live
 * legacy process, and `strangers` keeps the entry until a test inspects it.
 */
export class FakeRuntime implements Runtime {
  readonly spawned: Array<{ kind: "root" | "worker" | "controller"; spec: SpawnSpec }> = [];
  readonly stopped: Array<{
    locator: Locator;
    timeoutMs: number;
    options: { skipGraceful?: boolean; refuseKill?: boolean } | undefined;
    /** Whether the destroy step ran: never for a handle a stranger occupies. */
    destroyed: boolean;
  }> = [];
  readonly reconciled: Array<{ known: ReadonlySet<string>; graceMs: number }> = [];
  readonly connects: Locator[] = [];
  /** Handles some other process now occupies, with the `detail` `probe` reports for each and
   * whether the recorded process is still reachable over its own socket. */
  readonly strangers = new Map<
    string,
    { process: FakeProcess; detail: string; reachable: boolean }
  >();
  private readonly processes = new Map<string, FakeProcess>();
  private nextId = 1;

  constructor(
    private readonly options: {
      clientFactory?: () => WorkerRpcClient;
      sleep?: (ms: number) => Promise<void>;
    } = {}
  ) {}

  /** The handle at `locator` is no longer the recorded process's: see the class doc. The recorded
   * process stays reachable over its socket (a still-live legacy process) unless `reachable` is
   * false (it is gone; a stranger merely wears its handle). */
  occupyHandle(locator: Locator, options: { detail?: string; reachable?: boolean } = {}): void {
    const uid = this.uid(locator);
    const process = this.processes.get(uid);
    if (!process) throw new Error(`fake runtime: no live process for ${uid}`);
    this.processes.delete(uid);
    this.strangers.set(uid, {
      process,
      detail: options.detail ?? `handle ${uid} now runs another process`,
      reachable: options.reachable ?? true,
    });
  }

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

  async probe(locator: Locator): Promise<ProbeResult> {
    const uid = this.uid(locator);
    if (this.processes.has(uid)) return { status: "alive" };
    const stranger = this.strangers.get(uid);
    if (stranger)
      return { status: "dead", reason: "not-recorded-process", detail: stranger.detail };
    return { status: "dead", reason: "gone" };
  }

  async connect(locator: Locator): Promise<WorkerRpcClient> {
    this.connects.push(locator);
    const uid = this.uid(locator);
    const stranger = this.strangers.get(uid);
    if (stranger && !stranger.reachable) throw new Error(`fake runtime: ECONNREFUSED ${uid}`);
    const process = this.processes.get(uid) ?? stranger?.process;
    if (!process) throw new Error(`fake runtime: no process for ${uid}`);
    process.client ??= (this.options.clientFactory ?? fakeWorkerRpcClient)();
    return process.client;
  }

  async stop(
    locator: Locator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean; refuseKill?: boolean }
  ): Promise<void> {
    const uid = this.uid(locator);
    const stranger = this.strangers.get(uid);
    const process = this.processes.get(uid) ?? stranger?.process;
    if (process?.client && !options?.skipGraceful && stranger?.reachable !== false) {
      await awaitShutdown(process.client, timeoutMs, this.options.sleep);
    }
    const destroyed = !options?.refuseKill && !this.strangers.has(uid) && this.processes.has(uid);
    this.stopped.push({ locator, timeoutMs, options, destroyed });
    if (destroyed) this.processes.delete(uid);
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
