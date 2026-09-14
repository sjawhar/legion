import { describe, expect, it, spyOn } from "bun:test";
import { readFileSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  controllerToken,
  type DaemonStateResponse,
  LEGION_DAEMON_API_VERSION,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import { getPluginsNodeModules } from "@oh-my-pi/pi-utils/dirs";
import type { CommandRunner, CommandRunnerOptions } from "../../state/fetch";
import { EnvoyPublishError } from "../api/http";
import {
  type DaemonConfig,
  DEFAULT_KUBERNETES_RESOURCES,
  DEFAULT_ROLE_PROFILES,
  type GitHubAppRole,
} from "../config";
import type { DaemonEnvironment, resolveDaemonEnvironment } from "../environment";
import { parseImageDigestRef } from "../image-ref";
import * as daemonIndex from "../index";
import { createK8sClient } from "../k8s-client";
import { type LegionState, newLegionState } from "../legion-state";
import type { DurableMessageControl } from "../nats-transport";
import { writeSecretFile } from "../secrets";
import { imageProbeCachePath, PROBE_CONTAINER, probePodName } from "../worker-image-probe";
import type { WorkerRpcClient } from "../worker-rpc";
import { fakeDispatchClient, procStatLine } from "./ci-fixtures";
import { createFakeK8sApi, type FakeK8sApi } from "./fake-k8s-api";
import { fakeWorkerRpcClient } from "./fake-runtime";

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

/** A `/proc/<pid>/stat` line for whatever pid the fake tmux reported: every pane a daemon under
 * test launches must record a process identity, and no real process exists behind these pids. */
async function fakeProcStat(pid: number): Promise<string> {
  return procStatLine(pid, 4242);
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
        const pointer = command.find((part) => part.startsWith("LEGION_CONTROLLER_SECRET_FILE="));
        if (pointer) {
          onControllerSecret(
            readFileSync(pointer.slice("LEGION_CONTROLLER_SECRET_FILE=".length), "utf8")
          );
        }
        if (command[0]?.endsWith("/tmux") && command[3] === "has-session") {
          return { stdout: "", stderr: "", exitCode: 1 };
        }
        if (command[0]?.endsWith("/tmux") && command[3] === "new-session") {
          return { stdout: "@42 %1 4242", stderr: "", exitCode: 0 };
        }
        if (command[0]?.endsWith("/tmux") && command[3] === "new-window") {
          return { stdout: "@42 %1 12345", stderr: "", exitCode: 0 };
        }
        return { stdout: "", stderr: "", exitCode: 0 };
      },
      resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
      connectWorkerRpc: async (): Promise<WorkerRpcClient> => {
        const closed = Promise.withResolvers<void>();
        return {
          closed: closed.promise,
          runState: "idle",
          negotiate: async () => {},
          adoptWorkingCopy: async () => {},
          prompt: async () => ({
            turnStarted: Promise.resolve(),
            hasStarted: true,
            abandonWait() {},
          }),
          getState: async () => ({}),
          shutdown: () => {},
          close: () => closed.resolve(),
          onIdle: () => {},
        };
      },
      statPrompt: async () => {},
      readProcessStat: fakeProcStat,
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
    runtime: { name: "tmux" },
    daemonUrl: "http://127.0.0.1:0",
    bind: "127.0.0.1",
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
    ompLaunchPrefix: [],
    dispatchProject: "LEGSMOKE",
    repos: ["acme/widgets"],
    repo: "acme/widgets",

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
    workerIdleRetireSeconds: 600,
    slowCommandTimeoutSeconds: 300,
    workerStreamPort: 0,
    gates: { design: "root-issues" },
    githubApps: {
      implement: { appId: "1", privateKey: "test", installations: {} },
      review: { appId: "2", privateKey: "test", installations: {} },
    },
    dispatchUrl: "http://127.0.0.1:18766",
    dispatchToken: "test-dispatch-token",
    stateDir,
  };
}

const validLegionPluginManifest = JSON.stringify({
  version: "0.9.0",
  omp: { extensions: ["dist/envoy.js", "dist/legion.js"] },
  legion: { daemonApiVersion: LEGION_DAEMON_API_VERSION },
});

/** The injected `resolveDaemonEnvironment`: one fixed environment whatever runtime literal the
 * daemon asks for. The production resolver's variant follows the literal; a fixture stands in for
 * that guarantee, so the cast is the fixture's promise, not a check. */
const environmentResolver =
  (environment: DaemonEnvironment): typeof resolveDaemonEnvironment =>
  async () =>
    environment as never;

const daemonEnvironment: DaemonEnvironment = {
  runtime: "tmux",
  commands: {
    jj: "/tools/jj",
    git: "/tools/git",
    gh: "/tools/gh",
    tmux: "/tools/tmux",
  },
  ompInvocation: "/tools/omp",
  paneEnv: { PATH: "/full/bin:/usr/bin" },
  rolePromptsDir: path.resolve(import.meta.dir, "../../../../pi-envoy/roles"),
};

describe("envoyPublishBody", () => {
  // The listener publish body the daemon puts on the wire: `dedupe_key` only when a re-send
  // carries the triggering exception's key (LEGION-108 uses it verbatim as the envelope's dedupe
  // key); an ordinary notice has no such property at all, never a `null` or `undefined` one.
  it("carries dedupe_key only when a key is given", () => {
    expect(daemonIndex.envoyPublishBody("notifications.role.x", "{}", "publish.abc")).toEqual({
      topic: "notifications.role.x",
      message: "{}",
      payload: "{}",
      dedupe_key: "publish.abc",
    });

    const keyless = daemonIndex.envoyPublishBody("notifications.role.x", "{}");
    expect(keyless).toEqual({ topic: "notifications.role.x", message: "{}", payload: "{}" });
    expect("dedupe_key" in keyless).toBeFalse();
  });
});

