import { expect, type Page, test } from "@playwright/test";

import { baseUrl, createIssue, createProject, sessionCookieName } from "./api";
import { holdFirstRequest } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

interface NavigationEventTrace {
  download?: unknown;
  sameDocument?: unknown;
  type: string;
  url?: unknown;
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a transient whoami failure shows a retry banner and keeps the app, not the sign-in page", async ({
  browser,
}, testInfo) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  await page.route("**/auth/whoami", (route) =>
    route.fulfill({
      body: JSON.stringify({ error: "Dispatch is unavailable" }),
      contentType: "application/json",
      status: 503,
    })
  );

  await page.goto("/");

  await expect(page.getByText("Couldn't reach Dispatch.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Sign in with Google" })).toHaveCount(0);

  await page.unroute("**/auth/whoami");
  await page.getByRole("button", { name: "Retry" }).click();

  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(page.getByText("Signed in as alice")).toBeVisible();

  await context.close();
});

test("signing out revokes the session on the server and returns to the sign-in page", async ({
  browser,
}, testInfo) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto("/");
  if (testInfo.project.name === "iphone") {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await expect(page.getByText("Signed in as alice")).toBeVisible();
  const session = (await context.cookies()).find((cookie) => cookie.name === sessionCookieName);
  if (session === undefined) {
    throw new Error(`no ${sessionCookieName} cookie after sign-in`);
  }
  const whoami = () =>
    fetch(new URL("/auth/whoami", baseUrl), {
      headers: { Cookie: `${sessionCookieName}=${session.value}` },
    });
  // The copy authenticates on its own before Sign out, so its refusal afterwards is the logout's.
  expect((await whoami()).status).toBe(200);

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("link", { name: "Sign in with Google" })).toBeVisible();

  // /auth/logout clears the browser's cookie, which alone would make the next whoami 401. The
  // check that cannot pass for that reason: a copy of the cookie the browser held, replayed after
  // Sign out, is refused because logout advanced alice's session generation on the server.
  expect((await whoami()).status).toBe(401);
  await context.close();
});

test("a page chunk that fails on every load reloads three times and then shows the failed download", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unreachable page" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // Every download of the issue page's code fails, the way a request does when the network
  // changes under it (a VPN reconnect, a Wi-Fi switch).
  await page.route(/\/assets\/IssuePage-[^/]+\.js$/u, (route) => route.abort());
  let loads = 0;
  page.on("load", () => {
    loads += 1;
  });

  try {
    await page.goto(`/issues/${issue.key}`);
    // Each of the first three failures reloads the page; the fourth, with the tab's three reloads
    // of the last five minutes spent, stays.
    await expect.poll(() => loads).toBe(4);
    const box = page.getByTestId("error-boundary");
    await expect(box).toContainText("Failed to fetch dynamically imported module");
    await expect(box).not.toContainText("Cannot read properties of undefined");
    await expect(box.getByRole("button", { name: "Reload Dispatch" })).toBeVisible();
  } finally {
    await context.close();
  }
});

