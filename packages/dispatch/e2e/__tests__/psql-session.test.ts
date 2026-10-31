import { expect, test } from "bun:test";

import { sql } from "../psql";

/** The pod's or CI's database. The session protocol is psql's answer to real statements, so the
 *  cases that need one are skipped where no database is named rather than faked against a mock
 *  psql that could agree with a wrong reading of the protocol. */
const live = (process.env.DATABASE_URL ?? "") !== "";

// First in the file on purpose: `sql` resolves the database when it starts its psql, and once a
// later case has started one, no call reaches that refusal again.
test("refuses when neither PLAYWRIGHT_DATABASE_URL nor DATABASE_URL names a database", async () => {
  const named = { ...process.env };
  delete process.env.DATABASE_URL;
  delete process.env.PLAYWRIGHT_DATABASE_URL;
  try {
    await expect(sql("SELECT 1")).rejects.toThrow(
      "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
    );
  } finally {
    process.env = named;
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
