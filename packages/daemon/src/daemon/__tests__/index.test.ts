import { describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, roleTopic } from "@legion/contracts";
import { getPluginsNodeModules } from "@oh-my-pi/pi-utils/dirs";
import type { CommandRunner, CommandRunnerOptions } from "../../state/fetch";
import type { DaemonConfig } from "../config";
import type { DaemonEnvironment } from "../environment";
import * as daemonIndex from "../index";
import { newLegionState } from "../legion-state";
import type { DurableMessageControl } from "../nats-transport";
import type { WorkerRpcClient } from "../worker-rpc";
import { fakeDispatchClient } from "./ci-fixtures";

const { startDaemon } = daemonIndex;

/** Yields once to the event loop's macrotask queue (never a wall-clock-bound wait --
 * `setImmediate` fires on the next tick, whatever that costs). */
function onceEventLoop(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** Polls `condition` across real macrotask ticks until it is true, rather than guessing a fixed
 * number of ticks (or, worse, a fixed real-time duration) is enough: the awaited chain here is a
 * boot-time fire-and-forget `ensureController()` call built entirely from mocked, instantly-
 * resolving dependencies, so under ordinary conditions this converges within one or two ticks --
 * but a single guessed tick is not a bound, only a guess, and a CPU-starved host can genuinely
 * need more than one before the condition is actually observable. */
async function flushEventLoopUntil(condition: () => boolean, maxTicks = 20_000): Promise<void> {
  for (let tick = 0; tick < maxTicks && !condition(); tick += 1) {
    await onceEventLoop();
  }
}

class FakeNats {
  readonly subscriptions: Array<{
    subject: string;
    callback: (subject: string, data: string, control: DurableMessageControl) => void;
  }> = [];
  readonly publications: Array<{ subject: string; data: string }> = [];
  closed = false;
  readyCalls = 0;

  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void {
    const subscription = {
      subject,
      callback: (s: string, d: string) => callback(s, d),
    };
    this.subscriptions.push(subscription);
    return () => {
      const index = this.subscriptions.indexOf(subscription);
      if (index >= 0) this.subscriptions.splice(index, 1);
    };
  }

  consumeDurable(
    _stream: string,
    _durable: string,
    filterSubjects: string[],
    callback: (subject: string, data: string, control: DurableMessageControl) => void
  ): () => void {
    const registered = filterSubjects.map((subject) => {
      const subscription = { subject, callback };
      this.subscriptions.push(subscription);
      return subscription;
    });
    return () => {
      for (const subscription of registered) {
        const index = this.subscriptions.indexOf(subscription);
        if (index >= 0) this.subscriptions.splice(index, 1);
      }
    };
  }

  publish(subject: string, data: string): void {
    this.publications.push({ subject, data });
  }
  async request(_subject: string, _data: string): Promise<string> {
    return JSON.stringify({ type: "ack" });
  }

  emit(subject: string, data: string, ack: () => void = () => {}): void {
    const control: DurableMessageControl = {
      streamSequence: 1,
      deliverySequence: 1,
      ack,
      nak: () => {},
      term: () => {},
    };
    for (const subscription of this.subscriptions) {
      if (matches(subscription.subject, subject)) subscription.callback(subject, data, control);
    }
  }
  async close(): Promise<void> {
    this.closed = true;
  }
  async ready(): Promise<void> {
    this.readyCalls += 1;
  }
  async flush(): Promise<void> {}
}

function matches(pattern: string, subject: string): boolean {
  const patternTokens = pattern.split(".");
  const subjectTokens = subject.split(".");
  for (let index = 0; index < patternTokens.length; index += 1) {
    const token = patternTokens[index];
    if (token === ">") return index < subjectTokens.length;
    if (token !== "*" && token !== subjectTokens[index]) return false;
  }
  return patternTokens.length === subjectTokens.length;
}

function controllerException(
  project: string,
  reason: "no_holder" | "delivery_failed" = "no_holder"
): string {
  const controller = controllerToken(project);
  return JSON.stringify({
    event_id: "controller-exception",
    source: "github",
    source_event_id: "controller-exception",
    topic: "notifications.github.acme.widgets.issue.42",
    dedupe_key: "controller-exception",
    issued_at: 1_000,
    payload_summary: "controller unavailable",
    payload: JSON.stringify({
      original_topic: roleTopic(controller),
      event_id: "lost-triage",
      reason,
      payload: JSON.stringify({
        type: "triage",
        issue: "WIDGETS-42",
        preexistingChildren: [],
      }),
    }),
    trace_id: "controller-exception",
  });
}

function daemonTestDependencies(
  nats: FakeNats,
  publications: Array<{ topic: string; payload: unknown }>,
  onControllerSecret: (secret: string) => void
): daemonIndex.DaemonStartOptions {
  return {
    deps: {
      createNatsTransport: async () => nats,
      runner: async (command) => {
        if (command[0] === "sh") {
          return {
            stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
            stderr: "",
            exitCode: 0,
          };
        }
        const controllerSecret = command.find((part) =>
          part.startsWith("LEGION_CONTROLLER_SECRET=")
        );
        if (controllerSecret)
          onControllerSecret(controllerSecret.slice("LEGION_CONTROLLER_SECRET=".length));
        if (command[0]?.endsWith("/tmux") && command[1] === "has-session") {
          return { stdout: "", stderr: "", exitCode: 1 };
        }
        if (command[0]?.endsWith("/tmux") && command[1] === "new-session") {
          return { stdout: "@42 %1 4242", stderr: "", exitCode: 0 };
        }
        if (command[0]?.endsWith("/tmux") && command[1] === "new-window") {
          return { stdout: "@42 %1 12345", stderr: "", exitCode: 0 };
        }
        return { stdout: "", stderr: "", exitCode: 0 };
      },
      resolveDaemonEnvironment: async () => daemonEnvironment,
      connectWorkerRpc: async (): Promise<WorkerRpcClient> => {
        const closed = Promise.withResolvers<void>();
        return {
          closed: closed.promise,
          runState: "idle",
          negotiate: async () => {},
          prompt: async () => {},
          getState: async () => ({}),
          shutdown: () => {},
          close: () => closed.resolve(),
          onIdle: () => {},
        };
      },
      statPrompt: async () => {},
      readPluginManifest: async () => validLegionPluginManifest,
      envoyPublish: async (topic, payload) => {
        publications.push({ topic, payload: JSON.parse(payload) });
      },
      dispatchClient: fakeDispatchClient(),
      tokenManager: {
        getToken: async () => ({
          token: "test-token",
          expiresAt: "2026-08-25T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "1+legion-implement[bot]@users.noreply.github.com",
          },
        }),
      },
      setTimeout: () => 1 as never,
      clearTimeout: () => {},
      setInterval: () => 1 as never,
      clearInterval: () => {},
      onSignal: () => {},
      exit: () => {},
      now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    },
  };
}

