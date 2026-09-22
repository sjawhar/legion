import { execFile } from "node:child_process";
import { promisify } from "node:util";

const tables = [
  "agent_tokens",
  "repo_projects",
  "architecture_sources",
  "user_issue_state",
  "user_agent_state",
  "events",
  "refs",
  "messages",
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

const defaultDatabaseUrl =
  "postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable";
const execFileAsync = promisify(execFile);

function databaseUrl(): string {
  return (
    globalThis.process.env.PLAYWRIGHT_DATABASE_URL ??
    globalThis.process.env.DATABASE_URL ??
    defaultDatabaseUrl
  );
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

const resetAttempts = 3;

function hasDeadlockSqlState(error: unknown): boolean {
  if (
    typeof error !== "object" ||
    error === null ||
    !("stderr" in error) ||
    typeof error.stderr !== "string"
  ) {
    return false;
  }
  return /^ERROR:\s+40P01:/m.test(error.stderr);
}

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
 * Truncates every table. The initial wait reduces contention from the prior scenario's document
 * settlement, but a server transaction can begin after that check and before TRUNCATE acquires
 * its locks. PostgreSQL resolves that structural race with SQLSTATE 40P01 by aborting one
 * participant, so retry only that error a bounded number of times.
 */
export async function resetDatabase(): Promise<void> {
  for (let attempt = 1; attempt <= resetAttempts; attempt++) {
    try {
      await resetDatabaseOnce();
      return;
    } catch (error) {
      if (!hasDeadlockSqlState(error)) {
        throw error;
      }
      const retrying = attempt < resetAttempts;
      console.warn(
        `dispatch e2e database reset deadlock (SQLSTATE 40P01) on attempt ${attempt}/${resetAttempts}${retrying ? "; retrying" : "; retry limit reached"}`
      );
      if (!retrying) {
        throw error;
      }
    }
  }
}