// A Download link (`<a download>`) starts a navigation the browser answers with a download, so the
// page stays the reader's. The Navigation API's `navigate` event fires for it with `downloadRequest`
// set and `destination.sameDocument` false, and a chunk that fails after it must still reload the
// page, as on any page nobody is leaving.
test("a chunk that fails after a Download link was followed reloads the page", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium" && testInfo.project.name !== "firefox-download-navigation",
    "the guard is checked in Chromium and Firefox"
  );
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec: "Downloadable spec.", title: "Files" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // The spec's document transport is still downloading when the reader opens the Artifacts tab
  // and follows a Download link; the chunk fails only after that.
  const transportCode = await holdFirstRequest(page, /\/assets\/yjs-[^/]+\.js$/u);
  await page.addInitScript(() => {
    const seen: Array<{
      download?: unknown;
      sameDocument?: unknown;
      type: string;
      url?: unknown;
    }> = [];
    Object.assign(window, { seenNavigations: seen });
    const navigation = "navigation" in window ? window.navigation : undefined;
    if (navigation instanceof EventTarget) {
      navigation.addEventListener("navigate", (event) => {
        const destination = "destination" in event ? event.destination : undefined;
        seen.push({
          download: "downloadRequest" in event ? event.downloadRequest : undefined,
          sameDocument:
            typeof destination === "object" && destination !== null && "sameDocument" in destination
              ? destination.sameDocument
              : undefined,
          type: "navigate",
          url:
            typeof destination === "object" && destination !== null && "url" in destination
              ? destination.url
              : undefined,
        });
      });
    }
    window.addEventListener("pageshow", () => seen.push({ type: "pageshow" }));
    document.addEventListener("visibilitychange", () =>
      seen.push({ type: `visibilitychange:${document.visibilityState}` })
    );
  });
  let loads = 0;
  page.on("load", () => {
    loads += 1;
  });

  try {
    await page.goto(`/issues/${issue.key}/spec`);
    const held = await transportCode.held;
    await page.getByRole("tab", { name: /^Artifacts/ }).click();
    const download = page.waitForEvent("download");
    await page.getByRole("link", { name: "Download version 1" }).click();
    await download;
    // The browser announced the download as a navigation to another document: the case the
    // guard has to tell apart from a page being left.
    const events = await page.evaluate<NavigationEventTrace[] | undefined>(() => {
      const eventWindow = window as Window & { seenNavigations?: NavigationEventTrace[] };
      return eventWindow.seenNavigations;
    });
    expect(events).toBeDefined();
    const downloadNavigation = events?.findIndex(
      (event) =>
        event.type === "navigate" && event.download !== null && event.download !== undefined
    );
    expect(downloadNavigation).toBeGreaterThanOrEqual(0);
    expect(events?.[downloadNavigation ?? -1]).toMatchObject({
      download: expect.any(String),
      sameDocument: false,
      type: "navigate",
    });
    if (testInfo.project.name === "firefox-download-navigation") {
      console.log(`firefox event order: ${JSON.stringify(events)}`);
      const downloadEvent = events?.[downloadNavigation ?? -1];
      const followOnNavigation = events?.findIndex(
        (event, index) =>
          index > (downloadNavigation ?? -1) &&
          event.type === "navigate" &&
          event.download === null &&
          event.sameDocument === false &&
          event.url === downloadEvent?.url
      );
      expect(followOnNavigation).toBeGreaterThan(downloadNavigation ?? -1);
      expect(
        events
          ?.slice((downloadNavigation ?? -1) + 1, followOnNavigation)
          .some((event) => event.type === "pageshow" || event.type === "visibilitychange:visible")
      ).toBe(false);
    }
    expect(loads).toBe(1);

    await held.abort();
    await expect.poll(() => loads).toBe(2);
  } finally {
    await context.close();
  }
});

/** The path of the asset the page at `/` names in `pattern`'s first group. */
async function pageAssetPath(pattern: RegExp, what: string): Promise<string> {
  const html = await (await fetch(new URL("/", baseUrl))).text();
  const asset = pattern.exec(html)?.[1];
  if (asset === undefined) {
    throw new Error(`the page names no ${what}: ${html}`);
  }
  return new URL(asset, baseUrl).pathname;
}

/**
 * Answers the page's entry chunk 404, as a server whose deployment never built it does, whenever
 * `missing` says so for that request (numbered from 1; it may hold the request until it answers),
 * and counts the page's document loads and entry requests.
 */
