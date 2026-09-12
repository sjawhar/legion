import { describe, expect, it } from "bun:test";
import type { IssueKey, LegionRole } from "@legion/contracts";
import {
  awaitShutdown,
  boundedWait,
  type Locator,
  locatorHandles,
  probeWorker,
  type Runtime,
  type SpawnSpec,
  sameProcess,
} from "../runtime";
import type { WorkerRpcClient } from "../worker-rpc";
import { FakeRuntime, type FakeWorkerRpcClient, fakeWorkerRpcClient } from "./fake-runtime";

const issue: IssueKey = "LEGION-42";

interface HarnessOptions {
  /** Every client the runtime hands out (or dials itself) has a `shutdown` that never closes
   * the socket, so a graceful stop can only end by timeout. */
  neverCloses?: boolean;
}

/** One `Runtime` under test plus the observation points the shared cases need. A runtime is
 * built fresh per case; `sleep` is always the no-wait fake so a timeout race settles at once. */
interface Harness {
  runtime: Runtime;
  /** The `runtime` discriminant every locator this harness spawns must carry. */
  expectedRuntime: Locator["runtime"];
  makeSpec(role: LegionRole | "controller", forIssue?: IssueKey): SpawnSpec;
  /** How many raw socket dials the runtime itself has made so far. */
  dials(): number;
  /** Every client the runtime created — via `connect` or its own stop-time dial. */
  clients(): readonly ShutdownCountingClient[];
}

type ShutdownCountingClient = FakeWorkerRpcClient & { shutdowns: number };

function countingClient(neverCloses: boolean): ShutdownCountingClient {
  const client = fakeWorkerRpcClient() as ShutdownCountingClient;
  client.shutdowns = 0;
  const realShutdown = client.shutdown.bind(client);
  client.shutdown = () => {
    client.shutdowns += 1;
    if (!neverCloses) realShutdown();
  };
  return client;
}

function makeSpec(role: LegionRole | "controller", forIssue?: IssueKey): SpawnSpec {
  return {
    ...(role === "controller" ? {} : { issue: forIssue ?? issue }),
    role,
    workspaceDir: "/work",
    env: { LEGION_ROLE: role, UNSET: undefined },
    innerCommand: "omp --mode rpc",
    secrets:
      role === "controller"
        ? { LEGION_CONTROLLER_SECRET: "controller-secret" }
        : { LEGION_BOOT_TOKEN: "boot-token" },
  };
}

async function fakeHarness(options: HarnessOptions = {}): Promise<Harness> {
  const clients: ShutdownCountingClient[] = [];
  const runtime = new FakeRuntime({
    clientFactory: () => {
      const client = countingClient(options.neverCloses ?? false);
      clients.push(client);
      return client;
    },
    sleep: async () => {},
  });
  return {
    runtime,
    expectedRuntime: "kubernetes",
    makeSpec,
    dials: () => runtime.connects.length,
    clients: () => clients,
  };
}

const harnesses: Array<[string, (options?: HarnessOptions) => Promise<Harness>]> = [
  ["FakeRuntime", fakeHarness],
];

