import { afterAll, beforeAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { captureEnvironment } from "@legion/test-fixtures/environment";
import { hostTool } from "@legion/test-fixtures/host-tools";

import { sql, sqlAt } from "../psql";

/** The PostgreSQL server this run may make a database on: an issue pod names it, a developer
 *  names it, and the `dispatch` CI job has no Postgres at all, so these cases skip there. The
 *  session protocol is psql's answer to real statements, so the cases that need a database are
 *  skipped where no server is named rather than faked against a mock psql that could agree with a
 *  wrong reading of the protocol. */
const server = process.env.LEGION_TEST_PG_DSN ?? "";
/** Everything those cases need and this host or run does not have. The psql half is the gate
 *  `skipWithout("psql")` is, composed here rather than called: it prints a line of its own, and a
 *  host that has neither psql nor a server would get one line for each half rather than one
 *  naming both (harness-lanes.test.ts composes its Playwright gate the same way). */
const missing = [
  ...(hostTool("psql") === null ? ["psql on this host's PATH"] : []),
  ...(server === ""
    ? ["LEGION_TEST_PG_DSN, naming a server this run may create a database on"]
    : []),
];
const live = missing.length === 0;
if (!live) {
  console.warn(
    `Skipped the tests in ${import.meta.path} that need a database. This run does not have: ` +
      `${missing.join("; ")}. Supply what is missing and run the tests again.`
  );
}

/** The database of this run alone, made in `beforeAll` and dropped in `afterAll`. The cases here
 *  create temporary tables and read pg_stat_activity, so they run on a database nobody else is
 *  using rather than on whatever a developer's own DATABASE_URL happens to name. `create
 *  database` copies template1, which carries the `vector` extension on the servers this runs on,
 *  and nothing here needs it anyway. */
const database = `dispatch_psql_session_${randomUUID().replaceAll("-", "_")}`;
const databaseUrl = (): string => {
  const url = new URL(server);
  url.pathname = `/${database}`;
  return url.href;
};

/** Puts back what the environment held before this file pointed `sql` at its own database. */
let restoreNames: (() => void) | undefined;

beforeAll(async () => {
  if (!live) return;
  await sqlAt(server, [`create database ${database}`]);
  // `sql` resolves its database from the environment when it starts its psql, so this is where
  // the run's own database replaces whatever the caller's environment named. A deployed URL wins
  // over DATABASE_URL in that resolution, so both names of that pair go.
  restoreNames = captureEnvironment([
    "DATABASE_URL",
    "PLAYWRIGHT_BASE_URL",
    "PLAYWRIGHT_DATABASE_URL",
  ]);
  process.env.DATABASE_URL = databaseUrl();
  delete process.env.PLAYWRIGHT_BASE_URL;
  delete process.env.PLAYWRIGHT_DATABASE_URL;
}, 60_000);

afterAll(async () => {
  restoreNames?.();
  if (!live) return;
  // FORCE because this process's own psql still holds a connection to the database: it is killed
  // by the `exit` handler e2e/psql.ts installs, which runs after this one. The connection is the
  // test role's own, which that role may terminate.
  await sqlAt(server, [`drop database if exists ${database} with (force)`]);
}, 60_000);

// First in the file on purpose: `sql` resolves the database when it starts its psql, and once a
// later case has started one, no call reaches that refusal again. It is the refusal of this
// process's own `sql` that the order proves, so the case runs here rather than in a child, and it
// puts the two names back rather than replacing `process.env`: a replaced object is no longer the
// runtime's environment, so every later `Bun.spawn` of the run would inherit the copy's values
// and a later write would reach neither the real environment nor `Bun.env`.
test("refuses when neither PLAYWRIGHT_DATABASE_URL nor DATABASE_URL names a database", async () => {
  const restore = captureEnvironment(["DATABASE_URL", "PLAYWRIGHT_DATABASE_URL"]);
  delete process.env.DATABASE_URL;
  delete process.env.PLAYWRIGHT_DATABASE_URL;
  try {
    await expect(sql("SELECT 1")).rejects.toThrow(
      "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
    );
  } finally {
    restore();
  }
});

// A temporary table lives in its own connection, so one that answers a later call is itself the
// proof that every call ran on the one psql this process started.
test.skipIf(!live)(
  "stops at the first failing statement, on a session that outlives the call",
  async () => {
    await sql("CREATE TEMP TABLE statement_log (ran int)");

    await expect(
      sql(
        "INSERT INTO statement_log VALUES (1)",
        "SELECT nosuchcolumn",
        "INSERT INTO statement_log VALUES (3)"
      )
    ).rejects.toThrow(/nosuchcolumn/);

    expect(
      await sql("SELECT coalesce(string_agg(ran::text, ',' ORDER BY ran), '') FROM statement_log")
    ).toBe("1");
    expect(await sql("SELECT 3")).toBe("3");
  }
);

test.skipIf(!live)(
  "rolls back the transaction a failure aborted before the next call",
  async () => {
    await expect(
      sql(
        "BEGIN",
        "INSERT INTO statement_log VALUES (2)",
        "SELECT nosuchcolumn",
        "COMMIT",
        "INSERT INTO statement_log VALUES (4)"
      )
    ).rejects.toThrow(/nosuchcolumn/);

    // Without the ROLLBACK the session sends, this INSERT would be refused as part of an aborted
    // transaction; with it, the insert lands and nothing of the failed call did.
    await sql("INSERT INTO statement_log VALUES (5)");
    expect(
      await sql("SELECT coalesce(string_agg(ran::text, ',' ORDER BY ran), '') FROM statement_log")
    ).toBe("1,5");
  }
);

test.skipIf(!live)("keeps each caller's statements together when two calls overlap", async () => {
  const [first, second] = await Promise.all([
    sql("SELECT 'a1'", "SELECT 'a2'"),
    sql("SELECT 'b1'", "SELECT 'b2'"),
  ]);
  expect(first).toBe("a1\na2");
  expect(second).toBe("b1\nb2");
});

// e2e/keyed-writes.e2e.ts holds a row locked in one call while it watches pg_stat_activity for
// that wait in another, so an overlapping call must reach the database beside the session's
// transaction rather than behind it.
test.skipIf(!live)("runs a call that overlaps another on a second connection", async () => {
  const holding = sql("BEGIN", "SELECT pg_sleep(3)", "COMMIT");
  const sleeping = await sql(
    "SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND query LIKE 'SELECT pg_sleep(3)%'"
  );
  await holding;
  expect(sleeping).toBe("1");
});

// A NOTICE and a WARNING go to the same pipe an error does, so what a statement wrote there is
// not what it is judged by: e2e/failed-room.e2e.ts drops a trigger that is not there between its
// rows, and Postgres answers that with a NOTICE.
test.skipIf(!live)("keeps a statement psql only wrote a notice or a warning for", async () => {
  await sql("CREATE TEMP TABLE notice_log (ran int)");
  // psql prints a command's own tag before the next statement's rows, `-tA` and all.
  expect(await sql("DROP TRIGGER IF EXISTS never_made ON notice_log; SELECT 'dropped'")).toBe(
    "DROP TRIGGER\ndropped"
  );
  expect(
    await sql("DO $$ BEGIN RAISE WARNING 'a warning is not a failure'; END $$", "SELECT 'warned'")
  ).toBe("DO\nwarned");
});

// Both spellings a caller sends: a statement it terminated itself leaves psql an empty query
// after it, whose own verdict is success, and one it left open is run by the terminator the
// session appends.
test.skipIf(!live)("rejects a statement that really failed with psql's own text", async () => {
  await expect(sql("SELECT nosuchcolumn")).rejects.toThrow(/nosuchcolumn/);
  await expect(sql("SELECT nosuchcolumn;")).rejects.toThrow(/nosuchcolumn/);
  expect(await sql("SELECT 'alive'")).toBe("alive");
});