async function missEntryChunk(
  page: Page,
  missing: (request: number) => boolean | Promise<boolean>
) {
  // The entry chunk: the module script Vite writes into index.html.
  const entry = await pageAssetPath(
    /<script\b[^>]*\btype="module"[^>]*\bsrc="([^"]+)"/u,
    "module script"
  );
  const counts = { documents: 0, entryRequests: 0 };
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      counts.documents += 1;
    }
  });
  await page.route(
    (url) => url.pathname === entry,
    async (route) => {
      counts.entryRequests += 1;
      if (!(await missing(counts.entryRequests))) {
        await route.continue();
        return;
      }
      await route.fulfill({
        body: JSON.stringify({ error: "not found" }),
        contentType: "application/json",
        status: 404,
      });
    }
  );
  return counts;
}

// During a rolling deploy the load balancer can send a page's HTML to one server and its entry
// chunk to another that never built it. Nothing of the app has run, so only the page itself can
// recover: it reloads, and the next load asks for both again.
test("a page whose entry chunk is missing once reloads and renders the app", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const counts = await missEntryChunk(page, (request) => request === 1);

  try {
    await page.goto("/", { waitUntil: "commit" });
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    expect(counts).toEqual({ documents: 2, entryRequests: 2 });
  } finally {
    await context.close();
  }
});

// The entry's reloads come from the tab's budget, so a page whose entry chunk never loads reloads
// three times and then stays blank rather than reloading for ever.
test("a page whose entry chunk is missing on every load reloads three times and stops", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const counts = await missEntryChunk(page, () => true);

  try {
    await page.goto("/", { waitUntil: "commit" });
    await expect.poll(() => counts.entryRequests).toBe(4);
    // Long enough for a reload the fourth miss started to have asked for the page again.
    await page.waitForTimeout(1_000);
    expect(counts).toEqual({ documents: 4, entryRequests: 4 });
    await expect(page.locator("#root")).toBeEmpty();
  } finally {
    await context.close();
  }
});

// One budget for the whole tab: a page chunk's failures that already reloaded the page three times
// leave none for the entry chunk, so the two recoveries cannot hand the page back and forth.
test("a missing entry chunk after a page chunk spent the tab's reloads stays", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Unreachable page" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.route(/\/assets\/IssuePage-[^/]+\.js$/u, (route) => route.abort());
  let entryMissing = false;
  const counts = await missEntryChunk(page, () => entryMissing);

  try {
    await page.goto(`/issues/${issue.key}`, { waitUntil: "commit" });
    // The issue page's code failed, reloaded the page three times, and failed again.
    await expect.poll(() => ({ ...counts })).toEqual({ documents: 4, entryRequests: 4 });
    await expect(page.getByTestId("error-boundary")).toBeVisible();

    entryMissing = true;
    await page.reload({ waitUntil: "commit" });
    await expect.poll(() => counts.entryRequests).toBe(5);
    // Long enough for a reload the miss started to have asked for the page again.
    await page.waitForTimeout(1_000);
    expect(counts).toEqual({ documents: 5, entryRequests: 5 });
    await expect(page.locator("#root")).toBeEmpty();
  } finally {
    await context.close();
  }
});

// The inline script and the bundle's own handler spend one budget between them: a page whose entry
// chunk is missing twice reloads twice, and its route's chunk failing after that reloads it once
// more and then stays. The broadcasts page's chunk imports nothing the entry does not already, so
// Vite inserts no preload link for it and only the bundle's handler sees it fail.
test("an entry chunk's reloads and a page chunk's share the tab's three", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.route(/\/assets\/BroadcastsPage-[^/]+\.js$/u, (route) => route.abort());
  const counts = await missEntryChunk(page, (request) => request <= 2);

  try {
    await page.goto("/agents/broadcasts", { waitUntil: "commit" });
    await expect.poll(() => ({ ...counts })).toEqual({ documents: 4, entryRequests: 4 });
    await expect(page.getByTestId("error-boundary")).toContainText(
      "Failed to fetch dynamically imported module"
    );
    // Long enough for a reload the last failure started to have asked for the page again.
    await page.waitForTimeout(1_000);
    expect(counts).toEqual({ documents: 4, entryRequests: 4 });
  } finally {
    await context.close();
  }
});

