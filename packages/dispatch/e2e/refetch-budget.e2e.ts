import { writeFile } from "node:fs/promises";

import { expect, type Page, type Request, test } from "@playwright/test";

import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  patchIssue,
  putIssueState,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// INFRA-492: how many reads one open tab sends while other sessions write. Every helper this file
// uses exists at the commit before the change (16e4bb0f758e) and no `/api/v1/events/_test/*` hook
// is touched, so the identical file runs at both commits: the budget and sidebar tests fail there
// on their counts and pass at the change. The writer is an autonomous session acting through the
// bearer API, as in `live.e2e.ts`; only alice's browser is real.
const bob = { actor: { id: "bob", kind: "session" as const }, as: "agent" as const };
const eventStreamUrl = /\/api\/v1\/events(\?|$)/;

test.beforeEach(async () => {
  await resetDatabase();
});

/** What one page sends: its GETs under `/api/` (the event stream not counted) since the last
 *  `reset`, every read of one issue's detail since the page opened, and whether any request other
 *  than the stream is still in flight. */
interface Traffic {
  /** Path and query of each GET under `/api/` since the last `reset`, the stream excepted. */
  readonly gets: string[];
  /** How many times `GET /api/v1/issues/<key>` (the full issue, not its summary) was sent. */
  issueReads(key: string): number;
  /** Whether no request but the stream has been in flight for `ms`. */
  quietFor(ms: number): boolean;
  reset(): void;
}

function watchTraffic(page: Page): Traffic {
  const gets: string[] = [];
  const issueReads = new Map<string, number>();
  const inFlight = new Set<Request>();
  let lastSettledAt = Date.now();
  page.on("request", (request) => {
    if (eventStreamUrl.test(request.url())) return;
    inFlight.add(request);
    const url = new URL(request.url());
    if (request.method() !== "GET" || !url.pathname.startsWith("/api/")) return;
    gets.push(`${url.pathname}${url.search}`);
    const issue = /^\/api\/v1\/issues\/([^/]+)$/.exec(url.pathname)?.[1];
    if (issue !== undefined) {
      const key = decodeURIComponent(issue);
      issueReads.set(key, (issueReads.get(key) ?? 0) + 1);
    }
  });
  const settle = (request: Request) => {
    if (inFlight.delete(request)) lastSettledAt = Date.now();
  };
  page.on("requestfinished", settle);
  page.on("requestfailed", settle);
  return {
    gets,
    issueReads: (key) => issueReads.get(key) ?? 0,
    quietFor: (ms) => inFlight.size === 0 && Date.now() - lastSettledAt >= ms,
    reset: () => {
      gets.length = 0;
    },
  };
}

/** Waits until no request but the event stream has been in flight for 500 ms. */
async function settled(traffic: Traffic): Promise<void> {
  await expect.poll(() => traffic.quietFor(500), { intervals: [100], timeout: 30_000 }).toBe(true);
}

/** Sleeps until `at` (a `Date.now()` time), or not at all once it has passed. */
async function until(at: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, Math.max(0, at - Date.now()));
  await promise;
}

/**
 * The event barrier, which needs nothing but the page and the API at either commit. Once the
 * header shows the open issue's current `title` (its first load landed), bob writes a new title,
 * again every 2 s, until the page has read `GET /api/v1/issues/<key>` once more than its loads so
 * far and the header shows the new title. Both can only happen after the page's event stream
 * delivered the write and the page handled it, so past this the stream is subscribed and every
 * event written before the barrier's has been handled. Returns the title it left.
 */
async function eventBarrier(
  page: Page,
  traffic: Traffic,
  key: string,
  title: string
): Promise<string> {
  const header = page.getByTestId("issue-header").getByRole("heading", { level: 1 });
  await expect(header).toHaveText(title);
  const loads = traffic.issueReads(key);
  expect(loads, `the page read ${key} before the barrier`).toBeGreaterThan(0);
  for (let attempt = 1; attempt <= 10; attempt += 1) {
    const written = `${title} (barrier ${attempt})`;
    await patchIssue(key, { title: written }, bob);
    const deadline = Date.now() + 2_000;
    while (Date.now() < deadline) {
      if (traffic.issueReads(key) > loads && (await header.textContent())?.trim() === written) {
        return written;
      }
      await page.waitForTimeout(100);
    }
  }
  throw new Error(`no event reached the page for ${key} in 10 writes 2 s apart`);
}

