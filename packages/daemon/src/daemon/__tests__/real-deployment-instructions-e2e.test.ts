// Real-tmux, real-shell E2E for the deployment-instructions fragment: a real `ProcessManager`
// launches the controller pane through a real `legion worker-shim` inside a real tmux server, and
// the wrapped "OMP" (`argv-recorder-omp.ts`) records the argv the pane's shell actually handed it
// after `"$(cat …)"` expansion. A mocked `run` can only prove the command string; this proves the
// process receives the materialized header + content as its last `--append-system-prompt` value.
import { afterAll, describe, expect, it } from "bun:test";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, watch, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { DaemonConfig } from "../config";
import { materializeDeploymentInstructions } from "../deployment-instructions";
import { newLegionState } from "../legion-state";
import { ProcessManager, type ProcessManagerDeps } from "../processes";
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
  command: string[]
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  const proc = Bun.spawn(command, { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  return { stdout, stderr, exitCode };
}

/** Resolves once `target` exists: an existence check first (the recorder may already have run),
 * then inotify events on its directory — the recorder is a real subprocess in a real pane, so
 * there is no promise in this process to await; the directory watch is the event, not a timer.
 * The recorder moves the finished record in from the parent directory, so this directory sees a
 * single event and the name appearing means the content is whole (see `argv-recorder-omp.ts`).
 * Times out naming the path, like `waitForSocket` in `real-shutdown-e2e.test.ts`. */
async function waitForFile(target: string): Promise<void> {
  if (existsSync(target)) return;
  const signal = AbortSignal.timeout(30_000);
  try {
    for await (const _ of watch(path.dirname(target), { signal })) {
      if (existsSync(target)) return;
    }
  } catch (error) {
    if (!signal.aborted) throw error;
  }
  throw new Error(`argv record never appeared at ${target}`);
}

function config(stateDir: string): DaemonConfig {
  return {
    project: PROJECT,
    legionId: "sjawhar/legion",
    port: 0,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: `${process.execPath} ${ARGV_RECORDER}`,
    ompLaunchPrefix: [],
    dispatchProject: "LEGSMOKE",
    repo: "sjawhar/legion",
    repos: ["sjawhar/legion"],
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
    gates: { design: "off" },
    githubApps: {},
    stateDir,
  };
}

afterAll(async () => {
  await run(["tmux", "-L", TMUX_SOCKET, "kill-server"]);
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("real deployment instructions fragment (tmux + worker-shim, no mocks)", () => {
  it.skipIf(process.env.LEGION_E2E !== "1")(
    "hands the launched process the materialized header + file content as its last --append-system-prompt value",
    async () => {
      const dir = await mkdtemp(path.join(os.tmpdir(), "legion-real-instructions-e2e-"));
      tempDirs.push(dir);
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
      const deps: ProcessManagerDeps = {
        state,
        saveState: async () => {},
        config: cfg,
        ompInvocation: cfg.ompInvocation,
        panePath: process.env.PATH ?? "",
        credentialHelper: "!true",
        run,
        natsPublish: () => {},
        natsRequest: async () => JSON.stringify({ type: "ack" }),
        mintControllerCapability: async () => "controller-secret",
        mintBootToken: async () => "boot-token",
        mintWorkerBootToken: async () => "worker-boot-token",
        connectWorkerRpc,
        provisioningToken: async () => "installation-token",
        workerCatchup: {
          runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
          tokenManager: {
            getToken: async () => ({
              token: "worker-token",
              expiresAt: "2099-01-01T00:00:00.000Z",
              gitIdentity: { name: "legion-implement[bot]", email: "implement@example.com" },
            }),
          },
          repo: "sjawhar/legion",
        },
        now: () => Date.now(),
        dispatchClient: fakeDispatchClient(),
        revokeSessionCapability: () => {},
        deploymentInstructionsFile,
      };
      const processes = new ProcessManager(deps);
      try {
        await processes.ensureController();
        expect(state.controllerLocator?.tmuxSession).toBe(TMUX_SOCKET);

        // `spawnController` runs the pane as `cd <state_dir>/controller && … worker-shim -- <omp>`,
        // so the recorder's cwd — where it drops argv.json — is that controller directory.
        const record = path.join(stateDir, "controller", "argv.json");
        await waitForFile(record);
        const argv = JSON.parse(await readFile(record, "utf8")) as string[];

        // The shell's `$(cat …)` strips trailing newlines from both file-backed fragments.
        const strip = (text: string) => text.replace(/\n+$/, "");
        const prompts = argv.flatMap((arg, index) =>
          arg === "--append-system-prompt" ? [argv[index + 1]] : []
        );
        expect(argv.slice(0, 2)).toEqual(["--mode", "rpc"]);
        expect(prompts).toEqual([
          strip(await readFile(CONTROLLER_PROMPT, "utf8")),
          `# Deployment instructions (${cfg.legionId})\n\n${strip(content)}`,
        ]);
        expect(argv.at(-2)).toBe("--append-system-prompt");
      } finally {
        processes.dispose();
      }
    },
    60_000
  );
});
