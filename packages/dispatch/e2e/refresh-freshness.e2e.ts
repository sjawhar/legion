import { setTimeout as sleep } from "node:timers/promises";

import { type APIResponse, expect, type Page, type Route, test } from "@playwright/test";

import { MIN_REFETCH_INTERVAL_MS } from "../web/src/api/query-refresh";
import {
  createArtifactComment,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  editComment,
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
const live = { timeout: 2_000 };

test.beforeEach(async () => {
  await resetDatabase();
});

/** Opens `path` and returns once Dispatch has started the page's event feed on its live socket
 *  (`events_start`): it subscribes the feed to its event relay before it sends that frame, so every
 *  write after this reaches the page. */
async function openLive(page: Page, path: string): Promise<void> {
  const started = Promise.withResolvers<void>();
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname !== "/ws/live") return;
    socket.on("framereceived", ({ payload }) => {
      const frame = JSON.parse(String(payload)) as { readonly type?: string };
      if (frame.type === "events_start") started.resolve();
    });
  });
  await page.goto(path);
  await started.promise;
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

test("reference: a title written while the reference's read is in flight shows after leaving the page and coming back @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Target v1" });
  const holder = await createIssue({ project: "CORE", title: "Holder" });
  await createMessage(holder.key, { body: `See dispatch://${target.key}.` }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Every wait below is a page timer of the refresh scheduler, so the page's own clock fires
    // them rather than the wall clock.
    await page.clock.install();
    await openLive(page, `/issues/${holder.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByRole("link", { exact: true, name: "Target v1" })).toBeVisible();
    // Past the first read's `MIN_REFETCH_INTERVAL_MS`, so the refresh v2 owes is fetched at once.
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS + 200);

    // The refresh v2 owes is composed at once and held for a moment, as a busy server holds it;
    // v3 lands while it is in flight, so the page owes one more fetch once it settles. The page
    // is left as soon as the held read settles, inside the second the scheduler waits.
    const held = await holdNextRead(page, new RegExp(`/api/v1/issues/${target.key}/summary$`));
    await patchIssue(target.key, { title: "Target v2" }, bob);
    await held.arrived;
    await patchIssue(target.key, { title: "Target v3" }, bob);
    // Past the event feed's 100 ms batch window, so v3 is owed before the read is handed back.
    await page.clock.fastForward(150);
    await held.release();
    const navigation = page.getByRole("navigation", { name: "Navigation" });
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    await navigation.getByRole("link", { name: /^Inbox/ }).click();
    await expect(page).toHaveURL(/\/(\?.*)?$/);
    // Away from the page for longer than the second the scheduler waits.
    await page.clock.fastForward(1_500);
    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/issues/${holder.key}/conversation`));
    await expect(turns.getByRole("link", { exact: true, name: "Target v3" })).toBeVisible(live);
  } finally {
    await alice.close();
  }
});

test("inbox: an issue closed while the Inbox read is in flight drops its ask from the Inbox @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const other = await createIssue({ project: "CORE", title: "Quarterly budget review" });
  await createAsk(other.key, { question: "An ask that stays" }, bob);
  const closing = await createIssue({ project: "CORE", title: "Flaky deploy step" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The pause below is a page timer of the refresh scheduler: the page's clock fires it.
    await page.clock.install();
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
    await page.clock.fastForward(300);
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
    // The wait below is the scheduler's own second, which the page's clock spends.
    await page.clock.install();
    await openLive(page, `/issues/${parent.key}/children`);
    const active = page
      .getByRole("tabpanel")
      .locator("li")
      .filter({ hasText: `${child.key} · ${child.title}` })
      .locator("time[datetime]");
    await expect(active).toHaveCount(1);
    const before = await active.getAttribute("datetime");
    expect(before).not.toBeNull();

    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS + 100);
    await patchIssue(child.key, { priority: 1 }, bob);
    await expect(active).not.toHaveAttribute("datetime", before ?? "", live);
  } finally {
    await alice.close();
  }
});

