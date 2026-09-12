import path from "node:path";
import type { IssueKey, LegionRole } from "@legion/contracts";
import { createCancellableSleep } from "./cancellable-sleep";
import { probeWorkerSocket, type SocketProbeResult, type WorkerRpcClient } from "./worker-rpc";

/** The daemon CLI every spawned process's `legion worker-shim` wrapper re-executes. */
export const DAEMON_CLI_ENTRYPOINT = path.resolve(import.meta.dir, "../cli/index.ts");

/** Where a tmux-runtime process lives: its window, its pane, and the shim socket it listens on.
 * `tmuxPaneId`/`socketPath` are optional only for records predating those fields; every locator
 * the tmux runtime writes carries both. */
export interface TmuxWindowLocator {
  tmuxSession: string;
  tmuxWindowId: string;
  tmuxPaneId?: string;
  socketPath?: string;
  ompSessionFile?: string;
}

/** Where a Kubernetes-runtime process lives (root spec section 3): one pod per process
 * generation, on the tree's PVC. */
export interface K8sLocator {
  namespace: string;
  podName: string;
  podUid: string;
  pvcName: string;
  ompSessionFile?: string;
}

export type TmuxLocator = { runtime: "tmux" } & TmuxWindowLocator;
export type Locator = TmuxLocator | ({ runtime: "kubernetes" } & K8sLocator);

/** Root spec section 3 profile shape; unused by the tmux runtime. */
export interface RoleResources {
  requests: { cpu: string; memory: string; ephemeralStorage: string };
  limits: { cpu: string; memory: string; ephemeralStorage: string };
}

export type SpawnSpec = {
  /** Absent only for kind "controller". */
  issue?: IssueKey;
  role: LegionRole | "controller";
  workspaceDir: string;
  env: Record<string, string | undefined>;
  innerCommand: string;
  resources?: RoleResources;
  /** name -> value; the runtime decides delivery (tmux: 0600 `<stateDir>/secrets/<role token>` +
   * `<NAME>_FILE` env). */
  secrets: Record<string, string>;
};

export interface ProbeResult {
  status: "alive" | "dead" | "unknown";
  pid?: number;
}

/**
 * How a Legion process is started, probed, reached, stopped, and swept. `ProcessManager` owns
 * the lifecycle (admission, generations, claims, launch-failure accounting, grants, linger/close,
 * its per-token RPC client cache) and drives every runtime operation through one injected
 * implementation of this interface, never reading a runtime-specific locator field itself.
 */
export interface Runtime {
  spawn(kind: "root" | "worker" | "controller", spec: SpawnSpec): Promise<Locator>;
  probe(locator: Locator): Promise<ProbeResult>;
  /** A raw dial: never negotiates, never caches. `ProcessManager.clientFor` owns both. */
  connect(locator: Locator, timeoutMs?: number): Promise<WorkerRpcClient>;
  stop(locator: Locator, timeoutMs: number, options?: { skipGraceful?: boolean }): Promise<void>;
  /** Reaps every process this runtime owns whose handle (see `locatorHandles`) is not in `known`
   * and that has been idle at least `graceMs`. */
  reconcileOrphans(known: ReadonlySet<string>, graceMs: number): Promise<void>;
}

/** A runtime could not confirm a process stopped (tmux: a real kill-pane failure).
 * `ProcessManager` rethrows it as `StopFailed(token, message)`. */
export class ProcessStopFailed extends Error {
  constructor(
    readonly locator: Locator,
    message: string
  ) {
    super(message);
    this.name = "ProcessStopFailed";
  }
}

/** Locator identity: the same process, not merely the same record. Two `undefined`s are the same
 * (absent) process; a locator from one runtime never matches one from another. */
export function sameProcess(a: Locator | undefined, b: Locator | undefined): boolean {
  if (a === undefined || b === undefined) return a === b;
  if (a.runtime === "tmux") return b.runtime === "tmux" && a.tmuxPaneId === b.tmuxPaneId;
  return b.runtime === "kubernetes" && a.podUid === b.podUid;
}

/** Handles `Runtime.reconcileOrphans` recognizes as known. tmux: the window id plus either the
 * pane id or `<windowId>/*` (no recorded pane id: exempt every pane of that window). kubernetes:
 * the pod name. */
export function locatorHandles(locator: Locator): readonly string[] {
  if (locator.runtime === "tmux") {
    return [locator.tmuxWindowId, locator.tmuxPaneId ?? `${locator.tmuxWindowId}/*`];
  }
  return [locator.podName];
}

/** A bounded wait whose underlying timer is cancellable, so a graceful stop that resolves quickly
 * doesn't leave a stray one running for the rest of `ms`. A test-injected `sleep` has no real
 * timer behind it to cancel. */
export function boundedWait(
  ms: number,
  sleep?: (ms: number) => Promise<void>
): { timedOut: Promise<boolean>; cancel: () => void } {
  if (sleep) return { timedOut: sleep(ms).then(() => true), cancel: () => {} };
  const timer = createCancellableSleep();
  return { timedOut: timer.sleep(ms).then(() => true), cancel: () => timer.cancel() };
}

/** Sends the shim a `{type:"shutdown"}` frame, THEN creates the bounded wait, THEN races it
 * against the socket closing — that order is load-bearing for callers whose `closed` reactions
 * were registered at connect time. Returns true when `closed` settled cleanly before the timeout;
 * a rejection while waiting is NOT proof the process exited and counts as unconfirmed, exactly
 * like a timeout. */
export async function awaitShutdown(
  client: WorkerRpcClient,
  timeoutMs: number,
  sleep?: (ms: number) => Promise<void>
): Promise<boolean> {
  client.shutdown();
  const { timedOut, cancel } = boundedWait(timeoutMs, sleep);
  const unconfirmed = await Promise.race([
    client.closed.then(
      () => false,
      () => true
    ),
    timedOut,
  ]);
  cancel();
  return !unconfirmed;
}

/** `probeWorkerSocket` over a locator-bound connect: its `socketPath` parameter only ever reaches
 * `connect`, which ignores it here. worker-rpc.ts is LEGION-22's file; this wrapper is the one
 * place that wart lives. */
export function probeWorker(
  connect: () => Promise<WorkerRpcClient>,
  timeoutMs: number
): Promise<SocketProbeResult> {
  return probeWorkerSocket(connect, "", timeoutMs);
}

export function shellPath(value: string): string {
  return /[^A-Za-z0-9_./:-]/.test(value) ? `'${value.replaceAll("'", "'\\''")}'` : value;
}
