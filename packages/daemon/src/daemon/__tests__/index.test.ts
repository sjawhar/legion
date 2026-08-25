import { describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { controllerToken, formatIssueKey, roleToken, roleTopic } from "@legion/contracts";
import type { CommandRunner, CommandRunnerOptions } from "../../state/fetch";
import type { DaemonConfig } from "../config";
import type { DaemonEnvironment } from "../environment";
import * as daemonIndex from "../index";
import { newLegionState } from "../legion-state";

const { startDaemon } = daemonIndex;

class FakeNats {
  readonly subscriptions: Array<{
    subject: string;
    callback: (subject: string, data: string) => void;
  }> = [];
  readonly publications: Array<{ subject: string; data: string }> = [];
  closed = false;
  readyCalls = 0;

  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void {
    const subscription = { subject, callback };
    this.subscriptions.push(subscription);
    return () => {
      const index = this.subscriptions.indexOf(subscription);
      if (index >= 0) this.subscriptions.splice(index, 1);
    };
  }

  publish(subject: string, data: string): void {
    this.publications.push({ subject, data });
  }
  async request(_subject: string, _data: string): Promise<string> {
    return JSON.stringify({ type: "ack" });
  }

  emit(subject: string, data: string): void {
    for (const subscription of this.subscriptions) {
      if (matches(subscription.subject, subject)) subscription.callback(subject, data);
    }
  }
  async close(): Promise<void> {
    this.closed = true;
  }
  async ready(): Promise<void> {
    this.readyCalls += 1;
  }
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

function controllerException(project: string): string {
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
      reason: "no_holder",
      payload: JSON.stringify({
        type: "triage",
        issue: formatIssueKey("acme", "widgets", 42),
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
            stdout: "LEGION_OMP_AGENTS=available\n",
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
        if (
          command[0]?.endsWith("/tmux") &&
          (command[1] === "new-session" || command[1] === "new-window")
        ) {
          return { stdout: "@42", stderr: "", exitCode: 0 };
        }
        return { stdout: "", stderr: "", exitCode: 0 };
      },
      resolveDaemonEnvironment: async () => daemonEnvironment,
      statPrompt: async () => {},
      envoyPublish: async (topic, payload) => {
        publications.push({ topic, payload: JSON.parse(payload) });
      },
      fetchGitHubProjectItems: async () => ({
        items: [],
        excludedNullContentItems: 0,
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
  };
}

function config(stateDir: string): DaemonConfig {
  return {
    project: "acme1",
    legionId: "acme/1",
    port: 0,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    dispatchUrl: "http://127.0.0.1:13380",
    dispatchBearer: "dispatch-bearer",
    ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
    boardProjectIds: ["PVT_board"],
    appLogins: ["legion-implement[bot]", "legion-review[bot]"],
    admissionCap: 4,
    workerBudget: 6,
    maxRecursionDepth: 8,
    lingerHours: 72,
    ciQuietMs: 30_000,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir,
  };
}

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
  it("runs board resync queries with the board owner's implementer App token", async () => {
    const originalEnvironment = {
      GH_TOKEN: process.env.GH_TOKEN,
      GITHUB_TOKEN: process.env.GITHUB_TOKEN,
      GH_CONFIG_DIR: process.env.GH_CONFIG_DIR,
      XDG_STATE_HOME: process.env.XDG_STATE_HOME,
    };
    process.env.GH_TOKEN = "personal-gh-token";
    process.env.GITHUB_TOKEN = "personal-github-token";
    process.env.GH_CONFIG_DIR = "/home/user/.config/gh";
    process.env.XDG_STATE_HOME = "/tmp/legion-daemon-test-state";
    const commandOptions: CommandRunnerOptions[] = [];
    const runner: CommandRunner = async (_command, options) => {
      if (options) commandOptions.push(options);
      return {
        stdout: JSON.stringify({
          data: {
            organization: {
              projectV2: {
                items: {
                  pageInfo: { hasNextPage: false, endCursor: null },
                  nodes: [],
                },
              },
            },
          },
        }),
        stderr: "",
        exitCode: 0,
      };
    };
    const tokenCalls: Array<{ role: string; owner: string }> = [];
    const tokenManager = {
      getToken: async (role: "implement" | "review", owner: string) => {
        tokenCalls.push({ role, owner });
        return {
          token: "ghs_board_owner_app_token",
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "3202636+legion-implement[bot]@users.noreply.github.com",
          },
        };
      },
    };

    try {
      const { createBoardProjectItemsFetcher } = daemonIndex as typeof daemonIndex & {
        createBoardProjectItemsFetcher?: (
          board: { owner: string; number: number },
          manager: typeof tokenManager,
          commandRunner: CommandRunner
        ) => () => Promise<unknown>;
      };
      expect(createBoardProjectItemsFetcher).toBeFunction();
      if (!createBoardProjectItemsFetcher) throw new Error("Missing board resync fetcher");
      await createBoardProjectItemsFetcher(
        { owner: "trajectory-labs-pbc", number: 7 },
        tokenManager,
        runner
      )();
      expect(tokenCalls).toEqual([{ role: "implement", owner: "trajectory-labs-pbc" }]);
      expect(commandOptions).toHaveLength(1);
      expect(commandOptions[0]?.env).toMatchObject({
        GH_TOKEN: "ghs_board_owner_app_token",
        GH_CONFIG_DIR: "/tmp/legion-daemon-test-state/legion/gh",
      });
      expect(commandOptions[0]?.env?.GITHUB_TOKEN).toBeUndefined();
    } finally {
      if (originalEnvironment.GH_TOKEN === undefined) delete process.env.GH_TOKEN;
      else process.env.GH_TOKEN = originalEnvironment.GH_TOKEN;
      if (originalEnvironment.GITHUB_TOKEN === undefined) delete process.env.GITHUB_TOKEN;
      else process.env.GITHUB_TOKEN = originalEnvironment.GITHUB_TOKEN;
      if (originalEnvironment.GH_CONFIG_DIR === undefined) delete process.env.GH_CONFIG_DIR;
      else process.env.GH_CONFIG_DIR = originalEnvironment.GH_CONFIG_DIR;
      if (originalEnvironment.XDG_STATE_HOME === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = originalEnvironment.XDG_STATE_HOME;
    }
  });
  it("heals missed board items and executes reconciled human approval wakes through the event pump", async () => {
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-daemon-"));
    const daemonConfig = config(stateDir);
    const state = newLegionState(daemonConfig.project, daemonConfig.admissionCap);
    const approvalIssue = formatIssueKey("acme", "widgets", 43);
    const architect = roleToken(daemonConfig.project, approvalIssue, "architect");
    state.issues[approvalIssue] = {
      key: approvalIssue,
      title: "Awaiting approval",
      state: "open",
      children: [],
      released: true,
      labels: ["needs-approval"],
    };
    state.trees[approvalIssue] = {
      root: approvalIssue,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
    };
    state.roles[architect] = { issue: approvalIssue, role: "architect" };
    const published: Array<{ topic: string; payload: unknown }> = [];
    const logs: string[] = [];
    const originalLog = console.log;
    let resync: (() => void) | undefined;
    let resyncComplete: Promise<void> | undefined;
    let saves = 0;
    let daemon: daemonIndex.DaemonHandle | undefined;
    console.log = (...values: unknown[]) => logs.push(values.join(" "));

    try {
      daemon = await startDaemon(daemonConfig, {
        deps: {
          loadState: async () => state,
          saveState: async () => {
            saves += 1;
          },
          createNatsTransport: async () => new FakeNats(),
          runner: async () => ({
            stdout: "LEGION_OMP_AGENTS=available\n",
            stderr: "",
            exitCode: 0,
          }),
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          envoyPublish: async (topic, payload) => {
            published.push({ topic, payload: JSON.parse(payload) });
          },
          fetchGitHubProjectItems: async () => ({
            items: [
              {
                content: {
                  type: "Issue",
                  number: 42,
                  title: "Recovered issue",
                  repository: "acme/widgets",
                },
                status: "Todo",
                labels: [],
              },
              {
                content: {
                  type: "Issue",
                  number: 43,
                  title: "Awaiting approval",
                  repository: "acme/widgets",
                },
                status: "Todo",
                labels: ["human-approved"],
              },
            ],
            excludedNullContentItems: 0,
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

      const issue = formatIssueKey("acme", "widgets", 42);
      expect(state.issues[issue]).toMatchObject({
        key: issue,
        title: "Recovered issue",
        state: "open",
        released: true,
      });
      expect(state.issues[approvalIssue]?.labels).toEqual(["human-approved"]);
      expect(published).toEqual([
        {
          topic: roleTopic(controllerToken(daemonConfig.project)),
          payload: { type: "triage", issue, preexistingChildren: [] },
        },
        {
          topic: roleTopic(architect),
          payload: { type: "human-approved" },
        },
        {
          topic: roleTopic(controllerToken(daemonConfig.project)),
          payload: {
            type: "resync",
            anomalies: [],
            healed: 1,
            reconciledLabels: 2,
            excludedNullContentItems: 0,
          },
        },
      ]);
      expect(saves).toBeGreaterThan(0);
      expect(logs).toContain(
        "[legion] resync complete: anomalies=0 healed=1 reconciled-labels=2 excluded-null-content-items=0"
      );
    } finally {
      await daemon?.stop();
      console.log = originalLog;
      await rm(stateDir, { recursive: true, force: true });
    }
  });
  it("persists a controller delivery exception across restart and redelivers it once when ready", async () => {
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

      second = await startDaemon(
        daemonConfig,
        daemonTestDependencies(secondNats, publications, () => {})
      );
      const beforeReady = await fetch(`http://127.0.0.1:${second.server.port}/legion/v1/state`);
      expect(beforeReady.status).toBe(200);
      expect(await beforeReady.json()).toMatchObject({
        roles: {},
        controllerHeldEvents: [
          {
            role: controller,
            payloadJson: JSON.stringify({
              type: "triage",
              issue: formatIssueKey("acme", "widgets", 42),
              preexistingChildren: [],
            }),
            eventId: "lost-triage",
          },
        ],
      });

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
      expect(publications).toEqual([
        {
          topic: roleTopic(controller),
          payload: {
            type: "triage",
            issue: formatIssueKey("acme", "widgets", 42),
            preexistingChildren: [],
          },
        },
      ]);

      expect((await ready()).status).toBe(200);
      expect(publications).toHaveLength(1);
      const afterReady = await fetch(`http://127.0.0.1:${second.server.port}/legion/v1/state`);
      expect(await afterReady.json()).toMatchObject({
        controllerHeldEvents: [],
      });
    } finally {
      await first?.stop();
      await second?.stop();
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
            stdout: "LEGION_OMP_AGENTS=available\n",
            stderr: "",
            exitCode: 0,
          }),
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          envoyPublish: async () => {},
          fetchGitHubProjectItems: async () => ({
            items: [],
            excludedNullContentItems: 0,
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
      expect(await response.json()).toMatchObject({
        version: 6,
        project: "acme1",
      });
      expect(nats.subscriptions.map((subscription) => subscription.subject)).toEqual([
        "notifications.github.>",
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
            resolveDaemonEnvironment: async () => daemonEnvironment,
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
          '/tools/omp models --no-extensions --extension "$1" --json >/dev/null'
        ),
        "sh",
      ]);
      expect(probeCommand?.at(-1)).toContain("legion-omp-probe-");
      expect(loadedState).toBeFalse();
      expect(natsCreated).toBeFalse();
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
        runner: async () => ({
          stdout: "[]",
          stderr: "LEGION_OMP_AGENTS=available\n",
          exitCode: 0,
        }),
        resolveDaemonEnvironment: async () => daemonEnvironment,
        statPrompt: async () => {},
        envoyPublish: async () => {},
        fetchGitHubProjectItems: async () => ({
          items: [],
          excludedNullContentItems: 0,
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

    try {
      nats.emit("notifications.github.acme.widgets.issue.1.comment", "not JSON");

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
});
