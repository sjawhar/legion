// Direct unit tests for WorkerBootWatchdog's real-timer cleanup, isolated from ProcessManager:
// every path a watch can exit through (a worker's socket closing, the observation interval
// timing out, an explicit cancel, and a confirmed-dead retirement) must clear every timer it
// armed along the way, never leaving one live in the background — see `cancelableSleep`'s own
// doc comment for why an uncleared one would otherwise accumulate without bound across a
// long-lived daemon watching a persistently borderline-slow worker.
import { describe, expect, it } from "bun:test";
import type { IssueKey, LegionRole } from "@legion/contracts";
import type { WorkerLocator } from "../legion-state";
import {
  type WatchedClaim,
  WorkerBootWatchdog,
  type WorkerBootWatchdogDeps,
} from "../worker-boot-watchdog";
import type { WorkerRpcClient } from "../worker-rpc";

const root = "sjawhar/legion#1" as IssueKey;
const child = "sjawhar/legion#2" as IssueKey;
const role: LegionRole = "implementer";
const token = "legion-omp-sjawhar__legion-2-implementer";
const locator: WorkerLocator = {
  tmuxSession: "legion-omp",
  tmuxWindowId: "@42",
  tmuxPaneId: "%7",
  socketPath: "/state/workers/child-implementer.sock",
};

/** Tracks every real `setTimeout` this process schedules for the duration of one test,
 * removing an id the moment it fires or is cleared — so `activeCount()` reflects genuinely
 * pending timers, never ones that already settled on their own. */
function trackRealTimers(): { activeCount: () => number; restore: () => void } {
  const active = new Set<ReturnType<typeof setTimeout>>();
  const realSetTimeout = globalThis.setTimeout;
  const realClearTimeout = globalThis.clearTimeout;
  // biome-ignore lint/suspicious/noExplicitAny: matching setTimeout's own permissive overload set
  (globalThis as any).setTimeout = (
    fn: (...args: unknown[]) => void,
    ms?: number,
    ...args: unknown[]
  ) => {
    const id = realSetTimeout(() => {
      active.delete(id);
      fn(...args);
    }, ms);
    active.add(id);
    return id;
  };
  // biome-ignore lint/suspicious/noExplicitAny: matching clearTimeout's own permissive overload set
  (globalThis as any).clearTimeout = (id: any) => {
    active.delete(id);
    return realClearTimeout(id);
  };
  return {
    activeCount: () => active.size,
    restore: () => {
      globalThis.setTimeout = realSetTimeout;
      globalThis.clearTimeout = realClearTimeout;
    },
  };
}

function fakeClient(): WorkerRpcClient & { resolveClosed: () => void } {
  const { promise, resolve } = Promise.withResolvers<void>();
  return {
    closed: promise,
    runState: "unknown",
    negotiate: async () => {},
    prompt: async () => {},
    getState: async () => ({}),
    shutdown: () => {},
    close: () => resolve(),
    onIdle: () => {},
    resolveClosed: resolve,
  };
}

function baseDeps(overrides: Partial<WorkerBootWatchdogDeps> = {}): WorkerBootWatchdogDeps {
  return {
    // A short real interval (well above BOOT_WATCHDOG_POLL_INTERVAL_MS's 100ms) so the
    // connect-retry loop and the interval-remainder wait both genuinely exercise the real
    // setTimeout branch (no `sleep` override) without a slow test.
    workerBootTimeoutSeconds: () => 0.3,
    // High enough that no existing test (which exercises re-arming, not the deadline itself)
    // ever reaches it; tests of the deadline itself override this explicitly.
    registrationDeadlineIntervals: () => 1_000,
    workerRpcTimeoutMs: () => 5_000,
    now: () => Date.now(),
    tmux: { socket: "legion-omp", run: async () => ({ stdout: "", exitCode: 1 }) },
    isOmpPane: async () => false,
    workerClient: async () => {
      throw new Error("no client configured for this test");
    },
    getClaim: (): WatchedClaim | undefined => ({ generation: 1 }),
    retireUnconfirmedBoot: async () => {},
    ...overrides,
  };
}

