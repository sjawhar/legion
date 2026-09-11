import { expect, type Page, test } from "@playwright/test";

import {
  createAsk,
  createIssue,
  createMessage,
  createProject,
  createProjectDocument,
  getAsk,
} from "./api";
import { actionBar, barAction, documentEditor, selectEditorText } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-phone" },
  as: "agent" as const,
};
const initialMarkdown =
  "The quick brown fox jumps over the lazy dog\n\n" +
  "| One | Two | Three | Four | Five | Six |\n" +
  "| --- | --- | --- | --- | --- | --- |\n" +
  "| 1 | 2 | 3 | 4 | 5 | 6 |";
const pinnedMessage = "Pin me before you forget";

function turn(page: Page, text: string) {
  return page.getByRole("list", { name: "Conversation turns" }).locator("li", { hasText: text });
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
    // focus to Menu on Escape. D17: the project key renders on a single line.
    if (isPhone) {
      await page.goto("/");
      const menuButton = page.getByRole("button", { name: "Open navigation" });
      await menuButton.click();
      const closeButton = page.getByRole("button", { name: "Close navigation" });
      await expect(closeButton).toBeFocused();
      await assertTabTrapped(page, '[aria-label="Navigation"]');

      const keySpan = page.getByText("CORE", { exact: true });
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
    await expect(page.getByRole("tabpanel", { name: "Spec" }).getByRole("article")).toContainText(
      "The quick brown fox jumps over the lazy dog"
    );
    await expect(documentEditor(page).locator("table")).toBeVisible();

    // D11: choosing Comment on a selection autofocuses the composer's body field.
    await selectEditorText(page, "brown");
    if (isPhone) {
      for (const button of await actionBar(page).getByRole("button").all()) {
        const box = await button.boundingBox();
        expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
        expect(box?.width ?? 0).toBeGreaterThanOrEqual(44);
      }
      expect(
        await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      ).toBe(true);
    }
    await barAction(page, "Comment");
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

    // D25: a pinned Conversation turn renders its own description, never the literal "Event <seq>".
    await page.getByRole("tab", { name: "Conversation" }).click();
    const pinnedTurn = turn(page, pinnedMessage);
    await pinnedTurn.hover();
    await pinnedTurn.getByRole("button", { name: "Pin" }).click();
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

test("a project document has no horizontal overflow, 44px controls, and a Comments margin sheet", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: "# Design notes\\n\\nProject document body.",
    name: "Design notes",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/design-notes");
    await expect(documentEditor(page)).toContainText("Project document body.");
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
      )
    ).toBe(true);
    if (testInfo.project.name === "iphone") {
      const controls = await page
        .getByRole("main")
        .locator("button:visible, a:visible, select:visible")
        .all();
      for (const control of controls) {
        const box = await control.boundingBox();
        expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
      }
      await page.getByRole("button", { name: /Open review panel/ }).click();
      await expect(page.getByRole("tab", { name: "Comments" })).toBeVisible();
      await expect(page.getByRole("tab", { name: "Pinned" })).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});
