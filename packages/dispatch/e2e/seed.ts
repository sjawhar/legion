const tables = [
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

export async function resetDatabase(): Promise<void> {
  const databaseUrl =
    globalThis.process.env.PLAYWRIGHT_DATABASE_URL ??
    globalThis.process.env.DATABASE_URL ??
    defaultDatabaseUrl;

  const command = Bun.spawn([
    "psql",
    databaseUrl,
    "-v",
    "ON_ERROR_STOP=1",
    "-c",
    `TRUNCATE TABLE ${tables.join(", ")} RESTART IDENTITY CASCADE`,
  ]);
  const exitCode = await command.exited;

  if (exitCode !== 0) {
    throw new Error(`Database reset failed with exit code ${exitCode}.`);
  }
}
