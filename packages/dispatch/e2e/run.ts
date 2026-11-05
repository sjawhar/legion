/** `bun e2e/run.ts -- <command> [args...]`: starts the Dispatch e2e harness (e2e/harness-start.ts),
 *  runs the command with each listener's port in its environment, stops the harness once the
 *  command exits, and exits with the command's status. `bun run e2e` runs Playwright through it:
 *
 *    bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts [args]
 *
 *  The harness serves on ports the OS chooses (or those its port variables set), and only its
 *  processes know which until they report them, so the ports reach Playwright here, before it
 *  starts: its config, every worker that re-imports it, and every helper that reads a port at
 *  import see the real ones. The command also gets `DISPATCH_E2E_REUSE_SERVERS=1`, since the
 *  servers it targets are already up (e2e/playwright.config.ts starts none). With
 *  `PLAYWRIGHT_BASE_URL` set, only the fakes a deployed server is tested with start
 *  (`bun run e2e:deployed`).
 *
 *  `DISPATCH_E2E_WORKERS` (unset or empty: one) is how many Playwright workers the run has, and
 *  this starts a lane for each before Playwright starts, in parallel: a whole harness with a
 *  Postgres, a file-store bucket, log directory and ports of its own, so no two workers read or
 *  write one server's state. Lane `i`'s variables go to the command as
 *  `DISPATCH_E2E_LANE_<i>_<V>`, beside `DISPATCH_E2E_LANES`, and lane 0's also under the plain
 *  names, so every one-worker consumer reads what it read before lanes existed: a worker picks
 *  its own through `TEST_PARALLEL_INDEX` (e2e/harness-ports.ts). Each lane's Postgres comes from
 *  `DISPATCH_E2E_PG_IMAGE` or `DISPATCH_E2E_PG_BIN` (e2e/harness-postgres.ts) and keeps its data
 *  under this run's own temporary directory. A run of one lane with `DATABASE_URL` set uses that
 *  database and starts no Postgres, as it always has; a run of more than one refuses, before
 *  anything starts, every variable a caller can only mean for one lane.
 *
 *  A Ctrl-C reaches the command itself, which shares this process group, and the harness stops
 *  once the command has; a SIGTERM sent to this process alone is passed on to the command. A
 *  signal while the harness starts stops what has started. */
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { constants, tmpdir } from "node:os";
import { join } from "node:path";
import type { Subprocess } from "bun";

import {
  laneCountVariable,
  laneVariable,
  refuseLaneConflicts,
  workerCount,
  workerCountVariable,
} from "./harness-ports";
import {
  type LaneDatabase,
  lanePostgres,
  pgBinVariable,
  pgImageVariable,
} from "./harness-postgres";
import { type Harness, startHarness } from "./harness-start";
import { Interrupted } from "./local-instance-support";

// Bun drops the first `--` after the script; a runtime that keeps it gets the same command.
const command = process.argv.slice(process.argv[2] === "--" ? 3 : 2);
if (command.length === 0) {
  console.error(
    "usage: bun e2e/run.ts -- <command> [args...], for example\n" +
      "  bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts"
  );
  process.exit(2);
}

const stopping = new AbortController();
let received: NodeJS.Signals | undefined;
let child: Subprocess | undefined;
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => {
    received ??= signal;
    stopping.abort();
    if (signal === "SIGTERM") child?.kill(signal);
  });
}
const signalStatus = (signal: NodeJS.Signals) => 128 + constants.signals[signal];

/** One lane: a harness, and the Postgres this run started for it unless the caller named the
 *  database. */
interface Lane {
  readonly index: number;
  readonly database: LaneDatabase | undefined;
  readonly harness: Harness;
  /** Every variable this lane carries: its ports, its database, its bucket. */
  readonly env: Readonly<Record<string, string>>;
  /** Where its processes write their output, unset for a run of one lane, which writes to this
   *  process's stderr as it always has. */
  readonly logDir: string | undefined;
}

const lanes: Lane[] = [];
/** The run's own temporary directory, once it has one. */
let runDir = "";

async function stopLanes(): Promise<void> {
  await Promise.allSettled(lanes.map((lane) => lane.harness.stop()));
  await Promise.allSettled(lanes.map((lane) => lane.database?.stop()));
}

