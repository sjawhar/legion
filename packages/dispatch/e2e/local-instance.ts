/** `bun run dispatch:local`: the e2e harness, left running and seeded, with a signed-in URL.
 *  It starts every listener the Playwright config starts (e2e/harness-listeners.ts), on the same
 *  ports and variables, so `DISPATCH_E2E_REUSE_SERVERS=1 bunx playwright test ...` runs any spec
 *  against this instance — and empties its database and its fake Envoy, so restart it to reseed. */
import { fileURLToPath } from "node:url";

import { baseUrl, getInbox, listAgents, listBroadcasts, userHeaders } from "./api";
import { type HarnessListener, harnessListeners } from "./harness-listeners";
import { harnessPorts } from "./harness-ports";
import { databaseUrlRefusal, Interrupted, Supervisor, shellQuote } from "./local-instance-support";
import { isPortUsed, takenPorts } from "./port-probe";
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

/** Every process this command starts. Ctrl-C or a SIGTERM interrupts it from the first step on,
 *  so a stop during the web build, the server build or the seed stops what has started rather
 *  than leaving it listening. */
const supervisor = new Supervisor();
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => supervisor.interrupt());
}
const untilInterrupted = <T>(step: Promise<T>) => supervisor.untilInterrupted(step);
/** psql against the dev container's admin database, killed by an interrupt like every child. */
const adminSql = (...statements: string[]) =>
  untilInterrupted(sqlAt(adminUrl, statements, supervisor.signal));

/** Waits until `listener` is ready: the Dispatch server once its `/healthz` answers 200, which
 *  the reset below waits for, and every other listener once its port takes a connection, which is
 *  what Playwright's `webServer` waits for (the plain-HTTP proxy answers no request addressed to
 *  127.0.0.1). It fails as soon as the process that serves it exits, so a server whose build
 *  failed is reported at once rather than when the wait runs out. */
async function waitUntilReady(listener: HarnessListener): Promise<void> {
  const { child, name } = supervisor.start(listener.name, listener.argv, dispatchPackage);
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

/** The repository's dev Postgres through packages/envoy/scripts/dev-postgres.sh, which creates,
 *  starts or reuses the `dispatch-pg` container and refuses one on any image but its pinned
 *  pgvector one, and its database `dispatch_local`, created once. psql runs through e2e/psql.ts,
 *  without PGHOSTADDR: libpq would otherwise connect to that address instead of the URL's host,
 *  and no check here or in the server sees it. */
async function ensureLocalPostgres(): Promise<string> {
  await supervisor.run(["bash", "scripts/dev-postgres.sh"], envoyPackage);
  const deadline = Date.now() + 30_000;
  for (;;) {
    try {
      await adminSql("select 1");
      break;
    } catch (error) {
      if (error instanceof Interrupted || Date.now() >= deadline) throw error;
      await untilInterrupted(Bun.sleep(500));
    }
  }
  if ((await adminSql(`select 1 from pg_database where datname = '${localDatabase}'`)) === "") {
    await adminSql(`create database ${localDatabase}`);
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
    `DATABASE_URL=${shellQuote(databaseUrl)}`,
    ...given.map((variable) => `${variable}=${shellQuote(process.env[variable] ?? "")}`),
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
  const refusal = given ? databaseUrlRefusal(given) : undefined;
  if (refusal !== undefined) throw new Error(refusal);
  const ports = await takenPorts(harnessListeners);
  if (ports !== undefined) {
    const variables = harnessListeners.map((listener) => listener.variable).join("/");
    throw new Error(
      `already in use: ${ports}. Stop that instance, or move this one with ${variables} and ` +
        "its own DATABASE_URL."
    );
  }

  // 2. Postgres: DATABASE_URL as given (it is truncated and seeded), else the repo's dev container.
  // It stays ahead of the web build, so a database problem is the failure reported.
  const databaseUrl = given || (await ensureLocalPostgres());
  process.env.DATABASE_URL = databaseUrl;

  // 3. The SPA, which the server reads from web/dist on every request.
  await supervisor.run(["bun", "run", "build:web"], dispatchPackage);

  // 4. The harness processes, then readiness.
  await untilInterrupted(Promise.all(harnessListeners.map(waitUntilReady)));

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
  try {
    await untilInterrupted(supervisor.firstExit());
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
await supervisor.stop();
process.exit(exitCode);
