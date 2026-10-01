// Every psql the e2e harness runs goes through here. It imports nothing from e2e/, so a module can
// import it statically without evaluating e2e/api.ts, whose module scope reads PLAYWRIGHT_BASE_URL.
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

/** psql's environment. psql is libpq, which also reads PGHOSTADDR — an address that replaces the
 *  URL's host for the connection and that pgx does not read (it is not in pgconn's environment
 *  list). With it set in a shell, every psql here would truncate a server the Dispatch server's
 *  own loopback check never saw, so it is dropped before psql runs. PGHOST and PGSERVICE stay:
 *  pgx and libpq read both, so the server's check and psql agree on them. */
export const psqlEnvironment: NodeJS.ProcessEnv = Object.fromEntries(
  Object.entries(process.env).filter(([name]) => name !== "PGHOSTADDR")
);

/** Runs psql with that environment; `args` are psql's own, the connection URL first. */
export function psql(args: string[]): Promise<{ stdout: string; stderr: string }> {
  return execFileAsync("psql", args, { env: psqlEnvironment });
}
