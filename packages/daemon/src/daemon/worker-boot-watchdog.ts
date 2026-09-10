import type { IssueKey, LegionRole } from "@legion/contracts";
import type { WorkerLocator } from "./legion-state";
import type { TmuxRun } from "./tmux";
import * as tmux from "./tmux";
import { probeWorkerSocket, type WorkerRpcClient } from "./worker-rpc";

/** The boot watchdog's own poll interval, both for its connect-retry loop and for
 * `cancelableSleep`'s waits. Distinct from (but numerically mirrors) `processes.ts`'s
 * `WORKER_RETIREMENT_POLL_INTERVAL_MS`, which bounds a different wait (a retiring pane's own
 * exit grace period). */
const BOOT_WATCHDOG_POLL_INTERVAL_MS = 100;

/** A worker claim's fields the watchdog needs to decide whether a still-armed watch is stale
 * (superseded by a later launch, or since confirmed) — never the whole `WorkerRoleClaim`. */
export interface WatchedClaim {
  generation?: number;
  sessionId?: string;
}

/** `cancelableSleep`'s own result: the wait itself, and an explicit cleanup a caller racing it
 * against something else must invoke for the loser — see `cancelableSleep`'s own doc comment. */
interface CancelableSleepHandle {
  promise: Promise<void>;
  cancel: () => void;
}

export interface WorkerBootWatchdogDeps {
  workerBootTimeoutSeconds(): number;
  now(): number;
  run: TmuxRun;
  isOmpPane(pid: number): Promise<boolean>;
  workerClient(token: string, socketPath: string): Promise<WorkerRpcClient>;
  /** Overridable for tests; defaults to a real timer. */
  sleep?(ms: number): Promise<void>;
  /** Overridable for tests; defaults to a real macrotask boundary (`setTimeout(fn, 0)`). */
  yield?(): Promise<void>;
  /** Re-reads the current claim for `token` — undefined if it no longer exists, is not a
   * worker claim, or has moved on since this watch was armed. */
  getClaim(token: string): WatchedClaim | undefined;
  /** Handles a boot the watchdog has confirmed dead (pane gone *and* socket refusing a
   * connection): retires it, counts a launch failure, and retries or escalates at the
   * threshold. Owned by `ProcessManager` — see its own doc comment. */
  retireUnconfirmedBoot(
    token: string,
    locator: WorkerLocator,
    generation: number | undefined,
    retry: { treeKey: IssueKey; issue: IssueKey; role: LegionRole }
  ): Promise<void>;
}

/**
 * Watches a freshly-launched worker's boot. `workerBootTimeoutSeconds` is an observation
 * interval, never a hard SLA: a real OMP startup routinely takes longer than that under host
 * load, and evicting a pane that is merely slow — rather than dead — would carry forward a
 * launch failure (and eventually a spurious `worker-died`) for a worker that never actually
 * died. Each interval patiently retries connecting to the shim socket (in case it has not
 * opened it yet) and, once connected, races the client's `closed` promise against the rest of
 * the interval — the fast path for a socket that closes well before the interval elapses.
 * Whichever way the interval ends, if `/worker/started` still has not confirmed this exact
 * generation, the watchdog probes before acting (`probeAlive`): a live pane or a reachable
 * socket re-arms the watch for another interval, touching neither the claim nor
 * `launchFailures`; only a pane that is both gone and refusing a connection is actually dead,
 * handled by `retireUnconfirmedBoot` (retire, count a launch failure, retry or give up at the
 * threshold). Every wait races the watchdog's own cancellation, armed under `token` so a
 * confirmed `/worker/started`, a retirement, a tree close, or `cancelAll()` can cancel it
 * outright instead of leaving it to poll or sleep unobserved. A no-op once cancelled, once
 * disposed, or once the claim has moved on (superseded by a later launch attempt). The whole
 * watch is wrapped in an outer catch so an unexpected throw anywhere in it can never abandon
 * its own registry entry (and the claim it is watching) stuck "booting" forever with nothing
 * left to observe it.
 */
export class WorkerBootWatchdog {
  private readonly armed = new Map<string, { generation: number; cancel: () => void }>();
  /** Set once `cancelAll()` runs; makes `arm` a no-op afterward, since an in-flight handler
   * racing shutdown (a launch's own success path, or `reconnectWorkers` mid-drain) must never
   * arm a new timer after the daemon has already decided to stop watching. */
  private disposed = false;