describe("WorkerBootWatchdog real-timer cleanup", () => {
  it("clears the interval's own timer when a closed-winning race beats it, instead of leaving it live", async () => {
    const timers = trackRealTimers();
    try {
      const client = fakeClient();
      const retired: string[] = [];
      const watchdog = new WorkerBootWatchdog(
        baseDeps({
          workerClient: (() => {
            let calls = 0;
            return async () => {
              calls += 1;
              if (calls === 1) return client;
              throw new Error("shim gone after close");
            };
          })(),
          // Confirmed dead on the very next probe after the socket closes, so the watch
          // retires in one step rather than re-arming (isolating this test to the
          // closed-wins-the-race cleanup, not a second interval's own timers).
          isOmpPane: async () => false,
          retireUnconfirmedBoot: async () => {
            retired.push(token);
          },
        })
      );

      watchdog.arm(root, child, role, token, locator, 1);
      // Lets the connect-retry loop's first attempt succeed and the race against `closed`
      // begin (its own real setTimeout for the ~300ms interval remainder now pending).
      await Promise.resolve();
      await Promise.resolve();
      client.resolveClosed();
      // Drains the microtask queue so the race settles and `sleep.cancel()` runs.
      for (let i = 0; i < 20; i += 1) await Promise.resolve();

      expect(retired).toEqual([token]);
      expect(timers.activeCount()).toBe(0);
    } finally {
      timers.restore();
    }
  });

  it("clears its timer once the observation interval itself times out and retires the boot", async () => {
    const timers = trackRealTimers();
    try {
      const retired: string[] = [];
      const watchdog = new WorkerBootWatchdog(
        baseDeps({
          // Never connects: the interval's own deadline is what ends `watchOneInterval`.
          workerClient: async () => {
            throw new Error("shim not listening");
          },
          isOmpPane: async () => false,
          retireUnconfirmedBoot: async () => {
            retired.push(token);
          },
        })
      );

      watchdog.arm(root, child, role, token, locator, 1);
      await new Promise((resolve) => setTimeout(resolve, 500));

      expect(retired).toEqual([token]);
      expect(timers.activeCount()).toBe(0);
    } finally {
      timers.restore();
    }
  }, 2_000);

  it("clears every outstanding timer the instant cancel() runs, mid-wait", async () => {
    const timers = trackRealTimers();
    try {
      const watchdog = new WorkerBootWatchdog(
        baseDeps({
          workerClient: async () => {
            throw new Error("shim not listening");
          },
        })
      );

      watchdog.arm(root, child, role, token, locator, 1);
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(timers.activeCount()).toBeGreaterThan(0);

      watchdog.cancel(token, 1);
      // A cancelled watch's own in-flight sleep resolves via the abort listener on its next
      // microtask, not synchronously.
      for (let i = 0; i < 20; i += 1) await Promise.resolve();

      expect(timers.activeCount()).toBe(0);
    } finally {
      timers.restore();
    }
  });

  it("never accumulates a listener per re-arm: many slow-but-alive cycles followed by cancel still clear every timer", async () => {
    const timers = trackRealTimers();
    try {
      const watchdog = new WorkerBootWatchdog(
        baseDeps({
          workerClient: async () => {
            throw new Error("shim not listening");
          },
          // Reports the pane alive on every probe: the watch re-arms indefinitely instead of
          // ever retiring, so the only way this test settles is via the explicit cancel below
          // — proving several full re-arm cycles worth of connect-retry timers were each
          // cleaned up along the way, not merely the last one.
          isOmpPane: async () => true,
          tmux: {
            socket: "legion-omp",
            run: async (cmd) => {
              if (cmd.includes("list-panes")) return { stdout: "%7 12345\n", exitCode: 0 };
              return { stdout: "", exitCode: 0 };
            },
          },
        })
      );

      watchdog.arm(root, child, role, token, locator, 1);
      // Several full ~300ms observation intervals' worth of real wall time.
      await new Promise((resolve) => setTimeout(resolve, 900));

      watchdog.cancel(token, 1);
      for (let i = 0; i < 20; i += 1) await Promise.resolve();

      expect(timers.activeCount()).toBe(0);
    } finally {
      timers.restore();
    }
  }, 2_000);
});

