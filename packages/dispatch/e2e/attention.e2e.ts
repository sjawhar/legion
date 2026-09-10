import { expect, type Page, test } from "@playwright/test";

import { createIssue, createMessage, createProject, getIssueEvents, putIssueState } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = { as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

// Finds whichever log article the reader is currently looking at (the one under the viewport's
// vertical center), scanning a little further down in case the center lands exactly on the gap
// between two cards.
async function readingAnchor(page: Page): Promise<string> {
  const readingText = await page.evaluate(() => {
    const x = window.innerWidth / 2;
    for (let offset = 0; offset < 200; offset += 4) {
      const el = document.elementFromPoint(x, window.innerHeight / 2 + offset);
      const text = el?.closest("article")?.querySelector("p")?.textContent;
      if (text) {
        return text;
      }
    }
    return null;
  });
  expect(readingText).not.toBeNull();
  return readingText ?? "";
}

test("new events leave the reader's scroll position unchanged", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}/log`);
  await expect(page.getByText("Existing message 29")).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 500));

  // The invariant is what the reader sees, not a raw scroll offset: the page's total height can
  // shift for unrelated reasons, so pin the on-screen position of whatever message the reader is
  // currently looking at. Confirming it differs from the very first article is the observable
  // proof that the scroll actually moved the reader past the top, in place of asserting scrollY.
  const readingText = await readingAnchor(page);
  const firstArticleText = await page.locator("[data-event-seq] p").first().textContent();
  expect(readingText).not.toBe(firstArticleText);
  const reading = page.getByText(readingText, { exact: true });
  const before = (await reading.boundingBox())?.y;

  await expect
    .poll(() =>
      page.evaluate(async (issueKey) => {
        const response = await fetch("/api/v1/me/state");
        const state = (await response.json()) as Record<string, { last_read_seq: number }>;
        return state[issueKey]?.last_read_seq ?? 0;
      }, issue.key)
    )
    .toBeGreaterThan(0);

  await createMessage(issue.key, { body: "Arrived while reading" }, session);

  await expect(page.getByText("New")).toBeVisible();
  await expect(page.getByText("Arrived while reading")).toBeVisible();
  await expect.poll(async () => (await reading.boundingBox())?.y).toBe(before);

  await context.close();
});

test("a reader already at the top sees a new event appear without any scroll compensation", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 10; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}/log`);
  const firstArticle = page.locator("[data-event-seq]").first();
  await expect(page.getByText("Existing message 9")).toBeVisible();
  const before = (await firstArticle.boundingBox())?.y;

  await createMessage(issue.key, { body: "Arrived while at the top" }, session);

  await expect(page.getByText("Arrived while at the top")).toBeVisible();
  await expect.poll(async () => (await firstArticle.boundingBox())?.y).toBe(before);
  await expect(firstArticle).toContainText("Arrived while at the top");

  await context.close();
});