function config(stateDir: string): DaemonConfig {
  return {
    project: "acme1",
    legionId: "acme/1",
    port: 0,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
    ompLaunchPrefix: [],
    dispatchProject: "LEGSMOKE",
    repos: ["acme/widgets"],
    repo: "acme/widgets",
    appLogins: ["legion-implement[bot]", "legion-review[bot]"],
    admissionCap: 4,
    workerCap: 6,
    maxRecursionDepth: 8,
    lingerHours: 72,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    workerStopTimeoutSeconds: 10,
    treeStopTimeoutSeconds: 60,
    workerBootTimeoutSeconds: 120,
    workerBootRegistrationDeadlineIntervals: 3,
    workerRpcTimeoutSeconds: 5,
    gates: { design: "root-issues", merge: "human" },
    githubApps: { implement: { appId: "1", privateKey: "test", installations: {} } },
    stateDir,
  };
}

const validLegionPluginManifest = JSON.stringify({
  version: "0.9.0",
  omp: { extensions: ["dist/envoy.js", "dist/legion.js"] },
});

const daemonEnvironment: DaemonEnvironment = {
  commands: {
    jj: "/tools/jj",
    git: "/tools/git",
    gh: "/tools/gh",
    tmux: "/tools/tmux",
  },
  ompInvocation: "/tools/omp",
  paneEnv: { PATH: "/full/bin:/usr/bin" },
};

