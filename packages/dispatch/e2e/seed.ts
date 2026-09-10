import { execFile } from "node:child_process";
import { promisify } from "node:util";

const tables = [
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

export async function resetDatabase(): Promise<void> {
  await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-c",
    `TRUNCATE TABLE ${tables.join(", ")} RESTART IDENTITY CASCADE`,
  ]);
}
