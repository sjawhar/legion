import { expect, type Page, test } from "@playwright/test";

import { createComment, createIssue, createMessage, createProject, putIssueState } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { id: "e2e-attention", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/**
 * The row the reader is on, read the way `ViewportAnchor` reads it: the row nearest the visible
 * centre of the list, probed through `elementFromPoint`. `root` is the list's own labelled box
 * and `item` the attribute each of its rows carries - `data-turn` in the Conversation,
 * `data-event-seq` in the Activity tab.
 */
async function readingAnchor(page: Page, root: string, item: string): Promise<string> {
  const value = await page.evaluate(
    ({ itemAttribute, rootSelector }) => {
      const list = document.querySelector<HTMLElement>(rootSelector);
      if (list === null) {
        return null;
      }
      const rect = list.getBoundingClientRect();
      const x = rect.left + rect.width / 2;
      const y = (Math.max(rect.top, 0) + Math.min(rect.bottom, window.innerHeight)) / 2;
      for (const offset of [0, 8, 16, 24, 32, 40, -8, -16, -24, -32, -40]) {
        const row = document
          .elementFromPoint(x, y + offset)
          ?.closest<HTMLElement>(`[${itemAttribute}]`);
        const found = row?.getAttribute(itemAttribute);
        if (found !== null && found !== undefined && found !== "") {
          return found;
        }
      }
      return null;
    },
    { itemAttribute: item, rootSelector: root }
  );
  expect(value).not.toBeNull();
  return value ?? "";
}

test("a reader scrolled up keeps their Conversation viewport when a new card arrives @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Existing message 29")).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 500));

    const id = await readingAnchor(page, '[aria-label="Conversation"]', "data-turn");
    const anchor = page.locator(`[data-turn="${id}"]`);
    const before = (await anchor.boundingBox())?.y;

    await createMessage(issue.key, { body: "Arrived while reading" }, session);

    await expect(page.getByTestId("jump-to-latest")).toBeVisible();
    await expect(page.getByText("Arrived while reading")).toBeVisible();
    await expect.poll(async () => (await anchor.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
});

test("a reader at the Conversation top follows a new card @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Existing message 29")).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 0));

    await createMessage(issue.key, { body: "Arrived at the top" }, session);

    const arrived = page.getByText("Arrived at the top", { exact: true });
    await expect(arrived).toBeVisible();
    const box = await arrived.boundingBox();
    expect(box).not.toBeNull();
    expect(box?.y ?? -1).toBeGreaterThanOrEqual(0);
    expect((box?.y ?? 0) + (box?.height ?? 0)).toBeLessThanOrEqual(
      await page.evaluate(() => window.innerHeight)
    );
  } finally {
    await context.close();
  }
});