  constructor(private readonly deps: WorkerBootWatchdogDeps) {}

  /** Cancels the armed watch for `token`, if any — a no-op if none is armed, or if
   * `generation` is given and does not match the armed watch's (a stale caller from an
   * earlier attempt must never cancel a newer one's watch). */
  cancel(token: string, generation?: number): void {
    const entry = this.armed.get(token);
    if (!entry || (generation !== undefined && entry.generation !== generation)) return;
    entry.cancel();
    this.armed.delete(token);
  }

  /** Cancels every armed watch and makes every future `arm` call a no-op. Daemon shutdown
   * calls this once before drain and again after — an in-flight handler during drain (a
   * launch's own success path, `reconnectWorkers`) can still arm a watch after the first call,
   * and this is the only guaranteed-safe way to catch that: no background timer may outlive
   * the watchdog. Idempotent. */
  cancelAll(): void {
    this.disposed = true;
    for (const entry of this.armed.values()) entry.cancel();
    this.armed.clear();
  }

  private async yieldToEventLoop(): Promise<void> {
    if (this.deps.yield) return this.deps.yield();
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
  }

  /**
   * As a plain sleep, but resolves the moment `signal` aborts, clearing the underlying real
   * timer outright rather than merely losing a race while it lingers. Symmetrically, detaches
   * its own abort listener the moment its own timer (or injected `deps.sleep`) fires normally:
   * `signal` is the watch's one long-lived abort source, and the connect-retry loop below calls
   * this once per failed attempt for as long as a worker is merely slow to open its socket —
   * without this detach, every one of those calls would leave a listener attached to `signal`
   * for the rest of the watch's life, accumulating without bound over a long-lived daemon
   * watching a persistently borderline-slow worker. Returns an explicit `cancel()` too: a
   * caller racing this against something else (the interval-remainder wait against a `closed`
   * promise, see `watchOneInterval`) must clean up the loser's own timer itself — `signal`
   * alone only observes the watch's overall cancellation, never "the other side of this one
   * race already won". `cancel()` and a natural settlement are both idempotent and safe to
   * invoke in either order or more than once.
   */
  private cancelableSleep(ms: number, signal: AbortSignal): CancelableSleepHandle {
    const { promise, resolve } = Promise.withResolvers<void>();
    if (signal.aborted) {
      resolve();
      return { promise, cancel: () => {} };
    }
    let timer: NodeJS.Timeout | undefined;
    const onAbort = (): void => {
      clearTimeout(timer);
      resolve();
    };
    signal.addEventListener("abort", onAbort, { once: true });
    const cancel = (): void => {
      clearTimeout(timer);
      signal.removeEventListener("abort", onAbort);
      resolve();
    };
    if (this.deps.sleep) {
      this.deps.sleep(ms).then(() => {
        signal.removeEventListener("abort", onAbort);
        resolve();
      });
    } else {
      timer = setTimeout(() => {
        signal.removeEventListener("abort", onAbort);
        resolve();
      }, ms);
    }
    return { promise, cancel };
  }

  /**
   * True if this watch should treat `locator` as still alive: either its tmux pane still holds
   * a running OMP process, or its shim socket accepts a connection and negotiates the RPC
   * protocol — matching the reconnect contract everywhere else in `processes.ts` (connect
   * failure means dead; a connected socket whose follow-up `get_state` fails only means the
   * shim is busy, never a reason to treat it as dead). `get_state` here is advisory only, run
   * for its `runState`-seeding side effect; a rejection is caught and logged, never folded into
   * the liveness verdict itself. Used only to decide whether an unconfirmed boot that has
   * missed an observation interval is merely slow (never evicted for that alone) or genuinely
   * dead (no pane, no socket).
   */
  private async probeAlive(token: string, locator: WorkerLocator): Promise<boolean> {
    const target = locator.tmuxPaneId ?? locator.tmuxWindowId;
    const pid = await tmux.panePid(this.deps.run, target);
    if (pid !== undefined && (await this.deps.isOmpPane(pid))) return true;
    const probe = await probeWorkerSocket(
      (socketPath) => this.deps.workerClient(token, socketPath),
      locator.socketPath
    );
    if (!probe.client) return false;
    if (!probe.stateAnswered) {
      console.error(
        `[legion] worker ${token} reconnected but get_state failed (treating as busy, not dead):`,
        probe.stateError
      );
    }
    return true;
  }

