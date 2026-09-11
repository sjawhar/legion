import { expect, type Page, test } from "@playwright/test";

import { createIssue, createMessage, createProject, putIssueState } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = { as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

async function readingAnchor(page: Page): Promise<string> {
  const eventSequence = await page.evaluate(() => {
    const conversation = document.querySelector<HTMLElement>('[aria-label="Conversation"]');
    if (conversation === null) {
      return null;
    }
    const rect = conversation.getBoundingClientRect();
    const x = rect.left + rect.width / 2;
    const y = (Math.max(rect.top, 0) + Math.min(rect.bottom, window.innerHeight)) / 2;
    for (const offset of [0, 8, 16, 24, 32, 40, -8, -16, -24, -32, -40]) {
      const item = document
        .elementFromPoint(x, y + offset)
        ?.closest<HTMLElement>("[data-event-seq]");
      if (item?.dataset.eventSeq) {
        return item.dataset.eventSeq;
      }
    }
    return null;
  });
  expect(eventSequence).not.toBeNull();
  return eventSequence ?? "";
}

test("a reader scrolled up keeps their Conversation viewport when a new turn arrives", async ({
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

    const sequence = await readingAnchor(page);
    const anchor = page.locator(`[data-event-seq="${sequence}"]`);
    const before = (await anchor.boundingBox())?.y;

    await createMessage(issue.key, { body: "Arrived while reading" }, session);

    await expect(page.getByTestId("jump-to-latest")).toBeVisible();
    await expect(page.getByText("Arrived while reading")).toBeVisible();
    await expect.poll(async () => (await anchor.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
});

test("a reader at the Conversation top follows a new turn", async ({ browser }) => {
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

test("loading older Conversation turns keeps the reader anchor", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Older turns" });
  for (let index = 0; index < 250; index += 1) {
    await createMessage(issue.key, { body: `Older message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
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

    const sequence = await readingAnchor(page);
    const anchor = page.locator(`[data-event-seq="${sequence}"]`);
    const before = (await anchor.boundingBox())?.y;
    await loadOlder.click();

    await expect(page.getByText("Older message 0")).toBeVisible();
    await expect.poll(async () => (await anchor.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
});

async function verifyGapDetection(
  browser: Parameters<typeof asUser>[0],
  viewport?: { height: number; width: number }
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 20; index += 1) {
    await createMessage(issue.key, { body: `Gap message ${index}` }, session);
  }
  await putIssueState(issue.key, { last_read_seq: 10_000 });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    if (viewport !== undefined) {
      await page.setViewportSize(viewport);
    }
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByText("Gap message 19")).toBeVisible();

    const gap = await page.evaluate(() => {
      const conversation = document.querySelector('[aria-label="Conversation"]');
      if (conversation === null) {
        return null;
      }
      const turns = [...document.querySelectorAll<HTMLElement>("[data-event-seq]")];
      const gaps = [];
      for (let index = 0; index < turns.length - 1; index += 1) {
        const upper = turns[index]?.getBoundingClientRect();
        const lower = turns[index + 1]?.getBoundingClientRect();
        if (upper !== undefined && lower !== undefined && lower.top > upper.bottom) {
          gaps.push({
            gapMidPage: (upper.bottom + lower.top) / 2 + window.scrollY,
            index,
            lowerSeq: turns[index + 1]?.dataset.eventSeq,
          });
        }
      }
      if (gaps.length === 0) {
        return null;
      }
      const middle = turns.length / 2;
      gaps.sort((left, right) => Math.abs(left.index - middle) - Math.abs(right.index - middle));
      return gaps[0];
    });
    expect(gap).not.toBeNull();
    if (gap === null || gap.lowerSeq === undefined) {
      throw new Error("no gap found between adjacent Conversation turns");
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
    const centerHitsTurn = await page.evaluate((point) => {
      const element = document.elementFromPoint(point.x, point.y);
      return element !== null && element.closest("[data-event-seq]") !== null;
    }, conversationCenter);
    expect(centerHitsTurn).toBe(false);

    const lowerTurn = page.locator(`[data-event-seq="${gap.lowerSeq}"]`);
    const before = (await lowerTurn.boundingBox())?.y;
    await putIssueState(issue.key, { last_read_seq: Number(gap.lowerSeq) });
    await page.evaluate(() => {
      const now = Date.now();
      Date.now = () => now + 31_000;
      window.dispatchEvent(new Event("visibilitychange"));
      window.dispatchEvent(new Event("focus"));
    });

    await expect(page.getByRole("separator", { name: "New since you last read" })).toBeVisible();
    await expect.poll(async () => (await lowerTurn.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
}

test("an unread divider landing in a Conversation gap keeps the lower turn fixed", async ({
  browser,
}) => {
  await verifyGapDetection(browser);
});

test("an unread divider in a narrow Conversation keeps the lower turn fixed", async ({
  browser,
}) => {
  await verifyGapDetection(browser, { height: 1024, width: 768 });
});
