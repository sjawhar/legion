import { describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { DaemonConfig } from "../config";
import type { DaemonEnvironment } from "../environment";
import { startDaemon } from "../index";
import { newLegionState } from "../legion-state";

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
      if (subscription.subject === "notifications.github.>") subscription.callback(subject, data);
    }
  }

  async close(): Promise<void> {
    this.closed = true;
  }
  async ready(): Promise<void> {
    this.readyCalls += 1;
  }
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
  it("boots the API, intake, and lifecycle timers then persists on SIGTERM", async () => {
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
            stdout: "[]",
            stderr: "LEGION_OMP_AGENTS=available\n",
            exitCode: 0,
          }),
          resolveDaemonEnvironment: async () => daemonEnvironment,
          statPrompt: async () => {},
          envoyPublish: async () => {},
          fetchGitHubProjectItems: async () => ({ items: [] }),
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
      expect(await response.json()).toMatchObject({ version: 5, project: "acme1" });
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
    const daemonConfig = { ...config(stateDir), ompInvocation: "omp-without-agents" };
    let loadedState = false;
    let natsCreated = false;
    let probeCommand: string[] | undefined;

    try {
      await expect(
        startDaemon(daemonConfig, {
          deps: {
            runner: async (command) => {
              probeCommand = command;
              return { stdout: "", stderr: "LEGION_OMP_AGENTS_MISSING\n", exitCode: 0 };
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
        fetchGitHubProjectItems: async () => ({ items: [] }),
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
