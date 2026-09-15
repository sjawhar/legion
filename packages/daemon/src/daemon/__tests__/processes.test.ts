import { afterAll, afterEach, describe, expect, it, vi } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { mkdir, mkdtemp, readdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  controllerToken,
  type IssueKey,
  type LegionRole,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import { adoptWorkingCopyCommand } from "@legion/workspace";
import { spawnCapabilityKey } from "../api/auth";
import type { DaemonConfig } from "../config";
import { resolveDaemonEnvironment } from "../environment";
import type { ExceptionInfo } from "../events";
import { appRoleForLegionRole } from "../github-apps";
import {
  type LegionState,
  loadState as legionStateLoadState,
  saveState as legionStateSaveState,
  newLegionState,
  type WorkerRoleClaim,
} from "../legion-state";
import { parseProcStatStartTicks } from "../proc-stat";
import { PromptNotStarted } from "../process-errors";
import {
  addressingFragment,
  type ChildAdoption,
  type ControlDirective,
  designGateFragment,
  locatorsForIssue,
  ProcessManager,
  type ProcessManagerDeps,
  StopFailed,
  TreeClosingError,
} from "../processes";
import type { Effect } from "../reducers";
import { MAX_RESENDS, RESEND_PAUSES_MS } from "../resend-ledger";
import { runResync } from "../resync";
import { type Locator, sameProcess, type TmuxLocator } from "../runtime";
import { TmuxRuntime, type TmuxRuntimeDeps } from "../runtime-tmux";
import { installWorkerGhShim, pathWithoutWorkerBin } from "../worker-bin";
import { checkPr, fakeDispatchClient, procStatLine } from "./ci-fixtures";
import { FakeRuntime, type FakeWorkerRpcClient, fakeWorkerRpcClient } from "./fake-runtime";

const root = "LEGION-42";
const child = "LEGION-43";
const grandchild = "LEGION-44";
const TEST_DELIVERY_ID = "00000000-0000-4000-8000-000000000001";
/** The App identity `manager()`'s default token lease answers for every role, and the six
 * variables a pane or a daemon jj command carries for it. */
const HARNESS_GIT_IDENTITY = {
  name: "legion-implement[bot]",
  email: "42+legion-implement[bot]@users.noreply.github.com",
};
const HARNESS_IDENTITY_ENV = {
  JJ_USER: HARNESS_GIT_IDENTITY.name,
  JJ_EMAIL: HARNESS_GIT_IDENTITY.email,
  GIT_AUTHOR_NAME: HARNESS_GIT_IDENTITY.name,
  GIT_AUTHOR_EMAIL: HARNESS_GIT_IDENTITY.email,
  GIT_COMMITTER_NAME: HARNESS_GIT_IDENTITY.name,
  GIT_COMMITTER_EMAIL: HARNESS_GIT_IDENTITY.email,
};
const tempDirs: string[] = [];
const liveManagers: ProcessManager[] = [];

// Every `armBootWatchdog` fired by a `spawnWorker`/`launchWorker` call in these tests races the
// real `deps.sleep` (never overridden here) up to the full `workerBootTimeoutSeconds`. Without
// this, a test that spawns a worker and never separately retires/closes it leaves a live
// multi-minute `setTimeout` pending, which keeps the test process alive long after its
// assertions finish. `dispose()` cancels every such watchdog outright.
afterEach(() => {
  for (const instance of liveManagers.splice(0)) instance.dispose();
});

/**
 * Yields once to the event loop's macrotask queue (never a wall-clock-bound wait — `setImmediate`
 * fires on the next tick, whatever that costs). A boot watchdog's `sleep` override must yield the
 * same way: a pure microtask loop (`Promise.resolve()`) never lets the real `mkdir`/`writeFile`
 * calls `provisionWorkspace` makes during a retry actually complete, since a chain that keeps
 * re-queuing its own microtasks monopolizes the microtask queue and starves every macrotask —
 * including the very I/O the retry is waiting on.
 */
function onceEventLoop(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** For negative waits only — a fixed drain over a decline that does no file write and runs no
 * injected fake, so a tick count IS the whole event (the fired deadline or clock reaches its
 * identity/role/disposed check by microtask hops and returns). Every positive wait awaits its
 * event through the fixture's observers (`saves`, `runs`, `sleeps`, `published`, an
 * `eventCounter`) or a gate the test's own fake resolves (`Promise.withResolvers`, settled from
 * inside the fake at the event); a drain must never be the thing a test waits on for work that
 * includes a file write or an injected `run`, because its tick budget races that I/O under
 * load. Each caller carries a `// Negative wait:` line naming the decline it drains over. */
async function flushEventLoop(ticks = 2_000): Promise<void> {
  for (let tick = 0; tick < ticks; tick += 1) {
    await onceEventLoop();
  }
}

async function flushEventLoopUntil(predicate: () => boolean, ticks = 2_000): Promise<void> {
  for (let tick = 0; tick < ticks; tick += 1) {
    if (predicate()) return;
    await onceEventLoop();
  }
  throw new Error("condition did not become true");
}

/** Counts occurrences of one event and lets a test await the Nth as a real event. `spawnRoot`
 * does real fs I/O (workspace provisioning, secret files) before it ever reaches tmux, so a
 * `setImmediate` budget racing it is load-sensitive; a waiter resolved from inside the fake dep
 * the moment the event happens is not. `reached(n)` resolves on the macrotask AFTER the count
 * reaches n (one `onceEventLoop()`), so the awaiting test resumes after every microtask
 * continuation that event unblocked in the code under test (the synchronous re-check that
 * follows a probe, the locator write that follows a spawn) — but not after any further real
 * I/O; await that I/O's own event instead. A wait that never resolves fails the test on bun's
 * own timeout with the assertion still unreached, never a false green. */
interface EventCounter {
  readonly count: number;
  increment(): void;
  reached(n: number): Promise<void>;
  /** `reached(count + 1)` captured now — for "the next one after this trigger" when the prior
   * count is incidental to the test's story. Call it BEFORE the trigger. */
  next(): Promise<void>;
}

function eventCounter(): EventCounter {
  let count = 0;
  const waiters: Array<{ n: number; resolve: () => void }> = [];
  const counter: EventCounter = {
    get count() {
      return count;
    },
    increment() {
      count += 1;
      for (const waiter of waiters.splice(0)) {
        if (count >= waiter.n) waiter.resolve();
        else waiters.push(waiter);
      }
    },
    async reached(n) {
      if (count < n) {
        const { promise, resolve } = Promise.withResolvers<void>();
        waiters.push({ n, resolve });
        await promise;
      }
      await onceEventLoop();
    },
    next() {
      return counter.reached(count + 1);
    },
  };
  return counter;
}

/** One injected dep observed at both ends. `issued` counts calls as they are made (before the
 * test's fake runs — the event to await when the fake blocks on a gate the test releases later);
 * `completed` counts them as they resolve (the event to await when the assertions need the
 * call's result to have reached the code under test). */
interface CallObserver {
  readonly issued: EventCounter;
  readonly completed: EventCounter;
}

function callObserver(): CallObserver {
  return { issued: eventCounter(), completed: eventCounter() };
}

/** A lazily-created observer per key, so a test can request `reached` before any event. */
function keyed<K, T>(create: () => T): (key: K) => T {
  const entries = new Map<K, T>();
  return (key) => {
    const existing = entries.get(key);
    if (existing) return existing;
    const created = create();
    entries.set(key, created);
    return created;
  };
}

/** The root/controller registration deadline `config` arms, in ms -- so a test names the
 * deadline sleep it awaits instead of a literal. */
function registrationDeadlineMs(config: DaemonConfig): number {
  return config.workerBootTimeoutSeconds * 1000 * config.workerBootRegistrationDeadlineIntervals;
}

/** Polls `condition` every 5 ms of real time until it holds. The timer is a poll interval, never
 * a wait budget: the wait ends the moment the condition holds and is bounded only by bun's
 * per-test timeout, so a condition that never holds fails the test loud with its assertion
 * unreached. Fake time cannot drive this one: it is reserved for an effect with no injectable
 * seam -- today the secret-file write `TmuxRuntime.preparePane` makes through `secrets.ts`
 * directly, with no injected dep between it and a tmux call the test can hold -- so there is no
 * fake to resolve from. Every other positive wait in this file awaits its event through the
 * fixture's observers or a gate its own fake resolves (see `flushEventLoop`). */
async function waitFor(condition: () => boolean): Promise<void> {
  while (!condition()) await new Promise<void>((resolve) => setTimeout(resolve, 5));
}

async function temporaryDir(): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), "legion-processes-"));
  tempDirs.push(directory);
  return directory;
}

/** A `deps.sleep` whose waits never resolve on their own: each call is recorded with its `ms`, and
 * `fire(ms)` resolves the oldest pending wait of exactly that length when the test decides the clock
 * has advanced — so "armed but not yet expired" is observable, and expiry is a deliberate step rather
 * than a race against real time. `boundedWait`'s `cancel` is a no-op under an injected sleep, so a
 * wait the code under test has superseded (a re-armed clock) or cancelled (`dispose()`) stays in
 * `pending` and can still be fired — which is how a test delivers a stale clock's expiry on purpose. */
interface ManualClock {
  pending: Array<{ ms: number; resolve: () => void }>;
  sleep(ms: number): Promise<void>;
  /** Resolves the oldest pending wait of exactly `ms`; false if none is pending. */
  fire(ms: number): boolean;
}

function manualSleep(): ManualClock {
  const pending: Array<{ ms: number; resolve: () => void }> = [];
  return {
    pending,
    sleep: (ms: number) =>
      new Promise<void>((resolve) => {
        pending.push({ ms, resolve });
      }),
    /** Resolves the oldest pending wait of exactly `ms`; false if none is pending. */
    fire(ms: number): boolean {
      const index = pending.findIndex((entry) => entry.ms === ms);
      if (index === -1) return false;
      const [entry] = pending.splice(index, 1);
      entry?.resolve();
      return true;
    },
  };
}

/** A ready-confirmed idle phase worker cached in the manager exactly as a daemon restart's
 * `reconnectWorkers` leaves it: the claim's locator names an `ompSessionFile` that exists on disk, the fake
 * client's `getState` seeded it idle (firing `onIdle`, which arms the idle-retire clock), and every LATER
 * `connectWorkerRpc` call — the dead-worker path's one reconnect probe after a retirement — is refused like a
 * socket whose shim has exited. `shutdownCalls` records every graceful `shutdown` frame sent to the client. */
async function idleWorkerFixture(options: {
  role: LegionRole;
  issue?: IssueKey;
  workerIdleRetireSeconds?: number;
  claim?: Partial<WorkerRoleClaim>;
  phases?: LegionState["phases"];
}) {
  const stateDir = await temporaryDir();
  await mkdir(path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42"), {
    recursive: true,
  });
  const sessionFile = path.join(stateDir, `${options.role}-session.jsonl`);
  await writeFile(sessionFile, "{}", "utf8");
  const state = newLegionState("omp", 1);
  tree(state);
  state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
  const issue = options.issue ?? root;
  if (issue !== root) {
    state.issues[root].children = [issue];
    state.issues[issue] = {
      key: issue,
      title: "Child",
      parent: root,
      children: [],
      status: "in_progress",
    };
  }
  const token = roleToken("omp", issue, options.role);
  state.roles[token] = {
    issue,
    role: options.role,
    sessionId: `ses_${options.role}`,
    generation: 1,
    readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
    locator: {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%7",
      socketPath: `/state/workers/${options.role}.sock`,
      ompSessionFile: sessionFile,
      // The identity the fixture's default `list-panes`/`readProcessStat` report, so the pane
      // verifies: a "no kill-pane" assertion below then means the graceful stop confirmed, not
      // that an identity-less locator had its kill refused.
      ...paneIdentity(),
    },
    ...options.claim,
  };
  if (options.phases) Object.assign(state.phases, options.phases);
  const client = fakeWorkerRpcClient();
  // Models the real client's getState(): a false isStreaming is an idle transition (see the
  // "does not count an idle worker (isStreaming: false)" test).
  client.getStateImpl = async () => {
    client.emitRunState("idle");
    return { data: { isStreaming: false } };
  };
  const shutdownCalls: string[] = [];
  const shutdown = client.shutdown.bind(client);
  client.shutdown = () => {
    shutdownCalls.push(token);
    shutdown();
  };
  let connectAttempts = 0;
  const clock = manualSleep();
  const harness = manager(state, {
    config: config(stateDir, { workerIdleRetireSeconds: options.workerIdleRetireSeconds ?? 600 }),
    sleep: clock.sleep,
    connectWorkerRpc: async () => {
      connectAttempts += 1;
      if (connectAttempts > 1) throw new Error("dead shim socket");
      return client;
    },
  });
  await harness.manager.reconnectWorkers();
  const claim = () => {
    const current = harness.state.roles[token];
    if (!current || !("issue" in current)) throw new Error(`worker claim ${token} disappeared`);
    return current;
  };
  return {
    ...harness,
    stateDir,
    sessionFile,
    issue,
    token,
    client,
    claim,
    shutdownCalls,
    clock,
    connectAttempts: () => connectAttempts,
  };
}

function config(stateDir: string, overrides: Partial<DaemonConfig> = {}): DaemonConfig {
  return {
    project: "omp",
    legionId: "sjawhar/1",
    port: 13999,
    runtime: { name: "tmux" },
    daemonUrl: "http://127.0.0.1:13999",
    bind: "127.0.0.1",
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
    ompLaunchPrefix: [],
    dispatchProject: "LEGSMOKE",
    repo: "sjawhar/legion",
    repos: ["sjawhar/legion"],
    admissionCap: 1,
    workerCap: 5,
    maxRecursionDepth: 8,
    lingerHours: 2,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    workerStopTimeoutSeconds: 10,
    treeStopTimeoutSeconds: 60,
    workerBootTimeoutSeconds: 120,
    workerBootRegistrationDeadlineIntervals: 3,
    workerRpcTimeoutSeconds: 5,
    // Disabled by default here: several tests inject an instantly-resolving deps.sleep, under which a live
    // clock would fire on the first idle transition and retire fixtures those tests expect to stay
    // resident. The idle-retire tests opt in explicitly.
    workerIdleRetireSeconds: 0,
    slowCommandTimeoutSeconds: 300,
    workerStreamPort: 13371,
    gates: { design: "root-issues" },
    githubApps: {},
    stateDir,
    ...overrides,
  };
}

/** The tmux locator `tree()` recorded for `issue`, narrowed for tests that extend or read it. */
function recordedTmuxLocator(state: LegionState, issue: IssueKey = root): TmuxLocator {
  const locator = state.trees[issue]?.locator;
  if (locator?.runtime !== "tmux") throw new Error(`${issue} has no recorded tmux locator`);
  return locator;
}

/** A locator's tmux fields, for expectations that read them; a locator of another runtime
 * (never spawned by these tests) reads as undefined and fails the expectation loudly. */
function tmuxFields(locator: Locator | undefined): TmuxLocator | undefined {
  return locator?.runtime === "tmux" ? locator : undefined;
}

/** The OMP stand-in the live-tmux PATH row runs under the real worker-shim (see
 * `real-prompt-delivery-e2e.test.ts`): ignores its argv, lives until stdin EOF. */
const DELAYED_START_OMP = path.join(
  import.meta.dir,
  "..",
  "..",
  "cli",
  "__tests__",
  "fixtures",
  "delayed-start-omp-rpc.ts"
);

async function childProcesses(pid: number): Promise<number[]> {
  const found: number[] = [];
  const parentPattern = new RegExp(`^PPid:\\s+${pid}$`, "m");
  for (const entry of await readdir("/proc")) {
    if (!/^\d+$/.test(entry)) continue;
    try {
      const status = await readFile(`/proc/${entry}/status`, "utf8");
      if (parentPattern.test(status)) found.push(Number(entry));
    } catch {}
  }
  return found.sort((a, b) => a - b);
}

/** The exec-time environment of the OMP stand-in under a live pane: the first-child chain from
 * the pane pid down to the process whose cmdline names `DELAYED_START_OMP`. Never the pane pid's
 * own `/proc/<pid>/environ`: that is the pane shell's exec-time block — or, since bash 5.1 execs
 * the last command of a `-c` list, the worker-shim's — and what matters is what OMP inherited.
 * Polls the real process tree with a real delay: the awaited condition is a kernel fork/exec under
 * a real tmux server, which no fake clock can advance (bun starts in tens of ms). */
async function ompEnvironment(panePid: number): Promise<Record<string, string>> {
  const deadline = Date.now() + 10_000;
  for (;;) {
    let pid = panePid;
    for (;;) {
      const cmdline = await readFile(`/proc/${pid}/cmdline`, "utf8").catch(() => "");
      if (cmdline.includes(DELAYED_START_OMP)) {
        const raw = await readFile(`/proc/${pid}/environ`, "utf8");
        return Object.fromEntries(
          raw
            .split("\0")
            .filter(Boolean)
            .map((entry) => [
              entry.slice(0, entry.indexOf("=")),
              entry.slice(entry.indexOf("=") + 1),
            ])
        );
      }
      const next = (await childProcesses(pid))[0];
      if (next === undefined) break;
      pid = next;
    }
    if (Date.now() > deadline) {
      throw new Error(`pane ${panePid} spawned no OMP stand-in within 10 s`);
    }
    await Bun.sleep(50);
  }
}

/** The root architect's addressing fragment exactly as `spawnTree` builds it — addressing
 * sentence, then the gate policy for `design` (the fixture config's is `root-issues`). */
function rootArchitectFragment(design: "root-issues" | "off" = "root-issues"): string {
  return `${addressingFragment("omp", root, root, "architect")} ${designGateFragment(design)}`;
}

/** The one `--append-system-prompt` argument a launch command carries, exactly as
 * `systemPromptArguments` builds it: one double-quoted shell word holding `$(cat <role prompt>)`,
 * then the inline addressing text (escaped for double quotes), then `$(cat <instructions>)` when
 * configured, separated by blank lines. OMP's flag is last-wins, so a second flag would discard
 * everything before it. */
function promptArgument(
  promptPath: string,
  addressing: string | undefined,
  deploymentInstructionsFile?: string
): string {
  const fragments = [`$(cat ${promptPath})`];
  if (addressing !== undefined) fragments.push(addressing.replaceAll(/[\\"$`]/g, (c) => `\\${c}`));
  if (deploymentInstructionsFile !== undefined)
    fragments.push(`$(cat ${deploymentInstructionsFile})`);
  return `--append-system-prompt "${fragments.join("\n\n")}"`;
}

function tree(state: LegionState, issue: IssueKey = root, generation = 1) {
  state.trees[issue] = {
    root: issue,
    generation,
    locator: {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
      ompSessionFile: "/state/trees/sjawhar-legion-42/.omp/session.json",
      ...paneIdentity(),
    },
    status: "active",
    launchFailures: 0,
  };
}

function exception(
  role: string,
  original: ExceptionInfo["original"] = {
    topic: "notifications.github.sjawhar.legion.issue.42.comment",
    payload: '{"body":"retry"}',
    eventId: "evt-1",
  },
  reason: ExceptionInfo["reason"] = "no_holder"
): ExceptionInfo {
  return { roleToken: role, reason, original };
}

function liveRun(command: string[]): Promise<{ stdout: string; exitCode: number }> {
  if (command[0] === "tmux" && command[3] === "list-windows") {
    return Promise.resolve({ stdout: "sjawhar-legion-42\n", exitCode: 0 });
  }
  if (command[0] === "tmux" && command[3] === "list-panes") {
    return Promise.resolve(livePanes(command));
  }
  return Promise.resolve({ stdout: "", exitCode: 0 });
}

/** Answers a `list-panes -t <pane> -F "#{pane_id} #{pane_pid}"` liveness probe for a pane that is
 * alive: one `<pane_id> <pid>` row for the probed pane. */
function livePanes(command: string[], pid = 12345): { stdout: string; exitCode: number } {
  const target = command[command.indexOf("-t") + 1];
  return { stdout: `${target} ${pid}\n`, exitCode: 0 };
}

/** Answers a per-pane `list-panes -t <pane>` probe for a pane that is gone the way real tmux
 * does: exit 1 with `can't find pane` on stderr (`PANE_GONE_STDERR`). A bare exit 1 with no
 * stderr is NOT a gone pane -- it is a failed listing that proves nothing, and `TmuxRuntime`
 * refuses to read it as either alive or gone. */
function paneGone(): { stdout: string; stderr: string; exitCode: number } {
  return { stdout: "", stderr: "can't find pane", exitCode: 1 };
}

/** Start ticks the fixture's default `readProcessStat` reports for every pid, so a locator
 * seeded with `paneIdentity()` verifies against the default fake tmux (pid 12345). */
const DEFAULT_START_TICKS = 4242;

/** A `/proc/<pid>/stat` line whose field 22 (starttime) is `startTicks`. */
function procStat(pid: number, startTicks = DEFAULT_START_TICKS): string {
  return procStatLine(pid, startTicks);
}

/** The process identity a seeded locator needs to verify: `pid` must be what the test's fake
 * `list-panes` reports for that pane and `startTicks` what its `readProcessStat` reports. */
function paneIdentity(pid = 12345, startTicks = DEFAULT_START_TICKS) {
  return { panePid: pid, paneStartTicks: startTicks };
}

/** Runtime-side overrides `manager()` threads into its `TmuxRuntime` rather than `ProcessManager`. */
type RuntimeOverrides = {
  connectWorkerRpc: TmuxRuntimeDeps["connectWorkerRpc"];
  readProcessCmdline: (pid: number) => Promise<string>;
  readProcessStat: (pid: number) => Promise<string>;
  issueLocators: TmuxRuntimeDeps["issueLocators"];
  run: TmuxRuntimeDeps["run"];
  statPrompt: NonNullable<TmuxRuntimeDeps["statPrompt"]>;
  provisioningToken: TmuxRuntimeDeps["provisioningToken"];
  deploymentInstructionsFile: string;
  ompInvocation: string;
};

function manager(
  state = newLegionState("omp", 1),
  options: Partial<ProcessManagerDeps & RuntimeOverrides> = {},
  {
    skipEnableLaunches = false,
    omitRun = false,
  }: { skipEnableLaunches?: boolean; omitRun?: boolean } = {}
): {
  manager: ProcessManager;
  state: LegionState;
  commands: string[][];
  controlRequests: Array<{ subject: string; json: string }>;
  publications: Array<{ subject: string; json: string; dedupeKey?: string }>;
  revokedSessions: string[];
  /** Every `deps.saveState` call (the test's override or the default no-op), both ends. */
  saves: CallObserver;
  /** Every `deps.run` call keyed by verb -- a tmux subcommand (`list-panes`, `kill-pane`,
   * `new-window`, `split-window`, `has-session`) or the executable (`jj`) -- both ends. */
  runs(verb: string): CallObserver;
  /** Every `deps.sleep` call keyed by its duration, counted the moment the wait is armed. Only
   * counted when the test injected a `sleep`: under real timers nothing is observed, and a
   * `reached` on it fails loud on the test timeout. */
  sleeps(ms: number): EventCounter;
  /** Every `deps.publishRole` call keyed by the payload's `type` (`"?"` when absent), counted
   * after the injected fn (the test's override or the default `publications.push`) ran. */
  published(type: string): EventCounter;
} {
  const commands: string[][] = [];
  const publications: Array<{ subject: string; json: string; dedupeKey?: string }> = [];
  const controlRequests: Array<{ subject: string; json: string }> = [];
  const revokedSessions: string[] = [];
  const {
    run: requestedRun,
    connectWorkerRpc,
    readProcessCmdline,
    readProcessStat,
    issueLocators,
    statPrompt,
    provisioningToken,
    deploymentInstructionsFile,
    ompInvocation,
    ...overrides
  } = options;
  let launchedAnyWindow = false;
  const commandRunner =
    requestedRun ??
    (async (command: string[]) => {
      commands.push(command);
      // A liveness probe of a previously-recorded window happens before this test's first
      // successful new-window/split-window; once one has succeeded, tmux's own `-P -F` output
      // already reports the pane id and pid synchronously, so no further discovery call happens.
      if (
        command[0] === "tmux" &&
        command[3] === "list-panes" &&
        command.includes("#{pane_id} #{pane_pid}")
      ) {
        if (!launchedAnyWindow) return paneGone();
        return livePanes(command);
      }
      if (command[0] === "tmux" && command[3] === "split-window") {
        launchedAnyWindow = true;
        return { stdout: "%2 12345\n", exitCode: 0 };
      }
      if (command[0] === "tmux" && command[3] === "new-window") {
        launchedAnyWindow = true;
        return { stdout: "@42 %1 12345\n", exitCode: 0 };
      }
      return { stdout: "", exitCode: 0 };
    });
  const saves = callObserver();
  const runs = keyed<string, CallObserver>(callObserver);
  const sleeps = keyed<number, EventCounter>(eventCounter);
  const published = keyed<string, EventCounter>(eventCounter);
  // The runtime's command runner (`TmuxRuntimeDeps.run`; `ProcessManagerDeps` has none): observed
  // from outside the test's own `commandRunner` fake, which keeps working unchanged. Two-ended
  // (`issued` before the fake, `completed` after it).
  const run: TmuxRuntimeDeps["run"] = async (command, runnerOptions) => {
    const observer = runs((command[0] === "tmux" ? command[3] : command[0]) ?? "?");
    observer.issued.increment();
    const result = await commandRunner(command, runnerOptions);
    if (result.exitCode === 0) {
      if (command[0] === "jj" && command[1] === "git" && command[2] === "clone") {
        const cloneDir = command[4];
        if (!cloneDir) throw new Error("Jujutsu clone is missing its destination");
        await mkdir(path.join(cloneDir, ".jj"), { recursive: true });
      }
      if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
        const workspaceDir = command[3];
        if (!workspaceDir) throw new Error("Jujutsu workspace is missing its destination");
        await mkdir(workspaceDir, { recursive: true });
      }
    }
    // The load-bearing order: `completed` fires AFTER the real `mkdir` the jj fakes perform, so
    // an awaiter never resumes ahead of that I/O; it fires before the stdout defaulting, which
    // is pure. The code under test sees the result on its next microtask, which `reached()`'s
    // one-macrotask deferral covers.
    observer.completed.increment();
    if (result.exitCode !== 0) return result;
    if (
      command[0] === "tmux" &&
      (command[3] === "new-window" || command[3] === "new-session") &&
      result.stdout.trim() === ""
    ) {
      return { ...result, stdout: "@42\n" };
    }
    return result;
  };
  const injected: Omit<ProcessManagerDeps, "runtime"> = {
    state,
    saveState: async () => {},
    config: config("/state"),
    // `dedupeKey` is spread only when defined so every `toEqual([{subject, json}])` stays exact.
    publishRole: (subject, json, dedupeKey) =>
      publications.push({ subject, json, ...(dedupeKey !== undefined ? { dedupeKey } : {}) }),
    natsRequest: async (subject, json) => {
      controlRequests.push({ subject, json });
      return JSON.stringify({ type: "ack" });
    },
    mintControllerCapability: async () => "controller-secret",
    mintBootToken: async () => "boot-token",
    mintWorkerBootToken: async () => "worker-boot-token",
    processPath: "/full/bin:/usr/bin",
    rolePromptsDir: path.resolve(import.meta.dir, "../../../../pi-envoy/roles"),
    credentialHelper: "!/opt/legion/bun /opt/legion/cli/index.ts credential",
    workerCatchup: {
      repo: "sjawhar/legion",
      baseEnv: {},
      runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
      tokenManager: {
        getToken: async () => ({
          token: "worker-token",
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: HARNESS_GIT_IDENTITY,
        }),
      },
    },
    now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    dispatchClient: fakeDispatchClient(),
    revokeSessionCapability: (sessionId) => revokedSessions.push(sessionId),
    // `omitRun` builds the manager the way the Kubernetes entry point does: no daemon-host runner.
    ...(omitRun ? {} : { run }),
    ...overrides,
  };
  // Observed from outside the test's own fakes (`saveState`/`publishRole`/`sleep` above), which
  // keep working unchanged. `saveState` is two-ended (`issued` before the injected fn,
  // `completed` after it); `sleep` counts when armed and `publishRole` after the fn.
  const injectedSleep = injected.sleep;
  const deps: Omit<ProcessManagerDeps, "runtime"> = {
    ...injected,
    saveState: async () => {
      saves.issued.increment();
      await injected.saveState();
      saves.completed.increment();
    },
    publishRole: (topic, json, dedupeKey) => {
      injected.publishRole(topic, json, dedupeKey);
      const payload: unknown = JSON.parse(json);
      const type =
        typeof payload === "object" && payload !== null && "type" in payload
          ? payload.type
          : undefined;
      published(typeof type === "string" ? type : "?").increment();
    },
    ...(injectedSleep && {
      sleep: (ms: number) => {
        sleeps(ms).increment();
        return injectedSleep(ms);
      },
    }),
  };
  // Mirrors index.ts: the private server and the secret-file project come from the fixture's
  // `state.project`, never a constant — the live-tmux tests below give each run its own project
  // and drive `tmux -L legion-<project>` themselves, so a hardcoded socket would spawn onto a
  // server those tests never look at (and leak it). A test that injects its own `runtime`
  // (the FakeRuntime lifecycle case) gets exactly that runtime and no tmux at all.
  const runtime =
    options.runtime ??
    new TmuxRuntime({
      tmux: { run, socket: `legion-${state.project}` },
      project: state.project,
      stateDir: deps.config.stateDir,
      ompInvocation: ompInvocation ?? "/opt/oh-my-pi/18.0.3/omp",
      ompLaunchPrefix: deps.config.ompLaunchPrefix,
      deploymentInstructionsFile,
      statPrompt: statPrompt ?? (async () => {}),
      provisioningToken: provisioningToken ?? (async () => "daemon-installation-token"),
      run,
      repo: deps.config.repo,
      credentialHelper: deps.credentialHelper,
      slowCommandTimeoutMs: deps.config.slowCommandTimeoutSeconds * 1000,
      connectWorkerRpc: connectWorkerRpc ?? (async () => fakeWorkerRpcClient()),
      workerRpcTimeoutMs: () => deps.config.workerRpcTimeoutSeconds * 1000,
      now: deps.now,
      sleep: deps.sleep,
      readProcessCmdline: readProcessCmdline ?? (async () => "omp\0"),
      // An explicit `readProcessStat: undefined` (the live-tmux test) selects the runtime's real
      // `/proc/<pid>/stat` read; an absent key gets the fixture's fake.
      readProcessStat:
        "readProcessStat" in options ? readProcessStat : async (pid) => procStat(pid),
      issueLocators: issueLocators ?? ((issue) => locatorsForIssue(state, issue)),
    });
  const processManager = new ProcessManager({ ...deps, runtime });
  liveManagers.push(processManager);
  // Every existing test exercises worker-queue promotion as already "booted" (index.ts calls
  // this once `reconnectWorkers` has settled, after `api` is assigned) — only the dedicated
  // boot-ordering test passes `skipEnableLaunches` to exercise the gate itself.
  if (!skipEnableLaunches) processManager.enableLaunches();
  return {
    manager: processManager,
    state,
    commands,
    controlRequests,
    publications,
    revokedSessions,
    saves,
    runs,
    sleeps,
    published,
  };
}

/** Common setup for running-worker-cap tests: a fresh single-root-tree `LegionState` wired to
 * `manager()` at the given `workerCap`, with a scratch `stateDir` already provisioned — cuts the
 * `temporaryDir`/`newLegionState`/`tree`/`config` boilerplate those tests otherwise repeat. Not
 * every `workerCap`-configured test in this file uses it: several earlier
 * dead-worker/`--resume`/launch-failure-threshold tests need bespoke workspace-directory,
 * `ompSessionFile`, or mock-command setup this fixture's minimal footprint does not cover, and
 * are left as their own hand-rolled `manager()` calls rather than forced onto it. Callers seed
 * `state.roles`/`state.workerAdmission.queue` on the returned `state` before touching
 * `processes`; `overrides` merges into (and can replace) any of `manager()`'s own deps. */
async function workerCapFixture(
  workerCap: number,
  overrides: Partial<ProcessManagerDeps & RuntimeOverrides> = {},
  { skipEnableLaunches = false }: { skipEnableLaunches?: boolean } = {}
) {
  const stateDir = await temporaryDir();
  const state = newLegionState("omp", 1);
  tree(state);
  const {
    manager: processes,
    state: managedState,
    commands,
    publications,
    controlRequests,
    saves,
    runs,
    sleeps,
    published,
  } = manager(
    state,
    { config: config(stateDir, { workerCap }), ...overrides },
    { skipEnableLaunches }
  );
  return {
    processes,
    state,
    managedState,
    commands,
    publications,
    controlRequests,
    stateDir,
    saves,
    runs,
    sleeps,
    published,
  };
}

function tmuxWindowEnvironment(command: readonly string[]): Record<string, string> {
  const environment: Record<string, string> = {};
  for (let index = 0; index < command.length; index += 1) {
    if (command[index] !== "-e") continue;
    const assignment = command[index + 1];
    if (!assignment) throw new Error("tmux -e is missing an environment assignment");
    const separator = assignment.indexOf("=");
    if (separator === -1) throw new Error(`tmux environment assignment is invalid: ${assignment}`);
    environment[assignment.slice(0, separator)] = assignment.slice(separator + 1);
  }
  return environment;
}

/** The pane's PATH as `TmuxRuntime.preparePane` delivers it: the `export PATH=<value> && ` prefix of
 * the pane shell command (tmux drops a `-e PATH=` pair — LEGION-91), unquoted or single-quoted as
 * `shellPath` renders it. `undefined` when the command carries no export. */
function tmuxPanePath(command: readonly string[]): string | undefined {
  const match = /^export PATH=(?:'((?:[^']|'\\'')*)'|(\S+)) && /.exec(command.at(-1) ?? "");
  if (!match) return undefined;
  return match[1] !== undefined ? match[1].replaceAll("'\\''", "'") : match[2];
}

afterAll(async () => {
  await Promise.all(tempDirs.map((directory) => rm(directory, { recursive: true, force: true })));
});

describe("ProcessManager", () => {
  it("admits only up to the configured global cap and selects the next queued tree on release", async () => {
    const stateDir = await temporaryDir();
    let windows = 0;
    let completeSpawns: (() => void) | undefined;
    const spawned = new Promise<void>((resolve) => {
      completeSpawns = resolve;
    });
    const { manager: processes, state } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "new-window") {
          if (++windows === 2) completeSpawns?.();
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    expect(processes.admit(root)).toBe("spawned");
    expect(processes.admit(child)).toBe("queued");
    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [child] });

    processes.releaseSlot(root);
    await spawned;

    expect(state.admission).toEqual({ cap: 1, active: [child], queue: [] });
  });

  it("drainSpawns awaits an admit-triggered spawn that admit itself never awaits, including that spawn's own saveState", async () => {
    const stateDir = await temporaryDir();
    const saveGate = Promise.withResolvers<void>();
    const { manager: processes } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@42 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
      saveState: async () => {
        await saveGate.promise;
      },
    });

    // admit()'s "spawned" path fires startRoot without awaiting it — the
    // spawn, and its own post-success saveState, are still running when
    // admit() returns.
    expect(processes.admit(root)).toBe("spawned");

    let drained = false;
    const draining = processes.drainSpawns().then(() => {
      drained = true;
    });

    // saveGate is still pending, so the tracked spawn cannot have settled
    // yet — no real wait needed to know `drained` is still false here.
    expect(drained).toBe(false);

    saveGate.resolve();
    await draining;
    expect(drained).toBe(true);
  });

  it("provisions the root issue workspace before launching OMP in that workspace", async () => {
    const stateDir = await temporaryDir();
    const repo = path.join(stateDir, "repos", "github.com", "sjawhar", "legion");
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(path.join(repo, ".jj"), { recursive: true });
    const workspaceCalls: Array<{
      readonly command: string[];
      readonly opts:
        | {
            readonly cwd?: string;
            readonly env?: Readonly<Record<string, string>>;
            readonly timeoutMs?: number;
          }
        | undefined;
    }> = [];
    const {
      manager: processes,
      state,
      commands,
    } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      run: async (
        command: string[],
        opts?: {
          readonly cwd?: string;
          readonly env?: Readonly<Record<string, string>>;
        }
      ) => {
        commands.push(command);
        workspaceCalls.push({ command, opts });
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
          await mkdir(workspace, { recursive: true });
        }
        if (command[3] === "has-session") return { stdout: "", exitCode: 1 };
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    await processes.spawnRoot(root);

    expect(existsSync(path.join(workspace, ".omp", "config.yml"))).toBeFalse();
    expect(state.trees[root]).toMatchObject({
      generation: 1,
      status: "active",
    });
    expect(commands).toEqual([
      ["jj", "config", "get", "git.abandon-unreachable-commits", "-R", repo],
      ["jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false", "-R", repo],
      ["jj", "git", "fetch", "-R", repo],
      [
        "jj",
        "log",
        "-r",
        "bookmarks(exact:legion/LEGION-42)",
        "--no-graph",
        "-T",
        'commit_id ++ "\n"',
        "--ignore-working-copy",
        "-R",
        repo,
      ],
      ["git", `--git-dir=${repo}/.git`, "worktree", "prune"],
      [
        "jj",
        "workspace",
        "add",
        workspace,
        "--name",
        "legion-42",
        "--revision",
        "main",
        "-R",
        repo,
      ],
      ["jj", "bookmark", "set", "legion/LEGION-42", "-r", "@"],
      ...[
        ["git", `--git-dir=${repo}/.git`, "config", "--replace-all", "credential.helper", ""],
        [
          "git",
          `--git-dir=${repo}/.git`,
          "config",
          "--add",
          "credential.helper",
          "!/opt/legion/bun /opt/legion/cli/index.ts credential",
        ],
        [
          "git",
          `--git-dir=${repo}/.git`,
          "config",
          "--replace-all",
          "credential.https://github.com.helper",
          "",
        ],
        [
          "git",
          `--git-dir=${repo}/.git`,
          "config",
          "--add",
          "credential.https://github.com.helper",
          "!/opt/legion/bun /opt/legion/cli/index.ts credential",
        ],
        ["git", `--git-dir=${repo}/.git`, "config", "credential.interactive", "false"],
      ],
      ["jj", "config", "list", "--repo", "--include-overridden", "-R", repo, "user.name"],
      ["jj", "config", "list", "--repo", "--include-overridden", "-R", repo, "user.email"],
      ["tmux", "-L", "legion-omp", "has-session", "-t", "legion-omp"],
      [
        "tmux",
        "-L",
        "legion-omp",
        "new-session",
        "-d",
        "-s",
        "legion-omp",
        "-n",
        "__legion_bootstrap",
        "sleep 3600",
        ";",
        "set-option",
        "-t",
        "legion-omp",
        "update-environment",
        "",
      ],
      ["tmux", "-L", "legion-omp", "set-option", "-t", "legion-omp", "@legion_owner", "legion-omp"],
      [
        "tmux",
        "-L",
        "legion-omp",
        "new-window",
        "-P",
        "-F",
        "#{window_id} #{pane_id} #{pane_pid}",
        "-t",
        "legion-omp",
        "-n",
        "legion-42",
        "-e",
        "LEGION_TREE=LEGION-42",
        "-e",
        "LEGION_ISSUE=LEGION-42",
        "-e",
        "LEGION_ROLE=architect",
        "-e",
        "LEGION_GENERATION=1",
        "-e",
        "LEGION_DAEMON_URL=http://127.0.0.1:13999",
        "-e",
        "LEGION_PROJECT=omp",
        "-e",
        "ENVOY_NATS_URL=nats://127.0.0.1:4222",
        "-e",
        "ENVOY_URL=http://127.0.0.1:9020",
        "-e",
        "LEGION_CONTROL_SUBJECT=legion.ctl.legion-42.1",
        "-e",
        "LEGION_MAX_RECURSION_DEPTH=8",
        "-e",
        `LEGION_STATE_DIR=${stateDir}`,
        "-e",
        "LEGION_CREDENTIAL_HELPER=!/opt/legion/bun /opt/legion/cli/index.ts credential",
        "-e",
        "GIT_CONFIG_COUNT=0",
        "-e",
        "GIT_TERMINAL_PROMPT=0",
        "-e",
        `GH_CONFIG_DIR=${path.join(stateDir, "gh")}`,
        "-e",
        "GH_TOKEN=",
        "-e",
        "GITHUB_TOKEN=",
        "-e",
        "GH_HOST=",
        "-e",
        `LEGION_GRANT_FILE=${path.join(stateDir, "secrets", `${roleToken("omp", root, "architect")}-grant`)}`,
        "-e",
        "DISPATCH_URL=http://127.0.0.1:18766",
        "-e",
        `DISPATCH_TOKEN_FILE=${path.join(stateDir, "secrets", "dispatch-token")}`,
        "-e",
        `LEGION_ROOT_WORKSPACE=${workspace}`,
        "-e",
        `LEGION_BOOT_TOKEN_FILE=${path.join(stateDir, "secrets", roleToken("omp", root, "architect"))}`,
        `export PATH=${path.join(stateDir, "worker-bin")}${path.delimiter}/full/bin:/usr/bin && cd ${workspace} && ${process.execPath} ${path.resolve(import.meta.dir, "../../cli/index.ts")} worker-shim --socket ${path.join(stateDir, "workers", "architect-9e2fb104.sock")} -- /opt/oh-my-pi/18.0.3/omp --mode rpc ${promptArgument(`${path.resolve(import.meta.dir, "../../../../pi-envoy")}/roles/architect-root.md`, rootArchitectFragment())}`,
      ],
      ["tmux", "-L", "legion-omp", "kill-window", "-t", "legion-omp:__legion_bootstrap"],
      ["tmux", "-L", "legion-omp", "set-option", "-w", "-t", "@42", "@legion_owner", "legion-omp"],
    ]);
    // Every provisioning command carries the configured slow budget (300 s), not the runner's
    // generic default.
    expect(workspaceCalls).toContainEqual({
      command: ["jj", "bookmark", "set", "legion/LEGION-42", "-r", "@"],
      opts: { cwd: workspace, timeoutMs: 300_000 },
    });
    expect(workspaceCalls).toContainEqual({
      command: ["jj", "git", "fetch", "-R", repo],
      opts: {
        env: {
          GIT_ASKPASS: expect.stringMatching(/provisioning-credential-.+\/askpass$/),
          GIT_TERMINAL_PROMPT: "0",
          LEGION_PROVISIONING_TOKEN: "daemon-installation-token",
        },
        timeoutMs: 300_000,
      },
    });
  });
  it("launches controller and root windows with disjoint exact environments", async () => {
    const stateDir = await temporaryDir();
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") {
          return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        }
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);

    const windows = commands.filter((command) => command[3] === "new-window");
    expect(windows).toHaveLength(2);
    const controllerWindow = windows[0];
    const rootWindow = windows[1];
    if (!controllerWindow || !rootWindow) throw new Error("missing Legion tmux windows");

    expect(tmuxWindowEnvironment(controllerWindow)).toEqual({
      LEGION_CONTROLLER: "1",
      LEGION_ROLE: "controller",
      LEGION_CONTROLLER_SECRET_FILE: path.join(stateDir, "secrets", "legion-omp-controller"),
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      GH_CONFIG_DIR: path.join(stateDir, "gh"),
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      GH_HOST: "",
      LEGION_GRANT_FILE: path.join(stateDir, "secrets", "legion-omp-controller-grant"),
    });
    expect(tmuxPanePath(controllerWindow)).toBe(
      `${path.join(stateDir, "worker-bin")}${path.delimiter}/full/bin:/usr/bin`
    );
    expect(tmuxWindowEnvironment(rootWindow)).toEqual({
      LEGION_TREE: root,
      LEGION_ISSUE: root,
      LEGION_ROLE: "architect",
      LEGION_ROOT_WORKSPACE: path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42"),
      LEGION_GENERATION: "1",
      LEGION_BOOT_TOKEN_FILE: path.join(stateDir, "secrets", roleToken("omp", root, "architect")),
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      LEGION_CONTROL_SUBJECT: "legion.ctl.legion-42.1",
      LEGION_MAX_RECURSION_DEPTH: "8",
      LEGION_STATE_DIR: stateDir,
      LEGION_CREDENTIAL_HELPER: "!/opt/legion/bun /opt/legion/cli/index.ts credential",
      GIT_CONFIG_COUNT: "0",
      GIT_TERMINAL_PROMPT: "0",
      GH_CONFIG_DIR: path.join(stateDir, "gh"),
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      GH_HOST: "",
      LEGION_GRANT_FILE: path.join(
        stateDir,
        "secrets",
        `${roleToken("omp", root, "architect")}-grant`
      ),
    });
    expect(tmuxPanePath(rootWindow)).toBe(
      `${path.join(stateDir, "worker-bin")}${path.delimiter}/full/bin:/usr/bin`
    );
  });
  it("puts the static credential environment on every pane, worker-bin first on PATH exactly once", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") {
          return { stdout: `@${commands.length} %${commands.length} 12345\n`, exitCode: 0 };
        }
        if (command[3] === "split-window") {
          return { stdout: `%${commands.length} 12345\n`, exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");

    const launches = commands.filter((c) => c[3] === "new-window" || c[3] === "split-window");
    expect(launches).toHaveLength(3);
    const workerBin = path.join(stateDir, "worker-bin");
    const secretsDir = path.join(stateDir, "secrets");
    const expectedGrantFiles = [
      "legion-omp-controller-grant",
      `${roleToken("omp", root, "architect")}-grant`,
      `${roleToken("omp", root, "tester")}-grant`,
    ];
    launches.forEach((launch, index) => {
      const environment = tmuxWindowEnvironment(launch);
      expect(environment).toMatchObject({
        GH_CONFIG_DIR: path.join(stateDir, "gh"),
        GH_TOKEN: "",
        GITHUB_TOKEN: "",
        GH_HOST: "",
        LEGION_GRANT_FILE: path.join(secretsDir, expectedGrantFiles[index] ?? ""),
      });
      const panePath = tmuxPanePath(launch);
      expect(panePath?.startsWith(`${workerBin}${path.delimiter}`)).toBe(true);
      expect(panePath?.split(path.delimiter).filter((e) => e === workerBin)).toHaveLength(1);
      // The grant itself never rides a -e pair: the extension writes the named file later.
      expect(environment.LEGION_GRANT).toBeUndefined();
      for (const part of launch) expect(part.startsWith("LEGION_GRANT=")).toBe(false);
    });
  });
  it("carries each phase worker's commit identity in its pane environment, from its role's GitHub App; root and controller panes carry none", async () => {
    // Every issue workspace is a `jj workspace` of one shared clone, and jj's `--repo` config is one
    // file for all of them: a worker that wrote its identity there set the author and committer for
    // every other tree's commits (LEGION-44). Identity therefore rides the pane's environment —
    // `JJ_USER`/`JJ_EMAIL` (jj reads these over every config scope) and the four Git variables —
    // resolved from the same App lease `/worker/started` and `legion gh` use, before the pane opens.
    // Which App a role acts as is `appRoleForLegionRole`'s to say; only the two roles every mapping
    // agrees on are pinned to a named App here, the sub-architect is checked against whatever the
    // mapping says.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [child] };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const apps = {
      review: { id: 3202653, slug: "legion-reviewer" },
      implement: { id: 3202636, slug: "legion-implementer" },
    };
    const identityOf = (appRole: keyof typeof apps) => ({
      name: `${apps[appRole].slug}[bot]`,
      email: `${apps[appRole].id}+${apps[appRole].slug}[bot]@users.noreply.github.com`,
    });
    const identityEnvOf = (appRole: keyof typeof apps) => {
      const { name, email } = identityOf(appRole);
      return {
        JJ_USER: name,
        JJ_EMAIL: email,
        GIT_AUTHOR_NAME: name,
        GIT_AUTHOR_EMAIL: email,
        GIT_COMMITTER_NAME: name,
        GIT_COMMITTER_EMAIL: email,
      };
    };
    const leases: Array<{ appRole: string; owner: string }> = [];
    let sessionExists = false;
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      workerCatchup: {
        repo: "sjawhar/legion",
        baseEnv: {},
        runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
        tokenManager: {
          getToken: async (appRole, owner) => {
            leases.push({ appRole, owner });
            return {
              token: `ghs_${appRole}`,
              expiresAt: "2099-01-01T00:00:00.000Z",
              gitIdentity: identityOf(appRole),
            };
          },
        },
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") {
          return { stdout: `@${commands.length} %${commands.length} 12345\n`, exitCode: 0 };
        }
        if (command[3] === "split-window") {
          return { stdout: `%${commands.length} 12345\n`, exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "reviewer", "review #41");
    await processes.spawnWorker(root, root, "implementer", "fix #41");
    // A sub-architect is opened by the same worker spawn path and commits like any phase worker.
    await processes.spawnWorker(root, child, "architect", `own ${child}`);

    const launches = commands.filter((c) => c[3] === "new-window" || c[3] === "split-window");
    expect(launches).toHaveLength(5);
    const [controller, rootWindow, reviewer, implementer, subArchitect] =
      launches.map(tmuxWindowEnvironment);
    // Root architect and controller never commit: none of the six.
    expect(controller?.LEGION_ROLE).toBe("controller");
    expect(rootWindow?.LEGION_ROLE).toBe("architect");
    for (const pane of [controller, rootWindow]) {
      for (const key of Object.keys(identityEnvOf("review"))) expect(pane?.[key]).toBeUndefined();
    }
    expect(reviewer).toMatchObject({ LEGION_ROLE: "reviewer", ...identityEnvOf("review") });
    expect(implementer).toMatchObject({
      LEGION_ROLE: "implementer",
      ...identityEnvOf("implement"),
    });
    expect(subArchitect).toMatchObject({
      LEGION_ROLE: "architect",
      LEGION_ISSUE: child,
      ...identityEnvOf(appRoleForLegionRole("architect")),
    });
    // The App is the role's (`appRoleForLegionRole`), the owner is the configured repo's.
    expect(leases).toEqual([
      { appRole: "review", owner: "sjawhar" },
      { appRole: "implement", owner: "sjawhar" },
      { appRole: appRoleForLegionRole("architect"), owner: "sjawhar" },
    ]);
  });
  it("fails a worker launch when the role's App identity cannot be resolved: no pane opens, one launch failure is counted", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    state.trees[root] = { root, generation: 1, status: "active", launchFailures: 0 };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      workerCatchup: {
        repo: "sjawhar/legion",
        baseEnv: {},
        runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
        tokenManager: {
          getToken: async () => {
            throw new Error("github_app_not_installed: sjawhar");
          },
        },
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await expect(processes.spawnWorker(root, root, "tester", "verify #41")).rejects.toThrow(
      "github_app_not_installed"
    );

    // No pane without an identity: the token manager's own error is the launch's error.
    expect(
      commands.some((c) => c[0] === "tmux" && (c[3] === "new-window" || c[3] === "split-window"))
    ).toBeFalse();
    const claim = managedState.roles[roleToken("omp", root, "tester")];
    if (!claim || !("issue" in claim)) throw new Error("worker claim missing");
    expect(claim.launchFailures).toBe(1);
    expect(claim.locator).toBeUndefined();
  });
  it("a daemon started from inside a Legion pane still launches panes with worker-bin exactly once", async () => {
    // The daemon's `processPath` is `resolveDaemonEnvironment`'s pane PATH. Started from a pane,
    // that function sees the pane's own `<state_dir>/worker-bin`-first PATH through `mise env`;
    // it must strip every such entry so its own `gh` is never the shim and the one prefix
    // `credentialProcessEnvironment` adds is the only one a launched pane carries.
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const inheritedWorkerBin = path.join(stateDir, "worker-bin");
    const environment = await resolveDaemonEnvironment("mise x omp@1 -- omp", {
      stateDir,
      runtime: "tmux",
      env: { PATH: "/narrow/bin" },
      resolveExecutable: (command) =>
        command === "mise"
          ? "/tools/mise"
          : command.startsWith("/")
            ? command
            : `/tools/${command}`,
      run: async (command) => {
        if (command[1] === "env") {
          return {
            stdout: JSON.stringify({
              PATH: `${inheritedWorkerBin}:/full/bin:/other/state/worker-bin:/usr/bin`,
            }),
            stderr: "",
            exitCode: 0,
          };
        }
        return { stdout: "/mise/omp\n", stderr: "", exitCode: 0 };
      },
    });
    expect(environment.commands.gh).toBe("/tools/gh");

    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      processPath: environment.paneEnv.PATH,
      rolePromptsDir: environment.rolePromptsDir,
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") {
          return { stdout: `@${commands.length} %${commands.length} 12345\n`, exitCode: 0 };
        }
        if (command[3] === "split-window") {
          return { stdout: `%${commands.length} 12345\n`, exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");

    const launches = commands.filter((c) => c[3] === "new-window" || c[3] === "split-window");
    expect(launches).toHaveLength(3);
    for (const launch of launches) {
      const entries = tmuxPanePath(launch)?.split(path.delimiter) ?? [];
      expect(entries[0]).toBe(inheritedWorkerBin);
      expect(entries.filter((entry) => path.basename(entry) === "worker-bin")).toHaveLength(1);
      expect(entries).toContain(path.join(stateDir, "bin"));
    }
  });
  it("runs every tmux command against the private legion-<project> socket", async () => {
    const stateDir = await temporaryDir();
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.reconcileOrphans();

    const tmuxCommands = commands.filter((command) => command[0] === "tmux");
    expect(tmuxCommands.length).toBeGreaterThan(0);
    for (const command of tmuxCommands) {
      expect(command.slice(0, 3)).toEqual(["tmux", "-L", "legion-omp"]);
    }
    expect(tmuxCommands.map((command) => command[3])).toContain("list-windows");
  });
  it("delivers every pane secret as a 0600 file pointer inside a 0700 secrets dir, never as a -e value", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      mintControllerCapability: async () => "controller-secret",
      mintBootToken: async () => "root-boot-token",
      mintWorkerBootToken: async () => "worker-boot-token",
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") {
          return { stdout: `@${commands.length} %${commands.length} 12345\n`, exitCode: 0 };
        }
        if (command[3] === "split-window") {
          return { stdout: `%${commands.length} 12345\n`, exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");

    const secrets = [
      "test-dispatch-token",
      "root-boot-token",
      "worker-boot-token",
      "controller-secret",
    ];
    const tmuxCommands = commands.filter((command) => command[0] === "tmux");
    for (const command of tmuxCommands) {
      expect(command.slice(0, 3)).toEqual(["tmux", "-L", "legion-omp"]);
      for (const part of command) for (const secret of secrets) expect(part).not.toContain(secret);
    }
    const launches = tmuxCommands.filter((c) => c[3] === "new-window" || c[3] === "split-window");
    expect(launches).toHaveLength(3);
    const [controller, architect, tester] = launches.map(tmuxWindowEnvironment);
    if (!controller || !architect || !tester) throw new Error("missing launches");
    const dir = path.join(stateDir, "secrets");
    expect(controller).toMatchObject({
      DISPATCH_TOKEN_FILE: path.join(dir, "dispatch-token"),
      LEGION_CONTROLLER_SECRET_FILE: path.join(dir, "legion-omp-controller"),
    });
    expect(architect).toMatchObject({
      DISPATCH_TOKEN_FILE: path.join(dir, "dispatch-token"),
      LEGION_BOOT_TOKEN_FILE: path.join(dir, roleToken("omp", root, "architect")),
    });
    expect(tester).toMatchObject({
      DISPATCH_TOKEN_FILE: path.join(dir, "dispatch-token"),
      LEGION_BOOT_TOKEN_FILE: path.join(dir, roleToken("omp", root, "tester")),
    });
    for (const environment of [controller, architect, tester]) {
      expect(environment.DISPATCH_TOKEN).toBeUndefined();
      expect(environment.LEGION_BOOT_TOKEN).toBeUndefined();
      expect(environment.LEGION_CONTROLLER_SECRET).toBeUndefined();
    }
    expect((await stat(dir)).mode & 0o777).toBe(0o700);
    for (const [file, value] of [
      [controller.LEGION_CONTROLLER_SECRET_FILE, "controller-secret"],
      [architect.LEGION_BOOT_TOKEN_FILE, "root-boot-token"],
      [tester.LEGION_BOOT_TOKEN_FILE, "worker-boot-token"],
    ] as const) {
      if (!file) throw new Error("pointer missing");
      expect((await stat(file)).mode & 0o777).toBe(0o600);
      expect(await readFile(file, "utf8")).toBe(value);
    }
  });
  it("prunes a pane's secret file once no locator references it, keeping the others", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    let sessionExists = false;
    const { manager: processes } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        if (command[3] === "split-window") return { stdout: "%2 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });
    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");
    const dir = path.join(stateDir, "secrets");
    const architectFile = path.join(dir, roleToken("omp", root, "architect"));
    const testerFile = path.join(dir, roleToken("omp", root, "tester"));
    expect((await readdir(dir)).sort()).toEqual(
      [
        roleToken("omp", root, "architect"),
        roleToken("omp", root, "tester"),
        "legion-omp-controller",
      ].sort()
    );
    // The grant files are the extension's to write, later in each pane's life; the daemon only
    // names them (LEGION_GRANT_FILE) and prunes them with the pane's boot-token file.
    const architectGrant = `${architectFile}-grant`;
    const testerGrant = `${testerFile}-grant`;
    const controllerGrant = path.join(dir, "legion-omp-controller-grant");
    for (const file of [architectGrant, testerGrant, controllerGrant]) {
      await writeFile(file, "g", { mode: 0o600 });
    }

    await processes.markProcessDead(root);

    expect(await stat(architectFile).catch(() => undefined)).toBeUndefined();
    expect(await stat(architectGrant).catch(() => undefined)).toBeUndefined();
    expect(await readFile(testerFile, "utf8")).toBe("worker-boot-token");
    expect(await readFile(testerGrant, "utf8")).toBe("g");

    await processes.closeTree(root);

    expect((await readdir(dir)).sort()).toEqual([
      "legion-omp-controller",
      "legion-omp-controller-grant",
    ]);
  });
  it("delivers a configured Envoy token to every process as a second 0600 file with its own ENVOY_TOKEN_FILE pointer, pruned with the pane's boot-token file", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir, { envoyToken: "envoy-listener-token" }),
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") {
          return { stdout: `@${commands.length} %${commands.length} 12345\n`, exitCode: 0 };
        }
        if (command[3] === "split-window") {
          return { stdout: `%${commands.length} 12345\n`, exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");

    const dir = path.join(stateDir, "secrets");
    const architectToken = roleToken("omp", root, "architect");
    const testerToken = roleToken("omp", root, "tester");
    const launches = commands.filter(
      (c) => c[0] === "tmux" && (c[3] === "new-window" || c[3] === "split-window")
    );
    for (const command of launches) {
      for (const part of command) expect(part).not.toContain("envoy-listener-token");
    }
    const [controller, architect, tester] = launches.map(tmuxWindowEnvironment);
    if (!controller || !architect || !tester) throw new Error("missing launches");
    expect(controller).toMatchObject({
      LEGION_CONTROLLER_SECRET_FILE: path.join(dir, "legion-omp-controller"),
      ENVOY_TOKEN_FILE: path.join(dir, "legion-omp-controller-envoy_token"),
    });
    expect(architect).toMatchObject({
      LEGION_BOOT_TOKEN_FILE: path.join(dir, architectToken),
      ENVOY_TOKEN_FILE: path.join(dir, `${architectToken}-envoy_token`),
    });
    expect(tester).toMatchObject({
      LEGION_BOOT_TOKEN_FILE: path.join(dir, testerToken),
      ENVOY_TOKEN_FILE: path.join(dir, `${testerToken}-envoy_token`),
    });
    for (const environment of [controller, architect, tester]) {
      expect(environment.ENVOY_TOKEN).toBeUndefined();
      if (!environment.ENVOY_TOKEN_FILE) throw new Error("pointer missing");
      expect((await stat(environment.ENVOY_TOKEN_FILE)).mode & 0o777).toBe(0o600);
      expect(await readFile(environment.ENVOY_TOKEN_FILE, "utf8")).toBe("envoy-listener-token");
    }

    await processes.markProcessDead(root);
    expect(await stat(path.join(dir, `${architectToken}-envoy_token`)).catch(() => undefined)).toBe(
      undefined
    );
    expect(await readFile(path.join(dir, `${testerToken}-envoy_token`), "utf8")).toBe(
      "envoy-listener-token"
    );
    await processes.closeTree(root);
    expect((await readdir(dir)).sort()).toEqual([
      "legion-omp-controller",
      "legion-omp-controller-envoy_token",
    ]);
  });
  it("puts a configured Envoy token in every SpawnSpec's secrets and never in its env, on any runtime", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const runtime = new FakeRuntime();
    const { manager: processes } = manager(state, {
      config: config(stateDir, { envoyToken: "envoy-listener-token" }),
      runtime,
    });
    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(runtime.spawned.map((spawn) => [spawn.kind, spawn.spec.secrets])).toEqual([
      [
        "controller",
        { LEGION_CONTROLLER_SECRET: "controller-secret", ENVOY_TOKEN: "envoy-listener-token" },
      ],
      ["root", { LEGION_BOOT_TOKEN: "boot-token", ENVOY_TOKEN: "envoy-listener-token" }],
      ["worker", { LEGION_BOOT_TOKEN: "worker-boot-token", ENVOY_TOKEN: "envoy-listener-token" }],
    ]);
    for (const spawn of runtime.spawned) {
      expect(spawn.spec.env).not.toHaveProperty("ENVOY_TOKEN");
      expect(spawn.spec.env).not.toHaveProperty("ENVOY_TOKEN_FILE");
      expect(JSON.stringify(spawn.spec.env)).not.toContain("envoy-listener-token");
    }
  });
  it("reaps a secret file inherited from a previous daemon process once its locator clears, not only at the next boot", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    // Files a previous daemon process wrote: the architect's and the tester's are still
    // referenced by locators; the planner's is a leftover from a locator that cleared right
    // before that process died.
    const dir = path.join(stateDir, "secrets");
    await mkdir(dir, { recursive: true });
    for (const name of [
      roleToken("omp", root, "architect"),
      token,
      roleToken("omp", root, "planner"),
      "dispatch-token",
    ]) {
      await writeFile(path.join(dir, name), `secret-${name}`);
    }
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
    });

    await processes.pruneSecretFiles();
    expect((await readdir(dir)).sort()).toEqual(
      [roleToken("omp", root, "architect"), token, "dispatch-token"].sort()
    );
    // Written after the boot prune, as the extension does during the pane's life: the prune
    // must have seeded the grant name from the live locator, not from the files it found.
    await writeFile(path.join(dir, `${token}-grant`), "g", { mode: 0o600 });

    // The tester's socket is dead: its locator clears, and the files this process never wrote
    // itself must go with it on that same persist.
    await processes.reconnectWorkers();

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
    expect((await readdir(dir)).sort()).toEqual(
      [roleToken("omp", root, "architect"), "dispatch-token"].sort()
    );
  });
  it("keeps a surviving pane's -envoy_token file across a restart that dropped envoy_token_file, and reaps it with the pane's other files once its locator clears", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    // A previous daemon had an Envoy token and handed this pane ENVOY_TOKEN_FILE; the pane still
    // reads that file on every listener call. This daemon boots with no envoy_token_file.
    const dir = path.join(stateDir, "secrets");
    await mkdir(dir, { recursive: true });
    for (const name of [token, `${token}-envoy_token`, `${token}-grant`, "dispatch-token"]) {
      await writeFile(path.join(dir, name), `secret-${name}`, { mode: 0o600 });
    }
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
    });

    await processes.pruneSecretFiles();
    expect((await readdir(dir)).sort()).toEqual(
      [token, `${token}-envoy_token`, `${token}-grant`, "dispatch-token"].sort()
    );

    await processes.reconnectWorkers();

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
    expect(await readdir(dir)).toEqual(["dispatch-token"]);
  });
  it("writes the tree's Dispatch status to in_progress on a successful spawn, then to done on close", async () => {
    const stateDir = await temporaryDir();
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    let sessionExists = false;
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);

    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
    expect(managedState.trees[root]?.status).toBe("active");

    await processes.closeTree(root);

    expect(statusWrites).toEqual([
      { issue: root, status: "in_progress" },
      { issue: root, status: "done" },
    ]);
    expect(managedState.trees[root]?.status).toBe("closed");
  });
  it("skips the close-time Dispatch done write when the issue is already closed", async () => {
    // Reproduces a real Dispatch server's behavior: `reduceIssueClosed` applies a closed issue's
    // `status: "done"` onto `state.issues` before `closeTree` ever runs (via `expireLinger`'s
    // linger, or `reportRootExit`'s own `status === "done"` gate) — so by the time `closeTree`
    // gets here, Dispatch already considers the issue closed and permanently refuses a further
    // status PATCH on it. Writing "done" again must not be attempted.
    const stateDir = await temporaryDir();
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    let sessionExists = false;
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "done", children: [] };
    tree(state);
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.closeTree(root);

    expect(statusWrites).toEqual([]);
    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.pendingStatusWrites[root]).toBeUndefined();
  });
  it("closes a parked root on linger expiry without overwriting Dispatch with done", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Parked root", status: "backlog", children: [] };
    tree(state);
    state.trees[root].status = "lingering";
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });

    await processes.expireLinger(root);

    expect(managedState.trees[root]?.status).toBe("closed");
    expect(statusWrites).toEqual([]);
  });

  it("retires a just-opened root pane without a status write when a human parks it during launch", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const launchStarted = Promise.withResolvers<void>();
    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          launchStarted.resolve();
          await launchGate.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawning = processes.spawnRoot(root);
    await launchStarted.promise;
    state.issues[root].status = "backlog";
    state.trees[root].status = "lingering";
    state.admission.active.splice(state.admission.active.indexOf(root), 1);
    launchGate.resolve();

    await spawning;

    expect(managedState.trees[root]).toMatchObject({ status: "lingering" });
    expect(managedState.trees[root]?.locator).toBeUndefined();
    expect(statusWrites).toEqual([]);
    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
  });

  it("proceeds to a running root when a delayed in_progress echo lands mid-launch", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const launchStarted = Promise.withResolvers<void>();
    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          launchStarted.resolve();
          await launchGate.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawning = processes.spawnRoot(root);
    await launchStarted.promise;
    // This daemon's own earlier `writeStatus(..., "in_progress")` lands through Dispatch's echo
    // while this launch is still in flight -- the launch's own write hasn't run yet.
    state.issues[root].status = "in_progress";
    launchGate.resolve();

    await spawning;

    expect(managedState.trees[root]).toMatchObject({ status: "active" });
    expect(managedState.trees[root]?.locator).toBeDefined();
    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
    expect(commands).not.toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
  });

  it("preserves a /process/ready confirmation that lands while the launch is still opening its pane", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Fast root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const launchStarted = Promise.withResolvers<void>();
    const launchGate = Promise.withResolvers<void>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "new-window") {
          launchStarted.resolve();
          await launchGate.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawning = processes.spawnRoot(root);
    await launchStarted.promise;
    // The real root process outside this event loop calls /process/started then /process/ready
    // for its own generation before this continuation ever resumes from `runtime.spawn` --
    // `readyConfirmedAt` must already be clear by now (spawnTree's own pre-spawn clear) so
    // this confirmation is recorded, not lost to a race with that clear.
    processes.confirmRootReady(root, 1);
    expect(managedState.trees[root]?.readyConfirmedAt).toBeDefined();
    launchGate.resolve();

    await spawning;

    // spawnTree's own happy-path continuation never wipes the confirmation that beat it back,
    // and arming the deadline afterward against an already-confirmed tree is a harmless no-op.
    expect(managedState.trees[root]?.readyConfirmedAt).toBeDefined();
    expect(managedState.trees[root]).toMatchObject({ status: "active" });
    expect(managedState.trees[root]?.locator).toBeDefined();
  });

  it("retires an older generation's launch as stale when a park-then-re-admit starts a newer one first", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const firstLaunchStarted = Promise.withResolvers<void>();
    const releaseFirstLaunch = Promise.withResolvers<void>();
    const secondLaunchSplitting = Promise.withResolvers<void>();
    const releaseSecondLaunch = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    // No injected `sleep`: the manager's root-registration deadline must stay a real (long) timer
    // here, or it would fire the moment generation 2's locator lands and start retiring that
    // never-confirmed root -- killing `%2`, clearing its locator, and pruning the very secret file
    // the assertions below read -- racing them on real fs I/O. `dispose()` (afterEach) cancels it.
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      mintBootToken: async (_issue, generation) => `boot-gen-${generation}`,
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          firstLaunchStarted.resolve();
          await releaseFirstLaunch.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          secondLaunchSplitting.resolve();
          await releaseSecondLaunch.promise;
          return { stdout: "%2 54321\n", exitCode: 0 };
        }
        // Generation 1's pane is still its recorded process when generation 2 probes the window
        // it opened, so generation 2 splits into it rather than opening a second window.
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const firstSpawn = processes.spawnRoot(root);
    await firstLaunchStarted.promise;

    // Human parks the issue while the first launch (generation 1) is still blocked in tmux,
    // releasing the admission slot and lingering the tree -- exactly as the reducer's linger
    // effect would.
    await processes.beginLinger(root);
    // ...then it is re-admitted immediately, starting generation 2's own launch (queued behind
    // generation 1's still-open tmux call, via the tmux runtime's per-issue launch queue)
    // before the stale generation-1 launch ever returns.
    state.issues[root].status = "todo";
    expect(processes.admit(root)).toBe("spawned");

    // Generation 2 writes its boot token to the shared `legion-omp-<root>-architect` secret file
    // before its launch queues behind generation 1's still-open tmux call; wait until that write
    // has landed so the older generation settles *after* the newer one already depends on it.
    // The write is `TmuxRuntime.preparePane`'s own `writeSecretFile` (no injected dep between it
    // and the lane-serialised `split-window` held below), so the file's content is the only
    // observable event -- polled, never tick-bounded (see `waitFor`).
    const architectFile = path.join(stateDir, "secrets", roleToken("omp", root, "architect"));
    await waitFor(
      () => existsSync(architectFile) && readFileSync(architectFile, "utf8") === "boot-gen-2"
    );
    expect(readFileSync(architectFile, "utf8")).toBe("boot-gen-2");

    // Only now does the older, generation-1 launch's tmux call finally resolve; generation 2's
    // queued launch runs immediately after it, splitting a second pane into the same window.
    // Generation 2's split is held open until generation 1's whole spawnRoot — its stale-pane
    // retirement AND the persist (with its secret-file prune) that follows — has settled, so
    // the prune runs while generation 2's pane launch is still in flight with no locator in
    // state: exactly the window in which the in-flight exemption must keep the shared secret
    // file alive for the pane that is about to read it.
    releaseFirstLaunch.resolve();
    await firstSpawn;
    await secondLaunchSplitting.promise;
    releaseSecondLaunch.resolve();
    await processes.drainSpawns();

    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
    expect(managedState.trees[root]).toMatchObject({
      generation: 2,
      status: "active",
      locator: { tmuxWindowId: "@42", tmuxPaneId: "%2", panePid: 54321 },
    });
    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
    // The surviving pane's boot token is still on disk, and it is generation 2's — the older
    // generation's settle neither removed the file nor left its own stale token behind.
    expect(await readFile(architectFile, "utf8")).toBe("boot-gen-2");
    expect(await readdir(path.join(stateDir, "secrets"))).toEqual([
      roleToken("omp", root, "architect"),
    ]);
  });

  it("resurrects a dead root whose Dispatch status is in_progress instead of treating it as a human park", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });

    await processes.resurrect(root);

    expect(managedState.trees[root]).toMatchObject({ status: "active" });
    expect(managedState.trees[root]?.locator).toBeDefined();
    expect(statusWrites).toEqual([]);
  });

  it("resurrects a dead root at retro without writing any Dispatch status: a resume is not an admission", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    // Past `in_progress`: the status a merged issue sits at while its held-for-verification
    // implementer waits. A resurrection here must not move it.
    state.issues[root] = { key: root, title: "Root", status: "retro", children: [] };
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });

    await processes.resurrect(root);

    expect(statusWrites).toEqual([]);
    expect(managedState.trees[root]).toMatchObject({ generation: 2, status: "active" });
    expect(managedState.trees[root]?.locator).toBeDefined();
    const launch = commands.find((c) => c[0] === "tmux" && c[3] === "new-window");
    expect(launch?.at(-1)).toContain(`--resume=${sessionFile}`);
  });

  it("keeps a resumed root's admission slot when the root's own exit self-report lands during the daemon's graceful stop, and promotes nothing (LEGION-83)", async () => {
    // The 09:13 shape: cap 1, one confirmed root whose persisted locator predates pane identity
    // (never verifies -> probes dead/not-recorded-process -> the stop asks the still-live root to
    // exit over its own socket), and one never-started issue queued behind it. The root's real
    // `session_shutdown` hook POSTs `/process/exit` for the CURRENT generation while that stop is
    // still awaiting the socket close, and `handleProcessExit` routes an open issue to
    // `markProcessDead`. Before the fix that report released the slot and promoted the queued
    // issue; the resume then activated the root outside `admission.active`.
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const { panePid: _pid, paneStartTicks: _ticks, ...legacyLocator } = recordedTmuxLocator(state);
    state.trees[root].locator = { ...legacyLocator, ompSessionFile: sessionFile };
    state.trees[root].readyConfirmedAt = Date.parse("2026-08-24T00:00:00.000Z");
    state.admission.active = [root];
    const queued: IssueKey = "LEGION-45";
    state.trees[queued] = { root: queued, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue = [queued];
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    state.issues[queued] = {
      key: queued,
      title: "Queued behind the cap",
      status: "todo",
      children: [],
    };
    let processes!: ProcessManager;
    let selfReportSettled = false;
    const rootClient = fakeWorkerRpcClient();
    rootClient.shutdown = () => {
      // The root's own session_shutdown hook: /process/exit for the current generation, answered
      // before OMP finishes exiting -- only then does the shim's socket close.
      void processes.markProcessDead(root, state.trees[root]?.generation).then(() => {
        selfReportSettled = true;
        rootClient.close();
      });
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      ({ manager: processes } = manager(state, {
        config: config(stateDir),
        connectWorkerRpc: async () => rootClient,
      }));

      await processes.resurrect(root);
      await processes.drainSpawns();

      expect(selfReportSettled).toBe(true);
      expect(state.trees[root]).toMatchObject({ status: "active", generation: 2 });
      expect(state.trees[root]?.locator).toBeDefined();
      expect(state.admission.active).toEqual([root]);
      expect(state.admission.queue).toEqual([queued]);
      expect(state.trees[queued]).toMatchObject({ status: "queued" });
      expect(state.trees[queued]?.locator).toBeUndefined();
      const ignored = errorLog.mock.calls
        .map(String)
        .filter((line) => line.includes("exit self-report"));
      expect(ignored).toHaveLength(1);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("keeps a self-exited root's session file on the tree and resumes it, into a free slot, at the next resurrection (LEGION-83, acceptance 2)", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = { ...recordedTmuxLocator(state), ompSessionFile: sessionFile };
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes, commands } = manager(state, { config: config(stateDir) });

      // The root exits on its own (its `/process/exit` on an open issue): dead, slot released.
      await processes.markProcessDead(root);
      expect(state.trees[root]).toMatchObject({ status: "dead", resumeSessionFile: sessionFile });
      expect(state.trees[root]?.locator).toBeUndefined();
      expect(state.admission.active).toEqual([]);

      // A wake routed to the dead root resurrects it: a slot is free, so it takes one and resumes.
      await processes.resurrect(root);
      await processes.drainSpawns();

      expect(state.trees[root]).toMatchObject({ status: "active", generation: 2 });
      expect(state.trees[root]?.resumeSessionFile).toBeUndefined();
      expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
      const launch = commands.find(
        (command) => command[0] === "tmux" && command[3] === "new-window"
      );
      expect(launch?.join(" ")).toContain(`--resume=${sessionFile}`);
      const taken = errorLog.mock.calls
        .map(String)
        .filter((line) => line.includes("taking a free"));
      expect(taken).toHaveLength(1);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("keeps a failed resurrection's session file for the queued retry (LEGION-83)", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = { ...recordedTmuxLocator(state), ompSessionFile: sessionFile };
    state.admission.active = [root];
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    const launches: string[][] = [];
    let launchAttempts = 0;
    const launched = eventCounter();
    let firstSavedTree:
      | { status: string; resumeSessionFile?: string; locator?: unknown }
      | undefined;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        firstSavedTree ??= structuredClone(state.trees[root]);
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "list-panes") return paneGone();
        if (command[0] !== "tmux" || command[3] !== "new-window") {
          return { stdout: "", exitCode: 0 };
        }
        launches.push(command);
        launchAttempts += 1;
        launched.increment();
        return launchAttempts === 1
          ? { stdout: "", stderr: "tmux new-window failed", exitCode: 1 }
          : { stdout: "@43 %1 12345\n", exitCode: 0 };
      },
    });

    await expect(processes.resurrect(root)).rejects.toThrow("tmux new-window failed");
    expect(firstSavedTree).toMatchObject({ status: "dead", resumeSessionFile: sessionFile });
    expect(firstSavedTree?.locator).toBeUndefined();
    expect(state.trees[root]).toMatchObject({ status: "queued", launchFailures: 1 });
    expect(processes.admit(root)).toBe("queued");
    await launched.reached(2);
    await processes.drainSpawns();

    expect(launches).toHaveLength(2);
    expect(launches[1]?.join(" ")).toContain(`--resume=${sessionFile}`);
  });

  it("persists a mid-resurrection root through boot and resumes it in its reserved slot (LEGION-83)", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    state.trees[root] = {
      root,
      generation: 1,
      status: "dead",
      launchFailures: 0,
      resumeSessionFile: sessionFile,
    };
    state.admission.active = [root];
    const { manager: processes, commands } = manager(
      state,
      { config: config(stateDir) },
      { skipEnableLaunches: true }
    );

    processes.reconnectRoots();
    processes.enableLaunches();
    await processes.reconcileAdmission();
    await processes.replayHeldRecoveries();
    await processes.drainSpawns();

    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
    expect(state.trees[root]).toMatchObject({ status: "active", generation: 2 });
    const launch = commands.find((command) => command[3] === "new-window");
    expect(launch?.join(" ")).toContain(`--resume=${sessionFile}`);
  });

  it("resumes a persisted mid-resurrection root during resync without releasing its slot (LEGION-83)", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    state.trees[root] = {
      root,
      generation: 1,
      status: "dead",
      launchFailures: 0,
      resumeSessionFile: sessionFile,
    };
    state.admission.active = [root];
    const { manager: processes, commands } = manager(state, { config: config(stateDir) });

    await processes.reconcileAdmissionDrift();
    await processes.drainSpawns();

    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
    expect(state.trees[root]).toMatchObject({ status: "active", generation: 2 });
    const launch = commands.find((command) => command[3] === "new-window");
    expect(launch?.join(" ")).toContain(`--resume=${sessionFile}`);
  });

  it("resumes a persisted dead root that still owns its admission slot when admitted again (LEGION-83)", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    state.trees[root] = {
      root,
      generation: 1,
      status: "dead",
      launchFailures: 0,
      resumeSessionFile: sessionFile,
    };
    state.admission.active = [root];
    const { manager: processes, commands } = manager(state, { config: config(stateDir) });

    expect(processes.admit(root)).toBe("spawned");
    await processes.drainSpawns();

    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
    expect(state.trees[root]).toMatchObject({ status: "active", generation: 2 });
    const launch = commands.find((command) => command[3] === "new-window");
    expect(launch?.join(" ")).toContain(`--resume=${sessionFile}`);
  });

  it("queues a resurrected root at the head of admission.queue with its session file when the cap is full, and its promotion resumes it (LEGION-83, acceptance 2)", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    // The root exited on its own earlier (markProcessDead's shape): dead, no locator, session file
    // kept, slot gone -- and the slot has since gone to `occupant`, with `waiting` never started.
    state.trees[root] = {
      root,
      generation: 1,
      status: "dead",
      launchFailures: 0,
      resumeSessionFile: sessionFile,
    };
    const occupant: IssueKey = "LEGION-45";
    const waiting: IssueKey = "LEGION-46";
    tree(state, occupant);
    state.trees[waiting] = { root: waiting, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.active = [occupant];
    state.admission.queue = [waiting];
    for (const key of [root, occupant, waiting]) {
      state.issues[key] = {
        key,
        title: key,
        status: key === waiting ? "todo" : "in_progress",
        children: [],
      };
    }
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes, commands } = manager(state, { config: config(stateDir) });

      await processes.resurrect(root);

      expect(state.trees[root]).toMatchObject({
        status: "queued",
        generation: 1,
        resumeSessionFile: sessionFile,
      });
      expect(state.admission).toEqual({ cap: 1, active: [occupant], queue: [root, waiting] });
      expect(commands.some((command) => command[3] === "new-window")).toBe(false);
      const full = errorLog.mock.calls
        .map(String)
        .filter((line) => line.includes("admission is full"));
      expect(full).toHaveLength(1);

      // The occupant's issue closes: its tree lingers and its slot frees; the queue head is promoted.
      state.trees[occupant].status = "lingering";
      await processes.releaseSlot(occupant);
      await processes.drainSpawns();

      expect(state.trees[root]).toMatchObject({ status: "active", generation: 2 });
      expect(state.trees[root]?.resumeSessionFile).toBeUndefined();
      expect(state.admission).toEqual({ cap: 1, active: [root], queue: [waiting] });
      expect(state.trees[waiting]).toMatchObject({ status: "queued" });
      const launch = commands.find((command) => command[3] === "new-window");
      expect(launch?.join(" ")).toContain(`--resume=${sessionFile}`);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("neither spawns nor re-queues a root that is already queued for a slot when another wake reaches it (LEGION-83)", async () => {
    // A repeated wake for a root already queued at capacity must not open a pane or
    // enqueue it again; the promotion sweep owns queued roots.
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "queued",
      launchFailures: 0,
      resumeSessionFile: "/state/sessions/root.json",
    };
    const occupant: IssueKey = "LEGION-45";
    const waiting: IssueKey = "LEGION-46";
    tree(state, occupant);
    state.trees[waiting] = { root: waiting, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.active = [occupant];
    state.admission.queue = [root, waiting];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes, commands } = manager(state);

      await processes.resurrect(root);

      expect(state.admission).toEqual({ cap: 1, active: [occupant], queue: [root, waiting] });
      expect(state.trees[root]).toMatchObject({ status: "queued", generation: 1 });
      expect(commands.some((command) => command[3] === "new-window")).toBe(false);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("forgets a tree's kept session file when it reaches launch-failed, so a controller re-admit starts fresh (LEGION-83)", async () => {
    // A kept session file that is what keeps failing (gone from disk: computeResumeArgument's
    // same-agent refusal) would otherwise fail every controller re-admit the same way, forever.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.trees[root] = {
      root,
      generation: 0,
      status: "active",
      launchFailures: 0,
      resumeSessionFile: path.join(stateDir, "missing-session.json"),
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes, commands } = manager(state, { config: config(stateDir) });
      for (let attempt = 0; attempt < 3; attempt += 1) {
        await expect(processes.spawnRoot(root)).rejects.toThrow(
          /recorded OMP session file is missing/
        );
      }
      expect(state.trees[root]).toMatchObject({ status: "launch-failed", launchFailures: 3 });
      expect(state.trees[root]?.resumeSessionFile).toBeUndefined();

      expect(processes.admit(root)).toBe("queued");
      await processes.reconcileAdmission();
      await processes.drainSpawns();

      expect(state.trees[root]).toMatchObject({ status: "active", launchFailures: 0 });
      const launch = commands.find((command) => command[3] === "new-window");
      expect(launch).toBeDefined();
      expect(launch?.join(" ")).not.toContain("--resume=");
    } finally {
      errorLog.mockRestore();
    }
  });

  it("marks a closed tree lingering again when its stale-pane retirement fails, instead of stranding an unreapable locator", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const launchStarted = Promise.withResolvers<void>();
    const launchGate = Promise.withResolvers<void>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "new-window") {
          launchStarted.resolve();
          await launchGate.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 1, stderr: "tmux: server not responding" };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawning = processes.spawnRoot(root);
    await launchStarted.promise;
    // A concurrent close finishes for this exact tree before the post-launch check runs.
    state.trees[root].status = "closed";
    launchGate.resolve();

    await spawning;

    expect(managedState.trees[root]).toMatchObject({
      status: "lingering",
      lingerUntil: "2026-08-24T00:00:00.000Z",
    });
    expect(managedState.trees[root]?.locator).toMatchObject({ tmuxPaneId: "%1" });
  });

  it("records a failed Dispatch status write in pendingStatusWrites without throwing, for both spawn and close", async () => {
    const stateDir = await temporaryDir();
    let sessionExists = false;
    const state = newLegionState("omp", 1);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async () => {
          throw new Error("Dispatch unavailable");
        },
      }),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);

    expect(managedState.trees[root]?.status).toBe("active");
    expect(managedState.pendingStatusWrites[root]).toEqual({
      status: "in_progress",
      statusAtRecord: "todo",
    });

    await processes.closeTree(root);

    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.pendingStatusWrites[root]).toEqual({
      status: "done",
      statusAtRecord: "todo",
    });
  });
  it("gives collision-prone issue paths distinct escaped cosmetic window names", async () => {
    const stateDir = await temporaryDir();
    const left = "ORGLEGIONSMOKE-1";
    const right = "LEGIONSMOKE-1";
    const { manager: processes, commands } = manager(newLegionState("omp", 2), {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.spawnRoot(left);
    await processes.spawnRoot(right);

    const names = commands
      .filter((command) => command[0] === "tmux" && command.includes("-n"))
      .map((command) => command[command.indexOf("-n") + 1]);
    expect(names).toEqual(["orglegionsmoke-1", "legionsmoke-1"]);
  });

  it("caps an escaped cosmetic window name", async () => {
    const stateDir = await temporaryDir();
    const issue = `${"B".repeat(200)}-1`;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
    });

    await processes.spawnRoot(issue);

    const window = commands.find((command) => command[0] === "tmux" && command.includes("-n"));
    expect(window?.[window.indexOf("-n") + 1]).toHaveLength(160);
  });

  it("keeps the worker socket path under the Unix socket length limit for a very long issue key", async () => {
    const stateDir = await temporaryDir();
    const issue = `${"B".repeat(200)}-1`;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
    });

    await processes.spawnRoot(issue);

    const window = commands.find((command) => command[0] === "tmux" && command.includes("-n"));
    const shellCommand = window?.at(-1);
    const socketMatch = shellCommand?.match(/--socket (\S+)/);
    if (!socketMatch?.[1]) throw new Error("launch command is missing its --socket argument");
    expect(Buffer.byteLength(socketMatch[1])).toBeLessThan(100);
  });

  it("records a new tmux window id and probes that id rather than its cosmetic name", async () => {
    const stateDir = await temporaryDir();
    const commands: string[][] = [];
    const { manager: processes, state } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@314 %7 12345\n", exitCode: 0 };
        if (command[3] === "list-panes" && command.includes("%7")) {
          return { stdout: "%7 12345\n", exitCode: 0 };
        }
        return { stdout: "sjawhar-legion-42\n", exitCode: 0 };
      },
    });

    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    await processes.spawnRoot(root);

    expect(state.trees[root]?.locator).toMatchObject({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@314",
      tmuxPaneId: "%7",
    });
    expect(await processes.probe(root)).toBe("alive");
    expect(commands).toContainEqual([
      "tmux",
      "-L",
      "legion-omp",
      "list-panes",
      "-t",
      "%7",
      "-F",
      "#{pane_id} #{pane_pid}",
    ]);
  });

  it("rejects a live pane whose process command is not OMP", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    recordedTmuxLocator(state).tmuxWindowId = "@314";
    const { manager: processes } = manager(state, {
      readProcessCmdline: async () => "bash\0",
      run: liveRun,
    });

    expect(await processes.probe(root)).toBe("dead");
  });

  it("kills only stale daemon-owned windows that are not state locators", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
    };
    const commands: string[][] = [];
    const activitySeconds = Date.parse("2026-08-24T00:00:00.000Z") / 1000;
    const { manager: processes } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-windows") {
          return {
            stdout: [
              `@42\tlegion-omp\t${activitySeconds}`,
              `@43\tlegion-omp\t${activitySeconds}`,
              `@99\tlegion-omp\t${activitySeconds - 121}`,
              `@100\t\t${activitySeconds - 121}`,
            ].join("\n"),
            exitCode: 0,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconcileOrphans();

    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-window", "-t", "@99"]);
    expect(commands).not.toContainEqual(["tmux", "-L", "legion-omp", "kill-window", "-t", "@100"]);
  });

  it("clears a self-reporting root's locator without attempting to stop it (it is the caller, still alive and blocked on this response)", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.admission.active.push(root);
    const { manager: processes, commands } = manager(state);

    await processes.markProcessDead(root);

    expect(state.trees[root]?.locator).toBeUndefined();
    expect(state.trees[root]?.status).toBe("dead");
    expect(commands).toEqual([]);
  });

  it("passes the packaged role prompt path to OMP for root and controller windows, without an --extension flag", async () => {
    const stateDir = await temporaryDir();
    const ompInvocation = "/opt/oh-my-pi/18.0.3/omp";
    const processPath = "/full/bin:/usr/bin";
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      ompInvocation,
      processPath,
      run: async (command) => {
        commands.push(command);
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    await processes.ensureController();

    const workspaceDir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    const controllerDir = path.join(stateDir, "controller");
    const extensionDir = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "architect-9e2fb104.sock");
    const controllerSocketPath = path.join(stateDir, "workers", "controller.sock");
    const windows = commands.filter((command) => command[3] === "new-window");
    expect(windows.map((command) => command.at(-1))).toEqual([
      `export PATH=${path.join(stateDir, "worker-bin")}${path.delimiter}${processPath} && cd ${workspaceDir} && ${process.execPath} ${entrypoint} worker-shim --socket ${socketPath} -- ${ompInvocation} --mode rpc ${promptArgument(`${extensionDir}/roles/architect-root.md`, rootArchitectFragment())}`,
      `export PATH=${path.join(stateDir, "worker-bin")}${path.delimiter}${processPath} && cd ${controllerDir} && ${process.execPath} ${entrypoint} worker-shim --socket ${controllerSocketPath} -- ${ompInvocation} --mode rpc ${promptArgument(`${extensionDir}/roles/controller-root.md`, undefined)}`,
    ]);
    const expectedPanePath = `${path.join(stateDir, "worker-bin")}${path.delimiter}${processPath}`;
    expect(windows.map((command) => tmuxPanePath(command))).toEqual([
      expectedPanePath,
      expectedPanePath,
    ]);
  });

  it("prepends the configured omp_launch_prefix before the OMP invocation for root and controller windows", async () => {
    const stateDir = await temporaryDir();
    const ompInvocation = "/opt/oh-my-pi/18.0.3/omp";
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir, { ompLaunchPrefix: ["secrets", "ANTHROPIC_API_KEY", "--"] }),
      ompInvocation,
      run: async (command) => {
        commands.push(command);
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    await processes.ensureController();

    const windows = commands.filter((command) => command[3] === "new-window");
    for (const command of windows) {
      expect(command.at(-1)).toContain(
        `-- secrets ANTHROPIC_API_KEY -- ${ompInvocation} --mode rpc`
      );
    }
    expect(windows).toHaveLength(2);
  });

  it("prepends the configured omp_launch_prefix before the OMP invocation for a phase worker's window", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const stateDir = await temporaryDir();
    const ompInvocation = "/opt/oh-my-pi/18.0.3/omp";
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { ompLaunchPrefix: ["secrets", "ANTHROPIC_API_KEY", "--"] }),
      ompInvocation,
    });

    await processes.spawnWorker(root, child, "implementer", "implement it");

    const windows = commands.filter(
      (command) => command[3] === "new-window" || command[3] === "split-window"
    );
    expect(windows.length).toBeGreaterThan(0);
    for (const command of windows) {
      expect(command.at(-1)).toContain(
        `-- secrets ANTHROPIC_API_KEY -- ${ompInvocation} --mode rpc`
      );
    }
  });

  it("hands root, worker, and controller windows exactly one --append-system-prompt whose value joins every fragment in order — role prompt, addressing (with the root's gate policy), then the deployment instructions — when configured", async () => {
    const state = newLegionState("omp", 1);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const stateDir = await temporaryDir();
    const deploymentInstructionsFile = path.join(stateDir, "deployment-instructions.md");
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      deploymentInstructionsFile,
      run: async (command) => {
        commands.push(command);
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        if (command[3] === "split-window") return { stdout: "%2 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    await processes.spawnWorker(root, child, "implementer", "implement it");
    await processes.ensureController();

    const extensionDir = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const launches = commands
      .filter((command) => command[3] === "new-window" || command[3] === "split-window")
      .map((command) => command.at(-1) ?? "");
    expect(launches).toHaveLength(3);
    const [rootLaunch, workerLaunch, controllerLaunch] = launches as [string, string, string];

    // OMP's `--append-system-prompt` is last-wins (its argv handler assigns
    // `appendSystemPrompt`), so a pane given three flags would receive only the deployment
    // instructions — no role prompt, no addressing, no gate policy. Every launch therefore carries
    // exactly one flag holding all of its fragments, in order, separated by a blank line.
    expect(launches.map((launch) => launch.split("--append-system-prompt ").length - 1)).toEqual([
      1, 1, 1,
    ]);
    expect(rootLaunch).toEndWith(
      ` --mode rpc ${promptArgument(`${extensionDir}/roles/architect-root.md`, rootArchitectFragment(), deploymentInstructionsFile)}`
    );
    expect(workerLaunch).toEndWith(
      ` --mode rpc ${promptArgument(`${extensionDir}/roles/implementer.md`, addressingFragment("omp", root, child, "implementer"), deploymentInstructionsFile)}`
    );
    expect(controllerLaunch).toEndWith(
      ` --mode rpc ${promptArgument(`${extensionDir}/roles/controller-root.md`, undefined, deploymentInstructionsFile)}`
    );
    // Order inside the one value: role prompt, then the addressing text (which for the root ends
    // with the gate policy), then the instructions.
    const rootValue = rootLaunch.slice(rootLaunch.indexOf('--append-system-prompt "') + 24, -1);
    const parts = rootValue.split("\n\n");
    expect(parts[0]).toBe(`$(cat ${extensionDir}/roles/architect-root.md)`);
    expect(parts.at(-1)).toBe(`$(cat ${deploymentInstructionsFile})`);
    expect(parts.slice(1, -1).join("\n\n")).toContain(
      "Design gate policy: \\`gates.design: root-issues\\`"
    );
  });

  it("rolls back a failed tmux launch instead of retaining an active tree or admission slot", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    let saves = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        saves += 1;
      },
      run: async (command) =>
        command[3] === "new-window"
          ? { stdout: "window creation failed", exitCode: 1 }
          : { stdout: "", exitCode: 0 },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");

    expect(state.trees[root]).toEqual({
      root,
      generation: 0,
      status: "queued",
      launchFailures: 1,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [root] });
    expect(saves).toBeGreaterThan(0);
  });

  it("rolls back a failed tmux session creation before recording a root locator", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 1 };
        if (command[3] === "new-session") return { stdout: "session creation failed", exitCode: 1 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-session failed");

    expect(commands.some((command) => command[3] === "new-window")).toBeFalse();
    expect(state.trees[root]).toEqual({
      root,
      generation: 0,
      status: "queued",
      launchFailures: 1,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [root] });
  });

  it("two roots whose first spawns race onto a not-yet-existing private server both launch; neither is charged a launch failure", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 2);
    for (const issue of [root, child]) {
      state.issues[issue] = { key: issue, title: issue, status: "todo", children: [] };
      state.trees[issue] = { root: issue, generation: 0, status: "active", launchFailures: 0 };
    }
    state.admission.active.push(root, child);
    const commands: string[][] = [];
    const newSessionIssued = Promise.withResolvers<void>();
    const newSessionGate = Promise.withResolvers<void>();
    let sessionExists = false;
    let creating = false;
    let windowCount = 0;
    const reached: Record<IssueKey, PromiseWithResolvers<void>> = {
      [root]: Promise.withResolvers<void>(),
      [child]: Promise.withResolvers<void>(),
    };
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
      run: async (command) => {
        commands.push(command);
        switch (command[3]) {
          case "has-session":
            return { stdout: "", exitCode: sessionExists ? 0 : 1 };
          case "new-session":
            if (sessionExists || creating) {
              return { stdout: "", stderr: "duplicate session: legion-omp", exitCode: 1 };
            }
            creating = true;
            newSessionIssued.resolve();
            await newSessionGate.promise;
            creating = false;
            sessionExists = true;
            return { stdout: "", exitCode: 0 };
          case "new-window":
            windowCount += 1;
            return { stdout: `@${windowCount} %${windowCount} 12345\n`, exitCode: 0 };
          default:
            return { stdout: "", exitCode: 0 };
        }
      },
      issueLocators: (issue) => {
        reached[issue]?.resolve();
        return locatorsForIssue(state, issue);
      },
    });

    const first = processes.spawnRoot(root);
    await newSessionIssued.promise;
    const second = processes.spawnRoot(child);
    const both = Promise.all([first, second]);
    await reached[child]?.promise;
    await flushEventLoop(1);
    newSessionGate.resolve();
    await both;

    expect(state.trees[root]).toMatchObject({ status: "active", launchFailures: 0 });
    expect(state.trees[child]).toMatchObject({ status: "active", launchFailures: 0 });
    const rootWindow = state.trees[root]?.locator;
    const childWindow = state.trees[child]?.locator;
    if (rootWindow?.runtime !== "tmux" || childWindow?.runtime !== "tmux") {
      throw new Error("both roots record tmux locators");
    }
    expect(rootWindow.tmuxWindowId).toBeDefined();
    expect(rootWindow.tmuxWindowId).not.toBe(childWindow.tmuxWindowId);
    expect(state.admission).toEqual({ cap: 2, active: [root, child], queue: [] });
    const verbs = commands.map((command) => command[3]);
    expect(verbs.filter((verb) => verb === "new-session")).toHaveLength(1);
    expect(verbs.filter((verb) => verb === "new-window")).toHaveLength(2);
    expect(
      commands.filter(
        (command) => command[3] === "kill-window" && command[5] === "legion-omp:__legion_bootstrap"
      )
    ).toHaveLength(1);
  });

  it("fails a root launch before tmux when its architect prompt is missing", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      statPrompt: async (promptPath) => {
        throw new Error(`Missing prompt: ${promptPath}`);
      },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("Missing prompt");

    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    expect(state.trees[root]).toEqual({
      root,
      generation: 0,
      status: "queued",
      launchFailures: 1,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [root] });
  });

  it("promotes the next queued tree when a failed launch releases capacity", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    let completePromotion: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      completePromotion = resolve;
    });
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "new-window" && command.includes(`LEGION_TREE=${root}`)) {
          return { stdout: "root launch failed", exitCode: 1 };
        }
        if (command[3] === "new-window" && command.includes(`LEGION_TREE=${child}`)) {
          completePromotion?.();
          return { stdout: "@2 %2 4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
    await promoted;

    expect(state.admission).toEqual({ cap: 1, active: [child], queue: [root] });
  });

  it("attempts each queued tree once in a bounded promotion sweep when launches keep failing", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    let attempts = 0;
    let finishSweep: (() => void) | undefined;
    const sweepFinished = new Promise<void>((resolve) => {
      finishSweep = resolve;
    });
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] !== "new-window") return { stdout: "", exitCode: 0 };
        attempts += 1;
        return { stdout: "window creation failed", exitCode: 1 };
      },
    });

    const originalConsoleError = console.error;
    console.error = () => {
      finishSweep?.();
    };
    try {
      await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
      await sweepFinished;

      expect(attempts).toBe(2);
      expect(state.trees[root]).toMatchObject({
        status: "queued",
        launchFailures: 1,
      });
      expect(state.trees[child]).toMatchObject({
        status: "queued",
        launchFailures: 1,
      });
    } finally {
      console.error = originalConsoleError;
    }
  });

  it("promotes queued trees into slots opened by a cap raised between restarts", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.queue.push(root, child);
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
    });

    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.issues[child] = { key: child, title: "Child", status: "todo", children: [] };
    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 2, active: [root, child], queue: [] });
    expect(state.trees[root]?.status).toBe("active");
    expect(state.trees[child]?.status).toBe("active");
  });

  it("demotes a persisted active tree with no recorded locator back to queued and re-spawns it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    // Simulates a crash between advancePromotionSweep's persist() (which
    // marks a promoted tree "active") and startRoot ever recording a
    // locator: on disk, the tree is active but nothing is running.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@77 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.reconcileAdmission();

      expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
      expect(state.trees[root]).toMatchObject({
        status: "active",
        locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@77" },
      });
      expect(errorLog).toHaveBeenCalledWith(expect.stringContaining(`demoted ${root}`));
    } finally {
      errorLog.mockRestore();
    }
  });

  it("adds an active tree missing from admission.active back at boot, with one log line, and promotes nothing into a slot it holds (LEGION-83, acceptance 3)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state); // active, with a locator: the 09:31 shape once `admission.active` forgets it
    state.admission.active = [];
    const queued: IssueKey = "LEGION-45";
    state.trees[queued] = { root: queued, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue = [queued];
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    state.issues[queued] = { key: queued, title: "Queued", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes, commands } = manager(state, { config: config(stateDir) });

      await processes.reconcileAdmission();

      expect(state.admission).toEqual({ cap: 1, active: [root], queue: [queued] });
      expect(state.trees[queued]).toMatchObject({ status: "queued" });
      expect(commands.some((command) => command[3] === "new-window")).toBe(false);
      const lines = errorLog.mock.calls
        .map(String)
        .filter((line) => line.includes("admission drift"));
      expect(lines).toHaveLength(1);
      expect(lines[0]).toContain(`added ${root}`);
      expect(lines[0]).toContain("at boot");
    } finally {
      errorLog.mockRestore();
    }
  });

  it("removes a stale admission.active entry at boot so the freed slot is promoted into (LEGION-83)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].status = "lingering";
    state.admission.active = [root];
    const queued: IssueKey = "LEGION-45";
    state.trees[queued] = { root: queued, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue = [queued];
    state.issues[root] = { key: root, title: "Root", status: "backlog", children: [] };
    state.issues[queued] = { key: queued, title: "Queued", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes } = manager(state, { config: config(stateDir) });

      await processes.reconcileAdmission();
      await processes.drainSpawns();

      expect(state.admission).toEqual({ cap: 1, active: [queued], queue: [] });
      expect(state.trees[queued]).toMatchObject({ status: "active" });
      expect(state.trees[root]).toMatchObject({ status: "lingering" });
      const lines = errorLog.mock.calls
        .map(String)
        .filter((line) => line.includes("admission drift"));
      expect(lines).toHaveLength(1);
      expect(lines[0]).toContain(`removed ${root}`);
      expect(lines[0]).toContain("tree is lingering");
    } finally {
      errorLog.mockRestore();
    }
  });

  it("demotes an active tree with no recorded locator even when it also fell out of admission.active, never re-admitting it as a slot with nothing running (LEGION-83)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.trees[root] = { root, generation: 1, status: "active", launchFailures: 0 };
    state.admission.active = [];
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes } = manager(state, {
        config: config(stateDir),
        run: async (command) => {
          if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
          if (command[3] === "new-window") return { stdout: "@77 %1 4242\n", exitCode: 0 };
          return { stdout: "", exitCode: 0 };
        },
      });

      await processes.reconcileAdmission();

      // Demoted to queued, then promoted into the free slot with a real locator -- exactly the
      // in-admission case above, not an "added" repair of a process-less tree.
      expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
      expect(state.trees[root]).toMatchObject({
        status: "active",
        generation: 2,
        locator: { tmuxWindowId: "@77" },
      });
      const logged = errorLog.mock.calls.map(String);
      expect(logged.some((line) => line.includes(`demoted ${root}`))).toBe(true);
      expect(logged.some((line) => line.includes("admission drift"))).toBe(false);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("promotes nothing while admission.active exceeds the cap, stops nothing, and warns once (LEGION-83, acceptance 4)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const second: IssueKey = "LEGION-45";
    const queued: IssueKey = "LEGION-46";
    tree(state);
    tree(state, second);
    state.trees[second].locator = { ...recordedTmuxLocator(state, second), tmuxPaneId: "%5" };
    state.trees[queued] = { root: queued, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.active = [root, second]; // the operator raised occupancy by hand (the 09:31 repair)
    state.admission.queue = [queued];
    for (const key of [root, second]) {
      state.issues[key] = { key, title: key, status: "in_progress", children: [] };
    }
    state.issues[queued] = { key: queued, title: "Queued", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const {
        manager: processes,
        commands,
        controlRequests,
      } = manager(state, { config: config(stateDir) });

      await processes.reconcileAdmission();

      expect(state.admission).toEqual({ cap: 1, active: [root, second], queue: [queued] });
      expect(state.trees[queued]).toMatchObject({ status: "queued" });
      expect(state.trees[root]).toMatchObject({ status: "active" });
      expect(state.trees[second]).toMatchObject({ status: "active" });
      expect(
        commands.some(
          (command) =>
            command[3] === "new-window" ||
            command[3] === "kill-pane" ||
            command[3] === "kill-window"
        )
      ).toBe(false);
      expect(controlRequests).toEqual([]);
      const lines = errorLog.mock.calls.map(String).filter((line) => line.includes("over cap"));
      expect(lines).toHaveLength(1);
      expect(lines[0]).toContain("2 active trees");
      expect(lines[0]).toContain("cap of 1");
    } finally {
      errorLog.mockRestore();
    }
  });

  it("reaps an unrecorded owner-marked window at boot, with no grace period, before re-spawning a demoted tree", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    // Same crash as above, plus a leaked tmux window from that same prior
    // spawn attempt: nothing in state names it, so boot must not wait out
    // the periodic sweep's grace period to reap it.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const killedWindows: string[] = [];
    let newWindowCalls = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "list-windows") {
          return {
            stdout: `@99\tlegion-omp\t${Date.parse("2026-08-24T00:00:00.000Z") / 1000}\n`,
            exitCode: 0,
          };
        }
        if (command[3] === "kill-window") {
          killedWindows.push(command[5] ?? "");
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") {
          newWindowCalls += 1;
          return { stdout: "@77 %1 4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.reconcileAdmission();

      expect(killedWindows).toEqual(["@99"]);
      expect(newWindowCalls).toBe(1);
      expect(state.trees[root]).toMatchObject({
        status: "active",
        locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@77" },
      });
    } finally {
      errorLog.mockRestore();
    }
  });

  it("kills a just-created window and rolls back the launch when its ownership marker fails", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const killedWindows: string[] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@88 %1 4242\n", exitCode: 0 };
        if (command[3] === "set-option" && command[4] === "-w") {
          return { stdout: "marker rejected", exitCode: 1 };
        }
        if (command[3] === "kill-window") {
          killedWindows.push(command[5] ?? "");
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Every window is either recorded (locator assigned) or reaped: the
    // marker never landed, so nothing would ever find this window again —
    // it must be killed synchronously instead of leaked.
    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux window ownership marker failed");

    expect(killedWindows).toEqual(["@88"]);
    expect(state.trees[root]?.status).toBe("queued");
    expect(state.trees[root]?.locator).toBeUndefined();
  });

  it("dequeue removes a waiting root's queue entry and its queued tree record, and persists once", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Finished", status: "done", children: [] };
    state.trees[root] = { root, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue.push(root);
    let saves = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        saves += 1;
      },
    });

    await processes.dequeue(root);

    expect(state.admission).toEqual({ cap: 1, active: [], queue: [] });
    expect(state.trees[root]).toBeUndefined();
    expect(saves).toBe(1);
  });

  it("dequeue drops only the queue entry of a launch-failed root, and is a silent no-op for an issue that is not waiting", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Parked", status: "backlog", children: [] };
    // A launch-failed root legitimately sits in the queue (see the reconcile cases below); its
    // record belongs to the launch-failure and re-admission paths, never to dequeue.
    state.trees[root] = { root, generation: 2, status: "launch-failed", launchFailures: 3 };
    state.admission.queue.push(root);
    let saves = 0;
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        saves += 1;
      },
    });

    try {
      await processes.dequeue(root);
      expect(state.admission.queue).toEqual([]);
      expect(state.trees[root]).toMatchObject({ status: "launch-failed", launchFailures: 3 });
      expect(saves).toBe(1);

      // Nothing recorded for `child` at all: no mutation, no save, no log line.
      await processes.dequeue(child);
      expect(saves).toBe(1);
      expect(errorLog).not.toHaveBeenCalled();
    } finally {
      errorLog.mockRestore();
    }
  });

  it("drops a finished issue from the head of the queue at boot, logs it, and promotes the todo entry behind it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    // LEGION-56's shape: closed in Dispatch, still first in line with a queued tree record.
    state.issues[root] = { key: root, title: "Finished", status: "done", children: [] };
    state.issues[child] = { key: child, title: "Waiting", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "queued", launchFailures: 0 };
    state.trees[child] = { root: child, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue.push(root, child);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { admissionCap: 1 }),
    });

    try {
      await processes.reconcileAdmission();

      expect(state.admission).toEqual({ cap: 1, active: [child], queue: [] });
      expect(state.trees[root]).toBeUndefined();
      expect(state.trees[child]?.status).toBe("active");
      expect(errorLog).toHaveBeenCalledWith(
        `[legion] dropped ${root} from the admission queue at boot: Dispatch status "done"`
      );
      // Exactly one root pane opened, and it is the waiting issue's.
      const windows = commands.filter(
        (command) => command[0] === "tmux" && command[3] === "new-window"
      );
      expect(windows).toHaveLength(1);
      expect(tmuxWindowEnvironment(windows[0]).LEGION_TREE).toBe(child);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("drops a queue entry with no issue record at boot, naming it unknown", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.queue.push(root);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
    });

    try {
      await processes.reconcileAdmission();

      expect(state.admission).toEqual({ cap: 2, active: [], queue: [] });
      expect(state.trees[root]).toBeUndefined();
      expect(errorLog).toHaveBeenCalledWith(
        `[legion] dropped ${root} from the admission queue at boot: unknown issue`
      );
    } finally {
      errorLog.mockRestore();
    }
  });

  it("leaves launch-failed trees queued when reconciling admission capacity", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.trees[root].status = "launch-failed";
    state.admission.queue.push(root);
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 2, active: [], queue: [root] });
  });

  it("skips launch-failed entries while promoting eligible queued trees during admission reconciliation", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    for (const issue of [root, child, grandchild]) {
      state.issues[issue] = { key: issue, title: issue, status: "todo", children: [] };
    }
    state.trees[root].status = "launch-failed";
    state.admission.queue.push(root, child, grandchild);
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 2, active: [child, grandchild], queue: [root] });
    expect(state.trees[root].status).toBe("launch-failed");
    expect(state.trees[child]?.status).toBe("active");
    expect(state.trees[grandchild]?.status).toBe("active");
  });

  it("persists a raised cap even when reconciliation has no queued trees to promote", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    let saves = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 3 }),
      saveState: async () => {
        saves += 1;
      },
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 3, active: [], queue: [] });
    expect(saves).toBeGreaterThan(0);
  });

  it("attempts each queued tree exactly once when every boot-promoted launch fails", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.queue.push(root, child, grandchild);
    for (const issue of [root, child, grandchild]) {
      state.issues[issue] = { key: issue, title: issue, status: "todo", children: [] };
    }
    const settled = Promise.withResolvers<void>();
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
      saveState: async () => {
        const failures = [root, child, grandchild].reduce(
          (sum, issue) => sum + (state.trees[issue]?.launchFailures ?? 0),
          0
        );
        if (failures === 3) settled.resolve();
      },
      run: async (command) =>
        command[3] === "new-window"
          ? { stdout: "window creation failed", exitCode: 1 }
          : { stdout: "", exitCode: 0 },
    });

    await processes.reconcileAdmission();
    await settled.promise;
    await Promise.resolve();

    expect(state.admission.active).toEqual([]);
    expect([...state.admission.queue].sort()).toEqual([root, child, grandchild].sort());
    expect(state.trees[root]?.launchFailures).toBe(1);
    expect(state.trees[child]?.launchFailures).toBe(1);
    expect(state.trees[grandchild]?.launchFailures).toBe(1);
  });

  it("never promotes stale queue entries whose trees are not queued", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const closed = "LEGION-45";
    tree(state, root);
    tree(state, child);
    tree(state, grandchild);
    tree(state, closed);
    for (const issue of [root, child, grandchild, closed]) {
      state.issues[issue] = { key: issue, title: issue, status: "todo", children: [] };
    }
    state.trees[root].status = "active";
    state.trees[child].status = "lingering";
    state.trees[grandchild].status = "dead";
    state.trees[closed].status = "closed";
    state.admission.active.push(root);
    state.admission.queue.push(root, child, grandchild, closed);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { admissionCap: 5 }),
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({
      cap: 5,
      active: [root],
      queue: [root, child, grandchild, closed],
    });
    expect(commands).toEqual([
      [
        "tmux",
        "-L",
        "legion-omp",
        "list-windows",
        "-t",
        "legion-omp",
        "-F",
        "#{window_id}\t#{@legion_owner}\t#{window_activity}",
      ],
      [
        "tmux",
        "-L",
        "legion-omp",
        "list-panes",
        "-a",
        "-F",
        "#{pane_id}\t#{window_id}\t#{@legion_owner}\t#{pane_start_command}\t#{pane_activity}",
      ],
    ]);
  });

  it("marks a tree launch-failed after its third launch failure and publishes a controller anomaly", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      run: async (command) =>
        command[3] === "new-window"
          ? { stdout: "window creation failed", exitCode: 1 }
          : { stdout: "", exitCode: 0 },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");

    expect(state.trees[root]).toMatchObject({
      status: "launch-failed",
      launchFailures: 3,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [] });
    expect(publications).toEqual([
      {
        subject: `notifications.role.${controllerToken("omp")}`,
        json: JSON.stringify({
          type: "launch-failed",
          issue: root,
          failures: 3,
        }),
      },
    ]);
  });

  it("releases the failed launch's secret-file hold even when the launch-failed publish throws, so the next persist still reaps the file", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    // Two prior failures: this attempt crosses MAX_LAUNCH_FAILURES and publishes the controller
    // anomaly — the one step in spawnRoot's catch that can throw after the rollback.
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 2 };
    let sessionExists = false;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      publishRole: (subject) => {
        if (subject === `notifications.role.${controllerToken("omp")}`) {
          throw new Error("nats down");
        }
      },
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window" && command[command.indexOf("-n") + 1] === "legion-42") {
          return { stdout: "window creation failed", exitCode: 1 };
        }
        if (command[3] === "new-window") return { stdout: "@1 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });
    const architectFile = path.join(stateDir, "secrets", roleToken("omp", root, "architect"));

    await expect(processes.spawnRoot(root)).rejects.toThrow("nats down");
    expect(state.trees[root]).toMatchObject({ status: "launch-failed", launchFailures: 3 });
    // The boot token was written before the failed tmux call and the catch never reached its
    // own persist, so the file is still there...
    expect(await readFile(architectFile, "utf8")).toBe("boot-token");

    // ...and the very next persist (here: the controller spawn's) must be free to reap it. A
    // hold leaked past the throw would keep it exempt for the daemon's lifetime.
    await processes.ensureController();

    expect((await readdir(path.join(stateDir, "secrets"))).sort()).toEqual([
      "legion-omp-controller",
    ]);
  });

  it("kills the just-spawned pane before propagating a saveState failure after a successful spawn, without rolling back the launch or requeuing it as a failure", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const killedPanes: string[] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@42 %1 4242\n", exitCode: 0 };
        // The pane is still exactly the process the spawn just recorded, so the kill is allowed.
        if (command[3] === "list-panes") return livePanes(command, 4242);
        if (command[3] === "kill-pane") {
          killedPanes.push(command[5] ?? "");
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      saveState: async () => {
        throw new Error("disk full");
      },
    });

    // The spawn itself (tmux pane, locator, generation) already
    // succeeded before this save runs — only persisting that fact failed.
    // Treating this like a launch failure would roll back the tracked
    // locator and requeue the tree while a real process keeps running, so
    // instead exactly that pane is killed (never its whole window, which a
    // resurrected root can share with live sibling worker panes) and the
    // failure propagates distinctly (see `SpawnPersistenceFailure`).
    await expect(processes.spawnRoot(root)).rejects.toThrow("disk full");

    expect(killedPanes).toEqual(["%1"]);
    expect(state.trees[root]).toMatchObject({
      generation: 1,
      status: "active",
      launchFailures: 0,
      locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@42" },
    });
    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
  });

  it("clears a launch-failed tree's counter when controller admission retries it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 3,
      status: "launch-failed",
      launchFailures: 3,
    };
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async () => ({ stdout: "", exitCode: 0 }),
    });

    expect(processes.admit(root)).toBe("queued");

    expect(state.trees[root]).toMatchObject({
      status: "active",
      launchFailures: 0,
    });
  });

  it("schedules a queued failed tree immediately when controller admission retries it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "queued",
      launchFailures: 1,
    };
    state.admission.queue.push(root);
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async () => ({ stdout: "", exitCode: 0 }),
    });

    expect(processes.admit(root)).toBe("queued");

    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
    expect(state.trees[root]).toMatchObject({
      status: "active",
      launchFailures: 1,
    });
  });

  it("serializes concurrent resurrection attempts for the same dead generation", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    let windows = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "list-windows") return { stdout: "", exitCode: 1 };
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") {
          windows += 1;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[3] === "list-panes" && command.includes("#{pane_id} #{pane_pid}")) {
          return windows > 0 ? livePanes(command) : paneGone();
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await Promise.all([processes.resurrect(root), processes.resurrect(root)]);

    expect(windows).toBe(1);
    expect(state.trees[root].generation).toBe(2);
  });

  it("resumes the recorded OMP session when resurrecting a dead root", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
    });

    await processes.resurrect(root);

    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    const extension = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "architect-9e2fb104.sock");
    const launch = commands.find((command) => command[0] === "tmux" && command[3] === "new-window");
    expect(launch?.at(-1)).toBe(
      `export PATH=${path.join(stateDir, "worker-bin")}${path.delimiter}/full/bin:/usr/bin && cd ${workspace} && ${process.execPath} ${entrypoint} worker-shim --socket ${socketPath} -- /opt/oh-my-pi/18.0.3/omp --resume=${sessionFile} --mode rpc ${promptArgument(`${extension}/roles/architect-root.md`, rootArchitectFragment())}`
    );
  });

  it("starts fresh when launching a root outside the resurrection path", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    const log = vi.spyOn(console, "info").mockImplementation(() => {});
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
    });

    try {
      await processes.spawnRoot(root);
      expect(log).not.toHaveBeenCalled();
    } finally {
      log.mockRestore();
    }

    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    const extension = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "architect-9e2fb104.sock");
    const launch = commands.find((command) => command[0] === "tmux" && command[3] === "new-window");
    expect(launch?.at(-1)).toBe(
      `export PATH=${path.join(stateDir, "worker-bin")}${path.delimiter}/full/bin:/usr/bin && cd ${workspace} && ${process.execPath} ${entrypoint} worker-shim --socket ${socketPath} -- /opt/oh-my-pi/18.0.3/omp --mode rpc ${promptArgument(`${extension}/roles/architect-root.md`, rootArchitectFragment())}`
    );
  });

  it("reads every role prompt from deps.rolePromptsDir — the checkout's pi-envoy/roles on a tmux host, /opt/legion/roles in the worker image — never a path relative to its own source", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      rolePromptsDir: "/opt/legion/roles",
    });

    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "test it");

    const launches = commands
      .filter((command) => command[3] === "new-window" || command[3] === "split-window")
      .map((command) => command.at(-1) ?? "");
    expect(launches).toHaveLength(2);
    expect(launches[0]).toContain("$(cat /opt/legion/roles/architect-root.md)");
    expect(launches[1]).toContain("$(cat /opt/legion/roles/tester.md)");
    expect(launches.join("\n")).not.toContain("pi-envoy");
  });

  it("tells a root architect in its system prompt when the project's design gate is off", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { gates: { design: "off" } }),
    });

    await processes.spawnRoot(root);

    const launch = commands.find((command) => command[0] === "tmux" && command[3] === "new-window");
    const argv = launch?.at(-1) ?? "";
    expect(argv).toEndWith(
      promptArgument(
        `${path.resolve(import.meta.dir, "../../../../pi-envoy")}/roles/architect-root.md`,
        rootArchitectFragment("off")
      )
    );
    // Inside the one double-quoted prompt word, the sentence's backticks are shell-escaped.
    expect(argv).toContain("\\`gates.design: off\\`");
    expect(argv).not.toContain("gates.design: root-issues");
  });

  it("fails a resurrection loudly when the recorded OMP session file is missing, never starting fresh", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "missing-architect-session.json");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
    });

    await expect(processes.resurrect(root)).rejects.toThrow(/recorded OMP session file is missing/);

    expect(
      commands.some((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toBeFalse();
    expect(state.trees[root].launchFailures).toBe(1);
    expect(state.trees[root].status).toBe("queued");
  });

  it("clears completed-tree phases and releases its admission slot at linger start, then shuts down its recorded tmux tree", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.roles[roleToken("omp", root, "architect")] = {
      issue: root,
      role: "architect",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "done",
      parent: root,
      children: [],
    };
    state.phases[root] = { phase: "merger", sessionId: "ses_root_merger" };
    state.phases[child] = { phase: "reviewer", sessionId: "ses_child_reviewer" };
    const commands: string[][] = [];
    const { manager: processes, publications } = manager(state, {
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.beginLinger(root);

    expect(state.trees[root]).toMatchObject({
      status: "lingering",
      lingerUntil: "2026-08-24T02:00:00.000Z",
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [] });
    expect(state.phases[root]).toBeUndefined();
    expect(state.phases[child]).toBeUndefined();

    await processes.expireLinger(root);

    expect(state.trees[root].status).toBe("closed");
    expect(state.roles[roleToken("omp", root, "architect")]).toBeUndefined();
    expect(state.phases[root]).toBeUndefined();
    expect(state.phases[child]).toBeUndefined();
    expect(publications).toEqual([]);
    // The root's pane is live and still the process its locator recorded, but its shim socket
    // refuses, so `stopProcess` falls through to reaping that recorded pane.
    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%0"]);
  });

  it("awaits a promoted queued tree's full spawn attempt before beginLinger resolves", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    state.issues[child] = { key: child, title: "Child", status: "todo", children: [] };
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@99 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // The whole release-promote-spawn cascade releaseSlot triggers must
    // settle before beginLinger's own promise resolves: otherwise the
    // durable transaction's outer save (applyDurableEvent) could persist
    // child as "active" before its spawn recorded a locator, and a crash
    // in that window would leave it consuming a slot with no tmux window
    // forever (reconcileAdmission only promotes queued work at boot, it
    // never resurrects an already-active tree with no locator).
    await processes.beginLinger(root);

    expect(state.admission).toEqual({ cap: 1, active: [child], queue: [] });
    expect(state.trees[child]).toMatchObject({
      status: "active",
      locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@99" },
    });
  });

  it("propagates a promoted spawn's persistence failure out of beginLinger instead of swallowing it in startRoot", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    state.issues[child] = { key: child, title: "Child", status: "todo", children: [] };
    let saveCalls = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@99 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
      saveState: async () => {
        saveCalls += 1;
        // The first save (child promoted into active, spliced from queue)
        // must succeed; the second — spawnRoot's own save after its
        // successful spawn — is the one under test.
        if (saveCalls === 2) throw new Error("disk full");
      },
    });

    // A SpawnPersistenceFailure must never be treated as a launch failure
    // by startRoot (which would roll back the just-created tmux window
    // and requeue child) — it must propagate all the way out of
    // beginLinger, so the durable transaction dispatching this linger
    // effect fails and goes fatal, exactly like any other durable effect
    // whose post-mutation save fails.
    await expect(processes.beginLinger(root)).rejects.toThrow("disk full");

    expect(state.trees[child]).toMatchObject({
      status: "active",
      launchFailures: 0,
      locator: { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@99" },
    });
    expect(state.admission.active).toEqual([child]);
    expect(state.admission.queue).toEqual([]);
  });
  it("gracefully stops the root and every worker under the tree via their own shim sockets before removing their claims", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const shutdownCalls: string[] = [];
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      // `probe(root)` must find the recorded root pane alive -- still its recorded process -- so
      // `closeTree`'s unilateral (non-self-report) leg actually attempts the root's own graceful
      // stop instead of skipping straight to a kill on the assumption nothing is there.
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        const client = fakeWorkerRpcClient();
        const shutdown = client.shutdown.bind(client);
        client.shutdown = () => {
          shutdownCalls.push(socketPath);
          shutdown();
        };
        return client;
      },
    });

    await processes.closeTree(root);

    expect(shutdownCalls.sort()).toEqual([
      "/state/workers/architect.sock",
      "/state/workers/implementer.sock",
    ]);
    expect(commands.filter((command) => command[3] === "kill-window")).toEqual([]);
    expect(commands.filter((command) => command[3] === "kill-pane")).toEqual([]);
    expect(state.roles[roleToken("omp", child, "implementer")]).toBeUndefined();
    expect(state.trees[root].locator).toBeUndefined();
    expect(state.trees[root].status).toBe("closed");
  });

  it("skips gracefully stopping the root's own process on a self-report, but still gracefully stops every worker", async () => {
    // markProcessDead's doc comment explains why: the architect's own `session_shutdown` hook
    // awaits `/process/exit` before OMP exits, so a self-reported closeTree IS that same
    // still-running process — asking its own shim to close would deadlock forever waiting on
    // itself. This is the one case where `probe`'s "alive" would otherwise be true but the root
    // leg must be skipped unconditionally.
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const connectedSockets: string[] = [];
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return fakeWorkerRpcClient();
      },
    });

    await processes.closeTree(root, { stopRoot: false });

    expect(connectedSockets).toEqual(["/state/workers/implementer.sock"]);
    expect(commands.filter((command) => command[3] === "kill-window")).toEqual([]);
    expect(commands.filter((command) => command[3] === "kill-pane")).toEqual([]);
    expect(state.trees[root].locator).toBeUndefined();
    expect(state.trees[root].status).toBe("closed");
  });

  it("kills only a timed-out worker's own pane on a tree close, leaving a sibling that closed gracefully untouched", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.issues[grandchild] = {
      key: grandchild,
      title: "Grandchild",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/hung.sock",
        ...paneIdentity(),
      },
    };
    state.roles[roleToken("omp", grandchild, "tester")] = {
      issue: grandchild,
      role: "tester",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@100",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/graceful.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      // A real macrotask (not an instantly-resolving microtask) for the timeout: microtasks
      // (including the default fake client's queueMicrotask-deferred close) always fully drain
      // before a timer fires, so the graceful worker's close deterministically wins this race
      // without relying on engine-specific microtask-tick counting.
      sleep: async () => new Promise<void>((resolve) => setTimeout(resolve, 0)),
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        if (socketPath === "/state/workers/hung.sock") {
          const stuck = fakeWorkerRpcClient();
          stuck.shutdown = () => {};
          return stuck;
        }
        return fakeWorkerRpcClient();
      },
    });

    await processes.closeTree(root);

    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
    expect(commands).not.toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%2"]);
    expect(commands).not.toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%0"]);
    expect(commands.filter((command) => command[3] === "kill-window")).toEqual([]);
  });

  it("closes a tree whose root locator has no pane id: the graceful shutdown still goes out over its socket, nothing is killed, nothing throws", async () => {
    // A tree locator's `tmuxPaneId` is optional (a pre-pane-id legacy record), so this is a
    // real, loadable state for a root. It is an identity-less locator like any other: `probe`
    // reports it not-recorded-process without touching tmux, and `stop` must agree -- the
    // shutdown is asked over the root's own socket, the kill is refused, and the tree closes --
    // instead of throwing above the gate and leaving the tree lingering for every sweep tick.
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      // No tmuxPaneId.
      socketPath: "/state/workers/architect.sock",
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const commands: string[][] = [];
    const shutdowns: string[] = [];
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        const client = fakeWorkerRpcClient();
        const shutdown = client.shutdown.bind(client);
        client.shutdown = () => {
          shutdowns.push(socketPath);
          shutdown();
        };
        return client;
      },
    });

    await processes.closeTree(root);

    expect(shutdowns).toEqual(["/state/workers/architect.sock"]);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
    ).toEqual([]);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "kill-window")
    ).toEqual([]);
    expect(state.trees[root].status).toBe("closed");
    expect(state.trees[root].locator).toBeUndefined();
    // The decision was logged once, at the probe, with what the record lacks.
    const logged = errorLog.mock.calls.map((call) => call.map(String).join(" "));
    expect(logged.filter((line) => line.includes("treating LEGION-42's root as dead"))).toEqual([
      "[legion] treating LEGION-42's root as dead: pane @42 has no recorded process identity (locator predates identity tracking)",
    ]);
    errorLog.mockRestore();
  });

  it("throws StopFailed and leaves the tree lingering with its locator intact when list-panes itself fails while closing, killing nothing", async () => {
    // B3: a failed listing proves nothing about the pane. The root's shim never confirms the
    // shutdown, so the stop reaches the kill gate; there tmux's `list-panes` fails for a reason
    // that is not "the pane is gone" (a client killed by the runner's timeout: nonzero exit,
    // empty stderr). The stop must throw, not return as if the pane were gone -- a possibly-live
    // root's locator is never cleared on a verdict nobody reached.
    const state = newLegionState("omp", 1);
    tree(state);
    const seededLocator = structuredClone(state.trees[root]?.locator);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const commands: string[][] = [];
    let listings = 0;
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") {
          listings += 1;
          // The first listing is `probeTree`'s: the pane is live and verifies. The second is the
          // kill gate's, after the shim ignored the shutdown -- and it fails.
          return listings === 1 ? livePanes(command) : { stdout: "", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => {
        const stuck = fakeWorkerRpcClient();
        stuck.shutdown = () => {};
        return stuck;
      },
    });

    await expect(processes.closeTree(root)).rejects.toThrow(StopFailed);

    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
    ).toEqual([]);
    expect(errorLog).toHaveBeenCalledWith(
      expect.stringContaining(`root process failed to stop while closing ${root}`),
      expect.objectContaining({ message: expect.stringContaining("list-panes -t %0 exited 1") })
    );
    // Never marked closed while the process could not be confirmed stopped: the tree is left
    // lingering, locator untouched, for the sweep to retry.
    expect(state.trees[root].status).toBe("lingering");
    expect(state.trees[root].locator).toEqual(seededLocator);
    errorLog.mockRestore();
  });

  it("probe rejects, clearing nothing and resurrecting nothing, when list-panes itself fails for a reason that proves nothing", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const seededLocator = structuredClone(state.trees[root]?.locator);
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") {
          return { stdout: "", stderr: "tmux: server not responding", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Exactly the composition index.ts's `onProbe` runs on a resync tick: a rejected probe
    // never reaches `resurrect`, and the resync loop logs and retries next interval.
    await expect(
      (async () => {
        if ((await processes.probe(root)) === "dead") await processes.resurrect(root);
      })()
    ).rejects.toThrow(
      "cannot verify pane %0: list-panes -t %0 exited 1: tmux: server not responding"
    );
    expect(state.trees[root]).toMatchObject({ status: "active", generation: 1 });
    expect(state.trees[root].locator).toEqual(seededLocator);
    expect(
      commands.filter(
        (command) =>
          command[0] === "tmux" && (command[3] === "new-window" || command[3] === "kill-pane")
      )
    ).toEqual([]);
  });

  it("tolerates killing a pane tmux already reaped without logging it as a failure", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", stderr: "can't find pane: %0", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => {
        const stuck = fakeWorkerRpcClient();
        stuck.shutdown = () => {};
        return stuck;
      },
    });

    await processes.closeTree(root);

    expect(errorLog).not.toHaveBeenCalled();
    errorLog.mockRestore();
  });

  it("is idempotent: a second concurrent closeTree call awaits the same in-flight close instead of stopping each locator twice", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const shutdownCalls: string[] = [];
    const { manager: processes } = manager(state, {
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        const client = fakeWorkerRpcClient();
        const shutdown = client.shutdown.bind(client);
        client.shutdown = () => {
          shutdownCalls.push(socketPath);
          shutdown();
        };
        return client;
      },
    });

    const [first, second] = await Promise.all([
      processes.closeTree(root),
      processes.closeTree(root),
    ]);

    expect(first).toBeUndefined();
    expect(second).toBeUndefined();
    expect(shutdownCalls).toEqual(["/state/workers/architect.sock"]);
    expect(state.trees[root].status).toBe("closed");
  });

  it("never joins an in-flight closeTree from a root's own self-report, letting the outer close complete once the root actually exits", async () => {
    // The self-report deadlock: closeTree's root leg sends `{type:"shutdown"}` and awaits the
    // root's own shim socket closing. The root's real `session_shutdown` hook awaits
    // `/process/exit` (== `reportRootExit`) before OMP finishes exiting -- so if
    // `reportRootExit` joined this same in-flight close, the close would wait on the root
    // exiting, and the root's own exit would wait on this call returning: deadlock. This test's
    // fake root client simulates exactly that self-report, firing synchronously from inside
    // `shutdown()`, before its own `closed` promise ever resolves.
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const commands: string[][] = [];
    let processes!: ProcessManager;
    let reportRootExitSettled = false;
    const rootClient = fakeWorkerRpcClient();
    rootClient.shutdown = () => {
      void processes.reportRootExit(root).then(() => {
        reportRootExitSettled = true;
        // Only now does OMP actually finish exiting session_shutdown -- the shim's socket closes.
        rootClient.close();
      });
    };
    ({ manager: processes } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => rootClient,
    }));

    const closing = processes.closeTree(root);

    for (let attempt = 0; attempt < 100 && !reportRootExitSettled; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(reportRootExitSettled).toBe(true);

    await closing;

    expect(state.trees[root].status).toBe("closed");
    expect(commands.filter((command) => command[3] === "kill-pane")).toEqual([]);
  });

  it("marks a tree lingering as its first durable act before any stop, so a crash mid-close leaves it retryable by the sweep instead of stuck active", async () => {
    const stateDir = await temporaryDir();
    const stateFile = path.join(stateDir, "state.json");
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    expect(state.trees[root].status).toBe("active");
    let saveCalls = 0;
    const { manager: processes } = manager(state, {
      saveState: async () => {
        saveCalls += 1;
        // Call 1 is closeTree's own upfront lingering mark: let it durably land on disk. Call 2
        // is the close's final success save recording "closed" -- simulate a crash losing that
        // write entirely (never reaches disk), not merely rejecting after writing.
        if (saveCalls === 2) throw new Error("simulated crash mid-close");
        await legionStateSaveState(stateFile, state);
      },
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => fakeWorkerRpcClient(),
    });

    await expect(processes.closeTree(root)).rejects.toThrow("simulated crash mid-close");
    expect(saveCalls).toBe(2);

    // What actually survives the "crash" is whatever the last successful write put on disk --
    // never the in-memory object, which a real process crash would discard entirely.
    const reloaded = await legionStateLoadState(stateFile, { project: "omp", cap: 1 });
    expect(reloaded.trees[root]?.status).toBe("lingering");
    expect(reloaded.trees[root]?.lingerUntil).toBeString();

    // The periodic sweep's retry, against the reloaded (post-crash) state: finishes cleanly.
    const { manager: retryProcesses } = manager(reloaded, {
      saveState: () => legionStateSaveState(stateFile, reloaded),
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return paneGone();
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    const retry = await retryProcesses.closeTree(root);
    expect(retry).toBeUndefined();
    expect(reloaded.trees[root]?.status).toBe("closed");
  });

  it("treats a socket error while waiting for a graceful close as unconfirmed, falling through to the kill instead of a false success", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", stderr: "lost server", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => {
        const closed = Promise.withResolvers<void>();
        return {
          closed: closed.promise,
          runState: "unknown" as const,
          negotiate: async () => {},
          adoptWorkingCopy: async () => {},
          prompt: async () => ({
            turnStarted: Promise.resolve(),
            hasStarted: true,
            abandonWait() {},
          }),
          getState: async () => ({}),
          // A socket reset while waiting, not a graceful close -- must never be mistaken for
          // proof the process exited.
          shutdown: () => {
            queueMicrotask(() => closed.reject(new Error("socket reset")));
          },
          close: () => {},
          onIdle: () => {},
        };
      },
    });

    await expect(processes.closeTree(root)).rejects.toThrow(StopFailed);

    expect(state.trees[root].status).toBe("lingering");
    expect(state.trees[root].locator).toBeDefined();
    errorLog.mockRestore();
  });

  it("treats a kill-pane that finds no server on the private socket as an already-gone pane", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      // The pane is still the recorded process, so the kill is attempted; its shim socket
      // refuses, so nothing is stopped gracefully first.
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return {
            stdout: "",
            stderr: "no server running on /tmp/tmux-1000/legion-omp",
            exitCode: 1,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.closeTree(root);

    expect(state.trees[root]?.status).toBe("closed");
    expect(state.trees[root]?.locator).toBeUndefined();
    errorLog.mockRestore();
  });

  it("does not launch a replacement when retiring a dead-socket claim's pane fails to stop, surfacing StopFailed instead", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ...paneIdentity(),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      // A genuinely dead socket (the shim itself is gone), not merely a busy one that answers
      // `get_state` late — a connected-but-slow client now queues instead of retiring (see
      // `spawnWorker`'s liveness dialect), so this test's "does not launch a replacement, sees
      // StopFailed instead" scenario needs the actual dead-socket path: a connect failure.
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", stderr: "lost server", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const error = await processes
      .spawnWorker(root, root, "tester", "verify again")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(StopFailed);
    // Never opened a replacement pane over a possibly-still-live one.
    expect(commands.filter((c) => c[3] === "new-window" || c[3] === "split-window")).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    // The failed-to-stop claim's own locator is untouched -- it is the only durable handle left
    // on a pane that might still be alive.
    expect(tmuxFields(claim.locator)?.tmuxPaneId).toBe("%7");
  });

  it("revokes the root architect's and every worker's session capability when closing a tree", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const architectToken = roleToken("omp", root, "architect");
    state.roles[architectToken] = {
      issue: root,
      role: "architect",
      sessionId: "ses_architect",
    };
    const implementerToken = roleToken("omp", child, "implementer");
    state.roles[implementerToken] = {
      issue: child,
      role: "implementer",
      sessionId: "ses_implementer",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const { manager: processes, revokedSessions } = manager(state);

    await processes.closeTree(root);

    expect(revokedSessions).toContain("ses_architect");
    expect(revokedSessions).toContain("ses_implementer");
  });

  it("removes the workspaces of a done root and its done child when the tree closes, keeps an unfinished child's, and logs each decision once", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "done",
      children: [child, grandchild],
    };
    state.issues[child] = {
      key: child,
      title: "Done child",
      status: "done",
      parent: root,
      children: [],
    };
    state.issues[grandchild] = {
      key: grandchild,
      title: "Unfinished child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    tree(state);
    state.trees[root].status = "lingering";
    const clone = path.join(stateDir, "repos", "github.com", "sjawhar", "legion");
    await mkdir(path.join(clone, ".jj"), { recursive: true });
    const dirs = Object.fromEntries(
      [root, child, grandchild].map((key) => [
        key,
        path.join(stateDir, "workspaces", "sjawhar", "legion", key.toLowerCase()),
      ])
    ) as Record<IssueKey, string>;
    for (const dir of Object.values(dirs)) await mkdir(dir, { recursive: true });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return paneGone();
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "list") {
          return { stdout: "default\nlegion-42\nlegion-43\nlegion-44\n", exitCode: 0 };
        }
        if (command[0] === "jj" && command[1] === "log") {
          return { stdout: "aaaa\nbbbb\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    let lines: string[];
    try {
      await processes.closeTree(root);
    } finally {
      lines = errors.mock.calls.map((call) => String(call[0]));
      errors.mockRestore();
    }

    expect(managedState.trees[root]?.status).toBe("closed");
    const forgotten = commands
      .filter((c) => c[0] === "jj" && c[1] === "workspace" && c[2] === "forget")
      .map((c) => c[3]);
    expect(forgotten).toEqual(["legion-42", "legion-43"]);
    expect(commands).toContainEqual([
      "jj",
      "abandon",
      "-r",
      "aaaa | bbbb",
      "--ignore-working-copy",
      "-R",
      clone,
    ]);
    // Every removal command runs after the last stop: the tree's processes are gone first.
    const lastTmux = commands.map((c) => c[0]).lastIndexOf("tmux");
    const firstJj = commands.findIndex((c) => c[0] === "jj");
    expect(firstJj).toBeGreaterThan(lastTmux);
    expect(existsSync(dirs[root])).toBeFalse();
    expect(existsSync(dirs[child])).toBeFalse();
    expect(existsSync(dirs[grandchild])).toBeTrue();
    expect(lines).toContain(
      `[legion] removed the workspace of ${root} (${dirs[root]}) at the close of tree ${root}: abandoned 2 commit(s) nothing else reached`
    );
    expect(lines).toContain(
      `[legion] removed the workspace of ${child} (${dirs[child]}) at the close of tree ${root}: abandoned 2 commit(s) nothing else reached`
    );
    expect(lines).toContain(
      `[legion] kept the workspace of ${grandchild} (${dirs[grandchild]}) at the close of tree ${root}: Dispatch status "in_progress"`
    );
    expect(lines.filter((line) => line.includes("workspace of")).length).toBe(3);
  });

  it("a runtime that retains its tree volume skips daemon-host workspace cleanup when the tree closes", async () => {
    // Kubernetes owns one PVC for every workspace in the tree; its retention belongs to
    // KubernetesRuntime.reconcileOrphans. ProcessManager must close the tree without calling a
    // host-side jj runner (which has neither this volume nor its workspaces). The sibling test
    // above proves the true branch removes a done root and child under TmuxRuntime.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "done", children: [] };
    tree(state);
    state.trees[root].status = "lingering";
    // Kubernetes has no host tmux locator; the tree is already process-free at close.
    delete state.trees[root].locator;
    const runtime = new FakeRuntime({ removesWorkspacesOnTreeClose: false });
    let hostCleanupCalled = false;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      runtime,
      run: async () => {
        hostCleanupCalled = true;
        throw new Error("Kubernetes tree close must not run a daemon-host workspace command");
      },
    });

    await processes.closeTree(root);

    expect(managedState.trees[root]?.status).toBe("closed");
    expect(hostCleanupCalled).toBe(false);
    expect(runtime.spawned).toEqual([]);
  });

  it("refuses to construct without a command runner under a runtime that removes workspaces at tree close, and constructs without one under a runtime that keeps them (LEGION-163)", () => {
    // The tmux entry point once dropped `run` (#1031) and the gap surfaced only at the first tree
    // close, hours later, as `runtime owns workspace cleanup but ProcessManager has no command
    // runner`. Construction is where a missing dependency is refused; `config.runtime.name` names
    // the runtime the operator selected.
    expect(() =>
      manager(
        newLegionState("omp", 1),
        { runtime: new FakeRuntime({ removesWorkspacesOnTreeClose: true }) },
        { omitRun: true }
      )
    ).toThrow(
      "ProcessManager needs a command runner: the tmux runtime removes workspaces at tree close"
    );
    // Kubernetes retains its tree volume and never runs a host jj command: it constructs as #1031
    // intended, with no runner at all.
    const { manager: kubernetesManager } = manager(
      newLegionState("omp", 1),
      { runtime: new FakeRuntime({ removesWorkspacesOnTreeClose: false }) },
      { omitRun: true }
    );
    expect(kubernetesManager).toBeInstanceOf(ProcessManager);
  });

  it("keeps a parked root's workspace at close — backlog or icebox — running no jj command and logging the status", async () => {
    for (const status of ["backlog", "icebox"] as const) {
      const stateDir = await temporaryDir();
      const state = newLegionState("omp", 1);
      state.issues[root] = { key: root, title: "Parked root", status, children: [] };
      tree(state);
      state.trees[root].status = "lingering";
      await mkdir(path.join(stateDir, "repos", "github.com", "sjawhar", "legion", ".jj"), {
        recursive: true,
      });
      const dir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
      await mkdir(dir, { recursive: true });
      const errors = vi.spyOn(console, "error").mockImplementation(() => {});
      const commands: string[][] = [];
      const { manager: processes, state: managedState } = manager(state, {
        config: config(stateDir),
        run: async (command) => {
          commands.push(command);
          if (command[0] === "tmux" && command[3] === "list-panes") return paneGone();
          return { stdout: "", exitCode: 0 };
        },
      });

      let lines: string[];
      try {
        await processes.closeTree(root);
      } finally {
        lines = errors.mock.calls.map((call) => String(call[0]));
        errors.mockRestore();
      }

      expect(managedState.trees[root]?.status, status).toBe("closed");
      expect(
        commands.filter((c) => c[0] === "jj" || c[0] === "git"),
        status
      ).toEqual([]);
      expect(existsSync(dir), status).toBeTrue();
      expect(lines, status).toContain(
        `[legion] kept the workspace of ${root} (${dir}) at the close of tree ${root}: Dispatch status "${status}"`
      );
    }
  });

  it("logs a failed workspace removal once, naming the issue, the directory, and the error, and still closes the tree", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "done", children: [] };
    tree(state);
    state.trees[root].status = "lingering";
    const clone = path.join(stateDir, "repos", "github.com", "sjawhar", "legion");
    await mkdir(path.join(clone, ".jj"), { recursive: true });
    const dir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(dir, { recursive: true });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return paneGone();
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "list") {
          return { stdout: "default\nlegion-42\n", exitCode: 0 };
        }
        if (command[0] === "jj" && command[1] === "log") return { stdout: "aaaa\n", exitCode: 0 };
        if (command[0] === "jj" && command[1] === "abandon") {
          return { stdout: "", stderr: "Error: Revision `aaaa` doesn't exist", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    let logged: Array<readonly unknown[]>;
    try {
      await processes.closeTree(root);
    } finally {
      logged = errors.mock.calls;
      errors.mockRestore();
    }

    expect(managedState.trees[root]?.status).toBe("closed");
    // The directory went first (the spec's order); the forget and prune never ran.
    expect(existsSync(dir)).toBeFalse();
    expect(
      commands.some((c) => c[0] === "jj" && c[1] === "workspace" && c[2] === "forget")
    ).toBeFalse();
    const failures = logged.filter((call) =>
      String(call[0]).startsWith(
        `[legion] failed to remove the workspace of ${root} (${dir}) at the close of tree ${root}:`
      )
    );
    expect(failures).toHaveLength(1);
    expect(String(failures[0]?.[1])).toContain("Revision `aaaa` doesn't exist");
  });

  it("runs no workspace removal at linger start; only the close removes", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "done", children: [] };
    tree(state);
    state.admission.active.push(root);
    await mkdir(path.join(stateDir, "repos", "github.com", "sjawhar", "legion", ".jj"), {
      recursive: true,
    });
    const dir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(dir, { recursive: true });
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.beginLinger(root);

    expect(managedState.trees[root]?.status).toBe("lingering");
    expect(commands.filter((c) => c[0] === "jj" || c[0] === "git")).toEqual([]);
    expect(existsSync(dir)).toBeTrue();
  });

  it("provisions a root admitted while its previous tree is still closing only after that close has finished, and that close removes nothing", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    // A human moved the finished root back to `todo`: the reducer's `admit` lands mid-close.
    state.issues[root] = { key: root, title: "Reopened root", status: "todo", children: [] };
    tree(state);
    state.trees[root].status = "lingering";
    const clone = path.join(stateDir, "repos", "github.com", "sjawhar", "legion");
    await mkdir(path.join(clone, ".jj"), { recursive: true });
    const dir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(dir, { recursive: true });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    // The root never confirms its graceful shutdown: the stop arms its timeout sleep (the event
    // the test awaits — the close is now blocked inside `awaitShutdown`) and the sleep resolves
    // only when the test says.
    const stopArmed = Promise.withResolvers<void>();
    const stopGate = Promise.withResolvers<void>();
    const stuckRootClient = fakeWorkerRpcClient();
    stuckRootClient.shutdown = () => {};
    // Every jj command's view of the tree record: provisioning's must all see `closed`, the mark
    // the close writes after its removal step (`closeTree`'s own promise also spans the promotion
    // sweep that follows the close, so it is not the instrument).
    const treeStatusAtJj: Array<string | undefined> = [];
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      sleep: () => {
        stopArmed.resolve();
        return stopGate.promise;
      },
      connectWorkerRpc: async () => stuckRootClient,
      run: async (command) => {
        commands.push(command);
        if (command[0] === "jj") treeStatusAtJj.push(state.trees[root]?.status);
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@43 %9 12345\n", exitCode: 0 };
        }
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "list") {
          return { stdout: "default\nlegion-42\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const closePromise = processes.closeTree(root);
    let lines: string[];
    try {
      await stopArmed.promise;

      expect(processes.admit(root)).toBe("spawned");
      // `admit`'s `startRoot` reaches `spawnRoot` → `awaitClosingTrees`, which logs the wait line
      // and parks on the in-flight close by microtask hops alone — no file write and no injected
      // `run` before it parks, so a tick budget is the whole event.
      // Negative wait: the launch parks on the close and nothing else is in flight.
      await flushEventLoop(50);
      // The reopened root's spawn is waiting on the close: no provisioning command yet.
      expect(commands.filter((c) => c[0] === "jj")).toEqual([]);
      expect(errors.mock.calls.map((call) => String(call[0]))).toContain(
        `[legion] launch of ${root} waits for the close of tree ${root} to finish`
      );

      stopGate.resolve();
      await closePromise;
      await processes.drainSpawns();
    } finally {
      lines = errors.mock.calls.map((call) => String(call[0]));
      errors.mockRestore();
    }

    // The close removed nothing: `admit` had re-activated the record.
    expect(
      commands.some((c) => c[0] === "jj" && c[1] === "workspace" && c[2] === "forget")
    ).toBeFalse();
    expect(commands.some((c) => c[0] === "jj" && c[1] === "abandon")).toBeFalse();
    expect(existsSync(dir)).toBeTrue();
    expect(lines).toContain(
      `[legion] kept every workspace of tree ${root} at its close: the tree record is "active", not lingering`
    );
    // Provisioning ran (`update-stale` on the surviving directory) and every jj command of it saw
    // the record already `closed`: the removal step, which precedes that mark, was over.
    expect(
      commands.some((c) => c[0] === "jj" && c[1] === "workspace" && c[2] === "update-stale")
    ).toBeTrue();
    expect(treeStatusAtJj.length).toBeGreaterThan(0);
    expect(treeStatusAtJj.every((status) => status === "closed")).toBeTrue();
  });

  it("settles a close whose freed slot promotes a queued child of the closing tree, and provisions that child without it ever waiting on the close", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    // A human moved the finished root back to `todo`: the reducer's `admit` lands mid-close.
    state.issues[root] = { key: root, title: "Reopened root", status: "todo", children: [child] };
    // The child was moved to `todo` while its parent lingered, so it was admitted as its own root
    // (`liveAncestorTree` sees no live ancestor); its first launch failed, and `spawnRoot`'s catch
    // left it `queued` in the admission queue with the cap's one slot free.
    state.issues[child] = {
      key: child,
      title: "Child re-admitted as its own root",
      status: "todo",
      parent: root,
      children: [],
    };
    tree(state);
    state.trees[root].status = "lingering";
    state.trees[child] = { root: child, generation: 1, status: "queued", launchFailures: 1 };
    state.admission.queue.push(child);
    await mkdir(path.join(stateDir, "repos", "github.com", "sjawhar", "legion", ".jj"), {
      recursive: true,
    });
    const rootDir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    const childDir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-43");
    await mkdir(rootDir, { recursive: true });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    // The root never confirms its graceful shutdown: the stop arms its timeout sleep (the event
    // the test awaits) and the sleep resolves only when the test says.
    const stopArmed = Promise.withResolvers<void>();
    const stopGate = Promise.withResolvers<void>();
    const stuckRootClient = fakeWorkerRpcClient();
    stuckRootClient.shutdown = () => {};
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: () => {
        stopArmed.resolve();
        return stopGate.promise;
      },
      connectWorkerRpc: async () => stuckRootClient,
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@43 %9 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%9 12345\n", exitCode: 0 };
        }
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "list") {
          return { stdout: "default\nlegion-42\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const closePromise = processes.closeTree(root);
    let lines: string[];
    try {
      await stopArmed.promise;
      expect(processes.admit(root)).toBe("spawned");
      stopGate.resolve();
      // Before the fix this never settled: the close's own slot release promoted the child from
      // inside the frame `closingTrees` still named, and the child's launch then waited on that
      // close. Bun's per-test timeout is the failure, with every assertion below unreached.
      await closePromise;
      await processes.drainSpawns();
    } finally {
      lines = errors.mock.calls.map((call) => String(call[0]));
      errors.mockRestore();
    }

    // The freed slot promoted the child, whose launch provisioned a fresh workspace and recorded
    // a running root — and it never had to wait on the close, because the sweep started only once
    // `closingTrees` had forgotten the tree. What the close then does to the reopened root itself
    // is LEGION-143's subject and is not pinned here.
    expect(
      commands.some(
        (c) => c[0] === "jj" && c[1] === "workspace" && c[2] === "add" && c[3] === childDir
      )
    ).toBeTrue();
    expect(managedState.trees[child]).toMatchObject({ status: "active" });
    expect(managedState.trees[child]?.locator).toBeDefined();
    expect(lines).toContain(
      `[legion] launch of ${root} waits for the close of tree ${root} to finish`
    );
    expect(lines).not.toContain(
      `[legion] launch of ${child} waits for the close of tree ${root} to finish`
    );
  });

  it("keeps every workspace when the sweep retries a close whose first attempt failed after the root was re-admitted: the record reads lingering again, but the tree holds an admission slot", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Reopened root", status: "todo", children: [] };
    tree(state);
    state.trees[root].status = "lingering";
    await mkdir(path.join(stateDir, "repos", "github.com", "sjawhar", "legion", ".jj"), {
      recursive: true,
    });
    const dir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(dir, { recursive: true });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    // The first close: the root never confirms its graceful shutdown (the stop arms its timeout
    // sleep — the event awaited — and the sleep resolves when the test says), then its kill fails,
    // so the close throws StopFailed and rewrites the record `lingering` over the mid-close
    // `admit`'s `active`. Every later sleep resolves at once and every later kill succeeds.
    const stopArmed = Promise.withResolvers<void>();
    const stopGate = Promise.withResolvers<void>();
    let firstSleep = true;
    let killFailed = false;
    const stuckRootClient = fakeWorkerRpcClient();
    stuckRootClient.shutdown = () => {};
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: () => {
        if (!firstSleep) return Promise.resolve();
        firstSleep = false;
        stopArmed.resolve();
        return stopGate.promise;
      },
      connectWorkerRpc: async () => stuckRootClient,
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "kill-pane" && !killFailed) {
          killFailed = true;
          return { stdout: "", stderr: "lost server", exitCode: 1 };
        }
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@43 %9 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%9 12345\n", exitCode: 0 };
        }
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "list") {
          return { stdout: "default\nlegion-42\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const firstClose = processes.closeTree(root);
    let lines: string[];
    try {
      await stopArmed.promise;
      expect(processes.admit(root)).toBe("spawned");
      stopGate.resolve();
      await expect(firstClose).rejects.toThrow(StopFailed);
      // The failed stop's own bookkeeping: `lingering` again, yet the slot `admit` took is held.
      expect(managedState.trees[root]?.status).toBe("lingering");
      expect(managedState.admission.active).toEqual([root]);
      // The re-admitted root's launch was waiting on that close and now runs to completion.
      await processes.drainSpawns();
      // The linger sweep's retry.
      await processes.closeTree(root);
    } finally {
      lines = errors.mock.calls.map((call) => String(call[0]));
      errors.mockRestore();
    }

    // Status alone would have read `lingering` and removed the re-admitted root's workspace.
    expect(
      commands.some((c) => c[0] === "jj" && c[1] === "workspace" && c[2] === "forget")
    ).toBeFalse();
    expect(commands.some((c) => c[0] === "jj" && c[1] === "abandon")).toBeFalse();
    expect(existsSync(dir)).toBeTrue();
    expect(lines).toContain(
      `[legion] kept every workspace of tree ${root} at its close: the tree holds an admission slot`
    );
  });

  it("requests control directives on the sanitized tree generation topic", async () => {
    const state = newLegionState("omp", 1);
    tree(state, root, 3);
    const { manager: processes, controlRequests, publications } = manager(state);
    const original = { topic: "notifications.role.legion-omp-1", payload: "{}", eventId: "evt-1" };
    const directive: ControlDirective = {
      type: "reclaim-architect",
      issue: root,
      redeliver: original,
    };

    await processes.controlDirective(root, directive, false);

    expect(controlRequests).toEqual([
      {
        subject: "legion.ctl.legion-42.3",
        json: JSON.stringify(directive),
      },
    ]);
    expect(publications).toEqual([]);
  });

  it("resumes a live worker directly (its shim socket answers) with a state-derived catch-up, never the raw missed event", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
      },
    };
    // The active phase's own worker lost its wake: exactly the shape a catch-up recovers.
    state.phases[child] = { phase: role, sessionId: "ses_implementer" };
    const client = fakeWorkerRpcClient();
    // A real client's own `getState()` call (in `spawnWorker`'s alive check, right before this)
    // would have already seeded `runState` from `isStreaming` - idle, since this worker has
    // nothing in flight to interrupt. Only an idle client is prompted directly (see
    // `WorkerAdmission.resumeOrQueueExisting`); a fake that never reports state stays at its
    // default "unknown" otherwise, which is no longer sufficient on its own.
    client.setRunStateSilently("idle");
    const original = exception(token).original;
    const { manager: processes, publications } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.handleException(exception(token, original));

    expect(client.prompts).toEqual([JSON.stringify({ type: "catchup-worker", unhandled: [] })]);
    expect(publications).toEqual([]);
    // A catch-up is recovery plumbing, never an assignment: the phase stays exactly as the
    // architect's last spawn_worker left it.
    expect(state.phases[child]).toEqual({ phase: role, sessionId: "ses_implementer" });
  });

  it("queues one catch-up and publishes no worker-queued when two role-lane exceptions name a busy live worker", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
      },
    };
    state.phases[child] = { phase: role, sessionId: "ses_implementer" };
    const client = fakeWorkerRpcClient();
    // Mid-turn: a prompt is never injected into a running client, so the catch-up queues.
    client.setRunStateSilently("running");
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, { connectWorkerRpc: async () => client });

    await processes.handleException(exception(token));
    await processes.handleException(exception(token));

    expect(client.prompts).toEqual([]);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("implementer claim disappeared");
    expect(claim.pendingAssignment?.kind).toBe("catchup");
    // LEGION-60's evidence item 3: eight worker-queued for one queued catch-up. The architect
    // did not ask for a catch-up and cannot act on it, so it hears nothing.
    expect(publications).toEqual([]);
  });

  it("receipt_timeout for a phase worker with a connected shim client publishes nothing, prompts nothing, and leaves pendingAssignment and the queue unchanged", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.emitRunState("idle");
    const { processes, state, managedState, commands, publications } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ...paneIdentity(),
      },
    };
    state.workerAdmission.queue.push(token);
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    await processes.reconnectWorkers();
    const before = structuredClone(state.roles[token]);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      await processes.handleException(exception(token, undefined, "receipt_timeout"));
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    expect(client.prompts).toEqual([]);
    expect(publications).toEqual([]);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(managedState.roles[token]).toEqual(before);
    expect(errorLines).toHaveLength(1);
    expect(errorLines[0]).toContain(token);
    expect(errorLines[0]).toContain("evt-1");
    expect(
      commands.some((command) => command[3] === "new-window" || command[3] === "split-window")
    ).toBeFalse();
  });

  it("receipt_timeout for a phase worker with no cached client but a live pane logs once and resumes nothing", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ...paneIdentity(),
      },
    };
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    let connectCalls = 0;
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, {
      run: liveRun,
      connectWorkerRpc: async () => {
        connectCalls += 1;
        return fakeWorkerRpcClient();
      },
    });
    const before = structuredClone(state.roles[token]);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      await processes.handleException(exception(token, undefined, "receipt_timeout"));
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    // The pane probe is a `list-panes` with no side effect; the socket is never dialed.
    expect(connectCalls).toBe(0);
    expect(publications).toEqual([]);
    expect(managedState.roles[token]).toEqual(before);
    expect(errorLines).toHaveLength(1);
    expect(errorLines[0]).toContain(token);
  });

  it("receipt_timeout for a phase worker whose socket and pane are gone follows the existing resume path", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "child-implementer.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
        ompSessionFile: sessionFile,
      },
    };
    state.phases[child] = { phase: role, sessionId: "ses_implementer" };
    const {
      manager: processes,
      publications,
      commands,
    } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
    });

    await processes.handleException(exception(token, undefined, "receipt_timeout"));

    // Identical to what a no_holder produces today: relaunched with --resume, catch-up queued.
    expect(publications).toEqual([]);
    expect(commands.some((command) => command.join(" ").includes("--resume"))).toBe(true);
    expect(state.roles[token]).toMatchObject({
      generation: 2,
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
      },
    });
  });

  it("resumes a dead worker with --resume through spawnWorker and never spawns a role with no locator", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "child-implementer.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
        ompSessionFile: sessionFile,
      },
    };
    state.phases[child] = { phase: role, sessionId: "ses_implementer" };
    const original = exception(token).original;
    const {
      manager: processes,
      publications,
      commands,
    } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
    });

    await processes.handleException(exception(token, original));

    // A respawned worker's catch-up is queued as its pendingAssignment (delivered over its
    // socket once it confirms boot via /worker/ready) rather than published over NATS: nothing
    // is published here, and never a replay of the raw missed event.
    expect(publications).toEqual([]);
    expect(commands.some((command) => command.join(" ").includes("--resume"))).toBe(true);
    expect(state.roles[token]).toMatchObject({
      generation: 2,
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
      },
    });

    // An exception on a role no claim has ever backed (never spawned) is a no-op: nothing to
    // resume, and never a fresh spawn from a missed-wake signal alone.
    const unbackedToken = roleToken("omp", child, "reviewer");
    publications.length = 0;
    commands.length = 0;
    await processes.handleException(exception(unbackedToken, exception(unbackedToken).original));
    expect(publications).toEqual([]);
    expect(commands).toEqual([]);
  });

  it("relaunches with --resume when a claim's locator was already cleared by markWorkerDeadLocked but its resumeSessionFile survives — the exact shape a confirmed-dead worker leaves behind for the next no-holder recovery", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      children: [],
      status: "in_progress",
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "child-implementer.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    // Exactly `markWorkerDeadLocked`'s own output shape (processes.ts:662-671): locator
    // deleted, resumeSessionFile carried forward from the dead locator's own ompSessionFile.
    // No prior fix (before this round) resumed this claim at all — `resumeWorker`'s own guard
    // required a locator, so a no-holder exception delivered after the worker was already
    // confirmed dead silently no-op'd forever, stranding the role.
    state.roles[token] = {
      issue: child,
      role,
      generation: 2,
      resumeSessionFile: sessionFile,
    };
    state.phases[child] = { phase: role, sessionId: "ses_implementer" };
    const original = exception(token).original;
    const {
      manager: processes,
      publications,
      commands,
    } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
    });

    await processes.handleException(exception(token, original));

    expect(publications).toEqual([]);
    expect(commands.some((command) => command.join(" ").includes("--resume"))).toBe(true);
    expect(state.roles[token]).toMatchObject({
      generation: 3,
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
      },
    });
    const relaunched = state.roles[token];
    if (!relaunched || !("issue" in relaunched) || !relaunched.locator) {
      throw new Error("expected a fresh locator after relaunch");
    }
    expect(relaunched.locator.ompSessionFile).toBe(sessionFile);
  });

  it("publishes worker-died to the tree architect once resume attempts exhaust the launch-failure threshold", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "child-implementer.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      launchFailures: 2,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
        ompSessionFile: sessionFile,
      },
    };
    state.phases[child] = { phase: role, sessionId: "ses_implementer" };
    const original = exception(token).original;
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      // The dead pane's PID-liveness poll never sees it die (readProcessCmdline reports an
      // always-alive OMP pane), so it always exhausts every retirement poll attempt; without
      // this override that is 20 real 100ms waits, flaky under load against bun's 5s per-test
      // timeout for a cost this test has no reason to pay.
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
      run: async (command) => {
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
          throw new Error("workspace provisioning is broken");
        }
        return liveRun(command);
      },
    });

    await processes.handleException(exception(token, original));

    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-died", issue: child, role }),
    });
  });

  it("skips the catch-up for a finished phase worker that is neither the active phase nor holding a pending assignment", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    // The implementer is the active phase; the tester finished earlier and was idle-retired
    // (locator cleared, resumeSessionFile kept). A wake misrouted to the tester's role must not
    // relaunch it: only the architect's next spawn_worker resumes a finished worker.
    state.phases[child] = { phase: "implementer", sessionId: "ses_implementer" };
    const testerToken = roleToken("omp", child, "tester");
    const retiredTester: WorkerRoleClaim = {
      issue: child,
      role: "tester",
      sessionId: "ses_tester",
      generation: 2,
      resumeSessionFile: "/state/sessions/child-tester.jsonl",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
    };
    state.roles[testerToken] = structuredClone(retiredTester);
    const {
      manager: processes,
      publications,
      commands,
    } = manager(state, {
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
    });

    await processes.handleException(exception(testerToken));

    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    expect(publications).toEqual([]);
    expect(state.roles[testerToken]).toEqual(retiredTester);
    expect(state.phases[child]).toEqual({ phase: "implementer", sessionId: "ses_implementer" });
  });

  it("keeps the mid-turn reviewer as the active phase through its own role-lane exceptions and a misrouted wake for the finished implementer, relaunching nothing (LEGION-27)", async () => {
    // The LEGION-27 sequence (2026-09-13 05:37 UTC, pull request #981 round 3): the reviewer is the
    // active phase and mid-turn — `legion handoff complete` runs inside the worker's own turn —
    // while the same issue's implementer has finished and been idle-retired (locator cleared,
    // resumeSessionFile kept). Two delivery exceptions arrive for the reviewer's role (a live but
    // busy holder), then a wake misrouted to the finished implementer's role finds no holder, so
    // `resumeWorker` runs for both roles. Before #991 the daemon relaunched the implementer and its
    // registration overwrote `phases[issue]`, so the reviewer's completion 409'd; now nothing may
    // move: no prompt reaches the busy reviewer, no pane opens, the retired claim stays retired,
    // and the record keeps the reviewer's session and its assignment time.
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    tree(state);
    const reviewerToken = roleToken("omp", root, "reviewer");
    const implementerToken = roleToken("omp", root, "implementer");
    state.roles[reviewerToken] = {
      issue: root,
      role: "reviewer",
      sessionId: "ses_reviewer",
      generation: 2,
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%5",
        socketPath: "/state/workers/reviewer.sock",
        ompSessionFile: "/state/sessions/reviewer.jsonl",
      },
    };
    const retiredImplementer: WorkerRoleClaim = {
      issue: root,
      role: "implementer",
      sessionId: "ses_implementer",
      generation: 1,
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      resumeSessionFile: "/state/sessions/implementer.jsonl",
    };
    state.roles[implementerToken] = structuredClone(retiredImplementer);
    const assigned = {
      phase: "reviewer",
      sessionId: "ses_reviewer",
      assignedAt: "2026-09-13T05:20:00.000Z",
    };
    state.phases[root] = structuredClone(assigned);
    const reviewerClient = fakeWorkerRpcClient();
    reviewerClient.setRunStateSilently("running");
    const stateDir = await temporaryDir();
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async (socketPath) => {
        if (socketPath === "/state/workers/reviewer.sock") return reviewerClient;
        throw new Error("dead shim socket");
      },
    });
    const infoSpy = vi.spyOn(console, "info").mockImplementation(() => {});

    try {
      await processes.handleException({
        ...exception(reviewerToken),
        reason: "delivery_failed",
      });
      await processes.handleException({
        ...exception(reviewerToken),
        reason: "delivery_failed",
      });
      await processes.handleException(exception(implementerToken));
    } finally {
      infoSpy.mockRestore();
    }

    // Nothing was injected into the reviewer's turn, no pane was opened for anyone, the retired
    // implementer is exactly as retired as before, and the phase record is byte-for-byte the
    // assignment that created it. (The reviewer's own catch-up may sit queued behind its busy
    // turn — that is incidental and not pinned here.)
    expect(reviewerClient.prompts).toEqual([]);
    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    expect(state.roles[implementerToken]).toEqual(retiredImplementer);
    expect(state.phases[root]).toEqual(assigned);
  });

  it("retires a relaunched worker at worker/ready when its only queued prompt is a bystander's catch-up, freeing its cap slot and promoting a spawn already queued behind it, while an assignment in the same position is delivered", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "tester-session.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const testerToken = roleToken("omp", root, "tester");
    const reviewerToken = roleToken("omp", root, "reviewer");
    const locator = (pane: string, sock: string, ompSessionFile?: string) => ({
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: pane,
      socketPath: `/state/workers/${sock}.sock`,
      ...(ompSessionFile ? { ompSessionFile } : {}),
    });
    // The tester finished earlier; a catch-up was queued on its relaunch (a delivery exception
    // arrived while the tester was still the active phase), and the architect moved the issue
    // on to the implementer before the relaunch reached ready. At delivery time the tester is a
    // bystander: the catch-up is dropped, never prompted -- and a relaunched pane that is never
    // prompted emits no agent_end, so left alive it would count against workerCap forever with
    // no path to idle-retire. It is retired right here instead.
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      generation: 2,
      launchFailures: 1,
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: locator("%2", "tester", sessionFile),
    };
    // The mirror: an architect assignment queued on a booting reviewer is delivered as today. It
    // is added only after the cap check below, so the tester's pane is the sole occupant of the
    // one slot while that check runs.
    const reviewerClaim: WorkerRoleClaim = {
      issue: root,
      role: "reviewer",
      sessionId: "ses_reviewer",
      generation: 1,
      pendingAssignment: {
        kind: "assignment",
        task: "review #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: locator("%3", "reviewer"),
    };
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    const testerClient = fakeWorkerRpcClient();
    const shutdownCalls: string[] = [];
    const shutdown = testerClient.shutdown.bind(testerClient);
    testerClient.shutdown = () => {
      shutdownCalls.push("tester");
      shutdown();
    };
    const reviewerClient = fakeWorkerRpcClient();
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const plannerToken = roleToken("omp", root, "planner");
    const plannerStarted = Promise.withResolvers<void>();
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async (socketPath) =>
        socketPath === "/state/workers/tester.sock" ? testerClient : reviewerClient,
      publishRole: (subject, json) => {
        if (
          subject === architectTopic &&
          json === JSON.stringify({ type: "worker-started", issue: root, role: "planner" })
        ) {
          plannerStarted.resolve();
        }
      },
    });

    // While the tester's relaunch is still booting, the architect spawns the planner: at
    // workerCap 1 the booting pane holds the one slot, so the planner queues.
    const queuedPlanner = await processes.spawnWorker(root, root, "planner", "plan #42");
    expect(queuedPlanner).toEqual({ status: "queued", roleToken: plannerToken });
    expect(managedState.workerAdmission.queue).toEqual([plannerToken]);
    commands.length = 0;

    await processes.workerReady(root, "tester", "ses_tester", 2);

    expect(testerClient.prompts).toEqual([]);
    const tester = managedState.roles[testerToken];
    if (!tester || !("issue" in tester)) throw new Error("tester claim disappeared");
    expect(tester.pendingAssignment).toBeUndefined();
    // Retired exactly as retireIdleWorker retires a finished worker: one graceful shutdown
    // frame, locator cleared, the session file kept for the next spawn_worker's --resume; the
    // boot itself succeeded, so ready is confirmed and launchFailures reset.
    expect(shutdownCalls).toEqual(["tester"]);
    expect(tester.locator).toBeUndefined();
    expect(tester.resumeSessionFile).toBe(sessionFile);
    expect(tester.readyConfirmedAt).toBeDefined();
    expect(tester.launchFailures).toBeUndefined();
    expect(managedState.phases[root]).toEqual({
      phase: "implementer",
      sessionId: "ses_implementer",
    });

    // The retire itself drains the queue -- no linger sweep (`reconcileWorkerAdmission`) is ever
    // called here. A retired pane has no cached client whose close could reach
    // `onWorkerClientClosed` -> `markWorkerDead` -> `promoteWorkerQueue()`, so the ready path
    // must trigger the drain explicitly, exactly as `markWorkerDead` does after its own
    // critical section; otherwise the queued planner waits for the next 60 s sweep. The
    // promotion's own `worker-started` publish is the awaited signal (the launch does real
    // workspace I/O through the fake runner, so tick-counting is not a bound); with the drain
    // missing nothing ever publishes it and the test times out instead of passing.
    await plannerStarted.promise;
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(
      commands.some((command) => command[3] === "new-window" || command[3] === "split-window")
    ).toBeTrue();
    const planner = managedState.roles[plannerToken];
    if (!planner || !("issue" in planner)) throw new Error("planner claim disappeared");
    expect(planner.locator).toBeDefined();
    expect(planner.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "plan #42",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });

    managedState.roles[reviewerToken] = reviewerClaim;
    await processes.workerReady(root, "reviewer", "ses_reviewer", 1);

    expect(reviewerClient.prompts).toEqual(["review #41"]);
    const reviewer = managedState.roles[reviewerToken];
    if (!reviewer || !("issue" in reviewer)) throw new Error("reviewer claim disappeared");
    expect(reviewer.pendingAssignment).toBeUndefined();
    expect(reviewer.locator).toBeDefined();
    expect(managedState.phases[root]).toEqual({
      phase: "reviewer",
      sessionId: "ses_reviewer",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("confirms the boot before the ready-time bystander retire, so a StopFailed leaves a confirmed live claim the next spawn_worker probes instead of queueing behind a boot forever", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "tester-session.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      generation: 2,
      launchFailures: 1,
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/tester.sock",
        ompSessionFile: sessionFile,
        // The pane still runs the recorded process (what the fake `list-panes` reports): only a
        // verified pane is ever killed, so the kill below is attempted and its failure surfaces.
        ...paneIdentity(),
      },
    };
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    // The stop fails for real: the shim is unreachable (so the graceful frame cannot be sent)
    // and `kill-pane` exits 1 with a stderr that is not one of the "pane already gone" shapes,
    // which the tmux runtime surfaces as StopFailed. The retire never clears a locator on a
    // StopFailed (the pane may still be alive), so the claim keeps its locator -- the question
    // this test asks is what shape the rest of the claim is left in.
    let connects = 0;
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 2 }),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        connects += 1;
        // First dial is the retire's own stop-time shutdown dial: unreachable, fall to kill-pane.
        if (connects === 1) throw new Error("ECONNREFUSED");
        return client;
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", stderr: "lost server", exitCode: 1 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const error = await processes
      .workerReady(root, "tester", "ses_tester", 2)
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(StopFailed);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    // Exactly retireIdleWorker's StopFailed shape: locator intact (never cleared on a failed
    // stop), but the boot is confirmed and the bystander catch-up is gone -- so this claim is a
    // live, confirmed worker to the rest of the daemon, not a boot still in flight.
    expect(tmuxFields(claim.locator)?.tmuxPaneId).toBe("%2");
    expect(claim.readyConfirmedAt).toBeDefined();
    expect(claim.launchFailures).toBeUndefined();
    expect(claim.pendingAssignment).toBeUndefined();
    expect(managedState.phases[root]).toEqual({
      phase: "implementer",
      sessionId: "ses_implementer",
    });

    // The architect's next spawn_worker for this role takes the live-claim probe path: the
    // task is prompted straight into the (still alive) pane. Unconfirmed, it would have taken
    // the booting branch instead -- queued as pendingAssignment for a /worker/ready that already
    // came and will never come again.
    const next = await processes.spawnWorker(root, root, "tester", "verify #42");

    expect(next).toEqual({ status: "resumed", roleToken: token });
    expect(client.prompts).toEqual(["verify #42"]);
    const prompted = managedState.roles[token];
    if (!prompted || !("issue" in prompted)) throw new Error("tester claim disappeared");
    expect(prompted.pendingAssignment).toBeUndefined();
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("delivers a sub-architect's queued catch-up at worker/ready whatever the child's active phase, without touching it", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const token = roleToken("omp", child, "architect");
    state.roles[token] = {
      issue: child,
      role: "architect",
      sessionId: "ses_sub_architect",
      generation: 2,
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-architect.sock",
      },
    };
    // A sub-architect is never its child's active phase once it has spawned a planner; its
    // catch-up is its only recovery path and is always delivered.
    state.phases[child] = { phase: "planner", sessionId: "ses_planner" };
    const client = fakeWorkerRpcClient();
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.workerReady(child, "architect", "ses_sub_architect", 2);

    expect(client.prompts).toEqual([JSON.stringify({ type: "catchup-worker", unhandled: [] })]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("sub-architect claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.readyConfirmedAt).toBeDefined();
    // A delivered catch-up still never writes the phase.
    expect(managedState.phases[child]).toEqual({ phase: "planner", sessionId: "ses_planner" });
  });

  it("drops a queued catch-up at promotion instead of prompting a live idle worker that is no longer the active phase, while a queued assignment is still prompted", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.emitRunState("idle");
    const { processes, state, managedState, commands } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
    });
    // Queued at the cap while the tester was still the active phase; the phase moved on to the
    // implementer before a slot freed.
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    await processes.reconnectWorkers();

    await processes.reconcileWorkerAdmission();

    expect(client.prompts).toEqual([]);
    expect(managedState.workerAdmission.queue).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    // The live pane is left alone: nothing launched, nothing retired.
    expect(claim.locator).toBeDefined();
    expect(
      commands.some((command) => command[3] === "new-window" || command[3] === "split-window")
    ).toBeFalse();
    expect(managedState.phases[root]).toEqual({
      phase: "implementer",
      sessionId: "ses_implementer",
    });

    // The mirror: the same queue position holding an architect assignment is prompted in place
    // and, being an assignment, makes the tester the active phase.
    claim.pendingAssignment = {
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: TEST_DELIVERY_ID,
    };
    managedState.workerAdmission.queue.push(token);
    await processes.reconcileWorkerAdmission();

    expect(client.prompts).toEqual(["verify #41"]);
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(claim.pendingAssignment).toBeUndefined();
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("drops a queued catch-up at promotion instead of relaunching a retired worker that is no longer the active phase, while a queued assignment still launches", async () => {
    const stateDir = await temporaryDir();
    const resumeFile = path.join(stateDir, "prior-tester-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const token = roleToken("omp", root, "tester");
    const state = newLegionState("omp", 1);
    tree(state);
    // Retired tester (locator cleared, session file kept) whose catch-up was queued at the cap
    // while it was still the active phase; the implementer took over before a slot freed.
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      resumeSessionFile: resumeFile,
      pendingAssignment: {
        kind: "catchup",
        task: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(token);
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        throw new Error("shim not listening yet");
      },
    });

    await processes.reconcileWorkerAdmission();

    expect(
      commands.some((command) => command[3] === "new-window" || command[3] === "split-window")
    ).toBeFalse();
    expect(managedState.workerAdmission.queue).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.locator).toBeUndefined();
    // The retired shape survives intact for the architect's next spawn_worker to --resume.
    expect(claim.resumeSessionFile).toBe(resumeFile);
    expect(managedState.phases[root]).toEqual({
      phase: "implementer",
      sessionId: "ses_implementer",
    });

    // The mirror: an architect assignment in the same queue position relaunches with --resume.
    claim.pendingAssignment = {
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: TEST_DELIVERY_ID,
    };
    managedState.workerAdmission.queue.push(token);
    await processes.reconcileWorkerAdmission();

    expect(commands.some((command) => command.join(" ").includes(`--resume=${resumeFile}`))).toBe(
      true
    );
    expect(managedState.workerAdmission.queue).toEqual([]);
    const launched = managedState.roles[token];
    if (!launched || !("issue" in launched)) throw new Error("tester claim disappeared");
    expect(launched.locator).toBeDefined();
    expect(launched.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("leaves a booting claim's architect assignment untouched when a catch-up arrives for the same role", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const testerToken = roleToken("omp", root, "tester");
    const assignment = {
      kind: "assignment" as const,
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: TEST_DELIVERY_ID,
    };
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      pendingAssignment: { ...assignment },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, commands } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.handleException(exception(testerToken));

    const claim = state.roles[testerToken];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toMatchObject(assignment);
    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    expect(client.prompts).toEqual([]);
  });

  it("drops a catch-up inside the role lock when the architect's assignment lands while the catch-up is being computed", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const testerToken = roleToken("omp", root, "tester");
    const claim: WorkerRoleClaim = {
      issue: root,
      role: "tester",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.roles[testerToken] = claim;
    // The tester is the active phase, so resumeWorker's pre-check passes and it goes on to
    // compute the catch-up -- a GitHub fetch it awaits outside the role's lock.
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const assignment = {
      kind: "assignment" as const,
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: TEST_DELIVERY_ID,
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, commands } = manager(state, {
      connectWorkerRpc: async () => client,
      workerCatchup: {
        repo: "sjawhar/legion",
        baseEnv: {},
        runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
        tokenManager: {
          getToken: async () => {
            // The architect's spawn_worker lands in the fetch window: the booting claim now
            // carries the assignment that /worker/ready must deliver.
            claim.pendingAssignment = { ...assignment };
            return {
              token: "worker-token",
              expiresAt: "2099-01-01T00:00:00.000Z",
              gitIdentity: HARNESS_GIT_IDENTITY,
            };
          },
        },
      },
    });

    await processes.handleException(exception(testerToken));

    expect(claim.pendingAssignment).toMatchObject(assignment);
    expect(client.prompts).toEqual([]);
    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
  });

  it("cancelBootWatchdog stops an armed watchdog from retiring and retrying a still-booting worker", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const {
      manager: processes,
      publications,
      state: managedState,
    } = manager(state, {
      config: config(stateDir, { workerBootTimeoutSeconds: 1 }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      connectWorkerRpc: async () => {
        throw new Error("shim not listening yet");
      },
    });

    const response = await processes.spawnWorker(root, child, role, "do the work");
    expect(response.status).toBe("spawned");
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim missing after spawn");
    processes.cancelBootWatchdog(token, claim.generation);

    // Negative wait: the abort only unwinds cancelableSleep; the connect loop exits on `cancelled`.
    await flushEventLoop();

    expect(publications).toEqual([]);
    const finalClaim = managedState.roles[token];
    expect(finalClaim && "issue" in finalClaim ? finalClaim.launchFailures : undefined).toBe(0);
  });
  it("retires a started worker whose ready delivery cannot connect, then delivers its original assignment after the resumed retry confirms ready", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const priorSession = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(priorSession, "{}", "utf8");
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const readyClient = fakeWorkerRpcClient();
    const {
      manager: processes,
      commands,
      state: managedState,
      runs,
    } = manager(state, {
      config: config(stateDir, {
        workerBootTimeoutSeconds: 1,
        // The first generation is dead, so it retires on its first deadline. The retry's socket
        // accepts connections while its /worker/started -> /worker/ready handshake runs, so it
        // must not be treated as a perpetually unregistered boot during this test.
        workerBootRegistrationDeadlineIntervals: 1_000,
      }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      connectWorkerRpc: async () => {
        const claim = managedState.roles[token];
        if (!claim || !("issue" in claim) || claim.generation === 1) {
          throw new Error("first-generation shim cannot connect");
        }
        return readyClient;
      },
      readProcessCmdline: async () => "bash\0",
    });

    await processes.spawnWorker(root, child, role, "implement #43");
    const started = managedState.roles[token];
    if (!started || !("issue" in started) || !started.locator) {
      throw new Error("first-generation claim missing");
    }
    // Models /worker/started: the session capability must exist before /worker/ready can be
    // attempted, but this deliberately does not mark the ready delivery as confirmed.
    started.sessionId = "ses_implementer";
    started.locator.ompSessionFile = priorSession;

    await expect(processes.workerReady(child, role, "ses_implementer", 1)).resolves.toBeUndefined();

    // The relaunch's own event: generation 1's pane was the 1st new-window; its retired locator
    // is gone and its window no longer verifies (`readProcessCmdline` says bash), so the retry
    // opens a 2nd. Generation, locator, and launchFailures are written in its microtask-only
    // continuation (the injected `readProcessStat` fake is awaited in between).
    await runs("new-window").completed.reached(2);

    const relaunched = managedState.roles[token];
    if (!relaunched || !("issue" in relaunched) || !relaunched.locator) {
      throw new Error("relaunched claim missing");
    }
    expect(relaunched.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "implement #43",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(relaunched.launchFailures).toBe(1);
    expect(commands.some((command) => command.join(" ").includes("--resume"))).toBeTrue();

    // Models the relaunched worker's /worker/started transition: the session capability is
    // minted, but launch-failure accounting is deliberately left untouched here -- only a
    // durably confirmed /worker/ready (the following workerReady call) resets it.
    relaunched.sessionId = "ses_implementer";
    await processes.workerReady(child, role, "ses_implementer", relaunched.generation ?? 0);

    expect(readyClient.prompts).toEqual(["implement #43"]);
    expect(relaunched.pendingAssignment).toBeUndefined();
    expect(relaunched.launchFailures).toBeUndefined();
  });

  it("retires and retries a worker whose boot the watchdog's timeout never sees /worker/started confirm, escalating to worker-died at the launch-failure threshold", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const {
      manager: processes,
      publications,
      commands,
      state: managedState,
      published,
    } = manager(state, {
      config: config(stateDir, { workerBootTimeoutSeconds: 1 }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      connectWorkerRpc: async () => {
        throw new Error("shim never listens");
      },
      // The boot watchdog now probes pane liveness before evicting a slow boot (see
      // `armBootWatchdog`/`probeWorkerAlive`): a dead shim alone is no longer sufficient to be
      // "dead" unless the pane is gone too, so this simulates a non-OMP (dead/replaced)
      // process at the recorded pid, matching what the socket refusal already implies.
      readProcessCmdline: async () => "bash\0",
    });

    await processes.spawnWorker(root, child, role, "do the work");

    // The watchdog's connect-retry and retire-polling loops drive entirely off the injected
    // fake clock/sleep, never a real timer, so three full workerBootTimeoutSeconds cycles (each
    // retrying with a fresh pane the fixture's default command runner always opens successfully)
    // drain deterministically without any real wait, despite simulating minutes of elapsed boot
    // time. Each below-threshold retry now goes through the normal admission queue (see
    // `WorkerAdmission.enqueueForRetry`), so it publishes its own `worker-started` exactly like
    // any other successful relaunch — flushing on the *first* publication would stop after that
    // intermediate one, so this waits specifically for the terminal `worker-died`.
    await published("worker-died").reached(1);

    // The watchdog probed the pane itself (its own row, by pane id) before retiring the boot.
    expect(
      commands.some(
        (command) =>
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
      )
    ).toBe(true);
    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-died", issue: child, role }),
    });
    const finalClaim = managedState.roles[token];
    expect(finalClaim && "issue" in finalClaim ? finalClaim.launchFailures : undefined).toBe(3);
    // Three retire cycles, two of them real relaunches through `launchWorker`'s
    // `provisionWorkspace` (real `mkdir` I/O -- see `onceEventLoop`'s doc comment above) and
    // `new-window`: the await above resolves only once that I/O has completed, which under a
    // CPU/IO-starved host can legitimately take longer than bun's default 5000ms per-test
    // budget even though every timer/clock the test itself controls is fake.
  }, 20_000);

  it("never evicts a slow-but-alive boot across repeated observation intervals; a later confirmation stops the watch", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    let connectAttempts = 0;
    const {
      manager: processes,
      publications,
      state: managedState,
      runs,
    } = manager(state, {
      config: config(stateDir, {
        workerBootTimeoutSeconds: 1,
        // High enough that 3 full observation cycles (this test's own scenario) never hits the
        // registration deadline -- this test is about the alive-vs-dead probe, not the deadline.
        workerBootRegistrationDeadlineIntervals: 1_000,
      }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      // The shim never answers (a real, slow OMP startup that has not yet opened its RPC
      // socket) — but the fixture's default `run`/`readProcessCmdline` report a genuinely
      // alive OMP pane throughout, so the watchdog's probe must never treat this as dead.
      connectWorkerRpc: async () => {
        connectAttempts += 1;
        throw new Error("shim not listening yet");
      },
    });

    // The watchdog is armed inside spawnWorker and its first poll sleep fires in spawnWorker's
    // own tail, so its interval start -- what `now()` read at arm time -- is the clock BEFORE the
    // spawn, not after it.
    const startTime = currentTime;
    await processes.spawnWorker(root, child, role, "do the work");

    // Three full workerBootTimeoutSeconds cycles (each interval's own connect-retry loop alone
    // needs ~10 fake-clock-driven sleeps at this timeout), driven entirely off the injected fake
    // clock/sleep plus the watchdog's own real macrotask yield on every re-arm (see
    // `armBootWatchdog`'s doc comment) - no real wait despite simulating several seconds of
    // elapsed boot time. Three liveness probes (one `list-panes` per observation interval) are
    // the event: the fake clock is +3 000 at the third.
    await runs("list-panes").completed.reached(3);

    expect(publications).toEqual([]);
    const stillBooting = managedState.roles[token];
    if (!stillBooting || !("issue" in stillBooting)) throw new Error("claim missing");
    // Never evicted, however many intervals passed: no retirement, no launch-failure count.
    expect(stillBooting.locator).toBeDefined();
    expect(stillBooting.launchFailures).toBe(0);
    expect(currentTime - startTime).toBeGreaterThanOrEqual(3_000);

    // Ready confirmation cancels the watch outright rather than waiting for its next wake.
    stillBooting.sessionId = "ses_implementer";
    stillBooting.readyConfirmedAt = currentTime;
    processes.cancelBootWatchdog(token, stillBooting.generation);
    const attemptsAtConfirmation = connectAttempts;
    // Negative wait: the watch sits in the re-arm's real 0 ms yield; `cancelled` then ends it.
    await flushEventLoop(400);

    expect(connectAttempts).toBe(attemptsAtConfirmation);
    expect(publications).toEqual([]);
  });

  it("re-arms (never retires) a boot whose pane is gone but whose shim socket still connects and negotiates, even when its get_state call rejects", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    let getStateCalls = 0;
    const client = {
      ...fakeWorkerRpcClient(),
      getState: async () => {
        getStateCalls += 1;
        throw new Error("get_state timed out");
      },
    };
    const {
      manager: processes,
      publications,
      state: managedState,
      runs,
    } = manager(state, {
      config: config(stateDir, {
        workerBootTimeoutSeconds: 1,
        // High enough that 3 full observation cycles (this test's own scenario) never hits the
        // registration deadline -- this test is about the alive-vs-dead probe, not the deadline.
        workerBootRegistrationDeadlineIntervals: 1_000,
      }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      // The recorded pane is gone throughout (a real dead-pane liveness check), but the shim
      // socket still accepts a connection and negotiates — the watchdog must treat this exactly
      // like a merely-busy shim, never as a dead boot, however many times `get_state` itself
      // then rejects.
      readProcessCmdline: async () => "bash\0",
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") {
          return paneGone();
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => client,
    });

    // As in the slow-boot test above: the interval start is the clock before the spawn.
    const startTime = currentTime;
    await processes.spawnWorker(root, child, role, "do the work");

    // Three liveness probes (one `list-panes` per observation interval) are the event: the fake
    // clock is +3 000 at the third, and each one's socket probe has already rejected get_state.
    await runs("list-panes").completed.reached(3);

    expect(publications).toEqual([]);
    expect(getStateCalls).toBeGreaterThan(0);
    const stillBooting = managedState.roles[token];
    if (!stillBooting || !("issue" in stillBooting)) throw new Error("claim missing");
    // Never retired, however many intervals passed and however many `get_state` calls
    // rejected: no locator clear, no launch-failure count.
    expect(stillBooting.locator).toBeDefined();
    expect(stillBooting.launchFailures).toBe(0);
    expect(currentTime - startTime).toBeGreaterThanOrEqual(3_000);
  });

  it("dispose() stops every armed boot watchdog's connect-retry loop for good, not merely until its next check", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const implementerToken = roleToken("omp", child, "implementer");
    const testerToken = roleToken("omp", child, "tester");
    const stateDir = await temporaryDir();
    const connects = eventCounter();
    const {
      manager: processes,
      publications,
      state: managedState,
    } = manager(state, {
      // The production default (120s): with `now` left at the fixture's fixed clock, the
      // watchdog's connect-retry loop can never reach its own deadline on its own — only
      // `dispose()`'s cancellation stops it, so a continuing rise in `connectAttempts` after
      // dispose() unambiguously means a live loop survived it. The registration deadline is
      // pinned out of reach: this test is about the loop stopping, and a retirement the loop
      // reached on its own before dispose() would count a launch failure that is not the point.
      config: config(stateDir, { workerBootRegistrationDeadlineIntervals: 1_000 }),
      connectWorkerRpc: async () => {
        connects.increment();
        throw new Error("shim not listening yet");
      },
      sleep: async () => {
        await onceEventLoop();
      },
    });

    await processes.spawnWorker(root, child, "implementer", "implement it");
    await processes.spawnWorker(root, child, "tester", "test it");
    // Let both watchdogs' connect-retry loops genuinely spin for a while first.
    await connects.reached(10);

    processes.dispose();
    const attemptsAtDispose = connects.count;

    // Negative wait: the abort only unwinds both cancelableSleeps; each loop exits on `cancelled`.
    await flushEventLoop();

    expect(connects.count).toBe(attemptsAtDispose);
    expect(publications).toEqual([]);
    const implementerClaim = managedState.roles[implementerToken];
    const testerClaim = managedState.roles[testerToken];
    expect(
      implementerClaim && "issue" in implementerClaim ? implementerClaim.launchFailures : undefined
    ).toBe(0);
    expect(testerClaim && "issue" in testerClaim ? testerClaim.launchFailures : undefined).toBe(0);
  });

  it("reports a reclaim-architect nack to the controller without a worker-side revival path", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes, publications } = manager(state, {
      run: liveRun,
      sleep: async () => {},
      natsRequest: async () =>
        JSON.stringify({ type: "nack", error: "architect transcript is missing" }),
    });

    await processes.handleException(exception(roleToken("omp", root, "architect")));

    expect(publications).toEqual([
      {
        subject: `notifications.role.${controllerToken("omp")}`,
        json: JSON.stringify({
          type: "revive-failed",
          issue: root,
          role: "architect",
        }),
      },
    ]);
  });

  it("spawns one controller window when the controller delivery fails", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    let controllerSpawned = false;
    const {
      manager: processes,
      commands,
      publications,
    } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-windows") {
          return {
            stdout: controllerSpawned ? "controller\n" : "",
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") {
          return controllerSpawned ? livePanes(command) : paneGone();
        }
        if (command[3] === "new-window") {
          controllerSpawned = true;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.handleException(
      exception(controllerToken("omp"), {
        topic: "notifications.github.sjawhar.legion.issue.42.comment",
        payload: "{}",
        eventId: "evt-controller",
      })
    );

    expect(commands.filter((command) => command[3] === "new-window")).toHaveLength(1);
    expect(state.controllerLocator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    });
    expect(publications).toEqual([]);
  });

  it("mints a fresh controller capability only for each controller window spawn", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const commands: string[][] = [];
    const launchedSecrets: string[] = [];
    let controllerLive = false;
    let mints = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      mintControllerCapability: async () => `controller-secret-${++mints}`,
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-windows") {
          return { stdout: controllerLive ? "controller\n" : "", exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          return controllerLive ? livePanes(command) : paneGone();
        }
        if (command[3] === "new-window") {
          controllerLive = true;
          const pointer = tmuxWindowEnvironment(command).LEGION_CONTROLLER_SECRET_FILE;
          if (pointer) launchedSecrets.push(await readFile(pointer, "utf8"));
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    controllerLive = false;
    await processes.ensureController();

    expect(mints).toBe(2);
    expect(launchedSecrets).toEqual(["controller-secret-1", "controller-secret-2"]);
  });

  it("ensureController's registration-deadline callback leaves an alive controller alone once its role claim arrives before the deadline elapses", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") return livePanes(command);
        if (command[3] === "new-window") return { stdout: "@43 %2 54321\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // Alive pane, no role claim yet: arms the registration deadline instead of treating this as
    // "there's nothing to do".
    await processes.ensureController();
    // The role claim arrives (what a real `/controller/ready` does) before the deadline elapses.
    managedState.roles[controllerToken("omp")] = {
      role: "controller",
      sessionId: "ses-controller",
    };
    // Only now does the deadline elapse -- its own callback must re-check the role rather than
    // trust whatever was true when it was armed.
    sleepGate.resolve();
    // Negative wait: the fire reaches only retireAndRespawnStuckController's role check.
    await flushEventLoop();

    expect(commands.some((command) => command[3] === "new-window")).toBe(false);
    expect(commands.some((command) => command[3] === "kill-pane")).toBe(false);
    expect(managedState.controllerLocator).toEqual(state.controllerLocator);
  });

  it("ensureController retires a stuck controller pane and spawns a fresh one once its registration deadline elapses with no role claim", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const staleLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    state.controllerLocator = { ...staleLocator };
    const sleepGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    const commands: string[][] = [];
    const controllerRelaunched = Promise.withResolvers<void>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        if (tmuxFields(state.controllerLocator)?.tmuxWindowId === "@44") {
          controllerRelaunched.resolve();
        }
      },
      // Only the first armed deadline (for the stale locator this test is about) is under this
      // test's control; the fresh controller `retireAndRespawnStuckController` spawns also arms
      // its own deadline (see `ensureController`'s doc comment), which must stay pending here --
      // otherwise, since `sleepGate` is already resolved by then, it would elapse immediately
      // too and retire the fresh pane this test is asserting survived.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await sleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") return livePanes(command);
        if (command[3] === "new-window") return { stdout: "@44 %3 65432\n", exitCode: 0 };
        if (command[0] === "tmux" && command[3] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // Alive pane, no role claim: arms the registration deadline.
    await processes.ensureController();
    // The role never gets claimed -- the deadline elapses with nothing having changed.
    sleepGate.resolve();
    // The respawn chain routes through real fs I/O (spawnController's own config write): the
    // save that records the fresh locator is the event, never a tick budget racing that I/O.
    await controllerRelaunched.promise;

    // The stuck pane was retired (no graceful shim response, so straight to kill-pane) and a
    // fresh one spawned in its place.
    const killPaneRan = commands.some(
      (command) => command[0] === "tmux" && command[3] === "kill-pane"
    );
    expect(killPaneRan).toBe(true);
    expect(commands.some((command) => command[3] === "new-window")).toBe(true);
    expect(managedState.controllerLocator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@44",
      tmuxPaneId: "%3",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(65432),
    });
  });

  // LEGION-89: the retired controller's window was the private session's only one, so its pane
  // closing tears session and server down -- and the graceful stop confirms before tmux has
  // finished doing so, so the respawn's `has-session` can still answer present while its
  // `new-window` finds the server gone. The respawn must recreate the session and open the window.
  it("respawns a stuck controller whose window was the session's last one even when the private server dies between the respawn's has-session and its new-window (LEGION-89)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const staleLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    state.controllerLocator = { ...staleLocator };
    const clock = manualSleep();
    const testConfig = config(stateDir);
    const commands: string[][] = [];
    const controllerRelaunched = Promise.withResolvers<void>();
    const noServer = "no server running on /tmp/tmux-1000/legion-omp";
    let hasSessionCalls = 0;
    let newWindowCalls = 0;
    let sessionRecreated = false;
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, state: managedState } = manager(state, {
      config: testConfig,
      saveState: async () => {
        if (tmuxFields(state.controllerLocator)?.tmuxWindowId === "@44") {
          controllerRelaunched.resolve();
        }
      },
      // As in the fixture above: only the stale locator's deadline elapses; the fresh
      // controller's own deadline must stay pending.
      sleep: async (ms) => {
        await clock.sleep(ms);
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "list-panes") return livePanes(command);
        if (command[3] === "kill-pane") return { stdout: "", exitCode: 0 };
        if (command[3] === "has-session") {
          hasSessionCalls += 1;
          // Present at the first check (the stale answer); gone afterwards, until recreated.
          if (hasSessionCalls === 1 || sessionRecreated) return { stdout: "", exitCode: 0 };
          return { stdout: "", stderr: noServer, exitCode: 1 };
        }
        if (command[3] === "new-session") {
          sessionRecreated = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          newWindowCalls += 1;
          if (newWindowCalls === 1) return { stdout: "", stderr: noServer, exitCode: 1 };
          return { stdout: "@44 %3 65432\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.ensureController();
      clock.fire(
        testConfig.workerBootTimeoutSeconds *
          1_000 *
          testConfig.workerBootRegistrationDeadlineIntervals
      );
      await controllerRelaunched.promise;

      expect(managedState.controllerLocator).toEqual({
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@44",
        tmuxPaneId: "%3",
        socketPath: path.join(stateDir, "workers", "controller.sock"),
        ...paneIdentity(65432),
      });
      expect(commands.filter((command) => command[3] === "new-window")).toHaveLength(2);
      expect(commands.filter((command) => command[3] === "new-session")).toHaveLength(1);
      const logged = errorLog.mock.calls.map((call) => String(call[0]));
      const recoveryLines = logged.filter(
        (line) =>
          line.includes(
            "tmux new-window for controller failed after has-session reported legion-omp present"
          ) && line.includes(noServer)
      );
      expect(recoveryLines).toHaveLength(1);
      expect(logged.some((line) => line.includes("failed to retire and respawn"))).toBe(false);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("a controller respawn whose new-window fails while the session still exists is logged with tmux's stderr (LEGION-89)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const staleLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    state.controllerLocator = { ...staleLocator };
    const clock = manualSleep();
    const testConfig = config(stateDir);
    const commands: string[][] = [];
    const respawnFailed = Promise.withResolvers<unknown>();
    const errorLog = vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      if (String(args[0]).includes("failed to retire and respawn a stuck controller")) {
        respawnFailed.resolve(args[1]);
      }
    });
    const { manager: processes, state: managedState } = manager(state, {
      config: testConfig,
      sleep: async (ms) => {
        await clock.sleep(ms);
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "list-panes") return livePanes(command);
        if (command[3] === "new-window") {
          return { stdout: "", stderr: "create window failed: fork failed", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.ensureController();
      clock.fire(
        testConfig.workerBootTimeoutSeconds *
          1_000 *
          testConfig.workerBootRegistrationDeadlineIntervals
      );
      const error = await respawnFailed.promise;

      expect(error).toBeInstanceOf(Error);
      expect((error as Error).message).toBe(
        "tmux new-window failed (exit 1): create window failed: fork failed"
      );
      expect(managedState.controllerLocator).toBeUndefined();
      expect(commands.filter((command) => command[3] === "new-window")).toHaveLength(1);
      expect(commands.filter((command) => command[3] === "new-session")).toHaveLength(0);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("dispose() cancels a pending controller-registration deadline so its stale expiry never retires or respawns", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const locator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    state.controllerLocator = { ...locator };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") return livePanes(command);
        if (command[3] === "new-window") return { stdout: "@45 %4 76543\n", exitCode: 0 };
        if (command[0] === "tmux" && command[3] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController(); // arms the deadline
    processes.dispose();
    // The underlying wait may still be pending (a test's injected sleep has no real timer to
    // cancel), but dispose() must have cleared the tracking `cancelControllerRegistrationDeadline`
    // relies on, so this stale fire is recognized as such and does nothing.
    sleepGate.resolve();
    // Negative wait: the stale fire reaches only the wait-identity check (dispose() cleared it).
    await flushEventLoop();

    expect(commands.some((command) => command[3] === "kill-pane")).toBe(false);
    expect(commands.some((command) => command[3] === "new-window")).toBe(false);
    expect(managedState.controllerLocator).toEqual(locator);
  });

  it("arms a fresh registration deadline for a controller spawned from scratch, so one that itself never registers also gets retried", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    let sleepCalls = 0;
    const firstSleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const windows = eventCounter();
    const launchedPids = new Map<string, number>();
    const controllerRelaunched = Promise.withResolvers<void>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        if (tmuxFields(state.controllerLocator)?.tmuxWindowId === "@52")
          controllerRelaunched.resolve();
      },
      // Only the very first armed deadline (for the from-scratch spawn this test is about) is
      // under this test's control; the *second* fresh spawn (once the first is retired) arms its
      // own deadline too, which must stay pending so this test's own assertions see a stable
      // result after exactly one retry cycle.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await firstSleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") {
          return livePanes(command, launchedPids.get(command[command.indexOf("-t") + 1]));
        }
        if (command[3] === "new-window") {
          windows.increment();
          launchedPids.set(`%${windows.count}`, Number(`8765${windows.count}`));
          return {
            stdout: `@5${windows.count} %${windows.count} 8765${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // No locator at all: spawns fresh outright, then must arm a deadline for THIS spawn too --
    // not merely for a controller that was already alive when observed.
    await processes.ensureController();
    const firstLocator = managedState.controllerLocator;
    expect(firstLocator).toBeDefined();
    expect(windows.count).toBe(1);

    // The freshly-spawned controller never registers either -- its own armed deadline elapses.
    firstSleepGate.resolve();
    await windows.reached(2);
    // The relaunched controller's locator is recorded and then persisted: that save is the event.
    await controllerRelaunched.promise;

    expect(commands.some((command) => command[0] === "tmux" && command[3] === "kill-pane")).toBe(
      true
    );
    expect(windows.count).toBe(2);
    expect(tmuxFields(managedState.controllerLocator)?.tmuxWindowId).not.toBe(
      tmuxFields(firstLocator)?.tmuxWindowId
    );
  });

  it("a stale registration-deadline expiry no-ops once a newer locator has replaced the one it observed", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const staleLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    state.controllerLocator = { ...staleLocator };
    const staleSleepGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    let panesAlive = true;
    const commands: string[][] = [];
    let windowCount = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      // Only the *first* armed wait (for `staleLocator`) is under this test's control; the fresh
      // replacement spawned below arms its own separate wait, which must stay pending here -- a
      // shared gate would resolve both simultaneously and make this test's own respawn
      // indistinguishable from the stale callback wrongly acting a second time.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await staleSleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") {
          return panesAlive ? livePanes(command) : paneGone();
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@6${windowCount} %${windowCount} 9876${windowCount}\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // Arms the stale wait for `staleLocator`.
    await processes.ensureController();
    // The stale pane dies on its own, independent of the registration deadline: the next
    // `ensureController` call observes this directly, cancels the stale wait, and spawns a
    // fresh replacement (arming its own, separately-tracked wait for it).
    panesAlive = false;
    await processes.ensureController();
    const freshLocator = managedState.controllerLocator;
    expect(freshLocator).toBeDefined();
    expect(tmuxFields(freshLocator)?.tmuxWindowId).not.toBe(staleLocator.tmuxWindowId);
    panesAlive = true;

    // The stale wait's own timer finally fires, late -- it must recognize itself as superseded
    // and touch neither the fresh locator nor spawn yet another replacement.
    staleSleepGate.resolve();
    // Negative wait: the stale fire reaches only the wait-identity check (fresh spawn replaced it).
    await flushEventLoop();

    expect(windowCount).toBe(1);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
    ).toHaveLength(0);
    expect(managedState.controllerLocator).toEqual(freshLocator);
  });

  it("a role claim landing during the post-deadline liveness re-check wins over the stale-timeout decision, leaving the pane untouched", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const locator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    state.controllerLocator = { ...locator };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let listPanesCalls = 0;
    const {
      manager: processes,
      state: managedState,
      runs,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") {
          listPanesCalls += 1;
          // The 1st call is `ensureController`'s own initial liveness check (before the deadline
          // is even armed); the 2nd is `retireAndRespawnStuckController`'s post-deadline
          // re-check. A real `/controller/ready` lands exactly during that 2nd call's own await.
          if (listPanesCalls === 2) {
            managedState.roles[controllerToken("omp")] = {
              role: "controller",
              sessionId: "ses-controller",
            };
          }
          return livePanes(command);
        }
        if (command[3] === "new-window") return { stdout: "@70 %9 111111\n", exitCode: 0 };
        if (command[0] === "tmux" && command[3] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    sleepGate.resolve();
    // The 2nd list-panes is the post-deadline re-check whose await the role claim lands in; the
    // re-check that follows it is its microtask-only continuation (the injected `readProcessStat`
    // and `readProcessCmdline` fakes are awaited in between).
    await runs("list-panes").completed.reached(2);

    expect(commands.some((command) => command[3] === "kill-pane")).toBe(false);
    expect(commands.some((command) => command[3] === "new-window")).toBe(false);
    expect(managedState.controllerLocator).toEqual(locator);
  });

  it("a confirmed /process/ready before the root registration deadline elapses cancels it, resets launchFailures, and takes no retire/resurrect action", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 2 };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(managedState.trees[root]?.generation).toBe(1);
    // `spawnRoot`'s own success path no longer resets `launchFailures` on a mere pane-open --
    // only a confirmed `/process/ready` does (`confirmRootReady`).
    expect(managedState.trees[root]?.launchFailures).toBe(2);

    processes.confirmRootReady(root, 1);
    expect(managedState.trees[root]?.launchFailures).toBe(0);

    sleepGate.resolve();
    // Negative wait: the fire reaches only stillUnconfirmed() (cancelled by confirmRootReady).
    await flushEventLoop();

    // The deadline was cancelled by the confirmation above: its stale fire takes no action.
    expect(windowCount).toBe(1);
    expect(commands.some((command) => command[0] === "tmux" && command[3] === "kill-pane")).toBe(
      false
    );
    expect(managedState.trees[root]).toMatchObject({ generation: 1, status: "active" });
  });

  it("resurrects directly, once, when the root registration deadline elapses on a dead pane", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    let sleepCalls = 0;
    const firstGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    const windows = eventCounter();
    let paneAlive = true;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      // Only the first armed deadline (this test's own, generation 1) is under this test's
      // control; the resurrect's own fresh spawn arms a second deadline (generation 2), which
      // must stay pending here so this test's own assertions see a stable result after exactly
      // one retry cycle.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await firstGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windows.increment();
          return {
            stdout: `@4${windows.count} %${windows.count} 1000${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") {
          return paneAlive ? livePanes(command) : paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windows.count).toBe(1);

    // The pane dies on its own before the deadline elapses.
    paneAlive = false;
    firstGate.resolve();
    await windows.reached(2);
    // The resurrection's whole spawn -- locator recorded, tree active, persisted -- is the event.
    await processes.drainSpawns();

    expect(windows.count).toBe(2);
    expect(managedState.trees[root]).toMatchObject({ generation: 2, status: "active" });
  });

  it("re-arms the same generation's registration deadline when the resurrection's own re-probe cannot complete, leaving the locator intact, and resurrects normally once it can", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    // Deadline 1 (generation 1's, armed by the spawn) and deadline 2 (the re-arm under test)
    // are each released by this test; the resurrected generation's own deadline stays pending.
    const gates = [Promise.withResolvers<void>(), Promise.withResolvers<void>()];
    const secondArm = Promise.withResolvers<void>();
    let sleepCalls = 0;
    // The `list-panes` answers, in order: the deadline's probe finds the pane gone; the
    // resurrection's own re-probe hits a tmux client that could not list anything (the runner's
    // timeout, a server not responding) -- a failure that proves nothing about the pane. Every
    // later listing finds the pane gone again.
    const listings: Array<{ stdout: string; stderr: string; exitCode: number }> = [
      paneGone(),
      { stdout: "", stderr: "tmux: server not responding", exitCode: 1 },
    ];
    const commands: string[][] = [];
    let sessionExists = false;
    const windows = eventCounter();
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 2) secondArm.resolve();
        const gate = gates[sleepCalls - 1];
        if (gate) {
          await gate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windows.increment();
          return {
            stdout: `@4${windows.count} %${windows.count} 1000${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") return listings.shift() ?? paneGone();
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.spawnRoot(root);
      expect(sleepCalls).toBe(1);
      const launched = managedState.trees[root]?.locator;
      expect(launched).toMatchObject({ tmuxPaneId: "%1", panePid: 10001 });

      // Deadline 1 elapses: probe dead, launch failure counted, resurrection started -- and its
      // re-probe throws. The root must not be left unwatched: the same generation's deadline is
      // armed again, and nothing about the tree was touched.
      gates[0]?.resolve();
      await secondArm.promise;
      expect(managedState.trees[root]).toMatchObject({
        generation: 1,
        status: "active",
        launchFailures: 1,
      });
      expect(managedState.trees[root]?.locator).toBe(launched);
      expect(managedState.trees[root]?.readyConfirmedAt).toBeUndefined();
      expect(windows.count).toBe(1);
      expect(commands.some((command) => command[3] === "kill-pane")).toBe(false);
      const logged = errors.mock.calls.filter(
        ([message]) => typeof message === "string" && message.includes(root)
      );
      expect(logged).toHaveLength(1);
      expect(String(logged[0]?.[1])).toContain("tmux: server not responding");

      // Deadline 2 elapses with the fault cleared: the ordinary retry -- probe dead, second
      // launch failure, resurrection onto a fresh pane whose own deadline is then armed.
      gates[1]?.resolve();
      await windows.reached(2);
      await processes.drainSpawns();
      expect(managedState.trees[root]).toMatchObject({
        generation: 2,
        status: "active",
        launchFailures: 2,
        locator: { tmuxPaneId: "%2", panePid: 10002 },
      });
      expect(sleepCalls).toBe(3);
      expect(commands.some((command) => command[3] === "kill-pane")).toBe(false);
    } finally {
      errors.mockRestore();
    }
  });

  it("retires an alive-but-unconfirmed root pane and resurrects once when its registration deadline elapses, counting the retirement toward launchFailures", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    let sleepCalls = 0;
    const firstGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    const windows = eventCounter();
    let paneAlive = true;
    const launchedPids = new Map<string, number>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await firstGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windows.increment();
          launchedPids.set(`%${windows.count}`, Number(`1000${windows.count}`));
          return {
            stdout: `@4${windows.count} %${windows.count} 1000${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") {
          return paneAlive
            ? livePanes(command, launchedPids.get(command[command.indexOf("-t") + 1]))
            : paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          // A real kill-pane actually kills the pane -- the resurrect that follows must see it
          // dead now, exactly as it would against a real tmux server.
          paneAlive = false;
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windows.count).toBe(1);

    // The pane never confirms via `/process/ready` and is still alive when the deadline elapses.
    firstGate.resolve();
    await windows.reached(2);
    // The resurrection's whole spawn -- locator recorded, tree active, persisted -- is the event.
    await processes.drainSpawns();

    expect(commands.some((command) => command[0] === "tmux" && command[3] === "kill-pane")).toBe(
      true
    );
    expect(windows.count).toBe(2);
    expect(managedState.trees[root]).toMatchObject({ generation: 2, status: "active" });
    // The retirement counts toward `launchFailures` itself -- `spawnRoot`'s own success path no
    // longer resets it on a mere pane-open, only a confirmed `/process/ready` does
    // (`confirmRootReady`), so repeated never-confirmed cycles still escalate to
    // `MAX_LAUNCH_FAILURES` instead of looping forever.
    expect(managedState.trees[root]?.launchFailures).toBe(1);
  });

  it("keeps an unconfirmed root's admission slot when its exit self-report lands during the deadline's retire, and still resurrects it (LEGION-83)", async () => {
    // Same self-report, other daemon-requested stop: the registration deadline finds the root
    // alive but never `/process/ready` and asks it to exit over its socket; the root registered,
    // so it holds an architect capability and POSTs `/process/exit` for the current generation
    // before its shim closes. Before the fix `markProcessDead` marked the tree dead and released
    // the slot; `retireUnconfirmedRoot`'s post-stop re-check then saw a tree that was no longer
    // `active` and declined to resurrect it -- stranded dead with no slot while the queued issue
    // took the slot.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    const queued: IssueKey = "LEGION-45";
    state.issues[queued] = {
      key: queued,
      title: "Queued behind the cap",
      status: "todo",
      children: [],
    };
    state.admission.active.push(root);
    state.admission.queue.push(queued);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.trees[queued] = { root: queued, generation: 0, status: "queued", launchFailures: 0 };
    let sleepCalls = 0;
    const firstGate = Promise.withResolvers<void>();
    let sessionExists = false;
    const windows = eventCounter();
    const launchedPids = new Map<string, number>();
    const gonePanes = new Set<string>();
    let processes!: ProcessManager;
    const rootClient = fakeWorkerRpcClient();
    rootClient.shutdown = () => {
      void processes.markProcessDead(root, state.trees[root]?.generation).then(() => {
        // The root exits: its pane is gone from here on, exactly as a real tmux would report.
        gonePanes.add("%1");
        rootClient.close();
      });
    };
    ({ manager: processes } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await firstGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => rootClient,
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windows.increment();
          launchedPids.set(`%${windows.count}`, Number(`1000${windows.count}`));
          return {
            stdout: `@4${windows.count} %${windows.count} 1000${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") {
          const target = command[command.indexOf("-t") + 1] ?? "";
          return gonePanes.has(target) ? paneGone() : livePanes(command, launchedPids.get(target));
        }
        return { stdout: "", exitCode: 0 };
      },
    }));

    await processes.spawnRoot(root);
    expect(windows.count).toBe(1);

    // The pane never confirms via `/process/ready`; the deadline elapses with it alive.
    firstGate.resolve();
    await windows.reached(2);
    await processes.drainSpawns();

    expect(state.trees[root]).toMatchObject({ generation: 2, status: "active", launchFailures: 1 });
    expect(state.admission.active).toEqual([root]);
    expect(state.admission.queue).toEqual([queued]);
    expect(state.trees[queued]).toMatchObject({ status: "queued" });
  });

  it("a stale root-registration-deadline expiry no-ops once a newer generation has superseded it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    let sleepCalls = 0;
    const staleGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    let paneAlive = true;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      // Only the first armed wait (this test's stale one, generation 1) is under this test's
      // control; the independent resurrect below arms its own separate wait (generation 2),
      // which must stay pending here -- a shared gate would resolve both, making the stale
      // callback's own no-op indistinguishable from a legitimate one.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await staleGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          return paneAlive ? livePanes(command) : paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Arms the stale wait for generation 1.
    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    // The pane dies on its own, independent of the registration deadline: an unrelated
    // exception-driven resurrect observes this directly, bumping to generation 2 and arming a
    // fresh, separately-tracked wait for it.
    paneAlive = false;
    await processes.resurrect(root);
    expect(windowCount).toBe(2);
    expect(managedState.trees[root]?.generation).toBe(2);
    const commandsBeforeStaleFire = commands.length;

    // The stale wait's own timer finally fires, late -- it must recognize itself as superseded
    // (generation 1 no longer matches the currently-armed generation 2) and touch nothing.
    paneAlive = true;
    staleGate.resolve();
    // Negative wait: the stale fire reaches only stillUnconfirmed() (generation 1 != armed 2).
    await flushEventLoop();

    expect(commands.length).toBe(commandsBeforeStaleFire);
    expect(managedState.trees[root]).toMatchObject({ generation: 2, status: "active" });
  });

  it("closeTree cancels a pending root-registration deadline so its stale expiry never retires or resurrects", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    await processes.closeTree(root);
    expect(managedState.trees[root]?.status).toBe("closed");
    const commandsAfterClose = commands.length;

    // The registration deadline armed by the spawn above must have been cancelled by
    // `closeTree`: its stale fire takes no action on the now-closed tree.
    sleepGate.resolve();
    // Negative wait: the fire reaches only stillUnconfirmed() (entry cancelled by closeTree).
    await flushEventLoop();

    expect(commands.length).toBe(commandsAfterClose);
    expect(managedState.trees[root]?.status).toBe("closed");
  });

  it("a /process/ready landing during the post-deadline liveness probe wins over the stale-timeout decision, leaving the pane untouched", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    const probeGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    let listPanesCalls = 0;
    const {
      manager: processes,
      state: managedState,
      runs,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          listPanesCalls += 1;
          // Blocks the deadline's own post-expiry probe exactly once, so a `/process/ready`
          // landing in the middle of it can be observed by the re-check that follows.
          if (listPanesCalls === 1) await probeGate.promise;
          return livePanes(command);
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    sleepGate.resolve();
    // Issuance, not completion: the fake blocks this probe on `probeGate` until released below.
    await runs("list-panes").issued.reached(1);

    // `/process/ready` confirms this exact generation while the deadline's own probe is still
    // pending.
    processes.confirmRootReady(root, 1);
    expect(managedState.trees[root]?.readyConfirmedAt).toBeDefined();

    probeGate.resolve();
    // The released probe completes; the stillUnconfirmed() re-check that declines is its
    // microtask-only continuation (the injected identity fakes are awaited in between).
    await runs("list-panes").completed.reached(1);

    expect(commands.some((command) => command[0] === "tmux" && command[3] === "kill-pane")).toBe(
      false
    );
    expect(windowCount).toBe(1);
    expect(managedState.trees[root]).toMatchObject({ generation: 1, status: "active" });
    expect(managedState.trees[root]?.launchFailures).toBe(0);
  });

  it("dispose() during the post-deadline liveness probe prevents any retire or resurrect once the probe resolves", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    const probeGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    let listPanesCalls = 0;
    const {
      manager: processes,
      state: managedState,
      runs,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          listPanesCalls += 1;
          if (listPanesCalls === 1) await probeGate.promise;
          return paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    sleepGate.resolve();
    // Issuance, not completion: the fake blocks this probe on `probeGate` until released below.
    await runs("list-panes").issued.reached(1);

    // Daemon shutdown begins while the deadline's own probe is still in flight -- unlike
    // cancelling the wait map entry (already gone by construction here: the probe already ran),
    // only the disposed flag can stop this in-flight expiry from acting once its probe resolves.
    processes.dispose();
    const commandsBeforeProbeResolves = commands.length;

    probeGate.resolve();
    // The released probe completes; the disposed re-check that declines is its microtask-only
    // continuation (the injected identity fakes are awaited in between).
    await runs("list-panes").completed.reached(1);

    expect(commands.length).toBe(commandsBeforeProbeResolves);
    expect(windowCount).toBe(1);
    expect(managedState.trees[root]).toMatchObject({ generation: 1, status: "active" });
  });

  it("repeated dead-before-ready cycles escalate to launch-failed instead of resurrecting forever", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const gates = [
      Promise.withResolvers<void>(),
      Promise.withResolvers<void>(),
      Promise.withResolvers<void>(),
    ];
    let sleepCalls = 0;
    const commands: string[][] = [];
    let sessionExists = false;
    const windows = eventCounter();
    let paneAlive = true;
    const {
      manager: processes,
      state: managedState,
      publications,
      published,
    } = manager(state, {
      config: config(stateDir),
      // Only the first three armed deadlines (one per resurrect cycle) are under this test's
      // control; a fourth would only ever be armed by a resurrect this test does not expect.
      sleep: async () => {
        const index = sleepCalls;
        sleepCalls += 1;
        const gate = gates[index];
        if (gate) {
          await gate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windows.increment();
          return {
            stdout: `@4${windows.count} %${windows.count} 1000${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") {
          return paneAlive ? livePanes(command) : paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windows.count).toBe(1);

    // The pane never confirms and dies before every one of the first two deadlines: each one
    // resurrects (a fresh generation, its own fresh deadline) and counts a failure.
    paneAlive = false;
    gates[0].resolve();
    await windows.reached(2);
    // The resurrection's whole spawn -- locator recorded, tree active, persisted -- is the event.
    await processes.drainSpawns();
    expect(managedState.trees[root]).toMatchObject({ generation: 2, status: "active" });
    expect(managedState.trees[root]?.launchFailures).toBe(1);

    gates[1].resolve();
    await windows.reached(3);
    // The resurrection's whole spawn -- locator recorded, tree active, persisted -- is the event.
    await processes.drainSpawns();
    expect(managedState.trees[root]).toMatchObject({ generation: 3, status: "active" });
    expect(managedState.trees[root]?.launchFailures).toBe(2);

    // The third dead-before-ready cycle reaches MAX_LAUNCH_FAILURES: it escalates instead of
    // resurrecting a fourth time.
    gates[2].resolve();
    // The escalation's own event: status, locator, revoke, and admission slot are all written
    // synchronously around this publish (escalateOrRetryUnconfirmedRoot, releaseSlot's prefix).
    await published("launch-failed").reached(1);

    expect(windows.count).toBe(3);
    expect(managedState.trees[root]?.launchFailures).toBe(3);
    expect(managedState.trees[root]?.locator).toBeUndefined();
    expect(managedState.admission.active).toEqual([]);
    expect(publications.some((publication) => publication.json.includes('"launch-failed"'))).toBe(
      true
    );
    // Three resurrect cycles, each opening a real pane through `spawnTree`'s
    // `provisionWorkspace` (real `mkdir` I/O -- see `onceEventLoop`'s doc comment above): the
    // awaits above resolve only once that I/O has completed, which under a CPU/IO-starved host
    // can legitimately take longer than bun's default 5000ms per-test budget even though every
    // timer/clock the test itself controls above is fake.
  }, 20_000);

  it("dispose() landing during the pre-resurrect persist prevents the retry from resurrecting a root after shutdown", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    const saveGate = Promise.withResolvers<void>();
    let saveStateCalls = 0;
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    let paneAlive = true;
    const {
      manager: processes,
      state: managedState,
      saves,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      saveState: async () => {
        saveStateCalls += 1;
        // The 1st save is spawnRoot's own happy-path save for the initial spawn; the 2nd is
        // escalateOrRetryUnconfirmedRoot's own pre-resurrect persist -- the one this test gates.
        if (saveStateCalls === 2) await saveGate.promise;
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          return paneAlive ? livePanes(command) : paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    paneAlive = false;
    sleepGate.resolve();
    // Issuance, not completion: the fake blocks this 2nd save (the retry's pre-resurrect persist)
    // on `saveGate` until released below.
    await saves.issued.reached(2);

    // Daemon shutdown begins while the retry's own pre-resurrect persist is still in flight.
    processes.dispose();
    // The released save is the next completion; the treeStillUnconfirmed() re-check that
    // declines (disposed) is its microtask-only continuation. `drainSpawns()` then awaits any
    // resurrection a regressed check would have started -- its real fs I/O precedes `new-window`,
    // so `windowCount` alone cannot see it one macrotask later; free when nothing was started.
    const released = saves.completed.next();
    saveGate.resolve();
    await released;
    await processes.drainSpawns();

    expect(windowCount).toBe(1);
    expect(managedState.trees[root]).toMatchObject({ generation: 1, status: "active" });
    // The failure was still counted and persisted; only the resurrect that would have followed
    // it was suppressed.
    expect(managedState.trees[root]?.launchFailures).toBe(1);
  });

  it("a /process/ready confirmation landing during the pre-resurrect persist prevents the retry from resurrecting an already-confirmed root", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    const saveGate = Promise.withResolvers<void>();
    let saveStateCalls = 0;
    let sessionExists = false;
    let windowCount = 0;
    let paneAlive = true;
    const {
      manager: processes,
      state: managedState,
      saves,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      saveState: async () => {
        saveStateCalls += 1;
        // The 1st save is spawnRoot's own happy-path save for the initial spawn; the 2nd is
        // escalateOrRetryUnconfirmedRoot's own pre-resurrect persist -- the one this test gates.
        if (saveStateCalls === 2) await saveGate.promise;
      },
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          return paneAlive ? livePanes(command) : paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    paneAlive = false;
    sleepGate.resolve();
    // Issuance, not completion: the fake blocks this 2nd save (the retry's pre-resurrect persist)
    // on `saveGate` until released below.
    await saves.issued.reached(2);

    // The real root process answers /process/ready for this exact generation while the retry's
    // own pre-resurrect persist is still in flight -- the probe that found it "dead" a moment
    // earlier was wrong (or already stale), and this confirmation must still win over the
    // decision already in flight.
    processes.confirmRootReady(root, 1);
    // The released save is the next completion; the treeStillUnconfirmed() re-check that
    // declines (readyConfirmedAt set) is its microtask-only continuation. `drainSpawns()` then
    // awaits any resurrection a regressed check would have started (see the dispose test above).
    const released = saves.completed.next();
    saveGate.resolve();
    await released;
    await processes.drainSpawns();

    expect(windowCount).toBe(1);
    expect(managedState.trees[root]).toMatchObject({ generation: 1, status: "active" });
    expect(managedState.trees[root]?.readyConfirmedAt).toBeDefined();
    // confirmRootReady resets launchFailures on a durable confirmation, exactly as it does for
    // the ordinary happy path -- the failed probe that preceded it is forgiven, not stranded.
    expect(managedState.trees[root]?.launchFailures).toBe(0);
  });

  it("closeTree landing during the pre-resurrect persist prevents the retry from resurrecting into a closed tree", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    const saveGate = Promise.withResolvers<void>();
    let saveStateCalls = 0;
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    let paneAlive = true;
    const {
      manager: processes,
      state: managedState,
      saves,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      saveState: async () => {
        saveStateCalls += 1;
        if (saveStateCalls === 2) await saveGate.promise;
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          return paneAlive ? livePanes(command) : paneGone();
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    paneAlive = false;
    sleepGate.resolve();
    // Issuance, not completion: the fake blocks this 2nd save (the retry's pre-resurrect persist)
    // on `saveGate` until released below.
    await saves.issued.reached(2);

    // closeTree runs to completion on its own (its own saves are never gated) while the retry's
    // pre-resurrect persist is still pending.
    await processes.closeTree(root);
    expect(managedState.trees[root]?.status).toBe("closed");

    // `closeTree` persisted three more times on its own, so the gated save is the NEXT completion
    // (the 5th: spawnRoot's, closeTree's three, then this one) -- `next()`, never a fixed count.
    // The treeStillUnconfirmed() re-check that declines (status closed) is its microtask-only
    // continuation; `drainSpawns()` then awaits any resurrection a regressed check would have
    // started (see the dispose test above).
    const released = saves.completed.next();
    saveGate.resolve();
    await released;
    await processes.drainSpawns();

    expect(windowCount).toBe(1);
    expect(managedState.trees[root]?.status).toBe("closed");
  });

  it("promotes a queued tree waiting behind a slot the terminal launch-failed escalation frees", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.issues[child] = { key: child, title: "Child", status: "todo", children: [] };
    state.admission = { cap: 1, active: [root], queue: [child] };
    // A fake, already-running root (no real spawn in this test -- only the queued child's own
    // promotion needs a real spawn attempt) with `launchFailures` one below the threshold: the
    // single dead-before-ready cycle this test drives crosses it.
    state.trees[root] = {
      root,
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@41",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/architect.sock",
      },
      status: "active",
      launchFailures: 2,
    };
    state.trees[child] = { root: child, generation: 0, status: "queued", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    const windows = eventCounter();
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await sleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") {
          windows.increment();
          return {
            stdout: `@5${windows.count} %${windows.count} 2000${windows.count}\n`,
            exitCode: 0,
          };
        }
        // root's own recorded pane always reads dead -- the only real spawn this test drives is
        // the queued child's promotion.
        if (command[3] === "list-panes") return paneGone();
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Arms root's deadline exactly as a boot-time `reconnectRoots` would for an active,
    // never-confirmed tree -- see that method's own doc comment.
    processes.reconnectRoots();
    expect(sleepCalls).toBe(1);

    // The deadline elapses against a dead pane, reaching MAX_LAUNCH_FAILURES: escalates instead
    // of resurrecting, and the queued child must be promoted into the slot this frees.
    sleepGate.resolve();
    // The freed slot's promotion is a real `startRoot` spawn: the child's launch is the event,
    // and `drainSpawns` awaits that spawn to completion (locator recorded, persisted).
    await windows.reached(1);
    await processes.drainSpawns();

    expect(managedState.trees[root]).toMatchObject({
      status: "launch-failed",
      launchFailures: 3,
    });
    expect(managedState.admission.active).toEqual([child]);
    expect(managedState.admission.queue).toEqual([]);
    expect(managedState.trees[child]?.locator).toBeDefined();
    expect(publications.some((publication) => publication.json.includes('"launch-failed"'))).toBe(
      true
    );
    // The freed slot's promotion drives a real spawn for the queued child through `spawnTree`'s
    // `provisionWorkspace` (real `mkdir` I/O -- see `onceEventLoop`'s doc comment above): the
    // awaits above resolve only once that I/O has completed, which under a CPU/IO-starved host
    // can legitimately take longer than bun's default 5000ms per-test budget even though every
    // timer/clock the test itself controls is fake.
  }, 20_000);

  it("re-arms the same generation's deadline when the alive-but-unconfirmed pane's stop fails, instead of stranding it with no retry", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    let sleepCalls = 0;
    const firstGate = Promise.withResolvers<void>();
    const secondGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    const windows = eventCounter();
    let killPaneShouldFail = true;
    let killPaneSucceeded = false;
    const launchedPids = new Map<string, number>();
    const cfg = config(stateDir);
    const {
      manager: processes,
      state: managedState,
      sleeps,
    } = manager(state, {
      config: cfg,
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await firstGate.promise;
          return;
        }
        if (sleepCalls === 2) {
          await secondGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windows.increment();
          launchedPids.set(`%${windows.count}`, Number(`1000${windows.count}`));
          return {
            stdout: `@4${windows.count} %${windows.count} 1000${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") {
          return killPaneSucceeded
            ? paneGone()
            : livePanes(command, launchedPids.get(command[command.indexOf("-t") + 1]));
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          if (killPaneShouldFail) {
            return { stdout: "", stderr: "tmux: unable to kill pane", exitCode: 1 };
          }
          killPaneSucceeded = true;
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windows.count).toBe(1);

    // The first deadline elapses against an alive-but-unconfirmed pane whose kill-pane fails --
    // it re-arms the same generation's deadline instead of stranding it with nothing left to
    // retry it.
    firstGate.resolve();
    // The re-armed deadline's own sleep is the event: armRootRegistrationDeadline arms it
    // synchronously after the failed stop's catch.
    await sleeps(registrationDeadlineMs(cfg)).reached(2);

    expect(windows.count).toBe(1);
    expect(managedState.trees[root]).toMatchObject({ generation: 1, status: "active" });
    expect(managedState.trees[root]?.launchFailures).toBe(0);

    // The re-armed deadline elapses; this time the stop succeeds, so it retires and resurrects.
    killPaneShouldFail = false;
    secondGate.resolve();
    await windows.reached(2);
    // The resurrection's whole spawn -- locator recorded, tree active, persisted -- is the event.
    await processes.drainSpawns();

    expect(managedState.trees[root]).toMatchObject({ generation: 2, status: "active" });
    expect(managedState.trees[root]?.launchFailures).toBe(1);
  });

  it("reconnectRoots re-arms the registration deadline for an active tree with a locator that never confirmed before a restart", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = {
      root,
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@41",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/architect.sock",
      },
      status: "active",
      launchFailures: 0,
      // No readyConfirmedAt: this tree never reached /process/ready before the restart this
      // test simulates -- reconnectRoots is what must re-arm its deadline, since the in-memory
      // rootRegistrationWaits map itself never survives a restart.
    };
    const sleepGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    const commands: string[][] = [];
    const windows = eventCounter();
    let paneAlive = true;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await sleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") {
          return paneAlive ? livePanes(command) : paneGone();
        }
        if (command[3] === "new-window") {
          windows.increment();
          return {
            stdout: `@5${windows.count} %${windows.count} 2000${windows.count}\n`,
            exitCode: 0,
          };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    processes.reconnectRoots();

    // The pane died sometime during the restart; the re-armed deadline's own expiry probe
    // discovers it dead and resurrects, exactly as it would have if the deadline had survived
    // the restart intact.
    paneAlive = false;
    sleepGate.resolve();
    await windows.reached(1);
    // The resurrection's whole spawn -- locator recorded, tree active, persisted -- is the event.
    await processes.drainSpawns();

    expect(windows.count).toBe(1);
    expect(managedState.trees[root]).toMatchObject({ generation: 2, status: "active" });
    expect(managedState.trees[root]?.launchFailures).toBe(1);
  });

  it("reconnectRoots leaves an already-confirmed active tree, a locator-less tree, and a non-active tree untouched", async () => {
    const state = newLegionState("omp", 1);
    const confirmedIssue = "LEGION-43";
    const noLocatorIssue = "LEGION-44";
    const queuedIssue = "LEGION-45";
    for (const issue of [confirmedIssue, noLocatorIssue, queuedIssue]) {
      state.issues[issue] = { key: issue, title: issue, status: "todo", children: [] };
    }
    const locator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@41",
      tmuxPaneId: "%1",
    };
    state.trees[confirmedIssue] = {
      root: confirmedIssue,
      generation: 1,
      locator,
      status: "active",
      launchFailures: 0,
      readyConfirmedAt: 1_700_000_000_000,
    };
    state.trees[noLocatorIssue] = {
      root: noLocatorIssue,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.trees[queuedIssue] = {
      root: queuedIssue,
      generation: 0,
      locator,
      status: "queued",
      launchFailures: 0,
    };
    let sleepCalls = 0;
    const { manager: processes } = manager(state, {
      sleep: async () => {
        sleepCalls += 1;
        await new Promise<void>(() => {});
      },
    });

    processes.reconnectRoots();

    // None of the three trees ever arms a deadline: the confirmed one is already confirmed, the
    // locator-less one has nothing worth protecting, and the queued one is not active.
    expect(sleepCalls).toBe(0);
  });

  it("beginLinger cancels a pending root-registration deadline so its stale expiry never retires or resurrects", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let sessionExists = false;
    let windowCount = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") {
          sessionExists = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windowCount += 1;
          return { stdout: `@4${windowCount} %${windowCount} 1000${windowCount}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    expect(windowCount).toBe(1);

    await processes.beginLinger(root);
    expect(managedState.trees[root]?.status).toBe("lingering");
    const commandsAfterLinger = commands.length;

    // The registration deadline armed by the spawn above must have been cancelled by
    // `beginLinger`: its stale fire takes no action on the now-lingering tree.
    sleepGate.resolve();
    // Negative wait: the fire reaches only stillUnconfirmed() (entry cancelled by beginLinger).
    await flushEventLoop();

    expect(commands.length).toBe(commandsAfterLinger);
    expect(managedState.trees[root]?.status).toBe("lingering");
  });

  it("leaves the stale locator in place when the stop/kill attempt fails, instead of orphaning a still-live pane with no controller ever spawned onto it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const locator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
      ...paneIdentity(),
    };
    state.controllerLocator = { ...locator };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const {
      manager: processes,
      state: managedState,
      runs,
    } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "list-panes") return livePanes(command);
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", stderr: "tmux: unable to kill pane", exitCode: 1 };
        }
        if (command[3] === "new-window") return { stdout: "@71 %10 222222\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    sleepGate.resolve();
    // The failed kill-pane is the last injected call: the stop's rejection, the catch, and the
    // log that leaves the locator in place are its synchronous continuation.
    await runs("kill-pane").completed.reached(1);

    // The kill-pane attempt ran and failed, but the locator survives exactly as it was -- never
    // cleared, and no second controller spawned onto what may still be a live pane.
    expect(commands.some((command) => command[0] === "tmux" && command[3] === "kill-pane")).toBe(
      true
    );
    expect(commands.some((command) => command[3] === "new-window")).toBe(false);
    expect(managedState.controllerLocator).toEqual(locator);
  });

  it("spawns a replacement when a stale controller claim receives a delivery exception", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.roles[controllerToken("omp")] = {
      issue: root,
      role: "controller",
      sessionId: "ses-stale",
    };
    let controllerSpawned = false;
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "list-windows") {
          return {
            stdout: controllerSpawned ? "controller\n" : "",
            exitCode: 0,
          };
        }
        if (command[3] === "list-panes") {
          return controllerSpawned ? livePanes(command) : paneGone();
        }
        if (command[3] === "new-window") {
          controllerSpawned = true;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.handleException(
      exception(controllerToken("omp"), {
        topic: "notifications.github.sjawhar.legion.issue.42.comment",
        payload: '{"body":"controller retry"}',
        eventId: "evt-stale-controller",
      })
    );

    expect(controllerSpawned).toBe(true);
    expect(publications).toEqual([]);
  });

  it("marks an exited tree dead and releases its admission slot", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    let saves = 0;
    const { manager: processes } = manager(state, {
      saveState: async () => {
        saves += 1;
      },
    });

    await processes.markProcessDead(root);

    expect(state.trees[root]).toMatchObject({ status: "dead" });
    expect(state.admission.active).toEqual([]);
    expect(saves).toBeGreaterThan(0);
  });

  it("ignores a stale explicit process-exit generation", async () => {
    const state = newLegionState("omp", 1);
    tree(state, root, 2);
    state.admission.active.push(root);
    const { manager: processes } = manager(state);

    await processes.markProcessDead(root, 1);

    expect(state.trees[root].status).toBe("active");
    expect(state.admission.active).toEqual([root]);
  });

  it("reclaims a live root architect directly instead of resurrecting it", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes, publications } = manager(state, {
      run: liveRun,
      sleep: async () => {},
    });

    await processes.handleException(exception(roleToken("omp", root, "architect")));

    const original = exception(roleToken("omp", root, "architect")).original;
    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
  });

  it.each([
    "no_holder",
    "receipt_timeout",
  ] as const)("resurrects a dead root architect on a %s exception", async (reason) => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const rootLocator = state.trees[root].locator;
    if (!rootLocator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...rootLocator, ompSessionFile: sessionFile };
    let launched = false;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[3] === "list-windows") return { stdout: "", exitCode: 1 };
        if (command[3] === "new-window") {
          launched = true;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[3] === "split-window") {
          launched = true;
          return { stdout: "%2 12345\n", exitCode: 0 };
        }
        if (command[3] === "list-panes" && command.includes("#{pane_id} #{pane_pid}")) {
          return launched ? livePanes(command) : paneGone();
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.handleException(
      exception(roleToken("omp", root, "architect"), undefined, reason)
    );

    expect(state.trees[root]).toMatchObject({ generation: 2 });
  });

  // The core-NATS exception lane has no redelivery: a `handleException` that rejected would sink
  // into the event pump's in-memory failures with no log at all. A probe the runtime could not
  // complete (a `list-panes` that proves nothing about the pane) must therefore resolve here --
  // logged once naming the role token, nothing cleared, nothing launched, no directive sent --
  // and leave the retry to the resync backstop or the next exception. Same shape for the root
  // architect (probe -> reclaim | resurrect) and the controller (`ensureController`).
  it.each([
    {
      subject: "the root architect",
      paneId: "%0",
      roleTokenUnderTest: roleToken("omp", root, "architect"),
      seed: (state: LegionState) => {
        tree(state);
        state.trees[root].readyConfirmedAt = Date.parse("2026-08-24T00:00:00.000Z");
        return state.trees[root].locator;
      },
      locatorAfter: (state: LegionState) => state.trees[root].locator,
      // What else must be untouched: the tree itself -- same generation, still active.
      untouched: (state: LegionState) =>
        expect(state.trees[root]).toMatchObject({ generation: 1, status: "active" }),
    },
    {
      subject: "the controller",
      paneId: "%1",
      roleTokenUnderTest: controllerToken("omp"),
      seed: (state: LegionState) => {
        state.controllerLocator = {
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: "@controller",
          tmuxPaneId: "%1",
          socketPath: "/state/workers/controller.sock",
          ...paneIdentity(),
        };
        return state.controllerLocator;
      },
      locatorAfter: (state: LegionState) => state.controllerLocator,
      // No role claim was minted for a controller nobody confirmed alive.
      untouched: (state: LegionState) =>
        expect(state.roles[controllerToken("omp")]).toBeUndefined(),
    },
  ])("handleException logs once, naming the role token, and clears nothing when $subject's liveness probe cannot complete", async ({
    paneId,
    roleTokenUnderTest,
    seed,
    locatorAfter,
    untouched,
  }) => {
    const state = newLegionState("omp", 1);
    const seededLocator = seed(state);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const {
      manager: processes,
      commands,
      controlRequests,
    } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") {
          return { stdout: "", stderr: "tmux: server not responding", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.handleException(exception(roleTokenUnderTest));

      const logged = errorLog.mock.calls.filter(([message]) =>
        String(message).includes(roleTokenUnderTest)
      );
      expect(logged).toHaveLength(1);
      expect(String(logged[0]?.[1])).toContain(
        `cannot verify pane ${paneId}: list-panes -t ${paneId} exited 1: tmux: server not responding`
      );
    } finally {
      errorLog.mockRestore();
    }
    expect(locatorAfter(state)).toBe(seededLocator);
    untouched(state);
    expect(
      commands.filter((command) => command[3] === "new-window" || command[3] === "kill-pane")
    ).toEqual([]);
    // The probe throw was never read as alive: no `reclaim-architect` directive went out
    // (`controlDirective` travels `natsRequest`, which the harness records here).
    expect(controlRequests).toEqual([]);
  });

  it("handleException logs once, naming the role token, when the token's issue is recorded by no tree, instead of rejecting on the exception lane", async () => {
    const state = newLegionState("omp", 1);
    const orphanToken = roleToken("omp", "LEGION-404", "implementer");
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, commands, controlRequests } = manager(state);

    try {
      await processes.handleException(exception(orphanToken));

      const logged = errorLog.mock.calls.filter(([message]) =>
        String(message).includes(orphanToken)
      );
      expect(logged).toHaveLength(1);
      expect(String(logged[0]?.[1])).toContain("No Legion tree records issue LEGION-404");
    } finally {
      errorLog.mockRestore();
    }
    expect(commands).toEqual([]);
    expect(controlRequests).toEqual([]);
  });

  it("receipt_timeout for a root architect whose pane probes alive publishes nothing, sends no directive, and logs one line naming the role token and event id", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const architectToken = roleToken("omp", root, "architect");
    const commands: string[][] = [];
    const {
      manager: processes,
      publications,
      controlRequests,
    } = manager(state, { run: recordingLiveRun(commands) });
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      await processes.handleException(exception(architectToken, undefined, "receipt_timeout"));
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    expect(publications).toEqual([]);
    expect(controlRequests).toEqual([]);
    expect(errorLines).toHaveLength(1);
    expect(errorLines[0]).toContain(architectToken);
    expect(errorLines[0]).toContain("evt-1");
    expect(state.trees[root]).toMatchObject({ generation: 1, status: "active" });
    expect(
      commands.some((command) => command[3] === "new-window" || command[3] === "split-window")
    ).toBeFalse();
  });

  /** The architect's role topic and one payload every failed copy of that message shares. */
  function architectMessage(payload: string, eventId: string, dedupeKey?: string) {
    return {
      topic: roleTopic(roleToken("omp", root, "architect")),
      payload,
      eventId,
      ...(dedupeKey === undefined ? {} : { dedupeKey }),
    };
  }

  /** Records every command while answering a live tree, so a "nothing launched" assertion can
   * read `commands` — `liveRun` alone records nothing. */
  function recordingLiveRun(commands: string[][]): ProcessManagerDeps["run"] {
    return async (command) => {
      commands.push(command);
      return liveRun(command);
    };
  }

  it("a different message starts its own re-send count, each re-send carrying its own exception's dedupe key", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const architectToken = roleToken("omp", root, "architect");
    const clock = manualSleep();
    const {
      manager: processes,
      publications,
      controlRequests,
      sleeps,
    } = manager(state, { run: liveRun, sleep: clock.sleep });
    const messageA = architectMessage(
      '{"type":"child-adopted","child":"LEGION-81","remaining":2}',
      "evt-1",
      "publish.d1"
    );
    const messageB = architectMessage(
      '{"type":"worker-started","issue":"LEGION-81","role":"tester"}',
      "evt-2",
      "publish.d2"
    );

    const armedA = sleeps(5_000).next();
    const handledA = processes.handleException(
      exception(architectToken, messageA, "delivery_failed")
    );
    await armedA;
    const armedB = sleeps(5_000).next();
    const handledB = processes.handleException(
      exception(architectToken, messageB, "delivery_failed")
    );
    await armedB;
    // Each message is its own chain at attempt 1; nothing is sent before a pause elapses.
    expect(clock.pending.map((wait) => wait.ms)).toEqual([5_000, 5_000]);
    expect(publications).toEqual([]);
    expect(clock.fire(5_000)).toBeTrue();
    expect(clock.fire(5_000)).toBeTrue();
    await Promise.all([handledA, handledB]);

    expect(controlRequests).toHaveLength(2);
    expect(publications).toEqual([
      { subject: messageA.topic, json: messageA.payload, dedupeKey: "publish.d1" },
      { subject: messageB.topic, json: messageB.payload, dedupeKey: "publish.d2" },
    ]);
  });

  it("re-sends a message whose exception carried no dedupe_key (an older listener) without one, logging 'dedupe key none'", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const architectToken = roleToken("omp", root, "architect");
    const clock = manualSleep();
    const {
      manager: processes,
      publications,
      sleeps,
    } = manager(state, { run: liveRun, sleep: clock.sleep });
    const message = architectMessage('{"type":"pr-comment"}', "evt-1");
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      const armed = sleeps(5_000).next();
      const handled = processes.handleException(
        exception(architectToken, message, "delivery_failed")
      );
      await armed;
      expect(clock.fire(5_000)).toBeTrue();
      await handled;
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    expect(publications).toEqual([{ subject: message.topic, json: message.payload }]);
    expect("dedupeKey" in (publications[0] ?? {})).toBeFalse();
    expect(errorLines.filter((line) => line.includes("dedupe key none"))).toHaveLength(1);
  });

  it("drops, without counting, an exception for a message whose re-send pause is still running", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const architectToken = roleToken("omp", root, "architect");
    const clock = manualSleep();
    const {
      manager: processes,
      publications,
      sleeps,
    } = manager(state, { run: liveRun, sleep: clock.sleep });
    const copy = (eventId: string) =>
      exception(
        architectToken,
        architectMessage('{"type":"pr-comment"}', eventId, "publish.d1"),
        "delivery_failed"
      );
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      const firstArmed = sleeps(5_000).next();
      const first = processes.handleException(copy("evt-1"));
      await firstArmed;
      // The same message fails again while its first re-send is still waiting its pause.
      await processes.handleException(copy("evt-2"));
      expect(clock.pending.map((wait) => wait.ms)).toEqual([5_000]);
      expect(clock.fire(5_000)).toBeTrue();
      await first;
      expect(publications).toHaveLength(1);

      // The dropped exception was not counted: the next failure is attempt 2 (15 s), not 3.
      const thirdArmed = sleeps(15_000).next();
      const third = processes.handleException(copy("evt-3"));
      await thirdArmed;
      expect(clock.pending.map((wait) => wait.ms)).toEqual([15_000]);
      expect(clock.fire(15_000)).toBeTrue();
      await third;
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    expect(publications).toHaveLength(2);
    const dropped = errorLines.filter((line) => line.includes("dropped without counting"));
    expect(dropped).toHaveLength(1);
    expect(dropped[0]).toContain(architectToken);
    expect(dropped[0]).toContain("evt-2");
  });

  it("resurrects a root that dies during the re-send pause instead of directing it", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const rootLocator = state.trees[root].locator;
    if (!rootLocator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...rootLocator, ompSessionFile: sessionFile };
    const architectToken = roleToken("omp", root, "architect");
    const clock = manualSleep();
    let alive = true;
    let launched = false;
    const deadRun = async (command: string[]) => {
      if (command[3] === "list-windows") return { stdout: "", exitCode: 1 };
      if (command[3] === "new-window") {
        launched = true;
        return { stdout: "@42 %1 12345\n", exitCode: 0 };
      }
      if (command[3] === "split-window") {
        launched = true;
        return { stdout: "%2 12345\n", exitCode: 0 };
      }
      if (command[3] === "list-panes" && command.includes("#{pane_id} #{pane_pid}")) {
        return launched ? livePanes(command) : paneGone();
      }
      return { stdout: "", exitCode: 0 };
    };
    const {
      manager: processes,
      publications,
      controlRequests,
      sleeps,
    } = manager(state, {
      config: config(stateDir),
      sleep: clock.sleep,
      run: (command) => (alive ? liveRun(command) : deadRun(command)),
    });

    const armed = sleeps(5_000).next();
    const handled = processes.handleException(
      exception(
        architectToken,
        architectMessage('{"type":"pr-comment"}', "evt-1", "publish.d1"),
        "delivery_failed"
      )
    );
    await armed;
    alive = false;
    expect(clock.fire(5_000)).toBeTrue();
    await handled;

    expect(state.trees[root]).toMatchObject({ generation: 2 });
    expect(controlRequests).toEqual([]);
    expect(publications).toEqual([]);
  });

  it("dispose() cancels a pending re-send pause so firing it afterwards sends nothing", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const architectToken = roleToken("omp", root, "architect");
    const clock = manualSleep();
    const {
      manager: processes,
      publications,
      controlRequests,
      sleeps,
    } = manager(state, { run: liveRun, sleep: clock.sleep });

    const armed = sleeps(5_000).next();
    const handled = processes.handleException(
      exception(
        architectToken,
        architectMessage('{"type":"pr-comment"}', "evt-1", "publish.d1"),
        "delivery_failed"
      )
    );
    await armed;
    processes.dispose();
    expect(clock.fire(5_000)).toBeTrue();
    await handled;

    expect(controlRequests).toEqual([]);
    expect(publications).toEqual([]);
  });

  // Acceptance line 6 (and line 3's spacing and cap): the whole chain against a fake runtime,
  // fake pane acknowledgement, and injected clock, with a receiver modelling the plugin's existing
  // dedupe. The stream models the real listener: it mints a fresh `event_id` for every publish
  // and the exception reports the failed copy's (`messageEnvelope`, api.go; `publishDeliveryException`,
  // delivery.go), so only the topic, the payload, and — from a LEGION-108 listener — the dedupe
  // key survive across the chain. No smoke rig, scratch daemon, throwaway broker, or scratch
  // tmux server stands in for this (Sami, 2026-09-13).
  it.each([
    {
      listener: "a listener honouring the publish body's dedupe_key (LEGION-108)",
      copyDedupeKey: (_copy: number) => "publish.d1",
      injections: 1,
    },
    {
      listener: "a listener minting a fresh dedupe key per copy (pre-LEGION-108)",
      copyDedupeKey: (copy: number) => `publish.fresh-${copy}`,
      injections: 3,
    },
  ])("integration: bounded re-sends for one message against an alive architect — $listener", async ({
    copyDedupeKey,
    injections: expectedInjections,
  }) => {
    const state = newLegionState("omp", 1);
    tree(state);
    const architectToken = roleToken("omp", root, "architect");
    const topic = roleTopic(architectToken);
    const payload = '{"type":"child-adopted","child":"LEGION-81","remaining":2}';
    const clock = manualSleep();
    const commands: string[][] = [];
    // The receiver: the plugin drops a copy whose dedupe key it has already seen and injects the
    // rest. A keyless copy is always new to it.
    const seen = new Set<string>();
    const injections: string[] = [];
    const copies: Array<{ subject: string; json: string; dedupeKey?: string }> = [];
    const {
      manager: processes,
      controlRequests,
      sleeps,
    } = manager(state, {
      run: recordingLiveRun(commands),
      sleep: clock.sleep,
      publishRole: (subject, json, dedupeKey) => {
        copies.push({ subject, json, dedupeKey });
        const key = dedupeKey ?? `keyless-${copies.length}`;
        if (seen.has(key)) return;
        seen.add(key);
        injections.push(json);
      },
    });
    // Exception 1 reports the original's key; every later one reports the failed copy's.
    const dedupeKeyOf = (copy: number) => (copy === 1 ? "publish.d1" : copyDedupeKey(copy));
    const failure = (copy: number) =>
      exception(
        architectToken,
        architectMessage(payload, `evt-${copy}`, dedupeKeyOf(copy)),
        "delivery_failed"
      );
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      for (let copy = 1; copy <= MAX_RESENDS; copy += 1) {
        const pauseMs = RESEND_PAUSES_MS[copy - 1] as number;
        const armed = sleeps(pauseMs).next();
        const handled = processes.handleException(failure(copy));
        await armed;
        // Nothing is sent before the pause elapses.
        expect(copies).toHaveLength(copy - 1);
        expect(controlRequests).toHaveLength(copy - 1);
        expect(clock.fire(pauseMs)).toBeTrue();
        await handled;
      }
      // The fourth failure of the same message hits the cap: no pause, no directive, no copy.
      await processes.handleException(failure(4));
      expect(clock.pending).toEqual([]);

      expect(copies).toEqual([
        { subject: topic, json: payload, dedupeKey: dedupeKeyOf(1) },
        { subject: topic, json: payload, dedupeKey: dedupeKeyOf(2) },
        { subject: topic, json: payload, dedupeKey: dedupeKeyOf(3) },
      ]);
      expect(injections).toHaveLength(expectedInjections);
      expect(injections[0]).toBe(payload);
      expect(controlRequests).toHaveLength(3);
      controlRequests.forEach(({ subject, json }, index) => {
        expect(subject).toBe("legion.ctl.legion-42.1");
        // The directive shape is unchanged: the dedupe key never travels inside it.
        expect(json).not.toContain("dedupeKey");
        expect(JSON.parse(json)).toEqual({
          type: "reclaim-architect",
          issue: root,
          redeliver: { topic, payload, eventId: `evt-${index + 1}` },
        });
      });

      // Negative control: a fifth failure after the cap is a new chain's attempt 1 (the cap
      // dropped the entry) — it waits its pause, and nothing more is sent until it elapses.
      const fifthArmed = sleeps(5_000).next();
      const fifth = processes.handleException(failure(5));
      await fifthArmed;
      expect(clock.pending.map((wait) => wait.ms)).toEqual([5_000]);
      expect(copies).toHaveLength(3);
      processes.dispose();
      clock.fire(5_000);
      await fifth;
      expect(copies).toHaveLength(3);
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    const resends = errorLines.filter((line) => line.includes("re-sending event"));
    expect(resends).toHaveLength(4);
    for (let copy = 1; copy <= MAX_RESENDS; copy += 1) {
      const line = resends[copy - 1] ?? "";
      expect(line).toContain(architectToken);
      expect(line).toContain(`evt-${copy}`);
      expect(line).toContain(`attempt ${copy} of ${MAX_RESENDS}`);
      expect(line).toContain(dedupeKeyOf(copy));
    }
    const capped = errorLines.filter((line) => line.includes("re-send cap reached"));
    expect(capped).toHaveLength(1);
    expect(capped[0]).toContain(architectToken);
    expect(capped[0]).toContain("evt-4");
    expect(capped[0]).toContain(dedupeKeyOf(4));
    expect(state.trees[root]).toMatchObject({ generation: 1, status: "active" });
    expect(
      commands.some((command) => command[3] === "new-window" || command[3] === "split-window")
    ).toBeFalse();
  });

  it("connects a worker-shim client to the root architect's socket when its tree becomes ready", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...recordedTmuxLocator(state),
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      socketPath: "/state/workers/sjawhar__legion-42-architect.sock",
    };
    const connectedSockets: string[] = [];
    const client = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return client;
      },
    });

    await processes.markTreeReady(root);

    expect(connectedSockets).toEqual(["/state/workers/sjawhar__legion-42-architect.sock"]);
    expect(client.negotiated).toBe(true);
  });

  it("connects a worker-shim client to the controller's socket when it becomes ready", async () => {
    const state = newLegionState("omp", 1);
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
      tmuxPaneId: "%1",
      socketPath: "/state/workers/controller.sock",
    };
    const connectedSockets: string[] = [];
    const client = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return client;
      },
    });

    await processes.markControllerReady();

    expect(connectedSockets).toEqual(["/state/workers/controller.sock"]);
    expect(client.negotiated).toBe(true);
  });

  it("does nothing when the controller has no recorded socket yet", async () => {
    const state = newLegionState("omp", 1);
    const connectedSockets: string[] = [];
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return fakeWorkerRpcClient();
      },
    });

    await processes.markControllerReady();

    expect(connectedSockets).toEqual([]);
  });

  it("leaves a root architect's recovery to the resync probe instead of starting a worker", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "root-architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const token = roleToken("omp", root, "architect");
    // Deliberately resumable: without resumeWorker's root-architect guard this claim would launch
    // a worker pane for the root's architect, bypassing the resync probe that owns root recovery.
    state.roles[token] = {
      issue: root,
      role: "architect",
      sessionId: "ses_root_architect",
      resumeSessionFile: sessionFile,
    };
    const {
      manager: processes,
      commands,
      publications,
    } = manager(state, {
      config: config(stateDir),
    });

    await processes.resumeWorker(root, root, "architect");

    expect(commands).toEqual([]);
    expect(publications).toEqual([]);
  });

  it("resumes a sub-architect (a child issue's architect role) through the same worker path, not the root-architect resurrection path", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "architect";
    const token = roleToken("omp", child, role);
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_sub_architect",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-architect.sock",
      },
    };
    // A sub-architect is never its child's active phase once it has spawned a planner; it is
    // still always resumed (this is its only recovery path), and the phase is left alone.
    state.phases[child] = { phase: "planner", sessionId: "ses_planner" };
    const client = fakeWorkerRpcClient();
    // Only an idle client is prompted directly (see `WorkerAdmission.resumeOrQueueExisting`); a
    // real client's own `getState()` call just before this would already have seeded `runState`
    // from `isStreaming` as idle here, since nothing is in flight to interrupt.
    client.setRunStateSilently("idle");
    const {
      manager: processes,
      publications,
      controlRequests,
    } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.handleException(exception(token));

    expect(client.prompts).toEqual([
      JSON.stringify({
        type: "catchup-overseer",
        gates: { [child]: {} },
        childCounts: { [child]: { total: 0, open: 0, closed: 0 } },
        prVerdicts: {},
        phaseCompletions: [],
      }),
    ]);
    expect(publications).toEqual([]);
    expect(controlRequests).toEqual([]);
    expect(state.phases[child]).toEqual({ phase: "planner", sessionId: "ses_planner" });
  });

  // Requires a real tmux installation to prove window IDs survive cosmetic-name collisions.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "probes its live root through its window id when a duplicate cosmetic name exists",
    async () => {
      const stateDir = await temporaryDir();
      const project = `duplicate${Date.now()}`;
      const state = newLegionState(project, 1);
      state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
      state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
      state.admission.active.push(root);
      const session = `legion-${project}`;
      const commandRunner = async (command: string[]) => {
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        const actual =
          command[3] === "new-window" || (command[3] === "new-session" && command.includes("-n"))
            ? [...command.slice(0, -1), "sleep 999"]
            : command;
        const child = Bun.spawn(actual, { stdout: "pipe", stderr: "pipe" });
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        // Exactly `defaultRunner`'s shape: stderr stays separate, so `lookupPane` can read a real
        // `can't find pane` as the pane being gone rather than as a listing that failed.
        return { stdout, stderr, exitCode };
      };
      const { manager: processes } = manager(state, {
        config: config(stateDir, { legionId: project }),
        readProcessCmdline: async () => "omp\0",
        run: commandRunner,
      });

      try {
        await processes.spawnRoot(root);
        await commandRunner([
          "tmux",
          "-L",
          session,
          "new-window",
          "-t",
          session,
          "-n",
          "sjawhar-legion-42",
          "sleep 999",
        ]);

        expect(await processes.probe(root)).toBe("alive");
      } finally {
        await commandRunner(["tmux", "-L", session, "kill-session", "-t", session]);
      }
    }
  );

  // Requires a real tmux installation to ensure session creation has no leftover shell window.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "creates the first live root without a default bash window",
    async () => {
      const stateDir = await temporaryDir();
      const project = `defaultwindow${Date.now()}`;
      const state = newLegionState(project, 1);
      state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
      state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
      state.admission.active.push(root);
      const session = `legion-${project}`;
      const commandRunner = async (command: string[]) => {
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        const actual =
          command[3] === "new-window" || (command[3] === "new-session" && command.includes("-n"))
            ? [...command.slice(0, -1), "sleep 999"]
            : command;
        const child = Bun.spawn(actual, { stdout: "pipe", stderr: "pipe" });
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        // Exactly `defaultRunner`'s shape: stderr stays separate, so `lookupPane` can read a real
        // `can't find pane` as the pane being gone rather than as a listing that failed.
        return { stdout, stderr, exitCode };
      };
      const { manager: processes } = manager(state, {
        config: config(stateDir, { legionId: project }),
        run: commandRunner,
      });

      try {
        await processes.spawnRoot(root);
        const windows = await commandRunner([
          "tmux",
          "-L",
          session,
          "list-windows",
          "-t",
          session,
          "-F",
          "#{window_name}",
        ]);

        // The listing must be a real answer from the server this root spawned into: a
        // `no server running` error (the runtime spawned somewhere else) would otherwise satisfy
        // the `bash` assertion below vacuously.
        expect(windows.exitCode).toBe(0);
        const windowNames = windows.stdout.split(/\r?\n/);
        expect(windowNames).toContain("legion-42");
        expect(windowNames).not.toContain("bash");
      } finally {
        await commandRunner(["tmux", "-L", session, "kill-session", "-t", session]);
      }
    }
  );

  // Requires a real tmux installation to exercise pane lifecycle.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "probes a real tmux pane as alive, detects its death, and resurrects it once",
    async () => {
      const stateDir = await temporaryDir();
      const project = `smoke${Date.now()}`;
      const state = newLegionState(project, 1);
      state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
      state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
      state.admission.active.push(root);
      const session = `legion-${project}`;
      const commandRunner = async (command: string[]) => {
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        const actual =
          command[3] === "new-window" || (command[3] === "new-session" && command.includes("-n"))
            ? [...command.slice(0, -1), "sleep 999"]
            : command;
        const child = Bun.spawn(actual, { stdout: "pipe", stderr: "pipe" });
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        // Exactly `defaultRunner`'s shape: stderr stays separate, so `lookupPane` can read a real
        // `can't find pane` as the pane being gone rather than as a listing that failed.
        return { stdout, stderr, exitCode };
      };
      const { manager: processes } = manager(state, {
        config: config(stateDir, { legionId: project }),
        readProcessCmdline: async () => "omp\0",
        // The real `/proc/<pid>/stat` read, against the real `sleep` the pane runs.
        readProcessStat: undefined,
        run: commandRunner,
      });

      try {
        await processes.spawnRoot(root);
        const launched = tmuxFields(state.trees[root]?.locator);
        if (!launched?.panePid) throw new Error("live root is missing its pane identity");
        expect(launched.paneStartTicks).toBe(
          parseProcStatStartTicks(await readFile(`/proc/${launched.panePid}/stat`, "utf8"))
        );
        expect(await processes.probe(root)).toBe("alive");
        const firstWindowId = tmuxFields(state.trees[root]?.locator)?.tmuxWindowId;
        if (!firstWindowId) throw new Error("live root is missing its tmux window id");
        await commandRunner(["tmux", "-L", session, "kill-pane", "-t", firstWindowId]);
        expect(await processes.probe(root)).toBe("dead");
        await Promise.all([processes.resurrect(root), processes.resurrect(root)]);
        expect(await processes.probe(root)).toBe("alive");
        expect(
          (await commandRunner(["tmux", "-L", session, "list-windows", "-t", session])).stdout
        ).toContain("legion-42");
      } finally {
        await commandRunner(["tmux", "-L", session, "kill-session", "-t", session]);
      }
    },
    // A real tmux server and real `/proc` reads: the same per-test budget the other real-process
    // tests (`real-shutdown-e2e.test.ts`) run under, never bun's 5 s default.
    30_000
  );

  // Requires a real tmux installation: the whole delivery chain for a pane's PATH — tmux, the
  // pane shell, the real `legion worker-shim`, the process it spawns — on a live server.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "a real controller, root, and worker pane hand their OMP process a PATH with worker-bin first exactly once, and gh under it is the shim (LEGION-91)",
    async () => {
      const stateDir = await temporaryDir();
      const project = `smoke${Date.now()}`;
      const state = newLegionState(project, 1);
      state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
      state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
      state.admission.active.push(root);
      const session = `legion-${project}`;
      const workerBin = await installWorkerGhShim(stateDir);
      // The daemon's own PATH as index.ts hands it over: never a worker-bin entry (this test may
      // itself run from a Legion pane whose PATH carries one).
      const processPath = pathWithoutWorkerBin(process.env.PATH ?? "");
      const commandRunner = async (command: string[]) => {
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        const child = Bun.spawn(command, { stdout: "pipe", stderr: "pipe" });
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        return { stdout, stderr, exitCode };
      };
      const { manager: processes } = manager(state, {
        config: config(stateDir, { legionId: project }),
        // The OMP stand-in ignores its argv and lives until stdin EOF, exactly where a real OMP
        // would sit under the worker-shim.
        ompInvocation: `${process.execPath} ${DELAYED_START_OMP}`,
        processPath,
        readProcessCmdline: async () => "omp\0",
        readProcessStat: undefined,
        run: commandRunner,
      });
      try {
        await processes.ensureController();
        await processes.spawnRoot(root);
        await processes.spawnWorker(root, root, "tester", "verify #41");
        const testerClaim = state.roles[roleToken(project, root, "tester")];
        const panePids = [
          tmuxFields(state.controllerLocator)?.panePid,
          tmuxFields(state.trees[root]?.locator)?.panePid,
          testerClaim && "issue" in testerClaim
            ? tmuxFields(testerClaim.locator)?.panePid
            : undefined,
        ];
        for (const panePid of panePids) {
          if (!panePid) throw new Error("a live pane is missing its identity");
          const environment = await ompEnvironment(panePid);
          expect(environment.PATH).toBe(`${workerBin}${path.delimiter}${processPath}`);
          expect(
            environment.PATH?.split(path.delimiter).filter((e) => path.basename(e) === "worker-bin")
          ).toHaveLength(1);
          // A bare `gh` in that pane is the shim, i.e. `legion gh`.
          const which = Bun.spawnSync(["sh", "-c", "command -v gh"], {
            env: { PATH: environment.PATH ?? "" },
          });
          expect(which.stdout.toString().trim()).toBe(path.join(workerBin, "gh"));
        }
      } finally {
        await commandRunner(["tmux", "-L", session, "kill-server"]);
      }
    },
    30_000
  );

  it("runs a root's whole lifecycle over a non-tmux Runtime: spawn, probe alive, close, reconcile", async () => {
    // The behavioural half of the runtime-agnostic gate: `ProcessManager` driven end to end by
    // `FakeRuntime`, whose locators are the kubernetes union member — nothing tmux-shaped exists
    // anywhere in this test, so any manager path that still assumed a pane would fail here.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const runtime = new FakeRuntime();
    const commands: string[][] = [];
    // No injected `sleep`: the manager's root-registration deadline must stay a real (long)
    // timer here, or it would fire at once and retire this never-confirmed root before the
    // assertions below — `dispose()` (afterEach) cancels it.
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      runtime,
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    const locator = state.trees[root]?.locator;
    expect(locator?.runtime).toBe("kubernetes");
    expect(runtime.spawned.map((spawn) => [spawn.kind, spawn.spec.role, spawn.spec.issue])).toEqual(
      [["root", "architect", root]]
    );
    expect(runtime.spawned[0]?.spec.secrets).toEqual({ LEGION_BOOT_TOKEN: "boot-token" });
    expect(runtime.spawned[0]?.spec.env).toMatchObject({
      LEGION_TREE: root,
      LEGION_ROLE: "architect",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
    });
    expect(await processes.probe(root)).toBe("alive");
    // What a real root's `/process/ready` does; the graceful close below then finds a confirmed,
    // reachable process rather than one whose shim never connected.
    processes.confirmRootReady(root, 1);
    expect(state.trees[root]).toMatchObject({ status: "active", launchFailures: 0 });

    await processes.closeTree(root);

    // The root was stopped through the runtime (gracefully: `probe` said alive, so no
    // skipGraceful), its record cleared, and the tree closed.
    expect(runtime.stopped.map((stop) => [stop.locator, stop.options?.skipGraceful])).toEqual([
      [locator, false],
    ]);
    expect((await runtime.probe(locator as Locator)).status).toBe("dead");
    expect(state.trees[root]?.status).toBe("closed");
    expect(state.trees[root]?.locator).toBeUndefined();

    // Nothing recorded, nothing known: the sweep hands the runtime an empty handle set.
    await processes.reconcileOrphans(0);
    expect(runtime.reconciled).toEqual([{ known: new Set(), graceMs: 0 }]);
    // And the manager never issued a tmux command of its own.
    expect(commands.filter((command) => command[0] === "tmux")).toEqual([]);
  });

  it("refuses a runtime's unknown verdict on the controller instead of treating it as dead", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const controllerLocator: Locator = {
      runtime: "kubernetes",
      namespace: "fake",
      podName: "controller-1",
      podUid: "uid-controller",
      pvcName: "fake-pvc",
      roleToken: "legion-omp-controller",
    };
    state.controllerLocator = controllerLocator;
    const runtime = new FakeRuntime();
    runtime.probe = async () => ({ status: "unknown" });
    const { manager: processes } = manager(state, { config: config(stateDir), runtime });

    await expect(processes.ensureController()).rejects.toThrow(
      "Runtime probe reported an unknown status for the controller; ProcessManager has no unknown-status policy"
    );
    // The record of a possibly-live controller survives; nothing was spawned beside it.
    expect(state.controllerLocator).toBe(controllerLocator);
    expect(runtime.spawned).toEqual([]);
  });

  /** A FakeRuntime-backed tree whose root has spawned and confirmed ready, with the manager's own
   * client for it already connected -- the shape every "handle now belongs to another process"
   * case below starts from. */
  async function fakeRuntimeTree(): Promise<{
    processes: ProcessManager;
    state: LegionState;
    runtime: FakeRuntime;
    locator: Locator;
    client: FakeWorkerRpcClient;
  }> {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const clients: FakeWorkerRpcClient[] = [];
    const runtime = new FakeRuntime({
      clientFactory: () => {
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
    });
    const { manager: processes } = manager(state, { config: config(stateDir), runtime });
    await processes.spawnRoot(root);
    processes.confirmRootReady(root, 1);
    const locator = state.trees[root]?.locator;
    if (!locator) throw new Error("spawned root has no locator");
    await runtime.connect(locator);
    const [client] = clients;
    if (!client) throw new Error("connect created no client");
    return { processes, state, runtime, locator, client };
  }

  it("closes a tree whose root's handle now belongs to another process: asks the recorded process to exit over its own socket, refuses the destroy step, clears the locator, and logs both identities once", async () => {
    const { processes, state, runtime, locator, client } = await fakeRuntimeTree();
    runtime.occupyHandle(locator, { detail: "pane %1 now runs pid 999 (recorded pid 1 start 2)" });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let logged: string[] = [];
    try {
      await processes.closeTree(root);
    } finally {
      logged = errors.mock.calls.map((call) => call.map(String).join(" "));
      errors.mockRestore();
    }

    // B2: the graceful ask still went out (the socket is role-scoped, so it reaches exactly the
    // recorded process if it is alive at all), and the runtime was told not to destroy what now
    // holds the handle -- the `options` are ProcessManager's decision; what the runtime does with
    // them is the runtime contract tests' business.
    expect(runtime.stopped.map((stop) => [stop.locator, stop.timeoutMs, stop.options])).toEqual([
      [locator, 60_000, { skipGraceful: false, refuseKill: true }],
    ]);
    expect(client.runState).toBe("idle"); // the fake shim closed after its shutdown frame
    expect(state.trees[root]?.status).toBe("closed");
    expect(state.trees[root]?.locator).toBeUndefined();
    expect(logged.filter((line) => line.includes("treating LEGION-42's root as dead"))).toEqual([
      "[legion] treating LEGION-42's root as dead: pane %1 now runs pid 999 (recorded pid 1 start 2)",
    ]);
  });

  it("resurrects a root whose handle now belongs to another process: asks the recorded process to exit with the destroy step refused, resumes onto a fresh process, and logs the decision once", async () => {
    const { processes, state, runtime, locator } = await fakeRuntimeTree();
    runtime.occupyHandle(locator, { detail: "pane %1 now runs pid 999 (recorded pid 1 start 2)" });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let logged: string[] = [];
    try {
      // Exactly the composition index.ts's `onProbe` runs on a resync tick.
      if ((await processes.probe(root)) === "dead") await processes.resurrect(root);
    } finally {
      logged = errors.mock.calls.map((call) => call.map(String).join(" "));
      errors.mockRestore();
    }

    // What ProcessManager decided: the graceful ask still goes out, the destroy step is refused.
    expect(runtime.stopped.map((stop) => stop.options)).toEqual([
      { skipGraceful: false, refuseKill: true },
    ]);
    expect(runtime.spawned.map((spawn) => spawn.kind)).toEqual(["root", "root"]);
    expect(state.trees[root]?.locator).toBeDefined();
    expect(state.trees[root]?.locator).not.toEqual(locator);
    expect(await processes.probe(root)).toBe("alive");
    expect(
      logged.filter((line) => line.includes("treating LEGION-42's root as dead"))
    ).toHaveLength(1);
  });

  it("replaces a controller whose handle now belongs to another process: asks the recorded controller to exit, refuses the destroy step, clears its locator, spawns a fresh one, and logs both identities once", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const runtime = new FakeRuntime();
    const { manager: processes } = manager(state, { config: config(stateDir), runtime });
    await processes.ensureController();
    const first = state.controllerLocator;
    if (!first) throw new Error("controller did not spawn");
    await runtime.connect(first);
    runtime.occupyHandle(first, { detail: "pane %9 now runs pid 777 (recorded pid 5 start 6)" });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let logged: string[] = [];
    try {
      await processes.ensureController();
    } finally {
      logged = errors.mock.calls.map((call) => call.map(String).join(" "));
      errors.mockRestore();
    }

    expect(runtime.stopped.map((stop) => [stop.locator, stop.timeoutMs, stop.options])).toEqual([
      [first, 10_000, { refuseKill: true }],
    ]);
    expect(runtime.spawned.map((spawn) => spawn.kind)).toEqual(["controller", "controller"]);
    expect(state.controllerLocator).toBeDefined();
    expect(state.controllerLocator).not.toEqual(first);
    expect(logged).toEqual([
      "[legion] treating the controller as dead: pane %9 now runs pid 777 (recorded pid 5 start 6)",
    ]);
  });

  it("under a runtime that does not launch the controller, ensureController mints nothing, spawns nothing, writes no state, and logs once across repeated controller-bound events", async () => {
    // Every controller-bound event under Kubernetes reaches `ensureController`; before, each one
    // minted a controller capability (a state write) and then threw from the runtime's refusal.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const runtime = new FakeRuntime({ launchesController: false });
    let mints = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      runtime,
      mintControllerCapability: async () => {
        mints += 1;
        return "controller-secret";
      },
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let logged: string[] = [];
    try {
      await processes.ensureController();
      await processes.ensureController();
      await processes.ensureController();
    } finally {
      logged = errors.mock.calls.map((call) => call.map(String).join(" "));
      errors.mockRestore();
    }
    expect(mints).toBe(0);
    expect(runtime.spawned).toEqual([]);
    expect(managedState.controllerLocator).toBeUndefined();
    expect(managedState.roles[controllerToken("omp")]).toBeUndefined();
    expect(logged).toEqual([
      "[legion] the controller is not launched by this runtime (LEGION-25); controller-bound events wait for one started elsewhere",
    ]);
  });

  it("retires an unconfirmed worker boot at the first watchdog interval once its handle belongs to another process and its socket refuses, relaunching the same role", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", root, role);
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const runtime = new FakeRuntime({ sleep: async () => {} });
    // Wrapped like `idleWorkerFixture`'s `client.shutdown`: every spawn the fake completes is one
    // event, and the relaunch is the 2nd.
    const spawns = eventCounter();
    const spawn = runtime.spawn.bind(runtime);
    runtime.spawn = async (kind, spec) => {
      const locator = await spawn(kind, spec);
      spawns.increment();
      return locator;
    };
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerBootTimeoutSeconds: 1 }),
      runtime,
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
    });

    await processes.spawnWorker(root, root, role, "implement #41");
    const booting = managedState.roles[token];
    if (!booting || !("issue" in booting) || !booting.locator) throw new Error("no worker claim");
    const workerLocator = booting.locator;
    runtime.occupyHandle(workerLocator, { reachable: false });

    // The retirement's own event: the same claim relaunched onto a fresh process (the 2nd spawn;
    // launchFailures was counted before it, and the fresh locator is written synchronously after
    // it), not merely the counter ticking before the promotion has run.
    await spawns.reached(2);

    // The retirement stopped exactly the old locator, once, letting the runtime decide the kill
    // (no `skipGraceful`: the graceful ask is still attempted; no verdict handed down -- the
    // watchdog reached the boot through the socket refusal, not a probe verdict of its own).
    expect(
      runtime.stopped
        .filter((stop) => sameProcess(stop.locator, workerLocator))
        .map((stop) => stop.options)
    ).toEqual([undefined]);
    // The retry relaunched the same role onto a fresh process, still carrying its assignment.
    const relaunched = managedState.roles[token];
    if (!relaunched || !("issue" in relaunched)) throw new Error("relaunched claim missing");
    expect(relaunched.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "implement #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(relaunched.locator).toBeDefined();
    expect(relaunched.locator).not.toEqual(workerLocator);
  });

  it("spawns a worker's first pane as a new window with the full worker env and worker-shim command", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(result).toEqual({ status: "spawned", roleToken: roleToken("omp", root, "tester") });
    const windowCommand = commands.find(
      (command) => command[0] === "tmux" && command[3] === "new-window" && command.includes("-n")
    );
    if (!windowCommand) throw new Error("worker spawn did not open a tmux window");
    expect(windowCommand[windowCommand.indexOf("-n") + 1]).toBe("legion-42");
    expect(tmuxWindowEnvironment(windowCommand)).toEqual({
      LEGION_TREE: root,
      LEGION_ISSUE: root,
      LEGION_ROLE: "tester",
      LEGION_WORKSPACE: workspace,
      LEGION_BOOT_TOKEN_FILE: path.join(stateDir, "secrets", roleToken("omp", root, "tester")),
      LEGION_GENERATION: "1",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      LEGION_STATE_DIR: stateDir,
      LEGION_CREDENTIAL_HELPER: "!/opt/legion/bun /opt/legion/cli/index.ts credential",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      GIT_CONFIG_COUNT: "0",
      GIT_TERMINAL_PROMPT: "0",
      ...HARNESS_IDENTITY_ENV,
      GH_CONFIG_DIR: path.join(stateDir, "gh"),
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      GH_HOST: "",
      LEGION_GRANT_FILE: path.join(
        stateDir,
        "secrets",
        `${roleToken("omp", root, "tester")}-grant`
      ),
    });
    expect(tmuxPanePath(windowCommand)).toBe(
      `${path.join(stateDir, "worker-bin")}${path.delimiter}/full/bin:/usr/bin`
    );
    const promptPath = path.join(
      path.resolve(import.meta.dir, "../../../../pi-envoy"),
      "roles",
      "tester.md"
    );
    expect(windowCommand.at(-1)).toBe(
      `export PATH=${path.join(stateDir, "worker-bin")}${path.delimiter}/full/bin:/usr/bin && cd ${workspace} && ${process.execPath} ${path.resolve(import.meta.dir, "../../cli/index.ts")} worker-shim --socket ${path.join(stateDir, "workers", "tester-9e2fb104.sock")} -- /opt/oh-my-pi/18.0.3/omp --mode rpc ${promptArgument(`${promptPath}`, addressingFragment("omp", root, root, "tester"))}`
    );
    const claim = managedState.roles[roleToken("omp", root, "tester")];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    expect(claim.generation).toBe(1);
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.locator).toMatchObject({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@99",
      tmuxPaneId: "%201",
      socketPath: path.join(stateDir, "workers", "tester-9e2fb104.sock"),
    });
  });

  it("puts an addressing line naming the launched process's own role topic and its tree's architect topic into its system prompt", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "implementer", "implement #41");

    const workerLaunch = commands.find(
      (command) => command[0] === "tmux" && command[3] === "new-window"
    );
    if (!workerLaunch) throw new Error("worker spawn did not open a tmux window");
    const workerArgv = workerLaunch.at(-1) ?? "";
    expect(workerArgv).toContain(roleTopic(roleToken("omp", root, "implementer")));
    expect(workerArgv).toContain(roleTopic(roleToken("omp", root, "architect")));

    const { manager: rootProcesses, commands: rootCommands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
    });
    await rootProcesses.spawnRoot(root);

    const rootLaunch = rootCommands.find(
      (command) => command[0] === "tmux" && command[3] === "new-window"
    );
    if (!rootLaunch) throw new Error("root spawn did not open a tmux window");
    const rootArgv = rootLaunch.at(-1) ?? "";
    expect(rootArgv).toContain(roleTopic(roleToken("omp", root, "architect")));
  });

  it("splits a second worker on the same issue into the window the first worker just opened", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@99 %101 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%201 67890\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    const result = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(result.status).toBe("spawned");
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toHaveLength(1);
    const split = commands.find(
      (command) => command[0] === "tmux" && command[3] === "split-window"
    );
    if (!split) throw new Error("second worker did not split the existing window");
    expect(split).toContain("@99");
    expect(commands).toContainEqual([
      "tmux",
      "-L",
      "legion-omp",
      "select-layout",
      "-t",
      "@99",
      "tiled",
    ]);
  });

  it("serializes two concurrent first spawns on the same issue into a single new window", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const commands: string[][] = [];
    let windowsOpened = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          // Widens the race window a concurrency bug would need to slip through.
          await Bun.sleep(5);
          windowsOpened += 1;
          return { stdout: `@99 %${100 + windowsOpened} 12345\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%201 67890\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") {
          return windowsOpened > 0 ? livePanes(command) : paneGone();
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const [plannerResult, testerResult] = await Promise.all([
      processes.spawnWorker(root, root, "planner", "plan #41"),
      processes.spawnWorker(root, root, "tester", "verify #41"),
    ]);

    expect(plannerResult.status).toBe("spawned");
    expect(testerResult.status).toBe("spawned");
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toHaveLength(1);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "split-window")
    ).toHaveLength(1);
    const plannerClaim = managedState.roles[roleToken("omp", root, "planner")];
    const testerClaim = managedState.roles[roleToken("omp", root, "tester")];
    if (!plannerClaim || !("issue" in plannerClaim) || !testerClaim || !("issue" in testerClaim)) {
      throw new Error("both worker claims must be recorded");
    }
    expect(tmuxFields(plannerClaim.locator)?.tmuxWindowId).toBe("@99");
    expect(tmuxFields(testerClaim.locator)?.tmuxWindowId).toBe("@99");
  });

  it("retires its own just-opened pane and reports TreeClosingError, not a launch failure, when the tree starts closing while the pane was opening", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-43");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%0",
        socketPath: "/state/workers/architect.sock",
        ...paneIdentity(),
      },
      status: "active",
      launchFailures: 0,
    };
    const paneOpenGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const stuckRootClient = fakeWorkerRpcClient();
    stuckRootClient.shutdown = () => {};
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      config: config(stateDir),
      connectWorkerRpc: async () => stuckRootClient,
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          await paneOpenGate.promise;
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, child, "tester", "verify #41");
    // Poll (real macrotask ticks, not just microtasks -- the workspace/socket prep this crosses
    // first are real fs operations) until the launch has actually reached its blocked
    // `new-window` call before starting the race.
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[3] === "new-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[3] === "new-window")).toBe(true);

    const closePromise = processes.closeTree(root);
    paneOpenGate.resolve();

    const result = await spawnPromise.catch((caught: unknown) => caught);

    expect(result).toBeInstanceOf(TreeClosingError);
    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%201"]);
    expect(managedState.roles[roleToken("omp", child, "tester")]).toBeUndefined();

    await closePromise;
  });

  it("keeps closeTree from finishing while a launch is still in flight, so its own retire always runs before closingTrees clears", async () => {
    // Without waiting on `inFlightLaunches`, closeTree's fixed-point loop could see an empty
    // `state.roles` snapshot (the launch below hasn't written its claim yet), finish, and clear
    // `closingTrees` -- all while this launch is still blocked mid-flight. Once it later
    // resumes, its own post-launch closing check would find `closingTrees` already empty and
    // write the claim as if nothing had happened, leaving a fresh pane alive and untracked in a
    // tree already reported closed.
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-43");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    // No root locator: the root leg is skipped entirely, isolating this test to the worker race.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const paneOpenGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          await paneOpenGate.promise;
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, child, "tester", "verify #41");
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[3] === "new-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[3] === "new-window")).toBe(true);

    let closeSettled = false;
    const closePromise = processes.closeTree(root).then(() => {
      closeSettled = true;
    });

    // Give closeTree every real chance to run its (otherwise-empty) worker loop and finish while
    // the launch above is still blocked on `new-window`.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(closeSettled).toBe(false);

    paneOpenGate.resolve();
    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    await closePromise;

    expect(spawnResult).toBeInstanceOf(TreeClosingError);
    expect(closeSettled).toBe(true);
    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%201"]);
    expect(managedState.roles[roleToken("omp", child, "tester")]).toBeUndefined();
    expect(managedState.trees[root].status).toBe("closed");
  });

  it("preserves a launch's just-opened locator for closeTree to retry when its own post-launch retire fails to stop the pane", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-43");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const paneOpenGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          await paneOpenGate.promise;
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", stderr: "lost server", exitCode: 1 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, child, "tester", "verify #41");
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[3] === "new-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    const closePromise = processes.closeTree(root);
    paneOpenGate.resolve();

    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    expect(spawnResult).toBeInstanceOf(StopFailed);
    const token = roleToken("omp", child, "tester");
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim was discarded on StopFailed");
    expect(tmuxFields(claim.locator)?.tmuxPaneId).toBe("%201");
    // This claim never existed before this launch, so freshClaim's carry-over
    // (`claim?.launchFailures ?? 0`) has nothing to carry and starts at 0 — never reset by a
    // mere relaunch; only accumulated across retries and cleared by /worker/started.
    expect(claim.launchFailures).toBe(0);

    const closeResult = await closePromise.catch((caught: unknown) => caught);
    expect(closeResult).toBeInstanceOf(StopFailed);
    expect(managedState.trees[root].status).toBe("lingering");
    errorLog.mockRestore();
  });

  it("throws TreeClosingError from a stale-claim probe's recheck when a concurrent closeTree starts mid-probe, leaving the untouched old claim for closeTree itself to reap", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    // No root locator: isolates this test to the worker race.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const probeGate = Promise.withResolvers<void>();
    const stuckClient = fakeWorkerRpcClient();
    stuckClient.getStateImpl = async () => {
      await probeGate.promise;
      throw new Error("dead");
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => stuckClient,
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, root, "tester", "verify again");
    for (let attempt = 0; attempt < 100 && stuckClient.getStateCalls === 0; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(stuckClient.getStateCalls).toBe(1);

    let closeSettled = false;
    const closePromise = processes.closeTree(root).then(() => {
      closeSettled = true;
    });
    // Give closeTree every real chance to reap the untouched stale claim and finish while the
    // probe above is still blocked -- without `inFlightLaunches` covering this whole decision
    // (not merely `launchWorker`'s own pane-opening step), closeTree's fixed-point loop could
    // take its first snapshot right now, see the claim's still-stale locator, stop it, and
    // delete the claim, all before the still-running decision below has had any chance to
    // recheck `closingTrees` and bail out on its own.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(closeSettled).toBe(false);

    probeGate.resolve();
    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    await closePromise;

    expect(spawnResult).toBeInstanceOf(TreeClosingError);
    expect(closeSettled).toBe(true);
    // Never retired or relaunched: the decision's post-probe recheck threw before either step,
    // leaving the stale claim entirely untouched for closeTree itself to reap.
    expect(
      commands.some((c) => c[0] === "tmux" && (c[3] === "new-window" || c[3] === "split-window"))
    ).toBe(false);
    expect(managedState.roles[token]).toBeUndefined();
    expect(managedState.trees[root].status).toBe("closed");
  });

  it("opens a fresh window when every recorded pane is gone, leaves the stale locators naming their dead window, and splits later workers into the fresh window", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const plannerToken = roleToken("omp", root, "planner");
    const implementerToken = roleToken("omp", root, "implementer");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[implementerToken] = {
      issue: root,
      role: "implementer",
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%301 67890\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") {
          // The panes recorded in the dead `@42` are gone; the fresh window's `%201` is live and
          // still the process the tester's locator recorded.
          const target = command[command.indexOf("-t") + 1];
          return target === "%201" ? livePanes(command) : paneGone();
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toHaveLength(1);
    // The stale claims keep naming the window their panes lived in -- a locator's window id is
    // never rewritten from the outside; each clears through its own probe.
    for (const token of [plannerToken, implementerToken]) {
      const claim = managedState.roles[token];
      if (!claim || !("issue" in claim)) throw new Error(`${token} claim disappeared`);
      expect(tmuxFields(claim.locator)?.tmuxWindowId).toBe("@42");
    }
    const tester = managedState.roles[roleToken("omp", root, "tester")];
    if (!tester || !("issue" in tester)) throw new Error("tester claim disappeared");
    expect(tmuxFields(tester.locator)?.tmuxWindowId).toBe("@99");

    await processes.spawnWorker(root, root, "reviewer", "review #41");

    // `@42`'s recorded panes still fail; the tester's fully-recorded pane in `@99` verifies, so
    // the reviewer splits into `@99` rather than opening yet another window.
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toHaveLength(1);
    const split = commands.find(
      (command) => command[0] === "tmux" && command[3] === "split-window"
    );
    if (!split) throw new Error("fourth worker did not split into the fresh window");
    expect(split).toContain("@99");
  });

  it("resumes an already-alive worker by sending the task over its live socket, spawning nothing new, and records the reassignment as the issue's active phase", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ompSessionFile: "/state/workers/tester-session.json",
      },
    };
    const client = fakeWorkerRpcClient();
    // Only an idle client is prompted directly (see `WorkerAdmission.resumeOrQueueExisting`); a
    // real client's own `getState()` call just before this would already have seeded `runState`
    // from `isStreaming` as idle here, since nothing is in flight to interrupt.
    client.setRunStateSilently("idle");
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify #55");

    expect(result).toEqual({ status: "resumed", roleToken: token });
    expect(client.prompts).toEqual(["verify #55"]);
    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    // Delivering an architect assignment is the one write of `phases[issue]`: a phase worker
    // re-prompted with a repeat assignment (the architect requested changes, or reassigned it a
    // second time) becomes the issue's active phase again, or its eventual
    // `legion handoff complete` 409s forever against a phase no route ever restored.
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  // jj keeps a rewritten commit's author: `jj split`/`jj describe` carve a phase's work out of the
  // issue's working-copy commit and only refresh the committer. The daemon's `jj workspace add`
  // created it under the daemon's identity, so jj preserves that author even when a split leaves
  // the remaining undescribed working copy empty. The runtime adopts that copy for the role under
  // the JJ_USER/JJ_EMAIL pair of the pane's lease identity (LEGION-44). The recorded command's
  // own env is asserted, never re-derived.
  /** Records every `jj metaedit` the daemon runs, with the command's env and timeout budget and how
   * many prompts the worker had received when it ran (0 = before the assignment frame). */
  function recordingMetaedits(client: FakeWorkerRpcClient) {
    const metaedits: Array<{
      command: string[];
      env: NodeJS.ProcessEnv;
      timeoutMs: number | undefined;
      promptsBefore: number;
    }> = [];
    const run: TmuxRuntimeDeps["run"] = async (command, options) => {
      if (command[0] === "jj" && command[1] === "metaedit") {
        metaedits.push({
          command,
          env: options?.env ?? {},
          timeoutMs: options?.timeoutMs,
          promptsBefore: client.prompts.length,
        });
        return { stdout: "", stderr: "Nothing changed.\n", exitCode: 0 };
      }
      return { stdout: "", exitCode: 0 };
    };
    return { metaedits, run };
  }

  it("adopts the issue's undescribed working copy for the role before prompting an assignment into a live idle worker", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    const { metaedits, run } = recordingMetaedits(client);
    const { manager: processes } = manager(state, { connectWorkerRpc: async () => client, run });

    await processes.spawnWorker(root, root, "tester", "verify #55");

    expect(client.prompts).toEqual(["verify #55"]);
    expect(metaedits).toHaveLength(1);
    expect(metaedits[0]?.command).toEqual(
      adoptWorkingCopyCommand("/state/workspaces/sjawhar/legion/legion-42")
    );
    expect(metaedits[0]?.env).toMatchObject({
      JJ_USER: HARNESS_GIT_IDENTITY.name,
      JJ_EMAIL: HARNESS_GIT_IDENTITY.email,
    });
    // `metaedit` snapshots the working copy: the slow budget, like every other daemon jj command
    // against a working copy, never the runner's generic one.
    expect(metaedits[0]?.timeoutMs).toBe(300_000);
    expect(metaedits[0]?.promptsBefore).toBe(0);
  });

  it("adopts the working copy for the role when /worker/ready delivers its pending assignment, and a failing metaedit prompts nothing", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    const claimAtRest: WorkerRoleClaim = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.roles[token] = structuredClone(claimAtRest);
    const client = fakeWorkerRpcClient();
    const { metaedits, run } = recordingMetaedits(client);
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
      run,
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual(["verify #41"]);
    expect(metaedits).toHaveLength(1);
    expect(metaedits[0]?.command).toEqual(
      adoptWorkingCopyCommand("/state/workspaces/sjawhar/legion/legion-42")
    );
    expect(metaedits[0]?.env).toMatchObject({
      JJ_USER: HARNESS_GIT_IDENTITY.name,
      JJ_EMAIL: HARNESS_GIT_IDENTITY.email,
    });
    expect(metaedits[0]?.timeoutMs).toBe(300_000);
    expect(metaedits[0]?.promptsBefore).toBe(0);
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });

    // A working copy that cannot be adopted is never prompted: nothing is written, the ready call
    // fails naming the command and why, and the claim is exactly as it was for the next ready
    // attempt. A jj error carries jj's own stderr; a kill by the runner at the budget carries the
    // runner's report (`timedOut`), never a bare `exit 143` with empty detail.
    const metaeditCommand = adoptWorkingCopyCommand(
      "/state/workspaces/sjawhar/legion/legion-42"
    ).join(" ");
    for (const [result, expectedMessage] of [
      [
        { stdout: "", stderr: "Error: The working copy is stale\n", exitCode: 1 },
        `Could not adopt ${root}'s working copy for tester: Command failed (exit 1): ${metaeditCommand}\nError: The working copy is stale`,
      ],
      [
        {
          stdout: "",
          stderr: "",
          exitCode: 143,
          timedOut: { limitMs: 300_000, elapsedMs: 300_412 },
        },
        `Could not adopt ${root}'s working copy for tester: Command timed out after 300 s (ran 300.4 s): ${metaeditCommand}`,
      ],
    ] as const) {
      const failing = newLegionState("omp", 1);
      failing.roles[token] = structuredClone(claimAtRest);
      const failingClient = fakeWorkerRpcClient();
      const { manager: failingProcesses, state: failingState } = manager(failing, {
        connectWorkerRpc: async () => failingClient,
        run: async (command) =>
          command[0] === "jj" && command[1] === "metaedit"
            ? { ...result }
            : { stdout: "", exitCode: 0 },
      });

      await expect(failingProcesses.workerReady(root, "tester", "ses_tester", 1)).rejects.toThrow(
        expectedMessage
      );
      expect(failingClient.prompts).toEqual([]);
      expect(failingState.phases[root]).toBeUndefined();
      expect(failingState.roles[token]).toEqual(claimAtRest);
      failingProcesses.dispose();
    }
  });

  it("runs no author adoption for a catch-up: recovery plumbing changes neither the phase nor the working copy", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      sessionId: "ses_implementer",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    state.phases[root] = { phase: "implementer", sessionId: "ses_implementer" };
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    const { metaedits, run } = recordingMetaedits(client);
    const { manager: processes } = manager(state, { connectWorkerRpc: async () => client, run });

    await processes.handleException(exception(token, exception(token).original));

    expect(client.prompts).toEqual([JSON.stringify({ type: "catchup-worker", unhandled: [] })]);
    expect(metaedits).toEqual([]);
  });

  it("treats a same-role spawn during an in-flight boot as resumed-pending, never launching a second pane", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    tree(state);
    const token = roleToken("omp", root, "tester");
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir),
    });

    const first = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(first).toEqual({ status: "spawned", roleToken: token });

    // /worker/started has not run yet, so the claim has a locator but no sessionId: this second
    // call must never open or split another pane, and must queue its task for worker/ready.
    const second = await processes.spawnWorker(root, root, "tester", "verify #55");
    expect(second).toEqual({ status: "resumed", roleToken: token });

    expect(
      commands.filter(
        (command) =>
          command[0] === "tmux" && (command[3] === "new-window" || command[3] === "split-window")
      )
    ).toHaveLength(1);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.sessionId).toBeUndefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #55",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("queues a same-role spawn against a started-but-unconfirmed claim as resumed-pending, never probing or prompting its socket", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      // readyConfirmedAt deliberately absent: /worker/started registered the session, but
      // /worker/ready has not yet durably confirmed the boot. This claim must be queued exactly
      // like the sessionId-less booting case, never probed or prompted as if already live.
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    let connectCalls = 0;
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => {
        connectCalls += 1;
        throw new Error("must not probe an unconfirmed boot's socket");
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify #55");

    expect(result).toEqual({ status: "resumed", roleToken: token });
    expect(connectCalls).toBe(0);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #55",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.readyConfirmedAt).toBeUndefined();
  });

  it("respawns with --resume when a worker's claimed socket is dead, splitting into its own persisted window", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    await writeFile(path.join(stateDir, "prior-tester-session.json"), "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: path.join(stateDir, "prior-tester-session.json"),
        ...paneIdentity(),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        // The dead-socket worker's pane itself is still running (its recorded process), which is
        // what keeps `@42` a window this respawn may split into.
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(result).toEqual({ status: "spawned", roleToken: token });
    const split = commands.find(
      (command) => command[0] === "tmux" && command[3] === "split-window"
    );
    if (!split) throw new Error("dead-worker respawn did not split its persisted window");
    expect(split).toContain("@42");
    expect(split.at(-1)).toContain(`--resume=${path.join(stateDir, "prior-tester-session.json")}`);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    expect(claim.generation).toBe(2);
  });

  it("queues a dead worker retried at cap by preserving its resume session file, then promotes it with --resume once a slot frees", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const resumeFile = path.join(stateDir, "prior-tester-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const occupierClient = fakeWorkerRpcClient();
    // Seeded idle so planner's own resume below prompts directly (see
    // `WorkerAdmission.resumeOrQueueExisting`), occupying the cap-1 slot: `prompt()` itself
    // then flips it to "running", matching what actually happens once the daemon calls it.
    occupierClient.setRunStateSilently("idle");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
        ...paneIdentity(),
      },
    };
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      agentId: "agt_tester",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: resumeFile,
      },
    };
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    let promotedExpectedSessionId: string | undefined;
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      sleep: async () => {},
      connectWorkerRpc: async (socketPath) => {
        if (socketPath === "/state/workers/dead-tester.sock") throw new Error("ECONNREFUSED");
        return occupierClient;
      },
      mintWorkerBootToken: async (_tree, _issue, _role, _generation, expectedSessionId) => {
        promotedExpectedSessionId = expectedSessionId;
        return "worker-boot-token";
      },
      saveState: async () => {
        const testerClaim = managedState.roles[testerToken];
        // The fixture's tester claim already carries a (stale, dead-socket) locator from the
        // start, so a bare `locator` truthiness check would resolve `promoted` prematurely
        // during the planner's own earlier resume `saveState` call. `%301` is the pane id only
        // the fresh promoted-relaunch's split-window mock reports, so this fires exactly once,
        // after the actual promotion lands.
        if (
          testerClaim &&
          "issue" in testerClaim &&
          tmuxFields(testerClaim.locator)?.tmuxPaneId === "%301"
        ) {
          resolvePromoted?.();
        }
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") {
          return command.includes("%7") ? paneGone() : livePanes(command);
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Discovers tester's dead socket up front (clearing its locator, preserving
    // resumeSessionFile) exactly as a real daemon restart's own reconnect sweep would - without
    // this, tester's still-recorded (but never yet probed) locator conservatively counts
    // against the cap-1 slot planner is about to claim (see `runningWorkerCount`'s doc
    // comment), blocking planner's own resume below before it ever gets a turn.
    await processes.reconnectWorkers();

    // Bring the planner into the cache as running, occupying the cap-1 slot.
    const plannerResume = await processes.spawnWorker(root, root, "planner", "plan #41");
    expect(plannerResume).toEqual({ status: "resumed", roleToken: plannerToken });
    expect(occupierClient.prompts).toEqual(["plan #41"]);

    const testerResult = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(testerResult).toEqual({ status: "queued", roleToken: testerToken });
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("tester claim disappeared");
    expect(queuedClaim.locator).toBeUndefined();
    expect(queuedClaim.resumeSessionFile).toBe(resumeFile);
    expect(queuedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify again",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    // Preserved in place, never replaced.
    expect(queuedClaim.agentId).toBe("agt_tester");
    // sessionId is kept (never deleted) while queued: a delayed promotion's boot token still
    // names this exact session as the one it must resume — the "respawn must resume the same
    // agent session" 409 guard the daemon-side worker/started handler enforces depends on it.
    expect(queuedClaim.sessionId).toBe("ses_tester");

    occupierClient.emitRunState("idle");
    await promoted;

    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim disappeared");
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(promotedClaim.resumeSessionFile).toBeUndefined();
    // The delayed promotion's mintWorkerBootToken call named the original session as the one
    // it must resume, since sessionId survived the whole queued interval.
    expect(promotedExpectedSessionId).toBe("ses_tester");
    const split = commands.find(
      (command) => command[0] === "tmux" && command[3] === "split-window"
    );
    if (!split) throw new Error("promoted worker did not split into its persisted window");
    expect(split).toContain("@42");
    expect(split.at(-1)).toContain(`--resume=${resumeFile}`);
  });

  it("kills a dead-socket worker's still-running pane before respawning it", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    await writeFile(path.join(stateDir, "prior-tester-session.json"), "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: path.join(stateDir, "prior-tester-session.json"),
        ...paneIdentity(22222),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("%7") &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          // The old pane's OMP child is still running despite the dead socket.
          return { stdout: "%7 22222\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(result).toEqual({ status: "spawned", roleToken: token });
    expect(
      commands.some(
        (command) => command[0] === "tmux" && command[3] === "kill-pane" && command.includes("%7")
      )
    ).toBeTrue();
    const killPaneIndex = commands.findIndex(
      (command) => command[3] === "kill-pane" && command.includes("%7")
    );
    const splitWindowIndex = commands.findIndex((command) => command[3] === "split-window");
    expect(killPaneIndex).toBeGreaterThanOrEqual(0);
    expect(splitWindowIndex).toBeGreaterThan(killPaneIndex);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    expect(claim.generation).toBe(2);
  });

  it("fails a worker respawn loudly when its recorded OMP session file is missing, never starting fresh", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: path.join(stateDir, "missing-tester-session.json"),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await expect(processes.spawnWorker(root, root, "tester", "verify again")).rejects.toThrow(
      /recorded OMP session file is missing/
    );

    expect(
      commands.some((command) => command[0] === "tmux" && command[3] === "split-window")
    ).toBeFalse();
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.launchFailures).toBe(1);
  });

  it("carries launchFailures forward through a successful launch - only a confirmed /worker/started resets it", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    // A claim that already failed twice below MAX_LAUNCH_FAILURES (e.g. rotated through the
    // queue) but has no locator (this attempt starts clean, not a --resume).
    state.roles[token] = { issue: root, role: "tester", launchFailures: 2 };
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(result).toEqual({ status: "spawned", roleToken: token });
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    // A launch opening a pane is not yet a confirmed boot - the watchdog's own accounting
    // (armed by this same launch) must still see the prior failure count in case this pane
    // never gets to /worker/started either. Only that confirmation (api/routes/workers.ts)
    // resets it.
    expect(claim.launchFailures).toBe(2);
  });

  it("respawns a dead-socket worker without deadlocking its own launch queue while retiring the stale locator", async () => {
    // `retireWorkerLocator`'s stop must be the RAW (unserialized) `stopProcess`. This whole call
    // is already running inside `spawnWorker`'s `workerAdmission.mutateClaim(token, …)` callback
    // for this exact token; if `retireWorkerLocator` instead called `stopProcessSerialized`
    // (which re-enters that same per-token critical section), it would await a promise that can
    // only settle after this very callback returns — deadlocking forever.
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ...paneIdentity(),
      },
    };
    const shutdownCalls: string[] = [];
    const staleClient = fakeWorkerRpcClient();
    const shutdown = staleClient.shutdown.bind(staleClient);
    staleClient.shutdown = () => {
      shutdownCalls.push("dead-tester");
      shutdown();
    };
    let connectAttempts = 0;
    // The first connect attempt is `spawnWorker`'s own liveness probe (`probeWorker` via the
    // cached, negotiating `clientFor`) — a genuine dead socket (connect failure), not
    // merely a busy one that answers `get_state` late (a connected-but-slow client now queues
    // instead of retiring+respawning — see `spawnWorker`'s liveness dialect). The second is
    // the tmux runtime's own raw, non-negotiating stop-time dial (`TmuxRuntime.stop`), used
    // only to send the shutdown frame — succeeding here is what this test's `shutdownCalls`
    // assertion needs.
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        connectAttempts += 1;
        if (connectAttempts === 1) throw new Error("ECONNREFUSED");
        return staleClient;
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const timeout = Symbol("timeout");
    const result = await Promise.race([
      processes.spawnWorker(root, root, "tester", "verify again"),
      new Promise((resolve) => setTimeout(() => resolve(timeout), 2_000)),
    ]);

    expect(result).not.toBe(timeout);
    expect(result).toEqual({ status: "spawned", roleToken: token });
    expect(shutdownCalls).toEqual(["dead-tester"]);
  });

  it("passes a respawned claim's existing sessionId as the worker boot token's expected session", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      sessionId: "ses_original",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ...paneIdentity(),
      },
    };
    let expectedSessionId: string | undefined;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      mintWorkerBootToken: async (_tree, _issue, _role, generation, sessionId) => {
        expectedSessionId = sessionId;
        return `boot-${generation}`;
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "tester", "verify again");

    expect(expectedSessionId).toBe("ses_original");
  });

  it("refuses to spawn a sub-architect at or beyond the configured recursion depth", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    tree(state);
    const { manager: processes } = manager(state, {
      config: config("/state", { maxRecursionDepth: 1 }),
    });

    await expect(processes.spawnWorker(root, child, "architect", "plan sub-tree")).rejects.toThrow(
      /recursion/
    );
  });

  it("spawns a released child's sub-architect as a phase worker of the parent tree", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "todo",
      children: [],
    };
    tree(state);
    state.admission.active = [root];
    const admissionBefore = structuredClone(state.admission);
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@7 %71 4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, child, "architect", "own this child");

    expect(result).toEqual({ status: "spawned", roleToken: roleToken("omp", child, "architect") });
    const windowCommand = commands.find(
      (command) => command[0] === "tmux" && command[3] === "new-window" && command.includes("-n")
    );
    if (!windowCommand) throw new Error("sub-architect spawn did not open a tmux window");
    expect(tmuxWindowEnvironment(windowCommand)).toMatchObject({
      LEGION_TREE: root,
      LEGION_ISSUE: child,
      LEGION_ROLE: "architect",
    });
    expect(windowCommand.at(-1)).toContain(path.join("roles", "architect.md"));
    // A worker claim with a locator: what `runningWorkerCount` counts against `worker_cap`.
    const claim = managedState.roles[roleToken("omp", child, "architect")];
    if (!claim || !("issue" in claim)) throw new Error("sub-architect claim was not recorded");
    expect(claim.issue).toBe(child);
    expect(claim.locator).toMatchObject({ tmuxWindowId: "@7", tmuxPaneId: "%71" });
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "own this child",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    // The child is a member of the parent's tree, never a tree or admission entry of its own.
    expect(managedState.trees[child]).toBeUndefined();
    expect(managedState.admission).toEqual(admissionBefore);
  });

  it("adoptOwnerlessChildTrees removes exactly the no-process child trees with a live ancestor, deletes and revokes each one's stale root-architect claim, and names surviving worker panes", () => {
    const issue = (key: string, parent?: string, children: string[] = []) => ({
      key,
      title: key,
      status: "todo" as const,
      children,
      ...(parent === undefined ? {} : { parent }),
    });
    const legacyTree = (
      key: string,
      status: LegionState["trees"][string]["status"],
      locator?: LegionState["trees"][string]["locator"]
    ) => ({
      root: key,
      generation: 1,
      status,
      launchFailures: 0,
      ...(locator === undefined ? {} : { locator }),
    });
    const state = newLegionState("omp", 4);
    // One active root owning five children of different shapes, a `queued` child carrying a
    // `queued` grandchild tree of its own, a lingering legacy child tree over a queued grandchild,
    // and a parentless queued root that must be left alone.
    state.issues["OMP-1"] = issue("OMP-1", undefined, [
      "OMP-2",
      "OMP-3",
      "OMP-4",
      "OMP-5",
      "OMP-6",
      "OMP-8",
    ]);
    tree(state, "OMP-1");
    state.issues["OMP-2"] = issue("OMP-2", "OMP-1", ["OMP-7"]);
    state.trees["OMP-2"] = legacyTree("OMP-2", "queued");
    state.issues["OMP-7"] = issue("OMP-7", "OMP-2");
    state.trees["OMP-7"] = legacyTree("OMP-7", "queued");
    state.issues["OMP-3"] = issue("OMP-3", "OMP-1");
    state.trees["OMP-3"] = legacyTree("OMP-3", "launch-failed");
    state.roles[roleToken("omp", "OMP-3", "architect")] = {
      issue: "OMP-3",
      role: "architect",
      sessionId: "ses_omp3_failed_root",
    };
    // A worker an earlier confirmed generation of OMP-3 spawned: locator kept, LEGION_TREE=OMP-3.
    state.roles[roleToken("omp", "OMP-3", "planner")] = {
      issue: "OMP-3",
      role: "planner",
      sessionId: "ses_omp3_planner",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@30",
        tmuxPaneId: "%30",
        socketPath: "/state/workers/omp3-planner.sock",
      },
    };
    state.issues["OMP-4"] = issue("OMP-4", "OMP-1");
    state.trees["OMP-4"] = legacyTree("OMP-4", "active");
    state.issues["OMP-5"] = issue("OMP-5", "OMP-1");
    state.trees["OMP-5"] = legacyTree("OMP-5", "dead");
    state.issues["OMP-6"] = issue("OMP-6", "OMP-1");
    state.trees["OMP-6"] = legacyTree("OMP-6", "active", {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@6",
      tmuxPaneId: "%6",
      socketPath: "/state/workers/omp6-architect.sock",
    });
    state.issues["OMP-8"] = issue("OMP-8", "OMP-1", ["OMP-9"]);
    state.trees["OMP-8"] = legacyTree("OMP-8", "lingering");
    state.issues["OMP-9"] = issue("OMP-9", "OMP-8");
    state.trees["OMP-9"] = legacyTree("OMP-9", "queued");
    state.issues["OMP-10"] = issue("OMP-10");
    state.trees["OMP-10"] = legacyTree("OMP-10", "queued");
    state.admission.active = ["OMP-1", "OMP-4", "OMP-6"];
    state.admission.queue = ["OMP-2", "OMP-7", "OMP-9", "OMP-10"];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const {
      manager: processes,
      state: managed,
      revokedSessions,
    } = manager(state, {
      config: config("/state"),
    });

    let adoptions: ChildAdoption[];
    let logged: string;
    try {
      adoptions = processes.adoptOwnerlessChildTrees();
      logged = errorLog.mock.calls.flat().map(String).join("\n");
    } finally {
      errorLog.mockRestore();
    }

    // Removed: queued (OMP-2), its queued grandchild (OMP-7, owned through OMP-2's parent chain
    // once the nearest tree is a live one -- OMP-2's own tree is `queued`, which owns),
    // launch-failed (OMP-3), active without a locator (OMP-4).
    expect(adoptions).toEqual([
      { child: "OMP-2", parent: "OMP-1" },
      { child: "OMP-7", parent: "OMP-2" },
      { child: "OMP-3", parent: "OMP-1" },
      { child: "OMP-4", parent: "OMP-1" },
    ]);
    // Kept: dead (OMP-5), active with a locator (OMP-6), the lingering legacy tree (OMP-8) and the
    // queued grandchild under it (OMP-9: its nearest ancestor tree is lingering, so it is an
    // orphan), and the parentless root (OMP-10).
    expect(Object.keys(managed.trees).sort()).toEqual([
      "OMP-1",
      "OMP-10",
      "OMP-5",
      "OMP-6",
      "OMP-8",
      "OMP-9",
    ]);
    expect(managed.admission.active).toEqual(["OMP-1", "OMP-6"]);
    expect(managed.admission.queue).toEqual(["OMP-9", "OMP-10"]);
    // The stale root-architect claim is gone and its capability revoked; the surviving worker's
    // claim stays (nothing here stops a process) and is named in the log.
    expect(managed.roles[roleToken("omp", "OMP-3", "architect")]).toBeUndefined();
    expect(revokedSessions).toEqual(["ses_omp3_failed_root"]);
    expect(managed.roles[roleToken("omp", "OMP-3", "planner")]).toBeDefined();
    expect(logged).toContain(
      `OMP-3's removed root tree still has worker claims with recorded panes (${roleToken("omp", "OMP-3", "planner")})`
    );
    expect(logged.match(/\(LEGION-57\)/g)).toHaveLength(4);

    // Idempotent.
    expect(processes.adoptOwnerlessChildTrees()).toEqual([]);
  });

  it("queues a second spawnWorker call at the running-worker cap and publishes worker-queued to the architect", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    const first = await processes.spawnWorker(root, root, "planner", "plan #41");
    expect(first).toEqual({ status: "spawned", roleToken: roleToken("omp", root, "planner") });

    const testerToken = roleToken("omp", root, "tester");
    const second = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(second).toEqual({ status: "queued", roleToken: testerToken });
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("queued claim missing");
    expect(queuedClaim.locator).toBeUndefined();
    expect(queuedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-queued", issue: root, role: "tester" }),
    });
  });

  it("replaces a queued task silently in place, preserving queuedAt", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    let now = Date.parse("2026-08-24T00:00:00.000Z");
    let saves = 0;
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      now: () => now,
      saveState: async () => {
        saves += 1;
      },
    });
    const testerToken = roleToken("omp", root, "tester");
    const workerQueuedCount = (): number =>
      publications.filter((publication) => publication.json.includes("worker-queued")).length;
    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "tester", "verify #41");
    const savesAfterQueue = saves;
    expect(workerQueuedCount()).toBe(1);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("queued claim missing");
    expect(queuedClaim.pendingAssignment?.queuedAt).toBe("2026-08-24T00:00:00.000Z");

    now += 60_000;
    const again = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(again).toEqual({ status: "queued", roleToken: testerToken });
    expect(saves).toBe(savesAfterQueue);
    expect(workerQueuedCount()).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    expect(queuedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("replaces a queued task with different text in place: same queue position, restamped queuedAt, worker-queued published again", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    let current = Date.parse("2026-08-24T00:00:00.000Z");
    let saves = 0;
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      now: () => current,
      saveState: async () => {
        saves += 1;
      },
    });
    const testerToken = roleToken("omp", root, "tester");
    const reviewerToken = roleToken("omp", root, "reviewer");
    const workerQueued = {
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-queued", issue: root, role: "tester" }),
    };
    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "tester", "verify #41");
    await processes.spawnWorker(root, root, "reviewer", "review #41");
    const savesBeforeReplacement = saves;

    current += 60_000;
    const again = await processes.spawnWorker(root, root, "tester", "verify #41 again");

    expect(again).toEqual({ status: "queued", roleToken: testerToken });
    expect(managedState.workerAdmission.queue).toEqual([testerToken, reviewerToken]);
    const claim = managedState.roles[testerToken];
    if (!claim || !("issue" in claim)) throw new Error("queued claim missing");
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41 again",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications.filter((p) => p.json === workerQueued.json)).toEqual([workerQueued]);
    expect(saves).toBe(savesBeforeReplacement + 1);
  });

  it("queues a root admitted and a worker spawned while launches are held, and launches both once enableLaunches() runs", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    const {
      manager: processes,
      state: managedState,
      commands,
      publications,
    } = manager(
      state,
      { config: config(stateDir, { workerCap: 2 }) },
      { skipEnableLaunches: true }
    );
    const testerToken = roleToken("omp", root, "tester");
    const paneOpens = () =>
      commands.filter(
        (command) =>
          command[0] === "tmux" && (command[3] === "new-window" || command[3] === "split-window")
      );

    expect(processes.admit(root)).toBe("queued");
    expect(managedState.admission.queue).toEqual([root]);
    expect(managedState.admission.active).toEqual([]);
    expect(managedState.trees[root]?.status).toBe("queued");

    const spawned = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(spawned).toEqual({ status: "queued", roleToken: testerToken });
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-queued", issue: root, role: "tester" }),
    });
    expect(paneOpens()).toEqual([]);

    processes.enableLaunches();
    await processes.reconcileAdmission();
    await processes.reconcileWorkerAdmission();

    expect(managedState.admission.active).toEqual([root]);
    expect(managedState.trees[root]).toMatchObject({ status: "active" });
    expect(managedState.trees[root]?.locator).toBeDefined();
    expect(managedState.workerAdmission.queue).toEqual([]);
    const testerClaim = managedState.roles[testerToken];
    if (!testerClaim || !("issue" in testerClaim)) throw new Error("tester claim missing");
    expect(testerClaim.locator).toBeDefined();
    expect(paneOpens().map((command) => command[3])).toEqual(["new-window", "split-window"]);
    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("holds a resurrection and a controller launch requested during the hold, and replays both after enableLaunches()", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    let windows = 0;
    const { manager: processes, state: managedState } = manager(
      state,
      {
        config: config(stateDir),
        run: async (command) => {
          if (command[3] === "list-windows") return { stdout: "", exitCode: 1 };
          if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
          if (command[3] === "new-window") {
            windows += 1;
            return { stdout: `@${windows} %${windows} ${12345 + windows}\n`, exitCode: 0 };
          }
          // Nothing recorded is alive: the root's pane is gone (tmux says so), no controller pane
          // exists.
          if (command[3] === "list-panes") return paneGone();
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnableLaunches: true }
    );
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      await processes.resurrect(root);
      await processes.ensureController();
      expect(windows).toBe(0);
      expect(managedState.controllerLocator).toBeUndefined();
      const logged = errorSpy.mock.calls.map((call) => String(call[0]));
      expect(logged).toContainEqual(
        expect.stringContaining(`resurrection of ${root} held until the OMP probe passes`)
      );
      expect(logged).toContainEqual(
        expect.stringContaining("controller launch held until the OMP probe passes")
      );

      processes.enableLaunches();
      await processes.replayHeldRecoveries();
    } finally {
      errorSpy.mockRestore();
    }

    expect(windows).toBe(2);
    expect(managedState.trees[root]).toMatchObject({ status: "active", generation: 2 });
    expect(recordedTmuxLocator(managedState).tmuxWindowId).toBe("@1");
    expect(tmuxFields(managedState.controllerLocator)?.tmuxWindowId).toBe("@2");
  });

  it("does not replay a held resurrection for a tree that stopped being active during the hold", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    let windows = 0;
    const { manager: processes, state: managedState } = manager(
      state,
      {
        config: config(stateDir),
        run: async (command) => {
          if (command[3] === "list-windows") return { stdout: "", exitCode: 1 };
          if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
          if (command[3] === "new-window") {
            windows += 1;
            return { stdout: `@${windows} %${windows} ${12345 + windows}\n`, exitCode: 0 };
          }
          if (command[3] === "list-panes") return paneGone();
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnableLaunches: true }
    );
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      await processes.resurrect(root);
      // The tree is parked while the probe is still retrying: nothing should bring it back.
      await processes.beginLinger(root);
      processes.enableLaunches();
      await processes.replayHeldRecoveries();
    } finally {
      errorSpy.mockRestore();
    }

    expect(windows).toBe(0);
    expect(managedState.trees[root]).toMatchObject({ status: "lingering", generation: 1 });
  });

  it("replays a held resurrection for a root whose own exit report marked it dead during the hold", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    let windows = 0;
    const { manager: processes, state: managedState } = manager(
      state,
      {
        config: config(stateDir),
        run: async (command) => {
          if (command[3] === "list-windows") return { stdout: "", exitCode: 1 };
          if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
          if (command[3] === "new-window") {
            windows += 1;
            return { stdout: `@${windows} %${windows} ${12345 + windows}\n`, exitCode: 0 };
          }
          if (command[3] === "list-panes") return paneGone();
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnableLaunches: true }
    );
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      await processes.resurrect(root);
      // A root that survived the restart exits during the hold and reports it (POST
      // /process/exit): `dead` is exactly the state a resurrection exists to recover from.
      await processes.markProcessDead(root);
      expect(managedState.trees[root]?.status).toBe("dead");
      processes.enableLaunches();
      await processes.replayHeldRecoveries();
    } finally {
      errorSpy.mockRestore();
    }

    expect(windows).toBe(1);
    expect(managedState.trees[root]).toMatchObject({ status: "active", generation: 2 });
  });

  it("promotes the queued worker once the running one goes idle, publishing worker-started", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const clients: Array<{ emitRunState(state: "running" | "idle"): void; close(): void }> = [];
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
      publishRole: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);

    const plannerClaim = managedState.roles[plannerToken];
    if (!plannerClaim || !("issue" in plannerClaim)) throw new Error("planner claim missing");
    plannerClaim.sessionId = "ses_planner";
    await processes.workerReady(root, "planner", "ses_planner", plannerClaim.generation ?? 1);
    expect(clients).toHaveLength(1);

    clients[0]?.emitRunState("idle");
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("queues an idle-resume request at the running-worker cap, then promotes it by prompting in place (not relaunching)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const plannerClient = fakeWorkerRpcClient();
    // Seeded idle so planner's own resume below prompts directly (see
    // `WorkerAdmission.resumeOrQueueExisting`), occupying the cap-1 slot: `prompt()` itself
    // then flips it to "running", matching what actually happens once the daemon calls it.
    plannerClient.setRunStateSilently("idle");
    const testerClient = fakeWorkerRpcClient();
    // Seeded (and, below, connected via `reconnectWorkers`) idle up front too: otherwise
    // tester's still-uncached client conservatively counts against the cap-1 slot planner is
    // about to claim (see `runningWorkerCount`'s doc comment), blocking planner's own resume
    // below before it ever gets a turn.
    testerClient.setRunStateSilently("idle");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async (socketPath) =>
        socketPath === "/state/workers/planner.sock" ? plannerClient : testerClient,
      publishRole: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    // Bring planner into the cache as RUNNING, occupying the cap-1 slot.
    const plannerResume = await processes.spawnWorker(root, root, "planner", "plan #41");
    expect(plannerResume).toEqual({ status: "resumed", roleToken: plannerToken });
    plannerClient.emitRunState("running");

    // Tester's client is already live and idle (it just finished a previous turn) — but the
    // cap has no free slot, so prompting it immediately would flip an uncounted-idle worker to
    // running past config.workerCap. It must queue instead, leaving the live pane and client
    // completely alone.
    testerClient.emitRunState("idle");
    const testerResume = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(testerResume).toEqual({ status: "queued", roleToken: testerToken });
    expect(testerClient.prompts).toEqual([]);
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("tester claim disappeared");
    // Locator left completely alone: no relaunch is ever needed for a still-live idle pane.
    expect(queuedClaim.locator).toBeDefined();
    expect(queuedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-queued", issue: root, role: "tester" }),
    });

    // Planner frees the slot; tester's already-idle, already-cached client is prompted in
    // place — no new tmux command is ever issued for it.
    plannerClient.emitRunState("idle");
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(testerClient.prompts).toEqual(["verify #41"]);
    expect(
      commands.some((command) => command[3] === "split-window" || command[3] === "new-window")
    ).toBeFalse();
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.pendingAssignment).toBeUndefined();
    // The promoted-by-prompt path delivers the architect's assignment exactly as a fresh launch
    // or a direct /worker/ready resume does, and that delivery is the one write of
    // `phases[issue]` — otherwise phase/complete 409s forever for a worker promoted this way.
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("role-lane exceptions for a worker whose assignment is already queued at the cap add no worker-queued and no queue entry", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const plannerClient = fakeWorkerRpcClient();
    plannerClient.setRunStateSilently("idle");
    const testerClient = fakeWorkerRpcClient();
    testerClient.setRunStateSilently("idle");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async (socketPath) =>
        socketPath === "/state/workers/planner.sock" ? plannerClient : testerClient,
      run: async () => ({ stdout: "", exitCode: 0 }),
    });
    await processes.reconnectWorkers();
    expect(await processes.spawnWorker(root, root, "planner", "plan #41")).toEqual({
      status: "resumed",
      roleToken: plannerToken,
    });
    plannerClient.emitRunState("running");
    testerClient.emitRunState("idle");
    const workerQueued = {
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-queued", issue: root, role: "tester" }),
    };
    expect(await processes.spawnWorker(root, root, "tester", "verify #41")).toEqual({
      status: "queued",
      roleToken: testerToken,
    });
    expect(publications.filter((p) => p.json === workerQueued.json)).toEqual([workerQueued]);

    // The listener reports the tester's wake undeliverable twice while its assignment waits.
    await processes.handleException(exception(testerToken));
    await processes.handleException(exception(testerToken));

    expect(publications.filter((p) => p.json === workerQueued.json)).toEqual([workerQueued]);
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("tester claim disappeared");
    expect(queuedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(testerClient.prompts).toEqual([]);
  });

  it("leaves a queued idle-resume assignment intact and stops draining when the promotion prompt itself rejects", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.emitRunState("idle");
    client.prompt = async () => {
      throw new Error("shim write failed");
    };
    const { processes, state, managedState } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);

    // Seed the cached client the decision phase reads from `workerClients` (established via a
    // prior connect, exactly as a real idle-resume queue entry would have one already cached).
    await processes.reconnectWorkers();
    expect(client.prompts).toEqual([]);

    await processes.reconcileWorkerAdmission();

    // The failed prompt never stranded the assignment: the token is still queued (never shifted
    // off for this decision) and its pendingAssignment is untouched (promptExistingWorker only
    // clears it after client.prompt() succeeds).
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.locator).toBeDefined();
    expect(client.prompts).toEqual([]);
  });

  it("clears a dead worker's locator and promotes the queue when its socket closes and one reconnect attempt fails", async () => {
    const clients: Array<{ close(): void }> = [];
    let connectAttempts = 0;
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { processes, managedState, stateDir } = await workerCapFixture(1, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        connectAttempts += 1;
        // The very first connection (workerReady, below) succeeds; the reconnect attempt
        // `onWorkerClientClosed` makes after the socket closes fails, confirming the worker dead.
        if (connectAttempts > 1) throw new Error("ECONNREFUSED");
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
      publishRole: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);

    const plannerClaim = managedState.roles[plannerToken];
    if (!plannerClaim || !("issue" in plannerClaim)) throw new Error("planner claim missing");
    plannerClaim.sessionId = "ses_planner";
    // Simulates /worker/started already having registered this generation's real OMP session
    // file, so markWorkerDead's resumeSessionFile-preservation has something real to preserve.
    if (plannerClaim.locator) {
      plannerClaim.locator.ompSessionFile = path.join(stateDir, "planner-session.json");
    }
    await processes.workerReady(root, "planner", "ses_planner", plannerClaim.generation ?? 1);
    expect(clients).toHaveLength(1);

    clients[0]?.close();
    await promoted;

    // At least 1 (workerReady) + 1 (onWorkerClientClosed's own reconnect attempt) + 1
    // (retireWorkerLocator's own attempt to shut the dead shim down) — all after the first
    // fail. Not an exact count: either planner's or the newly-promoted tester's own boot
    // watchdog may also probe the shim socket for liveness before this point (see
    // `armBootWatchdog`/`probeWorkerAlive`), adding its own connect attempt.
    expect(connectAttempts).toBeGreaterThanOrEqual(3);
    expect(managedState.workerAdmission.queue).toEqual([]);
    const deadPlannerClaim = managedState.roles[plannerToken];
    if (!deadPlannerClaim || !("issue" in deadPlannerClaim)) {
      throw new Error("planner claim disappeared");
    }
    // Locator cleared (not left dangling forever), its ompSessionFile preserved as
    // resumeSessionFile so a later respawn/promotion still resumes the same agent.
    expect(deadPlannerClaim.locator).toBeUndefined();
    expect(deadPlannerClaim.resumeSessionFile).toBe(path.join(stateDir, "planner-session.json"));
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("treats a reconnected socket whose get_state fails as busy, not dead, keeping its locator", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      generation: 1,
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    client.getStateImpl = async () => {
      throw new Error("get_state timed out after 5000ms");
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    expect(client.getStateCalls).toBe(1);
    // The connect itself succeeded; only the follow-up get_state request timed out. Never a
    // reason to kill a live worker — the claim (and its locator) is left exactly as is.
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeDefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(
      commands.some(
        (command) =>
          command[0] === "tmux" && (command[3] === "kill-pane" || command[3] === "kill-window")
      )
    ).toBeFalse();
  });

  it("mints nothing for a queued worker when an idle reconnect fires onIdle before enableLaunches, then promotes it once enabled", async () => {
    const idleToken = roleToken("omp", root, "planner");
    const queuedToken = roleToken("omp", root, "tester");
    let mintCalls = 0;
    const { processes, state, managedState } = await workerCapFixture(
      1,
      {
        connectWorkerRpc: async () => {
          const client = fakeWorkerRpcClient();
          // Simulates the real worker-rpc client's get_state response seeding runState from
          // isStreaming: false — synchronously fires the registered onIdle callback (exactly
          // like a real boot-time reconnect probe would) while still inside reconnectWorkers,
          // before enableLaunches() is called in the real boot sequence.
          client.getStateImpl = async () => {
            client.emitRunState("idle");
            return { isStreaming: false };
          };
          return client;
        },
        mintWorkerBootToken: async () => {
          mintCalls += 1;
          return "worker-boot-token";
        },
      },
      { skipEnableLaunches: true }
    );
    state.roles[idleToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[queuedToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(queuedToken);

    await processes.reconnectWorkers();

    // The onIdle trigger fired mid-reconnect but the gate was still closed: no boot token was
    // ever minted (a launch mid-probe would decide against a running-worker count that still
    // holds every unprobed claim as running), and the queued assignment is untouched.
    expect(mintCalls).toBe(0);
    expect(managedState.workerAdmission.queue).toEqual([queuedToken]);

    processes.enableLaunches();
    await processes.reconcileWorkerAdmission();

    // Now that the gate is open, the exact same queued assignment promotes normally.
    expect(mintCalls).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[queuedToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
  });

  it("goes straight to markWorkerDead on a second close for the same generation, without chaining into a second reconnect", async () => {
    const token = roleToken("omp", root, "tester");
    const clients: ReturnType<typeof fakeWorkerRpcClient>[] = [];
    let connectCalls = 0;
    const { processes, state, managedState } = await workerCapFixture(1, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        connectCalls += 1;
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };

    // Establishes the initial cached connection (connect #1).
    await processes.workerReady(root, "tester", "ses_tester", 1);
    expect(connectCalls).toBe(1);
    const firstClient = clients[0];
    if (!firstClient) throw new Error("first client never connected");

    // First close: onWorkerClientClosed's one-reconnect-attempt succeeds (connect #2) — the
    // worker is reconnected, not yet dead, and this generation's one reconnect credit is spent.
    firstClient.close();
    await Bun.sleep(0);
    await Bun.sleep(0);
    expect(connectCalls).toBe(2);
    expect(managedState.roles[token]).toMatchObject({ locator: { tmuxPaneId: "%7" } });
    const secondClient = clients[1];
    if (!secondClient) throw new Error("reconnect never created a second client");

    // Second close, same generation: goes straight to markWorkerDead instead of attempting yet
    // another reconnect — connect #3 below is only retireWorkerLocator's own best-effort
    // shutdown probe (an unconditional part of confirming death), never a second reconnect
    // attempt. If the one-reconnect-per-generation guard were broken, this would instead chain
    // into a fresh `clientFor` reconnect call before even reaching retirement, landing at
    // connect #4 (or more, looping) instead of settling at exactly 3.
    secondClient.close();
    await Bun.sleep(0);
    await Bun.sleep(0);
    await Bun.sleep(0);

    expect(connectCalls).toBe(3);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
  });

  it("never deletes a newer launch's locator when a stale dead-worker confirmation catches up late", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    const staleLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%7",
      socketPath: "/state/workers/tester.sock",
    };
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: staleLocator,
    };
    const connectGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      // A reconnect attempt for the STALE locator's socket that hangs until the test releases
      // it, then fails — confirming (stale) death only after a newer launch has already
      // replaced this claim's locator.
      connectWorkerRpc: async () => {
        await connectGate.promise;
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const reconnectPromise = processes.reconnectWorkers();

    // While the stale reconnect attempt is still in flight, a fresh launch replaces this
    // token's claim with a genuinely newer locator (a different pane).
    const freshLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%99",
      socketPath: "/state/workers/tester.sock",
    };
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      pendingAssignment: {
        kind: "assignment",
        task: "verify #55",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: freshLocator,
    };

    connectGate.resolve();
    await reconnectPromise;

    // The stale confirmation must never delete the newer launch's locator, nor kill its pane.
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toEqual(freshLocator);
    expect(claim.generation).toBe(2);
    expect(
      commands.some(
        (command) =>
          command[0] === "tmux" && (command[3] === "kill-pane" || command[3] === "kill-window")
      )
    ).toBeFalse();
  });

  it("retires an unlaunchable queue head at MAX_LAUNCH_FAILURES instead of blocking the queue forever", async () => {
    const failingToken = roleToken("omp", root, "planner");
    const okToken = roleToken("omp", root, "tester");
    const records: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { processes, state, managedState, stateDir } = await workerCapFixture(2, {
      publishRole: (subject, json) => {
        records.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });
    state.roles[failingToken] = {
      issue: root,
      role: "planner",
      pendingAssignment: {
        kind: "assignment",
        task: "plan #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      // A deterministic, permanent failure (missing session file, never appears on retry) — the
      // same mechanism "fails a worker respawn loudly..." above exercises directly.
      resumeSessionFile: path.join(stateDir, "missing-planner-session.json"),
      launchFailures: 2,
    };
    state.roles[okToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(failingToken, okToken);

    await processes.reconcileWorkerAdmission();
    await promoted;

    // The permanently-failing head never blocked tester behind it: both were drained in the
    // same pass, one retired at the failure threshold, the other launched.
    expect(managedState.workerAdmission.queue).toEqual([]);
    const failingClaim = managedState.roles[failingToken];
    if (!failingClaim || !("issue" in failingClaim)) throw new Error("planner claim missing");
    expect(failingClaim.launchFailures).toBe(3);
    expect(failingClaim.locator).toBeUndefined();
    const okClaim = managedState.roles[okToken];
    if (!okClaim || !("issue" in okClaim)) throw new Error("tester claim missing");
    expect(okClaim.locator).toBeDefined();
    expect(records).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "launch-failed", issue: root, role: "planner", failures: 3 }),
    });
    // launch-failed publishes exactly once, at the exact tick the threshold is crossed — not
    // again on some later, otherwise-nonexistent retry of the now-retired token.
    expect(records.filter((record) => record.json.includes("launch-failed"))).toHaveLength(1);
  });

  it("rotates a below-threshold launch failure to the tail instead of blocking the queue behind it", async () => {
    // A reviewer outranks a tester (`workerPriority`), so the failing token is the head the drain
    // attempts first; the point of the test is what happens to the token behind it.
    const failingToken = roleToken("omp", root, "reviewer");
    const okToken = roleToken("omp", root, "tester");
    const { processes, state, managedState, stateDir } = await workerCapFixture(1);
    state.roles[failingToken] = {
      issue: root,
      role: "reviewer",
      pendingAssignment: {
        kind: "assignment",
        task: "review #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      // A deterministic, permanent failure (missing session file, never appears on retry) — but
      // launchFailures starts at 0, so one attempt stays *below* MAX_LAUNCH_FAILURES (3).
      resumeSessionFile: path.join(stateDir, "missing-reviewer-session.json"),
    };
    state.roles[okToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(failingToken, okToken);

    // Cap 1: once okToken successfully launches (occupying the only slot), the drain loop's own
    // cap check stops it from immediately re-attempting the rotated failingToken again in the
    // same pass — isolating this test to exactly one below-threshold rotation.
    await processes.reconcileWorkerAdmission();

    // failingToken failed once (below threshold), was rotated to the tail rather than left
    // blocking the head, and okToken — now at the head — launched successfully in the same pass.
    expect(managedState.workerAdmission.queue).toEqual([failingToken]);
    const failingClaim = managedState.roles[failingToken];
    if (!failingClaim || !("issue" in failingClaim)) throw new Error("reviewer claim missing");
    expect(failingClaim.launchFailures).toBe(1);
    expect(failingClaim.locator).toBeUndefined();
    const okClaim = managedState.roles[okToken];
    if (!okClaim || !("issue" in okClaim)) throw new Error("tester claim missing");
    expect(okClaim.locator).toBeDefined();
  });

  it("promotes work that finishes an open pull request before work that opens a new one, whatever order it was queued in", async () => {
    // Queued in the worst arrival order: a fresh implementer (no PR yet) first, then the phases
    // that would finish existing PRs. One slot: exactly one token launches per drain, so the
    // order the queue is left in is the order the daemon will run them.
    const otherRoot = "LEGION-99";
    const newImplementer = roleToken("omp", root, "implementer");
    const tester = roleToken("omp", otherRoot, "tester");
    const reviewer = roleToken("omp", root, "reviewer");
    const merger = roleToken("omp", otherRoot, "merger");
    const { processes, state, managedState } = await workerCapFixture(1);
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    for (const [token, issue, role] of [
      [newImplementer, root, "implementer"],
      [tester, otherRoot, "tester"],
      [reviewer, root, "reviewer"],
      [merger, otherRoot, "merger"],
    ] as const) {
      state.roles[token] = {
        issue,
        role,
        pendingAssignment: {
          kind: "assignment",
          task: role,
          queuedAt: "2026-08-24T00:00:00.000Z",
          deliveryId: TEST_DELIVERY_ID,
        },
      };
      state.workerAdmission.queue.push(token);
    }

    await processes.reconcileWorkerAdmission();

    const mergerClaim = managedState.roles[merger];
    if (!mergerClaim || !("issue" in mergerClaim)) throw new Error("merger claim missing");
    expect(mergerClaim.locator).toBeDefined();
    expect(managedState.workerAdmission.queue).toEqual([reviewer, tester, newImplementer]);
  });

  it("ranks an implementer whose issue already has a pull request with the finishing work, ahead of planners and fresh implementers", async () => {
    const otherRoot = "LEGION-99";
    const correctiveImplementer = roleToken("omp", root, "implementer");
    const planner = roleToken("omp", otherRoot, "planner");
    const { processes, state, managedState } = await workerCapFixture(1);
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.prs["acme/widgets#7"] = checkPr(root, { number: 7 });
    for (const [token, issue, role] of [
      [planner, otherRoot, "planner"],
      [correctiveImplementer, root, "implementer"],
    ] as const) {
      state.roles[token] = {
        issue,
        role,
        pendingAssignment: {
          kind: "assignment",
          task: role,
          queuedAt: "2026-08-24T00:00:00.000Z",
          deliveryId: TEST_DELIVERY_ID,
        },
      };
      state.workerAdmission.queue.push(token);
    }

    await processes.reconcileWorkerAdmission();

    const implementerClaim = managedState.roles[correctiveImplementer];
    if (!implementerClaim || !("issue" in implementerClaim)) throw new Error("claim missing");
    expect(implementerClaim.locator).toBeDefined();
    expect(managedState.workerAdmission.queue).toEqual([planner]);
  });

  it("retries a token that already failed to launch behind every clean token, whatever its tier", async () => {
    const otherRoot = "LEGION-99";
    const retryingMerger = roleToken("omp", root, "merger");
    const cleanImplementer = roleToken("omp", otherRoot, "implementer");
    const { processes, state, managedState } = await workerCapFixture(1);
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.roles[retryingMerger] = {
      issue: root,
      role: "merger",
      pendingAssignment: {
        kind: "assignment",
        task: "merge #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      launchFailures: 1,
    };
    state.roles[cleanImplementer] = {
      issue: otherRoot,
      role: "implementer",
      pendingAssignment: {
        kind: "assignment",
        task: "implement #99",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    // Arrival order and tier both favour the merger; its unresolved launch failure decides.
    state.workerAdmission.queue.push(retryingMerger, cleanImplementer);

    await processes.reconcileWorkerAdmission();

    const cleanClaim = managedState.roles[cleanImplementer];
    if (!cleanClaim || !("issue" in cleanClaim)) throw new Error("implementer claim missing");
    expect(cleanClaim.locator).toBeDefined();
    expect(managedState.workerAdmission.queue).toEqual([retryingMerger]);
  });

  it("stops the drain after one below-threshold failure on a single-item queue instead of burning every MAX_LAUNCH_FAILURES attempt in one pass", async () => {
    const failingToken = roleToken("omp", root, "planner");
    let launchAttempts = 0;
    const { processes, state, managedState, stateDir } = await workerCapFixture(1, {
      run: async (command) => {
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
          launchAttempts += 1;
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    state.roles[failingToken] = {
      issue: root,
      role: "planner",
      pendingAssignment: {
        kind: "assignment",
        task: "plan #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      // A deterministic, permanent failure (missing session file, never appears on retry) — but
      // launchFailures starts at 0, so one attempt stays *below* MAX_LAUNCH_FAILURES (3). This
      // queue holds ONLY this one token — a rotate-in-place (shift then push, same array)
      // re-exposes the exact same head, which is the scenario the fix targets.
      resumeSessionFile: path.join(stateDir, "missing-planner-session.json"),
    };
    state.workerAdmission.queue.push(failingToken);

    await processes.reconcileWorkerAdmission();

    // A single below-threshold rotation on a one-item queue exposes the SAME head again — the
    // drain must stop there instead of looping straight back into it, which would otherwise
    // burn all 3 MAX_LAUNCH_FAILURES attempts in this one synchronous pass instead of leaving
    // the retry to the periodic sweep or the next idle/dead event.
    expect(launchAttempts).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([failingToken]);
    const failingClaim = managedState.roles[failingToken];
    if (!failingClaim || !("issue" in failingClaim)) throw new Error("planner claim missing");
    expect(failingClaim.launchFailures).toBe(1);
    expect(failingClaim.locator).toBeUndefined();
  });

  it("counts a sub-architect claim toward the cap even after its issue graduates into its own tree root", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 2);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    // The child issue has since graduated into its own tree root: rootForIssue(child) now
    // resolves to `child` itself, not `root` — exactly the case the old root-architect exclusion
    // clause in runningWorkerCount misfired on, undercounting this claim and over-admitting.
    state.trees[child] = {
      root: child,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const subArchitectToken = roleToken("omp", child, "architect");
    state.roles[subArchitectToken] = {
      issue: child,
      role: "architect",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%99",
        socketPath: "/state/workers/sub-architect.sock",
      },
    };

    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    const result = await processes.spawnWorker(root, root, "planner", "plan #41");

    expect(result.status).toBe("queued");
    expect(managedState.workerAdmission.queue).toEqual([roleToken("omp", root, "planner")]);
  });

  it("does not let a slow launch block a concurrent different-role admission decision", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    // A different issue from the tree root, so the two spawns below never contend on the
    // per-issue `issueLaunchQueue` window-creation serialization — only on `admissionLock`.
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const plannerGate = Promise.withResolvers<void>();
    const { manager: processes } = manager(state, {
      config: config(stateDir, { workerCap: 2 }),
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[3] === "split-window" &&
          command.includes("LEGION_ROLE=planner")
        ) {
          // Held open for the whole test: proves the admission decision for a different role
          // does not wait on this launch, since the lock scope no longer wraps launchWorker.
          await plannerGate.promise;
          return { stdout: "%201 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@43 %202 23456\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const plannerPromise = processes.spawnWorker(root, root, "planner", "plan #41");
    const testerPromise = processes.spawnWorker(root, child, "tester", "verify #43");

    const testerResult = await testerPromise;
    expect(testerResult.status).toBe("spawned");

    plannerGate.resolve();
    const plannerResult = await plannerPromise;
    expect(plannerResult.status).toBe("spawned");
  });

  it("a concurrent admission decision during the post-locator-write, pre-saveState window sees exactly one occupied slot, not two, and only releases/re-checks the queue after the save settles", async () => {
    const saveStateCalled = Promise.withResolvers<void>();
    const releaseSave = Promise.withResolvers<void>();
    let saveStateCalls = 0;
    const sequence: string[] = [];
    const plannerToken = roleToken("omp", root, "planner");
    const { processes, managedState } = await workerCapFixture(2, {
      saveState: async () => {
        saveStateCalls += 1;
        // Only planner's own post-locator-write save is held open. `launchWorker` writes the
        // claim's locator (in memory) *before* calling this, and the caller's `finally` — which
        // releases planner's `launching` reservation and re-checks the queue — only runs once
        // this whole `launchWorker` call (including this save) resolves. During this exact
        // window planner's reservation is still held in `this.launching` AND its locator is
        // already written — `runningWorkerCount()` must count that as one occupied slot, not
        // two.
        if (saveStateCalls === 1) {
          saveStateCalled.resolve();
          await releaseSave.promise;
          sequence.push("saveState resolved");
        }
      },
      // Test-only hook (see `WorkerAdmissionDeps.onAdmissionEvent`'s doc comment): records the
      // exact moment `launchOrQueue`'s own `finally` releases planner's reservation and
      // triggers the queue re-check, so this test can assert both happen strictly after the
      // gated save resolves — never during, and never because of the concurrent tester
      // decision racing in below.
      onAdmissionEvent: (token, event) => {
        if (token !== plannerToken) return;
        sequence.push(event === "reservation-released" ? "release" : "promote");
      },
    });

    const plannerPromise = processes.spawnWorker(root, root, "planner", "plan #41");
    await saveStateCalled.promise;

    // Racing in right as planner's locator write has landed but before its saveState — and
    // therefore the release of its own `launching` reservation — has settled: at cap 2 this
    // must LAUNCH, not queue. A formula that sums `this.launching` unconditionally alongside
    // every locator-bearing claim would double-count planner here (reserved *and* located) and
    // wrongly report both of cap 2's slots already occupied, queuing this tester instead.
    const testerResult = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(testerResult.status).toBe("spawned");

    // Still gated: planner's own release/promote must not have fired yet, regardless of the
    // concurrent tester decision above (that decision never touches planner's reservation).
    expect(sequence).toEqual([]);

    releaseSave.resolve();
    const plannerResult = await plannerPromise;
    expect(plannerResult.status).toBe("spawned");
    expect(managedState.workerAdmission.queue).toEqual([]);
    // Release and the queue-drain trigger both fire exactly once, strictly after the gated
    // save resolves — never before it, and never more than once.
    expect(sequence).toEqual(["saveState resolved", "release", "promote"]);
  });

  it("retires the pane instead of writing a claim when closeTree closes the tree while a launch is still in flight", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      // The just-opened tester never gets its shim listening before the close reaches it, so
      // its retire falls through to the kill; the root closes gracefully over its own socket.
      connectWorkerRpc: async (socketPath) => {
        if (path.basename(socketPath).startsWith("tester-")) throw new Error("ECONNREFUSED");
        return fakeWorkerRpcClient();
      },
      run: async (command) => {
        commands.push(command);
        // The tester splits into the root's own already-alive window (from `tree()`), rather
        // than opening a fresh one.
        if (command[0] === "tmux" && command[3] === "split-window") {
          // Held open until closeTree below has actually started -- proves launchWorker
          // re-checks after this I/O, not before it.
          await launchGate.promise;
          return { stdout: "%1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, root, "tester", "verify #41");
    // Poll until the launch has actually reached its blocked `split-window` call before racing:
    // `inFlightLaunches` makes `closeTree` await this exact decision before concluding the
    // tree is empty, so starting the race any earlier would just serialize the two calls.
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[3] === "split-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[3] === "split-window")).toBe(true);

    const closePromise = processes.closeTree(root);
    launchGate.resolve();
    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    await closePromise;

    // The just-opened pane is retired and reported as a closing error, not a silent success or
    // a launch failure -- `spawnWorker`/`launchWorker` never write a claim for a tree that has
    // already started tearing down.
    expect(spawnResult).toBeInstanceOf(TreeClosingError);
    expect(managedState.trees[root]?.status).toBe("closed");
    // No zombie claim was ever written for a tree that closed mid-launch, and the pane this
    // launch just opened was retired (killed), not left running unrecorded and forever
    // occupying a running-worker slot.
    expect(managedState.roles[roleToken("omp", root, "tester")]).toBeUndefined();
    expect(commands).toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
  });

  it("keeps closeTree from finishing while a QUEUE-PROMOTED launch is still in flight, not only a direct spawnWorker one", async () => {
    // Without the constructor's `trackLaunch` wrap on the `launchWorker` dependency
    // `WorkerAdmission`'s own queue-drain calls directly (`promoteQueuedWorker`, bypassing
    // `spawnWorker`'s own `trackLaunch` entirely), closeTree's fixed-point loop would see an
    // empty `state.roles` snapshot (this promoted launch has not written its claim yet), finish,
    // and clear `closingTrees` -- all while this exact promotion is still blocked mid-flight.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const testerToken = roleToken("omp", root, "tester");
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(testerToken);

    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      run: async (command) => {
        commands.push(command);
        // The tester splits into the root's own already-alive window (from `tree()`) — held
        // open until closeTree below has had every real chance to finish, proving the PROMOTED
        // launch (not just a direct spawnWorker call) is what `inFlightLaunches` awaits.
        if (command[0] === "tmux" && command[3] === "split-window") {
          await launchGate.promise;
          return { stdout: "%1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    // Nothing else occupies the cap, so this promotes immediately through WorkerAdmission's own
    // queue-drain, never through `spawnWorker`.
    const drainPromise = processes.reconcileWorkerAdmission();

    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[3] === "split-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[3] === "split-window")).toBe(true);

    let closeSettled = false;
    const closePromise = processes.closeTree(root).then(() => {
      closeSettled = true;
    });

    // Give closeTree every real chance to run its (otherwise-empty) worker loop and finish while
    // the promoted launch above is still blocked on `split-window`.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(closeSettled).toBe(false);

    launchGate.resolve();
    await drainPromise;
    await closePromise;

    expect(closeSettled).toBe(true);
    expect(managedState.trees[root]?.status).toBe("closed");
    // No zombie claim was ever left for a tree that closed mid-promotion, and the promoted
    // launch's own queue entry does not survive a close it never got the chance to run inside.
    expect(managedState.roles[testerToken]).toBeUndefined();
    expect(managedState.workerAdmission.queue).toEqual([]);
    // Exactly the one pane this launch opened -- no second pane from a retry treating the
    // TreeClosingError like an ordinary launch failure and rotating/relaunching this token.
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "split-window")
    ).toHaveLength(1);
  });

  it("re-evaluates a persisted running-worker queue against a raised cap across a restart", async () => {
    const testerToken = roleToken("omp", root, "tester");
    const records: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { processes, state, managedState } = await workerCapFixture(2, {
      publishRole: (subject, json) => {
        records.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(testerToken);

    await processes.reconcileWorkerAdmission();
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
    expect(records).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("admits exactly one of two concurrent different-role spawns racing for the last slot", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    const [plannerResult, testerResult] = await Promise.all([
      processes.spawnWorker(root, root, "planner", "plan #41"),
      processes.spawnWorker(root, root, "tester", "verify #41"),
    ]);

    expect([plannerResult.status, testerResult.status].sort()).toEqual(["queued", "spawned"]);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toHaveLength(1);
    const plannerClaim = managedState.roles[roleToken("omp", root, "planner")];
    const testerClaim = managedState.roles[roleToken("omp", root, "tester")];
    if (!plannerClaim || !("issue" in plannerClaim) || !testerClaim || !("issue" in testerClaim)) {
      throw new Error("both worker claims must be recorded");
    }
    const spawnedClaim = plannerResult.status === "spawned" ? plannerClaim : testerClaim;
    const queuedClaim = plannerResult.status === "spawned" ? testerClaim : plannerClaim;
    expect(spawnedClaim.locator).toBeDefined();
    expect(queuedClaim.locator).toBeUndefined();
    expect(managedState.workerAdmission.queue).toEqual([
      plannerResult.status === "queued"
        ? roleToken("omp", root, "planner")
        : roleToken("omp", root, "tester"),
    ]);
  });

  it("promotes exactly one queued worker when two running workers go idle in the same tick", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const clients: Array<{ emitRunState(state: "running" | "idle"): void }> = [];
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 2 }),
      connectWorkerRpc: async () => {
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
      publishRole: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "implementer", "implement #41");
    const testerResult = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(testerResult).toEqual({ status: "queued", roleToken: roleToken("omp", root, "tester") });

    for (const role of ["planner", "implementer"] as const) {
      const token = roleToken("omp", root, role);
      const claim = managedState.roles[token];
      if (!claim || !("issue" in claim)) throw new Error(`${role} claim missing`);
      claim.sessionId = `ses_${role}`;
      await processes.workerReady(root, role, `ses_${role}`, claim.generation ?? 1);
    }
    expect(clients).toHaveLength(2);

    // Both running workers finish their turn in the same synchronous tick, racing to promote the
    // single queued tester. The admission lock's inside-the-critical-section re-peek of the queue
    // head must let only the first through; the second must see the head already moved on.
    clients[0]?.emitRunState("idle");
    clients[1]?.emitRunState("idle");
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toHaveLength(1);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[3] === "split-window")
    ).toHaveLength(2);
    const testerClaim = managedState.roles[roleToken("omp", root, "tester")];
    if (!testerClaim || !("issue" in testerClaim)) throw new Error("tester claim missing");
    expect(testerClaim.locator).toBeDefined();
    expect(
      publications.filter(
        (publication) =>
          publication.subject === architectTopic && publication.json.includes("worker-started")
      )
    ).toHaveLength(1);
  });

  it("refuses to enqueue a spawn against an already-closed tree, even at cap, instead of queueing a locator-less claim nothing would ever prune", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const treeState = state.trees[root];
    if (!treeState) throw new Error("tree missing");
    // Already fully closed -- not merely closing (`closingTrees` is empty here, unlike the
    // mid-teardown test below): the entry check must catch this via the SAME `isTreeGone`
    // predicate `launchWorker` itself uses, not just `closingTrees.has`.
    treeState.status = "closed";
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 0 }),
    });

    const error = await processes
      .spawnWorker(root, root, "tester", "verify #41")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(TreeClosingError);
    // Never enqueued: before this fix, a spawn against an already-closed tree at cap would still
    // reach `launchOrQueue` and push a locator-less claim that nothing left running would ever
    // prune (`pruneQueueForTree` only runs from inside an active `closeTreeLocked` call).
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(managedState.roles[roleToken("omp", root, "tester")]).toBeUndefined();
  });

  it("drops a queued token whose tree is already closed instead of leaving it wedged at the head forever, then promotes the next queued token", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const treeState = state.trees[root];
    if (!treeState) throw new Error("tree missing");
    treeState.status = "closed";
    // Models a token that was validly queued before its tree closed (or a legacy pre-fix wedge):
    // locator-less, still carrying a pending assignment, sitting at the head of the FIFO queue.
    const deadToken = roleToken("omp", root, "tester");
    state.roles[deadToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };

    const otherRoot = "LEGION-99";
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const liveToken = roleToken("omp", otherRoot, "planner");
    state.roles[liveToken] = {
      issue: otherRoot,
      role: "planner",
      pendingAssignment: {
        kind: "assignment",
        task: "plan #99",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(deadToken, liveToken);

    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    await processes.reconcileWorkerAdmission();

    // The dead entry never blocks the live one behind it: dropped with no failure accounting
    // (not rotated to the tail, not left wedged at the head for every future drain to retry and
    // fail identically), and the queue is now empty because the live token was promoted in the
    // same pass.
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(managedState.roles[deadToken]).toBeUndefined();
    const liveClaim = managedState.roles[liveToken];
    if (!liveClaim || !("issue" in liveClaim)) throw new Error("live claim missing");
    expect(liveClaim.locator).toBeDefined();
  });

  it("prunes every spawn capability recorded for a tree once it closes, so a pre-shutdown spawn token can no longer authorize the legacy role-backing route into recreating a claim", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const closingSpawnToken = "spawn-token-for-closing-tree";
    state.spawnCapabilities[spawnCapabilityKey(closingSpawnToken)] = {
      tree: root,
      issue: root,
      role: "tester",
    };
    const otherRoot = "LEGION-77";
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const unrelatedSpawnToken = "spawn-token-for-a-different-tree";
    state.spawnCapabilities[spawnCapabilityKey(unrelatedSpawnToken)] = {
      tree: otherRoot,
      issue: otherRoot,
      role: "planner",
    };
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
    });

    await processes.closeTree(root);

    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.spawnCapabilities[spawnCapabilityKey(closingSpawnToken)]).toBeUndefined();
    // A capability for an unrelated, still-active tree must survive this tree's own close.
    expect(managedState.spawnCapabilities[spawnCapabilityKey(unrelatedSpawnToken)]).toEqual({
      tree: otherRoot,
      issue: otherRoot,
      role: "planner",
    });
  });

  it("refuses to spawn or resume a worker on a tree that is mid-teardown, with a 409-mapped TreeClosingError", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const stuckClient = fakeWorkerRpcClient();
    stuckClient.shutdown = () => {};
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => stuckClient,
    });

    // Not awaited: `closeTree` runs synchronously up to its first internal await (inside
    // `probe`), so `closingTrees` is already populated by the time this line returns control.
    const closePromise = processes.closeTree(root);

    const error = await processes
      .spawnWorker(root, root, "tester", "verify #41")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(TreeClosingError);
    expect((error as TreeClosingError).treeKey).toBe(root);

    await closePromise;
  });

  it("refuses to deliver a worker/ready queued assignment on a tree that is mid-teardown", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.roles[roleToken("omp", root, "tester")] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const stuckClient = fakeWorkerRpcClient();
    stuckClient.shutdown = () => {};
    const readyClient = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async (socketPath) =>
        socketPath === "/state/workers/tester.sock" ? readyClient : stuckClient,
    });

    const closePromise = processes.closeTree(root);

    const error = await processes
      .workerReady(root, "tester", "ses_tester", 1)
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(TreeClosingError);
    expect((error as TreeClosingError).treeKey).toBe(root);
    expect(readyClient.prompts).toEqual([]);

    await closePromise;
  });

  it("rejects mutateLiveRoleClaim (the fence /worker/started's own claim write runs behind) once closeTree has already deleted this token's claim under the same per-role lock", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const { manager: processes, state: managedState } = manager(state, { sleep: async () => {} });

    await processes.closeTree(root);
    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.roles[token]).toBeUndefined();

    let fnCalled = false;
    const error = await processes
      .mutateLiveRoleClaim(root, root, token, async () => {
        fnCalled = true;
      })
      .catch((caught: unknown) => caught);

    // Rejected at entry -- before ever touching the per-role lock or `fn` -- exactly like
    // spawnWorker/workerReady already do for a tree that is closed by the time the request
    // arrives, not merely one still tearing down.
    expect(error).toBeInstanceOf(TreeClosingError);
    expect((error as TreeClosingError).treeKey).toBe(root);
    expect(fnCalled).toBe(false);
  });

  it("rejects mutateLiveRoleClaim queued behind an in-progress closeTree for the same token, never running fn once that close has deleted the claim", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const stopGate = Promise.withResolvers<void>();
    const client = fakeWorkerRpcClient();
    client.shutdown = () => {
      void stopGate.promise.then(() => client.close());
    };
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => client,
    });

    // closeTree marks `closingTrees` synchronously as its very first act, before ever touching
    // this token's own critical section (its stop-then-delete only follows once the gated
    // `shutdown()` below resolves) -- so a call arriving any time after this line, on this same
    // per-token lock `closeTreeLocked`'s delete also uses, is rejected by the entry check alone,
    // consistent with every other write this fence protects (`spawnWorker`, `workerReady`).
    const closePromise = processes.closeTree(root);

    let fnCalled = false;
    const mutatePromise = processes
      .mutateLiveRoleClaim(root, root, token, async () => {
        fnCalled = true;
      })
      .catch((caught: unknown) => caught);

    stopGate.resolve();
    const error = await mutatePromise;
    await closePromise;

    expect(error).toBeInstanceOf(TreeClosingError);
    expect(fnCalled).toBe(false);
    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.roles[token]).toBeUndefined();
  });

  it("delivers a worker's pending assignment over its socket on worker/ready, clears it, and resets launchFailures", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      // A prior generation's unconfirmed boot(s) left this behind; a durable ready
      // confirmation is the only thing that ever resets it (never mere registration).
      launchFailures: 2,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual(["verify #41"]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.launchFailures).toBeUndefined();
    // Same as the resumed-live-socket branch: delivering the architect's assignment from
    // `pendingAssignment` at ready is the one write of `phases[issue]`, or the worker's eventual
    // `handoff complete` 409s against a phase this delivery path never restored.
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("resets launchFailures once worker/ready durably confirms a claim with no pending assignment", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      launchFailures: 2,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.readyConfirmedAt).toBeNumber();
    expect(claim.launchFailures).toBeUndefined();
  });
  it("serializes concurrent worker/ready calls so the pending assignment prompts once", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const originalPrompt = client.prompt;
    const promptStarted = Promise.withResolvers<void>();
    const releasePrompt = Promise.withResolvers<void>();
    client.prompt = async (task, deliveryId) => {
      promptStarted.resolve();
      await releasePrompt.promise;
      return originalPrompt(task, deliveryId);
    };
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    const first = processes.workerReady(root, "tester", "ses_tester", 1);
    await promptStarted.promise;
    const second = processes.workerReady(root, "tester", "ses_tester", 1);
    releasePrompt.resolve();
    await Promise.all([first, second]);

    expect(client.prompts).toEqual(["verify #41"]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.readyConfirmedAt).toBeNumber();
  });

  it("ignores worker/ready from a stale generation even when the session id matches, leaving pendingAssignment intact", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #55",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    // A same-agent respawn keeps the session id, so a late worker/ready from the replaced
    // (generation 1) process must not be able to consume generation 2's pending assignment.
    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #55",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("treats a saveState failure after a successful prompt as a persistence issue, not a prompt failure: keeps the delivered phase and cleared pendingAssignment regardless", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    let saveStateCalls = 0;
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
      saveState: async () => {
        saveStateCalls += 1;
        // Both the original persist and its one retry fail here — mirroring launchWorker's
        // post-locator-write save handling (see the "treats a saveState failure after a
        // successful launch..." test above): the prompt itself already succeeded (the worker
        // is already working), so this must never surface as a failure to the caller.
        throw new Error("disk full");
      },
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual(["verify #41"]);
    expect(saveStateCalls).toBe(2);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    // The in-memory claim remains authoritative even though both persist attempts failed: the
    // phase written by the delivered architect assignment and the cleared pendingAssignment
    // stay exactly as the successful prompt left them, never rolled back.
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.promptFailures).toBe(0);
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("reconnects to every worker claim with a locator on daemon start", async () => {
    const state = newLegionState("omp", 1);
    state.roles[roleToken("omp", root, "planner")] = {
      issue: root,
      role: "planner",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[roleToken("omp", root, "tester")] = {
      issue: root,
      role: "tester",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const connectedSockets: string[] = [];
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return fakeWorkerRpcClient();
      },
    });

    await processes.reconnectWorkers();

    expect(connectedSockets.sort()).toEqual(
      ["/state/workers/planner.sock", "/state/workers/tester.sock"].sort()
    );
  });

  it("does not count an idle worker (isStreaming: false) toward the running-worker cap after reconnectWorkers", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    tree(state);
    const idleToken = roleToken("omp", root, "planner");
    state.roles[idleToken] = {
      issue: root,
      role: "planner",
      sessionId: "ses_planner",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    const idleClient = fakeWorkerRpcClient();
    // Models the real client's getState(): interprets a false isStreaming into an idle
    // transition, exactly as reconnectWorkers relies on to seed runState from a fresh
    // reconnection instead of leaving it stuck at the conservative "unknown" default.
    idleClient.getStateImpl = async () => {
      idleClient.emitRunState("idle");
      return { data: { isStreaming: false } };
    };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => idleClient,
    });

    await processes.reconnectWorkers();
    expect(idleClient.getStateCalls).toBe(1);
    expect(idleClient.runState).toBe("idle");

    // The cap is 1 and the only existing claim is idle (not counted by runningWorkerCount): a
    // fresh spawn for a different role must be admitted immediately, not queued behind a
    // phantom occupant.
    const result = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(result.status).toBe("spawned");
    expect(
      commands.some((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toBeTrue();
  });

  it("does not count a retired claim (locator cleared, resumeSessionFile kept) toward the running-worker cap", async () => {
    const { processes, state, commands, stateDir } = await workerCapFixture(1);
    await mkdir(path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42"), {
      recursive: true,
    });
    // Exactly the shape an idle retirement (or markWorkerDeadLocked) leaves behind.
    state.roles[roleToken("omp", root, "implementer")] = {
      issue: root,
      role: "implementer",
      sessionId: "ses_implementer",
      generation: 1,
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      resumeSessionFile: path.join(stateDir, "implementer-session.jsonl"),
    };

    const result = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(result).toEqual({ status: "spawned", roleToken: roleToken("omp", root, "tester") });
    expect(
      commands.some((command) => command[0] === "tmux" && command[3] === "new-window")
    ).toBeTrue();
    expect(state.workerAdmission.queue).toEqual([]);
  });

  it.each<[string, LegionState["phases"][IssueKey]]>([
    ["absent", undefined],
    [
      "completed",
      {
        phase: "implementer",
        sessionId: "ses_implementer",
        completed: { summary: "done", at: "2026-08-24T00:00:00.000Z" },
      },
    ],
    ["naming another role", { phase: "tester", sessionId: "ses_tester" }],
  ])("retires a confirmed idle worker past the idle window when phases[issue] is %s: one graceful stop, locator cleared, claim kept with its session file, no failure counted, no worker-died", async (_shape, phase) => {
    const worker = await idleWorkerFixture({
      role: "implementer",
      phases: phase ? { [root]: phase } : undefined,
    });
    expect(worker.client.runState).toBe("idle");
    // Armed by the idle transition reconnectWorkers' get_state seeded.
    expect(worker.clock.pending.some((wait) => wait.ms === 600_000)).toBeTrue();
    expect(worker.shutdownCalls).toEqual([]);

    // The retirement's own persist is the event; its socket-close tail (the reconnect probe and
    // the queued no-op markWorkerDead) is microtask-only and settles inside `next()`'s deferral.
    const retired = worker.saves.completed.next();
    expect(worker.clock.fire(600_000)).toBeTrue();
    await retired;

    expect(worker.shutdownCalls).toEqual([worker.token]);
    expect(
      worker.commands.filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
    ).toEqual([]);
    expect(worker.claim()).toMatchObject({
      issue: root,
      role: "implementer",
      sessionId: "ses_implementer",
      generation: 1,
      resumeSessionFile: worker.sessionFile,
    });
    expect(worker.claim().locator).toBeUndefined();
    expect(worker.claim().launchFailures).toBeUndefined();
    expect(worker.claim().promptFailures).toBeUndefined();
    // The stop's own socket close ran the dead-worker path: exactly one reconnect probe (the
    // fixture's second connect, refused like an exited shim), then markWorkerDeadLocked's
    // locator-identity re-check found the locator already cleared and did nothing further — one
    // revoke, no publish of any kind.
    expect(worker.connectAttempts()).toBe(2);
    expect(worker.revokedSessions).toEqual(["ses_implementer"]);
    expect(worker.publications).toEqual([]);
  });

  it("leaves a confirmed idle worker untouched while its idle window is still running", async () => {
    const worker = await idleWorkerFixture({ role: "implementer" });
    const seededLocator = structuredClone(worker.claim().locator);

    // Negative wait: nothing fires -- the armed clock stays pending in manualSleep.
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.revokedSessions).toEqual([]);
    // Armed and waiting: one clock, not yet fired.
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(1);
  });

  it("leaves a worker re-prompted inside the idle window untouched, and its next idle transition arms a fresh clock that does retire it", async () => {
    const worker = await idleWorkerFixture({ role: "implementer" });
    const seededLocator = structuredClone(worker.claim().locator);
    // What `prompt()` does synchronously before the request is even sent.
    worker.client.emitRunState("running");

    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the fire reaches only retireIdleWorker's runState check (client running).
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.revokedSessions).toEqual([]);
    expect(worker.connectAttempts()).toBe(1);
    // A running decline does not re-arm: this worker's own next idle transition is what arms.
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(0);

    // The turn ends: a fresh clock is armed, and this one is live — the first arm's map entry does
    // not block it.
    worker.client.emitRunState("idle");
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(1);
    const retired = worker.saves.completed.next();
    expect(worker.clock.fire(600_000)).toBeTrue();
    await retired;

    expect(worker.shutdownCalls).toEqual([worker.token]);
  });

  it("ignores a superseded clock's expiry: after idle -> running -> idle, firing the older wait does nothing and firing the newer one retires", async () => {
    // Under real timers `createCancellableSleep.cancel()` RESOLVES the sleep it cancels, so a
    // re-armed clock's predecessor fires at once; only the entry-identity check in armIdleRetire's
    // expiry keeps that stale fire from retiring a worker that just went idle. This pins it.
    const worker = await idleWorkerFixture({ role: "implementer" });
    const seededLocator = structuredClone(worker.claim().locator);
    worker.client.emitRunState("running");
    worker.client.emitRunState("idle");
    // Two clocks recorded: the superseded one (never consumed) and the live re-arm.
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(2);

    // Oldest first: the superseded clock expires.
    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the stale fire reaches only armIdleRetire's wait-identity check.
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.revokedSessions).toEqual([]);
    expect(worker.connectAttempts()).toBe(1);
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(1);

    // The live clock expires: exactly one retirement.
    const retired = worker.saves.completed.next();
    expect(worker.clock.fire(600_000)).toBeTrue();
    await retired;

    expect(worker.shutdownCalls).toEqual([worker.token]);
    expect(worker.claim().resumeSessionFile).toBe(worker.sessionFile);
    expect(worker.publications).toEqual([]);
  });

  it("ignores an armed clock's expiry after dispose(): nothing is stopped and no clock is re-armed", async () => {
    const worker = await idleWorkerFixture({ role: "implementer" });
    const seededLocator = structuredClone(worker.claim().locator);
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(1);

    worker.manager.dispose();
    // dispose() cannot un-record the wait under an injected sleep; the fire still reaches the code.
    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the fire reaches only retireIdleWorker's disposed check.
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.revokedSessions).toEqual([]);
    expect(worker.connectAttempts()).toBe(1);
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(0);
  });

  it("never retires an idle worker whose role is the issue's active phase", async () => {
    const worker = await idleWorkerFixture({
      role: "implementer",
      phases: { [root]: { phase: "implementer", sessionId: "ses_implementer" } },
    });
    const seededLocator = structuredClone(worker.claim().locator);

    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the fire reaches only retireIdleWorker's active-phase check (re-armed).
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.revokedSessions).toEqual([]);
    expect(worker.connectAttempts()).toBe(1);
  });

  it("never retires an idle worker holding a queued pendingAssignment", async () => {
    // Deliberately NOT pushed onto state.workerAdmission.queue: the promotion trigger has nothing
    // to drain, so this isolates the pendingAssignment guard itself.
    const worker = await idleWorkerFixture({
      role: "implementer",
      claim: {
        pendingAssignment: {
          kind: "assignment",
          task: "verify again",
          queuedAt: "2026-08-24T00:00:00.000Z",
          deliveryId: TEST_DELIVERY_ID,
        },
      },
    });
    const seededLocator = structuredClone(worker.claim().locator);

    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the fire reaches only retireIdleWorker's pendingAssignment check (re-armed).
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.claim().pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify again",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(worker.revokedSessions).toEqual([]);
    expect(worker.connectAttempts()).toBe(1);
  });

  it("re-arms the clock when expiry declines because the role is the active phase, and retires within one further window once phases[issue] moves to another role", async () => {
    // Scenario: a reviewer finished its turn while still phases[issue].phase; its clock expired as
    // a correct no-op; the planner was then resumed and /worker/started re-wrote
    // phases[issue] = planner. Being already idle, the reviewer never transitions to idle again —
    // nothing but the expiry itself can arm its next clock.
    const worker = await idleWorkerFixture({
      role: "reviewer",
      phases: { [root]: { phase: "reviewer", sessionId: "ses_reviewer" } },
    });
    const seededLocator = structuredClone(worker.claim().locator);

    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the fire reaches only retireIdleWorker's active-phase check (re-armed).
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.connectAttempts()).toBe(1);
    // Declined for a reason that can change without this worker's own idle transition: re-armed.
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(1);

    worker.state.phases[root] = { phase: "planner", sessionId: "ses_planner" };
    const retired = worker.saves.completed.next();
    expect(worker.clock.fire(600_000)).toBeTrue();
    await retired;

    expect(worker.shutdownCalls).toEqual([worker.token]);
    expect(
      worker.commands.filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
    ).toEqual([]);
    expect(worker.claim().locator).toBeUndefined();
    expect(worker.claim().resumeSessionFile).toBe(worker.sessionFile);
    expect(worker.claim().launchFailures).toBeUndefined();
    expect(worker.claim().promptFailures).toBeUndefined();
    expect(worker.publications).toEqual([]);
    // A retired worker's clock does not keep polling.
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(0);
  });

  it("re-arms the clock when expiry declines because a pendingAssignment is queued, and retires within one further window once it is cleared", async () => {
    const worker = await idleWorkerFixture({
      role: "reviewer",
      claim: {
        pendingAssignment: {
          kind: "assignment",
          task: "x",
          queuedAt: "2026-08-24T00:00:00.000Z",
          deliveryId: TEST_DELIVERY_ID,
        },
      },
    });
    const seededLocator = structuredClone(worker.claim().locator);

    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the fire reaches only retireIdleWorker's pendingAssignment check (re-armed).
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.claim().pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "x",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(1);

    delete worker.claim().pendingAssignment;
    const retired = worker.saves.completed.next();
    expect(worker.clock.fire(600_000)).toBeTrue();
    await retired;

    expect(worker.shutdownCalls).toEqual([worker.token]);
    expect(worker.claim().locator).toBeUndefined();
    expect(worker.claim().resumeSessionFile).toBe(worker.sessionFile);
    expect(worker.claim().launchFailures).toBeUndefined();
    expect(worker.claim().promptFailures).toBeUndefined();
    expect(worker.publications).toEqual([]);
  });

  it("never retires an idle sub-architect, however long its idle window has run", async () => {
    const worker = await idleWorkerFixture({ issue: child, role: "architect" });
    const seededLocator = structuredClone(worker.claim().locator);

    expect(worker.clock.fire(600_000)).toBeTrue();
    // Negative wait: the fire reaches only retireIdleWorker's architect check (never re-armed).
    await flushEventLoop(20);

    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toEqual(seededLocator);
    expect(worker.revokedSessions).toEqual([]);
    expect(worker.connectAttempts()).toBe(1);
    // An architect is never retired, so re-arming would only spin: no clock pending after the fire.
    expect(worker.clock.pending.filter((wait) => wait.ms === 600_000)).toHaveLength(0);
  });

  it("never arms the idle-retire clock when worker_idle_retire_seconds is 0, while the idle trigger still fires for queue promotion", async () => {
    const worker = await idleWorkerFixture({ role: "implementer", workerIdleRetireSeconds: 0 });

    expect(worker.clock.pending).toEqual([]);
    // The composed idle callback lost nothing: the trigger fired once for the queue re-check.
    expect(worker.client.idleFireCount).toBe(1);
    // No wait: armIdleRetire no-ops synchronously at 0 and reconnectWorkers was awaited above,
    // so nothing is in flight.
    expect(worker.shutdownCalls).toEqual([]);
    expect(worker.claim().locator).toBeDefined();
  });

  it("resumes a retired worker with --resume on its next spawn_worker, exactly like a dead-pane recovery", async () => {
    const worker = await idleWorkerFixture({ role: "implementer" });
    const retired = worker.saves.completed.next();
    expect(worker.clock.fire(600_000)).toBeTrue();
    await retired;
    expect(worker.claim().resumeSessionFile).toBe(worker.sessionFile);

    const result = await worker.manager.spawnWorker(
      root,
      root,
      "implementer",
      "Run the legion-retro skill now."
    );

    expect(result).toEqual({ status: "spawned", roleToken: worker.token });
    const launch = worker.commands.find(
      (command) =>
        command[0] === "tmux" && (command[3] === "new-window" || command[3] === "split-window")
    );
    if (!launch) throw new Error("retired worker's resume did not open a pane");
    expect(launch.at(-1)).toContain(`--resume=${worker.sessionFile}`);
    expect(worker.claim()).toMatchObject({ generation: 2, expectedSessionId: "ses_implementer" });
    expect(worker.claim().locator?.ompSessionFile).toBe(worker.sessionFile);
    expect(worker.claim().resumeSessionFile).toBeUndefined();
    expect(worker.claim().launchFailures).toBe(0);
  });

  it("closes and does not cache a worker socket whose negotiation fails, so a later call reconnects", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    let connectCalls = 0;
    let firstClientClosed = false;
    const goodClient = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        connectCalls += 1;
        if (connectCalls === 1) {
          return {
            ...fakeWorkerRpcClient(),
            negotiate: async () => {
              throw new Error("shim never answered negotiate_protocol");
            },
            close: () => {
              firstClientClosed = true;
            },
          };
        }
        return goodClient;
      },
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);
    expect(firstClientClosed).toBe(true);

    expect(connectCalls).toBe(2);
    expect(goodClient.prompts).toEqual(["verify #41"]);
  });

  it("clears a claim's stale locator when reconnectWorkers finds its socket dead, keeping its pending assignment", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
    });

    await processes.reconnectWorkers();

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("clears a dead worker's locator on boot when the private tmux socket does not exist yet (first boot after upgrade or reboot)", async () => {
    // Nothing has created the private server yet: the kill-pane fallback fails to *connect*
    // rather than finding a server with no such pane. That must count as pane-gone exactly like
    // `no server running`, or every dead worker keeps its locator (and its running-worker slot)
    // until some later launch happens to fork the server.
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    // A confirmed, previously-live worker (session registered, ready confirmed): reconnect takes
    // the `markWorkerDead` path, not the unconfirmed-boot retirement.
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return {
            stdout: "",
            stderr: "error connecting to /tmp/tmux-1000/legion-omp (No such file or directory)",
            exitCode: 1,
          };
        }
        return { stdout: "", exitCode: 1 };
      },
    });

    await processes.reconnectWorkers();

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("passes DISPATCH_URL and a DISPATCH_TOKEN_FILE pointer to a spawned phase worker, never the token or the retired DISPATCH_MCP_URL alias", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "tester", "verify #41");

    const windowCommand = commands.find(
      (command) => command[0] === "tmux" && command[3] === "new-window" && command.includes("-n")
    );
    if (!windowCommand) throw new Error("worker spawn did not open a tmux window");
    const environment = tmuxWindowEnvironment(windowCommand);
    expect(environment.DISPATCH_URL).toBe("http://127.0.0.1:18766");
    expect(environment.DISPATCH_TOKEN).toBeUndefined();
    expect(environment.DISPATCH_TOKEN_FILE).toBe(path.join(stateDir, "secrets", "dispatch-token"));
    expect(environment.DISPATCH_MCP_URL).toBeUndefined();
  });

  it("passes DISPATCH_URL and a DISPATCH_TOKEN_FILE pointer to the controller pane, never the token or the retired DISPATCH_MCP_URL alias", async () => {
    const stateDir = await temporaryDir();
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
    });

    await processes.ensureController();

    const windowCommand = commands.find(
      (command) => command[0] === "tmux" && command[3] === "new-window"
    );
    if (!windowCommand) throw new Error("controller spawn did not open a tmux window");
    const environment = tmuxWindowEnvironment(windowCommand);
    expect(environment.DISPATCH_URL).toBe("http://127.0.0.1:18766");
    expect(environment.DISPATCH_TOKEN).toBeUndefined();
    expect(environment.DISPATCH_TOKEN_FILE).toBe(path.join(stateDir, "secrets", "dispatch-token"));
    expect(environment.DISPATCH_MCP_URL).toBeUndefined();
  });

  it("kills a still-running pane whose socket is unreachable before clearing its locator on reconnect", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ...paneIdentity(22222),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[3] === "list-panes" &&
          command.includes("%7") &&
          command.includes("#{pane_id} #{pane_pid}")
        ) {
          return { stdout: "%7 22222\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    expect(
      commands.some(
        (command) => command[0] === "tmux" && command[3] === "kill-pane" && command.includes("%7")
      )
    ).toBeTrue();
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("bails without connecting again or killing when the current claim's locator no longer matches the one that was probed", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    const staleLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%7",
      socketPath: "/state/workers/tester.sock",
    };
    const freshLocator = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@50",
      tmuxPaneId: "%9",
      socketPath: "/state/workers/tester.sock",
    };
    state.roles[token] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: staleLocator,
    };
    const connectAttempts: string[] = [];
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectAttempts.push(socketPath);
        // A respawn completes and replaces this claim's locator with a fresh pane id while the
        // probe is still in flight -- mirrors a concurrent spawnWorker finishing between this
        // probe starting and its failure being handled.
        const current = state.roles[token];
        if (current && "issue" in current) current.locator = freshLocator;
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    // Exactly the one probe connect -- the identity mismatch bails before the runtime's
    // stop-time dial ever gets a chance to connect (which would otherwise reach the socket path
    // a respawn reuses).
    expect(connectAttempts).toEqual(["/state/workers/tester.sock"]);
    expect(commands.filter((c) => c[3] === "kill-pane")).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toEqual(freshLocator);
  });

  it("reconnectWorkers enqueues a dead unconfirmed claim for the normal admission drain, never launching it directly at restart before promotion is enabled", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const token = roleToken("omp", child, "implementer");
    state.roles[token] = {
      issue: child,
      role: "implementer",
      generation: 1,
      pendingAssignment: {
        kind: "assignment",
        task: "implement #43",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/dead-implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const commands: string[][] = [];
    // `skipEnableLaunches` models exactly what `reconnectWorkers` runs under in production: the
    // daemon calls it during boot, before `enableLaunches()` releases the launch hold.
    const { manager: processes, state: managedState } = manager(
      state,
      {
        config: config(stateDir, { workerCap: 1 }),
        connectWorkerRpc: async () => {
          throw new Error("ECONNREFUSED");
        },
        run: async (command) => {
          commands.push(command);
          if (command[0] === "tmux" && command[3] === "list-panes") {
            // Reports the original dead pane as gone (drives the first, restart-time
            // retirement decision); the freshly-launched pane's own later liveness is never
            // queried by this test.
            return paneGone();
          }
          if (command[0] === "tmux" && command[3] === "new-window") {
            return { stdout: "@50 %50 54321\n", exitCode: 0 };
          }
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnableLaunches: true }
    );

    await processes.reconnectWorkers();

    // Never launched directly: no tmux command at all beyond the pane-liveness poll
    // `retireWorkerLocator` itself makes (which reports dead here) — specifically no
    // `new-window`/`split-window` opening a fresh pane.
    expect(
      commands.some((command) => command[3] === "new-window" || command[3] === "split-window")
    ).toBeFalse();
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const queuedClaim = managedState.roles[token];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("claim disappeared");
    expect(queuedClaim.locator).toBeUndefined();
    expect(queuedClaim.resumeSessionFile).toBe(resumeFile);
    expect(queuedClaim.launchFailures).toBe(1);
    expect(queuedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "implement #43",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });

    // Only once promotion is enabled (the post-`api`-assignment boot step) does the queued
    // retry actually launch, through the normal cap-aware, role-locked drain.
    processes.enableLaunches();
    commands.length = 0;
    await processes.reconcileWorkerAdmission();

    expect(commands.some((command) => command[3] === "new-window")).toBeTrue();
    expect(managedState.workerAdmission.queue).toEqual([]);
    const launchedClaim = managedState.roles[token];
    if (!launchedClaim || !("issue" in launchedClaim)) throw new Error("claim disappeared");
    expect(launchedClaim.locator).toBeDefined();
  });

  it("a throw while reconnecting one worker claim is logged with its token and leaves that claim alone while every other claim is reconciled", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const roles = ["planner", "tester", "reviewer"] as const;
    const tokens = roles.map((role) => roleToken("omp", root, role));
    roles.forEach((role, index) => {
      state.roles[tokens[index] as string] = {
        issue: root,
        role,
        generation: 1,
        sessionId: `ses-${index + 1}`,
        readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: "@42",
          tmuxPaneId: `%${index + 1}`,
          socketPath: `/state/workers/${role}.sock`,
          ompSessionFile: `/state/sessions/${role}.json`,
          ...paneIdentity(),
        },
      };
    });
    const commands: string[][] = [];
    const failure = new TypeError("api.revokeSessionCapability is not a function");
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      revokeSessionCapability: (sessionId) => {
        if (sessionId === "ses-2") throw failure;
      },
      run: async (command) => {
        commands.push(command);
        // Every pane is live and still its recorded process (pid 12345, the fixture identity),
        // so a dead-socket retirement's kill is a real kill of the recorded pane.
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    let isolationLogs: unknown[][];
    try {
      await processes.reconnectWorkers();
      isolationLogs = errorSpy.mock.calls.filter((call) =>
        String(call[0]).includes("failed to reconcile worker")
      );
    } finally {
      errorSpy.mockRestore();
    }

    const killed = commands
      .filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
      .map((command) => command.at(-1));
    expect(killed.sort()).toEqual(["%1", "%3"]);
    for (const index of [0, 2]) {
      const claim = managedState.roles[tokens[index] as string];
      if (!claim || !("issue" in claim)) throw new Error(`claim ${index + 1} disappeared`);
      expect(claim.locator).toBeUndefined();
      expect(claim.resumeSessionFile).toBe(`/state/sessions/${roles[index]}.json`);
    }
    const untouched = managedState.roles[tokens[1] as string];
    if (!untouched || !("issue" in untouched)) throw new Error("claim 2 disappeared");
    expect(tmuxFields(untouched.locator)?.tmuxPaneId).toBe("%2");
    expect(untouched.resumeSessionFile).toBeUndefined();
    expect(isolationLogs).toEqual([
      [expect.stringContaining(`failed to reconcile worker ${tokens[1]}`), failure],
    ]);
  });

  it("a throw while re-arming one root's registration deadline is logged with its tree and leaves the other roots armed", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 3);
    const roots = ["LEGION-41", "LEGION-42", "LEGION-43"] as const;
    for (const key of roots) {
      state.issues[key] = { key, title: key, status: "in_progress", children: [] };
      state.trees[key] = {
        root: key,
        generation: 1,
        status: "active",
        launchFailures: 0,
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: `@${key.slice(-2)}`,
          tmuxPaneId: `%${key.slice(-2)}`,
          socketPath: `/state/workers/${key}.sock`,
        },
      };
      state.admission.active.push(key);
    }
    let sleeps = 0;
    const failure = new Error("timer registry exploded");
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      sleep: () => {
        sleeps += 1;
        if (sleeps === 2) throw failure;
        return new Promise<void>(() => {});
      },
    });
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    let isolationLogs: unknown[][];
    try {
      expect(() => processes.reconnectRoots()).not.toThrow();
      isolationLogs = errorSpy.mock.calls.filter((call) =>
        String(call[0]).includes("failed to reconcile root")
      );
    } finally {
      errorSpy.mockRestore();
    }
    expect(sleeps).toBe(3);
    expect(isolationLogs).toEqual([
      [expect.stringContaining("failed to reconcile root LEGION-42"), failure],
    ]);
  });

  it("persists a retirement's queue-push in the same save as its locator-clear, so a reload after a crash mid-drain still finds the token queued", async () => {
    const stateDir = await temporaryDir();
    const stateFile = path.join(stateDir, "state.json");
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const token = roleToken("omp", child, "implementer");
    state.roles[token] = {
      issue: child,
      role: "implementer",
      generation: 1,
      pendingAssignment: {
        kind: "assignment",
        task: "implement #43",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: "00000000-0000-4000-8000-000000000043",
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/dead-implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const { manager: processes } = manager(
      state,
      {
        config: config(stateDir, { workerCap: 1 }),
        // The real save/load round trip, exactly as `index.ts` wires it — this test's whole
        // point is proving what actually lands on disk, not just what the in-memory object
        // holds afterward.
        saveState: () => legionStateSaveState(stateFile, state),
        connectWorkerRpc: async () => {
          throw new Error("ECONNREFUSED");
        },
        run: async (command) => {
          if (command[0] === "tmux" && command[3] === "list-panes") {
            return paneGone();
          }
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnableLaunches: true }
    );

    await processes.reconnectWorkers();

    const reloaded = await legionStateLoadState(stateFile, { project: "omp", cap: 1 });
    expect(reloaded.workerAdmission.queue).toEqual([token]);
    const reloadedClaim = reloaded.roles[token];
    if (!reloadedClaim || !("issue" in reloadedClaim)) throw new Error("claim disappeared");
    expect(reloadedClaim.locator).toBeUndefined();
    expect(reloadedClaim.resumeSessionFile).toBe(resumeFile);
    expect(reloadedClaim.launchFailures).toBe(1);
    expect(reloadedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "implement #43",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });

    // Promotion, once enabled against the *reloaded* state, finds the token still queued and
    // relaunches it — proving the persisted queue entry is actually usable, not merely present.
    const relaunchCommands: string[][] = [];
    const { manager: reloadedProcesses, state: reloadedManagedState } = manager(reloaded, {
      config: config(stateDir, { workerCap: 1 }),
      run: async (command) => {
        relaunchCommands.push(command);
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@50 %50 54321\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    reloadedProcesses.enableLaunches();
    await reloadedProcesses.reconcileWorkerAdmission();

    expect(relaunchCommands.some((command) => command[3] === "new-window")).toBeTrue();
    expect(reloadedManagedState.workerAdmission.queue).toEqual([]);
  });

  it("a spawnWorker call racing a runtime retirement decision for the same unconfirmed token, at cap 1, produces exactly one pane and one coherent claim", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const token = roleToken("omp", root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 1,
      launchFailures: 0,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/dead-implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const commands: string[][] = [];
    const publications: Array<{ subject: string; json: string }> = [];
    const relaunched = Promise.withResolvers<void>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      // Never overridden before: the freshly-launched worker's boot watchdog otherwise fell back
      // to a real timer (`createCancellableSleep`'s `setTimeout`) for its 100ms connect-retry
      // poll, since `now()` below is a fixed fake clock whose deadline check the watchdog can
      // never advance past. Deterministic like every other test in this file that arms a watch.
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      publishRole: (subject, json) => {
        publications.push({ subject, json });
        if (
          subject === roleTopic(roleToken("omp", root, "architect")) &&
          json === JSON.stringify({ type: "worker-started", issue: root, role: "implementer" })
        ) {
          relaunched.resolve();
        }
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") {
          // The recorded pane is gone in every liveness probe throughout this test.
          return paneGone();
        }
        if (
          command[0] === "tmux" &&
          (command[3] === "new-window" || command[3] === "split-window")
        ) {
          return { stdout: "@50 %50 54321\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    processes.enableLaunches();

    // Races a runtime retirement decision (as `reconnectWorkers`, the boot watchdog, and
    // `onWorkerClientClosed` all funnel through `retireUnconfirmedBoot`) against a concurrent
    // `spawnWorker` call for the *same* token. Both serialize through the same
    // `WorkerAdmission.mutateClaim(token, …)` per-token queue, so exactly one of them runs
    // first — but either order must still yield exactly one pane: if `spawnWorker` wins the
    // queue, it sees the still-unconfirmed claim and queues the new task without launching
    // (the "booting" branch); if the retirement wins, it clears the locator and enqueues,
    // leaving `spawnWorker` to see no locator and launch fresh. A bug that let both sides
    // decide to launch independently would open two panes at a cap of one.
    await Promise.all([
      processes.reconnectWorkers(),
      processes.spawnWorker(root, root, "implementer", "task2"),
    ]);
    await relaunched.promise;

    const paneOpens = commands.filter(
      (command) => command[3] === "new-window" || command[3] === "split-window"
    );
    expect(paneOpens.length).toBe(1);
    expect(publications.filter((publication) => publication.json.includes("worker-died"))).toEqual(
      []
    );

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim disappeared");
    expect(claim.locator).toBeDefined();
    expect(tmuxFields(claim.locator)?.tmuxPaneId).toBe("%50");
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "task2",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.resumeSessionFile ?? claim.locator?.ompSessionFile).toBe(resumeFile);
    // Whichever race order wins, the decision still funnels through `launchWorker`'s
    // `provisionWorkspace` step, which performs real `mkdir` I/O (via this file's shared
    // `manager()` harness's `jj git clone`/`jj workspace add` handling) rather than a fake one;
    // under a CPU/IO-starved host this can legitimately take longer than bun's default 5000ms
    // per-test budget even though every timer/clock the test itself controls above is fake.
  }, 20_000);

  it("a worker-ready confirmation whose lock acquisition wins a race against a runtime retirement decision keeps the claim confirmed, making the retirement a no-op", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const token = roleToken("omp", root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 3,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const capturedGeneration = 3;
    const capturedPaneId = "%9";
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return paneGone();
        return { stdout: "", exitCode: 0 };
      },
    });
    processes.enableLaunches();

    // Models `workerReady`'s atomic confirmation state change. Called with no preceding await,
    // its `mutateClaim` registration lands on the per-token queue essentially immediately —
    // ahead of `reconnectWorkers`' own retirement decision, which only reaches its `mutateClaim`
    // call after its `clientFor` connect attempt rejects (several microtask ticks later) — so
    // this wins the race to run first.
    const confirm = () =>
      processes.mutateLiveRoleClaim(root, root, token, async () => {
        const current = managedState.roles[token];
        if (
          !current ||
          !("issue" in current) ||
          !current.locator ||
          current.generation !== capturedGeneration ||
          tmuxFields(current.locator)?.tmuxPaneId !== capturedPaneId
        ) {
          throw new Error("Stale worker generation");
        }
        // Replaces the object (never mutates it in place), exactly like the real ready path
        // does — so `reconnectWorkers`' claim reference, captured by value before confirmation,
        // stays stale and this test genuinely exercises `retireUnconfirmedBoot`'s fresh re-read.
        managedState.roles[token] = {
          ...current,
          sessionId: "ses_confirmed",
          readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
        };
        return "confirmed";
      });

    const [reconnectResult, confirmResult] = await Promise.allSettled([
      processes.reconnectWorkers(),
      confirm(),
    ]);

    expect(reconnectResult.status).toBe("fulfilled");
    expect(confirmResult.status).toBe("fulfilled");
    if (confirmResult.status === "fulfilled") expect(confirmResult.value).toBe("confirmed");

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim disappeared");
    expect(claim.sessionId).toBe("ses_confirmed");
    expect(tmuxFields(claim.locator)?.tmuxPaneId).toBe("%9");
    expect(claim.generation).toBe(3);
    expect(claim.launchFailures).toBeUndefined();
    expect(commands.some((command) => command[3] === "kill-pane")).toBeFalse();
  });

  it("a runtime retirement decision that wins its lock acquisition before a racing worker registration's slow GitHub-lease fetch resolves rejects the stale registration and leaves the claim retired for retry", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const token = roleToken("omp", root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 3,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const capturedGeneration = 3;
    const capturedPaneId = "%9";
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") return paneGone();
        return { stdout: "", exitCode: 0 };
      },
    });

    // Simulates `handleWorkerStarted`'s slow, deliberately-outside-any-lock GitHub lease fetch:
    // a real macrotask tick flushes every pending microtask first, so `reconnectWorkers`' own
    // `mutateClaim` registration (a handful of microtask ticks past its rejected connect
    // attempt) always lands on the per-token queue — and therefore commits — before this
    // confirmation's lock acquisition is even attempted.
    const confirm = async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
      return processes.mutateLiveRoleClaim(root, root, token, async () => {
        const current = managedState.roles[token];
        if (
          !current ||
          !("issue" in current) ||
          !current.locator ||
          current.generation !== capturedGeneration ||
          tmuxFields(current.locator)?.tmuxPaneId !== capturedPaneId
        ) {
          throw new Error("Stale worker generation");
        }
        managedState.roles[token] = { ...current, sessionId: "ses_confirmed" };
        return "confirmed";
      });
    };

    const [reconnectResult, confirmResult] = await Promise.allSettled([
      processes.reconnectWorkers(),
      confirm(),
    ]);

    expect(reconnectResult.status).toBe("fulfilled");
    expect(confirmResult.status).toBe("rejected");
    if (confirmResult.status === "rejected") {
      expect(String(confirmResult.reason)).toContain("Stale worker generation");
    }

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim disappeared");
    expect(claim.sessionId).toBeUndefined();
    expect(claim.locator).toBeUndefined();
    expect(claim.launchFailures).toBe(1);
    expect(claim.resumeSessionFile ?? "").toBe(resumeFile);
  });

  it("keeps the controller ready-connect failure prefix on its first retry", async () => {
    const state = newLegionState("omp", 1);
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
      tmuxPaneId: "%1",
      socketPath: "/state/workers/controller.sock",
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
    });
    try {
      await expect(processes.markControllerReady()).resolves.toBeUndefined();
      expect(
        errorLog.mock.calls.find(
          (call) => call[0] === "[legion] failed to connect controller shim socket on ready:"
        )
      ).toBeDefined();
    } finally {
      errorLog.mockRestore();
    }
  });

  it("keeps the architect ready-connect failure prefix on its first retry", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
    });
    try {
      await expect(processes.markTreeReady(root)).resolves.toBeUndefined();
      expect(
        errorLog.mock.calls.find(
          (call) =>
            call[0] === `[legion] failed to connect architect shim socket on ready for ${root}:`
        )
      ).toBeDefined();
    } finally {
      errorLog.mockRestore();
    }
  });

  it("restores runState to idle without firing onIdle when a queued idle-resume prompt rejects, keeping the assignment queued", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    client.prompt = async (message: string) => {
      client.prompts.push(message);
      const previous = client.runState;
      client.setRunStateSilently("running");
      // Mirrors the real worker-rpc.ts fix: an ordinary rejection restores runState silently —
      // an undo, not a transition — never firing onIdle (see WorkerRpcClient.prompt's doc
      // comment). A "transition" restore here would re-trigger promoteWorkerQueue synchronously
      // mid-rejection: an unbounded retry storm for a persistently-broken client.
      client.setRunStateSilently(previous);
      throw new Error("shim write failed");
    };
    const { processes, state, managedState } = await workerCapFixture(2, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    await processes.reconcileWorkerAdmission();

    expect(client.runState).toBe("idle");
    expect(client.idleFireCount).toBe(0);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.locator).toBeDefined();
    expect(claim.promptFailures).toBe(1);
  });

  it("retires a persistently-rejecting queued worker after MAX_LAUNCH_FAILURES consecutive prompt rejections (relaunch cycle 1, bound 2), falling through to a fresh launch on the next drain", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    client.prompt = async (message: string) => {
      client.prompts.push(message);
      const previous = client.runState;
      client.setRunStateSilently("running");
      client.setRunStateSilently(previous);
      throw new Error("shim write failed");
    };
    const { processes, state, managedState } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    // Three separate drain attempts, each a rejected prompt — the third crosses
    // MAX_LAUNCH_FAILURES (3) and retires the pane (markWorkerDeadLocked, invoked via
    // WorkerAdmissionDeps.retireDeadClaim).
    await processes.reconcileWorkerAdmission();
    await processes.reconcileWorkerAdmission();
    await processes.reconcileWorkerAdmission();

    const retiredClaim = managedState.roles[token];
    if (!retiredClaim || !("issue" in retiredClaim)) throw new Error("tester claim disappeared");
    expect(retiredClaim.promptFailures).toBe(0);
    expect(retiredClaim.promptRetires).toBe(1);
    expect(retiredClaim.locator).toBeUndefined();
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(client.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);

    // Falls through to the launch path on the next drain: a fresh cold launch, not another
    // prompt attempt against the now-retired client.
    await processes.reconcileWorkerAdmission();

    expect(managedState.workerAdmission.queue).toEqual([]);
    const launchedClaim = managedState.roles[token];
    if (!launchedClaim || !("issue" in launchedClaim)) throw new Error("tester claim disappeared");
    expect(launchedClaim.locator).toBeDefined();
    expect(tmuxFields(launchedClaim.locator)?.tmuxPaneId).toBe("%301");
    // The relaunch carries the cycle count: a refusal-driven retirement counts toward the same
    // bound a swallowed prompt does (LEGION-93).
    expect(launchedClaim.promptRetires).toBe(1);
    expect(client.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);
  });

  it("resets promptFailures to 0 on a successful prompt after prior rejections", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    let shouldReject = true;
    const realPrompt = client.prompt;
    client.prompt = async (message, deliveryId) => {
      if (shouldReject) {
        client.prompts.push(message);
        const previous = client.runState;
        client.setRunStateSilently("running");
        client.setRunStateSilently(previous);
        throw new Error("shim write failed");
      }
      return realPrompt(message, deliveryId);
    };
    const { processes, state, managedState } = await workerCapFixture(2, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    await processes.reconcileWorkerAdmission();
    const rejectedClaim = managedState.roles[token];
    if (!rejectedClaim || !("issue" in rejectedClaim)) throw new Error("tester claim disappeared");
    expect(rejectedClaim.promptFailures).toBe(1);

    shouldReject = false;
    await processes.reconcileWorkerAdmission();

    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[token];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim disappeared");
    expect(promotedClaim.promptFailures).toBe(0);
  });

  /** A ready-confirmed, idle-live tester whose architect assignment waits on the promotion queue,
   * cached in the manager exactly as `reconnectWorkers` leaves it, on a manual clock — the fixture
   * every acknowledged-without-a-turn case below starts from. Its confirming `get_state` answers
   * "no stream" (the fake ignores an `isStreaming: false` answer's run-state side, so the reconnect
   * probe's own call leaves the silently-seeded idle alone). `turnStartsOnPrompt` is left `true`
   * here; a case models the acknowledgement no turn follows by flipping it. */
  async function queuedIdleWorkerFixture(
    workerCap: number,
    overrides: Partial<ProcessManagerDeps & RuntimeOverrides> = {},
    client: FakeWorkerRpcClient = fakeWorkerRpcClient()
  ) {
    const token = roleToken("omp", root, "tester");
    const clock = manualSleep();
    client.setRunStateSilently("idle");
    const fixture = await workerCapFixture(workerCap, {
      connectWorkerRpc: async () => client,
      sleep: clock.sleep,
      ...overrides,
    });
    fixture.state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: "00000000-0000-4000-8000-000000000001",
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    fixture.state.workerAdmission.queue.push(token);
    await fixture.processes.reconnectWorkers();
    client.getStateImpl = async () => ({ data: { isStreaming: false } });
    return { ...fixture, token, clock, client };
  }

  /** The turn-start bound `awaitTurnStart` arms on the injected clock: `workerRpcTimeoutSeconds`
   * (5) in ms, the one `sleep` these tests' manager makes besides a stop timeout. */
  const TURN_START_BOUND_MS = 5_000;

  /** Expires one prompt's turn-start bound. Call it right after the triggering call (before any
   * await): `next()` captures the bound `promptExistingWorker` is about to arm on the manual clock
   * -- however many earlier bounds this test already fired -- then fires it once armed; the fake's
   * `get_state` then answers "no stream". */
  async function expireTurnStartWait(
    sleeps: (ms: number) => EventCounter,
    clock: ManualClock
  ): Promise<void> {
    await sleeps(TURN_START_BOUND_MS).next();
    expect(clock.fire(TURN_START_BOUND_MS)).toBeTrue();
  }

  /** Counts the fake's completed `prompt` calls (wrapped like `idleWorkerFixture`'s
   * `client.shutdown`), for the tests whose event is "the worker was prompted (again)". */
  function promptCounter(client: FakeWorkerRpcClient): EventCounter {
    const prompted = eventCounter();
    const prompt = client.prompt.bind(client);
    client.prompt = async (...args) => {
      const receipt = await prompt(...args);
      prompted.increment();
      return receipt;
    };
    return prompted;
  }

  function testerClaim(state: LegionState, token: string): WorkerRoleClaim {
    const claim = state.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    return claim;
  }

  const workerStartedJson = JSON.stringify({ type: "worker-started", issue: root, role: "tester" });
  const workerQueuedJson = JSON.stringify({ type: "worker-queued", issue: root, role: "tester" });
  const workerDiedJson = JSON.stringify({ type: "worker-died", issue: root, role: "tester" });
  const retireLine = (token: string, cycle: number): string =>
    `[legion] ${token}: retiring after 3 prompts with no turn started; relaunch cycle ${cycle} (bound 2)`;

  it("an identical re-send to a queued idle-live worker at cap changes nothing: no save, no publish, no prompt", async () => {
    let saves = 0;
    const { processes, managedState, publications, token, client } = await queuedIdleWorkerFixture(
      1,
      {
        saveState: async () => {
          saves += 1;
        },
      }
    );
    // An idle worker holds no slot; a located claim with no cached client counts as running and
    // fills the one slot, so the tester's re-send is judged at cap.
    managedState.roles[roleToken("omp", root, "planner")] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%8",
        socketPath: "/state/workers/planner.sock",
      },
    };
    const savesBefore = saves;
    const publicationsBefore = publications.length;

    const result = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(result).toEqual({ status: "queued", roleToken: token });
    expect(saves).toBe(savesBefore);
    expect(publications).toHaveLength(publicationsBefore);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(client.prompts).toEqual([]);
  });

  it("commits a queued idle-resume prompt only when the worker's turn starts, not on the shim's acknowledgement", async () => {
    const { processes, managedState, publications, token, clock, client, sleeps } =
      await queuedIdleWorkerFixture(2);
    client.turnStartsOnPrompt = false;

    const run = processes.reconcileWorkerAdmission();
    // The prompt was acknowledged and `awaitTurnStart` armed its bound: the first (and only)
    // turn-start sleep this test makes.
    await sleeps(TURN_START_BOUND_MS).reached(1);

    // Acknowledged, no turn yet: nothing is committed.
    expect(client.prompts).toEqual(["verify #41"]);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(testerClaim(managedState, token).pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(managedState.phases[root]).toBeUndefined();
    expect(client.runState).toBe("running");
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(0);

    // The worker's agent_start arrives.
    client.emitRunState("running");
    await run;

    expect(managedState.workerAdmission.queue).toEqual([]);
    const claim = testerClaim(managedState, token);
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.promptFailures).toBe(0);
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(1);
    // The bound was never reached: its wait is still pending, unfired.
    expect(clock.pending.filter((wait) => wait.ms === 5_000)).toHaveLength(1);
    expect(client.getStateCalls).toBe(1);
  });

  it("keeps the assignment queued and counts a prompt failure when the acknowledged prompt starts no turn within the bound", async () => {
    const { processes, managedState, publications, token, clock, client, sleeps } =
      await queuedIdleWorkerFixture(2);
    client.turnStartsOnPrompt = false;
    const initialDeliveryId = testerClaim(managedState, token).pendingAssignment?.deliveryId;
    if (!initialDeliveryId) throw new Error("queued assignment has no delivery ID");
    const getStateCallsBefore = client.getStateCalls;
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorCalls: unknown[][] = [];
    try {
      const run = processes.reconcileWorkerAdmission();
      await expireTurnStartWait(sleeps, clock);
      await run;
    } finally {
      errorCalls = errorLog.mock.calls.map((call) => [...call]);
      errorLog.mockRestore();
    }

    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = testerClaim(managedState, token);
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.pendingAssignment?.deliveryId).not.toBe(initialDeliveryId);
    expect(claim.locator).toBeDefined();
    expect(claim.promptFailures).toBe(1);
    expect(managedState.phases[root]).toBeUndefined();
    // The pre-prompt run state is restored silently — an undo, never an idle transition — and
    // the spec's second signal was asked for exactly once.
    expect(client.runState).toBe("idle");
    expect(client.idleFireCount).toBe(0);
    expect(client.getStateCalls).toBe(getStateCallsBefore + 1);
    expect(client.prompts).toEqual(["verify #41"]);
    expect(client.deliveryIds).toEqual([initialDeliveryId]);
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(0);
    expect(errorCalls).toHaveLength(1);
    const [message, error] = errorCalls[0] ?? [];
    expect(String(message)).toContain(token);
    expect(error).toBeInstanceOf(PromptNotStarted);
    expect(String(error)).toContain("get_state: isStreaming=false");
  });

  it("commits a turn that starts after the bound as the same delivery and never prompts the task again", async () => {
    const { processes, managedState, publications, token, clock, client, sleeps, published } =
      await queuedIdleWorkerFixture(2);
    client.turnStartsOnPrompt = false;
    const originalDeliveryId = testerClaim(managedState, token).pendingAssignment?.deliveryId;
    if (!originalDeliveryId) throw new Error("queued assignment has no delivery ID");
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const infoLog = vi.spyOn(console, "info").mockImplementation(() => {});
    let infoLines: string[] = [];
    try {
      const run = processes.reconcileWorkerAdmission();
      await expireTurnStartWait(sleeps, clock);
      await run;
      const retryDeliveryId = testerClaim(managedState, token).pendingAssignment?.deliveryId;
      expect(retryDeliveryId).not.toBe(originalDeliveryId);
      expect(client.deliveryIds).toEqual([originalDeliveryId]);
      expect(testerClaim(managedState, token).promptFailures).toBe(1);

      // The worker was merely slow: its agent_start arrives after the bound expired.
      // `commitLateStart`'s terminal effect: the queue entry is removed and persisted, then
      // `worker-started` is published -- the next publish of that type after the late start.
      const committed = published("worker-started").next();
      client.emitRunState("running");
      await committed;
    } finally {
      infoLines = infoLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
      infoLog.mockRestore();
    }

    const claim = testerClaim(managedState, token);
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.promptFailures).toBe(0);
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(1);
    expect(client.prompts).toEqual(["verify #41"]);
    expect(client.deliveryIds).toEqual([originalDeliveryId]);
    expect(infoLines).toContainEqual(
      expect.stringContaining(`${token} started its turn after the prompt wait expired`)
    );

    // The next drain finds nothing to deliver: the task is never prompted a second time.
    await processes.reconcileWorkerAdmission();
    expect(client.prompts).toEqual(["verify #41"]);
    expect(managedState.workerAdmission.queue).toEqual([]);
  });

  it("commits nothing on a late start when the queued task has been replaced meanwhile", async () => {
    const { processes, managedState, publications, token, clock, client, sleeps } =
      await queuedIdleWorkerFixture(2);
    client.turnStartsOnPrompt = false;
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const run = processes.reconcileWorkerAdmission();
      await expireTurnStartWait(sleeps, clock);
      await run;
    } finally {
      errorLog.mockRestore();
    }

    // A newer assignment for the same role replaced the one whose receipt is still pending.
    testerClaim(managedState, token).pendingAssignment = {
      kind: "assignment",
      task: "other",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: TEST_DELIVERY_ID,
    };
    client.emitRunState("running");
    // Negative wait: the late start reaches only commitLateStart's task-value check (replaced).
    await flushEventLoop(50);

    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(testerClaim(managedState, token).pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "other",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(managedState.phases[root]).toBeUndefined();
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(0);
  });

  it("retires a worker whose acknowledged prompts start no turn three times, then relaunches the assignment cold with --resume", async () => {
    const { processes, managedState, token, clock, client, sleeps } = await queuedIdleWorkerFixture(
      1,
      {
        run: async (command) => {
          if (command[0] === "tmux" && command[3] === "split-window") {
            return { stdout: "%301 23456\n", exitCode: 0 };
          }
          if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
          return { stdout: "", exitCode: 0 };
        },
      }
    );
    client.turnStartsOnPrompt = false;
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      // Three separate drain attempts, each an acknowledgement no turn follows — the third
      // crosses MAX_LAUNCH_FAILURES (3) and retires the pane through the same path a refusal
      // takes (retirePromptFailedClaim, via WorkerAdmissionDeps.retireDeadClaim), zeroing the
      // prompt count and counting the first relaunch cycle.
      for (let attempt = 0; attempt < 3; attempt += 1) {
        const run = processes.reconcileWorkerAdmission();
        await expireTurnStartWait(sleeps, clock);
        await run;
        expect(testerClaim(managedState, token).promptFailures).toBe(
          attempt === 2 ? 0 : attempt + 1
        );
      }
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }
    // Three failure lines and the one retirement line.
    expect(errorLines).toHaveLength(4);
    expect(errorLines).toContain(retireLine(token, 1));

    const retiredClaim = testerClaim(managedState, token);
    expect(retiredClaim.promptFailures).toBe(0);
    expect(retiredClaim.promptRetires).toBe(1);
    expect(retiredClaim.locator).toBeUndefined();
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(client.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);

    // Falls through to the launch path on the next drain: a fresh cold launch, not another
    // prompt attempt against the now-retired client.
    await processes.reconcileWorkerAdmission();

    expect(managedState.workerAdmission.queue).toEqual([]);
    const launchedClaim = testerClaim(managedState, token);
    expect(tmuxFields(launchedClaim.locator)?.tmuxPaneId).toBe("%301");
    expect(launchedClaim.promptRetires).toBe(1);
    expect(client.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);
  });

  /** A ready-confirmed idle tester whose every acknowledged prompt starts no turn, driven through
   * three failures, the first retirement, and the cold `--resume` relaunch (LEGION-93). Two fakes
   * keyed by socket path: `first` behind the seeded locator, `relaunched` behind whatever socket the
   * relaunch opens; a dial to a fake whose socket has closed is refused like an exited shim, so the
   * close handler's reconnect fails and its own `promoteWorkerQueue` drives the relaunch — no
   * explicit sweep. Returns once the relaunched claim (pane `%301`, generation 2) is in state, with
   * `/worker/started` simulated (session registered, session file recorded) but `/worker/ready`
   * not yet called; the caller decides whether the relaunched worker starts a turn. */
  async function swallowedRelaunchFixture() {
    const sessionFile = path.join(await temporaryDir(), "tester-session.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    const seededSocket = "/state/workers/tester.sock";
    const first = fakeWorkerRpcClient();
    first.turnStartsOnPrompt = false;
    const relaunched = fakeWorkerRpcClient();
    relaunched.turnStartsOnPrompt = false;
    // The confirming get_state seeds the fresh relaunched client from "unknown" to idle exactly
    // once (its only genuine idle transition); `abandonWait` has already restored later prompts'
    // run state to idle before this runs, so those calls fire nothing.
    relaunched.getStateImpl = async () => {
      relaunched.emitRunState("idle");
      return { data: { isStreaming: false } };
    };
    const closedSockets = new Set<FakeWorkerRpcClient>();
    void first.closed.then(() => closedSockets.add(first));
    void relaunched.closed.then(() => closedSockets.add(relaunched));
    const tmuxFake = relaunchingTmux();
    const publications: Array<{ subject: string; json: string }> = [];
    // The relaunch does real filesystem work (the session-file `stat`, the boot-token secret
    // file, the workspace config, the socket dir) before and after its tmux call, so a
    // `setImmediate` budget racing it is load-sensitive. `worker-started` is published only once
    // `launchWorker` has written the fresh claim, dequeued the token, and persisted — the last
    // step of the chain — so a waiter resolved from inside the fake publish is the observable end
    // itself. A wait that never resolves fails on bun's own timeout with the assertions
    // unreached, never a false green.
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const relaunchPublished = Promise.withResolvers<void>();
    const fixture = await queuedIdleWorkerFixture(
      1,
      {
        connectWorkerRpc: async (socketPath: string) => {
          const target = socketPath === seededSocket ? first : relaunched;
          if (closedSockets.has(target)) throw new Error("ECONNREFUSED");
          return target;
        },
        run: tmuxFake.run,
        publishRole: (subject, json) => {
          publications.push({ subject, json });
          if (subject === architectTopic && json === workerStartedJson) relaunchPublished.resolve();
        },
      },
      first
    );
    const { processes, managedState, token, clock, sleeps } = fixture;
    const claim = (): WorkerRoleClaim => testerClaim(managedState, token);
    const seeded = claim().locator;
    if (!seeded) throw new Error("seeded locator missing");
    seeded.ompSessionFile = sessionFile;

    // Three drains, each an acknowledgement no turn follows; the third retires the pane
    // (relaunch cycle 1, bound 2) and the close handler's drain relaunches the task cold.
    for (let attempt = 0; attempt < 3; attempt += 1) {
      const run = processes.reconcileWorkerAdmission();
      await expireTurnStartWait(sleeps, clock);
      await run;
    }
    await relaunchPublished.promise;
    // `/worker/started` for generation 2: the session registered, the session file recorded.
    const booted = claim();
    booted.sessionId = "ses_tester";
    return {
      ...fixture,
      publications,
      first,
      relaunched,
      tmuxFake,
      sessionFile,
      claim,
      splitWindows: () =>
        tmuxFake.commands.filter(
          (command) => command[0] === "tmux" && command[3] === "split-window"
        ).length,
    };
  }

  it("bounds the no-turn retire cycle: the relaunched worker's third swallowed prompt publishes worker-died once and is never relaunched again (LEGION-93)", async () => {
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      const {
        processes,
        managedState,
        publications,
        token,
        clock,
        sleeps,
        first,
        relaunched,
        claim,
        sessionFile,
        tmuxFake,
        splitWindows,
      } = await swallowedRelaunchFixture();
      const architectTopic = roleTopic(roleToken("omp", root, "architect"));

      // The first cycle: retired with the accounting the spec names, relaunched with --resume,
      // the cycle count carried onto the fresh claim.
      const relaunchedClaim = claim();
      expect(relaunchedClaim.generation).toBe(2);
      expect(relaunchedClaim.promptRetires).toBe(1);
      expect(relaunchedClaim.promptFailures).toBeUndefined();
      expect(relaunchedClaim.pendingAssignment).toMatchObject({
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
      });
      expect(first.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);
      expect(resumeArgument(tmuxFake.commands)).toContain(`--resume=${sessionFile}`);
      expect(splitWindows()).toBe(1);

      // /worker/ready delivers the queued task to the relaunched pane: acknowledged, no turn.
      const ready = processes.workerReady(root, "tester", "ses_tester", 2);
      await expireTurnStartWait(sleeps, clock);
      await ready;
      expect(claim().promptFailures).toBe(1);
      expect(claim().readyConfirmedAt).toBeDefined();
      expect(managedState.workerAdmission.queue).toEqual([token]);

      // Two more drains; the third failure on this pane is relaunch cycle 2 (bound 2): terminal.
      for (let attempt = 0; attempt < 2; attempt += 1) {
        const run = processes.reconcileWorkerAdmission();
        await expireTurnStartWait(sleeps, clock);
        await run;
      }
      // `worker-died` was published inside the drain `run` awaited (`retirePromptFailedClaim`,
      // under the role lock), so it is already in `publications`.
      // Negative wait: the retired socket's close reaches `onWorkerClientClosed` -> one refused
      // reconnect dial -> `markWorkerDead`, a no-op on the already-cleared locator, whose
      // `promoteWorkerQueue` finds the queue empty -- no file write, no injected `run`.
      await flushEventLoop(50);

      const died = publications.filter((p) => p.json === workerDiedJson);
      expect(died).toHaveLength(1);
      expect(died[0]?.subject).toBe(architectTopic);
      expect(publications.map((p) => JSON.parse(p.json).type)).toEqual([
        "worker-started", // the relaunch
        "worker-queued", // the relaunched pane's ready-time failure
        "worker-died", // the terminal retirement; no worker-queued for it
      ]);
      const terminal = claim();
      expect(terminal.locator).toBeUndefined();
      expect(terminal.pendingAssignment).toMatchObject({
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
      });
      expect(terminal.promptRetires).toBe(2);
      expect(terminal.promptFailures).toBe(3);
      expect(terminal.resumeSessionFile).toBe(sessionFile);
      expect(terminal.launchFailures).toBeUndefined();
      expect(managedState.workerAdmission.queue).toEqual([]);
      expect(managedState.phases[root]).toBeUndefined();
      expect(relaunched.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);
      expect(splitWindows()).toBe(1);

      // A later sweep finds nothing to relaunch either.
      await processes.reconcileWorkerAdmission();
      expect(splitWindows()).toBe(1);
      expect(claim().locator).toBeUndefined();
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }
    // Acceptance 3: each retirement logs the token, the prompt-failure count, and the cycle.
    const token = roleToken("omp", root, "tester");
    expect(errorLines.filter((line) => line === retireLine(token, 1))).toHaveLength(1);
    expect(errorLines.filter((line) => line === retireLine(token, 2))).toHaveLength(1);
  });

  it("clears promptRetires when the relaunched worker's first prompt starts a turn, so one later swallowed prompt neither retires nor escalates (LEGION-93)", async () => {
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      const { processes, managedState, publications, token, clock, sleeps, relaunched, claim } =
        await swallowedRelaunchFixture();
      expect(claim().promptRetires).toBe(1);

      // The relaunched worker is healthy: its ready-time prompt starts a turn.
      relaunched.turnStartsOnPrompt = true;
      await processes.workerReady(root, "tester", "ses_tester", 2);
      const delivered = claim();
      expect(delivered.pendingAssignment).toBeUndefined();
      expect(delivered.promptFailures).toBe(0);
      expect(delivered.promptRetires).toBeUndefined();
      expect(managedState.phases[root]).toEqual({
        phase: "tester",
        sessionId: "ses_tester",
        assignedAt: "2026-08-24T00:00:00.000Z",
      });
      // Only the relaunch published worker-started: a ready-time delivery answers the boot, it
      // publishes nothing (the architect's spawn already answered `spawned`).
      expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(1);

      // It finishes, and the architect's next task is acknowledged without a turn: one failure,
      // counted from zero — no retirement, no escalation.
      relaunched.emitRunState("idle");
      relaunched.turnStartsOnPrompt = false;
      const spawn = processes.spawnWorker(root, root, "tester", "verify #41 again");
      await expireTurnStartWait(sleeps, clock);
      expect(await spawn).toEqual({ status: "queued", roleToken: token });
      const counted = claim();
      expect(counted.locator).toBeDefined();
      expect(counted.promptFailures).toBe(1);
      expect(counted.promptRetires).toBeUndefined();
      expect(counted.pendingAssignment).toMatchObject({
        kind: "assignment",
        task: "verify #41 again",
        queuedAt: "2026-08-24T00:00:00.000Z",
      });
      expect(managedState.workerAdmission.queue).toEqual([token]);
      expect(publications.filter((p) => p.json === workerDiedJson)).toHaveLength(0);
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }
    const token = roleToken("omp", root, "tester");
    expect(errorLines.filter((line) => line === retireLine(token, 1))).toHaveLength(1);
    expect(errorLines.filter((line) => line === retireLine(token, 2))).toHaveLength(0);
  });

  it("answers queued and queues the task when spawn_worker's direct prompt is acknowledged but starts no turn", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ompSessionFile: "/state/workers/tester-session.json",
      },
    };
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    client.turnStartsOnPrompt = false;
    client.getStateImpl = async () => ({ data: { isStreaming: false } });
    const clock = manualSleep();
    const prompted = promptCounter(client);
    const {
      manager: processes,
      state: managedState,
      commands,
      publications,
      sleeps,
    } = manager(state, { connectWorkerRpc: async () => client, sleep: clock.sleep });
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      const result = processes.spawnWorker(root, root, "tester", "verify #55");
      await expireTurnStartWait(sleeps, clock);
      expect(await result).toEqual({ status: "queued", roleToken: token });
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    const claim = testerClaim(managedState, token);
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #55",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    const retryDeliveryId = claim.pendingAssignment?.deliveryId;
    if (!retryDeliveryId) throw new Error("queued retry has no delivery ID");
    expect(client.deliveryIds).toHaveLength(1);
    expect(retryDeliveryId).not.toBe(client.deliveryIds[0]);
    expect(claim.locator).toBeDefined();
    expect(claim.promptFailures).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(managedState.phases[root]).toBeUndefined();
    expect(publications.filter((p) => p.json === workerQueuedJson)).toHaveLength(1);
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(0);
    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    // No immediate retry: the not-started path triggers no drain of its own.
    expect(client.prompts).toEqual(["verify #55"]);
    expect(errorLines).toHaveLength(1);
    expect(errorLines[0]).toContain(token);

    // The next drain retries the queued task; this time the turn starts.
    const retry = processes.reconcileWorkerAdmission();
    // The 2nd prompt (the retry) has been acknowledged; `awaitTurnStart` is racing its bound.
    await prompted.reached(2);
    client.emitRunState("running");
    await retry;

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(testerClaim(managedState, token).pendingAssignment).toBeUndefined();
    expect(testerClaim(managedState, token).promptFailures).toBe(0);
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(1);
  });

  it("queues a catch-up the worker acknowledged without a turn and publishes no worker-queued for it", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ompSessionFile: "/state/workers/tester-session.json",
      },
    };
    // The active phase's own worker, so the catch-up is not a bystander's and is delivered.
    state.phases[root] = { phase: "tester", sessionId: "ses_tester" };
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    client.turnStartsOnPrompt = false;
    client.getStateImpl = async () => ({ data: { isStreaming: false } });
    const clock = manualSleep();
    const {
      manager: processes,
      state: managedState,
      publications,
      sleeps,
    } = manager(state, { connectWorkerRpc: async () => client, sleep: clock.sleep });
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const handled = processes.handleException(exception(token));
      await expireTurnStartWait(sleeps, clock);
      await handled;
    } finally {
      errorLog.mockRestore();
    }

    const claim = testerClaim(managedState, token);
    expect(claim.pendingAssignment?.kind).toBe("catchup");
    expect(claim.promptFailures).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(client.prompts).toEqual([JSON.stringify({ type: "catchup-worker", unhandled: [] })]);
    // The architect did not ask for a catch-up and cannot act on it: no worker-queued.
    expect(publications).toEqual([]);
  });

  it("confirms the boot but queues the assignment and counts a prompt failure when the ready-time prompt is acknowledged without a turn", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      launchFailures: 2,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    client.turnStartsOnPrompt = false;
    // The confirming get_state is what seeds this fresh client from "unknown" to idle — the
    // one genuine idle observation on this path, and what makes the queued retry promotable.
    client.getStateImpl = async () => {
      client.emitRunState("idle");
      return { data: { isStreaming: false } };
    };
    const clock = manualSleep();
    const prompted = promptCounter(client);
    const {
      manager: processes,
      state: managedState,
      publications,
      sleeps,
    } = manager(state, { connectWorkerRpc: async () => client, sleep: clock.sleep });
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      const ready = processes.workerReady(root, "tester", "ses_tester", 1);
      await expireTurnStartWait(sleeps, clock);
      await ready;
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    const claim = testerClaim(managedState, token);
    // The boot did succeed and is confirmed exactly as a delivered prompt would have confirmed it.
    expect(claim.readyConfirmedAt).toBeDefined();
    expect(claim.launchFailures).toBeUndefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.locator).toBeDefined();
    expect(claim.promptFailures).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(managedState.phases[root]).toBeUndefined();
    expect(publications.filter((p) => p.json === workerQueuedJson)).toHaveLength(1);
    expect(client.runState).toBe("idle");
    expect(client.idleFireCount).toBe(1);
    // No immediate retry: the not-started path triggers no drain of its own.
    expect(client.prompts).toEqual(["verify #41"]);
    expect(errorLines).toHaveLength(1);
    expect(errorLines[0]).toContain("boot confirmed, task queued");

    // The next drain retries the queued task; this time the turn starts.
    const retry = processes.reconcileWorkerAdmission();
    // The 2nd prompt (the retry) has been acknowledged; `awaitTurnStart` is racing its bound.
    await prompted.reached(2);
    client.emitRunState("running");
    await retry;

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(testerClaim(managedState, token).pendingAssignment).toBeUndefined();
    expect(testerClaim(managedState, token).promptFailures).toBe(0);
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(1);
  });

  /** A fake tmux for a worker that is launched, dies, and is relaunched: `split-window` reports a
   * new pane each time (`%301`, `%302`, …) under the fixture's default process identity, so every
   * recorded pane verifies against `list-panes`. Records every command it is given (a custom
   * `run` replaces `manager()`'s recording runner, so its `commands` stays empty). */
  function relaunchingTmux(): { run: TmuxRuntimeDeps["run"]; commands: string[][] } {
    let panes = 0;
    const commands: string[][] = [];
    return {
      commands,
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "split-window") {
          panes += 1;
          return { stdout: `%${300 + panes} 12345\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") return livePanes(command);
        return { stdout: "", exitCode: 0 };
      },
    };
  }

  /** A `connectWorkerRpc` that hands out `client` once and refuses every later dial — a shim whose
   * process exited after the daemon's one connection to it. */
  function connectOnce(client: FakeWorkerRpcClient): {
    connect: () => Promise<FakeWorkerRpcClient>;
    attempts: () => number;
  } {
    let attempts = 0;
    return {
      attempts: () => attempts,
      connect: async () => {
        attempts += 1;
        if (attempts > 1) throw new Error("ECONNREFUSED");
        return client;
      },
    };
  }

  /** Makes `client` acknowledge each prompt and then close its socket before any turn starts —
   * a worker that died right after saying "got it". */
  function closeAfterAcknowledging(client: FakeWorkerRpcClient): void {
    client.turnStartsOnPrompt = false;
    const acknowledge = client.prompt.bind(client);
    client.prompt = async (message, deliveryId) => {
      const receipt = await acknowledge(message, deliveryId);
      client.close();
      return receipt;
    };
  }

  function resumeArgument(commands: string[][]): string | undefined {
    return commands
      .filter((command) => command[0] === "tmux" && command[3] === "split-window")
      .map((command) => command.at(-1) ?? "")
      .find((argv) => argv.includes("--resume="));
  }

  for (const workerCap of [1, 2]) {
    it(`retires a ready-time worker whose socket closes after acknowledging its first task and relaunches the task cold with --resume (workerCap ${workerCap})`, async () => {
      const token = roleToken("omp", root, "planner");
      const clock = manualSleep();
      const sessionFile = path.join(await temporaryDir(), "planner-session.jsonl");
      await writeFile(sessionFile, "{}", "utf8");
      const client = fakeWorkerRpcClient();
      closeAfterAcknowledging(client);
      const shim = connectOnce(client);
      const tmuxFake = relaunchingTmux();
      // The relaunch does real filesystem work (the session-file `stat`, the boot-token secret
      // file, the workspace config, the socket dir) before and after its tmux call, so a
      // `setImmediate` budget racing it is load-sensitive. `worker-started` is published only
      // once `launchWorker` has written the fresh claim, dequeued the token, and persisted — the
      // last step of the chain under test — so a waiter resolved from inside the fake publish is
      // the observable end itself, exactly as the queued-promotion tests above await it. A wait
      // that never resolves fails on bun's own timeout with the assertions unreached, never a
      // false green.
      const architectTopic = roleTopic(roleToken("omp", root, "architect"));
      const publications: Array<{ subject: string; json: string }> = [];
      const promoted = Promise.withResolvers<void>();
      const { processes, managedState } = await workerCapFixture(workerCap, {
        sleep: clock.sleep,
        connectWorkerRpc: shim.connect,
        run: tmuxFake.run,
        publishRole: (subject, json) => {
          publications.push({ subject, json });
          if (subject === architectTopic && json.includes("worker-started")) promoted.resolve();
        },
      });
      const claimOf = (): WorkerRoleClaim => {
        const claim = managedState.roles[token];
        if (!claim || !("issue" in claim)) throw new Error("planner claim disappeared");
        return claim;
      };

      expect(await processes.spawnWorker(root, root, "planner", "plan #41")).toEqual({
        status: "spawned",
        roleToken: token,
      });
      const booted = claimOf();
      expect(tmuxFields(booted.locator)?.tmuxPaneId).toBe("%301");
      expect(booted.readyConfirmedAt).toBeUndefined();
      // `/worker/started` registered this generation's session and its OMP session file.
      booted.sessionId = "ses_planner";
      if (!booted.locator) throw new Error("planner locator disappeared");
      booted.locator.ompSessionFile = sessionFile;

      const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
      let errorLines: string[] = [];
      try {
        await processes.workerReady(root, "planner", "ses_planner", 1);
        // The close handler's reconnect (refused), the retirement, the requeue and the relaunch
        // all run detached from `workerReady`; the relaunch's `worker-started` is the observable end.
        await promoted.promise;
      } finally {
        errorLines = errorLog.mock.calls.map((call) => String(call[0]));
        errorLog.mockRestore();
      }

      // Exactly where any dead worker with a pending assignment ends: the dead pane killed, its
      // locator replaced by the relaunch, the task kept for the new boot's `/worker/ready`, the
      // death counted against `launchFailures` (never `promptFailures`), the boot unconfirmed.
      const relaunched = claimOf();
      expect(tmuxFields(relaunched.locator)?.tmuxPaneId).toBe("%302");
      expect(relaunched.generation).toBe(2);
      expect(relaunched.readyConfirmedAt).toBeUndefined();
      expect(relaunched.pendingAssignment).toMatchObject({
        kind: "assignment",
        task: "plan #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
      });
      expect(relaunched.launchFailures).toBe(1);
      expect(relaunched.promptFailures).toBeUndefined();
      expect(managedState.workerAdmission.queue).toEqual([]);
      expect(managedState.phases[root]).toBeUndefined();
      expect(resumeArgument(tmuxFake.commands)).toContain(`--resume=${sessionFile}`);
      expect(
        tmuxFake.commands.some(
          (command) =>
            command[0] === "tmux" && command[3] === "kill-pane" && command.includes("%301")
        )
      ).toBeTrue();
      expect(shim.attempts()).toBeGreaterThanOrEqual(2);
      expect(client.prompts).toEqual(["plan #41"]);
      // The architect hears the relaunch, never a "queued" for a worker that was dead.
      expect(publications.map((p) => JSON.parse(p.json).type)).toEqual(["worker-started"]);
      expect(errorLines.filter((line) => line.includes(token))).toHaveLength(1);
    });
  }

  it("does not count a socket that closes during a queued promotion's wait as a prompt failure: the task stays queued and the next drain re-prompts the reconnected worker", async () => {
    // The shim's socket drops right after it acknowledges the prompt, but the shim is still
    // listening: the close handler's one reconnect succeeds. A socket close is the close
    // handler's event, never a swallowed prompt — nothing is counted against `promptFailures`,
    // the entry stays queued, and the reconnected client's idle seed drives the next drain.
    const reconnected = fakeWorkerRpcClient();
    const reconnectedPrompts = promptCounter(reconnected);
    reconnected.turnStartsOnPrompt = false;
    reconnected.getStateImpl = async () => {
      reconnected.emitRunState("idle");
      return { data: { isStreaming: false } };
    };
    const first = fakeWorkerRpcClient();
    let dials = 0;
    const { processes, managedState, publications, token, client } = await queuedIdleWorkerFixture(
      2,
      {
        connectWorkerRpc: async () => {
          dials += 1;
          return dials === 1 ? first : reconnected;
        },
      },
      first
    );
    closeAfterAcknowledging(client);

    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorLines: string[] = [];
    try {
      await processes.reconcileWorkerAdmission();
      // The retry's prompt on the reconnected socket has been acknowledged and is waiting on its
      // own bound.
      await reconnectedPrompts.reached(1);
    } finally {
      errorLines = errorLog.mock.calls.map((call) => String(call[0]));
      errorLog.mockRestore();
    }

    // The retry is in flight on the reconnected socket, waiting on its own bound; the close
    // itself was counted against nothing.
    const claim = testerClaim(managedState, token);
    expect(claim.promptFailures ?? 0).toBe(0);
    expect(claim.locator).toBeDefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(managedState.phases[root]).toBeUndefined();
    expect(client.prompts).toEqual(["verify #41"]);
    expect(reconnected.prompts).toEqual(["verify #41"]);
    expect(publications).toEqual([]);
    expect(errorLines.filter((line) => line.includes(token))).toHaveLength(1);
  });

  it("credits a turn that starts while the confirming get_state is failing, counting no prompt failure", async () => {
    const { processes, managedState, publications, token, clock, client, sleeps } =
      await queuedIdleWorkerFixture(2);
    client.turnStartsOnPrompt = false;
    client.getStateImpl = async () => {
      // The worker's agent_start lands while get_state is in flight; the call itself then fails.
      client.emitRunState("running");
      throw new Error('Worker RPC "get_state" timed out after 5000ms');
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    let errorCalls = 0;
    try {
      const run = processes.reconcileWorkerAdmission();
      await expireTurnStartWait(sleeps, clock);
      await run;
    } finally {
      errorCalls = errorLog.mock.calls.length;
      errorLog.mockRestore();
    }

    expect(managedState.workerAdmission.queue).toEqual([]);
    const claim = testerClaim(managedState, token);
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.promptFailures).toBe(0);
    expect(managedState.phases[root]).toEqual({
      phase: "tester",
      sessionId: "ses_tester",
      assignedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(publications.filter((p) => p.json === workerStartedJson)).toHaveLength(1);
    expect(publications.filter((p) => p.json === workerQueuedJson)).toHaveLength(0);
    expect(client.prompts).toEqual(["verify #41"]);
    expect(errorCalls).toBe(0);
  });

  it("does not drop a queued idle-resume assignment as stale when its client is alive but not currently idle, only stops the drain until it goes idle", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    const dequeued = Promise.withResolvers<void>();
    const { processes, state, managedState } = await workerCapFixture(2, {
      connectWorkerRpc: async () => client,
      saveState: async () => {
        if (client.prompts.length > 0 && managedState.workerAdmission.queue.length === 0) {
          dequeued.resolve();
        }
      },
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();
    // The client is alive (a real session, confirmed sessionId) but currently mid-turn/busy —
    // not idle, so this queued idle-resume assignment must stay queued and untouched instead of
    // being dropped as stale (only a genuinely dead/never-connected client is stale here).
    client.emitRunState("running");

    await processes.reconcileWorkerAdmission();

    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    expect(claim.locator).toBeDefined();
    expect(client.prompts).toEqual([]);

    // Once it genuinely goes idle, the exact same queued assignment promotes normally. The
    // signal is the persist that follows the dequeue, not a guessed number of ticks.
    client.emitRunState("idle");
    await dequeued.promise;

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(client.prompts).toEqual(["verify #41"]);
  });

  it("leaves a queued idle-resume entry alone when its claim has a session but no ready confirmation, deferring to the ready path/watchdog", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    const { processes, state, managedState } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      // readyConfirmedAt deliberately absent: a restart landed between /worker/ready's ack and
      // its durable confirmation write, so this claim is neither stale (a real session and a
      // live client) nor safely promotable (admission cannot yet trust its readiness).
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    await processes.reconcileWorkerAdmission();

    expect(client.prompts).toEqual([]);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.locator).toBeDefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
  });

  it("rotates an unconfirmed locator-carrying head to the tail, persists that order, and promotes the token behind it in the same pass", async () => {
    // The tester outranks the planner (`workerPriority`), so `orderWorkerQueue` keeps the
    // unconfirmed tester at the head and the clean planner behind it: whatever the planner gets
    // this pass, it gets only because the head was rotated out of its way.
    const unconfirmedToken = roleToken("omp", root, "tester");
    const queuedToken = roleToken("omp", root, "planner");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    const persistedQueues: string[][] = [];
    const { processes, state, managedState } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
      saveState: async () => {
        persistedQueues.push([...managedState.workerAdmission.queue]);
      },
    });
    state.roles[unconfirmedToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      // readyConfirmedAt deliberately absent: a live session and a cached idle client, but the
      // boot is not yet durably confirmed — its ready retry or boot watchdog owns recovery.
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.roles[queuedToken] = {
      issue: root,
      role: "planner",
      pendingAssignment: {
        kind: "assignment",
        task: "plan #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(unconfirmedToken, queuedToken);
    await processes.reconnectWorkers();
    persistedQueues.length = 0;

    await processes.reconcileWorkerAdmission();

    // The unconfirmed head was neither prompted nor dropped: still queued, locator intact.
    expect(client.prompts).toEqual([]);
    const unconfirmedClaim = managedState.roles[unconfirmedToken];
    if (!unconfirmedClaim || !("issue" in unconfirmedClaim)) throw new Error("tester claim gone");
    expect(unconfirmedClaim.locator).toBeDefined();
    expect(unconfirmedClaim.readyConfirmedAt).toBeUndefined();
    expect(unconfirmedClaim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
    });
    // The planner behind it launched in this same pass instead of waiting behind the head.
    const promotedClaim = managedState.roles[queuedToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("planner claim gone");
    expect(promotedClaim.locator).toBeDefined();
    expect(managedState.workerAdmission.queue).toEqual([unconfirmedToken]);
    // Exactly one rotation, persisted before the planner's launch: the first save carries the
    // rotated order, the second the launch's dequeue. Re-peeking the rotated tester as the head
    // again ended the pass (it had already had its turn) rather than rotating it a second time.
    expect(persistedQueues).toEqual([[queuedToken, unconfirmedToken], [unconfirmedToken]]);
  });

  it("stops the drain after one below-threshold failure each for two queued tokens instead of burning every MAX_LAUNCH_FAILURES attempt on both in one pass", async () => {
    // Two planners on two trees: the same `workerPriority` tier, so the queue order below is
    // arrival order and the rotation arithmetic in the comment holds.
    const otherRoot = "LEGION-99";
    const failingTokenA = roleToken("omp", root, "planner");
    const failingTokenB = roleToken("omp", otherRoot, "planner");
    const { processes, state, managedState, stateDir } = await workerCapFixture(1);
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.roles[failingTokenA] = {
      issue: root,
      role: "planner",
      pendingAssignment: {
        kind: "assignment",
        task: "plan #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      // Deterministic, permanent failures (missing session files, never appear on retry) — but
      // launchFailures starts at 0 for both, so one attempt each stays *below* MAX_LAUNCH_FAILURES.
      resumeSessionFile: path.join(stateDir, "missing-planner-session.json"),
    };
    state.roles[failingTokenB] = {
      issue: otherRoot,
      role: "planner",
      pendingAssignment: {
        kind: "assignment",
        task: "plan #99",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      resumeSessionFile: path.join(stateDir, "missing-planner-99-session.json"),
    };
    state.workerAdmission.queue.push(failingTokenA, failingTokenB);

    await processes.reconcileWorkerAdmission();

    // Without the attempted-token tracking, rotating a below-threshold failure to the tail
    // exposes the OTHER token as the new head, and rotating THAT one exposes the first again —
    // the drain would cycle both tokens through every MAX_LAUNCH_FAILURES attempt in this one
    // synchronous pass. With it: each token gets exactly one attempt this pass, both still
    // queued (rotated back to their original order — two rotations of a two-item queue is a
    // no-op on ordering), and the drain stops instead of continuing a third time.
    expect(managedState.workerAdmission.queue).toEqual([failingTokenA, failingTokenB]);
    const claimA = managedState.roles[failingTokenA];
    if (!claimA || !("issue" in claimA)) throw new Error("planner claim missing");
    expect(claimA.launchFailures).toBe(1);
    expect(claimA.locator).toBeUndefined();
    const claimB = managedState.roles[failingTokenB];
    if (!claimB || !("issue" in claimB)) throw new Error("second planner claim missing");
    expect(claimB.launchFailures).toBe(1);
    expect(claimB.locator).toBeUndefined();
  });

  it("treats a saveState failure after a successful launch as a persistence issue, not a launch failure: keeps the locator and pendingAssignment, never bumps launchFailures, never re-queues", async () => {
    // The tester outranks the planner (`workerPriority`), so it is the token attempted first.
    const token = roleToken("omp", root, "tester");
    const secondToken = roleToken("omp", root, "planner");
    let saveStateCalls = 0;
    const saveStateCalled = Promise.withResolvers<void>();
    const releaseSave = Promise.withResolvers<void>();
    const { processes, state, managedState } = await workerCapFixture(1, {
      saveState: async () => {
        saveStateCalls += 1;
        if (saveStateCalls === 1) {
          saveStateCalled.resolve();
          await releaseSave.promise;
          throw new Error("disk full");
        }
        // The one retry `launchWorker`'s own catch attempts also fails here, proving the
        // in-memory claim stays authoritative (never rethrown, never rotated, never retried a
        // second time) even when persistence never recovers within this call.
        throw new Error("disk still full");
      },
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.roles[secondToken] = {
      issue: root,
      role: "planner",
      pendingAssignment: {
        kind: "assignment",
        task: "plan #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
    };
    state.workerAdmission.queue.push(token, secondToken);

    const reconciling = processes.reconcileWorkerAdmission();
    await saveStateCalled.promise;

    // Tester's failing save is still gated. `launchWorker` already spliced tester out of the
    // queue (in memory) before calling this gated save — proving the second queued token is
    // completely untouched while tester's own attempt is in flight: `drainWorkerQueue`'s loop
    // awaits each token's own `promoteQueuedWorker` call fully before peeking the next one.
    expect(managedState.workerAdmission.queue).toEqual([secondToken]);
    const untouchedPlanner = managedState.roles[secondToken];
    if (!untouchedPlanner || !("issue" in untouchedPlanner)) {
      throw new Error("planner claim missing");
    }
    expect(untouchedPlanner.locator).toBeUndefined();

    releaseSave.resolve();
    await reconciling;

    // The pane already exists at this point (a real, running worker-shim process) — a
    // `saveState` failure here is a durable-state persistence issue, not a launch failure:
    // `launchFailures` is never touched at all (a launch success carries forward whatever it
    // was before -- 0 for a claim with no prior failures, exactly as here -- only a durably
    // confirmed `/worker/ready` ever resets it), the token is never
    // re-queued (it already has a live pane; re-queuing it would launch a second pane for the
    // same issue/role the next time it is promoted), and the locator plus `pendingAssignment`
    // stay exactly as `launchWorker` wrote them — the in-memory claim remains authoritative
    // even though both persist attempts failed. The pane's own `/worker/started` ->
    // `/worker/ready` handshake calls back into the daemon independent of this save.
    expect(saveStateCalls).toBe(2);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim missing");
    expect(claim.launchFailures).toBe(0);
    expect(claim.locator).toBeDefined();
    expect(claim.pendingAssignment).toMatchObject({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
    });
    // Tester's own (never-rolled-back) locator keeps cap 1 fully occupied, so the second
    // queued token correctly never promotes either.
    expect(managedState.workerAdmission.queue).toEqual([secondToken]);
  });

  it("boot reconciliation kills an unrecorded worker-shim pane split into a known window, leaves recorded panes (even in an otherwise-unknown window) alone", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    recordedTmuxLocator(state).tmuxPaneId = "%42";
    const recordedPaneId = tmuxFields(state.trees[root]?.locator)?.tmuxPaneId;
    if (!recordedPaneId) throw new Error("test root tree is missing its own recorded pane id");
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-windows") {
          // The tree's own window is known (excluded from `known` in this fixture only via the
          // orphan-pane path below, not this one) — return no unowned windows, isolating this
          // test to pane-level reconciliation only.
          return { stdout: "", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes" && command[4] === "-a") {
          return {
            stdout: [
              // Recorded: this tree's own root pane. Must never be reaped, even tagged with a
              // window id this fixture's `known` set (built from locators only) does not
              // separately special-case — `known.has(paneId)` alone must protect it.
              `${recordedPaneId}\t@42\tlegion-omp\tworker-shim --socket x -- omp --mode rpc\t1000`,
              // Unrecorded worker-shim pane split into the SAME known window @42 — exactly the
              // crash-window orphan item 3 targets: a real process the daemon forgot about.
              `%99\t@42\tlegion-omp\tworker-shim --socket y -- omp --mode rpc\t1000`,
              // A pane in a window owned by a different session entirely — never touched.
              `%50\t@50\tlegion-other\tworker-shim --socket z -- omp --mode rpc\t1000`,
            ].join("\n"),
            exitCode: 0,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconcileOrphans(0);

    const killedPanes = commands
      .filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
      .map((command) => command[5]);
    expect(killedPanes).toEqual(["%99"]);
  });

  it("exempts a known window's panes from reaping when its owning tree/controller locator never recorded a pane id, while still reaping a genuine orphan pane in a fully-recorded known window", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    // `tree()`'s default now carries a `tmuxPaneId` (needed by the graceful-stop tests
    // elsewhere in this file) -- reconstructed without it here to restore the pre-backfill
    // state this exemption protects.
    const rootLocator = recordedTmuxLocator(state);
    const { tmuxPaneId: _rootPaneId, ...rootLocatorWithoutPaneId } = rootLocator;
    const rootTree = state.trees[root];
    if (!rootTree) throw new Error("test setup expects tree() to have recorded a root tree");
    rootTree.locator = rootLocatorWithoutPaneId;
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
      socketPath: "/state/controller.sock",
    };
    const plannerToken = roleToken("omp", root, "planner");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@44",
        tmuxPaneId: "%44",
        socketPath: "/state/workers/planner.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-windows") {
          // Every window here (@42, @43, @44) is known via a recorded `tmuxWindowId` — none are
          // candidates for window-level reaping regardless of whether their owner's pane id is
          // recorded; isolates this test to the pane-level pass.
          return { stdout: "", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes" && command[4] === "-a") {
          return {
            stdout: [
              // Root tree's own pane, in its known window @42 — but this locator never recorded
              // a pane id, so this pane cannot be distinguished from a real orphan by id alone.
              // Must be exempted, not killed.
              `%10\t@42\tlegion-omp\tworker-shim --socket root -- omp --mode rpc\t1000`,
              // Controller's own pane, same situation, in its known window @43.
              `%11\t@43\tlegion-omp\tworker-shim --socket ctrl -- omp --mode rpc\t1000`,
              // Planner's own recorded pane in its fully-recorded known window @44 — never a
              // reap candidate; `known.has(paneId)` alone already protects it.
              `%44\t@44\tlegion-omp\tworker-shim --socket planner -- omp --mode rpc\t1000`,
              // A genuinely unrecorded worker-shim pane split into that SAME fully-recorded
              // window @44 — no ambiguity here (the window's owner's own pane id IS known), so
              // this one is still a real orphan and must be reaped exactly as before.
              `%99\t@44\tlegion-omp\tworker-shim --socket orphan -- omp --mode rpc\t1000`,
            ].join("\n"),
            exitCode: 0,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconcileOrphans(0);

    const killedPanes = commands
      .filter((command) => command[0] === "tmux" && command[3] === "kill-pane")
      .map((command) => command[5]);
    expect(killedPanes).toEqual(["%99"]);
  });

  it("resurrects a dead active root during a resync probe tick, but leaves a live one alone", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const aliveIssue = "LEGION-99" as IssueKey;
    const state = newLegionState("omp", 2);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    state.trees[root].readyConfirmedAt = Date.parse("2026-08-24T00:00:00.000Z");
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    state.issues[aliveIssue] = {
      key: aliveIssue,
      title: "Alive root",
      status: "in_progress",
      children: [],
    };
    state.trees[aliveIssue] = {
      root: aliveIssue,
      generation: 1,
      status: "active",
      launchFailures: 0,
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@43",
        tmuxPaneId: "%1",
        ...paneIdentity(),
      },
    };
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];

    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        // The root's original pane ("%0", from `tree()`) is gone; the untouched second tree's
        // pane ("%1") is still live and running OMP (the default `readProcessCmdline`).
        if (command[0] === "tmux" && command[3] === "list-panes" && command[5] === "%0") {
          return paneGone();
        }
        if (command[0] === "tmux" && command[3] === "list-panes" && command[5] === "%1") {
          return { stdout: "%1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "new-window") {
          return { stdout: "@50 %2 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    const resurrectSpy = vi.spyOn(processes, "resurrect");

    const dispatched: Effect[][] = [];
    await runResync(
      {
        state,
        config: {
          resyncIntervalMs: 600_000,
          dispatchProject: "LEGSMOKE",
          maxFixAttempts: 3,
        },
        dispatchClient: fakeDispatchClient(),
        saveState: async () => {},
        fetchCiStatusBatch: async () => ({}),
        now: () => Date.parse("2026-08-24T00:00:00.000Z"),
        applyEffects: async (effects) => {
          dispatched.push(effects);
          for (const effect of effects) {
            if (effect.kind !== "probe") continue;
            if ((await processes.probe(effect.tree)) === "dead")
              await processes.resurrect(effect.tree);
          }
        },
        reconcileAdmissionDrift: () => processes.reconcileAdmissionDrift(),
        isResurrecting: (issue) => processes.isResurrecting(issue),
      },
      { force: true }
    );

    expect(dispatched).toContainEqual([{ kind: "probe", tree: root }]);
    expect(dispatched).toContainEqual([{ kind: "probe", tree: aliveIssue }]);
    expect(resurrectSpy).toHaveBeenCalledTimes(1);
    expect(resurrectSpy).toHaveBeenCalledWith(root);
    expect(managedState.trees[root]).toMatchObject({ status: "active" });
    expect(statusWrites).toEqual([]);
  });

  it("promotes a queued root when resync removes a stale active admission entry (LEGION-83)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].status = "lingering";
    state.admission.active = [root];
    state.issues[root] = { key: root, title: "Stale root", status: "done", children: [] };
    const queued: IssueKey = "LEGION-45";
    state.issues[queued] = { key: queued, title: "Queued root", status: "todo", children: [] };
    state.trees[queued] = { root: queued, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue = [queued];
    const { manager: processes, commands } = manager(state, { config: config(stateDir) });

    await runResync(
      {
        state,
        config: {
          resyncIntervalMs: 600_000,
          dispatchProject: "LEGSMOKE",
          maxFixAttempts: 3,
        },
        dispatchClient: fakeDispatchClient(),
        saveState: async () => {},
        fetchCiStatusBatch: async () => ({}),
        now: () => Date.parse("2026-08-24T00:00:00.000Z"),
        applyEffects: async () => {},
        reconcileAdmissionDrift: () => processes.reconcileAdmissionDrift(),
        isResurrecting: (issue) => processes.isResurrecting(issue),
      },
      { force: true }
    );
    await processes.drainSpawns();

    expect(state.admission).toEqual({ cap: 1, active: [queued], queue: [] });
    expect(state.trees[queued]).toMatchObject({ status: "active" });
    expect(commands.some((command) => command[3] === "new-window")).toBe(true);
  });
  it("logs and persists a resync admission addition only when it repairs state (LEGION-83)", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active = [];
    let saves = 0;
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const { manager: processes } = manager(state, {
        saveState: async () => {
          saves += 1;
        },
      });

      expect(await processes.reconcileAdmissionDrift()).toEqual({ added: [root], removed: [] });
      expect(state.admission.active).toEqual([root]);
      expect(saves).toBe(1);
      expect(
        errorLog.mock.calls
          .map(String)
          .filter((line) => line.includes("admission drift repaired by resync"))
      ).toHaveLength(1);

      expect(await processes.reconcileAdmissionDrift()).toEqual({ added: [], removed: [] });
      expect(saves).toBe(1);
    } finally {
      errorLog.mockRestore();
    }
  });

  it("warns without stopping or promoting when resync finds occupancy above the cap (LEGION-83, acceptance 4)", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const second: IssueKey = "LEGION-43";
    tree(state, second);
    const waiting: IssueKey = "LEGION-44";
    state.trees[waiting] = { root: waiting, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.active = [root, second];
    state.admission.queue = [waiting];
    let saves = 0;
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const {
        manager: processes,
        commands,
        controlRequests,
      } = manager(state, {
        saveState: async () => {
          saves += 1;
        },
      });

      expect(await processes.reconcileAdmissionDrift()).toEqual({
        added: [],
        removed: [],
        overCap: { active: 2, cap: 1 },
      });
      expect(state.admission).toEqual({ cap: 1, active: [root, second], queue: [waiting] });
      expect(
        commands.some(
          (command) =>
            command[3] === "new-window" ||
            command[3] === "kill-pane" ||
            command[3] === "kill-window"
        )
      ).toBe(false);
      expect(controlRequests).toEqual([]);
      expect(saves).toBe(0);
      expect(
        errorLog.mock.calls
          .map(String)
          .filter((line) => line.includes("admission over cap by resync"))
      ).toHaveLength(1);
    } finally {
      errorLog.mockRestore();
    }
  });

  // `observed` is what the pane itself reports, distinct from the recorded identity every case
  // shares (pid 12345, start 4242): another pid; the same pid with its own start ticks; or, when
  // the process vanished between `list-panes` and the read, an unreadable `/proc` entry -- the
  // only observable fact that case has.
  const reissuedPaneCases: Array<
    [string, { pid: number; startTicks: number | undefined; observed: RegExp }]
  > = [
    [
      "another process took the pane id",
      { pid: 777, startTicks: DEFAULT_START_TICKS, observed: /pid 777/ },
    ],
    [
      "the same pid came back with a different start time",
      { pid: 12345, startTicks: 999_999, observed: /pid 12345 .*999999/ },
    ],
    // The process vanished between `list-panes` and the `/proc` read: unknown is never alive.
    [
      "its /proc stat vanished after list-panes reported it",
      { pid: 12345, startTicks: undefined, observed: /pid 12345 .*\/proc/ },
    ],
  ];
  it.each(
    reissuedPaneCases
  )("probe reports a root dead when its recorded pane now runs another process (%s), and resurrecting it resumes the session without killing that pane", async (_case, reissued) => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state); // %0 in @42, recorded pid 12345 / DEFAULT_START_TICKS
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let log = "";
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      readProcessStat: async (pid) => {
        if (pid !== reissued.pid) return procStat(pid);
        if (reissued.startTicks === undefined) {
          throw Object.assign(new Error("ENOENT"), { code: "ENOENT" });
        }
        return procStat(pid, reissued.startTicks);
      },
      // The stale root's shim socket refuses, so the stop falls straight through to the
      // kill gate this test is about.
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "list-panes") return { stdout: `%0 ${reissued.pid}\n`, exitCode: 0 };
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@43 %5 555\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      expect(await processes.probe(root)).toBe("dead");
      await processes.resurrect(root);
    } finally {
      log = errors.mock.calls.flat().join("\n");
      errors.mockRestore();
    }

    expect(commands.some((c) => c[0] === "tmux" && c[3] === "kill-pane")).toBeFalse();
    const launch = commands.find((c) => c[0] === "tmux" && c[3] === "new-window");
    expect(launch?.at(-1)).toContain(`--resume=${sessionFile}`);
    expect(state.trees[root].locator).toMatchObject({
      tmuxWindowId: "@43",
      tmuxPaneId: "%5",
      panePid: 555,
      paneStartTicks: DEFAULT_START_TICKS,
    });
    // Both identities side by side: what the pane reports now, and what the locator recorded.
    expect(log).toMatch(reissued.observed);
    expect(log).toMatch(/recorded pid 12345 start 4242/);
  });

  it("treats a locator recorded before identity tracking as dead on its first probe -- even though its pane is live and running OMP -- logging that once and resuming the root onto a fresh, fully-recorded pane", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const { panePid: _pid, paneStartTicks: _ticks, ...legacy } = recordedTmuxLocator(state);
    state.trees[root].locator = { ...legacy, ompSessionFile: sessionFile };
    state.admission.active = [root];
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "list-panes") return livePanes(command); // %0 alive, pid 12345, OMP
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@43 %5 555\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });
    try {
      expect(await processes.probe(root)).toBe("dead");
      expect(errors.mock.calls.flat().join("\n")).toMatch(/no recorded process identity/);
      await processes.resurrect(root);
    } finally {
      errors.mockRestore();
    }
    expect(commands.some((c) => c[0] === "tmux" && c[3] === "kill-pane")).toBeFalse();
    expect(commands.find((c) => c[3] === "new-window")?.at(-1)).toContain(
      `--resume=${sessionFile}`
    );
    expect(state.trees[root].locator).toMatchObject({
      panePid: 555,
      paneStartTicks: DEFAULT_START_TICKS,
    });
  });

  it("reconnectWorkers clears a stale unconfirmed claim whose pane id was reissued to a live sibling -- without killing that pane -- and the live claim's re-armed watchdog verifies its pane at the next interval", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const liveToken = roleToken("omp", root, "implementer");
    const staleToken = roleToken("omp", root, "tester");
    const sharedPane = {
      runtime: "tmux" as const,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%7",
    };
    state.roles[liveToken] = {
      issue: root,
      role: "implementer",
      generation: 1,
      pendingAssignment: {
        kind: "assignment",
        task: "implement #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        ...sharedPane,
        socketPath: "/state/workers/live.sock",
        ...paneIdentity(2001, 5000),
      },
    };
    state.roles[staleToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        ...sharedPane,
        socketPath: "/state/workers/stale.sock",
        ...paneIdentity(1001, 3000),
      },
    };
    const stateDir = await temporaryDir();
    const commands: string[][] = [];
    // The live claim's first watchdog interval ends in a re-arm, observed through its log line:
    // the spy gates on it, installed before `reconnectWorkers` so an early line is never missed.
    const rearmed = Promise.withResolvers<void>();
    const errors = vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      if (String(args[0]).includes("re-arming the watch")) rearmed.resolve();
    });
    let log = "";
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const { manager: processes } = manager(
      state,
      {
        config: config(stateDir, {
          workerBootTimeoutSeconds: 1,
          workerBootRegistrationDeadlineIntervals: 1_000,
        }),
        now: () => currentTime,
        sleep: async (ms) => {
          currentTime += ms;
          await onceEventLoop();
        },
        readProcessStat: async (pid) => procStat(pid, pid === 2001 ? 5000 : 3000),
        connectWorkerRpc: async (socketPath) => {
          if (socketPath.endsWith("stale.sock")) throw new Error("ECONNREFUSED");
          return fakeWorkerRpcClient();
        },
        run: async (command) => {
          commands.push(command);
          if (command[0] === "tmux" && command[3] === "list-panes") {
            return { stdout: "%7 2001\n", exitCode: 0 };
          }
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnableLaunches: true }
    );
    try {
      await processes.reconnectWorkers();
      // One watchdog interval for the live claim: its pane verifies (pid 2001 / ticks 5000) so
      // the watch re-arms instead of retiring -- the re-arm log line is the event. The stale
      // claim was already retired inside the awaited `reconnectWorkers()` above.
      await rearmed.promise;
    } finally {
      processes.dispose();
      log = errors.mock.calls.flat().join("\n");
      errors.mockRestore();
    }

    const stale = state.roles[staleToken];
    const live = state.roles[liveToken];
    if (!stale || !("issue" in stale) || !live || !("issue" in live)) {
      throw new Error("claims missing");
    }
    expect(stale.locator).toBeUndefined(); // no longer counts toward the worker cap
    expect(stale.launchFailures).toBe(1);
    expect(state.workerAdmission.queue).toEqual([staleToken]);
    expect(live.locator).toMatchObject({ tmuxPaneId: "%7", panePid: 2001 });
    expect(commands.some((c) => c[0] === "tmux" && c[3] === "kill-pane")).toBeFalse();
    expect(log).toMatch(/recorded pid 1001 start 3000/);
    expect(log).toMatch(/pid 2001/);
  });

  /** Shared by the two registration-deadline-on-a-reissued-pane tests below: a fake tmux whose
   * `list-panes` answers each launched pane with ITS OWN launched pid (so a freshly-recorded
   * identity verifies) until the test reissues a pane id to another process. `resurrected`
   * settles on the `saveState` call that records the resurrected generation's fresh locator --
   * the actual event those tests wait for. */
  function reissuablePanes(
    state: LegionState,
    resurrectedGeneration: number,
    commands: string[][],
    sessionExists: boolean
  ): {
    panes: Map<string, number>;
    windowCount: () => number;
    run: (command: string[]) => Promise<{ stdout: string; exitCode: number }>;
    resurrected: Promise<void>;
    saveState: () => Promise<void>;
  } {
    const panes = new Map<string, number>();
    let windows = 0;
    let hasSession = sessionExists;
    const resurrected = Promise.withResolvers<void>();
    return {
      panes,
      windowCount: () => windows,
      resurrected: resurrected.promise,
      saveState: async () => {
        const tree = state.trees[root];
        const fresh = panes.get(tmuxFields(tree?.locator)?.tmuxPaneId ?? "");
        if (
          tree?.generation === resurrectedGeneration &&
          tree.status === "active" &&
          fresh !== undefined &&
          tmuxFields(tree.locator)?.panePid === fresh
        ) {
          resurrected.resolve();
        }
      },
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: hasSession ? 0 : 1 };
        if (command[3] === "new-session") {
          hasSession = true;
          return { stdout: "", exitCode: 0 };
        }
        if (command[3] === "new-window") {
          windows += 1;
          panes.set(`%${windows}`, 10000 + windows);
          return { stdout: `@4${windows} %${windows} ${10000 + windows}\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "list-panes") {
          const target = command[command.indexOf("-t") + 1];
          const pid = panes.get(target);
          return pid === undefined ? paneGone() : { stdout: `${target} ${pid}\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    };
  }

  it("resurrects a root once, without killing the pane, when its registration deadline elapses on a pane id since reissued to another OMP process", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    let sleepCalls = 0;
    const firstGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const tmuxFake = reissuablePanes(state, 2, commands, false);
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      saveState: tmuxFake.saveState,
      // Only the first armed deadline (generation 1) is under this test's control; the
      // resurrect's own fresh spawn arms a second deadline (generation 2), which must stay
      // pending so the assertions see exactly one retry cycle.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await firstGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: tmuxFake.run,
    });

    try {
      await processes.spawnRoot(root);
      expect(tmuxFake.windowCount()).toBe(1);
      expect(managedState.trees[root]?.locator).toMatchObject({
        tmuxPaneId: "%1",
        panePid: 10001,
        paneStartTicks: DEFAULT_START_TICKS,
      });

      // The pane is still live and running OMP -- but it is another process now (the id was
      // reissued), not the one this locator recorded.
      tmuxFake.panes.set("%1", 777);
      firstGate.resolve();
      await tmuxFake.resurrected;
    } finally {
      errors.mockRestore();
    }

    expect(tmuxFake.windowCount()).toBe(2);
    expect(managedState.trees[root]).toMatchObject({
      generation: 2,
      status: "active",
      launchFailures: 1,
    });
    expect(commands).not.toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
    expect(managedState.trees[root]?.locator).toMatchObject({
      tmuxPaneId: "%2",
      panePid: 10002,
      paneStartTicks: DEFAULT_START_TICKS,
    });
  });

  it("reconnectRoots' re-armed deadline treats a restart-surviving locator whose pane id was reissued as dead: resumes the root once, never killing that pane", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = {
      root,
      generation: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@41",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/architect.sock",
        ompSessionFile: sessionFile,
        ...paneIdentity(10001),
      },
      status: "active",
      launchFailures: 0,
      // No readyConfirmedAt: this tree never reached /process/ready before the restart.
    };
    const sleepGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    const commands: string[][] = [];
    const tmuxFake = reissuablePanes(state, 2, commands, true);
    // As a restart sees it: the recorded pane id is live, but it is some other role's OMP now.
    tmuxFake.panes.set("%1", 777);
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      saveState: tmuxFake.saveState,
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await sleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: tmuxFake.run,
    });

    try {
      processes.reconnectRoots();
      sleepGate.resolve();
      await tmuxFake.resurrected;
    } finally {
      errors.mockRestore();
    }

    expect(tmuxFake.windowCount()).toBe(1);
    expect(managedState.trees[root]).toMatchObject({
      generation: 2,
      status: "active",
      launchFailures: 1,
    });
    expect(commands).not.toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
    const launch = commands.find((c) => c[0] === "tmux" && c[3] === "new-window");
    expect(launch?.at(-1)).toContain(`--resume=${sessionFile}`);
    // The fresh window's pane is `%1` again (ids restart) -- recorded with its own identity.
    expect(managedState.trees[root]?.locator).toMatchObject({
      tmuxWindowId: "@41",
      tmuxPaneId: "%1",
      panePid: 10001,
      paneStartTicks: DEFAULT_START_TICKS,
    });
  });

  it("resurrects every located root with --resume, killing nothing, when the private tmux server was recreated under the daemon and reissued its pane ids", async () => {
    const stateDir = await temporaryDir();
    const second = "LEGION-77" as IssueKey;
    const sessionFiles = {
      [root]: path.join(stateDir, "root-session.json"),
      [second]: path.join(stateDir, "second-session.json"),
    };
    await writeFile(sessionFiles[root], "{}", "utf8");
    await writeFile(sessionFiles[second], "{}", "utf8");
    const state = newLegionState("omp", 2);
    tree(state);
    tree(state, second);
    for (const [issue, windowId, paneId, pid] of [
      [root, "@42", "%0", 12345],
      [second, "@43", "%1", 12346],
    ] as const) {
      const locator = recordedTmuxLocator(state, issue);
      state.trees[issue].locator = {
        ...locator,
        tmuxWindowId: windowId,
        tmuxPaneId: paneId,
        ompSessionFile: sessionFiles[issue],
        ...paneIdentity(pid),
      };
    }
    // Server B, recreated under the daemon: `%0` now belongs to another role's live OMP; `%1`
    // does not exist until the first fresh window claims it again (ids restart from 1).
    const panes = new Map<string, number>([["%0", 999]]);
    let windows = 0;
    const commands: string[][] = [];
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") {
          windows += 1;
          panes.set(`%${windows}`, 5000 + windows);
          return { stdout: `@${windows} %${windows} ${5000 + windows}\n`, exitCode: 0 };
        }
        if (command[3] === "list-panes") {
          const target = command[command.indexOf("-t") + 1];
          const pid = panes.get(target);
          return pid === undefined ? paneGone() : { stdout: `${target} ${pid}\n`, exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      // Exactly the composition index.ts's `onProbe` runs on every resync tick.
      for (const issue of [root, second]) {
        if ((await processes.probe(issue)) === "dead") await processes.resurrect(issue);
      }
    } finally {
      errors.mockRestore();
    }

    expect(commands.some((c) => c[0] === "tmux" && c[3] === "kill-pane")).toBeFalse();
    const launches = commands.filter((c) => c[0] === "tmux" && c[3] === "new-window");
    expect(launches).toHaveLength(2);
    expect(launches[0]?.at(-1)).toContain(`--resume=${sessionFiles[root]}`);
    expect(launches[1]?.at(-1)).toContain(`--resume=${sessionFiles[second]}`);
    expect(state.trees[root]).toMatchObject({
      generation: 2,
      locator: { tmuxWindowId: "@1", tmuxPaneId: "%1", panePid: 5001, paneStartTicks: 4242 },
    });
    // The second root's stale `%1` had been reissued to the first root's fresh pane in between:
    // still dead (pid 5001 is not 12346), still never killed.
    expect(state.trees[second]).toMatchObject({
      generation: 2,
      locator: { tmuxWindowId: "@2", tmuxPaneId: "%2", panePid: 5002, paneStartTicks: 4242 },
    });
  });

  it("never kills a pane whose process is not the one the locator recorded on tree close: treats it as already gone, clears the claim, and logs both identities", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const token = roleToken("omp", child, "implementer");
    state.roles[token] = {
      issue: child,
      role: "implementer",
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/hung.sock",
        ...paneIdentity(12345),
      },
    };
    const commands: string[][] = [];
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let log = "";
    const { manager: processes } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[3] === "list-panes") {
          const target = command[command.indexOf("-t") + 1];
          // The root's own pane is still its recorded process; the worker's `%1` is not.
          return target === "%1" ? { stdout: "%1 777\n", exitCode: 0 } : livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        // The worker's shim is unreachable, so its stop falls straight through to the kill gate;
        // the root closes gracefully over its own socket.
        if (socketPath === "/state/workers/hung.sock") throw new Error("ECONNREFUSED");
        return fakeWorkerRpcClient();
      },
    });

    try {
      await processes.closeTree(root);
    } finally {
      log = errors.mock.calls.flat().join("\n");
      errors.mockRestore();
    }

    expect(commands.filter((c) => c[0] === "tmux" && c[3] === "kill-pane")).toEqual([]);
    expect(state.roles[token]).toBeUndefined(); // claim removed as for an already-gone pane
    expect(state.trees[root]).toMatchObject({ status: "closed" });
    // The observed identity names the pane and the pid it now runs, on one line.
    expect(log).toMatch(/%1[^\n]*pid 777/);
    expect(log).toMatch(/recorded pid 12345 start 4242/);
  });

  function windowReuseFixture(paneRows: string) {
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    tree(state); // root `@42`/`%0`, identity 12345
    state.roles[roleToken("omp", root, "planner")] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
        ...paneIdentity(12346),
      },
    };
    const commands: string[][] = [];
    const run = async (command: string[]) => {
      commands.push(command);
      if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
      if (command[3] === "list-panes") return { stdout: paneRows, exitCode: 0 };
      if (command[3] === "new-window") return { stdout: "@99 %201 12345\n", exitCode: 0 };
      if (command[3] === "split-window") return { stdout: "%202 67890\n", exitCode: 0 };
      return { stdout: "", exitCode: 0 };
    };
    return { state, commands, run };
  }

  it("opens a fresh window for a new worker when the issue's recorded window is live but none of its recorded panes is still the process its locator recorded", async () => {
    const stateDir = await temporaryDir();
    // Both recorded panes in `@42` now report other processes: the ids were reissued.
    const { state, commands, run } = windowReuseFixture("%0 900\n%1 901\n");
    const { manager: processes } = manager(state, { config: config(stateDir), run });

    await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(commands.some((c) => c[0] === "tmux" && c[3] === "new-window")).toBeTrue();
    expect(commands.some((c) => c[0] === "tmux" && c[3] === "split-window")).toBeFalse();
    expect(commands.some((c) => c[0] === "tmux" && c[3] === "select-layout")).toBeFalse();
    const tester = state.roles[roleToken("omp", root, "tester")];
    if (!tester || !("issue" in tester)) throw new Error("tester claim missing");
    expect(tester.locator).toMatchObject({
      tmuxWindowId: "@99",
      tmuxPaneId: "%201",
      panePid: 12345,
      paneStartTicks: DEFAULT_START_TICKS,
    });
  });

  it("splits a new worker into the issue's recorded window when a recorded pane in it still verifies as its recorded process", async () => {
    const stateDir = await temporaryDir();
    const { state, commands, run } = windowReuseFixture("%0 12345\n%1 12346\n");
    const { manager: processes } = manager(state, { config: config(stateDir), run });

    await processes.spawnWorker(root, root, "tester", "verify #41");

    const split = commands.find((c) => c[0] === "tmux" && c[3] === "split-window");
    expect(split).toContain("@42");
    expect(commands.some((c) => c[0] === "tmux" && c[3] === "new-window")).toBeFalse();
    const tester = state.roles[roleToken("omp", root, "tester")];
    if (!tester || !("issue" in tester)) throw new Error("tester claim missing");
    expect(tester.locator).toMatchObject({
      tmuxWindowId: "@42",
      tmuxPaneId: "%202",
      panePid: 67890,
      paneStartTicks: DEFAULT_START_TICKS,
    });
  });

  it("retires an unconfirmed boot at the first watchdog interval -- never re-arming -- once its pane id has been reissued to another OMP process and its socket refuses", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    let reissued = false;
    const commands: string[][] = [];
    // The retirement's own persist -- locator cleared, launch failure counted, token queued, all
    // in one save -- is the event. Polling for the cleared locator instead would race the queue
    // promotion that follows that save: it re-launches the worker (a fresh pane, a fresh watch)
    // and can close the locator-less gap before a poll sees it, so the poll would then observe
    // the *second* watch's retirement a whole interval later.
    const retired = Promise.withResolvers<{ at: number; snapshot: LegionState }>();
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, {
        workerBootTimeoutSeconds: 1,
        workerBootRegistrationDeadlineIntervals: 1_000,
      }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      saveState: async () => {
        // `persist()` saves `deps.state` itself: snapshot it here, at the save.
        const claim = state.roles[token];
        if (
          claim &&
          "issue" in claim &&
          claim.locator === undefined &&
          claim.launchFailures === 1
        ) {
          retired.resolve({ at: currentTime, snapshot: structuredClone(state) });
        }
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        if (command[3] === "list-panes") {
          return reissued ? { stdout: "%1 777\n", exitCode: 0 } : livePanes(command);
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    let retirement: { at: number; snapshot: LegionState };
    try {
      await processes.spawnWorker(root, child, role, "do the work");
      const launched = managedState.roles[token];
      if (!launched || !("issue" in launched)) throw new Error("claim missing");
      expect(launched.locator).toMatchObject({ tmuxPaneId: "%1", panePid: 12345 });

      // Between launch and the first interval, the pane id came to belong to another OMP.
      reissued = true;
      const startTime = currentTime;
      retirement = await retired.promise;
      // Retired within the first interval, not re-armed for a second one.
      expect(retirement.at - startTime).toBeLessThan(2_000);
    } finally {
      errors.mockRestore();
    }

    const claim = retirement.snapshot.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim missing");
    expect(claim.locator).toBeUndefined();
    expect(claim.launchFailures).toBe(1);
    expect(retirement.snapshot.workerAdmission.queue).toEqual([token]);
    expect(commands).not.toContainEqual(["tmux", "-L", "legion-omp", "kill-pane", "-t", "%1"]);
  });

  it("fails a root launch through the ordinary launch-failure rollback when the pane's process is already gone before its identity can be recorded", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      readProcessStat: async () => {
        throw Object.assign(new Error("ENOENT"), { code: "ENOENT" });
      },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow(
      /exited before its process identity could be recorded/
    );

    expect(state.trees[root]).toEqual({
      root,
      generation: 0,
      status: "queued",
      launchFailures: 1,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [root] });
  });

  it("fails a worker launch through the ordinary launch-failure path when the pane's process is already gone before its identity can be recorded", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    tree(state);
    const token = roleToken("omp", root, "tester");
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      readProcessStat: async () => {
        throw Object.assign(new Error("ENOENT"), { code: "ENOENT" });
      },
    });

    await expect(processes.spawnWorker(root, root, "tester", "verify #41")).rejects.toThrow(
      /exited before its process identity could be recorded/
    );

    const claim = state.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim missing");
    expect(claim.launchFailures).toBe(1);
    expect(claim.locator).toBeUndefined();
  });

  /** A controller locator on pane `%1` recorded as pid 12345, where `%1` now reports pid 777;
   * `new-window` hands out a fresh controller pane `@44`/`%3`/3333. `connectWorkerRpc` decides
   * which `stopProcess` branch the stale controller's stop takes. */
  async function reissuedControllerFixture(
    connectWorkerRpc: TmuxRuntimeDeps["connectWorkerRpc"]
  ): Promise<{
    processes: ProcessManager;
    state: LegionState;
    commands: string[][];
    socketPath: string;
    run(): Promise<string>;
  }> {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const socketPath = path.join(stateDir, "workers", "controller.sock");
    state.controllerLocator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath,
      ...paneIdentity(12345),
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc,
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "list-panes") return { stdout: "%1 777\n", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@44 %3 3333\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });
    return {
      processes,
      state: managedState,
      commands,
      socketPath,
      // Runs `ensureController` under a console.error spy and returns everything it logged.
      async run() {
        const errors = vi.spyOn(console, "error").mockImplementation(() => {});
        try {
          await processes.ensureController();
          return errors.mock.calls.flat().join("\n");
        } finally {
          errors.mockRestore();
        }
      },
    };
  }

  it("never kills a controller pane whose id was reissued to another OMP process when its recorded process is unreachable: the identity gate treats it as already gone, logs both identities, and a fresh controller spawns", async () => {
    // The recorded controller's socket refuses, so its stop skips the graceful branch and falls
    // straight through to the kill -- which only the identity gate stands in front of.
    const fixture = await reissuedControllerFixture(async () => {
      throw new Error("ECONNREFUSED");
    });

    const log = await fixture.run();

    expect(fixture.commands.some((c) => c[0] === "tmux" && c[3] === "kill-pane")).toBeFalse();
    const launch = fixture.commands.find((c) => c[0] === "tmux" && c[3] === "new-window");
    expect(launch).toContain("controller");
    expect(fixture.state.controllerLocator).toMatchObject({
      tmuxWindowId: "@44",
      tmuxPaneId: "%3",
      panePid: 3333,
      paneStartTicks: DEFAULT_START_TICKS,
    });
    expect(log).toMatch(/pid 777/);
    expect(log).toMatch(/recorded pid 12345 start 4242/);
  });

  it("asks the recorded controller process to shut down over its own socket before replacing a locator whose pane id was reissued, spawning a fresh controller once it closes", async () => {
    const shutdowns: string[] = [];
    const fixture = await reissuedControllerFixture(async (connected: string) => {
      const client = fakeWorkerRpcClient();
      const shutdown = client.shutdown;
      client.shutdown = () => {
        shutdowns.push(connected);
        shutdown();
      };
      return client;
    });

    const log = await fixture.run();

    // The recorded process was asked to shut down over its own (per-role) socket and closed
    // gracefully, so nothing was left for the kill gate to decide.
    expect(shutdowns).toEqual([fixture.socketPath]);
    expect(fixture.commands.some((c) => c[0] === "tmux" && c[3] === "kill-pane")).toBeFalse();
    expect(fixture.commands.find((c) => c[0] === "tmux" && c[3] === "new-window")).toContain(
      "controller"
    );
    expect(fixture.state.controllerLocator).toMatchObject({
      tmuxWindowId: "@44",
      tmuxPaneId: "%3",
      panePid: 3333,
      paneStartTicks: DEFAULT_START_TICKS,
    });
    expect(log).toMatch(/pid 777/);
    expect(log).toMatch(/recorded pid 12345 start 4242/);
  });

  /** A deployed daemon's state as the v24 upgrade first sees it: every locator live, running
   * OMP, and identity-less. The root lives in `@42`/`%0`; when `withWorker` is set an
   * implementer claim lives beside it in `@42`/`%3`. The fake tmux answers every probed pane as
   * alive (pid 12345, OMP), hands out `@43`/`%5`/555 for a fresh window and `%6`/666 for a split,
   * and reports both windows as this daemon's own with activity long past the sweep's grace.
   * Every shim socket connects and closes gracefully on `shutdown`. */
  async function legacyLiveTreeFixture(withWorker: boolean): Promise<{
    processes: ProcessManager;
    state: LegionState;
    commands: string[][];
    sessionFile: string;
    implementerToken: string;
    shutdowns: string[];
  }> {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    tree(state);
    const { panePid: _pid, paneStartTicks: _ticks, ...legacyRoot } = recordedTmuxLocator(state);
    state.trees[root].locator = { ...legacyRoot, ompSessionFile: sessionFile };
    state.trees[root].readyConfirmedAt = Date.parse("2026-08-24T00:00:00.000Z");
    const implementerToken = roleToken("omp", root, "implementer");
    if (withWorker) {
      state.roles[implementerToken] = {
        issue: root,
        role: "implementer",
        generation: 1,
        sessionId: "ses_implementer",
        readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
        locator: {
          runtime: "tmux",
          tmuxSession: "legion-omp",
          tmuxWindowId: "@42",
          tmuxPaneId: "%3",
          socketPath: "/state/workers/implementer.sock",
        },
      };
    }
    const shutdowns: string[] = [];
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async (socketPath) => {
        const client = fakeWorkerRpcClient();
        const shutdown = client.shutdown;
        client.shutdown = () => {
          shutdowns.push(socketPath);
          shutdown();
        };
        return client;
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        if (command[3] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[3] === "new-window") return { stdout: "@43 %5 555\n", exitCode: 0 };
        if (command[3] === "split-window") return { stdout: "%6 666\n", exitCode: 0 };
        if (command[3] === "list-windows") {
          return { stdout: "@42\tlegion-omp\t0\n@43\tlegion-omp\t0\n", exitCode: 0 };
        }
        if (command[3] === "list-panes" && command[4] === "-a") return { stdout: "", exitCode: 0 };
        if (command[3] === "list-panes") {
          const target = command[command.indexOf("-t") + 1];
          const fresh: Record<string, number> = { "%5": 555, "%6": 666 };
          return livePanes(command, fresh[target] ?? 12345);
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    return { processes, state: managedState, commands, sessionFile, implementerToken, shutdowns };
  }

  it("resurrecting a legacy root beside a live legacy worker leaves that worker's window id untouched, resumes the root in a fresh window, and the sweep never kills the window the worker still lives in", async () => {
    const { processes, state, commands, sessionFile, implementerToken } =
      await legacyLiveTreeFixture(true);
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});

    try {
      // Exactly the composition index.ts's `onProbe` runs on a resync tick.
      if ((await processes.probe(root)) === "dead") await processes.resurrect(root);
      await processes.reconcileOrphans(0);
    } finally {
      errors.mockRestore();
    }

    // The root resumed onto a fresh, fully-recorded pane ...
    const launch = commands.find((c) => c[0] === "tmux" && c[3] === "new-window");
    expect(launch?.at(-1)).toContain(`--resume=${sessionFile}`);
    expect(state.trees[root].locator).toMatchObject({
      tmuxWindowId: "@43",
      tmuxPaneId: "%5",
      panePid: 555,
      paneStartTicks: DEFAULT_START_TICKS,
    });
    // ... while the live legacy worker still records the window its pane actually lives in.
    const implementer = state.roles[implementerToken];
    if (!implementer || !("issue" in implementer)) throw new Error("implementer claim missing");
    expect(implementer.locator).toMatchObject({ tmuxWindowId: "@42", tmuxPaneId: "%3" });
    // `@42` stays a known window, so the sweep -- grace already elapsed -- reaps nothing.
    expect(commands.filter((c) => c[0] === "tmux" && c[3] === "kill-window")).toEqual([]);
    expect(commands.filter((c) => c[0] === "tmux" && c[3] === "kill-pane")).toEqual([]);
  });

  it("spawning a worker on a live legacy tree opens a fresh window without repointing the legacy root, later workers split into that fresh window, and the sweep never kills the legacy root's window", async () => {
    const { processes, state, commands } = await legacyLiveTreeFixture(false);
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});

    try {
      await processes.spawnWorker(root, root, "tester", "verify #41");
      await processes.spawnWorker(root, root, "planner", "plan #41");
      await processes.reconcileOrphans(0);
    } finally {
      errors.mockRestore();
    }

    // The legacy root still records the window its pane actually lives in.
    expect(state.trees[root].locator).toMatchObject({ tmuxWindowId: "@42", tmuxPaneId: "%0" });
    // The tester could verify no recorded pane in `@42`, so it opened `@43`; the planner found
    // the tester's fully-recorded pane there and split into it.
    const tester = state.roles[roleToken("omp", root, "tester")];
    const planner = state.roles[roleToken("omp", root, "planner")];
    if (!tester || !("issue" in tester) || !planner || !("issue" in planner)) {
      throw new Error("worker claims missing");
    }
    expect(tester.locator).toMatchObject({ tmuxWindowId: "@43", tmuxPaneId: "%5", panePid: 555 });
    expect(planner.locator).toMatchObject({ tmuxWindowId: "@43", tmuxPaneId: "%6", panePid: 666 });
    expect(commands.filter((c) => c[0] === "tmux" && c[3] === "new-window")).toHaveLength(1);
    expect(commands.find((c) => c[0] === "tmux" && c[3] === "split-window")).toContain("@43");
    // `@42` stays a known window, so the sweep -- grace already elapsed -- reaps nothing.
    expect(commands.filter((c) => c[0] === "tmux" && c[3] === "kill-window")).toEqual([]);
    expect(commands.filter((c) => c[0] === "tmux" && c[3] === "kill-pane")).toEqual([]);
  });

  it("charges an exhausted ready-delivery cycle once and stops after the claim is superseded", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const clock = manualSleep();
    let attempts = 0;
    const { manager: processes } = manager(state, {
      sleep: clock.sleep,
      connectWorkerRpc: async () => {
        attempts += 1;
        throw new Error("socket refused");
      },
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const ready = processes.workerReady(root, "tester", "ses_tester", 1);
      for (const delay of [5_000, 15_000, 45_000, 90_000, 180_000]) {
        await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === delay));
        expect(clock.fire(delay)).toBeTrue();
      }
      await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === 180_000));
      expect(state.roles[token]?.promptFailures).toBe(1);
      // Five retry lines, then the cycle's single exhausted line names the verdict; the retrier
      // itself logs nothing for the final attempt.
      expect(
        errors.mock.calls.filter((call) => String(call[0]).includes("failed (attempt")).length
      ).toBe(5);
      const workerPrefix = `[legion] failed to deliver worker/ready assignment for ${root}/tester:`;
      expect(errors.mock.calls.filter((call) => call[0] === workerPrefix)).toHaveLength(1);
      expect(errors.mock.calls.filter((call) => String(call[0]).includes("attempt 6/6"))).toEqual(
        []
      );
      expect(
        errors.mock.calls.filter((call) => String(call[0]).includes("exhausted cycle"))
      ).toEqual([
        [
          `[legion] worker/ready delivery for ${root}/tester exhausted cycle 1 (6 attempts; last: socket refused); prompt failure 1/3`,
        ],
      ]);

      expect(clock.fire(180_000)).toBeTrue();
      await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === 5_000));
      expect(errors.mock.calls.filter((call) => call[0] === workerPrefix)).toHaveLength(2);
      const claim = state.roles[token];
      if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
      claim.generation = 2;
      expect(clock.fire(5_000)).toBeTrue();
      await ready;
      expect(attempts).toBe(7);
    } finally {
      errors.mockRestore();
      processes.dispose();
    }
  });

  it("cancels a ready-delivery cycle gap without making another attempt", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const clock = manualSleep();
    let attempts = 0;
    const { manager: processes } = manager(state, {
      sleep: clock.sleep,
      connectWorkerRpc: async () => {
        attempts += 1;
        throw new Error("socket refused");
      },
    });
    const ready = processes.workerReady(root, "tester", "ses_tester", 1);
    for (const delay of [5_000, 15_000, 45_000, 90_000, 180_000]) {
      await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === delay));
      expect(clock.fire(delay)).toBeTrue();
    }
    await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === 180_000));
    processes.dispose();
    expect(clock.fire(180_000)).toBeTrue();
    await ready;
    expect(attempts).toBe(6);
  });

  it("keeps a ready-delivery cycle alive when breaker retirement cannot stop its pane", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      promptFailures: 2,
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        panePid: 12345,
        paneStartTicks: DEFAULT_START_TICKS,
      },
    };
    const clock = manualSleep();
    const { manager: processes } = manager(state, {
      sleep: clock.sleep,
      connectWorkerRpc: async () => {
        throw new Error("socket refused");
      },
      run: async (command) =>
        command.includes("metaedit")
          ? { stdout: "", stderr: "", exitCode: 0 }
          : { stdout: "", stderr: "tmux: unable to kill pane", exitCode: 1 },
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const ready = processes.workerReady(root, "tester", "ses_tester", 1);
      for (const delay of [5_000, 15_000, 45_000, 90_000, 180_000]) {
        await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === delay));
        expect(clock.fire(delay)).toBeTrue();
      }
      await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === 180_000));
      const claim = state.roles[token];
      if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
      expect(claim.promptRetires).toBeUndefined();
      expect(claim.locator).toBeDefined();
      expect(claim.promptFailures).toBe(2);
      expect(
        errors.mock.calls.some((call) => String(call[0]).includes("could not retire"))
      ).toBeTrue();
      processes.dispose();
      expect(clock.fire(180_000)).toBeTrue();
      await ready;
    } finally {
      errors.mockRestore();
      processes.dispose();
    }
  });

  it("promotes a cold resume after the ready-delivery breaker retires its third failed cycle", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    const token = roleToken("omp", root, "tester");
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "tester-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      promptFailures: 2,
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ompSessionFile: sessionFile,
      },
    };
    const clock = manualSleep();
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      sleep: clock.sleep,
      connectWorkerRpc: async () => {
        throw new Error("socket refused");
      },
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const ready = processes.workerReady(root, "tester", "ses_tester", 1);
      for (const delay of [5_000, 15_000, 45_000, 90_000, 180_000]) {
        await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === delay));
        expect(clock.fire(delay)).toBeTrue();
      }
      await ready;
      const retired = state.roles[token];
      if (!retired || !("issue" in retired)) throw new Error("worker claim disappeared");
      expect(retired.locator).toBeUndefined();
      expect(retired.promptFailures).toBe(0);
      expect(retired.promptRetires).toBe(1);
      expect(state.workerAdmission.queue).toEqual([token]);
      await processes.reconcileWorkerAdmission();
      expect(
        commands.filter((command) => command[0] === "tmux" && command[3] === "new-window")
      ).toHaveLength(1);
      const claim = state.roles[token];
      if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
      expect(claim.generation).toBe(2);
      expect(claim.locator).toBeDefined();
      expect(state.workerAdmission.queue).toEqual([]);
      expect(
        commands.some((command) => command.join(" ").includes(`--resume=${sessionFile}`))
      ).toBeTrue();
    } finally {
      errors.mockRestore();
      processes.dispose();
    }
  });

  it("closing a tree whose live legacy root has no recorded identity still asks that root to shut down over its own socket, kills nothing, and closes the tree", async () => {
    const { processes, state, commands, shutdowns } = await legacyLiveTreeFixture(false);
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});

    try {
      await processes.closeTree(root);
    } finally {
      errors.mockRestore();
    }

    expect(shutdowns).toEqual(["/state/workers/architect.sock"]);
    expect(commands.filter((c) => c[0] === "tmux" && c[3] === "kill-pane")).toEqual([]);
    expect(state.trees[root]).toMatchObject({ status: "closed" });
    expect(state.trees[root].locator).toBeUndefined();
  });
  it("requeues a committed assignment after the client reports a late refusal", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "verify #41",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, publications } = manager(state, {
      connectWorkerRpc: async () => client,
    });
    await processes.workerReady(root, "tester", "ses_tester", 1);
    client.emitLateRefusal();
    await flushEventLoopUntil(() => {
      const current = state.roles[token];
      return current !== undefined && "issue" in current && current.pendingAssignment !== undefined;
    });
    expect(state.phases[root]).toBeUndefined();
    const claim = state.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toEqual({
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: expect.any(String),
    });
    expect(claim.pendingAssignment?.deliveryId).not.toBe(TEST_DELIVERY_ID);
    expect(claim.promptFailures).toBe(1);
    expect(publications.some(({ json }) => json.includes('"worker-queued"'))).toBeTrue();
  });
  it("requeues a late-refused catchup without deleting the active phase", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.phases[root] = { phase: "tester", sessionId: "ses_existing" };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "catchup",
        task: "catch up",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: TEST_DELIVERY_ID,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, { connectWorkerRpc: async () => client });
    await processes.workerReady(root, "tester", "ses_tester", 1);
    client.emitLateRefusal();
    await flushEventLoopUntil(() => {
      const current = state.roles[token];
      return current !== undefined && "issue" in current && current.pendingAssignment !== undefined;
    });
    expect(state.phases[root]).toEqual({ phase: "tester", sessionId: "ses_existing" });
    const claim = state.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toEqual({
      kind: "catchup",
      task: "catch up",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: expect.any(String),
    });
  });
  it("adopts a replacement ready assignment before its retry prompt, but does not re-adopt the same delivery", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    const firstDelivery = "00000000-0000-4000-8000-000000000041";
    const secondDelivery = "00000000-0000-4000-8000-000000000042";
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: {
        kind: "assignment",
        task: "first",
        queuedAt: "2026-08-24T00:00:00.000Z",
        deliveryId: firstDelivery,
      },
      locator: {
        runtime: "tmux",
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const clock = manualSleep();
    const client = fakeWorkerRpcClient();
    const { metaedits, run } = recordingMetaedits(client);
    let connects = 0;
    const { manager: processes } = manager(state, {
      sleep: clock.sleep,
      run,
      connectWorkerRpc: async () => {
        if (connects++ === 0) {
          const claim = state.roles[token];
          if (claim && "issue" in claim) {
            claim.pendingAssignment = {
              kind: "assignment",
              task: "second",
              queuedAt: "2026-08-24T00:00:01.000Z",
              deliveryId: secondDelivery,
            };
          }
          throw new Error("refused");
        }
        return client;
      },
    });
    const ready = processes.workerReady(root, "tester", "ses_tester", 1);
    await flushEventLoopUntil(() => clock.pending.some((wait) => wait.ms === 5_000));
    expect(clock.fire(5_000)).toBeTrue();
    await ready;
    expect(metaedits).toHaveLength(2);
    expect(metaedits[1]?.promptsBefore).toBe(0);
    expect(client.prompts).toEqual(["second"]);
  });
});
