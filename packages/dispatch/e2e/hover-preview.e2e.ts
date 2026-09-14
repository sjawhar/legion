import { expect, test } from "@playwright/test";

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

    // The hover bridge: the pointer can travel onto the card without closing it, and clicking
    // the card follows the same link.
    await card.hover();
    await expect(card).toBeVisible();
    await card.getByRole("link").click();
    await expect(page).toHaveURL(new RegExp(`/issues/${target.key}(/|$)`));
    await expect(page.getByRole("tooltip")).toHaveCount(0);
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
