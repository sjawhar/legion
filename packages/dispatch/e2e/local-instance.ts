/** `bun run dispatch:local`: the e2e harness, left running and seeded, with a signed-in URL.
 *  It starts every listener the Playwright config starts (e2e/harness-listeners.ts), on the same
 *  ports and variables, so `DISPATCH_E2E_REUSE_SERVERS=1 bunx playwright test ...` runs any spec
 *  against this instance — and empties its database and its fake Envoy, so restart it to reseed. */
import { fileURLToPath } from "node:url";
import type { Subprocess } from "bun";

import { baseUrl, getInbox, listAgents, listBroadcasts, userHeaders } from "./api";
import { type HarnessListener, harnessListeners } from "./harness-listeners";
import { harnessPorts } from "./harness-ports";
import { isPortUsed } from "./port-probe";
import { sqlAt } from "./psql";
import { resetDatabase } from "./seed";
import { seedWorkspace, workspaceSessionIds } from "./workspace";

const dispatchPackage = fileURLToPath(new URL("..", import.meta.url));
const envoyPackage = fileURLToPath(new URL("../../envoy", import.meta.url));
/** The server packages/envoy/scripts/dev-postgres.sh serves, at its `postgres` database. */
const adminUrl = "postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable";
const localDatabase = "dispatch_local";
/** run-server.sh builds the server before it listens, and a cold `go build` on a busy machine is
 *  the long pole, so the server gets the ten minutes the docs media harness gives it. */
const serverBootMs = 600_000;
const listenerBootMs = 30_000;

interface Running {
  readonly name: string;
  readonly child: Subprocess;
}

/** Every process this command has started and not yet seen stop. */
const children: Running[] = [];

/** Ctrl-C or a SIGTERM. Handled from the first step on, so a stop during the web build, the
 *  server build or the seed stops what has started rather than leaving it listening. */
const interrupt = new AbortController();
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => interrupt.abort());
}

class Interrupted extends Error {}

/** `step`, or Interrupted as soon as a signal arrives. A step the signal outruns settles later
 *  into nothing: stopChildren ends the processes it waits on. */
function untilInterrupted<T>(step: Promise<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const onInterrupt = () => reject(new Interrupted("interrupted"));
    if (interrupt.signal.aborted) {
      onInterrupt();
      return;
    }
    interrupt.signal.addEventListener("abort", onInterrupt, { once: true });
    step
      .then(resolve, reject)
      .finally(() => interrupt.signal.removeEventListener("abort", onInterrupt));
  });
}

/** Starts `argv` as the leader of a process group of its own, so stopping it reaches what it
 *  starts in turn: run-server.sh's `flock … go build` before it execs the server, and the vite
 *  process `bun run build:web` runs. A terminal's Ctrl-C reaches only this command, which stops
 *  them in order. Their output is this terminal's. */
function start(name: string, argv: readonly string[], cwd: string): Running {
  const child = Bun.spawn([...argv], {
    cwd,
    detached: true,
    env: process.env,
    stderr: "inherit",
    stdin: "ignore",
    stdout: "inherit",
  });
  const running = { child, name };
  children.push(running);
  return running;
}

/** SIGTERM to every process group this command started, SIGKILL to whatever still runs ten
 *  seconds later, and a return the moment every leader has exited: the force-kill timer is
 *  cleared then, so a clean Ctrl-C does not keep the shell prompt waiting on it. */
async function stopChildren(): Promise<void> {
  const running = children.splice(0);
  const signalGroups = (signal: NodeJS.Signals) => {
    for (const { child } of running) {
      try {
        process.kill(-child.pid, signal);
      } catch {
        // ESRCH: every process of that group has already exited.
      }
    }
  };
  signalGroups("SIGTERM");
  const forceKill = setTimeout(() => signalGroups("SIGKILL"), 10_000);
  await Promise.all(running.map(({ child }) => child.exited));
  clearTimeout(forceKill);
}

/** A foreground step. It throws on a non-zero exit, so a failed web build, docker start or
 *  dev-postgres script reaches the one catch below (and stopChildren) like every other failure,
 *  never a half-run exit that leaves a child listening. */
async function run(argv: readonly string[], cwd: string): Promise<void> {
  const step = start(argv.join(" "), argv, cwd);
  const code = await untilInterrupted(step.child.exited);
  children.splice(children.indexOf(step), 1);
  if (code !== 0) throw new Error(`${step.name} exited ${code}`);
}

/** Waits until `listener` is ready: the Dispatch server once its `/healthz` answers 200, which
 *  the reset below waits for, and every other listener once its port takes a connection, which is
 *  what Playwright's `webServer` waits for (the plain-HTTP proxy answers no request addressed to
 *  127.0.0.1). It fails as soon as `running` exits, so a server whose build failed is reported at
 *  once rather than when the wait runs out. */
