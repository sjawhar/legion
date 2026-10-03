import { execFile } from "node:child_process";
import { promisify } from "node:util";

import { quiesceDocuments } from "./api";

const tables = [
  "agent_tokens",
  "repo_projects",
  "architecture_sources",
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
  "users",
];

const execFileAsync = promisify(execFile);

// A deployed server can name its database independently, but a leftover deployed URL must not
// override a local harness's DATABASE_URL. Every SQL mutation, especially resetDatabase's
// TRUNCATE, requires a URL the caller explicitly supplied for this run: no shared default exists.
function databaseUrl(): string {
  const deployedDatabaseUrl = globalThis.process.env.PLAYWRIGHT_BASE_URL
    ? globalThis.process.env.PLAYWRIGHT_DATABASE_URL
    : undefined;
  const url = deployedDatabaseUrl ?? globalThis.process.env.DATABASE_URL;
  if (url === undefined || url.trim() === "") {
    throw new Error(
      "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for this e2e run"
    );
  }
  return url;
}

function sqlLiteral(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

export async function insertExternalLink(issueKey: string, url: string): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-c",
    `INSERT INTO issue_external_links (issue_key, url, kind) VALUES (${sqlLiteral(issueKey)}, ${sqlLiteral(url)}, 'url')`,
  ]);
}

export async function setEventCreatedAt(eventId: number, iso: string): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-c",
    `UPDATE events SET created_at = ${sqlLiteral(iso)}::timestamptz WHERE id = ${Number(eventId)}`,
  ]);
}

/** Dates a seeded comment or message: the server stamps `created_at` itself, so two rows in one
 *  second or one millisecond with chosen fractions can only be fixtured on the row. */
export async function setCreatedAt(
  table: "comments" | "messages",
  id: string,
  iso: string
): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-c",
    `UPDATE ${table} SET created_at = ${sqlLiteral(iso)}::timestamptz WHERE id = ${sqlLiteral(id)}::uuid`,
  ]);
}

/** Stamps a verified service token's subject on a seeded comment's author. The API seeder posts
 *  its actor under the shared token and the server drops a body-supplied `service`, so the only
 *  way to fixture a service-authored write is the `comments.author` jsonb itself. */
export async function setCommentAuthorService(commentId: string, service: string): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-c",
    `UPDATE comments SET author = author || jsonb_build_object('service', ${sqlLiteral(service)}) WHERE id = ${sqlLiteral(commentId)}::uuid`,
  ]);
}

/** Marks a newly created fixture issue as pre-creator metadata. */
export async function clearIssueCreator(issueKey: string): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-c",
    `UPDATE issues SET created_by = 'null'::jsonb WHERE key = ${sqlLiteral(issueKey)}`,
  ]);
}

// The DO block waits for every other session to leave its transaction. That is a barrier, not a
// fix: the server's document settlements run on their own timers, so one can open a transaction
// after the check and before TRUNCATE takes its locks. resetDatabase closes that by quiescing
// the document service first.

async function resetDatabaseOnce(): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-v",
    "VERBOSITY=verbose",
    "-c",
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
    "-c",
    `TRUNCATE TABLE ${tables.join(", ")} RESTART IDENTITY CASCADE`,
  ]);
}

/**
 * Truncates every table, after the server has closed every live document and finished the
 * settlements in flight. A settlement locks its document's owner row and then reads
 * artifact_versions, while TRUNCATE takes an exclusive lock on every table in its own order;
 * with both running PostgreSQL breaks the cycle by aborting one of them (LEGION-168), which is
 * either a failed reset or a settlement that dies mid-scenario. Quiescing first leaves the
 * server with nothing to run, so the two never overlap.
 */
export async function resetDatabase(): Promise<void> {
  await quiesceDocuments();
  await resetDatabaseOnce();
}