  arm(
    treeKey: IssueKey,
    issue: IssueKey,
    role: LegionRole,
    token: string,
    locator: WorkerLocator,
    generation: number
  ): void {
    if (this.disposed) return;
    this.armed.get(token)?.cancel();
    const controller = new AbortController();
    let cancelled = false;
    const cancel = (): void => {
      cancelled = true;
      controller.abort();
    };
    this.armed.set(token, { generation, cancel });

    const intervalMs = this.deps.workerBootTimeoutSeconds() * 1000;
    // A hard attempt cap alongside the time-based deadline: `now()` is caller-supplied (a real
    // clock in production, a fixture's clock in tests) and the connect-retry loop below must
    // never spin unboundedly just because a caller's clock does not advance in lockstep with
    // `sleep` — whether that is a test fixture that fakes `sleep` without also faking `now`, or
    // a real clock that stalls.
    const maxAttemptsPerInterval = Math.max(
      1,
      Math.ceil(intervalMs / BOOT_WATCHDOG_POLL_INTERVAL_MS)
    );

    const watchOneInterval = async (): Promise<void> => {
      const deadline = this.deps.now() + intervalMs;
      let client: WorkerRpcClient | undefined;
      let attempts = 0;
      while (
        !cancelled &&
        client === undefined &&
        this.deps.now() < deadline &&
        attempts < maxAttemptsPerInterval
      ) {
        attempts += 1;
        try {
          client = await this.deps.workerClient(token, locator.socketPath);
        } catch {
          await this.cancelableSleep(BOOT_WATCHDOG_POLL_INTERVAL_MS, controller.signal).promise;
        }
      }
      if (!cancelled && client) {
        const remaining = Math.max(0, deadline - this.deps.now());
        // A rejection from `closed` (never expected in practice — see `WorkerRpcClient`) must
        // never escape this race uncaught: treat it exactly like an ordinary close rather than
        // let it abort the whole watch mid-flight.
        const closed = client.closed.catch(() => undefined);
        const sleep = this.cancelableSleep(remaining, controller.signal);
        await Promise.race([closed, sleep.promise]);
        // Explicit cleanup regardless of which side won: a `closed`-winning race must never
        // leave the interval's own timer live in the background until it separately fires.
        sleep.cancel();
      }
    };

    const watch = async (): Promise<void> => {
      while (!cancelled) {
        await watchOneInterval();
        if (cancelled) return;
        const claim = this.deps.getClaim(token);
        if (!claim || claim.generation !== generation || claim.sessionId) {
          if (this.armed.get(token)?.cancel === cancel) this.armed.delete(token);
          return;
        }
        if (await this.probeAlive(token, locator)) {
          console.error(
            `[legion] worker ${issue}/${role} has not confirmed its boot within ${this.deps.workerBootTimeoutSeconds()}s but its pane/socket is still alive; re-arming the watch instead of evicting a slow boot`
          );
          // A real macrotask boundary, never merely another microtask: a mocked `sleep`/`now`
          // that never advances real time (test fixtures routinely do this for speed) would
          // otherwise let this loop re-arm indefinitely without ever yielding to the event
          // loop's timer phase, starving everything else — including the test runner's own
          // per-test timeout — of a turn. Production's own real timers make this a no-op in
          // practice (the interval wait itself already yields for real). Routed through
          // `deps.yield` (a real timer by default) rather than a raw `setTimeout` inline, so a
          // test using an injected fake clock still owns every wait this watchdog makes.
          await this.yieldToEventLoop();
          continue;
        }
        if (this.armed.get(token)?.cancel === cancel) this.armed.delete(token);
        console.error(
          `[legion] worker ${issue}/${role} never confirmed its boot; retiring and retrying`
        );
        await this.deps.retireUnconfirmedBoot(token, locator, generation, { treeKey, issue, role });
        return;
      }
    };

    void watch().catch((error) => {
      console.error(`[legion] boot watchdog crashed for ${token}:`, error);
      if (this.armed.get(token)?.cancel === cancel) this.armed.delete(token);
    });
  }
}