// The main stylesheet is linked after the inline recovery script, so one a server that never built
// it fails to serve reloads the page as a missing entry chunk does, rather than leaving the app
// running unstyled.
test("a page whose main stylesheet fails once reloads and renders styled", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const stylesheet = await pageAssetPath(
    /<link\b[^>]*\brel="stylesheet"[^>]*\bhref="([^"]+)"/u,
    "stylesheet"
  );
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const counts = await missEntryChunk(page, () => false);
  let stylesheetRequests = 0;
  await page.route(
    (url) => url.pathname === stylesheet,
    async (route) => {
      stylesheetRequests += 1;
      if (stylesheetRequests === 1) {
        await route.abort();
        return;
      }
      await route.continue();
    }
  );

  try {
    await page.goto("/", { waitUntil: "commit" });
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    // The stylesheet's preflight takes away the browser's own 8px margin around the body.
    await expect
      .poll(() => page.evaluate(() => getComputedStyle(document.body).margin))
      .toBe("0px");
    expect({ ...counts, stylesheetRequests }).toEqual({
      documents: 2,
      entryRequests: 2,
      stylesheetRequests: 2,
    });
  } finally {
    await context.close();
  }
});

/** Records reloads `minutesAgo` by the page's clock as the tab's recovery reloads, and returns
 *  their times. */
function recordTabReloads(page: Page, minutesAgo: number[]): Promise<number[]> {
  return page.evaluate((ages) => {
    const now = Date.now();
    const times = ages.map((age) => now - age * 60_000);
    window.sessionStorage.setItem("dispatch.chunk-reloads", JSON.stringify(times));
    return times;
  }, minutesAgo);
}

/** The tab's recorded recovery reloads. */
function tabReloads(page: Page): Promise<number[] | null> {
  return page.evaluate(() =>
    JSON.parse(window.sessionStorage.getItem("dispatch.chunk-reloads") ?? "null")
  );
}

/** Adds a script of this build that the server never had, as a replaced deployment's would fail. */
async function failBuildScript(page: Page): Promise<void> {
  await page.evaluate(() => {
    const script = document.createElement("script");
    script.src = "/assets/index-missing.js";
    document.head.append(script);
  });
}

// The budget renews: a reload counts against the tab for five minutes, so a tab that spent all
// three long ago recovers from the next deployment. This tab also holds the once-only key an
// earlier build's page spends in the same tab, which this build neither reads nor writes.
test("a tab whose three reloads are older than five minutes reloads for a new failure", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const counts = await missEntryChunk(page, () => false);

  try {
    await page.goto("/");
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    const spent = await recordTabReloads(page, [8, 7, 6]);
    await page.evaluate(() => window.sessionStorage.setItem("dispatch.reloaded-for-chunk", "true"));

    await failBuildScript(page);
    await expect.poll(() => counts.documents).toBe(2);
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    // The three old reloads have left the window; the new one is all the tab now holds.
    const reloads = await tabReloads(page);
    expect(reloads).toHaveLength(1);
    expect(reloads?.[0]).toBeGreaterThan(Math.max(...spent));
  } finally {
    await context.close();
  }
});

// Three reloads in the last five minutes spend the tab's budget, whatever the failure: a page
// whose assets keep failing stops reloading rather than reloading for ever.
test("a tab that reloaded three times in the last five minutes does not reload for a new failure", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const counts = await missEntryChunk(page, () => false);

  try {
    await page.goto("/");
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    const spent = await recordTabReloads(page, [3, 2, 1]);

    await failBuildScript(page);
    // Long enough for a reload the failure started to have asked for the page again.
    await page.waitForTimeout(1_000);
    expect(counts).toEqual({ documents: 1, entryRequests: 1 });
    expect(await tabReloads(page)).toEqual(spent);
  } finally {
    await context.close();
  }
});

