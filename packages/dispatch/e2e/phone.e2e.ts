import { expect, type Page, test } from "@playwright/test";
import { setLiveSessions } from "./agents";

import {
  createAsk,
  createComment,
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
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(":scope > li", { hasText: text });
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
      await askCard.getByRole("button", { name: "Add a note or answer in your own words" }).click();
      await askCard.getByLabel("Your answer").fill("Yes.");
      await askCard.getByRole("button", { exact: true, name: "Answer" }).click();
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

test("inline @ autocomplete has touch-sized controls and no overflow on iPhone", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the 390 px interaction applies on iPhone");
  await setLiveSessions([
    {
      capabilities: ["aside", "btw"],
      dir: "/w/legion",
      last_seen: 1_700_000_000_000,
      roles: ["legion-planner"],
      session_id: "phone-planner",
      title: "planner",
    },
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Phone inline mentions" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const composer = page.getByRole("form", { name: "Comment composer" });
    await composer.getByLabel("Comment").fill("@");
    const picker = composer.getByRole("listbox", { name: "Mention suggestions" });
    await expect(picker).toBeVisible();
    const option = picker.getByRole("option", { exact: true, name: "planner" });
    await expect(option).toBeVisible();
    for (const control of await composer.locator("button:visible, textarea:visible").all()) {
      const box = await control.boundingBox();
      expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
    }
    await option.click();
    await composer.getByRole("button", { name: "Send" }).click();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
      )
    ).toBe(true);
  } finally {
    await alice.close();
  }
});

// An anchor inside a sentence is part of the sentence. The compact touch-target rule gave every
// anchor a 44px box, so a reply's `view` and a `dispatch://` reference inside a body became
// 44px-tall inline-flex boxes with blank space around them in the middle of a line.
test("an inline link keeps its line height and its 44px touch area", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Inline links" });
  const ask = await createAsk(
    issue.key,
    { question: "Which transport?" },
    {
      actor: { id: "e2e-session", kind: "session" },
      as: "agent",
    }
  );
  // A session's reply to an ask is an activity line - "<author> replied to … view · <time>" -
  // whose `view` is a link in the middle of a sentence.
  await createComment(
    issue.key,
    { ask_id: ask.id, body: "Still measuring." },
    { actor: { id: "e2e-session", kind: "session" }, as: "agent" }
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issue.key}/conversation`);
    const link = page.locator('[data-kind="activity"]').getByRole("link", { name: "view" }).first();
    await expect(link).toBeVisible();

    const box = await link.boundingBox();
    expect(box).not.toBeNull();
    // A line of body text, not a 44px control box: the link takes no layout space of its own.
    expect(box?.height ?? 0).toBeLessThanOrEqual(28);

    // Its touch area is an overlay at least 44px tall, centred on the link.
    const overlay = await link.evaluate((node) => {
      const after = getComputedStyle(node, "::after");
      return { minHeight: after.minHeight, position: after.position };
    });
    expect(overlay).toEqual({ minHeight: "44px", position: "absolute" });

    // And it really is hit-testable outside the link's own box: a tap below its last line,
    // where the text is not, still lands on the link.
    const hit = await page.evaluate(
      ([x, y]) => {
        const node = document.elementFromPoint(x, y);
        return node === null ? null : (node.closest("a")?.textContent ?? node.tagName);
      },
      [(box?.x ?? 0) + (box?.width ?? 0) / 2, (box?.y ?? 0) + (box?.height ?? 0) + 5]
    );
    expect(hit).toBe("view");
  } finally {
    await context.close();
  }
});
