import {
  closeSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { createServer, type Server } from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import type { Subprocess } from "bun";
import { z } from "zod";
import { preConsumerNatsServer } from "./listener-grant";

// Destructured: claude-envoy's typecheck, which compiles this file, forbids `process.env.X`.
const { LEGION_E2E_ENVOY_STACK } = process.env;

/**
 * Whether the tests that run a real natsd and Envoy listener run: they build both with Go, which
 * a Bun-only job lacks, so they run only where `LEGION_E2E_ENVOY_STACK=1` says Go is there. With
 * it set, a missing `go` or a failed build fails the test instead of skipping it.
 */
export const envoyStackEnabled = LEGION_E2E_ENVOY_STACK === "1";

/** A natsd with JetStream and an Envoy listener on it, both on loopback ports of their own. */
export interface EnvoyStack {
  /** The listener's base URL, `http://127.0.0.1:<port>`. */
  readonly envoyUrl: string;
  /** natsd's client URL, `nats://127.0.0.1:<port>`. */
  readonly natsUrl: string;
  /** natsd's client address, `127.0.0.1:<port>`, for a proxy to front. */
  readonly natsHostPort: string;
  /** natsd's monitoring base URL, `http://127.0.0.1:<port>` (`/connz`, `/jsz`). */
  readonly monitorUrl: string;
  /** The grant the listener's NATS user holds, as `startEnvoyStack` was asked for it. */
  readonly listenerGrant: ListenerGrant;
  /** Stops the listener and natsd and removes their scratch directory, binaries included. */
  stop(): Promise<void>;
}

/** What a caller may change about the stack it starts. */
export interface EnvoyStackOptions {
  /**
   * Environment for the listener process, merged over the stack's own, so a caller can add a
   * setting or replace one of these. The session-stall rig (test/session-stall.ts) shortens the
   * session windows (`ENVOY_SESSION_LIVE_WINDOW`, `ENVOY_SESSION_GONE_HORIZON`) with it, which
   * is the only way to cross them in a test's seconds rather than the deployment's hour.
   */
  readonly listenerEnv?: Readonly<Record<string, string>>;
  /**
   * Which grant the listener's NATS user holds; `current` by default. See `ListenerGrant`.
   */
  readonly listenerGrant?: ListenerGrant;
}

const ENVOY_MODULE = path.resolve(import.meta.dir, "../../envoy");
const READY_TIMEOUT_MS = 60_000;
const STOP_GRACE_MS = 10_000;

/** Builds natsd and the listener from this checkout into `bin`, as `bin/natsd` and `bin/listener`. */
async function buildBinaries(bin: string): Promise<void> {
  if (Bun.which("go") === null) {
    throw new Error("the Envoy stack needs go on PATH to build natsd and the listener");
  }
  const packages = ["./internal/testnats/cmd/natsd", "./cmd/listener"];
  // An -o ending in a separator names a directory: each main package's binary goes in it, named
  // after the package's directory.
  const build = Bun.spawn(["go", "build", "-o", `${bin}${path.sep}`, ...packages], {
    cwd: ENVOY_MODULE,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stderr, code] = await Promise.all([new Response(build.stderr).text(), build.exited]);
  if (code !== 0) {
    throw new Error(`go build ${packages.join(" ")} in ${ENVOY_MODULE} exited ${code}: ${stderr}`);
  }
}

/**
 * `count` distinct loopback ports nothing listens on now. Every probe socket stays open until all
 * of them are bound, so the kernel cannot hand one ephemeral port out twice; one at a time, each
 * closed before the next opened, it can, and two processes of one stack then race for one port.
 */
export async function freePorts(count: number): Promise<number[]> {
  const servers: Server[] = [];
  try {
    const ports: number[] = [];
    for (let i = 0; i < count; i++) {
      const server = createServer();
      servers.push(server);
      const listening = Promise.withResolvers<void>();
      server.once("error", listening.reject);
      server.listen(0, "127.0.0.1", () => listening.resolve());
      await listening.promise;
      const address = server.address();
      if (address === null || typeof address === "string") throw new Error("no free loopback port");
      ports.push(address.port);
    }
    return ports;
  } finally {
    await Promise.all(
      servers.map((server) => {
        const closed = Promise.withResolvers<void>();
        server.close(() => closed.resolve());
        return closed.promise;
      })
    );
  }
}

/** The last lines of a log file, for an error that names why a process failed. */
function tail(file: string): string {
  try {
    return readFileSync(file, "utf8").trimEnd().split("\n").slice(-10).join("\n");
  } catch {
    return "(no log)";
  }
}

/** Polls `ready` until it holds, throwing when `child` exits first or the timeout passes. */
async function awaitReady(
  what: string,
  child: Subprocess,
  log: string,
  ready: () => Promise<boolean>
): Promise<void> {
  const deadline = Date.now() + READY_TIMEOUT_MS;
  for (;;) {
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error(`${what} exited (${child.exitCode ?? child.signalCode}): ${tail(log)}`);
    }
    if (await ready().catch(() => false)) return;
    if (Date.now() > deadline) {
      throw new Error(`${what} was not ready within ${READY_TIMEOUT_MS} ms: ${tail(log)}`);
    }
    await Bun.sleep(100);
  }
}

/**
 * Writes into `work` the NATS server configuration whose listener user of `machineID` holds the
 * grant from before session consumers, and that user's seed, and names both.
 */
function writePreConsumerServer(
  work: string,
  machineID: string
): { config: string; seedFile: string } {
  const server = preConsumerNatsServer([machineID]);
  const seed = server.seeds.get(machineID);
  if (seed === undefined) throw new Error(`no listener seed for ${machineID}`);
  const config = path.join(work, "nats.conf");
  writeFileSync(config, server.config, { mode: 0o600 });
  const seedFile = path.join(work, "listener.seed");
  writeFileSync(seedFile, `${seed}\n`, { mode: 0o600 });
  return { config, seedFile };
}

const StatusSchema = z.object({ status: z.string() });
const JszSchema = z.object({
  account_details: z
    .array(z.object({ stream_detail: z.array(z.object({ name: z.string() })).optional() }))
    .optional(),
});

/** `url`'s JSON answer read with `schema`; undefined when it answers no 2xx or another shape. */
async function getJson<T>(url: string, schema: z.ZodType<T>): Promise<T | undefined> {
  const response = await fetch(url, { signal: AbortSignal.timeout(2_000) });
  if (!response.ok) return undefined;
  const parsed = schema.safeParse(await response.json());
  return parsed.success ? parsed.data : undefined;
}

async function stopProcess(child: Subprocess): Promise<void> {
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), STOP_GRACE_MS);
  await child.exited;
  clearTimeout(timer);
}

