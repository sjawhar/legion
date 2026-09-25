import { expect, type Page, test } from "@playwright/test";

import { REF_PREVIEW_CLOSE_DELAY_MS } from "../web/src/features/refs/ref-preview-timing";
import { createComment, createIssue, createProject, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-hover-agent", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** Hold the page's clock still, so a check about the close delay cannot race it. `pauseAt` takes
 * a time on the page's own installed clock, and the Node process's `Date.now()` can already be
 * behind that clock, which would ask it to move backwards: a lead of `+ 1` ms failed 11 runs of
 * 12, and a second is past any skew. `pauseAt` runs every page timer that falls due inside the
 * jump; at all three call sites the card is open, so its open timer has already fired and no
 * close is armed, and the jump fires no close. */
async function pauseClock(page: Page): Promise<void> {
  await page.clock.pauseAt(Date.now() + 1000);
}

async function seedReference() {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({
    project: "CORE",
    spec: "The target's spec explains what hovering should reveal about it.",
    title: "Hover target",
  });
  await patchIssue(target.key, { labels: ["ui"], priority: 1, status: "todo" });
  const source = await createIssue({ project: "CORE", title: "Hover source" });
  await createComment(source.key, { body: `See dispatch://${target.key} for context.` }, agent);
  return { source, target };
}

test("hovering a dispatch:// reference in a comment previews the target issue; Escape closes it", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "touch behaviour is covered by the tap test");
  const { source, target } = await seedReference();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${source.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    const link = turns.getByRole("link", { name: "Hover target" });
    await expect(link).toBeVisible();
    await expect(page.getByRole("tooltip")).toHaveCount(0);

    await link.hover();
    const card = page.getByRole("tooltip");
    await expect(card).toBeVisible();
    await expect(card).toContainText(target.key);
    await expect(card).toContainText("Hover target");
    await expect(card).toContainText("Todo");
    await expect(card).toContainText("P1");
    await expect(card).toContainText("ui");
    const cardId = await card.getAttribute("id");
    expect(cardId).not.toBeNull();
    await expect(link).toHaveAttribute("aria-describedby", cardId ?? "");
    await expect(card.getByRole("link")).toHaveAttribute("href", `/issues/${target.key}`);
    await page.screenshot({ path: testInfo.outputPath("hover-preview-comment-1280.png") });

    await page.keyboard.press("Escape");
    await expect(card).toHaveCount(0);
    // Escape dismissed the card only: the page is unchanged and the reference still navigates.
    await expect(page).toHaveURL(new RegExp(`/issues/${source.key}/conversation`));

    // Keyboard focus opens the card without any hover delay, and a scroll while the reference
    // holds focus (focusing can itself scroll it into view) keeps the card rather than closing it.
    await page.mouse.move(0, 0);
    await page.keyboard.press("Tab");
    await link.focus();
    await expect(card).toBeVisible();
    await page.evaluate(() => window.scrollBy(0, 40));
    await expect(card).toBeVisible();
    await link.blur();
    await expect(card).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("hovering a board card previews the issue and moving onto the card keeps it open", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "touch behaviour is covered by the tap test");
  const { target } = await seedReference();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const todo = page.getByRole("region", { name: "Todo" });
    const link = todo.getByRole("link", { name: `${target.key} Hover target` });
    await link.hover();
    const card = page.getByRole("tooltip");
    await expect(card).toBeVisible();
    await expect(card).toContainText("Hover target");
    await expect(card).toContainText("The target's spec explains");
    await page.screenshot({ path: testInfo.outputPath("hover-preview-board-1280.png") });

    // A drag lift closes the card: the title link is the whole-card drag activator, and the
    // trailing click that would otherwise dismiss the card is swallowed by the drag.
    const box = await link.boundingBox();
    if (box === null) {
      throw new Error("board card is not visible");
    }
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 + 12, box.y + box.height / 2 + 12, { steps: 4 });
    await expect(card).toHaveCount(0);
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 4 });
    await page.mouse.up();
    await expect(page).toHaveURL(/\/projects\/CORE/);
    await expect(page.getByRole("tooltip")).toHaveCount(0);

    // Hovering again reopens it - and the drop left keyboard focus on the title link, so the
    // press on the card must not blur the card away. The hover bridge still holds: the pointer
    // can travel onto the card without closing it, and clicking the card follows the same link.
    await page.mouse.move(0, 0);
    await link.hover();
    await expect(card).toBeVisible();
    // `hover()` moves the pointer in one step, straight from the reference onto the card. A close
    // that move armed would run on the page's timers, so the check drives those timers: with the
    // clock held still the pointer's travel takes no page time at all, and advancing it past
    // twice REF_PREVIEW_CLOSE_DELAY_MS fires any close the move started.
    await pauseClock(page);
    await card.hover();
    await page.clock.runFor(REF_PREVIEW_CLOSE_DELAY_MS * 2);
    await expect(card).toBeVisible();
    await page.clock.resume();
    await card.getByRole("link").click();
    await expect(page).toHaveURL(new RegExp(`/issues/${target.key}(/|$)`));
    await expect(page.getByRole("tooltip")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a card survives a held-button drag that leaves its reference and comes back", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "a held mouse button is not a touch gesture");
  const { source } = await seedReference();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The gesture below takes longer than the close delay on a loaded machine, so the page's
    // clock is the test's: paused for the drag, then advanced past the delay deliberately.
    // Wall-clock timing would race the timer this test is about.
    await page.clock.install();
    await page.goto(`/issues/${source.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    const link = turns.getByRole("link", { name: "Hover target" });
    await expect(link).toBeVisible();
    await link.hover();
    const card = page.getByRole("tooltip");
    await expect(card).toBeVisible();
    await pauseClock(page);

    // The press lands on the text beside the reference, not on the reference: pressing the link
    // itself starts Chromium's native link drag, which stops delivering boundary events. The
    // press point is inside the turn's own collapsed button, so Chromium paints no selection
    // either - what this pins is the rule the handler reads, a pointer returning to the anchor
    // with a button held, whatever the drag means to the page.
    const box = await link.boundingBox();
    const cardBox = await card.boundingBox();
    if (box === null || cardBox === null) {
      throw new Error("reference or card is not visible");
    }
    const beside = { x: box.x - 8, y: box.y + box.height / 2 };
    await page.mouse.move(beside.x, beside.y);
    await page.mouse.down();
    await page.mouse.move(beside.x, box.y + box.height + 40, { steps: 4 });
    // Back up the same column and only then right onto the link. The final leg runs at the
    // anchor's own height, which the card never covers - it hangs 6 px below it - so the pointer
    // arrives at the anchor from outside the zone and the anchor's re-entry is what cancels the
    // close. The column check is this gesture's precondition rather than what makes the test
    // red: a card clamped left under the column (`PreviewCard` clamps one near the right edge)
    // would be entered and left again before the anchor, and leaving re-arms the close, so the
    // anchor would still have to cancel it.
    expect(cardBox.x).toBeGreaterThan(beside.x);
    await page.mouse.move(beside.x, beside.y, { steps: 4 });
    await page.mouse.move(box.x + box.width / 2, beside.y, { steps: 4 });
    await expect(card).toBeVisible();
    await page.clock.runFor(REF_PREVIEW_CLOSE_DELAY_MS * 2);
    await expect(card).toBeVisible();
    await page.mouse.up();
  } finally {
    await alice.close();
  }
});

test("a card survives a held-button move from the card back onto its reference", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name === "iphone", "a held mouse button is not a touch gesture");
  const { source } = await seedReference();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.clock.install();
    await page.goto(`/issues/${source.key}/conversation`);
    const turns = page.getByRole("list", { name: "Conversation turns" });
    const link = turns.getByRole("link", { name: "Hover target" });
    await expect(link).toBeVisible();
    await link.hover();
    const card = page.getByRole("tooltip");
    await expect(card).toBeVisible();
    await pauseClock(page);

    // The other end of the same zone. The press starts beside the reference, because pressing
    // the card's own link starts Chromium's native link drag and boundary events stop. Reaching
    // that point leaves the zone and arms a close, which the drag's entry onto the card cancels;
    // the step from the card back onto the reference is the crossing under test (`pointerout`
    // from the card naming the anchor, then `pointerover` on the anchor with the button held),
    // and that step must arm nothing.
    const box = await link.boundingBox();
    const cardBox = await card.boundingBox();
    if (box === null || cardBox === null) {
      throw new Error("reference or card is not visible");
    }
    const beside = { x: box.x - 8, y: box.y + box.height / 2 };
    await page.mouse.move(beside.x, beside.y);
    await page.mouse.down();
    await page.mouse.move(beside.x, cardBox.y + cardBox.height / 2, { steps: 4 });
    await page.mouse.move(box.x + box.width / 2, cardBox.y + cardBox.height / 2, { steps: 4 });
    await page.mouse.move(box.x + box.width / 2, cardBox.y + 4, { steps: 2 });
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.clock.runFor(REF_PREVIEW_CLOSE_DELAY_MS * 2);
    await expect(card).toBeVisible();
    await page.mouse.up();
  } finally {
    await alice.close();
  }
});

test("on a phone, tapping a reference follows it and never shows a card", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "touch geometry is covered by the iphone project");
  const { source, target } = await seedReference();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${source.key}/conversation`);
    const link = page
      .getByRole("list", { name: "Conversation turns" })
      .getByRole("link", { name: "Hover target" });
    await expect(link).toBeVisible();
    await link.tap();
    await expect(page).toHaveURL(new RegExp(`/issues/${target.key}(/|$)`));
    await expect(page.getByRole("tooltip")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