describe.each(harnesses)("Runtime contract: %s", (_name, makeHarness) => {
  it("spawn returns a locator of this runtime that probe reports alive", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    expect(locator.runtime).toBe(harness.expectedRuntime);
    expect((await harness.runtime.probe(locator)).status).toBe("alive");
  });

  it("connect returns a client whose negotiate resolves", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("root", harness.makeSpec("architect"));
    const client = await harness.runtime.connect(locator, 50);
    await expect(client.negotiate()).resolves.toBeUndefined();
  });

  it("stop with skipGraceful never dials and leaves the process dead", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    await harness.runtime.stop(locator, 50, { skipGraceful: true });
    expect(harness.dials()).toBe(0);
    expect((await harness.runtime.probe(locator)).status).toBe("dead");
  });

  it("graceful stop sends exactly one shutdown frame and still ends dead when closed never settles", async () => {
    const harness = await makeHarness({ neverCloses: true });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("reviewer"));
    await harness.runtime.connect(locator, 50);
    await harness.runtime.stop(locator, 50);
    const shutdowns = harness.clients().reduce((sum, client) => sum + client.shutdowns, 0);
    expect(shutdowns).toBe(1);
    expect((await harness.runtime.probe(locator)).status).toBe("dead");
  });

  it("reconcileOrphans keeps a process whose handles are known and reaps one whose are not", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("planner"));
    await harness.runtime.reconcileOrphans(new Set(locatorHandles(locator)), 0);
    expect((await harness.runtime.probe(locator)).status).toBe("alive");
    await harness.runtime.reconcileOrphans(new Set(), 0);
    expect((await harness.runtime.probe(locator)).status).toBe("dead");
  });

  it("sameProcess identifies a locator with itself and never with another spawn", async () => {
    const harness = await makeHarness();
    const first = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    const second = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    expect(sameProcess(first, first)).toBe(true);
    expect(sameProcess(first, { ...first })).toBe(true);
    expect(sameProcess(first, second)).toBe(false);
    expect(sameProcess(first, undefined)).toBe(false);
    expect(sameProcess(undefined, undefined)).toBe(true);
  });
});

describe("runtime helpers", () => {
  it("boundedWait releases its real timer on cancel", async () => {
    const { timedOut, cancel } = boundedWait(60_000);
    cancel();
    // Without the cancel this would hold the test for a minute; the cancelled sleep resolves.
    expect(await timedOut).toBe(true);
  });

  it("boundedWait defers to an injected sleep, whose cancel is a no-op", async () => {
    const slept: number[] = [];
    const { timedOut, cancel } = boundedWait(7, async (ms) => {
      slept.push(ms);
    });
    cancel();
    expect(await timedOut).toBe(true);
    expect(slept).toEqual([7]);
  });

  it("awaitShutdown sends the frame before it creates the wait, and confirms a clean close", async () => {
    const order: string[] = [];
    const client = fakeWorkerRpcClient();
    const realShutdown = client.shutdown.bind(client);
    client.shutdown = () => {
      order.push("shutdown");
      realShutdown();
    };
    // The fake client's `shutdown` closes on a microtask; a sleep that never settles proves the
    // clean close alone confirms the stop, and the recorded order proves the frame went first.
    const confirmed = await awaitShutdown(client, 50, async () => {
      order.push("sleep");
      await new Promise<void>(() => {});
    });
    expect(confirmed).toBe(true);
    expect(order).toEqual(["shutdown", "sleep"]);
  });

  it("awaitShutdown treats a timeout and a rejected close alike: unconfirmed", async () => {
    const silent = fakeWorkerRpcClient();
    silent.shutdown = () => {};
    expect(await awaitShutdown(silent, 50, async () => {})).toBe(false);

    const closed = Promise.withResolvers<void>();
    const rejecting: WorkerRpcClient = { ...fakeWorkerRpcClient(), closed: closed.promise };
    rejecting.shutdown = () => closed.reject(new Error("reset"));
    expect(await awaitShutdown(rejecting, 60_000)).toBe(false);
  });

  it("probeWorker forwards connect failures and get_state outcomes like probeWorkerSocket", async () => {
    const connectError = new Error("ECONNREFUSED");
    const refused = await probeWorker(async () => {
      throw connectError;
    }, 50);
    expect(refused).toEqual({
      client: undefined,
      connectError,
      stateAnswered: false,
      stateError: undefined,
    });

    const busy = fakeWorkerRpcClient();
    const stateError = new Error("get_state timed out");
    busy.getStateImpl = async () => {
      throw stateError;
    };
    const busyProbe = await probeWorker(async () => busy, 50);
    expect(busyProbe.client).toBe(busy);
    expect(busyProbe.stateAnswered).toBe(false);
    expect(busyProbe.stateError).toBe(stateError);

    const idle = fakeWorkerRpcClient();
    const idleProbe = await probeWorker(async () => idle, 50);
    expect(idleProbe).toEqual({
      client: idle,
      connectError: undefined,
      stateAnswered: true,
      stateError: undefined,
    });
    expect(idle.getStateCalls).toBe(1);
  });
});