test("document panel: an edit of a comment on a project document shows its new text in the document's open Referenced by panel within 2 s", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const doc = await createProjectDocument("CORE", { content: "# Runbook\n", name: "Runbook" });
  // Alice's own comment, edited from her other tab: only a person may edit a comment.
  const comment = await createArtifactComment(doc.artifact.id, { body: "Original remark" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openLive(page, `/projects/CORE/documents/${doc.artifact.slug}`);
    const panel = page.getByRole("region", { name: "Referenced by" });
    await expect(panel).toContainText("Original remark");

    await editComment(comment.id, { body: "Edited remark" });
    await expect(panel).toContainText("Edited remark", live);
  } finally {
    await alice.close();
  }
});

test("budget: a document in another project citing 25 documents reads no issue detail, Inbox or ask list for an open issue page", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OTHER", name: "Other" });
  const issue = await createIssue({ project: "CORE", title: "Open issue page" });
  // Twenty-five documents to cite; nothing here reads the order they were written in.
  const cited = await Promise.all(
    Array.from({ length: 25 }, (_, number) =>
      createProjectDocument("OTHER", {
        content: `# Chapter ${number + 1}\n`,
        name: `Chapter ${String(number + 1).padStart(2, "0")}`,
      })
    )
  );
  const index = cited.map((doc) => `dispatch://OTHER/artifact/${doc.artifact.slug}`).join(" ");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The batch window and the refresh deferral below are page timers: the page's clock fires
    // them rather than the wall clock.
    await page.clock.install();
    const gets: string[] = [];
    page.on("request", (request) => {
      const url = new URL(request.url());
      // The live socket is `/ws/live`, outside `/api/`, so no read below is the event feed.
      if (request.method() === "GET" && url.pathname.startsWith("/api/")) {
        gets.push(url.pathname);
      }
    });
    await openLive(page, `/issues/${issue.key}/conversation`);
    await expect(page.getByTestId("issue-header").getByRole("heading", { level: 1 })).toHaveText(
      issue.title
    );
    // The opening reads are the page's own; count from the moment they have stopped arriving.
    await expect
      .poll(
        async () => {
          const before = gets.length;
          await sleep(100);
          return gets.length - before;
        },
        { intervals: [50, 50, 100, 200, 500] }
      )
      .toBe(0);
    gets.length = 0;

    // One document in another project whose one paragraph cites 25 documents.
    await createProjectDocument("OTHER", { content: `Index: ${index}\n`, name: "Index" }, bob);
    // The citation's event is batched for 100 ms and any read it owes is deferred up to
    // `MIN_REFETCH_INTERVAL_MS`; one jump fires both, and the margin is for a request they make
    // reaching Playwright's counter.
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS + 100);
    await sleep(200);
    console.log(`REFETCH-INDEX [${testInfo.project.name}] GETs: ${gets.join(", ")}`);
    const unrelated = gets.filter(
      (path) =>
        path === `/api/v1/issues/${issue.key}` ||
        path === "/api/v1/inbox" ||
        /^\/api\/v1\/issues\/[^/]+\/(asks|comments|messages)$/.test(path)
    );
    expect(unrelated, "reads of what a citation in another project cannot change").toEqual([]);
  } finally {
    await alice.close();
  }
});

test("snooze: another person's event feed carries nothing that names the ask alice snoozed", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Private triage" });

  // Bob's open tab: every event frame its live socket carries.
  const bobContext = await asUser(browser, "bob");
  try {
    const page = await bobContext.newPage();
    const frames: string[] = [];
    page.on("websocket", (socket) => {
      if (new URL(socket.url()).pathname !== "/ws/live") return;
      socket.on("framereceived", ({ payload }) => {
        const frame = String(payload);
        if ((JSON.parse(frame) as { readonly type?: string }).type === "event") frames.push(frame);
      });
    });
    await openLive(page, "/");

    const ask = await createAsk(issue.key, { question: "Snoozed in private" }, bob);
    await snoozeAsk(ask.id, new Date(Date.now() + 3_600_000).toISOString(), { login: "alice" });
    const aliceFrames = () =>
      frames.filter(
        (frame) => frame.includes("user_state.updated") && frame.includes('"login":"alice"')
      );
    // The snooze's event reaches bob's feed, as it reaches every signed-in one, and names no ask.
    await expect.poll(() => aliceFrames().length, live).toBeGreaterThan(0);
    expect(frames.some((frame) => frame.includes(ask.id))).toBe(true);
    expect(aliceFrames().filter((frame) => frame.includes(ask.id))).toEqual([]);
  } finally {
    await bobContext.close();
  }
});
