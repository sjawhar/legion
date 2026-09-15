import type { JjIdentity } from "@legion/workspace";
import {
  awaitShutdown,
  type Locator,
  type ProbeResult,
  type Runtime,
  type SpawnSpec,
} from "../runtime";
import type { PromptReceipt, WorkerRpcClient } from "../worker-rpc";

export type FakeWorkerRpcClient = WorkerRpcClient & {
  prompts: string[];
  deliveryIds: string[];
  negotiated: boolean;
  getStateCalls: number;
  getStateImpl?: () => Promise<Record<string, unknown>>;
  /** Every `adopt-working-copy` frame this client was asked to send, in order. */
  adoptions: Array<{ jjUser: string; jjEmail: string; timeoutMs: number }>;
  /** The shim's answer to the next adoption frame; a rejection models `ok: false`. */
  adoptImpl?: () => Promise<void>;
  idleFireCount: number;
  /** Whether `prompt()` starts the turn the moment it is acknowledged (the default: a healthy
   * worker's `agent_start` follows the acknowledgement). A test sets it `false` to model an
   * acknowledgement no turn follows; `emitRunState("running")` then plays the `agent_start`. */
  turnStartsOnPrompt: boolean;
  /** `"running"` models the worker's `agent_start` frame: it also settles the last prompt's
   * pending receipt. `"idle"` models `agent_end`, firing `onIdle` on a genuine transition. */
  emitRunState(state: "running" | "idle"): void;
  emitLateRefusal(): void;
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
  /** The last prompt's not-yet-started receipt, exactly like the real client's one slot. */
  let pendingTurnStart: { start(): void } | undefined;
  let lateRefusal: (() => void) | undefined;
  const observeTurnStart = (): void => {
    const slot = pendingTurnStart;
    pendingTurnStart = undefined;
    slot?.start();
  };
  const client = {
    closed: closed.promise,
    get runState() {
      return runState;
    },
    prompts: [] as string[],
    deliveryIds: [] as string[],
    negotiated: false,
    getStateCalls: 0,
    getStateImpl: undefined as (() => Promise<Record<string, unknown>>) | undefined,
    /** Every `adopt-working-copy` frame this client was asked to send, in order. */
    adoptions: [] as Array<{ jjUser: string; jjEmail: string; timeoutMs: number }>,
    /** The shim's answer to the next adoption frame; a rejection models `ok: false`. */
    adoptImpl: undefined as (() => Promise<void>) | undefined,
    idleFireCount: 0,
    turnStartsOnPrompt: true,
    async adoptWorkingCopy(identity: JjIdentity, timeoutMs: number) {
      client.adoptions.push({ ...identity, timeoutMs });
      await client.adoptImpl?.();
    },
    async negotiate() {
      client.negotiated = true;
    },
    async prompt(
      message: string,
      deliveryId: string,
      onLateRefusal?: () => void
    ): Promise<PromptReceipt> {
      const previousRunState = runState;
      runState = "running";
      client.prompts.push(message);
      client.deliveryIds.push(deliveryId);
      lateRefusal = onLateRefusal;
      let hasStarted = false;
      const started = Promise.withResolvers<void>();
      const slot = {
        start() {
          hasStarted = true;
          runState = "running";
          started.resolve();
        },
      };
      pendingTurnStart = slot;
      if (client.turnStartsOnPrompt) observeTurnStart();
      return {
        turnStarted: started.promise,
        get hasStarted() {
          return hasStarted;
        },
        abandonWait() {
          if (hasStarted || pendingTurnStart !== slot || runState !== "running") return;
          runState = previousRunState;
        },
      };
    },
    async getState() {
      client.getStateCalls += 1;
      const response = client.getStateImpl ? await client.getStateImpl() : {};
      // Mirrors the real client: a stream in progress is a turn observed to start. An
      // `isStreaming: false` answer is left to the test-supplied `getStateImpl` to model (an
      // explicit `emitRunState("idle")`), exactly as the idle-worker fixtures already do.
      const data = response.data;
      if (
        typeof data === "object" &&
        data !== null &&
        "isStreaming" in data &&
        data.isStreaming === true
      ) {
        runState = "running";
        observeTurnStart();
      }
      return response;
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
      if (state === "running") observeTurnStart();
      if (state === "idle" && !wasIdle) {
        client.idleFireCount += 1;
        idleCallback?.();
      }
    },
    emitLateRefusal() {
      lateRefusal?.();
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
  readonly launchesController: boolean;
  readonly removesWorkspacesOnTreeClose: boolean;
  readonly spawned: Array<{ kind: "root" | "worker" | "controller"; spec: SpawnSpec }> = [];
  readonly stopped: Array<{
    locator: Locator;
    timeoutMs: number;
    options: { skipGraceful?: boolean; refuseKill?: boolean } | undefined;
  }> = [];
  readonly reconciled: Array<{ known: ReadonlySet<string>; graceMs: number }> = [];
  readonly connects: Locator[] = [];
  readonly adoptions: Array<{
    issue: string;
    role: string;
    identity: JjIdentity;
    timeoutMs: number;
  }> = [];
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
      adoptWorkingCopy?: (
        issue: string,
        role: string,
        identity: JjIdentity,
        timeoutMs: number
      ) => Promise<void>;
      /** `false` models the Kubernetes runtime, which does not launch the controller. */
      launchesController?: boolean;
      removesWorkspacesOnTreeClose?: boolean;
    } = {}
  ) {
    this.launchesController = options.launchesController ?? true;
    this.removesWorkspacesOnTreeClose = options.removesWorkspacesOnTreeClose ?? true;
  }

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

  /** The process at `locator` died — pane killed, pod crashed. `probe` answers `dead`/`gone` and
   * `connect` refuses from now on. Its stream to the daemon closes too (a killed shim's socket)
   * unless `closeSocket: false`, which models a half-open connection the daemon never hears close
   * — what only the resync probe can catch. */
  crash(locator: Locator, options: { closeSocket?: boolean } = {}): void {
    const uid = this.uid(locator);
    const process = this.processes.get(uid);
    if (!process) throw new Error(`fake runtime: no live process for ${uid}`);
    this.processes.delete(uid);
    if (options.closeSocket !== false) process.client?.close();
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
      roleToken: `${spec.issue ?? "controller"}-${spec.role}`,
    };
    this.spawned.push({ kind, spec });
    this.processes.set(locator.podUid, { kind, spec, locator });
    return locator;
  }

  async adoptWorkingCopy(
    issue: string,
    role: string,
    identity: JjIdentity,
    timeoutMs: number
  ): Promise<void> {
    this.adoptions.push({ issue, role, identity, timeoutMs });
    await this.options.adoptWorkingCopy?.(issue, role, identity, timeoutMs);
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
    // Never destroys what a stranger holds, and never anything when the caller refused the kill.
    const destroy = !options?.refuseKill && !stranger && this.processes.has(uid);
    this.stopped.push({ locator, timeoutMs, options });
    if (destroy) this.processes.delete(uid);
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
