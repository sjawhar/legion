import { resetFakeEnvoy } from "./agents";
import { forgetSessions, quiesceDocuments } from "./api";
import { resetFakeBroker } from "./fake-broker-helpers";
import { sql } from "./psql";

const tables = [
  "agent_tokens",
  "repo_projects",
  "architecture_sources",
  "delivery_settings",
  "delivery_run_jobs",
  "delivery_runs",
  "delivery_pull_requests",
  "user_issue_state",
  "user_agent_state",
  "user_agent_read",
  "user_agent_reply_read",
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
  "people",
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

/** Dates a seeded comment or message: the server stamps `created_at` itself, so two rows in one
 *  second or one millisecond with chosen fractions can only be fixtured on the row. */
export async function setCreatedAt(
  table: "comments" | "messages",
  id: string,
  iso: string
): Promise<void> {
  await sql(
    `UPDATE ${table} SET created_at = ${sqlLiteral(iso)}::timestamptz WHERE id = ${sqlLiteral(id)}::uuid`
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

// TRUNCATE locks its tables one at a time, in its own order, waiting for each while it holds the
// ones before it. A transaction that reads two of those tables in another order can close a cycle
// with it (the Inbox reads asks before comments, the sidebar issues before user_issue_state), and
// PostgreSQL breaks the cycle by aborting one side: the reset, or a server request or sweep. Such
// a transaction can start at any moment: a page an earlier scenario left open refetches whenever
// the live stream tells it to, and the server runs its own sweeps on timers. So before the
// TRUNCATE, the reset takes ACCESS EXCLUSIVE on every table the TRUNCATE empties: the tables it
// names, and every table whose foreign keys reach them, which CASCADE empties too. It never waits
// while it holds one of those locks. Each try takes them all with NOWAIT; when one is held, the
// try lets go of every lock it took, and the next try first waits for that one table while it
// holds none, so the wait cannot close a cycle. Once it holds them all, nothing else touches those
// tables until COMMIT; the TRUNCATE's other locks are on the tables' own sequences, which only an
// insert into one of those tables advances. A table still held after 5 s fails the reset, naming
// the transactions that hold it.

async function resetDatabaseOnce(): Promise<void> {
  await sql(
    "BEGIN",
    // Bounds each wait for one held table; the loop's own deadline bounds the whole step.
    "SET LOCAL lock_timeout = '1s'",
    `DO $$
      DECLARE
        targets regclass[];
        target regclass;
        contended regclass;
        holders text;
        deadline timestamptz := clock_timestamp() + interval '5 seconds';
      BEGIN
        WITH RECURSIVE truncated(rel) AS (
          SELECT unnest(ARRAY[${tables.map(sqlLiteral).join(", ")}]::regclass[])
          UNION
          SELECT c.conrelid::regclass
            FROM pg_constraint c JOIN truncated ON c.confrelid = truncated.rel
            WHERE c.contype = 'f'
        )
        SELECT array_agg(rel ORDER BY rel::text) INTO targets FROM truncated;
        LOOP
          -- A failed try rolls back this block, releasing every lock it took.
          BEGIN
            IF contended IS NOT NULL THEN
              target := contended;
              EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE', target);
            END IF;
            FOREACH target IN ARRAY targets LOOP
              EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE NOWAIT', target);
            END LOOP;
            EXIT;
          EXCEPTION WHEN lock_not_available THEN
            contended := target;
          END;
          IF clock_timestamp() >= deadline THEN
            SELECT string_agg(DISTINCT format('%s %s by %s (%s)', l.relation::regclass, l.mode,
                     left(regexp_replace(a.query, '\\s+', ' ', 'g'), 120), a.state), '; ')
              INTO holders
              FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
              WHERE l.granted AND l.pid <> pg_backend_pid() AND l.relation = ANY (targets::oid[]);
            RAISE EXCEPTION 'could not lock the tables to truncate within 5 s; held: %',
              coalesce(holders, 'by nobody now, after waiting on ' || contended);
          END IF;
        END LOOP;
      END
    $$`,
    `TRUNCATE TABLE ${tables.join(", ")} RESTART IDENTITY CASCADE`,
    // The people the scenarios act as, as if each had signed in: the only assignable names.
    "INSERT INTO people (email) VALUES ('alice'), ('bob')",
    "COMMIT"
  );
}

/**
 * Truncates every table, after the server has closed every live document and finished the
 * settlements in flight, so the documents the previous scenario had open finish writing before
 * the truncate rather than failing against the emptied tables after it. The truncate cannot
 * deadlock with whatever else still reaches the database (the comment above resetDatabaseOnce).
 * It also clears the fake Envoy, whose
 * process-local subscriptions outlive a database truncate and otherwise match recycled issue keys,
 * and the fake broker, whose seeded credential requests would otherwise reach every later row's
 * Inbox; those resets wait on nothing in the database, so they run beside the quiesce and truncate,
 * which keep their order. All settle before this returns, a failed one included, so a reset that
 * fails never leaves another running into the next test's.
 * The truncate takes `user_sessions` with it, so a session cookie minted before it is refused
 * until its login signs in again: e2e/api.ts forgets its cached ones here, and a browser context
 * signs in after the reset (e2e/users.ts).
 */
export async function resetDatabase(): Promise<void> {
  const resets = await Promise.allSettled([
    quiesceDocuments().then(resetDatabaseOnce),
    resetFakeEnvoy(),
    resetFakeBroker(),
  ]);
  for (const reset of resets) if (reset.status === "rejected") throw reset.reason;
  forgetSessions();
}
