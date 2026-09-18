// Real-tmux, real-shell E2E for the system-prompt argument: a real `ProcessManager` launches the
// controller pane — an interactive OMP session run bare in its pane, no `legion worker-shim` —
// inside a real tmux server, and the stand-in "OMP" (`argv-recorder-omp.ts`) records the argv
// the pane's shell actually handed it after the `$(cat …)` expansions. A mocked `run` can only
// prove the command string; this proves the process receives exactly ONE `--append-system-prompt`
// (OMP's flag is last-wins) whose value is the role prompt, a blank line, then the materialized
// deployment instructions.
import { afterAll, describe, expect, it } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type DaemonConfig, repoForIssue } from "../config";
import { materializeDeploymentInstructions } from "../deployment-instructions";
import { newLegionState } from "../legion-state";
import { locatorsForIssue, ProcessManager, type ProcessManagerDeps } from "../processes";
import { TmuxRuntime } from "../runtime-tmux";
import { connectWorkerRpc } from "../worker-rpc";
import { fakeDispatchClient } from "./ci-fixtures";

/** `ProcessManager` targets `tmux -L legion-<project>`; the cleanup below must hit the same server. */
const PROJECT = "realinstructions";
const TMUX_SOCKET = `legion-${PROJECT}`;
const ARGV_RECORDER = path.join(
  import.meta.dir,
  "..",
  "..",
  "cli",
  "__tests__",
  "fixtures",
  "argv-recorder-omp.ts"
);
const CONTROLLER_PROMPT = path.resolve(
  import.meta.dir,
  "../../../../pi-envoy/roles/controller-root.md"
);

const tempDirs: string[] = [];