describe("WorkerBootWatchdog registration deadline", () => {
  it("retires a boot whose pane/socket stays alive but never registers, once it exceeds registrationDeadlineIntervals", async () => {
    const retirements: Array<{ token: string; generation: number | undefined }> = [];
    const watchdog = new WorkerBootWatchdog(
      baseDeps({
        workerBootTimeoutSeconds: () => 0.01,
        registrationDeadlineIntervals: () => 3,
        sleep: async () => {},
        yield: async () => {},
        // Alive on every single probe -- the pane/socket never actually goes away, and
        // `/worker/started` never confirms either. Without the deadline this would re-arm
        // forever; with it, the watch must give up after exactly 3 consecutive alive intervals.
        isOmpPane: async () => true,
        tmux: {
          socket: "legion-omp",
          run: async (cmd) => {
            if (cmd.includes("list-panes")) return { stdout: "%7 12345\n", exitCode: 0 };
            return { stdout: "", exitCode: 0 };
          },
        },
        workerClient: async () => {
          throw new Error("shim not listening");
        },
        retireUnconfirmedBoot: async (retireToken, _locator, generation) => {
          retirements.push({ token: retireToken, generation });
        },
      })
    );

    watchdog.arm(root, child, role, token, locator, 1);
    for (let i = 0; i < 200 && retirements.length === 0; i += 1) await Promise.resolve();

    expect(retirements).toEqual([{ token, generation: 1 }]);
  });

  it("never treats a boot as failed while it stays under the registration deadline", async () => {
    const retirements: unknown[] = [];
    let probeCount = 0;
    const watchdog = new WorkerBootWatchdog(
      baseDeps({
        workerBootTimeoutSeconds: () => 0.01,
        registrationDeadlineIntervals: () => 3,
        sleep: async () => {},
        yield: async () => {},
        isOmpPane: async () => {
          probeCount += 1;
          return true;
        },
        tmux: {
          socket: "legion-omp",
          run: async (cmd) => {
            if (cmd.includes("list-panes")) return { stdout: "%7 12345\n", exitCode: 0 };
            return { stdout: "", exitCode: 0 };
          },
        },
        workerClient: async () => {
          throw new Error("shim not listening");
        },
        retireUnconfirmedBoot: async () => {
          retirements.push(true);
        },
      })
    );

    watchdog.arm(root, child, role, token, locator, 1);
    // Waits for exactly 2 completed probes -- one short of the 3-interval deadline -- then
    // asserts immediately, before a 3rd probe (and the retirement it would trigger) can run.
    for (let i = 0; i < 500 && probeCount < 2; i += 1) await Promise.resolve();

    expect(probeCount).toBe(2);
    expect(retirements).toEqual([]);
    watchdog.cancel(token, 1);
  });
});

describe("WorkerBootWatchdog pid probe", () => {
  it("probes the worker's own pane pid, not its window's first pane, so a sibling's live OMP never confirms a dead boot", async () => {
    const events: string[] = [];
    const watchdog = new WorkerBootWatchdog(
      baseDeps({
        // 0.01s → exactly one connect attempt per interval (see `maxAttemptsPerInterval`).
        workerBootTimeoutSeconds: () => 0.01,
        sleep: async () => {},
        yield: async () => {},
        tmux: {
          socket: "legion-omp",
          run: async (cmd) => {
            if (cmd.includes("list-panes")) {
              // The worker's whole window: the architect's pane first, then two split-in workers.
              return { stdout: "%1531 2363427\n%1533 3003090\n%1534 446716\n", exitCode: 0 };
            }
            return { stdout: "", exitCode: 0 };
          },
        },
        // Only the architect's pane still runs OMP; the watched worker's own process is gone.
        isOmpPane: async (pid) => {
          events.push(`isOmpPane:${pid}`);
          return pid === 2363427;
        },
        workerClient: async () => {
          events.push("workerClient");
          throw new Error("shim not listening");
        },
        retireUnconfirmedBoot: async () => {
          events.push("retire");
        },
      })
    );

    watchdog.arm(
      root,
      child,
      role,
      token,
      { ...locator, tmuxWindowId: "@1464", tmuxPaneId: "%1533" },
      1
    );
    for (let i = 0; i < 200 && !events.includes("retire"); i += 1) await Promise.resolve();

    // The interval's single connect attempt, then `probeAlive`: the pid probe asks about %1533's
    // own pid (not OMP), falls through to the socket probe (refused), and the boot is retired.
    // Before the fix the first row's 2363427 confirmed the boot and no socket probe ran.
    expect(events).toEqual(["workerClient", "isOmpPane:3003090", "workerClient", "retire"]);
  });
});
