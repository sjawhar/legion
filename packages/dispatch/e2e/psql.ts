// Every psql the e2e harness runs goes through here. It imports nothing from e2e/, so a module can
// import it statically without evaluating e2e/api.ts, whose module scope reads PLAYWRIGHT_BASE_URL.
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

/** psql's environment. psql is libpq, which also reads PGHOSTADDR — an address that replaces the
 *  URL's host for the connection and that pgx does not read (it is not in pgconn's environment
 *  list). With it set in a shell, every psql here would truncate a server the Dispatch server's
 *  own loopback check never saw, so it is dropped before psql runs. PGHOST and PGSERVICE stay,
 *  since pgx and libpq read both; a service entry's `hostaddr` is the one key they part on, and
 *  pgx sends it to Postgres as a setting, which refuses the connection, so the harness server
 *  never boots on such an entry. */
const psqlEnvironment: NodeJS.ProcessEnv = Object.fromEntries(
  Object.entries(process.env).filter(([name]) => name !== "PGHOSTADDR")
);

/** The database this run's SQL writes to: a deployed server's own `PLAYWRIGHT_DATABASE_URL`, else
 *  the `DATABASE_URL` the caller supplied. A leftover deployed URL must not override a local
 *  harness's DATABASE_URL, and nothing falls back to libpq's default database, because
 *  `resetDatabase` truncates whatever this names. */
function databaseUrl(): string {
  const deployed = process.env.PLAYWRIGHT_BASE_URL
    ? process.env.PLAYWRIGHT_DATABASE_URL
    : undefined;
  const url = deployed ?? process.env.DATABASE_URL;
  if (url === undefined || url.trim() === "") {
    throw new Error(
      "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
    );
  }
  return url;
}

/** Runs each statement, in order on one connection, against this run's database and returns the
 *  trimmed output. psql reads no `~/.psqlrc` (`-X`), stops at the first error with its SQLSTATE
 *  and location, and prints a query's values bare (`-tA`), so a one-value query returns that
 *  value. */
export async function sql(...statements: string[]): Promise<string> {
  const { stdout } = await execFileAsync(
    "psql",
    [
      databaseUrl(),
      "-X",
      "-v",
      "ON_ERROR_STOP=1",
      "-v",
      "VERBOSITY=verbose",
      "-tA",
      ...statements.flatMap((statement) => ["-c", statement]),
    ],
    { env: psqlEnvironment }
  );
  return stdout.trim();
}
