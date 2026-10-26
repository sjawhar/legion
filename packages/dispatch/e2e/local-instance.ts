/** `bun run dispatch:local`: the e2e harness, left running and seeded, with a signed-in URL.
 *  It starts the harness through e2e/harness-start.ts, the starter e2e/run.ts runs Playwright on,
 *  on ports the OS chooses (or those the port variables set), and prints them, so
 *  `DISPATCH_E2E_REUSE_SERVERS=1 <those ports> bunx playwright test ...` runs any spec against this
 *  instance — and empties its database and its fake Envoy, so restart it to reseed. */
import { fileURLToPath } from "node:url";

import { type Harness, startHarness } from "./harness-start";
import { databaseUrlRefusal, Interrupted, Supervisor, shellQuote } from "./local-instance-support";
import { sqlAt } from "./psql";

const dispatchPackage = fileURLToPath(new URL("..", import.meta.url));
const envoyPackage = fileURLToPath(new URL("../../envoy", import.meta.url));
/** The server packages/envoy/scripts/dev-postgres.sh serves, at its `postgres` database. */
const adminUrl = "postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable";
const localDatabase = "dispatch_local";

/** Every foreground step this command runs. Ctrl-C or a SIGTERM interrupts it from the first step
 *  on, so a stop during the web build, the server build or the seed stops what has started rather
 *  than leaving it listening: the harness stops its own processes when its signal fires. */
const supervisor = new Supervisor();
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => supervisor.interrupt());
}
const untilInterrupted = <T>(step: Promise<T>) => supervisor.untilInterrupted(step);
/** psql against the dev container's admin database, killed by an interrupt like every child. */
const adminSql = (...statements: string[]) =>
  untilInterrupted(sqlAt(adminUrl, statements, supervisor.signal));

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
 *  each port the harness reported, and the broker switch (e2e/harness-broker.ts) this shell set. */
function harnessEnvironment(databaseUrl: string, ports: Readonly<Record<string, string>>): string {
  const switches = [
    "DISPATCH_E2E_AGENT_SECRETS_URL",
    "DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE",
  ].filter((variable) => process.env[variable] !== undefined);
  return [
    `DATABASE_URL=${shellQuote(databaseUrl)}`,
    ...Object.entries(ports).map(([variable, port]) => `${variable}=${port}`),
    ...switches.map((variable) => `${variable}=${shellQuote(process.env[variable] ?? "")}`),
  ].join(" ");
}

let harness: Harness | undefined;

async function main(): Promise<void> {
  // 1. Preconditions: no deployed-server override, the tools, a loopback database.
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("unset PLAYWRIGHT_BASE_URL: this command starts its own servers");
  }
  const given = process.env.DATABASE_URL?.trim();
  for (const tool of ["go", "psql", "flock", "openssl", "python3", ...(given ? [] : ["docker"])]) {
    if (!Bun.which(tool)) throw new Error(`${tool} is required on PATH`);
  }
  const refusal = given ? databaseUrlRefusal(given) : undefined;
  if (refusal !== undefined) throw new Error(refusal);

  // 2. Postgres: DATABASE_URL as given (it is truncated and seeded), else the repo's dev container.
  // It stays ahead of the web build, so a database problem is the failure reported.
  const databaseUrl = given || (await ensureLocalPostgres());
  process.env.DATABASE_URL = databaseUrl;

  // 3. The SPA, which the server reads from web/dist on every request.
  await supervisor.run(["bun", "run", "build:web"], dispatchPackage);

  // 4. The harness, ready once every listener reported its port and the server's /healthz
  // answered. An interrupt while it starts stops what it started, and it rejects with Interrupted.
  harness = await startHarness({ signal: supervisor.signal });
  // The e2e modules read the ports when they are first imported, so they are imported only now.
  Object.assign(process.env, harness.env);
  const { baseUrl, getInbox, listAgents, listBroadcasts, userHeaders } = await import("./api");
  const { resetDatabase } = await import("./seed");
  const { seedWorkspace, workspaceSessionIds } = await import("./workspace");

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
  const fakeEnvoy = `http://127.0.0.1:${harness.env.FAKE_ENVOY_PORT}`;
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
      `    DISPATCH_E2E_REUSE_SERVERS=1 ${harnessEnvironment(databaseUrl, harness.env)} bunx ` +
      "playwright test --config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts\n" +
      "Open exactly that origin: 'localhost' is another Host and the server refuses it (421).\n" +
      "Press Ctrl-C to stop."
  );
  try {
    await untilInterrupted(harness.firstExit());
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
await harness?.stop();
await supervisor.stop();
process.exit(exitCode);
