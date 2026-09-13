import path from "node:path";
import type { IssueKey, LegionRole } from "@legion/contracts";
import { createCancellableSleep } from "./cancellable-sleep";
import { probeWorkerSocket, type SocketProbeResult, type WorkerRpcClient } from "./worker-rpc";

/** The daemon CLI every spawned process's `legion worker-shim` wrapper re-executes. */
export const DAEMON_CLI_ENTRYPOINT = path.resolve(import.meta.dir, "../cli/index.ts");

/** Where a tmux-runtime process lives: its window, its pane, the shim socket it listens on, and
 * the identity of the process the pane was opened with. `tmuxPaneId`/`socketPath` are optional
 * only for records predating those fields; every locator the tmux runtime writes carries both. */
export interface TmuxWindowLocator {
  tmuxSession: string;
  tmuxWindowId: string;
  tmuxPaneId?: string;
  socketPath?: string;
  ompSessionFile?: string;
  /** The pane's root process id as tmux reported it at launch, paired with `paneStartTicks`:
   * together the identity `TmuxRuntime` re-checks before ever trusting or killing this pane. A
   * recreated tmux server hands out the same pane ids again, so the id alone can name some other
   * role's live process. Absent only on a locator persisted before this field existed; such a
   * locator never verifies and probes dead (`reason: "not-recorded-process"`) on its first probe. */
  panePid?: number;
  /** Field 22 of `/proc/<panePid>/stat` (start time in clock ticks since boot), read at launch. */
  paneStartTicks?: number;
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

/**
 * A runtime's liveness verdict on a locator. A `dead` verdict always says which kind: `gone` --
 * nothing is where the locator points (tmux: the pane no longer exists; kubernetes: the pod is
 * missing) -- or `not-recorded-process` -- something is there, but it is not the process this
 * locator recorded (tmux: a pane id reissued to another role's process, or a legacy locator with
 * no identity to verify). The two are decided differently by `ProcessManager`: a gone process has
 * nothing left to ask, while a present-but-not-recorded one may still be exactly the daemon's own
 * process reachable over the locator's role-scoped socket, so it is still asked to shut down and
 * only the runtime's own kill is refused. `detail` is the runtime's one-line description of both
 * identities (what is there now, what was recorded) for the decision's log line.
 */
export type ProbeResult =
  | { status: "alive"; pid?: number }
  | { status: "dead"; reason: "gone" }
  | { status: "dead"; reason: "not-recorded-process"; detail: string }
  | { status: "unknown" };

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
  /** Asks the process to exit gracefully (unless `skipGraceful`), then destroys whatever is at
   * the locator only if it still verifies as the recorded process -- never a stranger wearing a
   * reused handle. `refuseKill` tells the runtime the caller's own probe already found the
   * target is not the recorded process (and logged it): the graceful ask still goes out, the
   * destroy step is skipped without re-verifying or re-logging. */
  stop(
    locator: Locator,
    timeoutMs: number,
    options?: { skipGraceful?: boolean; refuseKill?: boolean }
  ): Promise<void>;
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
 * (absent) process; a locator from one runtime never matches one from another. For tmux the
 * pane id alone is not identity -- a reissued id can name another process -- so the recorded
 * pid and start ticks must match too (two legacy locators without them compare by pane id). */
export function sameProcess(a: Locator | undefined, b: Locator | undefined): boolean {
  if (a === undefined || b === undefined) return a === b;
  if (a.runtime === "tmux") {
    return (
      b.runtime === "tmux" &&
      a.tmuxPaneId === b.tmuxPaneId &&
      a.panePid === b.panePid &&
      a.paneStartTicks === b.paneStartTicks
    );
  }
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
