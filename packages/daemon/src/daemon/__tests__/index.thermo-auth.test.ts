import { expect, it } from "bun:test";
import type { DaemonConfig } from "../config";
import type { DaemonEnvironment } from "../environment";
import { type DaemonHandle, startDaemon } from "../index";
import { newLegionState } from "../legion-state";

const environment: DaemonEnvironment = {
  commands: {
    jj: "/tools/jj",
    git: "/tools/git",
    gh: "/tools/gh",
    tmux: "/tools/tmux",
  },
  ompInvocation: "/tools/omp",
  paneEnv: { PATH: "/tools" },
};

function config(): DaemonConfig {
  return {
    project: "acme1",
    legionId: "acme/1",
    port: 0,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "mise x omp",
    boardProjectIds: [],
    appLogins: [],
    admissionCap: 1,
    workerBudget: 1,
    maxRecursionDepth: 1,
    lingerHours: 1,
    ciQuietMs: 1,
    maxFixAttempts: 1,
    resyncIntervalMs: 1,
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir: "/tmp/legion-thermo-auth",
  };
}

it("refuses to start a human merge gate with no resolvable GitHub App login", async () => {
  const daemonConfig = config();
  let daemon: DaemonHandle | undefined;
  let startupError: unknown;
  try {
    daemon = await startDaemon(daemonConfig, {
      deps: {
        loadState: async () => newLegionState(daemonConfig.project, daemonConfig.admissionCap),
        saveState: async () => {},
        createNatsTransport: async () => ({
          subscribe: () => () => {},
          publish: () => {},
          request: async () => JSON.stringify({ type: "ack" }),
          ready: async () => {},
          close: async () => {},
        }),
        runner: async (command) => ({
          stdout: command[0] === "sh" ? "LEGION_OMP_AGENTS=available\n" : "",
          stderr: "",
          exitCode: 0,
        }),
        resolveDaemonEnvironment: async () => environment,
        statPrompt: async () => {},
        envoyPublish: async () => {},
        fetchGitHubProjectItems: async () => ({ items: [], excludedNullContentItems: 0 }),
        tokenManager: {
          getToken: async () => ({
            token: "test-token",
            expiresAt: "2099-01-01T00:00:00.000Z",
            gitIdentity: {
              name: "legion-reviewer[bot]",
              email: "1+legion-reviewer[bot]@users.noreply.github.com",
            },
          }),
        },
        setTimeout: () => 1,
        clearTimeout: () => {},
        setInterval: () => 1,
        clearInterval: () => {},
        onSignal: () => {},
        exit: () => {},
        now: () => 0,
      },
    });
  } catch (error) {
    startupError = error;
  } finally {
    await daemon?.stop();
  }

  expect(startupError).toBeInstanceOf(Error);
  expect((startupError as Error).message).toBe(
    "gates.merge=human requires at least one configured GitHub App login"
  );
});