/** Removes the run's own directory, which holds every lane's data directory. A failing run keeps
 *  its lanes' logs, the only record of what their servers said, and loses the data directories,
 *  which are the bulk of it; a directory the lane's Postgres made as another user (the image
 *  provider's container) can outlive the run rather than fail its exit status. */
function clearRunDir(status: number): void {
  if (runDir === "") return;
  const root = runDir;
  const keepLogs = status !== 0 && lanes.some((lane) => lane.logDir !== undefined);
  // Every data directory under the run's own, named from that directory rather than from the
  // lanes that started: a lane that failed to start left a copy of the template there and no
  // entry among them, and those copies are the bulk this branch is here to lose.
  const removed = keepLogs ? readdirSync(root).map((entry) => join(root, entry, "pgdata")) : [root];
  for (const path of removed) {
    try {
      rmSync(path, { force: true, recursive: true });
    } catch (error) {
      console.error(`e2e/run.ts: ${path} is left behind: ${(error as Error).message}`);
    }
  }
  if (keepLogs) console.error(`e2e/run.ts: the lanes' logs are in ${runDir}`);
}

function fail(error: unknown): never {
  if (error instanceof Interrupted) process.exit(signalStatus(received ?? "SIGINT"));
  console.error(`e2e/run.ts: ${error instanceof Error ? error.message : String(error)}`);
  process.exit(1);
}

/** The lane's bucket in the file store the caller started, made here with one request. A bucket
 *  is mutable state every lane would otherwise share; a run of one lane keeps the caller's own,
 *  the bucket it made itself (`.github/workflows/ci.yaml`). */
async function laneBucket(index: number, count: number): Promise<string | undefined> {
  const endpoint = (process.env.DISPATCH_E2E_FILE_STORE_ENDPOINT ?? "").trim().replace(/\/$/, "");
  const bucket = (process.env.DISPATCH_E2E_FILE_STORE_BUCKET ?? "").trim();
  if (count === 1 || endpoint === "" || bucket === "") return undefined;
  const own = `${bucket}-lane-${index}`;
  const made = await fetch(`${endpoint}/${own}`, { method: "PUT" });
  if (!made.ok) {
    throw new Error(
      `the file store at ${endpoint} refused lane ${index}'s bucket ${own}: ` +
        `${made.status} ${await made.text()}`
    );
  }
  return own;
}

/** Every lane's variables as the command reads them: each lane's under its own names, lane 0's
 *  also under the plain ones, so a one-lane run is what it was before lanes existed. */
function laneEnvironment(count: number): Record<string, string> {
  const exported: Record<string, string> = { [laneCountVariable]: String(count) };
  for (const lane of lanes) {
    for (const [variable, value] of Object.entries(lane.env)) {
      exported[laneVariable(lane.index, variable)] = value;
    }
  }
  return { ...exported, ...lanes[0]?.env };
}

/** No two lanes share a port. Each lane's listeners bind port 0 and its Postgres takes a port the
 *  OS named, so this is the check that no port was handed out twice, which would put two workers
 *  on one server. */
function refuseSharedPorts(): void {
  const owners = new Map<string, string>();
  for (const lane of lanes) {
    const ports = Object.entries(lane.env).filter(([variable]) => variable.endsWith("_PORT"));
    if (lane.database !== undefined) ports.push(["the Postgres port", String(lane.database.port)]);
    for (const [variable, port] of ports) {
      const owner = `lane ${lane.index}'s ${variable}`;
      const first = owners.get(port);
      if (first !== undefined) {
        throw new Error(
          `${first} and ${owner} are both port ${port}, so the two lanes would read one ` +
            "listener. Nothing holds a port between the moment the OS names it and the moment " +
            "its server binds it, so run again."
        );
      }
      owners.set(port, owner);
    }
  }
}

