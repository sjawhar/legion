import { execFile } from "node:child_process";
import { promisify } from "node:util";

const tables = [
  "agent_tokens",
  "repo_projects",
  "user_issue_state",
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

/**
 * Truncates every table. A multi-table TRUNCATE takes its ACCESS EXCLUSIVE locks one table at a
 * time, in list order, so it deadlocks with any server transaction that already holds one of
 * the later tables and asks for an earlier one - and the previous scenario's teardown leaves
 * exactly that in flight: closing its page settles the document it edited (`artifacts`, then
 * `issues`, then `artifact_versions`, ...). Wait for every open server transaction on this
 * database to finish first; between scenarios nothing starts another one.
 */
export async function resetDatabase(): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
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