async function waitUntilReady(listener: HarnessListener, running: Running): Promise<void> {
  const server = listener.port === harnessPorts.dispatch.port;
  const target = server ? `${baseUrl}/healthz` : `127.0.0.1:${listener.port}`;
  const timeoutMs = server ? serverBootMs : listenerBootMs;
  const ready = async () => {
    if (!server) return isPortUsed(listener.port);
    try {
      return (await fetch(target)).ok;
    } catch {
      return false;
    }
  };
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const { child, name } = running;
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error(
        `${name} exited ${child.exitCode ?? child.signalCode} before ${target} answered`
      );
    }
    if (await ready()) return;
    if (Date.now() > deadline) {
      throw new Error(`${target} (${name}) did not answer within ${timeoutMs / 1000} s`);
    }
    await untilInterrupted(Bun.sleep(500));
  }
}

function check(ok: boolean, what: string): void {
  if (!ok) throw new Error(`smoke failed: ${what}`);
}

/** The command's own early refusal of a database it must not truncate. It reads the URL with the
 *  URL parser, which is not libpq's: a `?host=` query parameter replaces the host for libpq and
 *  pgx but not for `new URL`, and a key=value DSN or a multi-host list is not one URL at all
 *  (refused here, naming the variable). That gap is closed by ORDER, not by this check: the
 *  Dispatch server's own pgx check (cmd/dispatch loopbackDatabase) refuses to boot on a
 *  non-loopback host, and resetDatabase runs only after /healthz answered, so a URL this check
 *  wrongly approves truncates nothing. Keep the health wait ahead of the reset. */
function assertLoopbackDatabaseUrl(raw: string): void {
  let hostname: string;
  try {
    hostname = new URL(raw).hostname.toLowerCase();
  } catch {
    throw new Error(
      "DATABASE_URL must be a single postgres:// URL naming a loopback host: this command " +
        "truncates it"
    );
  }
  if (!["127.0.0.1", "localhost", "[::1]"].includes(hostname)) {
    throw new Error(
      "DATABASE_URL must name a loopback Postgres (127.0.0.1, localhost or [::1]), not " +
        `${hostname || "no host"}: this command truncates it`
    );
  }
}

/** The repository's dev Postgres through packages/envoy/scripts/dev-postgres.sh, which creates,
 *  starts or reuses the `dispatch-pg` container and refuses one on any image but its pinned
 *  pgvector one, and its database `dispatch_local`, created once. psql runs through e2e/psql.ts,
 *  without PGHOSTADDR: libpq would otherwise connect to that address instead of the URL's host,
 *  and no check here or in the server sees it. */
async function ensureLocalPostgres(): Promise<string> {
  await run(["bash", "scripts/dev-postgres.sh"], envoyPackage);
  const deadline = Date.now() + 30_000;
  for (;;) {
    try {
      await untilInterrupted(sqlAt(adminUrl, "select 1"));
      break;
    } catch (error) {
      if (error instanceof Interrupted || Date.now() >= deadline) throw error;
      await untilInterrupted(Bun.sleep(500));
    }
  }
  const exists = `select 1 from pg_database where datname = '${localDatabase}'`;
  if ((await untilInterrupted(sqlAt(adminUrl, exists))) === "") {
    await untilInterrupted(sqlAt(adminUrl, `create database ${localDatabase}`));
  }
  return adminUrl.replace("/postgres?", `/${localDatabase}?`);
}

/** The harness inputs a command needs to reach this instance, as a shell prefix: the database,
 *  and each port variable and the broker switch (e2e/harness-broker.ts) this shell set. */
function harnessEnvironment(databaseUrl: string): string {
  const variables = [
    ...harnessListeners.map((listener) => listener.variable),
    "DISPATCH_E2E_AGENT_SECRETS_URL",
    "DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE",
  ];
  const given = variables.filter((variable) => process.env[variable] !== undefined);
  return [
    `DATABASE_URL='${databaseUrl}'`,
    ...given.map((variable) => `${variable}='${process.env[variable]}'`),
  ].join(" ");
}

