import { type APIResponse, expect, type Page, type Route, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  patchIssue,
  putArchitectureSource,
  snoozeAsk,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// INFRA-492: what an open page shows must still follow the writes it shows, once an event
// refreshes only what it can change. Each test writes as another party through the API while
// alice's page stays open, and expects the change on the page within 2 s, with no reload.
const bob = { actor: { id: "bob", kind: "session" as const }, as: "agent" as const };
const eventStreamUrl = /\/api\/v1\/events(\?|$)/;
const live = { timeout: 2_000 };

test.beforeEach(async () => {
  await resetDatabase();
});

/** Opens `path` and returns once the page's event stream answered: the server subscribes the
 *  stream before it answers, so every write after this reaches the page. */
async function openLive(page: Page, path: string): Promise<void> {
  const stream = page.waitForResponse(
    (response) => eventStreamUrl.test(response.url()) && response.status() === 200
  );
  await page.goto(path);
  await stream;
}

/** The next GET matching `pattern` is fetched from the server at once, so its body is composed
 *  now, and handed back only when the test calls `release`. */
interface HeldRead {
  readonly arrived: Promise<void>;
  release(): Promise<void>;
}

async function holdNextRead(page: Page, pattern: RegExp): Promise<HeldRead> {
  let armed = true;
  const arrived = Promise.withResolvers<{ route: Route; response: APIResponse }>();
  await page.route(pattern, async (route) => {
    if (!armed || route.request().method() !== "GET") {
      await route.continue();
      return;
    }
    armed = false;
    arrived.resolve({ response: await route.fetch(), route });
  });
  return {
    arrived: arrived.promise.then(() => undefined),
    release: async () => {
      const { response, route } = await arrived.promise;
      await route.fulfill({ response });
    },
  };
}

test("document panel: a new comment citing a project document shows in its open Referenced by panel within 2 s", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Citing issue" });
  const doc = await createProjectDocument("CORE", { content: "# Runbook\n", name: "Runbook" });
  const reference = `dispatch://CORE/artifact/${doc.artifact.slug}`;
  await createComment(issue.key, { body: `First cite of ${reference}.` }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, `/projects/CORE/documents/${doc.artifact.slug}`);
    const panel = page.getByRole("region", { name: "Referenced by" });
    await expect(panel).toContainText(`First cite of ${reference}.`);

    await createComment(issue.key, { body: `Second cite of ${reference}.` }, bob);
    await expect(panel).toContainText(`Second cite of ${reference}.`, live);
  } finally {
    await alice.close();
  }
});

test("reference: a title written while the reference's read is in flight shows after leaving the page and coming back", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Target v1" });
  const holder = await createIssue({ project: "CORE", title: "Holder" });
  await createMessage(holder.key, { body: `See dispatch://${target.key}.` }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, `/issues/${holder.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByRole("link", { exact: true, name: "Target v1" })).toBeVisible();
    await page.waitForTimeout(1_200);

    // The refresh v2 owes is composed at once and held for a moment, as a busy server holds it;
    // v3 lands while it is in flight, so the page owes one more fetch once it settles. The page
    // is left as soon as the held read settles, inside the second the scheduler waits.
    const held = await holdNextRead(page, new RegExp(`/api/v1/issues/${target.key}/summary$`));
    await patchIssue(target.key, { title: "Target v2" }, bob);
    await held.arrived;
    await patchIssue(target.key, { title: "Target v3" }, bob);
    await page.waitForTimeout(150);
    await held.release();
    const navigation = page.getByRole("navigation", { name: "Navigation" });
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    await navigation.getByRole("link", { name: /^Inbox/ }).click();
    await expect(page).toHaveURL(/\/(\?.*)?$/);
    await page.waitForTimeout(1_500);
    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/issues/${holder.key}/conversation`));
    await expect(turns.getByRole("link", { exact: true, name: "Target v3" })).toBeVisible(live);
  } finally {
    await alice.close();
  }
});