// Paging is the Activity tab's: the Conversation reads each of its three lists whole and offers
// no `Load older` at all. Older events land beneath the rows already on screen, so the row the
// reader is on does not move.
test("loading older Activity rows keeps the reader anchor @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Older events" });
  for (let index = 0; index < 250; index += 1) {
    await createMessage(issue.key, { body: `Older message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("button", { name: "Load older" })).toHaveCount(0);

    await page.goto(`/issues/${issue.key}/activity`);
    const loadOlder = page.getByRole("button", { name: "Load older" });
    await expect(loadOlder).toBeVisible();
    await loadOlder.scrollIntoViewIfNeeded();
    await page.evaluate(
      () =>
        new Promise<void>((resolve) => {
          requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
        })
    );
    const loadOlderBox = await loadOlder.boundingBox();
    expect(loadOlderBox).not.toBeNull();
    expect(loadOlderBox?.y ?? -1).toBeGreaterThanOrEqual(0);
    expect((loadOlderBox?.y ?? 0) + (loadOlderBox?.height ?? 0)).toBeLessThanOrEqual(
      await page.evaluate(() => window.innerHeight)
    );

    const sequence = await readingAnchor(page, '[aria-label="Issue activity"]', "data-event-seq");
    const anchor = page.locator(`[data-event-seq="${sequence}"]`);
    const before = (await anchor.boundingBox())?.y;
    await loadOlder.click();

    // An Activity row reads the event's description, not a message's body, so the proof the
    // older page landed is the oldest row of all: the issue's own creation, which only the
    // page beneath the first 200 holds.
    await expect(
      page
        .getByRole("list", { name: "Issue activity" })
        .getByRole("listitem")
        .filter({ hasText: "created the issue" })
    ).toHaveCount(1);
    await expect.poll(async () => (await anchor.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
});

// The read mark is the Conversation's, and the only thing it draws is each card's own `New`:
// a mark that moves adds or removes a row above every unread card, so the reader's own card has
// to stay where it was. The reader is parked over a gap between two cards, where
// `ViewportAnchor`'s probe has to fall back to the card below.
async function verifyGapDetection(
  browser: Parameters<typeof asUser>[0],
  viewport?: { height: number; width: number }
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 20; index += 1) {
    await createComment(issue.key, { body: `Gap comment ${index}` }, session);
  }
  // Far beyond the issue's head, so nothing is unread and no card carries the mark yet.
  await putIssueState(issue.key, { last_read_seq: 10_000 });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    if (viewport !== undefined) {
      await page.setViewportSize(viewport);
    }
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Gap comment 19")).toBeVisible();
    const turns = page.getByRole("list", { name: "Conversation turns" });
    await expect(turns.getByText("New", { exact: true })).toHaveCount(0);

    const gap = await page.evaluate(() => {
      const conversation = document.querySelector('[aria-label="Conversation"]');
      if (conversation === null) {
        return null;
      }
      const cards = [
        ...document.querySelectorAll<HTMLElement>('[aria-label="Conversation turns"] > li'),
      ];
      const gaps = [];
      for (let index = 0; index < cards.length - 1; index += 1) {
        const upper = cards[index]?.getBoundingClientRect();
        const lower = cards[index + 1]?.getBoundingClientRect();
        if (upper !== undefined && lower !== undefined && lower.top > upper.bottom) {
          gaps.push({
            gapMidPage: (upper.bottom + lower.top) / 2 + window.scrollY,
            index,
            lowerId: cards[index + 1]?.getAttribute("data-turn"),
          });
        }
      }
      if (gaps.length === 0) {
        return null;
      }
      const middle = cards.length / 2;
      gaps.sort((left, right) => Math.abs(left.index - middle) - Math.abs(right.index - middle));
      return gaps[0];
    });
    expect(gap).not.toBeNull();
    if (gap === null || gap.lowerId === undefined || gap.lowerId === null) {
      throw new Error("no gap found between adjacent Conversation cards");
    }

    await page.evaluate(
      (midPage) => window.scrollTo(0, midPage - window.innerHeight / 2),
      gap.gapMidPage
    );
    const conversationCenter = await page.evaluate(() => {
      const conversation = document.querySelector('[aria-label="Conversation"]');
      if (conversation === null) {
        return null;
      }
      const rect = conversation.getBoundingClientRect();
      const x = rect.left + rect.width / 2;
      const visibleTop = Math.max(rect.top, 0);
      const visibleBottom = Math.min(rect.bottom, window.innerHeight);
      return { x, y: (visibleTop + visibleBottom) / 2 };
    });
    expect(conversationCenter).not.toBeNull();
    if (conversationCenter === null) {
      throw new Error("could not locate the Conversation column");
    }
    const centerHitsCard = await page.evaluate((point) => {
      const element = document.elementFromPoint(point.x, point.y);
      return element !== null && element.closest("[data-turn]") !== null;
    }, conversationCenter);
    expect(centerHitsCard).toBe(false);

    const lowerCard = page.locator(`[data-turn="${gap.lowerId}"]`);
    const before = (await lowerCard.boundingBox())?.y;
    // The reader's mark moves back to the start of the issue, so every card above them gains a
    // `New` row. Only a refocus re-reads it, as a tab left open overnight would.
    await putIssueState(issue.key, { last_read_seq: 0 });
    await page.evaluate(() => {
      const now = Date.now();
      Date.now = () => now + 31_000;
      window.dispatchEvent(new Event("visibilitychange"));
      window.dispatchEvent(new Event("focus"));
    });

    await expect(lowerCard.getByText("New", { exact: true })).toHaveCount(1);
    await expect.poll(async () => (await lowerCard.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
}

test("a read mark moving under a reader parked in a Conversation gap keeps the lower card fixed @phone", async ({
  browser,
}) => {
  await verifyGapDetection(browser);
});

test("a read mark moving in a narrow Conversation keeps the lower card fixed", async ({
  browser,
}) => {
  await verifyGapDetection(browser, { height: 1024, width: 768 });
});