async function openNavigation(page: Page, isPhone: boolean) {
  if (isPhone) {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  return page.getByRole("navigation", { name: "Navigation" });
}

/** The GETs of one window grouped by path, most sent first, for an assertion's message. */
function byPath(gets: readonly string[]): string {
  const counts = new Map<string, number>();
  for (const get of gets) {
    const path = get.split("?")[0] ?? get;
    counts.set(path, (counts.get(path) ?? 0) + 1);
  }
  return [...counts]
    .sort((left, right) => right[1] - left[1])
    .slice(0, 12)
    .map(([path, count]) => `${count} × ${path}`)
    .join("\n");
}

async function createIssues(
  count: number,
  input: (index: number) => { project: string; title: string }
) {
  const issues = [];
  for (let start = 0; start < count; start += 10) {
    issues.push(
      ...(await Promise.all(
        Array.from({ length: Math.min(10, count - start) }, (_value, offset) =>
          createIssue(input(start + offset))
        )
      ))
    );
  }
  return issues;
}

test("budget: 20 title writes to issues the page does not show send at most 5 reads, and a change the page shows lands within 2 s", async ({
  browser,
}, testInfo) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OTHER", name: "Other" });
  const pad = (index: number) => String(index + 1).padStart(2, "0");
  const targets = await createIssues(50, (index) => ({
    project: "CORE",
    title: `Budget reference ${pad(index)}`,
  }));
  const unshown = await createIssues(20, (index) => ({
    project: index % 2 === 0 ? "CORE" : "OTHER",
    title: `Unshown issue ${pad(index)}`,
  }));
  const links = targets.map((target) => `dispatch://${target.key}`).join("\n");
  const source = await createIssue({ project: "CORE", spec: links, title: "Budget source" });
  const child = await createIssue({ parent: source.key, project: "CORE", title: "Budget child" });
  await patchIssue(child.key, { status: "todo" });
  // The live spec editor renders a link with its author's text and reads nothing, so the 50
  // references that each read their target are this message's, on the Conversation tab.
  await createMessage(source.key, { body: links }, bob);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchTraffic(page);
    await page.goto(`/issues/${source.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByRole("link", { name: /^Budget reference \d\d$/ })).toHaveCount(50, {
      timeout: 60_000,
    });
    await eventBarrier(page, traffic, source.key, source.title);
    await settled(traffic);
    traffic.reset();

    const written = new Set<string>();
    const startedAt = Date.now();
    for (const [index, issue] of unshown.entries()) {
      await until(startedAt + index * 250);
      await patchIssue(issue.key, { title: `${issue.title} renamed` }, bob);
      written.add(issue.key);
    }
    await until(Date.now() + 2_000);
    const window = [...traffic.gets];
    await writeFile(testInfo.outputPath("budget-window-gets.txt"), `${window.join("\n")}\n`);
    const untouched = window.filter((get) => {
      const key = /^\/api\/v1\/issues\/([^/?]+)(\/summary)?(\?|$)/.exec(get)?.[1];
      return key !== undefined && !written.has(decodeURIComponent(key));
    });
    console.log(
      `REFETCH-BUDGET [${testInfo.project.name}] GETs under /api/ in the window: ${window.length}; reads of an issue no write touched: ${untouched.length}`
    );
    expect(window.length, `GETs in the window, by path:\n${byPath(window)}`).toBeLessThanOrEqual(5);
    expect(untouched, "reads of an issue no write touched").toEqual([]);

    // A change the page shows still lands: a referenced issue's new title in its reference ...
    const renamed = `${targets[0]?.title ?? ""} renamed`;
    await patchIssue(targets[0]?.key ?? "", { title: renamed }, bob);
    await expect(turns.getByRole("link", { exact: true, name: renamed })).toBeVisible({
      timeout: 2_000,
    });

    // ... and a child's new status in the open parent's Children tab, which reads the parent's
    // own detail: only the child's event can refresh it once the tab shows the old status.
    await page.getByRole("tab", { name: "Children" }).click();
    const childRow = page
      .getByRole("tabpanel")
      .locator("li")
      .filter({ hasText: `${child.key} · ${child.title}` });
    await expect(childRow).toContainText("Todo");
    await patchIssue(child.key, { status: "in_progress" }, bob);
    await expect(childRow).toContainText("In progress", { timeout: 2_000 });
  } finally {
    await alice.close();
  }
});

test("sidebar: an issue event while the sidebar's first reads are in flight discards none of them", async ({
  browser,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OTHER", name: "Other" });
  const subject = await createIssue({ project: "OTHER", title: "Sidebar subject" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchTraffic(page);
    const reads = {
      inbox: (url: URL) => url.pathname === "/api/v1/inbox",
      pinned: (url: URL) =>
        url.pathname === "/api/v1/issues" && url.searchParams.get("pinned") === "true",
      projects: (url: URL) => url.pathname === "/api/v1/projects",
    };
    type Read = keyof typeof reads;
    const sent: Record<Read, number> = { inbox: 0, pinned: 0, projects: 0 };
    page.on("request", (request) => {
      if (request.method() !== "GET") return;
      const url = new URL(request.url());
      for (const name of Object.keys(reads) as Read[]) {
        if (reads[name](url)) sent[name] += 1;
      }
    });
    // The first request of each is answered by the server now and held on the wire, so the body
    // that lands is older than the event below; any later one goes straight through.
    const release = Promise.withResolvers<void>();
    const arrived = (Object.keys(reads) as Read[]).map((name) => {
      const reached = Promise.withResolvers<void>();
      let routed = 0;
      return {
        reached: reached.promise,
        route: page.route(reads[name], async (route) => {
          routed += 1;
          if (route.request().method() !== "GET" || routed > 1) {
            await route.fallback();
            return;
          }
          const response = await route.fetch();
          reached.resolve();
          await release.promise;
          // The commit before the change dropped this request once the event arrived; its
          // answer then has nowhere to land.
          await route.fulfill({ response }).catch(() => undefined);
        }),
      };
    });
    await Promise.all(arrived.map(({ route }) => route));

    await page.goto(`/issues/${subject.key}`);
    const navigation = await openNavigation(page, isPhone);
    await Promise.all(arrived.map(({ reached }) => reached));
    // A title change moves none of the three: the issue is in no pinned row, no Inbox row and
    // holds no open ask.
    await eventBarrier(page, traffic, subject.key, subject.title);
    release.resolve();
    const releasedAt = Date.now();

    await expect(navigation.getByRole("link", { name: /^Inbox/ })).toBeVisible();
    await expect(navigation.locator('a[href="/projects/OTHER"]')).toBeVisible();
    await expect(navigation.locator('a[href="/projects/CORE"]')).toBeVisible();
    await until(releasedAt + 2_000);
    console.log(
      `REFETCH-SIDEBAR [${testInfo.project.name}] requests sent: inbox ${sent.inbox}, pinned ${sent.pinned}, projects ${sent.projects}`
    );
    expect(sent).toEqual({ inbox: 1, pinned: 1, projects: 1 });
  } finally {
    await alice.close();
  }
});

test("pin: a pin from another tab shows in this tab's sidebar within 2 s with one pinned-list read", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Pinned elsewhere" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchTraffic(page);
    let pinnedReads = 0;
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (
        request.method() === "GET" &&
        url.pathname === "/api/v1/issues" &&
        url.searchParams.get("pinned") === "true"
      ) {
        pinnedReads += 1;
      }
    });
    await page.goto(`/issues/${issue.key}`);
    const navigation = await openNavigation(page, testInfo.project.name === "iphone");
    await expect(navigation.locator('a[href="/projects/CORE"]')).toBeVisible();
    const title = await eventBarrier(page, traffic, issue.key, issue.title);
    await settled(traffic);
    pinnedReads = 0;

    // The other tab: alice's own write through the API, which this page did not make.
    await putIssueState(issue.key, { pinned: true });
    const pinnedAt = Date.now();
    await expect(
      navigation.getByRole("link", { name: issue.key }).filter({ hasText: title })
    ).toBeVisible({ timeout: 2_000 });
    await until(pinnedAt + 2_000);
    console.log(`REFETCH-PIN [${testInfo.project.name}] pinned-list reads: ${pinnedReads}`);
    expect(pinnedReads).toBe(1);
  } finally {
    await alice.close();
  }
});

test("replies: a reply in a thread alice wrote in moves her Replies count, and one in a thread she never wrote in reads nothing", async ({
  browser,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  const strangers = await createIssue({ project: "CORE", title: "Somebody else's thread" });
  const own = await createIssue({ project: "CORE", title: "Alice's thread" });
  const strangerRoot = await createComment(strangers.key, { body: "Bob asks." }, { login: "bob" });
  const ownRoot = await createComment(own.key, { body: "Alice asks." });
  const session = { actor: { id: "e2e-replier", kind: "session" as const }, as: "agent" as const };

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchTraffic(page);
    let repliesReads = 0;
    page.on("request", (request) => {
      if (request.method() === "GET" && new URL(request.url()).pathname === "/api/v1/me/replies") {
        repliesReads += 1;
      }
    });
    // From 1280 px the sidebar's Replies row carries the count; below, the compact header's
    // `Replies N` link, absent at 0.
    const repliesCount = isPhone
      ? page.getByRole("link", { name: /^Replies \d+$/ })
      : page
          .getByRole("navigation", { name: "Navigation" })
          .getByRole("link", { name: /^Replies/ });

    await page.goto(`/issues/${strangers.key}/conversation`);
    const title = await eventBarrier(page, traffic, strangers.key, strangers.title);
    if (isPhone) {
      await expect(repliesCount).toHaveCount(0);
    } else {
      await expect(repliesCount).toHaveText("Replies");
    }
    await settled(traffic);
    repliesReads = 0;

    await createComment(
      strangers.key,
      { body: "A reply alice is not party to.", reply_to: strangerRoot.id },
      session
    );
    // Events reach the page in order: once the barrier's write shows, the reply's was handled.
    await eventBarrier(page, traffic, strangers.key, title);
    await page.waitForTimeout(1_200);
    console.log(
      `REFETCH-REPLIES [${testInfo.project.name}] Replies reads after a reply in a stranger's thread: ${repliesReads}`
    );
    expect(repliesReads, "Replies reads after a reply in a thread alice never wrote in").toBe(0);

    await createComment(own.key, { body: "A reply to alice.", reply_to: ownRoot.id }, session);
    if (isPhone) {
      await expect(repliesCount).toHaveText("Replies 1", { timeout: 2_000 });
    } else {
      await expect(repliesCount).toHaveText(/^Replies\s*1$/, { timeout: 2_000 });
    }
    expect(repliesReads).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("project board: 20 title writes 250 ms apart send at most 7 list reads and the last title shows within 2 s", async ({
  browser,
}, testInfo) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issues = await createIssues(20, (index) => ({
    project: "CORE",
    title: `Board card ${String(index + 1).padStart(2, "0")}`,
  }));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchTraffic(page);
    let listReads = 0;
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (
        request.method() === "GET" &&
        url.pathname === "/api/v1/issues" &&
        url.searchParams.get("project") === "CORE"
      ) {
        listReads += 1;
      }
    });
    await page.goto("/projects/CORE");
    const first = issues[0];
    const last = issues[issues.length - 1];
    if (first === undefined || last === undefined) throw new Error("no issues seeded");
    await expect(page.getByLabel(`${last.key} ${last.title}`, { exact: true })).toBeVisible();
    // The board's barrier: a card shows a title only an event can have brought.
    let barrier = 0;
    await expect
      .poll(
        async () => {
          barrier += 1;
          const title = `${first.title} (barrier ${barrier})`;
          await patchIssue(first.key, { title }, bob);
          try {
            await expect(page.getByLabel(`${first.key} ${title}`, { exact: true })).toBeVisible({
              timeout: 2_000,
            });
            return true;
          } catch {
            return false;
          }
        },
        { intervals: [0], timeout: 30_000 }
      )
      .toBe(true);
    await settled(traffic);
    listReads = 0;

    const startedAt = Date.now();
    for (const [index, issue] of issues.entries()) {
      await until(startedAt + index * 250);
      await patchIssue(issue.key, { title: `${issue.title} renamed` }, bob);
    }
    const lastWriteAt = Date.now();
    await expect(page.getByLabel(`${last.key} ${last.title} renamed`, { exact: true })).toBeVisible(
      { timeout: 2_000 }
    );
    await until(lastWriteAt + 2_000);
    console.log(`REFETCH-BOARD [${testInfo.project.name}] project list reads: ${listReads}`);
    expect(listReads).toBeLessThanOrEqual(7);
  } finally {
    await alice.close();
  }
});
