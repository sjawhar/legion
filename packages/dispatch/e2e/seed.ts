import { resetFakeEnvoy } from "./agents";
import { forgetSessions, quiesceDocuments } from "./api";
import { sql } from "./psql";

const tables = [
  "agent_tokens",
  "repo_projects",
  "architecture_sources",
  "user_issue_state",
  "user_agent_state",
  "events",
  "refs",
  "messages",
  "broadcasts",
  "comments",
  "asks",
  "doc_checkpoints",
  "doc_snapshots",
  "doc_updates",
  "artifact_versions",
  "artifacts",
  "issue_external_links",
  "issues",
  "projects",
  "users",
  "user_sessions",
];

function sqlLiteral(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

export async function insertExternalLink(issueKey: string, url: string): Promise<void> {
  await sql(
    `INSERT INTO issue_external_links (issue_key, url, kind) VALUES (${sqlLiteral(issueKey)}, ${sqlLiteral(url)}, 'url')`
  );
}

export async function setEventCreatedAt(eventId: number, iso: string): Promise<void> {
  await sql(
    `UPDATE events SET created_at = ${sqlLiteral(iso)}::timestamptz WHERE id = ${Number(eventId)}`
  );
}

/** Stamps a verified service token's subject on a seeded comment's author. The API seeder posts
 *  its actor under the shared token and the server drops a body-supplied `service`, so the only
 *  way to fixture a service-authored write is the `comments.author` jsonb itself. */
export async function setCommentAuthorService(commentId: string, service: string): Promise<void> {
  await sql(
    `UPDATE comments SET author = author || jsonb_build_object('service', ${sqlLiteral(service)}) WHERE id = ${sqlLiteral(commentId)}::uuid`
  );
}

/** Marks a newly created fixture issue as pre-creator metadata. */
export async function clearIssueCreator(issueKey: string): Promise<void> {
  await sql(`UPDATE issues SET created_by = 'null'::jsonb WHERE key = ${sqlLiteral(issueKey)}`);
}

// The DO block waits for every other session to leave its transaction. That is a barrier, not a
// fix: the server's document settlements run on their own timers, so one can open a transaction
// after the check and before TRUNCATE takes its locks. resetDatabase closes that by quiescing
// the document service first.

async function resetDatabaseOnce(): Promise<void> {
  await sql(
    `DO $$
      DECLARE open_transactions text;
      BEGIN
        FOR attempt IN 1..500 LOOP
          -- pg_stat_activity is cached per transaction; without this the loop rereads its
          -- first snapshot.
          PERFORM pg_stat_clear_snapshot();
          SELECT string_agg(format('%s (%s)', left(regexp_replace(query, '\\s+', ' ', 'g'), 120), state), '; ')
            INTO open_transactions
            FROM pg_stat_activity
            WHERE datname = current_database()
              AND backend_type = 'client backend'
              AND pid <> pg_backend_pid()
              AND state <> 'idle';
          EXIT WHEN open_transactions IS NULL;
          PERFORM pg_sleep(0.01);
        END LOOP;
        IF open_transactions IS NOT NULL THEN
          RAISE EXCEPTION 'server transactions still open after 5 s: %', open_transactions;
        END IF;
      END
    $$`,
    `TRUNCATE TABLE ${tables.join(", ")} RESTART IDENTITY CASCADE`
  );
}

/**
 * Truncates every table, after the server has closed every live document and finished the
 * settlements in flight. A settlement locks its document's owner row and then reads
 * artifact_versions, while TRUNCATE takes an exclusive lock on every table in its own order;
 * with both running PostgreSQL breaks the cycle by aborting one of them (LEGION-168), which is
 * either a failed reset or a settlement that dies mid-scenario. Quiescing first leaves the
 * server with nothing to run, so the two never overlap. It also clears the fake Envoy, whose
 * process-local subscriptions outlive a database truncate and otherwise match recycled issue keys;
 * that reset waits on nothing in the database, so it runs beside the quiesce and truncate, which
 * keep their order. Both settle before this returns, a failed one included, so a reset that fails
 * never leaves the other running into the next test's.
 * The truncate takes `user_sessions` with it, so a session cookie minted before it is refused
 * until its login signs in again: e2e/api.ts forgets its cached ones here, and a browser context
 * signs in after the reset (e2e/users.ts).
 */
export async function resetDatabase(): Promise<void> {
  const resets = await Promise.allSettled([
    quiesceDocuments().then(resetDatabaseOnce),
    resetFakeEnvoy(),
  ]);
  for (const reset of resets) if (reset.status === "rejected") throw reset.reason;
  forgetSessions();
}