let status: number;
try {
  const count = workerCount();
  // Before a directory is made or a server starts, and before this adds its own values of the
  // variables it names, so a caller's value can never reach one lane's servers.
  if (count > 1) refuseLaneConflicts(count);
  const given = (process.env.DATABASE_URL ?? "").trim();
  runDir = mkdtempSync(join(tmpdir(), "dispatch-e2e-run-"));
  // A run of one lane against the database the caller named starts no Postgres, which is what
  // every other user of this harness does (packages/dispatch/README.md).
  const postgres =
    count === 1 && given !== ""
      ? undefined
      : lanePostgres({ dir: runDir, env: process.env, run: String(process.pid) });
  if (count > 1 && postgres === undefined) {
    throw new Error(
      `${workerCountVariable}=${count} runs ${count} lanes, each with a Postgres of its own, so ` +
        `one of ${pgImageVariable} (a \`docker run\` of that image per lane) and ` +
        `${pgBinVariable} (a directory holding initdb and pg_ctl) must name one.`
    );
  }
  const startLane = async (index: number): Promise<void> => {
    const logDir = count === 1 ? undefined : join(runDir, `lane-${index}`, "log");
    const database =
      postgres === undefined ? undefined : await postgres.start(index, stopping.signal);
    // Everything from here on runs with a server of this lane's own up, so each of them — the
    // bucket the file store makes as much as the harness — stops it rather than leave it behind.
    try {
      const own: Record<string, string> = {};
      const url = database?.url ?? given;
      if (url !== "") own.DATABASE_URL = url;
      const bucket = await laneBucket(index, count);
      if (bucket !== undefined) own.DISPATCH_E2E_FILE_STORE_BUCKET = bucket;
      const harness = await startHarness({ env: own, logDir, signal: stopping.signal });
      lanes.push({ database, env: { ...own, ...harness.env }, harness, index, logDir });
    } catch (error) {
      await database?.stop();
      throw error;
    }
  };
  // Every lane at once: each is an initdb or a `docker run` and a server build away, and they
  // share nothing. A lane that fails leaves the others to stop in the catch below.
  const started = await Promise.allSettled(
    Array.from({ length: count }, (_, index) => startLane(index))
  );
  lanes.sort((first, second) => first.index - second.index);
  const failed = started.find((lane) => lane.status === "rejected");
  if (failed !== undefined) throw (failed as PromiseRejectedResult).reason;
  refuseSharedPorts();
  if (received !== undefined) throw new Interrupted();

  if (count === 1) {
    const ports = Object.entries(lanes[0]?.harness.env ?? {}).map(
      ([variable, port]) => `${variable}=${port}`
    );
    console.error(
      `e2e/run.ts: the harness is up (${ports.join(" ")}); running ${command.join(" ")}`
    );
  } else {
    for (const lane of lanes) {
      const ports = Object.entries(lane.harness.env)
        .map(([variable, port]) => `${variable}=${port}`)
        .join(" ");
      const postgresPort =
        lane.database === undefined ? "" : `POSTGRES_PORT=${lane.database.port} `;
      console.error(`e2e/run.ts: lane ${lane.index}: ${postgresPort}${ports} (${lane.logDir})`);
    }
    const from = postgres === undefined ? "" : `, each on ${postgres.name}`;
    console.error(`e2e/run.ts: ${count} lanes are up${from}; running ${command.join(" ")}`);
  }

  let commandDone = false;
  // A harness process that stops while the command runs is named, so rows failing against a
  // server that died say why, and each lane is watched, since a row runs in whichever lane its
  // worker took.
  for (const lane of lanes) {
    const named = lanes.length === 1 ? "" : `lane ${lane.index}: `;
    lane.harness.firstExit().catch((error: Error) => {
      if (!commandDone) console.error(`e2e/run.ts: ${named}${error.message}`);
    });
  }
  try {
    child = Bun.spawn(command, {
      env: { ...process.env, ...laneEnvironment(count), DISPATCH_E2E_REUSE_SERVERS: "1" },
      stdio: ["inherit", "inherit", "inherit"],
    });
    const code = await child.exited;
    status = child.signalCode ? signalStatus(child.signalCode) : code;
  } catch (error) {
    console.error(
      `e2e/run.ts: cannot run ${command[0]}: ${error instanceof Error ? error.message : String(error)}`
    );
    status = 127;
  }
  commandDone = true;
} catch (error) {
  await stopLanes();
  clearRunDir(1);
  fail(error);
}
await stopLanes();
clearRunDir(status);
process.exit(status);
