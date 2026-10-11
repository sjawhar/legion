import { expect, type Page, type TestInfo, test } from "@playwright/test";

import { createAsk, createIssue, createProject, getAsk, setAgentStreamResponder } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox as a drawer over any page (LEGION-547): the same list and actions as the Inbox page,
 * mounted once in the shell so the page beneath it - a live conversation with a half-typed
 * message, an issue - keeps its state while an ask is answered and the drawer is closed.
 */

const session = {
  actor: { id: "e2e-inbox-drawer", kind: "session" as const },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

async function seedAsk(question: string): Promise<{ askId: string; issueKey: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Drawer issue" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question },
    session
  );
  return { askId: ask.id, issueKey: issue.key };
}

function drawer(page: Page) {
  return page.getByRole("dialog", { name: "Inbox" });
}

async function attachShot(page: Page, testInfo: TestInfo, name: string) {
  const path = testInfo.outputPath(`${name}-${testInfo.project.name}.png`);
  await page.screenshot({ path });
  await testInfo.attach(`${name} (${testInfo.project.name})`, { contentType: "image/png", path });
}

test("a half-typed message in a live conversation survives opening the drawer, answering an ask in it and closing it", async ({
  browser,
}, testInfo) => {
  const { askId } = await seedAsk("Ship the drawer behind a flag?");
  const sessionID = "01a0e0c1-0000-7000-8000-0000000d4a3e";
  await setAgentStreamResponder(sessionID, true);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sessionID}/live`);
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    const composer = page.getByTestId("agent-composer").locator("textarea");
    await composer.fill("half a thought about the rollout");
    // `i` is a page key, so it never fires from inside a text field.
    await composer.blur();

    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    const card = drawer(page).getByTestId(`ask-${askId}`);
    await expect(card).toBeVisible();
    await attachShot(page, testInfo, "inbox-drawer-over-agent-conversation");

    await card.getByRole("radio", { name: "Yes" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(card).toHaveCount(0);
    await expect
      .poll(() => getAsk(askId))
      .toMatchObject({ ask: { answer: { selected: ["Yes"], user: "alice" }, state: "answered" } });

    await page.keyboard.press("Escape");
    await expect(drawer(page)).toHaveCount(0);
    // The conversation underneath was never unmounted: same route, same draft.
    await expect(page).toHaveURL(new RegExp(`/agents/${sessionID}/live$`));
    await expect(composer).toHaveValue("half a thought about the rollout");
  } finally {
    await context.close();
  }
});

test("over an issue page the drawer opens from the header, keeps its page, and Escape returns focus to the button", async ({
  browser,
}, testInfo) => {
  const { askId, issueKey } = await seedAsk("Which label for the drawer?");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/issues/${issueKey}`);
    await expect(page.getByRole("heading", { name: "Drawer issue" })).toBeVisible();
    const trigger =
      testInfo.project.name === "iphone"
        ? page.getByRole("button", { name: /^Open inbox/ })
        : page
            .getByRole("navigation", { name: "Navigation" })
            .getByRole("button", { name: "Peek at the inbox without leaving this page" });
    await trigger.click();
    await expect(drawer(page).getByTestId(`ask-${askId}`)).toBeVisible();
    await expect(page).toHaveURL(new RegExp(`/issues/${issueKey}$`));
    await attachShot(page, testInfo, "inbox-drawer-over-issue");

    if (testInfo.project.name === "iphone") {
      // A phone gets the whole screen.
      const box = await drawer(page).boundingBox();
      const viewport = page.viewportSize();
      expect(box?.x).toBe(0);
      expect(box?.width).toBe(viewport?.width);
    }

    await page.keyboard.press("Escape");
    await expect(drawer(page)).toHaveCount(0);
    await expect(trigger).toBeFocused();
    await expect(page.getByRole("heading", { name: "Drawer issue" })).toBeVisible();

    if (testInfo.project.name !== "iphone") {
      // A click outside the panel closes it too.
      await trigger.click();
      await expect(drawer(page)).toBeVisible();
      await page.mouse.click(10, 10);
      await expect(drawer(page)).toHaveCount(0);
    }
  } finally {
    await context.close();
  }
});

test("`i` toggles the drawer, is listed in ?, and is not offered on the Inbox page itself", async ({
  browser,
}, testInfo) => {
  await seedAsk("Is the shortcut listed?");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    await attachShot(page, testInfo, "inbox-drawer-over-agents-list");
    await page.getByRole("button", { name: "Close" }).click();
    await expect(drawer(page)).toHaveCount(0);

    await page.keyboard.press("?");
    const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    const row = help.getByRole("listitem").filter({ hasText: "Open the inbox" });
    await expect(row.locator("kbd")).toHaveText(["i"]);
    await expect(row).toHaveAttribute("data-enabled", "true");
    await page.keyboard.press("Escape");

    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(1);
    await page.locator("body").focus();
    await page.keyboard.press("i");
    await expect(drawer(page)).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Open inbox/ })).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Peek at the inbox without leaving this page" })
    ).toHaveCount(0);
  } finally {
    await context.close();
  }
});