test("inbox: an issue closed while the Inbox read is in flight drops its ask from the Inbox", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const other = await createIssue({ project: "CORE", title: "Quarterly budget review" });
  await createAsk(other.key, { question: "An ask that stays" }, bob);
  const closing = await createIssue({ project: "CORE", title: "Flaky deploy step" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, "/?assignee=everyone");
    await expect(page.getByText("An ask that stays")).toBeVisible();
    // The sidebar's project list, whose refresh is the close's barrier below, is mounted on a
    // phone only while its drawer is open.
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
      await expect(
        page.getByRole("navigation", { name: "Navigation" }).locator('a[href="/projects/CORE"]')
      ).toBeVisible();
    }

    // The Inbox read the new ask owes is composed with the ask and held; the close lands while it
    // is in flight.
    const held = await holdNextRead(page, /\/api\/v1\/inbox(\?|$)/);
    const ask = await createAsk(closing.key, { question: "An ask whose issue closes" }, bob);
    await held.arrived;
    const projectsRead = page.waitForRequest(
      (request) => new URL(request.url()).pathname === "/api/v1/projects"
    );
    await patchIssue(closing.key, { status: "done" }, bob);
    // A close refreshes the sidebar's project list: the page has handled the close's event.
    await projectsRead;
    await page.waitForTimeout(300);
    await held.release();

    await expect(page.locator(`[data-inbox-row="${ask.id}"]`)).toHaveCount(0, live);
    await expect(page.getByText("An ask whose issue closes")).toHaveCount(0, live);
  } finally {
    await alice.close();
  }
});

test("architecture: an attached issue's new title shows in its component's open work list within 2 s", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: { "web.md": "---\ntitle: Web\n---\nThe SPA.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  const source = await syncArchitectureSource("CORE", "legion/arch");
  expect(source.last_error).toBeNull();
  const issue = await createIssue({ project: "CORE", title: "Web login" });
  await patchIssue(issue.key, { components: { ids: ["web"], mode: "explicit" } });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, "/projects/CORE/architecture?component=web");
    const work = page.getByTestId(`work-${issue.key}`);
    await expect(work).toContainText("Web login");

    await patchIssue(issue.key, { title: "Web login, renamed" }, bob);
    await expect(work).toContainText("Web login, renamed", live);
  } finally {
    await alice.close();
  }
});

test("inbox: a snooze from alice's other tab moves the row to Later in this tab within 2 s", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Snoozed elsewhere" });
  const ask = await createAsk(issue.key, { question: "Snooze me from the other tab" }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, "/?assignee=everyone");
    await expect(page.locator(`[data-inbox-row="${ask.id}"]`)).toBeVisible();
    await expect(page.getByRole("button", { name: /^Later \(/ })).toHaveCount(0);

    // The other tab: alice's own write through the API, which this page did not make.
    await snoozeAsk(ask.id, new Date(Date.now() + 3_600_000).toISOString(), { login: "alice" });
    await expect(page.getByRole("button", { name: "Later (1)" })).toBeVisible(live);
    await expect(page.locator(`[data-inbox-row="${ask.id}"]`)).toBeHidden(live);
  } finally {
    await alice.close();
  }
});

test("replies: an issue's new title shows on the open Replies page within 2 s", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Release train" });
  const root = await createComment(issue.key, { body: "Please fix the flaky test." });
  await createComment(issue.key, { body: "Done: the build is green.", reply_to: root.id }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, "/replies");
    const row = page.locator(`[data-replies-row="${root.id}"]`);
    await expect(row).toContainText("Release train");

    await patchIssue(issue.key, { title: "Release train, renamed" }, bob);
    await expect(row).toContainText("Release train, renamed", live);
  } finally {
    await alice.close();
  }
});

test("children: a child's priority write moves its active time in the open parent's Children tab within 2 s", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const parent = await createIssue({ project: "CORE", title: "Parent" });
  const child = await createIssue({ parent: parent.key, project: "CORE", title: "Child" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, `/issues/${parent.key}/children`);
    const active = page
      .getByRole("tabpanel")
      .locator("li")
      .filter({ hasText: `${child.key} · ${child.title}` })
      .locator("time[datetime]");
    await expect(active).toHaveCount(1);
    const before = await active.getAttribute("datetime");
    expect(before).not.toBeNull();

    await page.waitForTimeout(1_100);
    await patchIssue(child.key, { priority: 1 }, bob);
    await expect(active).not.toHaveAttribute("datetime", before ?? "", live);
  } finally {
    await alice.close();
  }
});