async function main(): Promise<void> {
  // 1. Preconditions: no deployed-server override, the tools, a loopback database, free ports.
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("unset PLAYWRIGHT_BASE_URL: this command starts its own servers");
  }
  const given = process.env.DATABASE_URL?.trim();
  for (const tool of ["go", "psql", "flock", "openssl", ...(given ? [] : ["docker"])]) {
    if (!Bun.which(tool)) throw new Error(`${tool} is required on PATH`);
  }
  if (given) assertLoopbackDatabaseUrl(given);
  const used = await Promise.all(harnessListeners.map((listener) => isPortUsed(listener.port)));
  const taken = harnessListeners.filter((_, index) => used[index]);
  if (taken.length > 0) {
    const ports = taken.map((listener) => `${listener.port} (${listener.variable})`).join(", ");
    const variables = harnessListeners.map((listener) => listener.variable).join("/");
    throw new Error(
      `already in use: ${ports}. Stop that instance, or move this one with ${variables} and ` +
        "its own DATABASE_URL."
    );
  }

  // 2. Postgres: DATABASE_URL as given (it is truncated and seeded), else the repo's dev container.
  const databaseUrl = given || (await ensureLocalPostgres());
  process.env.DATABASE_URL = databaseUrl;

  // 3. The SPA, which the server reads from web/dist on every request.
  await run(["bun", "run", "build:web"], dispatchPackage);

  // 4. The harness processes, then readiness.
  await untilInterrupted(
    Promise.all(
      harnessListeners.map((listener) =>
        waitUntilReady(listener, start(listener.name, listener.argv, dispatchPackage))
      )
    )
  );

  // 5. Reset, seed, and prove the signed-in surfaces over HTTP before handing out the URL. The
  // broadcast's recipient ORDER is deliberately not checked here: e2e/broadcast.e2e.ts owns that
  // assertion, and this instance has to start on a server without the fix so that spec can go
  // red against it. The reset runs only now, after the server's own loopback check let it boot.
  await untilInterrupted(resetDatabase());
  const seeded = await untilInterrupted(seedWorkspace());
  const whoami = await fetch(`${baseUrl}/auth/whoami`, { headers: await userHeaders("alice") });
  const identity = (await whoami.json()) as { login?: string };
  check(whoami.status === 200 && identity.login === "alice", "signed-in whoami");
  check((await getInbox({ login: "alice" })).length === seeded.inboxRows, "inbox rows");
  const [newest] = await listBroadcasts({ login: "alice" });
  check(newest?.id === seeded.broadcast.id, "broadcast listed");
  const agents = new Set((await listAgents({ login: "alice" })).map((agent) => agent.session_id));
  check(
    workspaceSessionIds.every((id) => agents.has(id)),
    "agents listed"
  );

  // 6. Hand over, and run until Ctrl-C or until one of the processes stops by itself.
  const fakeEnvoy = `http://127.0.0.1:${harnessPorts.fakeEnvoy.port}`;
  console.log(
    "\nDispatch local instance is up.\n" +
      `  Open (signed in as alice): ${baseUrl}/auth/_dev/signin?login=alice&next=/\n` +
      `  Sign in as bob:            ${baseUrl}/auth/_dev/signin?login=bob&next=/\n` +
      `  Seeded broadcast:          ${baseUrl}/agents/broadcasts/${seeded.broadcast.id}\n` +
      `  Agent bearer:              e2e-token (for a local Oh My Pi session: DISPATCH_URL=${baseUrl} ` +
      "DISPATCH_TOKEN=e2e-token, with DISPATCH_TOKEN_FILE unset since it wins)\n" +
      `  Database (truncated and reseeded on every start): ${databaseUrl}\n` +
      `  Fake Envoy: live sessions GET ${fakeEnvoy}/v1/sessions; ` +
      `reseed them with PUT ${fakeEnvoy}/__fixture/sessions\n` +
      "  Run a spec against it from packages/dispatch (this empties the instance; restart it to " +
      "reseed):\n" +
      `    DISPATCH_E2E_REUSE_SERVERS=1 ${harnessEnvironment(databaseUrl)} bunx playwright test ` +
      "--config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts\n" +
      "Open exactly that origin: 'localhost' is another Host and the server refuses it (421).\n" +
      "Press Ctrl-C to stop."
  );
  const firstExit = Promise.race(
    children.map(async ({ child, name }) => {
      const code = await child.exited;
      throw new Error(`${name} exited ${code ?? child.signalCode} while the instance was up`);
    })
  );
  try {
    await untilInterrupted(firstExit);
  } catch (error) {
    if (error instanceof Interrupted) return;
    throw error;
  }
}

let exitCode = 0;
try {
  await main();
  console.log("Stopping the Dispatch local instance.");
} catch (error) {
  if (error instanceof Interrupted) {
    console.error("dispatch:local interrupted before the instance was up");
    exitCode = 130;
  } else {
    console.error(
      `dispatch:local failed: ${error instanceof Error ? error.message : String(error)}`
    );
    exitCode = 1;
  }
}
await stopChildren();
process.exit(exitCode);
