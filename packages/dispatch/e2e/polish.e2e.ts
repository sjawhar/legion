import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, patchIssue } from "./api";
import { filterPicker, pickFilterOption } from "./filters";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = { actor: { id: "e2e-polish", kind: "session" as const }, as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

test("project filters stay collapsed until needed and margin asks use the compact composer", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Polish the queue" });
  await patchIssue(issue.key, { labels: ["frontend"], status: "todo" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Which option should ship?" },
    session
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  if (testInfo.project.name === "chromium") {
    await page.setViewportSize({ height: 800, width: 1280 });
  }
  const viewport = testInfo.project.name === "chromium" ? "1280" : "390";

  try {
    await page.goto("/projects/CORE");
    const filters = page.getByRole("button", { name: "Filters · 0 active" });
    await expect(filters).toHaveAttribute("aria-expanded", "false");
    await expect(filterPicker(page, "Status")).toHaveCount(0);
    await page.screenshot({
      path: testInfo.outputPath(`polish-project-list-${viewport}.png`),
      fullPage: true,
    });

    await filters.click();
    await pickFilterOption(page, "Status", "Todo");
    await expect(page.getByRole("button", { name: "Remove Status: Todo filter" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Filters · 1 active" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    const boardCard = page.getByRole("article", { name: `${issue.key} Polish the queue` });
    await boardCard.scrollIntoViewIfNeeded();
    await expect(boardCard).toBeVisible();
    if (testInfo.project.name === "iphone") {
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
        )
      ).toBe(true);
    }
    await page.screenshot({
      path: testInfo.outputPath(`polish-project-board-${viewport}.png`),
      fullPage: true,
    });

    await page.goto("/");
    const inboxCard = page.getByTestId(`ask-${ask.id}`);
    await expect(inboxCard.getByRole("radio", { name: "Ship" })).toBeVisible();
    await expect(inboxCard.getByLabel("Your answer")).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath(`polish-inbox-${viewport}.png`),
      fullPage: true,
    });

    await page.goto(`/issues/${issue.key}`);
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open review panel (1 open ask)" }).click();
    }
    const marginCard = page.getByRole("region", { name: "Needs you" }).getByTestId(`ask-${ask.id}`);
    await expect(marginCard.getByRole("button", { name: "Ship" })).toBeVisible();
    await expect(marginCard.getByLabel("Your answer")).toBeHidden();
    await marginCard.getByText("Add a note or answer in your own words", { exact: true }).click();
    await expect(marginCard.getByLabel("Your answer")).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath(`polish-margin-${viewport}.png`),
      fullPage: true,
    });
    if (!process.env.PLAYWRIGHT_BASE_URL) {
      await setLiveSessions([]);
    }

    await page.goto("/agents");
    await expect(page.getByRole("region", { name: "Agents empty state" })).toBeVisible();
    await page.screenshot({
      path: testInfo.outputPath(`polish-agents-${viewport}.png`),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

test("an untitled session reads the same on the Agents page, the ask card, its followers, and the Conversation", async ({
  browser,
}) => {
  const sessionId = "e2e-label-session";
  const label = `session:${sessionId.slice(0, 8)}…`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "One label" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Same name everywhere?" },
    { actor: { id: sessionId, kind: "session" }, as: "agent" }
  );
  if (!process.env.PLAYWRIGHT_BASE_URL) {
    await setLiveSessions([{ session_id: sessionId, title: "" }]);
  }

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(page.getByRole("heading", { level: 2, name: label })).toBeVisible();

    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    await expect(card.getByRole("region", { name: "Followers" })).toContainText("Followed by 1");
    // The author line and the follower chip both carry the label — the same text twice.
    await expect(card.getByText(label, { exact: true })).toHaveCount(2);

    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.getByRole("region", { name: "Conversation" });
    await expect(conversation.getByTestId(`ask-${ask.id}`)).toContainText(label);
  } finally {
    await context.close();
  }
});