test("scrolling without a state-changing render still compensates against the reader's latest position", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }
  // Mark everything already read directly, so neither scroll below triggers a read-tracking
  // write (and therefore no React render) before the new event arrives: the fix must track the
  // reader's position from the actual scroll events, not from whatever a render last saw.
  const events = await getIssueEvents(issue.key, { limit: 200 });
  await putIssueState(issue.key, { last_read_seq: Math.max(...events.map((event) => event.seq)) });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}/log`);
  await expect(page.getByText("Existing message 29")).toBeVisible();

  await page.evaluate(() => window.scrollTo(0, 200));
  await page.evaluate(() => window.scrollTo(0, 600));

  const reading = page.getByText(await readingAnchor(page), { exact: true });
  const before = (await reading.boundingBox())?.y;

  await createMessage(issue.key, { body: "Arrived after silent scrolling" }, session);

  await expect(page.getByText("Arrived after silent scrolling")).toBeVisible();
  await expect.poll(async () => (await reading.boundingBox())?.y).toBe(before);

  await context.close();
});

test("returning to the top after reading further down is not undone by a stale anchor", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }
  const events = await getIssueEvents(issue.key, { limit: 200 });
  await putIssueState(issue.key, { last_read_seq: Math.max(...events.map((event) => event.seq)) });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}/log`);
  await expect(page.getByText("Existing message 29")).toBeVisible();

  // Force a genuine render while scrolled away from the top (deterministically, via a Pin click
  // rather than the read-tracking timer) — this is the render a stale, render-sampled anchor
  // would remember.
  await page.evaluate(() => window.scrollTo(0, 500));
  await page.evaluate(() => {
    const button = [...document.querySelectorAll("button")].find(
      (element) => element.textContent === "Pin"
    );
    button?.click();
  });
  await expect(page.getByRole("button", { name: "Unpin" }).first()).toBeVisible();

  // Scroll back to the top without any further render before the new event arrives.
  await page.evaluate(() => window.scrollTo(0, 0));
  const firstArticle = page.locator("[data-event-seq]").first();
  const before = (await firstArticle.boundingBox())?.y;

  await createMessage(issue.key, { body: "Arrived back at the top" }, session);

  await expect(page.getByText("Arrived back at the top")).toBeVisible();
  await expect.poll(async () => (await firstArticle.boundingBox())?.y).toBe(before);
  await expect(firstArticle).toContainText("Arrived back at the top");

  await context.close();
});

