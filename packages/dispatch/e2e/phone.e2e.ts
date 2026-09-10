import { expect, type Locator, type Page, test } from "@playwright/test";

import { createAsk, createIssue, createMessage, createProject, getAsk } from "./api";
import { enterEditMode } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-phone" },
  as: "agent" as const,
};
const initialMarkdown = "The quick brown fox jumps over the lazy dog";
const pinnedMessage = "Pin me before you forget";

async function selectEditorRange(editor: Locator, from: number, length: number): Promise<void> {
  await editor.click();
  await editor.press("Control+Home");
  for (let index = 0; index < from; index += 1) {
    await editor.press("ArrowRight");
  }
  for (let index = 0; index < length; index += 1) {
    await editor.press("Shift+ArrowRight");
  }
}

async function activeElementInside(page: Page, selector: string): Promise<boolean> {
  return page.evaluate((sel) => {
    const container = document.querySelector(sel);
    return container?.contains(document.activeElement) ?? false;
  }, selector);
}

async function assertTabTrapped(page: Page, selector: string): Promise<void> {
  for (let index = 0; index < 8; index += 1) {
    await page.keyboard.press("Tab");
    expect(await activeElementInside(page, selector)).toBe(true);
  }
  for (let index = 0; index < 8; index += 1) {
    await page.keyboard.press("Shift+Tab");
    expect(await activeElementInside(page, selector)).toBe(true);
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the phone shell traps focus, dismisses on Escape at the right nesting level, never wraps a key, and renders a pinned event's own description", async ({
  browser,
}, testInfo) => {
  const isPhone = testInfo.project.name === "iphone";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Cache invalidation issue with a long descriptive title",
  });
  await createMessage(issue.key, { body: pinnedMessage }, session);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();

    // D13: the phone drawer moves focus in, traps Tab in both directions, and restores
    // focus to Menu on Escape. D17: the issue key renders on a single line, never wrapping.
    if (isPhone) {
      await page.goto("/");
      const menuButton = page.getByRole("button", { name: "Open navigation" });
      await menuButton.click();
      const closeButton = page.getByRole("button", { name: "Close navigation" });
      await expect(closeButton).toBeFocused();
      await assertTabTrapped(page, '[aria-label="Navigation"]');

      const keySpan = page.getByText(issue.key, { exact: true });
      const [keyBox, lineHeight] = await Promise.all([
        keySpan.boundingBox(),
        keySpan.evaluate((element) => Number.parseFloat(getComputedStyle(element).lineHeight)),
      ]);
      expect(keyBox?.height ?? Number.POSITIVE_INFINITY).toBeLessThan(lineHeight * 1.5);

      await page.keyboard.press("Escape");
      await expect(closeButton).toBeHidden();
      await expect(menuButton).toBeFocused();
    }
    const openAsk = await createAsk(
      issue.key,
      { question: "Is this landing view ready?" },
      session
    );

    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("tab", { name: "Spec" })).toHaveAttribute("aria-selected", "true");
    if (isPhone) {
      const reviewToggle = page.getByRole("button", { name: "Open review panel (1 open ask)" });
      await expect(reviewToggle).toBeVisible();
      await reviewToggle.click();
      const askCard = page
        .getByRole("region", { name: "Needs you" })
        .getByTestId(`ask-${openAsk.id}`);
      await askCard.getByLabel("Your answer").fill("Yes.");
      await askCard.getByRole("button", { name: "Submit answer" }).click();
      await expect
        .poll(() => getAsk(openAsk.id))
        .toMatchObject({
          ask: { answer: { text: "Yes.", user: "alice" }, state: "answered" },
        });
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      ).toBe(true);
      await page.getByRole("button", { name: /Close review panel/ }).click();
    }
    await enterEditMode(page);
    const editor = page.getByRole("textbox", { name: "Document editor" });
    await expect(editor).toContainText(initialMarkdown);

    // D11: choosing Comment on a selection autofocuses the composer's body field.
    await selectEditorRange(editor, 10, 5);
    await page.getByRole("button", { exact: true, name: "Comment" }).click();
    const commentComposer = page.getByRole("form", { name: "Comment composer" });
    const commentBody = commentComposer.getByLabel("Comment");
    await expect(commentBody).toBeFocused();

    // D43: on phone the sheet the composer opened in also traps Tab.
    if (isPhone) {
      await assertTabTrapped(page, '[data-testid="margin-sheet"]');
      await commentBody.focus();
    }

    // D12: Ctrl+K opens a real dialog; Escape dismisses only the picker, restores focus to
    // the composer, and leaves the composer's own draft (and, on phone, the sheet) open.
    await commentBody.press("Control+k");
    const picker = page.getByRole("dialog", { name: "Reference picker" });
    await expect(picker).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(picker).toHaveCount(0);
    await expect(commentComposer).toBeVisible();
    await expect(commentBody).toBeFocused();

    // D11: Escape on a non-empty draft shows an inline "Discard draft?" affordance (not a
    // blocking confirm()); Escape again closes it. Only the composer closes — the phone
    // sheet (if any) stays open, because the composer's own Escape handler claims it first.
    await commentBody.fill("why?");
    await commentBody.press("Escape");
    await expect(page.getByText("Discard draft?")).toBeVisible();
    await expect(commentComposer).toBeVisible();
    await commentBody.press("Escape");
    await expect(commentComposer).toHaveCount(0);
    if (isPhone) {
      await expect(page.getByRole("button", { name: /Close review panel/ })).toBeVisible();
      await page.getByRole("button", { name: /Close review panel/ }).click();
    }

    // D25: a pinned log event renders its own description, never the literal "Event <seq>".
    await page.getByRole("tab", { name: "Log" }).click();
    await page
      .locator("article", { hasText: pinnedMessage })
      .getByRole("button", { name: "Pin" })
      .click();
    if (isPhone) {
      await page.getByRole("button", { name: /Open review panel/ }).click();
    }
    await page.getByRole("tab", { name: "Pinned" }).click();
    const margin = page.getByTestId("margin-sheet");
    await expect(margin.getByText(pinnedMessage)).toBeVisible();
    await expect(margin.getByText(/^Event \d+$/)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
