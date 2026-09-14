import { expect, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, replyToMessageDelivery } from "./api";
import { recordClipboard } from "./clipboard";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const planner: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/workspaces/planner",
  last_seen: Date.now() - 30_000,
  machine_id: "planner-host",
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};
const reviewer: FakeSession = {
  capabilities: ["aside"],
  dir: "/workspaces/reviewer",
  last_seen: Date.now() - 5 * 60_000,
  machine_id: "reviewer-host",
  roles: ["reviewer"],
  session_id: "reviewer-session",
  title: "Reviewer",
};

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("Agents collapses cards, orders activity, pins a card, copies identifiers, and holds an issue-less BTW conversation", async ({
  browser,
}, testInfo) => {
  await setLiveSessions([planner, reviewer]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agent page activity" });
  await createAsk(
    issue.key,
    { question: "First" },
    { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const copied = await recordClipboard(page);
    await page.goto("/agents");
    const width = testInfo.project.name === "iphone" ? "390" : "1280";

    const agents = page.getByRole("region", { name: "Agents" });
    if (testInfo.project.name === "chromium") {
      await expect(page.getByRole("link", { name: "Agents", exact: true })).toBeVisible();
    }
    const plannerCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
    const reviewerCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Reviewer" }) });
    await expect(plannerCard).toBeVisible();
    await expect(reviewerCard).toBeVisible();
    await expect(agents.getByText("Open asks 1", { exact: true })).toBeVisible();
    await expect(
      plannerCard.getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeVisible();

    // Collapsed by default: no conversation or composer until a card's title is expanded.
    await expect(agents.getByRole("textbox", { name: "Message" })).toHaveCount(0);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-collapsed-${width}.png`),
    });
    const reviewerToggle = reviewerCard.getByRole("button", { exact: true, name: "Reviewer" });
    await expect(reviewerToggle).toHaveAttribute("aria-expanded", "false");
    await reviewerToggle.click();
    await expect(reviewerToggle).toHaveAttribute("aria-expanded", "true");
    await expect(reviewerCard.getByRole("button", { name: "BTW", exact: true })).toBeDisabled();
    await expect(reviewerCard.getByRole("button", { name: "Aside", exact: true })).toBeEnabled();
    await expect(reviewerCard.getByRole("button", { name: "Steer", exact: true })).toBeEnabled();
    await expect(plannerCard.getByRole("textbox", { name: "Message" })).toHaveCount(0);
    await reviewerToggle.click();
    await expect(reviewerCard.getByRole("textbox", { name: "Message" })).toHaveCount(0);

    // The identifiers copy from the collapsed row.
    await plannerCard.getByRole("button", { name: "Copy session ID planner-session" }).click();
    await expect(plannerCard.getByText("Copied", { exact: true })).toBeVisible();
    await plannerCard.getByRole("button", { name: "Copy session title Planner" }).click();
    await expect.poll(copied).toEqual(["planner-session", "Planner"]);

    // The ask pills link to the Inbox narrowed to this agent; zero counts stay text.
    await expect(plannerCard.getByRole("link", { name: "Needs you 1" })).toHaveAttribute(
      "href",
      "/?agent=planner-session&section=needs-you"
    );
    await expect(reviewerCard.getByRole("link")).toHaveCount(0);
    await expect(reviewerCard.getByText("Open asks 0", { exact: true })).toBeVisible();

    await reviewerCard.getByRole("button", { name: "Pin Reviewer" }).click();
    await expect(reviewerCard.getByRole("button", { name: "Unpin Reviewer" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(page.locator("article h2").allTextContents()).resolves.toEqual([
      "Reviewer",
      "Planner",
    ]);

    const plannerToggle = plannerCard.getByRole("button", { exact: true, name: "Planner" });
    await plannerToggle.click();
    await expect(plannerCard.getByRole("button", { name: "BTW", exact: true })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(plannerCard).toContainText("Ctrl/Cmd+Enter to send · Enter for a new line");
    const body = "Please inspect the current implementation.";
    const sentBody = `${body}\n`;
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/agents/${planner.session_id}/messages`) &&
        response.status() === 201
    );
    const composer = plannerCard.getByRole("textbox", { name: "Message" });
    await composer.fill(body);
    await composer.press("Enter");
    await expect(composer).toHaveValue(sentBody);
    await composer.press("Control+Enter");
    const request = await sent;
    expect(request.request().postDataJSON()).toEqual({ body: sentBody, delivery: "btw" });
    const message = (await request.json()) as { id: string };
    expect(await getSentMessages()).toMatchObject([{ target_session: planner.session_id }]);

    await replyToMessageDelivery(
      message.id,
      { attempt: 1, body: "The implementation is ready." },
      { id: planner.session_id, kind: "session" }
    );
    const conversation = plannerCard.getByRole("list", { name: "Conversation with Planner" });
    await expect(conversation).toContainText(body);
    await expect(conversation).toContainText("The implementation is ready.");

    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-expanded-${width}.png`),
    });

    await plannerCard.getByRole("link", { name: "Open asks 1" }).click();
    await expect(page).toHaveURL(/\/\?agent=planner-session$/);
    const inbox = page.locator("main");
    const chip = inbox.getByRole("link", { name: "Clear agent filter" });
    await expect(chip).toHaveText("Asks from Planner · clear");
    await expect(inbox.getByText("First", { exact: true })).toBeVisible();
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`inbox-agent-filter-${width}.png`),
    });
    await chip.click();
    await expect(page).toHaveURL(/\/$/);
    await expect(inbox.getByRole("link", { name: "Clear agent filter" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