test("returning to the top is safe even if no animation frame ever samples the scroll", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }
  const events = await getIssueEvents(issue.key, { limit: 200 });
  await putIssueState(issue.key, { last_read_seq: Math.max(...events.map((event) => event.seq)) });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}/log`);
  await expect(page.getByText("Existing message 29")).toBeVisible();

  // Force a genuine render while scrolled away from the top, same as above.
  await page.evaluate(() => window.scrollTo(0, 500));
  await page.evaluate(() => {
    const button = [...document.querySelectorAll("button")].find(
      (element) => element.textContent === "Pin"
    );
    button?.click();
  });
  await expect(page.getByRole("button", { name: "Unpin" }).first()).toBeVisible();

  // From here on, disable the trailing animation-frame sample entirely — this deterministically
  // forces the exact race a throttled-only sample would lose: the final scroll back to the top,
  // and the event that follows it, both land with no scroll sample able to complete. Only a live
  // window.scrollY check at compensation-decision time (not any sample, stale or fresh) can keep
  // a reader who has just returned to the top from being pulled back down.
  await page.evaluate(() => {
    window.requestAnimationFrame = () => 0;
    const withCounter = window as unknown as Window & { scrollByCalls: number };
    withCounter.scrollByCalls = 0;
    const originalScrollBy = window.scrollBy.bind(window);
    withCounter.scrollBy = ((x: number, y: number) => {
      withCounter.scrollByCalls += 1;
      originalScrollBy(x, y);
    }) as typeof window.scrollBy;
  });

  await page.evaluate(() => window.scrollTo(0, 0));
  const firstArticle = page.locator("[data-event-seq]").first();
  const before = (await firstArticle.boundingBox())?.y;

  await createMessage(issue.key, { body: "Arrived with no trailing frame" }, session);

  await expect(page.getByText("Arrived with no trailing frame")).toBeVisible();
  expect(
    await page.evaluate(
      () => (window as unknown as Window & { scrollByCalls: number }).scrollByCalls
    )
  ).toBe(0);
  await expect.poll(async () => (await firstArticle.boundingBox())?.y).toBe(before);
  await expect(firstArticle).toContainText("Arrived with no trailing frame");

  await context.close();
});

async function verifyGapDetection(
  browser: Parameters<typeof asUser>[0],
  viewport?: { height: number; width: number }
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  // Plenty of short messages so the log column is comfortably taller than the viewport at a
  // middle gap.
  for (let index = 0; index < 20; index += 1) {
    await createMessage(issue.key, { body: `Msg ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  if (viewport !== undefined) {
    await page.setViewportSize(viewport);
  }
  await page.goto(`/issues/${issue.key}/log`);
  await expect(page.getByText("Msg 19")).toBeVisible();

  // Find a gap between two adjacent cards near the middle of the log (comfortable content on
  // both sides) and scroll it to the log column's own visible center — not the window's —
  // matching the fix: the log column is often narrower than, and off-center in, the viewport
  // once the sidebar and/or margin panel take their share of it. At a 768px viewport (the
  // Tailwind `md` breakpoint, where the margin panel becomes a co-resident static column
  // instead of a bottom sheet) the log column is both far narrower than, and off-center from,
  // the window: window-center probing lands in the sidebar or margin panel, missing every
  // article and never detecting a content change at all.
  const gap = await page.evaluate(() => {
    const section = document.querySelector('[aria-label="Issue log"]');
    if (section === null) {
      return null;
    }
    const articles = [...document.querySelectorAll<HTMLElement>("[data-event-seq]")];
    const gaps = [];
    for (let index = 0; index < articles.length - 1; index += 1) {
      const upper = articles[index].getBoundingClientRect();
      const lower = articles[index + 1].getBoundingClientRect();
      if (lower.top > upper.bottom) {
        gaps.push({
          gapMidPage: (upper.bottom + lower.top) / 2 + window.scrollY,
          index,
          lowerSeq: articles[index + 1].dataset.eventSeq,
        });
      }
    }
    if (gaps.length === 0) {
      return null;
    }
    const middle = articles.length / 2;
    gaps.sort((a, b) => Math.abs(a.index - middle) - Math.abs(b.index - middle));
    return gaps[0];
  });
  expect(gap).not.toBeNull();
  if (gap === null) {
    throw new Error("no gap found between adjacent log cards");
  }

  await page.evaluate(
    (midPage) => window.scrollTo(0, midPage - window.innerHeight / 2),
    gap.gapMidPage
  );

  // Recompute the hook's own probe point from the live log column geometry, rather than assuming
  // it lines up with the window's center, and confirm it genuinely misses both cards.
  const probePoint = await page.evaluate(() => {
    const section = document.querySelector('[aria-label="Issue log"]');
    if (section === null) {
      return null;
    }
    const rect = section.getBoundingClientRect();
    const x = rect.left + rect.width / 2;
    const visibleTop = Math.max(rect.top, 0);
    const visibleBottom = Math.min(rect.bottom, window.innerHeight);
    return { x, y: (visibleTop + visibleBottom) / 2 };
  });
  expect(probePoint).not.toBeNull();
  if (probePoint === null) {
    throw new Error("could not locate the log column");
  }
  const centerHitsCard = await page.evaluate((point) => {
    const element = document.elementFromPoint(point.x, point.y);
    return element !== null && element.closest("[data-event-seq]") !== null;
  }, probePoint);
  expect(centerHitsCard).toBe(false);

  const lowerCard = page.locator(`[data-event-seq="${gap.lowerSeq}"]`);
  const before = (await lowerCard.boundingBox())?.y;

  // Mark the lower card (and everything older) read without reloading the page — inserting a
  // "New" divider directly between the two cards straddling the reader's viewport center — by
  // setting the read state through the API and letting TanStack Query's refetch-on-focus pick it
  // up, the same live-update path a real tab regaining focus goes through.
  await putIssueState(issue.key, { last_read_seq: Number(gap.lowerSeq) });
  await page.evaluate(() => {
    window.dispatchEvent(new Event("visibilitychange"));
    window.dispatchEvent(new Event("focus"));
  });

  await expect(page.getByText("New since you last read")).toBeVisible();
  await expect.poll(async () => (await lowerCard.boundingBox())?.y).toBe(before);

  await context.close();
}

test("a content change landing in the gap between two cards is still detected via the lower card", async ({
  browser,
}) => {
  await verifyGapDetection(browser);
});

test("a content change landing in the gap is still detected at the md breakpoint, where the log column is narrow and off-center", async ({
  browser,
}) => {
  await verifyGapDetection(browser, { height: 1024, width: 768 });
});