describe("publishToEnvoy", () => {
  const { publishToEnvoy } = daemonIndex;
  const recordingFetch = (status: number) => {
    const requests: Request[] = [];
    const fetchImpl = async (input: string, init?: RequestInit): Promise<Response> => {
      requests.push(new Request(String(input), init));
      return new Response(status === 200 ? "{}" : "unauthorized", { status });
    };
    return { requests, fetchImpl };
  };

  it("sends the configured Envoy token as a bearer on the listener publish", async () => {
    const { requests, fetchImpl } = recordingFetch(200);
    const daemonConfig = { ...config("/tmp/unused"), envoyToken: "listener-token" };
    await publishToEnvoy(
      daemonConfig,
      "notifications.role.x",
      '{"type":"ping"}',
      undefined,
      fetchImpl
    );
    expect(requests).toHaveLength(1);
    expect(requests[0]?.url).toBe("http://127.0.0.1:9020/v1/messages/publish");
    expect(requests[0]?.headers.get("Authorization")).toBe("Bearer listener-token");
    expect(requests[0]?.headers.get("Content-Type")).toBe("application/json");
    await expect(requests[0]?.json()).resolves.toEqual({
      topic: "notifications.role.x",
      message: '{"type":"ping"}',
      payload: '{"type":"ping"}',
    });
  });

  it("sends no Authorization header when no token is configured (tmux, unchanged)", async () => {
    const { requests, fetchImpl } = recordingFetch(200);
    await publishToEnvoy(config("/tmp/unused"), "notifications.role.x", "{}", undefined, fetchImpl);
    expect(requests[0]?.headers.has("Authorization")).toBe(false);
  });

  it.each([
    [401, "listener-token", "with a bearer token sent"],
    [403, "listener-token", "with a bearer token sent"],
    [401, undefined, "with no bearer token sent"],
  ])("keeps EnvoyPublishError on %i and logs one line naming the listener URL and whether a token was sent (%s)", async (status, envoyToken, sent) => {
    const { fetchImpl } = recordingFetch(status);
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const failure = await publishToEnvoy(
        { ...config("/tmp/unused"), envoyToken },
        "notifications.role.x",
        "{}",
        undefined,
        fetchImpl
      ).then(
        () => undefined,
        (error: unknown) => error
      );
      expect(failure).toBeInstanceOf(EnvoyPublishError);
      expect(failure).toMatchObject({ topic: "notifications.role.x", status });
      expect(errorSpy.mock.calls.map((call) => String(call[0]))).toEqual([
        `[legion] Envoy listener http://127.0.0.1:9020 refused the publish to notifications.role.x (${status}) ${sent}; the daemon's envoy_token_file (or ENVOY_TOKEN_FILE) must hold the listener's ENVOY_API_TOKEN`,
      ]);
    } finally {
      errorSpy.mockRestore();
    }
  });

  it("does not log for a non-auth failure", async () => {
    const { fetchImpl } = recordingFetch(404);
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      await expect(
        publishToEnvoy(config("/tmp/unused"), "notifications.role.x", "{}", undefined, fetchImpl)
      ).rejects.toMatchObject({ status: 404 });
      expect(errorSpy).not.toHaveBeenCalled();
    } finally {
      errorSpy.mockRestore();
    }
  });
});

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
      runner,
      {}
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

  it("the CI status fetcher spawns gh with the daemon's pane environment as its base, never process.env", async () => {
    const commandOptions: CommandRunnerOptions[] = [];
    const runner: CommandRunner = async (_command, options) => {
      if (options) commandOptions.push(options);
      return {
        stdout: JSON.stringify({ data: { repo0: { pr0: null } } }),
        stderr: "",
        exitCode: 0,
      };
    };
    const tokenManager = {
      getToken: async () => ({
        token: "ghs_acme_app_token",
        expiresAt: "2099-01-01T00:00:00.000Z",
        gitIdentity: {
          name: "legion-implement[bot]",
          email: "3202636+legion-implement[bot]@users.noreply.github.com",
        },
      }),
    };
    const saved = process.env.GH_AGENT_APP_PRIVATE_KEY_B64;
    process.env.GH_AGENT_APP_PRIVATE_KEY_B64 = "leaked";
    try {
      await daemonIndex.createCiStatusFetcher(tokenManager, runner, {
        PATH: "/pane/bin",
        HOME: "/home/legion",
      })({
        "acme/api#1": { owner: "acme", repo: "api", number: 1 },
      });
    } finally {
      if (saved === undefined) delete process.env.GH_AGENT_APP_PRIVATE_KEY_B64;
      else process.env.GH_AGENT_APP_PRIVATE_KEY_B64 = saved;
    }

    expect(commandOptions).toHaveLength(1);
    expect(commandOptions[0]?.env).toMatchObject({
      PATH: "/pane/bin",
      HOME: "/home/legion",
      GH_TOKEN: "ghs_acme_app_token",
    });
    expect(commandOptions[0]?.env).not.toHaveProperty("GH_AGENT_APP_PRIVATE_KEY_B64");
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
            if (command[0]?.endsWith("/tmux") && command[3] === "list-windows") {
              // reconcileAdmission's boot-time orphan reap: slow to prove
              // startDaemon does not resolve until it — and every promotion
              // it gates — has fully settled.
              await listWindowsGate.promise;
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[3] === "has-session") {
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (
              command[0]?.endsWith("/tmux") &&
              (command[3] === "new-session" || command[3] === "new-window")
            ) {
              return { stdout: "@42 %1 4242", stderr: "", exitCode: 0 };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          readPluginManifest: async () => validLegionPluginManifest,
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
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
      // Installed before any pane can launch: the shim every pane's PATH puts first.
      expect((await stat(path.join(stateDir, "worker-bin", "gh"))).mode & 0o777).toBe(0o700);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("binds the API and loads state while the OMP probe is still timing out, launches nothing during the hold, and promotes the queued root and worker once it passes", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    // The API port must be known before `startDaemon` resolves: reserve one and hand it over.
    const reserved = Bun.listen<undefined>({
      hostname: "127.0.0.1",
      port: 0,
      socket: { data() {} },
    });
    const port = reserved.port;
    reserved.stop(true);
    const daemonConfig = { ...config(stateDir), port };
    const issue = "WIDGETS-42";
    const testerToken = roleToken(daemonConfig.project, issue, "tester");
    const resumeSessionFile = path.join(stateDir, "workers", "queued-tester.session.json");
    await mkdir(path.dirname(resumeSessionFile), { recursive: true });
    await writeFile(resumeSessionFile, "{}", "utf8");
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    state.issues[issue] = { key: issue, title: "Queued at boot", status: "todo", children: [] };
    state.trees[issue] = { root: issue, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue.push(issue);
    state.roles[testerToken] = {
      issue,
      role: "tester",
      pendingAssignment: { kind: "assignment", task: "verify #41" },
      resumeSessionFile,
    };
    state.workerAdmission.queue.push(testerToken);
    await mkdir(path.join(stateDir, "repos", "github.com", "acme", "widgets", ".jj"), {
      recursive: true,
    });
    let loadedState = false;
    let probeAttempts = 0;
    const sleeps: number[] = [];
    const commands: string[][] = [];
    let started = false;
    let daemon: daemonIndex.DaemonHandle | undefined;
    const paneOpens = () =>
      commands.filter(
        (command) =>
          command[0]?.endsWith("/tmux") &&
          (command[3] === "new-window" || command[3] === "split-window")
      );
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      const starting = startDaemon(daemonConfig, {
        deps: {
          loadState: async () => {
            loadedState = true;
            return state;
          },
          saveState: async () => {},
          createNatsTransport: async () => new FakeNats(),
          runner: async (command) => {
            commands.push(command);
            if (command[0] === "sh") {
              probeAttempts += 1;
              // The first two pi.agents probes hang past their budget and are killed.
              if (probeAttempts <= 2) {
                return {
                  stdout: "",
                  stderr: "",
                  exitCode: 143,
                  timedOut: { limitMs: 300_000, elapsedMs: 300_200 },
                };
              }
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
            if (command[0]?.endsWith("/tmux") && command[3] === "has-session") {
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[3] === "new-window") {
              return { stdout: "@42 %1 4242", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[3] === "split-window") {
              return { stdout: "%2 4243", stderr: "", exitCode: 0 };
            }
            // The root's just-opened pane is live and still its recorded process (pid 4242, the
            // identity `fakeProcStat` reports), so the worker splits into the root's window.
            if (command[0]?.endsWith("/tmux") && command[3] === "list-panes") {
              return { stdout: "%1 4242\n", stderr: "", exitCode: 0 };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          sleep: (ms) => {
            sleeps.push(ms);
            // The same injected `sleep` backs the ProcessManager's registration deadlines and
            // boot watchdog once panes open; those must not fire mid-test (an instantly-elapsed
            // deadline would retire the panes this test asserts on), so only the probe's two
            // backoffs resolve.
            if (sleeps.length > 2) return new Promise<void>(() => {});
            if (sleeps.length === 2) return Promise.resolve();
            return (async () => {
              // The probe is waiting out its first backoff. Boot must have carried on without it:
              // the API answers (with the loaded state), yet no pane has opened and `startDaemon`
              // has not resolved. Every step between the probe's first attempt and the API bind is
              // a fake dependency resolving in a microtask, so the bind has happened by the time a
              // refused connection (one event-loop turn) comes back; the retry is a bounded safety
              // net awaiting the real accept, never a timed wait.
              let response: Response | undefined;
              for (let attempt = 0; attempt < 100 && response === undefined; attempt += 1) {
                response = await fetch(`http://127.0.0.1:${port}/legion/v1/state`).catch(
                  () => undefined
                );
              }
              if (!response) throw new Error("the API never came up during the probe hold");
              expect(response.status).toBe(200);
              const body = (await response.json()) as DaemonStateResponse;
              expect(body.admission.queue).toEqual([issue]);
              expect(body.trees[issue]?.status).toBe("queued");
              expect(body.roles[testerToken]?.locator).toBeUndefined();
              expect(loadedState).toBeTrue();
              expect(paneOpens()).toEqual([]);
              expect(started).toBeFalse();
            })();
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
          // No real process exists behind the fake tmux's pid 4242; the identity check's last step
          // reads its command line, and the worker below splits into the root's window only if
          // the root's pane verifies end to end.
          readProcessCmdline: async () => "omp\0",
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          readPluginManifest: async () => validLegionPluginManifest,
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
      void starting.then((handle) => {
        started = true;
        daemon = handle;
      });
      daemon = await starting;

      // The injected `sleep` is shared with the ProcessManager (registration deadlines arm after
      // a spawn): the probe's two backoffs are the sleeps that precede any launch.
      expect(sleeps.slice(0, 2)).toEqual([10_000, 20_000]);
      const logged = errorSpy.mock.calls.map((call) => String(call[0]));
      expect(logged).toContainEqual(
        expect.stringMatching(
          /OMP pi\.agents probe failed transiently \(attempt 1\); retrying in 10s: command timed out after 300 s \(ran 300\.2 s\)/
        )
      );
      expect(logged).toContainEqual(
        expect.stringMatching(
          /OMP pi\.agents probe failed transiently \(attempt 2\); retrying in 20s/
        )
      );
      expect(state.admission.active).toEqual([issue]);
      expect(state.trees[issue]?.status).toBe("active");
      expect(state.workerAdmission.queue).toEqual([]);
      expect(paneOpens().map((command) => command[3])).toEqual(["new-window", "split-window"]);
      const workerLaunch = paneOpens()[1]?.at(-1) ?? "";
      expect(workerLaunch).toContain(`--resume=${resumeSessionFile}`);
    } finally {
      errorSpy.mockRestore();
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("redrives a queued promotion whose prompt was in flight when the daemon restarted, prompting the idle live worker on boot's first drain", async () => {
    // The LEGION-10 candidate cause: a restart between the shim's acknowledgement and the
    // daemon's commit. The contract defended here is that the entry is still queued with its
    // task after the restart and boot delivers it — `reconnectWorkers` seeds the surviving
    // worker idle from `get_state`, then the first `reconcileWorkerAdmission` prompts it.
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const issue = "WIDGETS-42";
    const testerToken = roleToken(daemonConfig.project, issue, "tester");
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    state.issues[issue] = {
      key: issue,
      title: "Restarted mid-promotion",
      status: "in_progress",
      children: [],
    };
    state.trees[issue] = { root: issue, generation: 1, status: "active", launchFailures: 0 };
    state.roles[testerToken] = {
      issue,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-23T00:00:00.000Z"),
      pendingAssignment: { kind: "assignment", task: "verify #41" },
      locator: {
        runtime: "tmux",
        tmuxSession: `legion-${daemonConfig.project}`,
        tmuxWindowId: "@7",
        tmuxPaneId: "%78",
        socketPath: path.join(stateDir, "workers", "tester.sock"),
      },
    };
    state.workerAdmission.queue.push(testerToken);
    const published: Array<{ topic: string; payload: string }> = [];
    const client = fakeWorkerRpcClient();
    // What `reconnectWorkers`' probe does to a real idle worker: `get_state` reports no stream
    // and seeds the fresh client idle.
    client.getStateImpl = async () => {
      client.emitRunState("idle");
      return { data: { isStreaming: false } };
    };
    const commands: string[][] = [];
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {},
          createNatsTransport: async () => new FakeNats(),
          runner: async (command) => {
            commands.push(command);
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          connectWorkerRpc: async () => client,
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          readPluginManifest: async () => validLegionPluginManifest,
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
          envoyPublish: async (topic, payload) => {
            published.push({ topic, payload });
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

      expect(client.getStateCalls).toBeGreaterThanOrEqual(1);
      expect(client.prompts).toEqual(["verify #41"]);
      expect(state.workerAdmission.queue).toEqual([]);
      const claim = state.roles[testerToken];
      if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
      expect(claim.pendingAssignment).toBeUndefined();
      expect(claim.promptFailures).toBe(0);
      expect(state.phases[issue]).toEqual({
        phase: "tester",
        sessionId: "ses_tester",
        assignedAt: "2026-08-24T00:00:00.000Z",
      });
      // Prompted in place: no pane was opened for it.
      expect(
        commands.filter(
          (command) =>
            command[0]?.endsWith("/tmux") &&
            (command[3] === "new-window" || command[3] === "split-window")
        )
      ).toEqual([]);
      // The architect learns of the delivery through the Envoy listener's publish API — the
      // only path a role-topic notice reaches its holder by (a bare NATS publish is rejected as
      // an invalid envelope and lost).
      expect(published).toContainEqual({
        topic: roleTopic(roleToken(daemonConfig.project, issue, "architect")),
        payload: JSON.stringify({ type: "worker-started", issue, role: "tester" }),
      });
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("resumes a child's architect with a subtree catch-up after its worker-queued notice has no holder", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = { ...config(stateDir), workerCap: 1 };
    const root = "WIDGETS-42";
    const child = "WIDGETS-43";
    const childArchitect = roleToken(daemonConfig.project, child, "architect");
    const childPlanner = roleToken(daemonConfig.project, child, "planner");
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [child] };
    state.trees[root] = { root, generation: 1, status: "queued", launchFailures: 0 };
    state.admission.queue.push(root);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    state.roles[childArchitect] = {
      issue: child,
      role: "architect",
      sessionId: "ses_child_architect",
      generation: 1,
      readyConfirmedAt: 1,
      locator: {
        runtime: "tmux",
        tmuxSession: `legion-${daemonConfig.project}`,
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: path.join(stateDir, "workers", "child-architect.sock"),
      },
    };
    await mkdir(path.join(stateDir, "repos", "github.com", "acme", "widgets", ".jj"), {
      recursive: true,
    });

    const nats = new FakeNats();
    const publications: Array<{ topic: string; payload: unknown }> = [];
    const attemptedTopics: string[] = [];
    const options = daemonTestDependencies(nats, publications, () => {});
    const baseRunner = options.deps?.runner;
    if (!baseRunner) throw new Error("daemon test runner is missing");
    let rootBootToken: string | undefined;
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          ...options.deps,
          loadState: async () => state,
          saveState: async () => {},
          runner: async (command, runnerOptions) => {
            const pointer = command.find((part) => part.startsWith("LEGION_BOOT_TOKEN_FILE="));
            if (pointer) {
              rootBootToken = await readFile(
                pointer.slice("LEGION_BOOT_TOKEN_FILE=".length),
                "utf8"
              );
            }
            if (command[0]?.endsWith("/jj") && command[1] === "workspace" && command[2] === "add") {
              const workspaceDir = command[3];
              if (!workspaceDir) throw new Error("Jujutsu workspace is missing its destination");
              await mkdir(workspaceDir, { recursive: true });
            }
            return baseRunner(command, runnerOptions);
          },
          connectWorkerRpc: async () => fakeWorkerRpcClient(),
          envoyPublish: async (topic, payload) => {
            attemptedTopics.push(topic);
            if (topic === roleTopic(childArchitect)) throw new EnvoyPublishError(topic, 404);
            publications.push({ topic, payload: JSON.parse(payload) });
          },
        },
      });

      if (!rootBootToken) throw new Error("root launch did not receive a boot token");
      const generation = state.trees[root]?.generation;
      if (generation === undefined) throw new Error("root launch did not record a generation");
      const processStarted = await fetch(
        `http://127.0.0.1:${daemon.server.port}/legion/v1/process/started`,
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            tree: root,
            generation,
            rootSessionId: "ses_root",
            bootToken: rootBootToken,
            agentId: "root-agent",
            ompSessionFile: path.join(stateDir, "root.json"),
          }),
        }
      );
      expect(processStarted.status).toBe(200);
      const { secret } = (await processStarted.json()) as { secret: string };

      const spawned = await fetch(`http://127.0.0.1:${daemon.server.port}/legion/v1/worker/spawn`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          tree: root,
          issue: child,
          sessionId: "ses_root",
          secret,
          role: "planner",
          task: "Plan the child",
        }),
      });
      expect(spawned.status).toBe(200);
      expect(await spawned.json()).toEqual({ status: "queued", roleToken: childPlanner });
      expect(attemptedTopics).toContain(roleTopic(childArchitect));

      await flushEventLoopUntil(() => {
        const claim = state.roles[childArchitect];
        return "issue" in claim && claim.pendingAssignment?.kind === "catchup";
      });

      const claim = state.roles[childArchitect];
      if (!claim || !("issue" in claim) || claim.pendingAssignment?.kind !== "catchup") {
        throw new Error("child architect was not resumed with a catch-up");
      }
      expect(JSON.parse(claim.pendingAssignment.task)).toMatchObject({
        type: "catchup-overseer",
        childCounts: { [child]: { total: 0, open: 0, closed: 0 } },
        phaseCompletions: [],
      });
      // The recovery queues a catch-up, not a new architect assignment, so it emits no second
      // worker-queued wake after the original no-holder notice.
      expect(publications).toEqual([]);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("with gates.design off, boot approves every registered gate a human never approved at its current version and wakes its architect", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = { ...config(stateDir), gates: { design: "off" as const } };
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    for (const key of ["WIDGETS-1", "WIDGETS-2", "WIDGETS-3"]) {
      state.issues[key] = { key, title: key, status: "in_progress", children: [] };
      state.trees[key] = { root: key, generation: 1, status: "active", launchFailures: 0 };
    }
    // Registered, never approved: the gate-off fixup approves it at its latest version.
    state.gates["WIDGETS-1"] = { artifactId: "art-1", latestVersion: 4 };
    // Approved by a human at the current version: already open, untouched.
    state.gates["WIDGETS-2"] = { artifactId: "art-2", latestVersion: 2, approvedVersion: 2 };
    // Approved at 1 but edited since (latest 3): closed, so the fixup re-opens it at 3.
    state.gates["WIDGETS-3"] = { artifactId: "art-3", latestVersion: 3, approvedVersion: 1 };
    state.issues["WIDGETS-4"] = { key: "WIDGETS-4", title: "closed", status: "done", children: [] };
    state.trees["WIDGETS-4"] = {
      root: "WIDGETS-4",
      generation: 1,
      status: "closed",
      launchFailures: 0,
    };
    state.gates["WIDGETS-4"] = { artifactId: "art-4", latestVersion: 1 };
    let saved = 0;
    const published: Array<{ topic: string; payload: string }> = [];
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {
            saved += 1;
          },
          createNatsTransport: async () => new FakeNats(),
          runner: async (command) => {
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          readPluginManifest: async () => validLegionPluginManifest,
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
          envoyPublish: async (topic, payload) => {
            published.push({ topic, payload });
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
      expect(state.gates["WIDGETS-1"]).toEqual({
        artifactId: "art-1",
        latestVersion: 4,
        approvedVersion: 4,
      });
      expect(state.gates["WIDGETS-2"]).toEqual({
        artifactId: "art-2",
        latestVersion: 2,
        approvedVersion: 2,
      });
      expect(state.gates["WIDGETS-3"]).toEqual({
        artifactId: "art-3",
        latestVersion: 3,
        approvedVersion: 3,
      });
      // A closed tree has no architect waiting: its gate is left as it was.
      expect(state.gates["WIDGETS-4"]).toEqual({ artifactId: "art-4", latestVersion: 1 });
      expect(saved).toBeGreaterThan(0);
      expect(published.filter((p) => p.payload.includes("design-approved"))).toEqual([
        {
          topic: roleTopic(roleToken(daemonConfig.project, "WIDGETS-1", "architect")),
          payload: JSON.stringify({ type: "design-approved" }),
        },
        {
          topic: roleTopic(roleToken(daemonConfig.project, "WIDGETS-3", "architect")),
          payload: JSON.stringify({ type: "design-approved" }),
        },
      ]);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("boot moves every ownerless child tree back into its parent's tree, leaves a live one alone, wakes each parent once per child, and writes no Dispatch status", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    // Cap 2 with two live trees: the orphan queued below stays queued instead of launching.
    const daemonConfig = { ...config(stateDir), admissionCap: 2 };
    const project = daemonConfig.project;
    const liveLocator = (windowId: string) => ({
      runtime: "tmux" as const,
      tmuxSession: `legion-${project}`,
      tmuxWindowId: windowId,
      tmuxPaneId: `%${windowId.slice(1)}`,
      socketPath: `/state/workers/${windowId}.sock`,
    });
    const state = newLegionState(project, daemonConfig.admissionCap);
    // The live deployment's shape (LEGION-19 with LEGION-25 queued and LEGION-24/-31 active),
    // plus the two other no-process shapes a pre-LEGION-57 daemon can leave behind.
    state.issues["WIDGETS-1"] = {
      key: "WIDGETS-1",
      title: "Root",
      status: "in_progress",
      children: ["WIDGETS-2", "WIDGETS-3", "WIDGETS-6", "WIDGETS-7"],
    };
    state.trees["WIDGETS-1"] = {
      root: "WIDGETS-1",
      generation: 1,
      locator: liveLocator("@1"),
      status: "active",
      launchFailures: 0,
      readyConfirmedAt: 1,
    };
    state.issues["WIDGETS-2"] = {
      key: "WIDGETS-2",
      title: "Released child, queued as a root",
      parent: "WIDGETS-1",
      status: "todo",
      children: [],
    };
    state.trees["WIDGETS-2"] = {
      root: "WIDGETS-2",
      generation: 0,
      status: "queued",
      launchFailures: 0,
    };
    state.issues["WIDGETS-3"] = {
      key: "WIDGETS-3",
      title: "Child already running as a root",
      parent: "WIDGETS-1",
      status: "in_progress",
      children: [],
    };
    state.trees["WIDGETS-3"] = {
      root: "WIDGETS-3",
      generation: 1,
      locator: liveLocator("@3"),
      status: "active",
      launchFailures: 0,
      readyConfirmedAt: 1,
    };
    state.issues["WIDGETS-6"] = {
      key: "WIDGETS-6",
      title: "Child whose root launch failed",
      parent: "WIDGETS-1",
      status: "todo",
      children: [],
    };
    state.trees["WIDGETS-6"] = {
      root: "WIDGETS-6",
      generation: 3,
      status: "launch-failed",
      launchFailures: 3,
    };
    // The claim `/process/started` wrote for a generation of that root that registered but never
    // reached ready: a `sessionId`, no locator, no session file. Left in place, its `sessionId`
    // would become the `expectedSessionId` of the parent's first sub-architect spawn.
    const staleWidgets6Architect = roleToken(project, "WIDGETS-6", "architect");
    state.roles[staleWidgets6Architect] = {
      issue: "WIDGETS-6",
      role: "architect",
      sessionId: "ses_widgets6_failed_root",
      agentId: "agt_widgets6_failed_root",
    };
    state.issues["WIDGETS-7"] = {
      key: "WIDGETS-7",
      title: "Child promoted as a root whose spawn never completed",
      parent: "WIDGETS-1",
      status: "in_progress",
      children: [],
    };
    state.trees["WIDGETS-7"] = {
      root: "WIDGETS-7",
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    // An orphan: its parent has no tree, so it admits as a root of its own, as before.
    state.issues["WIDGETS-4"] = {
      key: "WIDGETS-4",
      title: "Parent with no tree",
      status: "todo",
      children: ["WIDGETS-5"],
    };
    state.issues["WIDGETS-5"] = {
      key: "WIDGETS-5",
      title: "Orphan child",
      parent: "WIDGETS-4",
      status: "todo",
      children: [],
    };
    state.trees["WIDGETS-5"] = {
      root: "WIDGETS-5",
      generation: 0,
      status: "queued",
      launchFailures: 0,
    };
    state.admission.active = ["WIDGETS-1", "WIDGETS-3", "WIDGETS-7"];
    state.admission.queue = ["WIDGETS-2", "WIDGETS-5"];
    const statusWrites: Array<{ issue: string; status: string }> = [];
    const published: Array<{ topic: string; payload: string }> = [];
    const logged: string[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      logged.push(args.map(String).join(" "));
    });
    const saved: LegionState[] = [];
    const boot = () =>
      startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async (_file, snapshot) => {
            saved.push(structuredClone(snapshot));
          },
          createNatsTransport: async () => new FakeNats(),
          runner: async (command) => {
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          readPluginManifest: async () => validLegionPluginManifest,
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
          envoyPublish: async (topic, payload) => {
            published.push({ topic, payload });
          },
          dispatchClient: fakeDispatchClient({
            setStatus: async (issue, status) => {
              statusWrites.push({ issue, status });
            },
          }),
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
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await boot();

      expect(state.trees["WIDGETS-2"]).toBeUndefined();
      expect(state.trees["WIDGETS-6"]).toBeUndefined();
      expect(state.trees["WIDGETS-7"]).toBeUndefined();
      expect(state.trees["WIDGETS-3"]).toMatchObject({
        status: "active",
        locator: liveLocator("@3"),
      });
      expect(state.trees["WIDGETS-5"]).toMatchObject({ status: "queued" });
      expect(state.admission.active).toEqual(["WIDGETS-1", "WIDGETS-3"]);
      expect(state.admission.queue).toEqual(["WIDGETS-5"]);
      expect(state.roles[staleWidgets6Architect]).toBeUndefined();
      expect(statusWrites).toEqual([]);
      // The repair reached disk through `reconcileAdmission`'s closing persist, not only memory.
      const persisted = saved.at(-1);
      if (!persisted) throw new Error("boot never saved state");
      expect(Object.keys(persisted.trees).sort()).toEqual(["WIDGETS-1", "WIDGETS-3", "WIDGETS-5"]);
      expect(persisted.admission.active).toEqual(["WIDGETS-1", "WIDGETS-3"]);
      expect(persisted.admission.queue).toEqual(["WIDGETS-5"]);
      expect(persisted.roles[staleWidgets6Architect]).toBeUndefined();
      const removed = logged.filter((line) => line.includes("LEGION-57"));
      expect(removed).toHaveLength(3);
      for (const child of ["WIDGETS-2", "WIDGETS-6", "WIDGETS-7"]) {
        expect(
          removed.filter((line) => line.includes(child) && line.includes("WIDGETS-1"))
        ).toHaveLength(1);
      }
      const architect = roleTopic(roleToken(project, "WIDGETS-1", "architect"));
      expect(published.filter((p) => p.payload.includes("child-adopted"))).toEqual([
        {
          topic: architect,
          payload: JSON.stringify({ type: "child-adopted", child: "WIDGETS-2", remaining: 4 }),
        },
        {
          topic: architect,
          payload: JSON.stringify({ type: "child-adopted", child: "WIDGETS-6", remaining: 4 }),
        },
        {
          topic: architect,
          payload: JSON.stringify({ type: "child-adopted", child: "WIDGETS-7", remaining: 4 }),
        },
      ]);

      // Idempotent: a second boot on the repaired state removes nothing, logs nothing, wakes nobody.
      await daemon.stop();
      daemon = undefined;
      published.length = 0;
      logged.length = 0;
      daemon = await boot();
      expect(state.admission).toMatchObject({
        active: ["WIDGETS-1", "WIDGETS-3"],
        queue: ["WIDGETS-5"],
      });
      expect(logged.filter((line) => line.includes("LEGION-57"))).toEqual([]);
      expect(published.filter((p) => p.payload.includes("child-adopted"))).toEqual([]);
      expect(statusWrites).toEqual([]);
    } finally {
      errorSpy.mockRestore();
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
        runtime: "tmux",
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
            if (command[0]?.endsWith("/tmux") && command[3] === "list-windows") {
              // reconcileAdmission's boot-time orphan reap: slow enough to observe whether
              // reconnectRoots (synchronous, no tmux calls of its own) has already armed the
              // restored tree's deadline before this settles.
              sleepCallsBeforeReapSettled = sleepCalls;
              await listWindowsGate.promise;
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[3] === "has-session") {
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (
              command[0]?.endsWith("/tmux") &&
              (command[3] === "new-session" || command[3] === "new-window")
            ) {
              return { stdout: "@42 %1 4242", stderr: "", exitCode: 0 };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          readPluginManifest: async () => validLegionPluginManifest,
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
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
  it("retires a ready-confirmed worker whose shim socket is dead at boot: retirement dereferences the live api without throwing, pane killed once, locator cleared, secret file removed", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const issue = "WIDGETS-42";
    const token = roleToken(daemonConfig.project, issue, "tester");
    const ompSessionFile = path.join(stateDir, "workers", "dead-tester.session.json");
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    // A confirmed, previously-live worker: `sessionId` gives its retirement a capability to
    // revoke (the `api` dereference), and `readyConfirmedAt` routes `reconnectWorkers` through
    // `markWorkerDead`, not the unconfirmed-boot retirement.
    state.roles[token] = {
      issue,
      role: "tester",
      generation: 1,
      sessionId: "ses_dead_tester",
      readyConfirmedAt: Date.parse("2026-08-23T00:00:00.000Z"),
      locator: {
        runtime: "tmux",
        tmuxSession: `legion-${daemonConfig.project}`,
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        panePid: 7777,
        paneStartTicks: 4242,
        socketPath: path.join(stateDir, "workers", "dead-tester.sock"),
        ompSessionFile,
      },
    };
    // The pane's boot-token file a previous daemon process wrote; boot-time hygiene must reap it
    // once the locator clears.
    const secretFile = await writeSecretFile(stateDir, token, "stale-boot-token");
    const saved: LegionState[] = [];
    const killedPanes: string[] = [];
    const errorLogs: unknown[][] = [];
    const consoleError = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      errorLogs.push(args);
    });
    const options = daemonTestDependencies(new FakeNats(), [], () => {});
    const baseRunner = options.deps?.runner;
    if (!baseRunner) throw new Error("daemonTestDependencies did not supply a runner");
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          ...options.deps,
          loadState: async () => state,
          saveState: async (_file, snapshot) => {
            saved.push(structuredClone(snapshot));
          },
          connectWorkerRpc: async () => {
            throw new Error("ECONNREFUSED: worker shim socket unreachable");
          },
          runner: async (command, runnerOptions) => {
            // The dead worker's pane is still there and still the recorded process (pid 7777,
            // start ticks 4242 per `fakeProcStat`) -- only its shim is dead -- so the retirement
            // is allowed to kill it; a pane that no longer ran the recorded process would be
            // refused instead, and this test is about the kill path.
            if (command[0]?.endsWith("/tmux") && command[3] === "list-panes") {
              return { stdout: "%7 7777\n", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[3] === "kill-pane") {
              killedPanes.push(command[5] ?? "");
              return { stdout: "", stderr: "can't find pane: %7", exitCode: 1 };
            }
            return baseRunner(command, runnerOptions);
          },
          readProcessCmdline: async () => "omp\0",
        },
      });

      // The whole retirement ran: no per-claim reconcile failure, no reconnect-wide failure, and
      // no TypeError anywhere in the boot log. (`[legion] failed to reconnect worker …:
      // ECONNREFUSED` is the expected per-worker verdict line and is not a failure.)
      const failures = errorLogs.filter((args) =>
        args.some(
          (arg) =>
            arg instanceof TypeError ||
            (typeof arg === "string" &&
              (arg.includes("failed to reconcile worker") ||
                arg.includes("worker reconnection failed")))
        )
      );
      expect(failures).toEqual([]);
      expect(killedPanes).toEqual(["%7"]);

      const persisted = saved.at(-1)?.roles[token];
      if (!persisted || !("issue" in persisted)) throw new Error("worker claim was not persisted");
      expect(persisted.locator).toBeUndefined();
      expect(persisted.resumeSessionFile).toBe(ompSessionFile);
      expect(persisted.sessionId).toBe("ses_dead_tester");

      const response = await fetch(`http://127.0.0.1:${daemon.server.port}/legion/v1/state`);
      expect(response.status).toBe(200);
      const body = (await response.json()) as DaemonStateResponse;
      const exposed = body.roles[token];
      if (!exposed) throw new Error("worker role missing from GET /legion/v1/state");
      expect(exposed.locator).toBeUndefined();
      expect(exposed.sessionId).toBe("ses_dead_tester");

      await expect(stat(secretFile)).rejects.toThrow(/ENOENT/);
    } finally {
      consoleError.mockRestore();
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
    // Boot's own App-token leases (the startup probes) succeed; every lease after `startDaemon`
    // resolves belongs to the resync CI fetch under test and fails.
    let booted = false;
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
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
          readPluginManifest: async () => validLegionPluginManifest,
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          tokenManager: {
            getToken: async () => {
              if (booted) throw new Error("GitHub App token request failed");
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
      booted = true;

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
      const dispatchTokenFile = path.join(stateDir, "secrets", "dispatch-token");
      expect(await readFile(dispatchTokenFile, "utf8")).toBe("test-dispatch-token");
      expect((await stat(dispatchTokenFile)).mode & 0o777).toBe(0o600);
      expect((await stat(path.join(stateDir, "secrets"))).mode & 0o777).toBe(0o700);
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
  it("refuses to boot on an unreadable instructions path before loading state, naming the resolved path", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const instructionsPath = path.join(stateDir, "ops", "deployment.md");
    const daemonConfig = { ...config(stateDir), instructionsPath };
    let loadedState = false;
    let natsCreated = false;
    let probed = false;
    const options = daemonTestDependencies(new FakeNats(), [], () => {});

    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...options.deps,
            runner: async (command) => {
              if (command[0] === "sh") probed = true;
              return { stdout: "", stderr: "", exitCode: 0 };
            },
            loadState: async () => {
              loadedState = true;
              return newLegionState(daemonConfig.project, daemonConfig.admissionCap);
            },
            createNatsTransport: async () => {
              natsCreated = true;
              throw new Error("NATS must not start after a failed instructions read");
            },
          },
        })
      ).rejects.toThrow(`instructions file ${instructionsPath} could not be read`);

      expect(probed).toBeFalse();
      expect(loadedState).toBeFalse();
      expect(natsCreated).toBeFalse();
      await expect(stat(path.join(stateDir, "deployment-instructions.md"))).rejects.toThrow();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("materializes the instructions file under state_dir at boot and appends it to the controller's launch command", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const instructionsPath = path.join(stateDir, "ops", "deployment.md");
    await mkdir(path.dirname(instructionsPath), { recursive: true });
    await writeFile(instructionsPath, "Required check: `pr-checks-result`.\n", "utf8");
    const daemonConfig = { ...config(stateDir), instructionsPath };
    const nats = new FakeNats();
    const commands: string[][] = [];
    const options = daemonTestDependencies(nats, [], () => {});
    const innerRunner = options.deps?.runner;
    if (!innerRunner) throw new Error("test dependencies are missing a runner");
    let daemon: daemonIndex.DaemonHandle | undefined;

    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          ...options.deps,
          runner: async (command, runnerOptions) => {
            commands.push(command);
            return innerRunner(command, runnerOptions);
          },
        },
      });
      const materialized = path.join(stateDir, "deployment-instructions.md");
      expect(await readFile(materialized, "utf8")).toBe(
        "# Deployment instructions (acme/1)\n\nRequired check: `pr-checks-result`.\n"
      );

      nats.emit(
        `notifications.envoy.exceptions.notifications.role.${controllerToken(daemonConfig.project)}`,
        controllerException(daemonConfig.project)
      );
      await daemon.drain();
      const controllerLaunch = commands.find(
        (command) => command[0]?.endsWith("/tmux") && command[3] === "new-window"
      );
      if (!controllerLaunch) throw new Error("controller spawn did not open a tmux window");
      // One flag, both fragments inside it (OMP's flag is last-wins): the packaged controller
      // prompt, a blank line, then the materialized instructions.
      expect(controllerLaunch.at(-1)).toEndWith(
        ` --mode rpc --append-system-prompt "$(cat ${path.resolve(import.meta.dir, "../../../../pi-envoy")}/roles/controller-root.md)\n\n$(cat ${materialized})"`
      );
      expect(controllerLaunch.at(-1)?.split("--append-system-prompt ")).toHaveLength(2);
    } finally {
      await daemon?.stop();
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
    for (const issue of [first, second]) {
      state.issues[issue] = { key: issue, title: issue, status: "todo", children: [] };
    }
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
            if (command[0]?.endsWith("/tmux") && command[3] === "has-session") {
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0]?.endsWith("/tmux") && command[3] === "new-window") {
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
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
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
          runner: async (command) =>
            command[0] === "sh"
              ? {
                  stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                  stderr: "",
                  exitCode: 0,
                }
              : { stdout: "", stderr: "", exitCode: 0 },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
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
  it("refuses to serve an OMP invocation without pi.agents: exits after closing the API and NATS it had opened", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = {
      ...config(stateDir),
      ompInvocation: "omp-without-agents",
    };
    let loadedState = false;
    const nats = new FakeNats();
    let probeCommand: string[] | undefined;
    const options = daemonTestDependencies(nats, [], () => {});

    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...options.deps,
            runner: async (command) => {
              if (command[0] === "sh") probeCommand = command;
              return {
                stdout: "",
                stderr: "LEGION_OMP_AGENTS_MISSING\n",
                exitCode: 0,
              };
            },
            loadState: async () => {
              loadedState = true;
              return newLegionState(daemonConfig.project, daemonConfig.admissionCap);
            },
            saveState: async () => {},
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
      // Boot carried on while the probe ran, then the definitive negative tore it all down.
      expect(loadedState).toBeTrue();
      expect(nats.closed).toBeTrue();
      // The instance lock was released: a second start with a passing OMP works.
      const daemon = await startDaemon(config(stateDir), daemonDeps(config(stateDir)));
      await daemon.stop();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("retries a boot probe whose launch died transiently, then boots once it passes", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    let attempts = 0;
    const sleeps: number[] = [];
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          runner: async (command) => {
            if (command[0] !== "sh") return { stdout: "", stderr: "", exitCode: 0 };
            attempts += 1;
            // The first two pi.agents probes: OMP printed its marker, then died under load.
            if (attempts <= 2) {
              return { stdout: "", stderr: "LEGION_OMP_AGENTS=available\n", exitCode: 1 };
            }
            return {
              stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
              stderr: "",
              exitCode: 0,
            };
          },
          sleep: async (ms) => {
            sleeps.push(ms);
          },
          loadState: async () => newLegionState(daemonConfig.project, daemonConfig.admissionCap),
          saveState: async () => {},
          createNatsTransport: async () => new FakeNats(),
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          statPrompt: async () => {},
          readProcessStat: fakeProcStat,
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          readPluginManifest: async () => validLegionPluginManifest,
          tokenManager: {
            getToken: async () => ({
              token: "test-token",
              expiresAt: "2099-01-01T00:00:00.000Z",
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
      // Two transient failures → two backoff sleeps (10 s doubling), then the pass; the plugin
      // probe adds one more sh call that passes first time.
      expect(sleeps).toEqual([10_000, 20_000]);
      expect(attempts).toBe(4);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("does not retry a definitive pi.agents negative", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    // Boot opens NATS and the API while the probe runs; a definitive negative closes them again.
    const baseDeps = {
      ...daemonTestDependencies(new FakeNats(), [], () => {}).deps,
      loadState: async () => newLegionState(daemonConfig.project, daemonConfig.admissionCap),
      saveState: async () => {},
    };
    try {
      // Definitive: the probe extension loaded and reported no pi.agents. One attempt, no sleep.
      let definitiveAttempts = 0;
      const definitiveSleeps: number[] = [];
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...baseDeps,
            runner: async () => {
              definitiveAttempts += 1;
              return { stdout: "", stderr: "LEGION_OMP_AGENTS=missing\n", exitCode: 1 };
            },
            sleep: async (ms) => {
              definitiveSleeps.push(ms);
            },
          },
        })
      ).rejects.toThrow("does not expose pi.agents");
      expect(definitiveAttempts).toBe(1);
      expect(definitiveSleeps).toEqual([]);

      // Also definitive: the launch command died before OMP ever loaded the probe extension (no
      // marker at all) — e.g. the launch prefix's `secrets` denying a key. Never retried.
      let prefixAttempts = 0;
      const prefixSleeps: number[] = [];
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...baseDeps,
            runner: async () => {
              prefixAttempts += 1;
              return {
                stdout: "",
                stderr: "secrets: ANTHROPIC_API_KEY: access denied\n",
                exitCode: 1,
              };
            },
            sleep: async (ms) => {
              prefixSleeps.push(ms);
            },
          },
        })
      ).rejects.toThrow("does not expose pi.agents: secrets: ANTHROPIC_API_KEY: access denied");
      expect(prefixAttempts).toBe(1);
      expect(prefixSleeps).toEqual([]);
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("keeps retrying a probe the runner killed past the old bound, with the backoff capped at five minutes", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    let attempts = 0;
    const sleeps: number[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          runner: async (command, options) => {
            if (command[0] !== "sh") return { stdout: "", stderr: "", exitCode: 0 };
            attempts += 1;
            expect(options?.timeoutMs).toBe(300_000);
            // The first seven pi.agents probes never finish: the runner kills each at its budget.
            if (attempts <= 7) {
              return {
                stdout: "",
                stderr: "",
                exitCode: 143,
                timedOut: { limitMs: 300_000, elapsedMs: 300_200 },
              };
            }
            return {
              stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
              stderr: "",
              exitCode: 0,
            };
          },
          sleep: async (ms) => {
            sleeps.push(ms);
          },
          loadState: async () => newLegionState(daemonConfig.project, daemonConfig.admissionCap),
          saveState: async () => {},
          createNatsTransport: async () => new FakeNats(),
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
          statPrompt: async () => {},
          envoyPublish: async () => {},
          dispatchClient: fakeDispatchClient(),
          readPluginManifest: async () => validLegionPluginManifest,
          tokenManager: {
            getToken: async () => ({
              token: "test-token",
              expiresAt: "2099-01-01T00:00:00.000Z",
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
      // Seven kills → seven sleeps, doubling from 10 s and capped at 300 s; the eighth pi.agents
      // attempt passes and the plugin probe adds one more sh call.
      expect(sleeps).toEqual([10_000, 20_000, 40_000, 80_000, 160_000, 300_000, 300_000]);
      expect(attempts).toBe(9);
      const logged = errorSpy.mock.calls.map((call) => String(call[0]));
      expect(logged).toContainEqual(
        expect.stringMatching(
          /OMP pi\.agents probe failed transiently \(attempt 1\); retrying in 10s: command timed out after 300 s \(ran 300\.2 s\)/
        )
      );
      expect(logged).toContainEqual(
        expect.stringMatching(/attempt 7\); retrying in 300s: command timed out/)
      );
    } finally {
      errorSpy.mockRestore();
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("a boot failure before the hold cancels a probe still in its transient backoff: no further probe attempt runs", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    let probeAttempts = 0;
    const sleepReleases: Array<() => void> = [];
    // Resolves once the probe has failed its first attempt and entered its backoff.
    const inBackoff = Promise.withResolvers<void>();
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...daemonTestDependencies(new FakeNats(), [], () => {}).deps,
            runner: async (command) => {
              if (command[0] !== "sh") return { stdout: "", stderr: "", exitCode: 0 };
              probeAttempts += 1;
              // Every probe attempt is killed at its budget: the chain is retrying transiently.
              return {
                stdout: "",
                stderr: "",
                exitCode: 143,
                timedOut: { limitMs: 300_000, elapsedMs: 300_100 },
              };
            },
            // The probe's backoff sleeps never resolve on their own: only the daemon's own
            // cancellation can end them.
            sleep: () =>
              new Promise<void>((resolve) => {
                sleepReleases.push(resolve);
                inBackoff.resolve();
              }),
            loadState: async () => {
              // Boot fails while attempt 1's backoff is pending — the window in which a leaked
              // chain would later spawn attempt 2.
              await inBackoff.promise;
              throw new Error("state.json is corrupt");
            },
            saveState: async () => {},
          },
        })
      ).rejects.toThrow("state.json is corrupt");

      expect(probeAttempts).toBe(1);
      expect(sleepReleases).toHaveLength(1);
      // Even if the fake backoff now elapses, the aborted chain must not run another attempt.
      for (const release of sleepReleases) release();
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      expect(probeAttempts).toBe(1);
    } finally {
      errorSpy.mockRestore();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("a boot failure while a probe attempt is still running aborts that attempt's runner call, so its OMP child is killed rather than left behind, and the chain ends without promising a retry", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    // Resolves once the first probe attempt is in flight (its runner call has started).
    const attemptStarted = Promise.withResolvers<void>();
    let attemptSignal: AbortSignal | undefined;
    let abortedWhileRunning = false;
    let probeAttempts = 0;
    const logged: string[] = [];
    const errorSpy = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      logged.push(args.map(String).join(" "));
    });
    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...daemonTestDependencies(new FakeNats(), [], () => {}).deps,
            runner: async (command, options) => {
              if (command[0] !== "sh") return { stdout: "", stderr: "", exitCode: 0 };
              probeAttempts += 1;
              attemptSignal = options?.signal;
              attemptStarted.resolve();
              // The real runner would be blocked on a hung OMP here; it returns only when its
              // signal aborts. This fixture reports the kill as the budget's (`timedOut`) — the
              // race where the budget timer fired as the daemon gave up — because at the daemon
              // level that is the one shape whose mishandling is observable: an attempt the
              // runner reports as `aborted` is never logged even when misclassified (the chain's
              // rejection is swallowed before the hold), so `boot-probes.test.ts` pins that case.
              await new Promise<void>((resolve) => {
                if (options?.signal?.aborted) return resolve();
                options?.signal?.addEventListener("abort", () => resolve(), { once: true });
              });
              abortedWhileRunning = true;
              return {
                stdout: "",
                stderr: "",
                exitCode: 143,
                timedOut: { limitMs: 300_000, elapsedMs: 1_000 },
              };
            },
            sleep: async () => {},
            loadState: async () => {
              await attemptStarted.promise;
              throw new Error("state.json is corrupt");
            },
            saveState: async () => {},
          },
        })
      ).rejects.toThrow("state.json is corrupt");
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      expect(attemptSignal?.aborted).toBeTrue();
      expect(abortedWhileRunning).toBeTrue();
      // The abandoned attempt is not a transient failure: nothing is logged as one, no retry is
      // announced, and none runs — the start-up error above is the only thing to report.
      expect(probeAttempts).toBe(1);
      expect(logged).not.toContainEqual(expect.stringContaining("probe failed transiently"));
      expect(logged).not.toContainEqual(expect.stringContaining("retrying in"));
    } finally {
      errorSpy.mockRestore();
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
          if (command[0] !== "sh") return { stdout: "", stderr: "", exitCode: 0 };
          probeCommands.push(command);
          return {
            stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
            stderr: "",
            exitCode: 0,
          };
        },
        resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
        statPrompt: async () => {},
        readProcessStat: fakeProcStat,
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
  it("runs both startup probes under the resolved pane environment, never the daemon's process.env", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const probeEnvs: Array<NodeJS.ProcessEnv | undefined> = [];
    const leaked = {
      GH_AGENT_APP_PRIVATE_KEY_B64: "leaked-agent-key",
      GH_REVIEW_APP_PRIVATE_KEY_B64: "leaked-review-key",
    };
    const saved = Object.fromEntries(Object.keys(leaked).map((key) => [key, process.env[key]]));
    for (const [key, value] of Object.entries(leaked)) process.env[key] = value;

    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {},
          createNatsTransport: async () => nats,
          runner: async (command, options) => {
            if (command[0] !== "sh") return { stdout: "", stderr: "", exitCode: 0 };
            probeEnvs.push(options?.env);
            return {
              stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
              stderr: "",
              exitCode: 0,
            };
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
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

      // Exact equality against the fixture: nothing from process.env was merged into either probe.
      expect(probeEnvs).toHaveLength(2);
      for (const env of probeEnvs) expect(env).toEqual(daemonEnvironment.paneEnv);
    } finally {
      for (const [key, value] of Object.entries(saved)) {
        if (value === undefined) delete process.env[key];
        else process.env[key] = value;
      }
      await daemon?.stop();
      await nats.close();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("removes what an earlier daemon left in the running private tmux server's environment before any pane can open, logging names only", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const tmuxCommands: string[][] = [];
    const warnings: string[] = [];
    const warnSpy = spyOn(console, "warn").mockImplementation((...args: unknown[]) => {
      warnings.push(args.map(String).join(" "));
    });
    // The tables as tmux would print them with `show-environment -s` (values already escaped); the
    // fake applies each `-u` to them so the scrub's post-removal verification reads the truth.
    const globalTable = new Map<string, string>([
      ["GH_AGENT_APP_PRIVATE_KEY_B64", "leaked-agent-key"],
      ["GH_REVIEW_APP_PRIVATE_KEY_B64", "leaked-review-key"],
      [
        "RAW_FAKE_PEM",
        "-----BEGIN FAKE KEY-----\nZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=\n-----END FAKE KEY-----",
      ],
      ["PATH", "/full/bin:/usr/bin"],
      ["PWD", "/srv"],
      ["SHLVL", "0"],
    ]);
    const sessionTable = new Map<string, string>([["SSH_AUTH_SOCK", "/tmp/ssh-x/agent.1"]]);

    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {},
          createNatsTransport: async () => nats,
          runner: async (command) => {
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            if (command[0] === "/tools/tmux") tmuxCommands.push(command.slice(3));
            if (command[0] === "/tools/tmux" && command[3] === "set-environment") {
              // tmux strips a trailing `;` from an argv token; `\;` is the literal.
              const token = command.at(-1) ?? "";
              const name = token.endsWith("\\;")
                ? `${token.slice(0, -2)};`
                : token.replace(/;$/, "");
              (command[4] === "-g" ? globalTable : sessionTable).delete(name);
              return { stdout: "", stderr: "", exitCode: 0 };
            }
            if (command[0] === "/tools/tmux" && command[3] === "show-environment") {
              // `show-environment -s`: the global table an earlier daemon forked the server with
              // (a PEM-shaped value whose padded last base64 line looks like `NAME=`), and the
              // session table an operator attach filled through tmux's default
              // update-environment, plus that table's unset markers.
              const dump = (t: Map<string, string>, markers: string[]) =>
                [...t.entries()]
                  .map(([k, v]) => `${k}="${v}"; export ${k};`)
                  .concat(markers.map((m) => `unset ${m};`))
                  .map((l) => `${l}\n`)
                  .join("");
              return {
                stdout:
                  command[5] === "-g"
                    ? dump(globalTable, [])
                    : dump(sessionTable, ["DISPLAY", "XAUTHORITY"]),
                stderr: "",
                exitCode: 0,
              };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
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

      expect(tmuxCommands).toContainEqual(["show-environment", "-s", "-g"]);
      expect(tmuxCommands).toContainEqual(["show-environment", "-s", "-t", "legion-acme1"]);
      expect(
        tmuxCommands.filter(
          (command) => command[0] === "set-environment" || command[0] === "set-option"
        )
      ).toEqual([
        ["set-environment", "-g", "-u", "GH_AGENT_APP_PRIVATE_KEY_B64"],
        ["set-environment", "-g", "-u", "GH_REVIEW_APP_PRIVATE_KEY_B64"],
        ["set-environment", "-g", "-u", "RAW_FAKE_PEM"],
        ["set-option", "-t", "legion-acme1", "update-environment", ""],
        ["set-environment", "-t", "legion-acme1", "-u", "SSH_AUTH_SOCK"],
      ]);
      expect(warnings).toEqual([
        "[legion] removed 4 variable(s) from the private tmux server environment that panes may not inherit: GH_AGENT_APP_PRIVATE_KEY_B64, GH_REVIEW_APP_PRIVATE_KEY_B64, RAW_FAKE_PEM, SSH_AUTH_SOCK",
      ]);
      expect(warnings.join("\n")).not.toContain("leaked-");
      expect(warnings.join("\n")).not.toContain("ZmFr");
      expect([...globalTable.keys()]).toEqual(["PATH", "PWD", "SHLVL"]);
    } finally {
      warnSpy.mockRestore();
      await daemon?.stop();
      await nats.close();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("logs nothing about the private tmux server when it carries only the pane environment", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const tmuxCommands: string[][] = [];
    const warnings: string[] = [];
    const warnSpy = spyOn(console, "warn").mockImplementation((...args: unknown[]) => {
      warnings.push(args.map(String).join(" "));
    });

    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {},
          createNatsTransport: async () => nats,
          runner: async (command) => {
            if (command[0] === "sh") {
              return {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              };
            }
            if (command[0] === "/tools/tmux") tmuxCommands.push(command.slice(3));
            if (command[0] === "/tools/tmux" && command[3] === "show-environment") {
              return {
                stdout:
                  command[5] === "-g"
                    ? 'PATH="/full/bin:/usr/bin"; export PATH;\nPWD="/srv"; export PWD;\nSHLVL="0"; export SHLVL;\n'
                    : "unset DISPLAY;\nunset SSH_AUTH_SOCK;\nunset SSH_CONNECTION;\n",
                stderr: "",
                exitCode: 0,
              };
            }
            return { stdout: "", stderr: "", exitCode: 0 };
          },
          resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
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

      expect(tmuxCommands.filter((command) => command[0] === "set-environment")).toEqual([]);
      expect(warnings).toEqual([]);
    } finally {
      warnSpy.mockRestore();
      await daemon?.stop();
      await nats.close();
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("refuses to serve an installed pi-legion-envoy that omp does not actually load (disabled or unregistered): exits after closing the API and NATS it had opened", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    let loadedState = false;
    const nats = new FakeNats();
    let capturedManifestPath: string | undefined;
    let shProbeCalls = 0;
    let probeCommand: string[] | undefined;
    const options = daemonTestDependencies(nats, [], () => {});
    const baseRunner = options.deps?.runner;
    if (!baseRunner) throw new Error("daemonTestDependencies did not supply a runner");

    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...options.deps,
            runner: async (command, runnerOptions) => {
              if (command[0] !== "sh") return baseRunner(command, runnerOptions);
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
            readPluginManifest: async (manifestPath) => {
              capturedManifestPath = manifestPath;
              return JSON.stringify({
                version: "0.8.5",
                omp: { extensions: ["dist/legion.js"] },
                legion: { daemonApiVersion: LEGION_DAEMON_API_VERSION },
              });
            },
            loadState: async () => {
              loadedState = true;
              return newLegionState(daemonConfig.project, daemonConfig.admissionCap);
            },
            saveState: async () => {},
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
      // Boot carried on while the probes ran, then the definitive negative tore it all down.
      expect(loadedState).toBeTrue();
      expect(nats.closed).toBeTrue();
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
    const nats = new FakeNats();
    const options = daemonTestDependencies(nats, [], () => {});
    const baseRunner = options.deps?.runner;
    if (!baseRunner) throw new Error("daemonTestDependencies did not supply a runner");

    try {
      let caughtError: unknown;
      try {
        await startDaemon(daemonConfig, {
          deps: {
            ...options.deps,
            runner: async (command, runnerOptions) => {
              if (command[0] !== "sh") return baseRunner(command, runnerOptions);
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
            loadState: async () => newLegionState(daemonConfig.project, daemonConfig.admissionCap),
            saveState: async () => {},
          },
        });
      } catch (error) {
        caughtError = error;
      }
      // The daemon had opened NATS and the API while the probe ran; the refusal closed them.
      expect(nats.closed).toBeTrue();

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

  it("accepts an installed pi-legion-envoy that omp actually loads and speaks this daemon's API contract", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const nats = new FakeNats();
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);

    const daemon = await startDaemon(daemonConfig, {
      deps: {
        loadState: async () => state,
        saveState: async () => {},
        createNatsTransport: async () => nats,
        runner: async (command) =>
          command[0] === "sh"
            ? {
                stdout: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                stderr: "",
                exitCode: 0,
              }
            : { stdout: "", stderr: "", exitCode: 0 },
        resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
        statPrompt: async () => {},
        readProcessStat: fakeProcStat,
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
      const response = await fetch(`http://127.0.0.1:${daemon.server.port}/legion/v1/state`);
      expect(response.status).toBe(200);
    } finally {
      await daemon.stop();
      await nats.close();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it.each([
    [
      "declares no daemon API contract version",
      JSON.stringify({
        version: "1.3.0",
        omp: { extensions: ["dist/envoy.js", "dist/legion.js"] },
      }),
      `[legion] pi-legion-envoy at ${path.join(getPluginsNodeModules(), "@sjawhar", "pi-legion-envoy", "package.json")} (package 1.3.0) speaks daemon API contract none; this daemon requires ${LEGION_DAEMON_API_VERSION}. Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into the active profile.`,
    ],
    [
      "declares a different daemon API contract version",
      JSON.stringify({
        version: "9.0.0",
        omp: { extensions: ["dist/envoy.js", "dist/legion.js"] },
        legion: { daemonApiVersion: LEGION_DAEMON_API_VERSION + 1 },
      }),
      `[legion] pi-legion-envoy at ${path.join(getPluginsNodeModules(), "@sjawhar", "pi-legion-envoy", "package.json")} (package 9.0.0) speaks daemon API contract ${LEGION_DAEMON_API_VERSION + 1}; this daemon requires ${LEGION_DAEMON_API_VERSION}. Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into the active profile.`,
    ],
  ])("refuses to start when the installed pi-legion-envoy %s, before loading state or opening NATS", async (_case, manifest, message) => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    let loadedState = false;
    let natsCreated = false;
    let loadProbeRan = false;
    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            runner: async (command) => {
              if (command[0] === "sh" && command[2]?.includes("--no-extensions")) {
                return { stdout: "", stderr: "LEGION_OMP_AGENTS=available\n", exitCode: 0 };
              }
              if (command[0] === "sh") loadProbeRan = true;
              return { stdout: "", stderr: "LEGION_PLUGIN_LOADED=yes\n", exitCode: 0 };
            },
            dispatchClient: fakeDispatchClient(),
            resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
            readPluginManifest: async () => manifest,
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
              throw new Error("NATS must not start after a failed plugin contract check");
            },
          },
        })
      ).rejects.toThrow(message);
      expect(loadProbeRan).toBeFalse();
      expect(loadedState).toBeFalse();
      expect(natsCreated).toBeFalse();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("refuses to start when the installed pi-legion-envoy manifest cannot be read", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            runner: async (command) =>
              command[0] === "sh"
                ? {
                    stdout: "",
                    stderr: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                    exitCode: 0,
                  }
                : { stdout: "", stderr: "", exitCode: 0 },
            dispatchClient: fakeDispatchClient(),
            resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
            readPluginManifest: async () => {
              throw new Error("ENOENT: no such file or directory");
            },
            createNatsTransport: async () => {
              throw new Error("NATS must not start after a failed plugin contract check");
            },
          },
        })
      ).rejects.toThrow(
        new RegExp(
          `pi-legion-envoy manifest at .* could not be read \\(ENOENT: no such file or directory\\); this daemon requires a plugin speaking daemon API contract ${LEGION_DAEMON_API_VERSION}\\.`
        )
      );
    } finally {
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
        runner: async (command) =>
          command[0] === "sh"
            ? {
                stdout: "[]",
                stderr: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
                exitCode: 0,
              }
            : { stdout: "", stderr: "", exitCode: 0 },
        resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
        statPrompt: async () => {},
        readProcessStat: fakeProcStat,
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
      runner: async (command: string[]) =>
        command[0] === "sh"
          ? {
              stdout: "[]",
              stderr: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
              exitCode: 0,
            }
          : { stdout: "", stderr: "", exitCode: 0 },
      resolveDaemonEnvironment: environmentResolver(daemonEnvironment),
      readPluginManifest: async () => validLegionPluginManifest,
      statPrompt: async () => {},
      readProcessStat: fakeProcStat,
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

  it("refuses to start when only the implement App is configured, naming the review App, before state is loaded", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    // A `legion.yaml` whose review App key cannot mint a token (the config loader already refuses
    // a missing section): `TokenManager.getToken("review", …)` throws while the implement lease
    // succeeds. Stubbed at the same `getToken` seam every other boot test in this file uses.
    const tokenManager = {
      getToken: async (role: GitHubAppRole) => {
        if (role === "review") throw new Error("role_not_configured: review");
        return {
          token: "test-token",
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "1+legion-implement[bot]@users.noreply.github.com",
          },
        };
      },
    };
    let loadedState = false;
    const base = daemonDeps(daemonConfig);
    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...base.deps,
            tokenManager,
            loadState: async () => {
              loadedState = true;
              return newLegionState(daemonConfig.project, daemonConfig.admissionCap);
            },
          },
        })
      ).rejects.toThrow("role_not_configured: review");
      expect(loadedState).toBeFalse();
      // The instance lock was released: a start with both Apps on the same state dir works.
      const daemon = await startDaemon(daemonConfig, daemonDeps(daemonConfig));
      await daemon.stop();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  /** The dependency bundle every startDaemon test in this file uses, on a fresh state. */
  function daemonDeps(daemonConfig: DaemonConfig): daemonIndex.DaemonStartOptions {
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const options = daemonTestDependencies(new FakeNats(), [], () => {});
    return { deps: { ...options.deps, loadState: async () => state, saveState: async () => {} } };
  }

  it("binds the worker stream listener with the API and closes it with the daemon", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const daemon = await startDaemon(daemonConfig, daemonDeps(daemonConfig));
    let port: number;
    try {
      port = daemon.workerStreamPort;
      expect(port).toBeGreaterThan(0);
      // A garbage first line is refused by Legion's own listener — proving the port is ours.
      const closed = Promise.withResolvers<void>();
      const socket = await Bun.connect<undefined>({
        hostname: "127.0.0.1",
        port,
        socket: { data() {}, close: () => closed.resolve(), error() {} },
      });
      socket.write("not json\n");
      await closed.promise;
    } finally {
      await daemon.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
    await expect(
      Bun.connect<undefined>({ hostname: "127.0.0.1", port, socket: { data() {} } })
    ).rejects.toThrow();
  });

  it("refuses to start when worker_stream_port is bound, naming the setting, and releases the lock", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    // Occupies the address the listener will bind: `config.bind`, the same host the API uses.
    const occupied = Bun.listen<undefined>({
      hostname: daemonConfig.bind,
      port: 0,
      socket: { data() {} },
    });
    try {
      await expect(
        startDaemon({ ...daemonConfig, workerStreamPort: occupied.port }, daemonDeps(daemonConfig))
      ).rejects.toThrow(
        `worker_stream_port ${occupied.port} on ${daemonConfig.bind} is unavailable`
      );
      // The instance lock and API port were released: a second start on a free stream port works.
      const daemon = await startDaemon(daemonConfig, daemonDeps(daemonConfig));
      await daemon.stop();
    } finally {
      occupied.stop(true);
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  const WORKER_IMAGE = parseImageDigestRef(
    `ghcr.io/sjawhar/legion-worker@sha256:${"a".repeat(64)}`
  );
  const PROBE_POD = probePodName("acme1", WORKER_IMAGE.digest);

  function kubernetesConfig(stateDir: string): DaemonConfig {
    return {
      ...config(stateDir),
      daemonUrl: "http://172.18.0.1:13370",
      bind: "0.0.0.0",
      runtime: {
        name: "kubernetes",
        namespace: "legion",
        image: WORKER_IMAGE,
        treeVolume: "20Gi",
        resources: DEFAULT_KUBERNETES_RESOURCES,
        roleProfiles: DEFAULT_ROLE_PROFILES,
      },
    };
  }

  /** What `resolveDaemonEnvironment` yields inside the worker image: jj/git/gh, no tmux, no OMP. */
  const kubernetesEnvironment: DaemonEnvironment = {
    runtime: "kubernetes",
    commands: { jj: "/usr/local/bin/jj", git: "/usr/bin/git", gh: "/usr/local/bin/gh" },
    paneEnv: { PATH: "/opt/legion/bin:/opt/omp/bin:/usr/local/bin:/usr/bin:/bin" },
    rolePromptsDir: path.resolve(import.meta.dir, "../../../../pi-envoy/roles"),
  };

  /** Boot deps for a daemon in a pod: the fake API is the cluster, every poll sleep lets `onPoll`
   * move the probe pod the way a real kubelet would, and the daemon must never read a local
   * plugin manifest (the image's CLI reads the image's). */
  function kubernetesDeps(
    fakeApi: FakeK8sApi,
    onPoll: (pod: string) => void
  ): {
    options: daemonIndex.DaemonStartOptions;
    commands: string[][];
    manifestReads: () => number;
  } {
    const commands: string[][] = [];
    let manifestReads = 0;
    const base = daemonTestDependencies(new FakeNats(), [], () => {});
    const baseRunner = base.deps?.runner;
    if (!baseRunner) throw new Error("test dependencies carry no runner");
    return {
      commands,
      manifestReads: () => manifestReads,
      options: {
        deps: {
          ...base.deps,
          resolveDaemonEnvironment: environmentResolver(kubernetesEnvironment),
          readPluginManifest: async () => {
            manifestReads += 1;
            return validLegionPluginManifest;
          },
          runner: async (command, runnerOptions) => {
            commands.push(command);
            return baseRunner(command, runnerOptions);
          },
          sleep: async () => {
            if (fakeApi.pods.get(PROBE_POD)?.status?.phase === "Pending") onPoll(PROBE_POD);
          },
          k8sClient: createK8sClient({
            server: "https://fake",
            namespace: "legion",
            fetch: fakeApi.fetch,
          }),
        },
      },
    };
  }

  function passProbe(fakeApi: FakeK8sApi, pod: string): void {
    fakeApi.setPhase(pod, "Succeeded");
    fakeApi.logs.set(
      `${pod}/${PROBE_CONTAINER}`,
      `probe-image: OK (/opt/omp/bin/omp) daemon-api-version=${LEGION_DAEMON_API_VERSION}\n`
    );
  }

  /** `metadata.name` of a posted pod body, read with runtime narrowing (the fake records bodies as
   * `unknown`). */
  function postedPodName(body: unknown): string | undefined {
    if (typeof body !== "object" || body === null || !("metadata" in body)) return undefined;
    const { metadata } = body;
    if (typeof metadata !== "object" || metadata === null || !("name" in metadata)) {
      return undefined;
    }
    return typeof metadata.name === "string" ? metadata.name : undefined;
  }

  it("under runtime: kubernetes, runs the probe pod once under the launch hold, admits nothing until it passes, caches the pass, and never reads a local plugin manifest or probes a local OMP", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = kubernetesConfig(stateDir);
    const issue = "WIDGETS-42";
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    state.issues[issue] = { key: issue, title: "Queued at boot", status: "todo", children: [] };
    state.trees[issue] = { root: issue, generation: 0, status: "queued", launchFailures: 0 };
    state.admission.queue.push(issue);
    const fakeApi = createFakeK8sApi({
      namespace: "legion",
      now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    });
    let podsWhenProbePassed: string[] | undefined;
    const { options, commands, manifestReads } = kubernetesDeps(fakeApi, (pod) => {
      // The moment the probe completes, no other pod may exist yet: the hold is on.
      podsWhenProbePassed = [...fakeApi.pods.keys()];
      passProbe(fakeApi, pod);
    });
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, {
        deps: { ...options.deps, loadState: async () => state, saveState: async () => {} },
      });
      expect(podsWhenProbePassed).toEqual([PROBE_POD]);
      const podPosts = fakeApi.requests
        .map((r, index) => ({ ...r, index }))
        .filter((r) => r.method === "POST" && r.path === "/pods");
      expect(podPosts.map((r) => postedPodName(r.body))).toEqual([
        PROBE_POD,
        "legion-widgets-42-architect-g1",
      ]);
      const probeDelete = fakeApi.requests.findIndex(
        (r) => r.method === "DELETE" && r.path === `/pods/${PROBE_POD}`
      );
      expect(probeDelete).toBeGreaterThan(podPosts[0]?.index ?? Number.NaN);
      expect(probeDelete).toBeLessThan(podPosts[1]?.index ?? Number.NaN);
      expect(fakeApi.pods.has(PROBE_POD)).toBe(false);
      expect(state.admission.active).toEqual([issue]);
      expect(state.trees[issue]?.status).toBe("active");
      expect(state.trees[issue]?.locator).toMatchObject({
        runtime: "kubernetes",
        namespace: "legion",
        podName: "legion-widgets-42-architect-g1",
        pvcName: "legion-widgets-42",
        roleToken: roleToken(daemonConfig.project, issue, "architect"),
      });
      expect(
        JSON.parse(await readFile(imageProbeCachePath(stateDir, WORKER_IMAGE.digest), "utf8"))
      ).toMatchObject({ digest: WORKER_IMAGE.digest, daemonApiVersion: LEGION_DAEMON_API_VERSION });
      expect(manifestReads()).toBe(0);
      expect(commands.filter((command) => command[0] === "sh")).toEqual([]);
      expect(commands.filter((command) => command[0]?.endsWith("/tmux"))).toEqual([]);
    } finally {
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("under runtime: kubernetes, a restart with the digest cached at this contract creates no probe pod", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = kubernetesConfig(stateDir);
    const cacheFile = imageProbeCachePath(stateDir, WORKER_IMAGE.digest);
    await mkdir(path.dirname(cacheFile), { recursive: true });
    await writeFile(
      cacheFile,
      JSON.stringify({
        digest: WORKER_IMAGE.digest,
        daemonApiVersion: LEGION_DAEMON_API_VERSION,
        probedAt: "2026-08-23T00:00:00.000Z",
      })
    );
    const fakeApi = createFakeK8sApi({
      namespace: "legion",
      now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    });
    const { options } = kubernetesDeps(fakeApi, () => {
      throw new Error("no probe pod may be polled when the cache holds this digest");
    });
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    let daemon: daemonIndex.DaemonHandle | undefined;
    try {
      daemon = await startDaemon(daemonConfig, options);
      expect(fakeApi.requests.filter((r) => r.method === "POST")).toEqual([]);
      expect(errorSpy.mock.calls.map((call) => String(call[0]))).toContainEqual(
        `[legion] worker image ${WORKER_IMAGE.digest} passed its probe at 2026-08-23T00:00:00.000Z (daemon API contract ${LEGION_DAEMON_API_VERSION}); reusing ${cacheFile}`
      );
    } finally {
      errorSpy.mockRestore();
      await daemon?.stop();
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("under runtime: kubernetes, a failed probe pod refuses startup naming the digest and quoting its log, and releases the lock", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = kubernetesConfig(stateDir);
    const fakeApi = createFakeK8sApi({
      namespace: "legion",
      now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    });
    const mismatch = `[legion] pi-legion-envoy at /home/legion/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json (package 0.9.0) speaks daemon API contract ${LEGION_DAEMON_API_VERSION - 1}; this daemon requires ${LEGION_DAEMON_API_VERSION}.`;
    const { options } = kubernetesDeps(fakeApi, (pod) => {
      fakeApi.setPhase(pod, "Failed");
      fakeApi.logs.set(`${pod}/${PROBE_CONTAINER}`, `${mismatch}\n`);
    });
    try {
      await expect(startDaemon(daemonConfig, options)).rejects.toThrow(
        `[legion] worker image ${WORKER_IMAGE.digest} failed its probe: pod ${PROBE_POD} Failed — log tail: ${mismatch}`
      );
      expect(fakeApi.pods.has(PROBE_POD)).toBe(false);
      await expect(stat(imageProbeCachePath(stateDir, WORKER_IMAGE.digest))).rejects.toThrow();
      // The instance lock was released: a tmux daemon on the same state directory starts.
      const daemon = await startDaemon(config(stateDir), daemonDeps(config(stateDir)));
      await daemon.stop();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });

  it("refuses to start under runtime: kubernetes with neither a kubeconfig nor in-cluster credentials, naming both, and releases the lock", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = kubernetesConfig(stateDir);
    const options = daemonDeps(daemonConfig);
    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            ...options.deps,
            resolveDaemonEnvironment: environmentResolver(kubernetesEnvironment),
          },
        })
      ).rejects.toThrow(
        "runtime.kubernetes.kubeconfig is not set and /var/run/secrets/kubernetes.io/serviceaccount/token does not exist: the daemon runs neither in a pod nor with a kubeconfig"
      );
      // The instance lock was released: a tmux daemon on the same state directory starts.
      const daemon = await startDaemon(config(stateDir), daemonDeps(config(stateDir)));
      await daemon.stop();
    } finally {
      await rm(stateDir, { recursive: true, force: true });
    }
  });
});
