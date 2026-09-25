// The failed-room probe: a document's live room fails while a writer is already queued behind
// it, and that writer must be told (503 DOC_SERVICE_UNAVAILABLE) instead of waiting for a
// recovery that cannot finish while the failing request holds what the recovery needs. The
// second half is the recovery: the room reloads, the next write lands, and the browser's own
// paragraph is still in the document the server serves.
//
// It is opt-in, because it takes about 40 s of real time (a trigger holds a durable append for
// 20 s) and it cancels a Postgres backend, so it is not part of the suite CI runs:
//
//   DISPATCH_FAILED_ROOM_PROBE=1 bunx playwright test --config e2e/playwright.config.ts \
//     --project=chromium e2e/failed-room.e2e.ts
//
// Comparing a tree against another is what it is for: run it on this head and on the base, and
// read the two lines it prints for the queued writer.
import { execFile } from "node:child_process";
import { promisify } from "node:util";

import { expect, test } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const execFileAsync = promisify(execFile);
const baseUrl =
  process.env.PLAYWRIGHT_BASE_URL || `http://127.0.0.1:${process.env.DISPATCH_E2E_PORT || "8777"}`;

/** The database this probe may write to, resolved as `seed.ts` resolves it: a deployed server's
 * own `PLAYWRIGHT_DATABASE_URL`, else the `DATABASE_URL` this run supplied. It never falls back
 * to libpq's default, because this probe installs a `doc_updates` trigger and cancels a
 * backend. */
function databaseUrl(): string {
  const deployed = process.env.PLAYWRIGHT_BASE_URL
    ? process.env.PLAYWRIGHT_DATABASE_URL
    : undefined;
  const url = deployed ?? process.env.DATABASE_URL;
  if (url === undefined || url.trim() === "") {
    throw new Error(
      "PLAYWRIGHT_DATABASE_URL or DATABASE_URL must name the database for the failed-room probe"
    );
  }
  return url;
}

async function sql(statement: string): Promise<string> {
  const { stdout } = await execFileAsync("psql", [
    databaseUrl(),
    "-v",
    "ON_ERROR_STOP=1",
    "-tAc",
    statement,
  ]);
  return stdout.trim();
}

/** Holds the room's next durable append inside its `doc_updates` insert for this long, which is
 * how the probe gets a writer it can cancel while it owns the room. Every `doc_updates` insert
 * on this database sleeps while the trigger is installed, so `releaseTheRoom` runs in a
 * `finally` and again in `test.afterEach`. */
async function holdTheRoomFor(seconds: number): Promise<void> {
  await sql(`
    create or replace function failed_room_hold() returns trigger language plpgsql as $$
    begin
      perform pg_sleep(${seconds});
      return new;
    end $$;
  `);
  await sql(`
    drop trigger if exists failed_room_hold on doc_updates;
    create trigger failed_room_hold before insert on doc_updates
      for each row execute function failed_room_hold();
  `);
}

async function releaseTheRoom(): Promise<void> {
  await sql("drop trigger if exists failed_room_hold on doc_updates");
  await sql("drop function if exists failed_room_hold()");
}

// A trigger left behind makes every later `doc_updates` insert on this database sleep, which
// fails whatever writes a document next - `resetDatabase`'s wait for open transactions
// included. This runs after a timeout, a Ctrl-C between tests, and a throw inside the body,
// which the `finally` below covers on its own.
test.afterEach(async () => {
  if (process.env.DISPATCH_FAILED_ROOM_PROBE === "1") {
    await releaseTheRoom();
  }
});

/** The backend the trigger is holding inside its `doc_updates` insert: the room's own durable
 * writer, which owns the room's advisory lock while it sleeps, so cancelling it fails the room
 * itself rather than one request's transaction. */