/**
 * Which grant the stack's listener's NATS user holds: `current`, the grant this checkout's
 * `infra/core.ListenerPermissions` renders, served by a NATS server with no authorization at all,
 * so every client may do anything; or `before-session-consumers`, the grant a hub carries until
 * its user is replaced, which leaves every session on the core path (see `./listener-grant`).
 */
export type ListenerGrant = "current" | "before-session-consumers";

/**
 * Starts natsd (JetStream on) and the listener on free loopback ports, with the environment
 * packages/pi-envoy/scripts/smoke-replay.sh starts them with, and resolves once the listener's
 * /healthz answers healthy and natsd holds the `ENVOY_NOTIFICATIONS` stream. Throws, naming why,
 * when `go` is missing, a build fails or either process does not come up.
 *
 * With `listenerGrant: "before-session-consumers"` natsd reads a configuration of the run's own
 * (`preConsumerNatsServer`): the listener connects as an nkey user holding the grant from before
 * session consumers, and everything else, a session's plugin and a test's own connection, as the
 * `no_auth_user` that may do anything.
 */
export async function startEnvoyStack(options: EnvoyStackOptions = {}): Promise<EnvoyStack> {
  const listenerGrant = options.listenerGrant ?? "current";
  const { PATH = "" } = process.env;
  const work = mkdtempSync(path.join(os.tmpdir(), "envoy-stack-"));
  const processes: Subprocess[] = [];
  const stop = async () => {
    for (const child of processes.reverse()) await stopProcess(child);
    rmSync(work, { recursive: true, force: true });
  };
  try {
    const bin = path.join(work, "bin");
    await buildBinaries(bin);
    const [natsPort, monitorPort, listenerPort] = await freePorts(3);
    const natsHome = path.join(work, "nats-home");
    const listenerHome = path.join(work, "listener-home");
    mkdirSync(natsHome);
    mkdirSync(listenerHome);
    const machineID = `envoy-stack-${process.pid}`;
    // On the pre-change grant natsd reads a configuration of its own, naming the listener's nkey
    // user; everything else presents no credential and becomes its `no_auth_user`.
    const preConsumer =
      listenerGrant === "before-session-consumers"
        ? writePreConsumerServer(work, machineID)
        : undefined;
    const natsArgs = [
      "-p",
      String(natsPort),
      "-m",
      String(monitorPort),
      "-store_dir",
      path.join(work, "nats-store"),
      ...(preConsumer === undefined ? [] : ["-c", preConsumer.config]),
    ];
    const listenerEnv = {
      PATH,
      HOME: listenerHome,
      PORT: String(listenerPort),
      ENVOY_LISTEN_HOST: "127.0.0.1",
      ENVOY_MACHINE_ID: machineID,
      NATS_URLS: `nats://127.0.0.1:${natsPort}`,
      ...(preConsumer === undefined ? {} : { NATS_NKEY_SEED_FILE: preConsumer.seedFile }),
    };
    const natsLog = path.join(work, "natsd.log");
    const natsOut = openSync(natsLog, "a");
    const natsd = Bun.spawn([path.join(bin, "natsd"), ...natsArgs], {
      env: { PATH, HOME: natsHome },
      stdout: natsOut,
      stderr: natsOut,
    });
    closeSync(natsOut);
    processes.push(natsd);
    const monitor = `http://127.0.0.1:${monitorPort}`;
    await awaitReady(
      "natsd",
      natsd,
      natsLog,
      async () => (await getJson(`${monitor}/healthz`, StatusSchema))?.status === "ok"
    );

    const natsHostPort = `127.0.0.1:${natsPort}`;
    const natsUrl = `nats://${natsHostPort}`;
    const envoyUrl = `http://127.0.0.1:${listenerPort}`;
    const listenerLog = path.join(work, "listener.log");
    const listenerOut = openSync(listenerLog, "a");
    const listener = Bun.spawn([path.join(bin, "listener")], {
      // The caller's environment last, so a test may replace a setting of the stack's own.
      env: { ...listenerEnv, ...options.listenerEnv },
      stdout: listenerOut,
      stderr: listenerOut,
    });
    closeSync(listenerOut);
    processes.push(listener);
    await awaitReady(
      "the listener",
      listener,
      listenerLog,
      async () => (await getJson(`${envoyUrl}/healthz`, StatusSchema))?.status === "healthy"
    );
    await awaitReady("natsd's ENVOY_NOTIFICATIONS stream", natsd, natsLog, async () => {
      const jsz = await getJson(`${monitor}/jsz?streams=true`, JszSchema);
      return (jsz?.account_details ?? []).some((account) =>
        (account.stream_detail ?? []).some((stream) => stream.name === "ENVOY_NOTIFICATIONS")
      );
    });
    return { envoyUrl, natsUrl, natsHostPort, monitorUrl: monitor, listenerGrant, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}