// A failed download while the browser is offline is an outage, not a replaced deployment, and a
// reload would swap the blank page for the browser's offline page.
test("a page whose entry chunk fails while the browser is offline does not reload", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.addInitScript(() => {
    Object.defineProperty(Navigator.prototype, "onLine", { configurable: true, get: () => false });
  });
  const counts = await missEntryChunk(page, () => true);

  try {
    await page.goto("/", { waitUntil: "commit" });
    await expect.poll(() => counts.entryRequests).toBeGreaterThan(0);
    // Long enough for a reload the miss started to have asked for the page again.
    await page.waitForTimeout(1_000);
    expect(counts).toEqual({ documents: 1, entryRequests: 1 });
  } finally {
    await context.close();
  }
});

// WebKit and Firefox cancel the downloads still in flight when the reader navigates away, and a
// cancelled entry chunk fails like a missing one; a reload then would replace the reader's
// navigation. The page counts as left from `beforeunload`, or from `pagehide` in a browser that
// fires no `beforeunload` (iOS Safari), until it is shown again.
for (const leaving of ["beforeunload", "pagehide"]) {
  test(`a page whose entry chunk fails after ${leaving} stays until it is shown again`, async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
    const context = await asUser(browser, "alice");
    const page = await context.newPage();
    const requested = Promise.withResolvers<void>();
    const released = Promise.withResolvers<void>();
    const counts = await missEntryChunk(page, async (request) => {
      if (request > 1) {
        return false;
      }
      requested.resolve();
      await released.promise;
      return true;
    });

    try {
      await page.goto("/", { waitUntil: "commit" });
      await requested.promise;
      await page.evaluate((type) => window.dispatchEvent(new Event(type)), leaving);
      released.resolve();
      // Long enough for a reload the miss started to have asked for the page again.
      await page.waitForTimeout(1_000);
      expect(counts).toEqual({ documents: 1, entryRequests: 1 });

      // Shown again (a back/forward-cache restore fires `pageshow`), the page's next failed build
      // script reloads it.
      await page.evaluate(() => {
        window.dispatchEvent(new Event("pageshow"));
        const script = document.createElement("script");
        script.src = "/assets/index-missing.js";
        document.head.append(script);
      });
      await expect(page.getByText("Signed in as alice")).toBeVisible();
      expect(counts).toEqual({ documents: 2, entryRequests: 2 });
    } finally {
      await context.close();
    }
  });
}

// The recovery is for this build's own assets. A script another origin serves (a browser
// extension's, or a third party's) is not this build's, whatever its path, so its failure to load
// neither reloads a page the reader is using nor spends the tab's budget.
test("a script from another origin that fails under an /assets/ path leaves the page alone", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the recovery does not depend on the layout");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.route("https://extension.invalid/**", (route) => route.abort());
  const counts = await missEntryChunk(page, () => false);

  try {
    await page.goto("/");
    await expect(page.getByText("Signed in as alice")).toBeVisible();
    await page
      .evaluate(
        () =>
          new Promise<void>((resolve) => {
            const script = document.createElement("script");
            script.src = "https://extension.invalid/assets/inject.js";
            script.addEventListener("error", () => resolve());
            document.head.append(script);
          })
      )
      .catch(() => undefined);
    // Long enough for a reload the failure started to have asked for the page again.
    await page.waitForTimeout(1_000);
    expect(counts).toEqual({ documents: 1, entryRequests: 1 });
    expect(await tabReloads(page)).toBeNull();
  } finally {
    await context.close();
  }
});