async function heldAppendPid(): Promise<string> {
  for (let attempt = 0; attempt < 250; attempt += 1) {
    const pid = await sql(`
      select pid from pg_stat_activity
      where datname = current_database()
        and wait_event = 'PgSleep'
        and query ilike '%doc_updates%'
      order by query_start limit 1
    `);
    if (pid !== "") {
      return pid;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error("no append was held in its insert");
}

interface Attempt {
  readonly status: number;
  readonly code: string;
  readonly seconds: number;
}

async function anchoredComment(issueKey: string, body: string, quote: string): Promise<Attempt> {
  const started = Date.now();
  const response = await fetch(`${baseUrl}/api/v1/issues/${issueKey}/comments`, {
    body: JSON.stringify({ anchor: { artifact: "spec", quote }, body }),
    headers: { "Content-Type": "application/json", "X-Dispatch-User": "alice" },
    method: "POST",
    signal: AbortSignal.timeout(30_000),
  });
  const text = await response.text();
  let code = "";
  try {
    code = (JSON.parse(text) as { code?: string }).code ?? "";
  } catch {
    code = text.slice(0, 40);
  }
  return { code, seconds: (Date.now() - started) / 1000, status: response.status };
}

test("a writer inside the docs layer when its room fails is told, and the room recovers", async ({
  browser,
}, testInfo) => {
  test.skip(
    process.env.DISPATCH_FAILED_ROOM_PROBE !== "1",
    "the failed-room probe is opt-in: it holds a durable append for 20 s and cancels a backend"
  );
  test.skip(testInfo.project.name === "iphone", "one browser is enough for a server behaviour");
  test.setTimeout(240_000);
  await resetDatabase();
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Plan\n\nShip the migration.",
    title: "Failed room probe",
  });
  await createComment(issue.key, { body: "seed" });

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.goto(`/issues/${issue.key}/spec`);
  const editor = documentEditor(page);
  await expect(editor).toBeVisible();
  await expect(editor).toContainText("Ship the migration.");

  // The browser's own edit, so the room holds a live paragraph the recovery has to bring back -
  // and so the room's durable writer has something to append.
  await editor.click();
  await page.keyboard.press("End");
  await page.keyboard.type(" The browser typed this.");
  await expect(editor).toContainText("The browser typed this.");

  // The room's own durable writer is what has to fail, and it is the browser's append: ygo
  // persists the keystrokes about twelve seconds later, on the connection that holds the room's
  // advisory lock. The trigger goes in before that flush, so the append sleeps inside its insert
  // where a cancel can reach it; an anchored comment then queues behind the room, a second one
  // behind that, and only then is the sleeping append cancelled. The room fails with both
  // writers already inside the docs layer, waiting for what its recovery needs.
  let holding: Promise<Attempt>;
  let queued: Promise<Attempt>;
  await holdTheRoomFor(20);
  try {
    const pid = await heldAppendPid();
    holding = anchoredComment(issue.key, "the writer that arrives after the failure", "migration");
    await new Promise((resolve) => setTimeout(resolve, 1000));
    queued = anchoredComment(issue.key, "the writer behind it", "migration");
    await new Promise((resolve) => setTimeout(resolve, 1000));
    const cancelled = await sql(`select pg_cancel_backend(${pid})`);
    console.log(`FAILED-ROOM cancelled the room's durable append pid=${pid} result=${cancelled}`);
  } finally {
    // Whatever happened above, no later `doc_updates` insert on this database may sleep.
    await releaseTheRoom();
  }

  const holder = await holding;
  console.log(
    `FAILED-ROOM first status=${holder.status} code=${holder.code} in ${holder.seconds}s`
  );
  const behind = await queued;
  console.log(
    `FAILED-ROOM queued status=${behind.status} code=${behind.code} in ${behind.seconds}s`
  );
  // The writer that was inside the docs layer when the room failed is told so. Before the fix it
  // waited for a recovery that could not finish while it held what that recovery needs, and its
  // client gave up at 30 s.
  expect(holder.status).toBe(503);
  expect(holder.code).toBe("DOC_SERVICE_UNAVAILABLE");
  // The one behind it has two correct outcomes: told the same way, or admitted after the room
  // has already reloaded. Anything else - a hang, a 500 - is the failure this probe is about.
  expect([201, 503]).toContain(behind.status);
  if (behind.status === 503) {
    expect(behind.code).toBe("DOC_SERVICE_UNAVAILABLE");
  }

  // The recovery half: the room reloads from its durable copy, so a later write lands and the
  // browser's edit is in the document the server serves.
  let recovered = await anchoredComment(issue.key, "after the recovery", "migration");
  for (let attempt = 0; attempt < 20 && recovered.status !== 201; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 1000));
    recovered = await anchoredComment(issue.key, `after the recovery ${attempt}`, "migration");
  }
  console.log(
    `FAILED-ROOM recovered status=${recovered.status} code=${recovered.code} in ${recovered.seconds}s`
  );
  expect(recovered.status).toBe(201);

  const artifacts = await fetch(`${baseUrl}/api/v1/issues/${issue.key}`, {
    headers: { "X-Dispatch-User": "alice" },
  }).then((response) => response.json() as Promise<{ artifacts: { id: string }[] }>);
  const text = await fetch(`${baseUrl}/api/v1/artifacts/${artifacts.artifacts[0].id}/text`, {
    headers: { "X-Dispatch-User": "alice" },
  }).then((response) => response.json() as Promise<{ markdown: string }>);
  console.log(`FAILED-ROOM document text=${JSON.stringify(text.markdown)}`);
  // The room reloaded from its durable copy with the browser's own paragraph in it, which is the
  // half of the recovery no status code shows.
  expect(text.markdown).toContain("The browser typed this.");
  await alice.close();
});
