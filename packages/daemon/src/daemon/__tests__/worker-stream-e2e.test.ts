// Real-process E2E for the reverse-dial transport: a real `legion worker-shim --connect` child
// (through the real CLI entrypoint) dials a real WorkerStreamListener wired to a real LegionApi's
// boot-token lookup, with a token that API minted. The listener is killed and restarted under
// the shim mid-stream. No tmux: the shim is a plain child process here.
import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type IssueKey, roleToken } from "@legion/contracts";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import { type LegionState, newLegionState } from "../legion-state";
import { startWorkerStreamListener, type WorkerStreamListener } from "../worker-stream-listener";
import { fakeDispatchClient, waitFor } from "./ci-fixtures";

const CLI_ENTRYPOINT = path.join(import.meta.dir, "..", "..", "cli", "index.ts");
const FAKE_OMP = path.join(
  import.meta.dir,
  "..",
  "..",
  "cli",
  "__tests__",
  "fixtures",
  "fake-omp-rpc.ts"
);
const root = "E2E-1" as IssueKey;

const cleanups: Array<() => Promise<void> | void> = [];
afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

/** A real API whose processManager is inert: this test only mints and resolves boot tokens. */
function startApi(): { api: LegionApi; state: LegionState } {
  const state = newLegionState("e2e", 4);
  const inert = () => {
    throw new Error("not exercised by this test");
  };
  const deps: LegionApiDeps = {
    state,
    tokenManager: { getToken: inert },
    dispatchClient: fakeDispatchClient(),
    processManager: {
      admit: inert,
      releaseSlot: inert,
      spawnWorker: inert,
      workerReady: inert,
      rejectIfTreeGone: inert,
      mutateLiveRoleClaim: inert,
      markProcessDead: inert,
      reportRootExit: inert,
      closeTree: inert,
      markTreeReady: inert,
      confirmRootReady: inert,
      cancelBootWatchdog: inert,
      beginLinger: inert,
    },
    envoyPublish: async () => {},
    onControllerReady: async () => {},
    onControllerEvent: async () => {},
  };
  const api = startLegionApi(
    { port: 0, hostname: "127.0.0.1", repo: "acme/widgets", gates: { design: "off" } },
    deps
  );
  cleanups.push(() => api.stop());
  return { api, state };
}

function startListener(api: LegionApi, port = 0, logs: string[] = []): WorkerStreamListener {
  const listener = startWorkerStreamListener({
    hostname: "127.0.0.1",
    port,
    rpcTimeoutMs: 5_000,
    resolveBootToken: api.resolveWorkerBootToken,
    log: (line) => logs.push(line),
  });
  cleanups.push(() => listener.close());
  return listener;
}

async function tokenFile(contents: string): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "legion-worker-stream-e2e-"));
  cleanups.push(() => rm(dir, { recursive: true, force: true }));
  const file = path.join(dir, "boot-token");
  await writeFile(file, contents, { mode: 0o600 });
  return file;
}

function spawnShim(args: string[]): {
  proc: Bun.Subprocess<"ignore", "pipe", "pipe">;
  stdout(): Promise<string>;
  stderr(): Promise<string>;
} {
  const proc = Bun.spawn(
    [process.execPath, CLI_ENTRYPOINT, "worker-shim", ...args, "--", "bun", FAKE_OMP],
    { stdin: "ignore", stdout: "pipe", stderr: "pipe" }
  );
  cleanups.push(() => {
    proc.kill();
  });
  return {
    proc,
    stdout: () => new Response(proc.stdout).text(),
    stderr: () => new Response(proc.stderr).text(),
  };
}

describe("worker stream end to end (real CLI shim, real API, real listener)", () => {
  it("registers with a minted token, survives a listener restart mid-stream, and exits on shutdown", async () => {
    const { api, state } = startApi();
    const token = roleToken(state.project, root, "tester");
    state.roles[token] = { issue: root, role: "tester", generation: 1 };
    const bootToken = await api.mintWorkerBootToken(root, root, "tester", 1);
    const file = await tokenFile(`${bootToken}\n`);
    const logs: string[] = [];
    const first = startListener(api, 0, logs);
    const shim = spawnShim([
      "--connect",
      `tcp://127.0.0.1:${first.port}`,
      "--boot-token-file",
      file,
    ]);

    const client = await first.awaitRegistration(token, 10_000);
    await client.negotiate();
    let idle = Promise.withResolvers<void>();
    client.onIdle(() => idle.resolve());
    await client.prompt("verify #1");
    await idle.promise;

    // Kill the listener under the shim; restart it on the same port; the same shim re-registers
    // with a fresh hello (the refused-dial backoff schedule itself is pinned in worker-shim.test.ts).
    const port = first.port;
    first.close();
    await client.closed;
    const second = startListener(api, port, logs);
    const again = await second.awaitRegistration(token, 10_000);
    expect(again).not.toBe(client);
    await again.negotiate();
    idle = Promise.withResolvers<void>();
    again.onIdle(() => idle.resolve());
    await again.prompt("verify #2");
    await idle.promise;

    again.shutdown();
    expect(await shim.proc.exited).toBe(0);
    const stdout = await shim.stdout();
    expect(stdout.split("agent_end").length - 1).toBe(2);
    expect(logs).toEqual([]);
  });

  it("negative control: a token the API never minted is rejected, logged once per dial, and OMP is never spawned", async () => {
    const { api } = startApi();
    const logs: string[] = [];
    const listener = startListener(api, 0, logs);
    const file = await tokenFile("not-a-minted-token\n");
    const shim = spawnShim([
      "--connect",
      `tcp://127.0.0.1:${listener.port}`,
      "--boot-token-file",
      file,
    ]);
    // 10 s: a fresh `bun` CLI child takes a moment to boot before its first dial.
    await waitFor(() => logs.length >= 2, 10_000); // at least the first dial and its 200 ms retry
    expect(new Set(logs)).toEqual(new Set(["worker-stream: rejected hello (unknown boot token)"]));
    expect(listener.registrations.size).toBe(0);
    shim.proc.kill();
    await shim.proc.exited;
    const stdout = await shim.stdout();
    expect(stdout).not.toContain("agent_start"); // no OMP ever ran
    expect(stdout).toContain("unavailable (stream closed before hello_ack)");
  });

  it("negative control: --socket with --connect, and a blank token file, exit non-zero before spawning anything", async () => {
    const both = spawnShim([
      "--socket",
      "/tmp/x.sock",
      "--connect",
      "tcp://127.0.0.1:1",
      "--boot-token-file",
      "/dev/null",
    ]);
    expect(await both.proc.exited).not.toBe(0);
    expect(await both.stderr()).toContain("--socket and --connect are mutually exclusive");
    const blank = await tokenFile("\n");
    const blankShim = spawnShim(["--connect", "tcp://127.0.0.1:1", "--boot-token-file", blank]);
    expect(await blankShim.proc.exited).not.toBe(0);
    expect(await blankShim.stderr()).toContain(`--boot-token-file ${blank} is blank`);
  });
});
