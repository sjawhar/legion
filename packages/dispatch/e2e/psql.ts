// Every psql the e2e harness runs goes through here. Of e2e/ it imports only
// e2e/harness-ports.ts, which reads the environment and opens nothing, so a module can import it
// statically without evaluating e2e/api.ts, whose module scope reads PLAYWRIGHT_BASE_URL.
import { type ChildProcessWithoutNullStreams, execFile, spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import type { Readable } from "node:stream";
import { promisify } from "node:util";

import { laneValue } from "./harness-ports";

const execFileAsync = promisify(execFile);

/** psql's environment. psql is libpq, which also reads PGHOSTADDR — an address that replaces the
 *  URL's host for the connection and that pgx does not read (it is not in pgconn's environment
 *  list). With it set in a shell, every psql here would empty a server the Dispatch server's
 *  own loopback check never saw, so it is dropped before psql runs. PGHOST and PGSERVICE stay,
 *  since pgx and libpq read both; a service entry's `hostaddr` is the one key they part on, and
 *  pgx sends it to Postgres as a setting, which refuses the connection, so the harness server
 *  never boots on such an entry. */
const psqlEnvironment: NodeJS.ProcessEnv = Object.fromEntries(
  Object.entries(process.env).filter(([name]) => name !== "PGHOSTADDR")
);

/** The database this run's SQL writes to: a deployed server's own `PLAYWRIGHT_DATABASE_URL`, else
 *  the `DATABASE_URL` of this process's lane, which is the one the caller supplied for a run of
 *  one lane (`e2e/harness-ports.ts`). A leftover deployed URL must not override a local
 *  harness's DATABASE_URL, and nothing falls back to libpq's default database, because
 *  `resetDatabase` empties whatever this names. It is read when the session connects rather than
 *  at import, so a module that only imports `sql` never refuses before the harness has set it. */
function databaseUrl(): string {
  const deployed = process.env.PLAYWRIGHT_BASE_URL
    ? process.env.PLAYWRIGHT_DATABASE_URL
    : undefined;
  const url = deployed ?? laneValue(process.env, "DATABASE_URL");
  if (url === undefined || url.trim() === "") {
    throw new Error(
      "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
    );
  }
  return url;
}

/** What psql prints when a statement is done: `\echo` on stdout and `\warn` (psql 13 and later)
 *  on stderr. The two pipes are read independently, so each needs its own end mark — everything
 *  stderr carried before it belongs to that statement, and nothing after it does. The UUID keeps
 *  a statement's own output from ever reading as a mark. psql's verdict on the statement follows
 *  the first mark on stdout, under a mark of its own (`${mark}-verdict`), so the statement's own
 *  output is what ends at the first one. */
const sentinel = `--psql-sentinel-${randomUUID()}`;
let sentinelCount = 0;

/** The psql variable a statement's own `:ERROR` is captured in before the terminator `send`
 *  appends can overwrite it. */
const verdictVariable = "e2e_verdict";

/** One of the child's pipes, read in the pieces the sentinels cut it into. One statement runs at
 *  a time, so at most one read waits. */
type MarkedStream = {
  /** Everything the stream carried before `mark`'s line, with the line consumed. */
  readTo: (mark: string) => Promise<string>;
  /** Fails the waiting read, and every later one: the child that would print the mark is gone. */
  fail: (error: Error) => void;
};

function markedStream(stream: Readable): MarkedStream {
  let buffered = "";
  let waiting: {
    mark: string;
    settled: (text: string) => void;
    failed: (e: Error) => void;
  } | null = null;
  let lost: Error | null = null;

  function deliver(): void {
    if (waiting === null) return;
    if (lost !== null) {
      const { failed } = waiting;
      waiting = null;
      failed(lost);
      return;
    }
    const line = `${waiting.mark}\n`;
    const at = buffered.indexOf(line);
    if (at < 0) return;
    const text = buffered.slice(0, at);
    buffered = buffered.slice(at + line.length);
    const { settled } = waiting;
    waiting = null;
    settled(text);
  }

  stream.setEncoding("utf8");
  stream.on("data", (chunk: string) => {
    buffered += chunk;
    deliver();
  });

  return {
    readTo: (mark) =>
      new Promise<string>((settled, failed) => {
        // One statement runs at a time, so a second read means the caller broke that: the first
        // promise would be neither settled nor failed, and its call would wait for a mark
        // nothing reads any more.
        if (waiting !== null) {
          throw new Error(`a read of ${waiting.mark} is already waiting, so ${mark} cannot be`);
        }
        waiting = { mark, settled, failed };
        deliver();
      }),
    fail: (error) => {
      lost = error;
      deliver();
    },
  };
}

type Session = { child: ChildProcessWithoutNullStreams; out: MarkedStream; errors: MarkedStream };

/** The one psql of this process, started by the first `sql` call. A process per call cost a spawn,
 *  a connection and a teardown — about 400 ms of `resetDatabase`'s 441 — for statements that take
 *  single-digit milliseconds on an open connection. */
let session: Session | null = null;

function startSession(): Session {
  // No ON_ERROR_STOP: it makes psql exit on the first error, which would take the connection with
  // it. A failing statement is recognised by psql's own verdict on it (`send`) instead, and the
  // session stays open for the next caller.
  const child = spawn("psql", [databaseUrl(), "-X", "-v", "VERBOSITY=verbose", "-tA"], {
    env: psqlEnvironment,
  });
  const started: Session = {
    child,
    out: markedStream(child.stdout),
    errors: markedStream(child.stderr),
  };
  const lose = (error: Error): void => {
    if (session === started) session = null;
    started.out.fail(error);
    started.errors.fail(error);
  };
  child.on("error", lose);
  child.on("exit", (code, signal) =>
    lose(new Error(`psql exited (code ${code}, signal ${signal})`))
  );
  // EPIPE on a child that has already gone is an exit this has handled, not an unhandled error.
  child.stdin.on("error", () => {});
  // The pipes are net sockets, and an `error` on one with no listener is an uncaught exception
  // that would take the Playwright worker with it; here it is the same loss of the session the
  // child's own `error` is.
  child.stdout.on("error", lose);
  child.stderr.on("error", lose);
  // A Playwright worker ends when its event loop empties, and a piped child holds the loop open
  // through its own handle and its three pipes. Unref'ed, none of them keeps a finished worker
  // alive; psql then reads EOF on the stdin this process leaves behind and exits by itself, and
  // the kill below closes it at once rather than on the next read.
  child.unref();
  for (const pipe of [child.stdin, child.stdout, child.stderr]) {
    // The pipes are net sockets, whose stream types do not say so.
    if ("unref" in pipe && typeof pipe.unref === "function") pipe.unref();
  }
  process.once("exit", () => session?.child.kill());
  return started;
}

/** Runs one statement and returns what each stream carried for it, and psql's verdict on it. The
 *  trailing `;` on its own line terminates a statement the caller left unterminated without
 *  swallowing a last-line comment; a statement that already ends in one leaves an empty query,
 *  which runs as a query of no statements and whose success sets `:ERROR` false. So the verdict
 *  is taken twice: `ERROR` is cleared before the statement, captured right after the text the
 *  caller sent — which is that text's own verdict when it terminated itself, and the cleared
 *  false when it left its last query for the terminator — and read again after the terminator,
 *  which is that pending query's verdict. The statement failed when either says so. psql keeps
 *  one verdict, so a caller that sends several queries in one string is judged by the last. */
async function send(
  open: Session,
  statement: string
): Promise<{ out: string; errors: string; failed: boolean }> {
  sentinelCount += 1;
  const mark = `${sentinel}-${sentinelCount}`;
  open.child.stdin.write(
    `\\set ERROR false\n${statement}\n\\set ${verdictVariable} :ERROR\n;\n` +
      `\\echo ${mark}\n\\echo :${verdictVariable} :ERROR\n\\echo ${mark}-verdict\n\\warn ${mark}\n`
  );
  const [out, errors] = await Promise.all([open.out.readTo(mark), open.errors.readTo(mark)]);
  const verdict = await open.out.readTo(`${mark}-verdict`);
  return { errors, failed: verdict.trim().split(/\s+/).includes("true"), out };
}

async function runStatements(statements: readonly string[]): Promise<string> {
  if (session === null) session = startSession();
  const open = session;
  const output: string[] = [];
  for (const statement of statements) {
    const { out, errors, failed } = await send(open, statement);
    if (failed) {
      // What ON_ERROR_STOP used to do: nothing after the failing statement runs. The transaction
      // the failure aborted is rolled back here, so the next caller starts on a usable session;
      // whatever that ROLLBACK itself says is this call's business no longer.
      await send(open, "ROLLBACK").catch(() => undefined);
      throw new Error(errors.trim());
    }
    output.push(out);
  }
  return output.join("").trim();
}

/** Whether a call holds the session's connection. */
let busy = false;

/** Runs each statement, in order on one connection, against this run's database and returns the
 *  trimmed output. It stops at the first statement psql reports an error for and rejects with that
 *  error's own text, SQLSTATE and location included (`VERBOSITY=verbose`), so the reset's
 *  `RAISE EXCEPTION` naming a lock's holders reaches whoever reads the failure. A statement psql
 *  answers with a NOTICE or a WARNING is not one that failed, though it writes to the same pipe:
 *  the verdict is psql's own `:ERROR` (`send`), which neither sets.
 *
 *  A call that arrives while another holds the session runs as a psql of its own, which is what
 *  every call was before. One caller's statements can then never interleave with another's, and
 *  two calls still reach the database at once: e2e/keyed-writes.e2e.ts holds a row locked in one
 *  call and watches pg_stat_activity for it in another, which one connection cannot do. A failure
 *  on that path is `execFile`'s own `Command failed: psql …` with psql's text under it, rather
 *  than psql's text alone: the two paths spell a failure differently. */
export async function sql(...statements: string[]): Promise<string> {
  if (busy) return sqlAt(databaseUrl(), statements);
  busy = true;
  try {
    return await runStatements(statements);
  } finally {
    busy = false;
  }
}

/** `sql` against the database `url` names rather than this run's, as a psql of its own: `bun run
 *  dispatch:local` reads and creates its database through the server's `postgres` database with
 *  it, passing its stop `signal`, which kills that psql. psql reads no `~/.psqlrc` (`-X`), stops
 *  at the first error with its SQLSTATE and location, and prints a query's values bare (`-tA`), so
 *  a one-value query returns that value. */
export async function sqlAt(
  url: string,
  statements: readonly string[],
  signal?: AbortSignal
): Promise<string> {
  const { stdout } = await execFileAsync(
    "psql",
    [
      url,
      "-X",
      "-v",
      "ON_ERROR_STOP=1",
      "-v",
      "VERBOSITY=verbose",
      "-tA",
      ...statements.flatMap((statement) => ["-c", statement]),
    ],
    { env: psqlEnvironment, signal }
  );
  return stdout.trim();
}