test("the document editor renders headings and ordered lists with real typography", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Title\n\nSome body text.\n\n## Section\n\n### Detail\n\n1. First\n2. Second\n",
    title: "Spec typography",
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();

  await expect(page.getByRole("heading", { level: 1, name: "Title" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 2, name: "Section" })).toBeVisible();

  const sizes = await page.evaluate(() => {
    const root = document.querySelector('[role="textbox"][aria-label="Document editor"]');
    const h1 = root?.querySelector("h1");
    const h2 = root?.querySelector("h2");
    const paragraph = root?.querySelector("p");
    const list = root?.querySelector("ol");
    return {
      h1: h1 ? Number.parseFloat(getComputedStyle(h1).fontSize) : 0,
      h2: h2 ? Number.parseFloat(getComputedStyle(h2).fontSize) : 0,
      listStyleType: list ? getComputedStyle(list).listStyleType : "",
      paragraph: paragraph ? Number.parseFloat(getComputedStyle(paragraph).fontSize) : 0,
    };
  });

  expect(sizes.h1).toBeGreaterThan(sizes.paragraph);
  expect(sizes.h2).toBeGreaterThan(sizes.paragraph);
  expect(sizes.h1).toBeGreaterThan(sizes.h2);
  expect(sizes.listStyleType).not.toBe("none");

  await context.close();
});

/** Enough projects that the navigation is taller than any viewport under test. */
async function seedManyProjects(count: number): Promise<void> {
  for (let index = 0; index < count; index += 1) {
    const key = `P${String(index).padStart(2, "0")}`;
    await createProject({ key, name: `Project ${key}` });
  }
}

// The sidebar's footer holds Sign out, and the navigation above it grows with the project list.
// A column that is only as tall as the viewport at its minimum pushes the footer and the lower
// links off the bottom of the screen, where a reader reaches them only by scrolling to the end
// of the page - or, on a phone, not at all, because the drawer is fixed to the viewport.
test("the sidebar scrolls, so Sign out is reachable with a navigation taller than the viewport", async ({
  browser,
}, testInfo) => {
  await seedManyProjects(17);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const compact = testInfo.project.name !== "chromium";

  try {
    if (compact) {
      await page.setViewportSize({ height: 390, width: 844 });
    } else {
      await page.setViewportSize({ height: 720, width: 1280 });
    }
    await page.goto("/");

    if (compact) {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    const signOut = page.getByRole("button", { name: "Sign out" });
    await expect(signOut).toBeAttached();
    // The rail is its own scroll container: the footer comes into view by scrolling the rail,
    // and the page itself never moves, so the footer is never left past the end of a long
    // issue, reachable only by scrolling the document to its end - or, in the drawer, not at all.
    await signOut.scrollIntoViewIfNeeded();
    await expect(signOut).toBeInViewport();
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    await signOut.focus();
    await expect(signOut).toBeInViewport();
    await expect(signOut).toBeFocused();
    // Every project link is reachable by scrolling that container, still not the page.
    const lastProject = page.getByRole("link", { name: /P16/ });
    await lastProject.scrollIntoViewIfNeeded();
    await expect(lastProject).toBeInViewport();
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
  } finally {
    await context.close();
  }
});

// The right gutter is reserved for the collapsed-margin rail, and that rail only exists where
// the route has a margin. Reading the raw preference instead left 80px of padding on the right
// of the Inbox, beside nothing.
test("a route with no margin reserves no gutter for a margin rail", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the rails are a desktop layout");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Has a margin" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("button", { name: "Hide margin" }).click();
    await expect(page.getByTestId("margin-rail")).toBeVisible();
    const main = page.getByTestId("main-content");
    expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("80px");

    await page.goto("/");
    await expect(page.getByTestId("margin-rail")).toHaveCount(0);
    expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("24px");
    await expect(main).toHaveAttribute("data-shell-layout", "standard");

    await page.getByRole("button", { name: "Hide sidebar" }).click();
    await expect(main).toHaveAttribute("data-shell-layout", "full-width");
    expect(await main.evaluate((node) => getComputedStyle(node).paddingRight)).toBe("24px");
    expect(await main.evaluate((node) => getComputedStyle(node).paddingLeft)).toBe("80px");
  } finally {
    await context.close();
  }
});
