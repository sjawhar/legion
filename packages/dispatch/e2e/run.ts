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
 *  A command that runs Playwright's test runner is refused here, before anything starts, when
 *  Playwright has not downloaded a browser the run's projects launch: it names `bun run
 *  e2e:install` and how large that download is, where Playwright's own message arrives at the
 *  first browser launch, with every lane's harness and Postgres already up.
 *
 *  A Ctrl-C reaches the command itself, which shares this process group, and the harness stops
 *  once the command has; a SIGTERM sent to this process alone is passed on to the command. A
 *  signal while the harness starts stops what has started. */
import { existsSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
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
import { harnessProcessCount } from "./harness-processes";
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

/** The browser engines a Playwright run of e2e/playwright.config.ts launches. */
const playwrightEngines = ["chromium", "firefox", "webkit"] as const;
type PlaywrightEngine = (typeof playwrightEngines)[number];

/** The one project of e2e/playwright.config.ts whose name does not hold its engine's: `iphone`
 *  is an iPhone's viewport in Chromium. Every other project is named for its engine
 *  (`chromium-plain-http`, `firefox-alone`, `webkit-iphone`), and a name this cannot read takes
 *  every engine of the mode, so a project added there is never skipped here. */
const namedProjectEngines: Readonly<Record<string, PlaywrightEngine>> = { iphone: "chromium" };

/** Whether `argv` runs Playwright's test runner, the only command this starts that launches a
 *  browser: `playwright test`, with or without the runner that resolves it (`bunx`, `npx`). A
 *  listing (`--list`) runs no spec, so it launches none either. */
function runsPlaywrightTests(argv: readonly string[]): boolean {
  const runner = argv.findIndex((word) => /(^|\/)playwright(\.js)?$/.test(word));
  if (runner === -1 || argv[runner + 1] !== "test") return false;
  return !argv.slice(runner + 2).includes("--list");
}

/** The projects `--project` names on the command line, in either spelling Playwright takes. */
function projectsOnCommandLine(argv: readonly string[]): readonly string[] {
  const projects: string[] = [];
  for (const [index, word] of argv.entries()) {
    if (word === "--project") {
      const named = argv[index + 1];
      if (named !== undefined) projects.push(named);
    } else if (word.startsWith("--project=")) projects.push(word.slice("--project=".length));
  }
  return projects;
}

/** The engines the projects this run would run launch: two-process mode has the one Chromium
 *  project, and every other run has Chromium, Firefox and WebKit projects. A `--project` narrows
 *  the run to the projects it names, so it narrows this; a name no project engine is read from
 *  leaves the mode's whole set, which refuses too much rather than too little. */
function neededEngines(argv: readonly string[]): readonly PlaywrightEngine[] {
  const mode: readonly PlaywrightEngine[] =
    harnessProcessCount === 2 ? ["chromium"] : playwrightEngines;
  const named = new Set<PlaywrightEngine>();
  for (const project of projectsOnCommandLine(argv)) {
    const engine =
      namedProjectEngines[project] ?? mode.find((candidate) => project.includes(candidate));
    if (engine === undefined) return mode;
    named.add(engine);
  }
  return named.size === 0 ? mode : [...named];
}

/** Refuses a Playwright run whose browsers Playwright has not downloaded, before a directory is
 *  made or a server starts. Playwright's own refusal comes when a worker launches the browser,
 *  with every lane's harness and Postgres already up and minutes of the run gone, and it names
 *  `playwright install`, which is not this package's command. */
async function refuseMissingBrowsers(): Promise<void> {
  if (!runsPlaywrightTests(command)) return;
  const engines = neededEngines(command);
  // Imported here rather than at the top of this module: @playwright/test takes 0.66 s to load,
  // and this script also runs commands that are no Playwright run at all (`bun e2e/run.ts --
  // psql …`, and the six refusals e2e/__tests__/harness-lanes.test.ts spawns), which would each
  // pay it for a check they skip.
  const playwright = await import("@playwright/test");
  const missing = engines
    .map((engine) => ({ engine, path: playwright[engine].executablePath() }))
    .filter((browser) => !existsSync(browser.path));
  if (missing.length === 0) return;
  const named = missing.map(({ engine, path }) => `${engine} (no ${path})`).join(", ");
  throw new Error(
    `Playwright has not downloaded ${named}. Run \`bun run e2e:install\` in packages/dispatch, ` +
      "a download of about 550 MB, then run this again. No harness and no Postgres were started."
  );
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
  // Before the lane count is read, so a run whose browsers are missing says so whatever else is
  // wrong with it.
  await refuseMissingBrowsers();
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
