import { stat } from "node:fs/promises";
import path from "node:path";
import type { IssueKey, LegionRole } from "@legion/contracts";
import type { JjIdentity } from "@legion/workspace";
import { createCancellableSleep } from "./cancellable-sleep";
import { probeWorkerSocket, type SocketProbeResult, type WorkerRpcClient } from "./worker-rpc";

/** The daemon CLI every spawned process's `legion worker-shim` wrapper re-executes. */
export const DAEMON_CLI_ENTRYPOINT = path.resolve(import.meta.dir, "../cli/index.ts");

/** Where a tmux-runtime process lives: its window, its pane, the shim socket a shim-bridged
 * process listens on, and the identity of the process the pane was opened with. Every locator the
 * tmux runtime writes carries a pane id and that identity; `socketPath` is absent for the
 * controller's interactive pane by design, and `tmuxPaneId`/`socketPath` are otherwise optional
 * only for records predating the fields. */
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
  /** The claim token the pod's shim registers its stream under (`roleToken(project, issue,
   * role)`; the architect token for a tree root) — what `KubernetesRuntime.connect`/`probe`/
   * `stop` key the listener's registrations by. */
  roleToken: string;
  /** The one field `--resume` reads (LEGION-31 renames it `ompSessionRef`). */
  ompSessionFile?: string;
}

export type TmuxLocator = { runtime: "tmux" } & TmuxWindowLocator;
export type Locator = TmuxLocator | ({ runtime: "kubernetes" } & K8sLocator);

/** What a runtime starts from. The runtime assembles the process (OMP path, `--resume`,
 * `--append-system-prompt`) and provisions the working copy itself; `ProcessManager` never
 * builds a shell string or stats a session file. */
export type SpawnSpec = {
  /** Absent only for kind "controller". */
  issue?: IssueKey;
  /** The tree `issue` belongs to (equals `issue` for a root). Absent only for kind "controller". */
  tree?: IssueKey;
  /** The process generation being launched. Absent only for kind "controller". */
  generation?: number;
  role: LegionRole | "controller";
  /** Every non-runtime-specific variable the process carries; the runtime adds its own (the
   * workspace path, `<NAME>_FILE` pointers) and may re-point daemon-path values at its own
   * locations. `undefined` values are omitted. */
  env: Record<string, string | undefined>;
  launch: {
    /** The packaged role prompt file (`packages/pi-envoy/roles/<role>.md`). */
    promptPath: string;
    /** The addressing fragment (roots and phase workers; the controller has none). */
    addressingPrompt?: string;
    /** The recorded OMP session file to `--resume`; a missing file is a launch failure, never a
     * silent fresh start. */
    resumeSessionFile?: string;
  };
  /** name -> value; the runtime decides delivery (tmux: 0600 `<stateDir>/secrets/<role token>` +
   * `<NAME>_FILE` env; kubernetes: a per-pod Secret projected as files). */
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
  /** Whether `spawn("controller", …)` is something this runtime does. tmux launches the
   * controller as its own window; the Kubernetes runtime does not launch it (LEGION-25), and
   * `ProcessManager.ensureController` must learn that before it mints a controller capability
   * for a spawn that would only be refused. */
  readonly launchesController: boolean;
  /** Whether this runtime owns individual issue workspaces on the daemon host and can remove
   * them when a tree closes. Kubernetes retains one tree PVC through its runtime-owned lifecycle. */
  readonly removesWorkspacesOnTreeClose: boolean;
  spawn(kind: "root" | "worker" | "controller", spec: SpawnSpec): Promise<Locator>;
  /** Makes `issue`'s undescribed working-copy commit use the assigned role's identity before an
   * assignment prompt can reach that role. The runtime owns this workspace command because its
   * workspace lives on the daemon host for tmux and on the tree volume for Kubernetes. */
  adoptWorkingCopy(
    issue: IssueKey,
    role: LegionRole,
    identity: JjIdentity,
    timeoutMs: number
  ): Promise<void>;
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

/** Runs `fn` after every operation previously queued under `key` has settled, in FIFO order; a
 * predecessor's rejection does not skip the next (`then(fn, fn)`), and `fn`'s own result or
 * rejection is what the caller gets. Both runtimes serialize with it: tmux its per-issue window
 * opening and per-repository provisioning, Kubernetes its per-(issue, role) pod/PVC mutation
 * sequence. The queue entry is removed once its last operation has settled and nothing newer is
 * queued behind it, so the map holds only lanes with work in flight, never one entry per key
 * the daemon has ever seen. */
export function serialize<T>(
  queue: Map<string, Promise<unknown>>,
  key: string,
  fn: () => Promise<T>
): Promise<T> {
  const previous = queue.get(key) ?? Promise.resolve();
  const gated = previous.then(fn, fn);
  const settled: Promise<void> = gated.then(
    () => undefined,
    () => undefined
  );
  queue.set(key, settled);
  void settled.then(() => {
    if (queue.get(key) === settled) queue.delete(key);
  });
  return gated;
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
 * the pod name and the tree PVC name (a volume some live locator names is known to the sweep). */
export function locatorHandles(locator: Locator): readonly string[] {
  if (locator.runtime === "tmux") {
    return [locator.tmuxWindowId, locator.tmuxPaneId ?? `${locator.tmuxWindowId}/*`];
  }
  return [locator.podName, locator.pvcName];
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

/** The same-agent invariant every resume shares: a recorded OMP session file that has gone
 * missing is a launch failure, never a silent fresh start that would lose the original agent's
 * context. `subject` names the process in the refusal (an issue key, or the controller's role
 * token). The tmux runtime applies it inside `spawn` for every process; `ProcessManager` applies
 * it to the controller before minting a capability, so a refusal there mutates nothing. */
export async function assertResumeSessionFile(
  subject: string,
  resumeSessionFile: string,
  logVerb: string
): Promise<void> {
  try {
    await stat(resumeSessionFile);
  } catch (error) {
    if (
      typeof error !== "object" ||
      error === null ||
      !("code" in error) ||
      error.code !== "ENOENT"
    ) {
      throw error;
    }
    throw new Error(
      `Refusing to start ${subject} fresh while ${logVerb}: recorded OMP session file is missing: ${resumeSessionFile}`
    );
  }
}