describe("startDaemon", () => {
  it("runs CI reconciliation queries with each PR owner's implementer App token", async () => {
    const commandOptions: CommandRunnerOptions[] = [];
    const runner: CommandRunner = async (_command, options) => {
      if (options) commandOptions.push(options);
      return {
        stdout: JSON.stringify({ data: { repo0: { pr0: null } } }),
        stderr: "",
        exitCode: 0,
      };
    };
    const tokenCalls: Array<{ role: string; owner: string }> = [];
    const tokenManager = {
      getToken: async (role: "implement" | "review", owner: string) => {
        tokenCalls.push({ role, owner });
        return {
          token: `ghs_${owner}_app_token`,
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "3202636+legion-implement[bot]@users.noreply.github.com",
          },
        };
      },
    };

    await daemonIndex.createCiStatusFetcher(
      tokenManager,
      runner
    )({
      "acme/api#1": { owner: "acme", repo: "api", number: 1 },
      "other/web#2": { owner: "other", repo: "web", number: 2 },
    });

    expect(tokenCalls).toEqual([
      { role: "implement", owner: "acme" },
      { role: "implement", owner: "other" },
    ]);
    expect(commandOptions).toHaveLength(2);
    expect(commandOptions.map((options) => options.env?.GH_TOKEN)).toEqual([
      "ghs_acme_app_token",
      "ghs_other_app_token",
    ]);
  });

  it("does not resolve until boot-time admission reconciliation, including its tmux orphan reap, has settled", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const issue = "WIDGETS-42";
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    state.issues[issue] = {
      key: issue,
      title: "Queued at boot",
      status: "todo",
      children: [],
    };
    state.trees[issue] = {
      root: issue,
      generation: 0,
      status: "queued",
      launchFailures: 0,
    };
    state.admission.queue.push(issue);
    await mkdir(path.join(stateDir, "repos", "github.com", "acme", "widgets", ".jj"), {
      recursive: true,
    });
    const listWindowsGate = Promise.withResolvers<void>();
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      const starting = startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {},
          createNatsTransport: async () => new FakeNats(),
          runner: async (command) => {
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            if (command[0]?.endsWith("/jj") && command[1] === "workspace" && command[2] === "add") {
              const workspaceDir = command[3];
              if (!workspaceDir) throw new Error("Jujutsu workspace is missing its destination");
              await mkdir(workspaceDir, { recursive: true });
            }
            if (command[0]?.endsWith("/tmux") && command[1] === "list-windows") {
              // reconcileAdmission's boot-time orphan reap: slow to prove
              // startDaemon does not resolve until it — and every promotion
              // it gates — has fully settled.
              await listWindowsGate.promise;
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[1] === "has-session") {
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (
              command[0]?.endsWith("/tmux") &&
              (command[1] === "new-session" || command[1] === "new-window")
            ) {
              return { stdout: "@42 %1 4242", stderr: "", exitCode: 0 };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          tokenManager: {
            getToken: async () => ({
              token: "test-token",
              expiresAt: "2026-08-25T00:00:00.000Z",
              gitIdentity: {
                name: "legion-implement[bot]",
                email: "1+legion-implement[bot]@users.noreply.github.com",
              },
            }),
          },
          setTimeout: () => 1 as never,
          clearTimeout: () => {},
          setInterval: () => 1 as never,
          clearInterval: () => {},
          onSignal: () => {},
          exit: () => {},
          now: () => Date.parse("2026-08-24T00:00:00.000Z"),
        },
      });

      let started = false;
      void starting.then((handle) => {
        started = true;
        daemon = handle;
      });

      // The reap's list-windows call is gated shut, so reconcileAdmission
      // cannot have settled yet — no real wait needed to know this.
      expect(started).toBe(false);

      listWindowsGate.resolve();
      daemon = await starting;
      expect(started).toBe(true);
      expect(state.admission.active).toEqual([issue]);
      expect(state.trees[issue]?.status).toBe("active");
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("arms a restored root's registration deadline before boot-time admission reconciliation settles", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const queuedIssue = "WIDGETS-42";
    const restoredIssue = "WIDGETS-43";
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    state.issues[queuedIssue] = {
      key: queuedIssue,
      title: "Queued at boot",
      status: "todo",
      children: [],
    };
    state.trees[queuedIssue] = {
      root: queuedIssue,
      generation: 0,
      status: "queued",
      launchFailures: 0,
    };
    state.admission.queue.push(queuedIssue);
    state.issues[restoredIssue] = {
      key: restoredIssue,
      title: "Restored, never confirmed before this restart",
      status: "in_progress",
      children: [],
    };
    state.trees[restoredIssue] = {
      root: restoredIssue,
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@41",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/architect.sock",
      },
      status: "active",
      launchFailures: 0,
      // No readyConfirmedAt: this tree never reached /process/ready before the restart this
      // test simulates.
    };
    state.admission.active.push(restoredIssue);
    await mkdir(path.join(stateDir, "repos", "github.com", "acme", "widgets", ".jj"), {
      recursive: true,
    });
    const listWindowsGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    let sleepCallsBeforeReapSettled = -1;
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      const starting = startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {},
          createNatsTransport: async () => new FakeNats(),
          sleep: async () => {
            sleepCalls += 1;
            await new Promise<void>(() => {});
          },
          runner: async (command) => {
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            if (command[0]?.endsWith("/jj") && command[1] === "workspace" && command[2] === "add") {
              const workspaceDir = command[3];
              if (!workspaceDir) throw new Error("Jujutsu workspace is missing its destination");
              await mkdir(workspaceDir, { recursive: true });
            }
            if (command[0]?.endsWith("/tmux") && command[1] === "list-windows") {
              // reconcileAdmission's boot-time orphan reap: slow enough to observe whether
              // reconnectRoots (synchronous, no tmux calls of its own) has already armed the
              // restored tree's deadline before this settles.
              sleepCallsBeforeReapSettled = sleepCalls;
              await listWindowsGate.promise;
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[1] === "has-session") {
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (
              command[0]?.endsWith("/tmux") &&
              (command[1] === "new-session" || command[1] === "new-window")
            ) {
              return { stdout: "@42 %1 4242", stderr: "", exitCode: 0 };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          tokenManager: {
            getToken: async () => ({
              token: "test-token",
              expiresAt: "2026-08-25T00:00:00.000Z",
              gitIdentity: {
                name: "legion-implement[bot]",
                email: "1+legion-implement[bot]@users.noreply.github.com",
              },
            }),
          },
          setTimeout: () => 1 as never,
          clearTimeout: () => {},
          setInterval: () => 1 as never,
          clearInterval: () => {},
          onSignal: () => {},
          exit: () => {},
          now: () => Date.parse("2026-08-24T00:00:00.000Z"),
        },
      });

      let started = false;
      void starting.then((handle) => {
        started = true;
        daemon = handle;
      });

      expect(started).toBe(false);

      listWindowsGate.resolve();
      daemon = await starting;
      expect(started).toBe(true);
      // The restored tree's deadline (one `sleep` call) was already armed before the reap's
      // gated `list-windows` call settled -- i.e. before `reconcileAdmission`'s promotion
      // cascade for the queued tree ever ran, not merely by the time the whole boot finished.
      expect(sleepCallsBeforeReapSettled).toBe(1);
      expect(state.admission.active).toEqual([restoredIssue, queuedIssue]);
      expect(state.trees[queuedIssue]?.status).toBe("active");
      expect(state.trees[restoredIssue]?.readyConfirmedAt).toBeUndefined();
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("logs an owner CI fetch failure instead of treating its PR as closed", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const prIssue = "WIDGETS-42";
    state.prs["acme/widgets#7"] = {
      key: prIssue,
      repo: "acme/widgets",
      number: 7,
      headSha: "head-1",
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 900 }],
      ciSettlementGeneration: null,
      ciSnapshot: null,
      ciReconciled: false,
      fixAttempts: 0,
    };
    const logs: string[] = [];
    const originalLog = console.log;
    let resync: (() => void) | undefined;
    let resyncComplete: Promise<void> | undefined;
    let daemon: daemonIndex.DaemonHandle | undefined;
    let tokenCalls = 0;
    console.log = (...values: unknown[]) => logs.push(values.join(" "));

    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {},
          createNatsTransport: async () => new FakeNats(),
          runner: async (command) => ({
            stdout:
              command[0] === "sh" ? "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n" : "",
            stderr: "",
            exitCode: 0,
          }),
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          readPluginManifest: async () => validLegionPluginManifest,
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          tokenManager: {
            getToken: async () => {
              tokenCalls += 1;
              if (tokenCalls > 2) throw new Error("GitHub App token request failed");
              return {
                token: "test-token",
                expiresAt: "2026-08-25T00:00:00.000Z",
                gitIdentity: {
                  name: "legion-implement[bot]",
                  email: "1+legion-implement[bot]@users.noreply.github.com",
                },
              };
            },
          },
          setTimeout: (callback) => {
            resync = () => {
              resyncComplete = Promise.resolve().then(callback);
            };
            return 1 as never;
          },
          clearTimeout: () => {},
          setInterval: () => 1 as never,
          clearInterval: () => {},
          onSignal: () => {},
          exit: () => {},
          now: () => Date.parse("2026-08-24T00:00:00.000Z"),
        },
      });

      if (!resync) throw new Error("Daemon did not schedule resync");
      resync();
      if (!resyncComplete) throw new Error("Daemon did not start resync");
      await resyncComplete;

      expect(state.prs["acme/widgets#7"]).toMatchObject({
        verdict: "green",
        ciCheckRuns: [{ name: "build", id: 900 }],
      });
      expect(logs).toContain(
        "[legion] resync complete: anomalies=0 healed=0 ciFetchFailures=1 ciFetchFailureDetails=owner=acme error=GitHub App token request failed"
      );
    } finally {
      await daemon?.stop();
      console.log = originalLog;
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("spins up the controller from a delivery exception, and its capability survives a restart", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const firstNats = new FakeNats();
    const secondNats = new FakeNats();
    const publications: Array<{ topic: string; payload: unknown }> = [];
    let controllerSecret: string | undefined;
    let first: daemonIndex.DaemonHandle | undefined;
    let second: daemonIndex.DaemonHandle | undefined;

    try {
      first = await startDaemon(
        daemonConfig,
        daemonTestDependencies(firstNats, publications, (secret) => {
          controllerSecret = secret;
        })
      );
      const controller = controllerToken(daemonConfig.project);
      firstNats.emit(
        `notifications.envoy.exceptions.notifications.role.${controller}`,
        controllerException(daemonConfig.project)
      );
      await first.drain();
      // No raw event is ever held or replayed: the exception's only durable effect is spinning
      // up the controller (see processes.ts's handleException, "controller" branch).
      expect(controllerSecret).toBeString();
      expect(publications).toEqual([]);
      await first.stop();
      first = undefined;

      // The minted controllerCapabilityHash is durable state, so the same secret still
      // authenticates a fresh daemon process after a restart.
      second = await startDaemon(
        daemonConfig,
        daemonTestDependencies(secondNats, publications, () => {})
      );
      const beforeReady = await fetch(`http://127.0.0.1:${second.server.port}/legion/v1/state`);
      expect(beforeReady.status).toBe(200);
      expect(await beforeReady.json()).toMatchObject({ project: daemonConfig.project });

      const ready = async () =>
        fetch(`http://127.0.0.1:${second?.server.port}/legion/v1/controller/ready`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            secret: controllerSecret,
            sessionId: "ses-controller",
          }),
        });
      expect((await ready()).status).toBe(200);
      expect((await ready()).status).toBe(200);
      const afterReady = await fetch(`http://127.0.0.1:${second.server.port}/legion/v1/state`);
      expect(await afterReady.json()).toMatchObject({ project: daemonConfig.project });
    } finally {
      await first?.stop();
      await second?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("forces a fresh anomaly resync every time a controller claims its role after a delivery_failed exception, never trusting the resync interval to have elapsed", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const untriagedRoot = "WIDGETS-42";
    state.issues[untriagedRoot] = {
      key: untriagedRoot,
      title: "Untriaged root",
      status: "triage",
      children: [],
    };
    const firstNats = new FakeNats();
    const secondNats = new FakeNats();
    const publications: Array<{ topic: string; payload: unknown }> = [];
    let controllerSecret: string | undefined;
    let first: daemonIndex.DaemonHandle | undefined;
    let second: daemonIndex.DaemonHandle | undefined;
    const resyncPayload = {
      type: "resync",
      anomalies: [
        {
          kind: "untriaged-open",
          issue: untriagedRoot,
          detail: "tracked triage issue has no Legion tree or admission entry",
        },
      ],
      healed: 0,
      ciFetchFailures: 0,
      ciFetchFailureDetails: [],
    };

    try {
      const firstOptions = daemonTestDependencies(firstNats, publications, (secret) => {
        controllerSecret = secret;
      });
      first = await startDaemon(daemonConfig, {
        deps: { ...firstOptions.deps, loadState: async () => state, saveState: async () => {} },
      });

      const controller = controllerToken(daemonConfig.project);
      // "delivery_failed" (a live-but-dying controller Envoy could not reach), not the
      // "no_holder" reason the sibling test above exercises: `handleException` routes both
      // reasons through the same `ensureController` recovery (see processes.ts).
      firstNats.emit(
        `notifications.envoy.exceptions.notifications.role.${controller}`,
        controllerException(daemonConfig.project, "delivery_failed")
      );
      await first.drain();
      expect(controllerSecret).toBeString();
      await first.stop();
      first = undefined;

      const secondOptions = daemonTestDependencies(secondNats, publications, () => {});
      second = await startDaemon(daemonConfig, {
        deps: { ...secondOptions.deps, loadState: async () => state, saveState: async () => {} },
      });

      const ready = () =>
        fetch(`http://127.0.0.1:${second?.server.port}/legion/v1/controller/ready`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ secret: controllerSecret, sessionId: "ses-controller" }),
        });

      expect((await ready()).status).toBe(200);
      expect(publications).toContainEqual({
        topic: roleTopic(controller),
        payload: resyncPayload,
      });

      publications.length = 0;
      // `now()` is fixed by the test harness, so a second `/controller/ready` at the exact same
      // clock reading would come back with the throttled empty report if this resync weren't
      // forced — proving `onControllerReady` never trusts `resyncIntervalMs` to have elapsed.
      expect((await ready()).status).toBe(200);
      expect(publications).toContainEqual({
        topic: roleTopic(controller),
        payload: resyncPayload,
      });
    } finally {
      await first?.stop();
      await second?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("accepts controller/ready even when the controller's shim socket is unreachable", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const firstNats = new FakeNats();
    const secondNats = new FakeNats();
    const publications: Array<{ topic: string; payload: unknown }> = [];
    let controllerSecret: string | undefined;
    let first: daemonIndex.DaemonHandle | undefined;
    let second: daemonIndex.DaemonHandle | undefined;

    try {
      first = await startDaemon(
        daemonConfig,
        daemonTestDependencies(firstNats, publications, (secret) => {
          controllerSecret = secret;
        })
      );
      const controller = controllerToken(daemonConfig.project);
      firstNats.emit(
        `notifications.envoy.exceptions.notifications.role.${controller}`,
        controllerException(daemonConfig.project)
      );
      await first.drain();
      expect(controllerSecret).toBeString();
      await first.stop();
      first = undefined;

      const secondOptions = daemonTestDependencies(secondNats, publications, () => {});
      second = await startDaemon(daemonConfig, {
        deps: {
          ...secondOptions.deps,
          connectWorkerRpc: async () => {
            throw new Error("ECONNREFUSED: controller shim socket unreachable");
          },
        },
      });

      const ready = await fetch(
        `http://127.0.0.1:${second.server.port}/legion/v1/controller/ready`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            secret: controllerSecret,
            sessionId: "ses-controller",
          }),
        }
      );

      // A shim connect failure must never block /controller/ready from accepting the role: the
      // socket connect is best-effort (see markControllerReady's doc comment).
      expect(ready.status).toBe(200);
    } finally {
      await first?.stop();
      await second?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("drains a pending controller notice at boot when the controller role claim was already live (no /controller/ready needed)", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const controller = controllerToken(daemonConfig.project);
    state.roles[controller] = { role: "controller", sessionId: "ses-controller" };
    const pendingPayload = JSON.stringify({ text: "@legion please investigate" });
    state.controllerPendingNotices.push({ payloadJson: pendingPayload, eventId: "mention-1" });
    const nats = new FakeNats();
    const publications: Array<{ topic: string; payload: unknown }> = [];
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      const options = daemonTestDependencies(nats, publications, () => {});
      daemon = await startDaemon(daemonConfig, {
        deps: {
          ...options.deps,
          loadState: async () => state,
        },
      });

      // Delivered without any `/controller/ready` call: the persisted role claim already shows
      // the controller live, so boot itself drains the notice a crash stranded before the
      // original request that recorded it ever got to drain it.
      expect(publications).toContainEqual({
        topic: roleTopic(controller),
        payload: JSON.parse(pendingPayload),
      });
      expect(state.controllerPendingNotices).toEqual([]);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("spawns the controller at boot when notices are pending but no controller role claim exists, then drains once it claims the role", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const pendingPayload = JSON.stringify({ text: "@legion please investigate" });
    state.controllerPendingNotices.push({ payloadJson: pendingPayload, eventId: "mention-1" });
    const nats = new FakeNats();
    const publications: Array<{ topic: string; payload: unknown }> = [];
    let controllerSecret: string | undefined;
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      const options = daemonTestDependencies(nats, publications, (secret) => {
        controllerSecret = secret;
      });
      daemon = await startDaemon(daemonConfig, {
        deps: {
          ...options.deps,
          loadState: async () => state,
        },
      });

      // No controller role claim existed at boot, so nothing would ever reach
      // `/controller/ready` on its own -- boot itself had to spawn the controller directly.
      // Polls for the actual condition (the boot-time `ensureController()` fire-and-forget call
      // reaching the point where the mocked `runner` observes the minted secret) instead of
      // guessing a single real tick is always enough -- see `flushEventLoopUntil`'s doc comment.
      await flushEventLoopUntil(() => controllerSecret !== undefined);
      expect(controllerSecret).toBeString();
      expect(publications).toEqual([]);

      const ready = await fetch(
        `http://127.0.0.1:${daemon.server.port}/legion/v1/controller/ready`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ secret: controllerSecret, sessionId: "ses-controller" }),
        }
      );
      expect(ready.status).toBe(200);

      // Drained once the spawned controller actually claims the role.
      expect(publications).toContainEqual({
        topic: roleTopic(controllerToken(daemonConfig.project)),
        payload: JSON.parse(pendingPayload),
      });
      expect(state.controllerPendingNotices).toEqual([]);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("promotes queued persisted issues before boot completes when config raises the admission cap", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = { ...config(stateDir), admissionCap: 2 };
    const state = newLegionState(daemonConfig.project, 1);
    const first = "WIDGETS-42";
    const second = "WIDGETS-43";
    state.admission.queue.push(first, second);
    await mkdir(path.join(stateDir, "repos", "github.com", "acme", "widgets", ".jj"), {
      recursive: true,
    });
    const nats = new FakeNats();
    const rootLaunches = Promise.withResolvers<void>();
    let launchedRoots = 0;
    let saves = 0;
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {
            saves += 1;
          },
          createNatsTransport: async () => nats,
          runner: async (command) => {
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            if (command[0]?.endsWith("/jj") && command[1] === "workspace" && command[2] === "add") {
              const workspaceDir = command[3];
              if (!workspaceDir) throw new Error("Jujutsu workspace is missing its destination");
              await mkdir(workspaceDir, { recursive: true });
            }
            if (command[0]?.endsWith("/tmux") && command[1] === "has-session") {
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[1] === "new-window") {
              if (command.some((part) => part.startsWith("LEGION_TREE="))) {
                launchedRoots += 1;
                if (launchedRoots === 2) rootLaunches.resolve();
              }
              return {
                stdout: `@${40 + launchedRoots} %${launchedRoots} 1234${launchedRoots}\n`,
                stderr: "",
                exitCode: 0,
              };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          readPluginManifest: async () => validLegionPluginManifest,
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          tokenManager: {
            getToken: async () => ({
              token: "test-token",
              expiresAt: "2026-08-25T00:00:00.000Z",
              gitIdentity: {
                name: "legion-implement[bot]",
                email: "1+legion-implement[bot]@users.noreply.github.com",
              },
            }),
          },
          setTimeout: () => 1 as never,
          clearTimeout: () => {},
          setInterval: () => 1 as never,
          clearInterval: () => {},
          onSignal: () => {},
          exit: () => {},
          now: () => Date.parse("2026-08-24T00:00:00.000Z"),
        },
      });

      expect(state.admission).toEqual({ cap: 2, active: [first, second], queue: [] });
      expect(state.trees[first]?.status).toBe("active");
      expect(state.trees[second]?.status).toBe("active");
      expect(saves).toBeGreaterThan(0);
      await Promise.race([
        rootLaunches.promise,
        new Promise((_, reject) =>
          setTimeout(
            () => reject(new Error("Timed out waiting for reconciled roots to launch")),
            1_000
          )
        ),
      ]);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("boots the API, intake, and lifecycle timers when OMP emits its capability marker on stdout", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const signals = new Map<string, () => void>();
    const timers: Array<() => void> = [];
    let saves = 0;
    let exitCode: number | undefined;
    const exited = Promise.withResolvers<void>();

    try {
      const daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {
            saves += 1;
          },
          createNatsTransport: async () => nats,
          runner: async () => ({
            stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
            stderr: "",
            exitCode: 0,
          }),
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          readPluginManifest: async () => validLegionPluginManifest,
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          tokenManager: {
            getToken: async () => ({
              token: "test-token",
              expiresAt: "2026-08-25T00:00:00.000Z",
              gitIdentity: {
                name: "legion-implement[bot]",
                email: "1+legion-implement[bot]@users.noreply.github.com",
              },
            }),
          },
          setTimeout: (callback) => {
            timers.push(callback);
            return 1 as never;
          },
          clearTimeout: () => {},
          setInterval: (callback) => {
            timers.push(callback);
            return 1 as never;
          },
          clearInterval: () => {},
          onSignal: (signal, handler) => {
            signals.set(signal, handler);
          },
          exit: (code) => {
            exitCode = code;
            exited.resolve();
          },
          now: () => Date.parse("2026-08-24T00:00:00.000Z"),
        },
      });

      const response = await fetch(`http://127.0.0.1:${daemon.server.port}/legion/v1/state`);
      expect(response.status).toBe(200);
      expect(await response.json()).toMatchObject({ project: "acme1" });
      expect(nats.subscriptions.map((subscription) => subscription.subject)).toEqual([
        "notifications.github.acme.widgets.>",
        "notifications.dispatch.issue.*.>",
        "notifications.slack.*.*.mention",
        "notifications.envoy.exceptions.notifications.role.>",
      ]);
      expect(timers).toHaveLength(2);
      await daemon.ready();
      expect(nats.readyCalls).toBe(1);
      await daemon.drain();

      signals.get("SIGTERM")?.();
      await exited.promise;

      expect(saves).toBeGreaterThan(0);
      expect(nats.closed).toBe(true);
      expect(exitCode).toBe(0);
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("rejects an OMP invocation without pi.agents before accepting daemon work", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = {
      ...config(stateDir),
      ompInvocation: "omp-without-agents",
    };
    let loadedState = false;
    let natsCreated = false;
    let probeCommand: string[] | undefined;

    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            runner: async (command) => {
              probeCommand = command;
              return {
                stdout: "",
                stderr: "LEGION_OMP_AGENTS_MISSING\n",
                exitCode: 0,
              };
            },
            dispatchClient: fakeDispatchClient(),
            resolveDaemonEnvironment: async () => daemonEnvironment,
            tokenManager: {
              getToken: async () => ({
                token: "test-token",
                expiresAt: "2099-01-01T00:00:00.000Z",
                gitIdentity: {
                  name: "legion-implementer[bot]",
                  email: "1+legion-implementer[bot]@users.noreply.github.com",
                },
              }),
            },
            loadState: async () => {
              loadedState = true;
              return newLegionState(daemonConfig.project, daemonConfig.admissionCap);
            },
            createNatsTransport: async () => {
              natsCreated = true;
              throw new Error("NATS must not start after a failed OMP capability probe");
            },
          },
        })
      ).rejects.toThrow("does not expose pi.agents");

      expect(probeCommand?.slice(0, 4)).toEqual([
        "sh",
        "-c",
        expect.stringContaining(
          'exec /tools/omp models --no-extensions --extension "$1" --json >/dev/null'
        ),
        "sh",
      ]);
      expect(probeCommand?.[2]).toStartWith("exec ");
      expect(probeCommand?.at(-1)).toContain("legion-omp-probe-");
      expect(loadedState).toBeFalse();
      expect(natsCreated).toBeFalse();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("prepends the configured omp_launch_prefix to both startup capability probes", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig: DaemonConfig = {
      ...config(stateDir),
      ompLaunchPrefix: ["secrets", "ANTHROPIC_API_KEY", "--"],
    };
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const probeCommands: string[][] = [];

    const daemon = await startDaemon(daemonConfig, {
      deps: {
        loadState: async () => state,
        saveState: async () => {},
        createNatsTransport: async () => nats,
        runner: async (command) => {
          if (command[0] === "sh") probeCommands.push(command);
          return {
            stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
            stderr: "",
            exitCode: 0,
          };
        },
        resolveDaemonEnvironment: async () => daemonEnvironment,
        statPrompt: async () => {},
        readPluginManifest: async () => validLegionPluginManifest,
        envoyPublish: async () => {},
        dispatchClient: fakeDispatchClient(),
        tokenManager: {
          getToken: async () => ({
            token: "test-token",
            expiresAt: "2026-08-25T00:00:00.000Z",
            gitIdentity: {
              name: "legion-implement[bot]",
              email: "1+legion-implement[bot]@users.noreply.github.com",
            },
          }),
        },
        setTimeout: () => 1 as never,
        clearTimeout: () => {},
        setInterval: () => 1 as never,
        clearInterval: () => {},
        onSignal: () => {},
        exit: () => {},
        now: () => Date.parse("2026-08-24T00:00:00.000Z"),
      },
    });

    try {
      expect(probeCommands).toHaveLength(2);
      expect(probeCommands[0]?.[2]).toStartWith(
        'exec secrets ANTHROPIC_API_KEY -- /tools/omp models --no-extensions --extension "$1"'
      );
      expect(probeCommands[1]?.[2]).toStartWith(
        'exec secrets ANTHROPIC_API_KEY -- /tools/omp models --extension "$1"'
      );
    } finally {
      await daemon.stop();
      await nats.close();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("rejects an installed pi-legion-envoy that omp does not actually load (disabled or unregistered) before accepting daemon work", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    let loadedState = false;
    let natsCreated = false;
    let capturedManifestPath: string | undefined;
    let shProbeCalls = 0;
    let probeCommand: string[] | undefined;

    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            runner: async (command) => {
              if (command[0] !== "sh") throw new Error(`Unexpected command: ${command.join(" ")}`);
              shProbeCalls += 1;
              if (shProbeCalls === 1) {
                // First sh-shaped probe: verifyOmpAgentsCapability.
                return {
                  stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                  stderr: "",
                  exitCode: 0,
                };
              }
              // Second sh-shaped probe: verifyLegionPluginLoaded. The plugin is
              // disabled/unregistered, so the marker never appears.
              probeCommand = command;
              return { stdout: "", stderr: "LEGION_PLUGIN_LOADED=no\n", exitCode: 0 };
            },
            dispatchClient: fakeDispatchClient(),
            resolveDaemonEnvironment: async () => daemonEnvironment,
            readPluginManifest: async (manifestPath) => {
              capturedManifestPath = manifestPath;
              return JSON.stringify({ version: "0.8.5", omp: { extensions: ["dist/legion.js"] } });
            },
            tokenManager: {
              getToken: async () => ({
                token: "test-token",
                expiresAt: "2099-01-01T00:00:00.000Z",
                gitIdentity: {
                  name: "legion-implementer[bot]",
                  email: "1+legion-implementer[bot]@users.noreply.github.com",
                },
              }),
            },
            loadState: async () => {
              loadedState = true;
              return newLegionState(daemonConfig.project, daemonConfig.admissionCap);
            },
            createNatsTransport: async () => {
              natsCreated = true;
              throw new Error("NATS must not start after a failed plugin load check");
            },
          },
        })
      ).rejects.toThrow(
        "pi-legion-envoy 0.8.5 is installed but not loaded by omp (disabled or unregistered); run omp plugin list"
      );

      expect(probeCommand?.slice(0, 4)).toEqual([
        "sh",
        "-c",
        expect.stringContaining('models --extension "$1" --json >/dev/null'),
        "sh",
      ]);
      expect(probeCommand?.[2]).not.toContain("--no-extensions");
      expect(capturedManifestPath).toBe(
        path.join(getPluginsNodeModules(), "@sjawhar", "pi-legion-envoy", "package.json")
      );
      expect(loadedState).toBeFalse();
      expect(natsCreated).toBeFalse();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("names the real cause when the configured launch prefix itself fails the plugin-load probe, instead of blaming a disabled/unregistered plugin", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig: DaemonConfig = {
      ...config(stateDir),
      ompLaunchPrefix: ["secrets", "ANTHROPIC_API_KEY", "--"],
    };
    let shProbeCalls = 0;

    try {
      let caughtError: unknown;
      try {
        await startDaemon(daemonConfig, {
          deps: {
            runner: async (command) => {
              if (command[0] !== "sh") {
                throw new Error(`Unexpected command: ${command.join(" ")}`);
              }
              shProbeCalls += 1;
              if (shProbeCalls === 1) {
                // First sh-shaped probe: verifyOmpAgentsCapability.
                return {
                  stdout: "LEGION_OMP_AGENTS=available",
                  stderr: "",
                  exitCode: 0,
                };
              }
              // Second sh-shaped probe: verifyLegionPluginLoaded. The launch prefix itself
              // fails here (e.g. `secrets` denying a key) — never reaches omp, so the marker
              // never appears and the exit is nonzero.
              return {
                stdout: "",
                stderr: "secrets: ANTHROPIC_API_KEY: access denied\n",
                exitCode: 1,
              };
            },
            dispatchClient: fakeDispatchClient(),
            resolveDaemonEnvironment: async () => daemonEnvironment,
            readPluginManifest: async () => validLegionPluginManifest,
            tokenManager: {
              getToken: async () => ({
                token: "test-token",
                expiresAt: "2099-01-01T00:00:00.000Z",
                gitIdentity: {
                  name: "legion-implementer[bot]",
                  email: "1+legion-implementer[bot]@users.noreply.github.com",
                },
              }),
            },
            loadState: async () => newLegionState(daemonConfig.project, daemonConfig.admissionCap),
            createNatsTransport: async () => {
              throw new Error("NATS must not start after a failed plugin load check");
            },
          },
        });
      } catch (error) {
        caughtError = error;
      }

      expect(caughtError).toBeInstanceOf(Error);
      const message = (caughtError as Error).message;
      // Names the launch failure directly — exit code, the launch command (prefix + omp path),
      // and the probe's own stderr — never the plugin-registration diagnosis a genuinely
      // disabled/unregistered plugin gets.
      expect(message).toContain("OMP launch probe failed (exit 1)");
      expect(message).toContain("secrets ANTHROPIC_API_KEY -- /tools/omp");
      expect(message).toContain("secrets: ANTHROPIC_API_KEY: access denied");
      expect(message).not.toContain("disabled or unregistered");
      expect(message).not.toContain("omp plugin list");
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("accepts an installed pi-legion-envoy that omp actually loads, without requiring a manifest read", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    let manifestReadCount = 0;

    const daemon = await startDaemon(daemonConfig, {
      deps: {
        loadState: async () => state,
        saveState: async () => {},
        createNatsTransport: async () => nats,
        runner: async () => ({
          stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
          stderr: "",
          exitCode: 0,
        }),
        resolveDaemonEnvironment: async () => daemonEnvironment,
        statPrompt: async () => {},
        readPluginManifest: async () => {
          manifestReadCount += 1;
          return validLegionPluginManifest;
        },
        envoyPublish: async () => {},
        dispatchClient: fakeDispatchClient(),
        tokenManager: {
          getToken: async () => ({
            token: "test-token",
            expiresAt: "2026-08-25T00:00:00.000Z",
            gitIdentity: {
              name: "legion-implement[bot]",
              email: "1+legion-implement[bot]@users.noreply.github.com",
            },
          }),
        },
        setTimeout: () => 1 as never,
        clearTimeout: () => {},
        setInterval: () => 1 as never,
        clearInterval: () => {},
        onSignal: () => {},
        exit: () => {},
        now: () => Date.parse("2026-08-24T00:00:00.000Z"),
      },
    });

    try {
      const response = await fetch(`http://127.0.0.1:${daemon.server.port}/legion/v1/state`);
      expect(response.status).toBe(200);
      // The load probe passing is the gate; the manifest read is only a
      // best-effort version hint for the (unused, on this path) error message.
      expect(manifestReadCount).toBe(0);
    } finally {
      await daemon.stop();
      await nats.close();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("closes API and NATS while surfacing a rejected tracked event during stop", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const daemon = await startDaemon(daemonConfig, {
      deps: {
        loadState: async () => state,
        saveState: async () => {},
        createNatsTransport: async () => nats,
        runner: async () => ({
          stdout: "[]",
          stderr: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
          exitCode: 0,
        }),
        resolveDaemonEnvironment: async () => daemonEnvironment,
        statPrompt: async () => {},
        readPluginManifest: async () => validLegionPluginManifest,
        envoyPublish: async () => {
          throw new Error("listener down");
        },
        dispatchClient: fakeDispatchClient(),
        tokenManager: {
          getToken: async () => ({
            token: "test-token",
            expiresAt: "2026-08-25T00:00:00.000Z",
            gitIdentity: {
              name: "legion-implement[bot]",
              email: "1+legion-implement[bot]@users.noreply.github.com",
            },
          }),
        },
        setTimeout: () => 1 as never,
        clearTimeout: () => {},
        setInterval: () => 1 as never,
        clearInterval: () => {},
        onSignal: () => {},
        exit: () => {},
        now: () => Date.parse("2026-08-24T00:00:00.000Z"),
      },
    });

    try {
      // A well-formed envelope whose durable processing fails (the publish
      // rejects) is nak'd for redelivery — a genuine, still-pending failure
      // that surfaces through drain()/stop(), unlike a malformed envelope
      // (which is termed and resolves normally; see events.test.ts's
      // poison-message coverage).
      nats.emit(
        "notifications.github.acme.widgets.mention",
        JSON.stringify({
          event_id: "mention-1",
          source: "github",
          source_event_id: "mention-1",
          topic: "notifications.github.acme.widgets.mention",
          dedupe_key: "dedupe-mention-1",
          issued_at: 1_000,
          payload_summary: "mention",
          payload: JSON.stringify({ text: "@legion please investigate" }),
          trace_id: "trace-mention-1",
        })
      );

      await expect(daemon.stop()).rejects.toThrow();
      expect(nats.closed).toBe(true);
      await expect(
        fetch(`http://127.0.0.1:${daemon.server.port}/legion/v1/state`)
      ).rejects.toThrow();
    } finally {
      daemon.server.stop();
      await nats.close();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("refuses a second daemon for the same project's state directory while the first is running", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const daemonDeps = {
      loadState: async () => newLegionState(daemonConfig.project, daemonConfig.admissionCap),
      saveState: async () => {},
      createNatsTransport: async () => new FakeNats(),
      runner: async () => ({
        stdout: "[]",
        stderr: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
        exitCode: 0,
      }),
      resolveDaemonEnvironment: async () => daemonEnvironment,
      statPrompt: async () => {},
      envoyPublish: async () => {},
      dispatchClient: fakeDispatchClient(),
      tokenManager: {
        getToken: async () => ({
          token: "test-token",
          expiresAt: "2026-08-25T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "1+legion-implement[bot]@users.noreply.github.com",
          },
        }),
      },
      setTimeout: () => 1 as never,
      clearTimeout: () => {},
      setInterval: () => 1 as never,
      clearInterval: () => {},
      onSignal: () => {},
      exit: () => {},
      now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    };

    const first = await startDaemon(daemonConfig, { deps: daemonDeps });
    try {
      await expect(startDaemon(daemonConfig, { deps: daemonDeps })).rejects.toThrow(
        `Legion daemon already running for this project (pid ${process.pid}`
      );
    } finally {
      first.server.stop();
      await first.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
});