async function run(
  command: string[],
  options?: { cwd?: string; env?: NodeJS.ProcessEnv }
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  const proc = Bun.spawn(command, {
    stdout: "pipe",
    stderr: "pipe",
    ...(options?.cwd ? { cwd: options.cwd } : {}),
    ...(options?.env ? { env: options.env } : {}),
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  return { stdout, stderr, exitCode };
}

/** Polls for `target` every 20 ms for up to 30 s, like `waitForSocket` in
 * `real-shutdown-e2e.test.ts`: the recorder is a real subprocess in a real pane, so there is no
 * promise in this process to await. The recorder renames the finished record into place, so the
 * name existing means the content is whole. */
async function waitForFile(target: string): Promise<void> {
  for (let attempt = 0; attempt < 1500; attempt += 1) {
    if (existsSync(target)) return;
    await Bun.sleep(20);
  }
  throw new Error(`argv record never appeared at ${target}`);
}

function config(stateDir: string): DaemonConfig {
  return {
    project: PROJECT,
    legionId: "sjawhar/legion",
    port: 0,
    runtime: { name: "tmux" },
    daemonUrl: "http://127.0.0.1:0",
    bind: "127.0.0.1",
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: `${process.execPath} ${ARGV_RECORDER}`,
    ompLaunchPrefix: [],
    projects: {
      LEGSMOKE: { repo: "sjawhar/legion" },
      LEGION: { repo: "sjawhar/legion" },
    },
    admissionCap: 1,
    workerCap: 5,
    maxRecursionDepth: 8,
    lingerHours: 72,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    workerStopTimeoutSeconds: 1,
    treeStopTimeoutSeconds: 1,
    workerBootTimeoutSeconds: 120,
    workerBootRegistrationDeadlineIntervals: 3,
    workerRpcTimeoutSeconds: 5,
    workerIdleRetireSeconds: 600,
    workerStreamPort: 13371,
    slowCommandTimeoutSeconds: 300,
    gates: { design: "off" },
    githubApps: {},
    stateDir,
  };
}

afterAll(async () => {
  await run(["tmux", "-L", TMUX_SOCKET, "kill-server"]);
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("real deployment instructions fragment (real tmux, the controller's bare interactive pane, no mocks)", () => {
  it.skipIf(process.env.LEGION_E2E !== "1")(
    "hands the launched process one --append-system-prompt value: the role prompt, then the materialized header + file content",
    async () => {
      const dir = await mkdtemp(path.join(os.tmpdir(), "legion-real-instructions-e2e-"));
      tempDirs.push(dir);
      // The controller pane runs the stand-in bare (no shim keeps the pane open), so it must
      // outlive the daemon's owner marker and identity read: the tmux server this test starts
      // inherits this process's environment and hands it to the pane. `afterAll` kills the server.
      process.env.ARGV_RECORDER_LINGER_MS = "60000";
      const stateDir = path.join(dir, "state");
      await mkdir(stateDir);
      const source = path.join(dir, "ops", "deployment.md");
      await mkdir(path.dirname(source));
      const content =
        "## Required checks\n\n- `pr-checks-result` must pass.\n\nIt's the operator's call: ask `notifications.role.librarian` first.\n";
      await writeFile(source, content, "utf8");

      const cfg = config(stateDir);
      const deploymentInstructionsFile = await materializeDeploymentInstructions(
        source,
        stateDir,
        cfg.legionId
      );
      const state = newLegionState(PROJECT, 1);
      const runtime = new TmuxRuntime({
        tmux: { run, socket: TMUX_SOCKET },
        project: PROJECT,
        stateDir,
        ompInvocation: cfg.ompInvocation,
        ompLaunchPrefix: cfg.ompLaunchPrefix,
        deploymentInstructionsFile,
        provisioningToken: async () => "installation-token",
        run,
        repoForIssue: (issue) => repoForIssue(cfg, issue),
        credentialHelper: "!true",
        slowCommandTimeoutMs: cfg.slowCommandTimeoutSeconds * 1000,
        connectWorkerRpc,
        workerRpcTimeoutMs: () => cfg.workerRpcTimeoutSeconds * 1000,
        now: () => Date.now(),
        issueLocators: (issue) => locatorsForIssue(state, issue),
      });
      const deps: ProcessManagerDeps = {
        state,
        saveState: async () => {},
        config: cfg,
        runtime,
        run,
        processPath: process.env.PATH ?? "",
        rolePromptsDir: path.resolve(import.meta.dir, "../../../../pi-envoy/roles"),
        credentialHelper: "!true",
        publishRole: () => {},
        natsRequest: async () => JSON.stringify({ type: "ack" }),
        mintControllerCapability: async () => "controller-secret",
        mintBootToken: async () => "boot-token",
        mintWorkerBootToken: async () => "worker-boot-token",
        workerCatchup: {
          runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
          baseEnv: {},
          tokenManager: {
            getToken: async () => ({
              token: "worker-token",
              expiresAt: "2099-01-01T00:00:00.000Z",
              gitIdentity: { name: "legion-implement[bot]", email: "implement@example.com" },
            }),
          },
          ownerForIssue: (issue) => repoForIssue(cfg, issue).split("/")[0] as string,
        },
        now: () => Date.now(),
        dispatchClient: fakeDispatchClient(),
        revokeSessionCapability: () => {},
      };
      const processes = new ProcessManager(deps);
      try {
        // `ProcessManager` boots with launches held until the daemon's OMP probes pass
        // (`index.ts` calls `enableLaunches()` at that point); this test stands in for boot.
        processes.enableLaunches();
        await processes.ensureController();
        expect(state.controllerLocator).toMatchObject({
          runtime: "tmux",
          tmuxSession: TMUX_SOCKET,
        });

        // `spawnController` runs the pane as `cd <state_dir>/controller && <omp …>` (bare: no
        // `legion worker-shim`), so the recorder's cwd — where it drops argv.json — is that
        // controller directory.
        const record = path.join(stateDir, "controller", "argv.json");
        await waitForFile(record);
        const argv = JSON.parse(await readFile(record, "utf8")) as string[];

        // The shell's `$(cat …)` strips trailing newlines from both file-backed fragments; the
        // daemon joins them with a blank line inside one double-quoted word.
        const strip = (text: string) => text.replace(/\n+$/, "");
        const prompts = argv.flatMap((arg, index) =>
          arg === "--append-system-prompt" ? [argv[index + 1]] : []
        );
        // An interactive session: no `--mode rpc`; the role prompt is the first argument and the
        // deployment-instructions fragment the last.
        expect(argv).not.toContain("--mode");
        expect(argv[0]).toBe("--append-system-prompt");
        expect(prompts).toEqual([
          `${strip(await readFile(CONTROLLER_PROMPT, "utf8"))}\n\n# Deployment instructions (${cfg.legionId})\n\n${strip(content)}`,
        ]);
        // Exactly the one flag and its value: nothing else reaches the interactive session's argv.
        expect(argv).toHaveLength(2);
      } finally {
        processes.dispose();
      }
    },
    60_000
  );
});
