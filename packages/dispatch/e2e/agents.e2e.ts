import { expect, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  createAsk,
  createIssue,
  createProject,
  replyToMessageDelivery,
} from "./api";
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
const archivist: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/workspaces/archivist",
  last_seen: Date.now() - 45 * 60_000,
  machine_id: "archivist-host",
  roles: [],
  session_id: "archivist-session",
  title: "Archivist",
};

test.beforeEach(async () => {
  await Promise.all([resetDatabase(), setLiveSessions([])]);
});

test("Agents collapses cards, orders activity, folds inactive sessions, pins a card, copies identifiers, and holds an issue-less BTW conversation", async ({
  browser,
}, testInfo) => {
  await setLiveSessions([planner, reviewer, archivist]);
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

    // A session unseen for ten minutes sits under the collapsed Inactive disclosure.
    const archivistCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Archivist" }) });
    const inactiveToggle = agents.getByRole("button", { name: "Inactive (1)" });
    await expect(inactiveToggle).toHaveAttribute("aria-expanded", "false");
    await expect(archivistCard).toHaveCount(0);
    await expect(page.locator("article h2").allTextContents()).resolves.toEqual([
      "Planner",
      "Reviewer",
    ]);
    await inactiveToggle.click();
    await expect(inactiveToggle).toHaveAttribute("aria-expanded", "true");
    const inactive = agents.getByRole("region", { name: "Inactive" });
    await expect(inactive.locator("article h2")).toHaveText(["Archivist"]);
    await expect(
      archivistCard.getByRole("status", { name: "Seen 10 minutes ago or longer" })
    ).toBeVisible();
    await archivistCard.getByRole("button", { exact: true, name: "Archivist" }).click();
    await expect(archivistCard.getByRole("button", { name: "BTW", exact: true })).toBeEnabled();
    await inactiveToggle.click();
    await expect(archivistCard).toHaveCount(0);
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

test("Agents shows the newest exchange, folds the older ones, and lets the viewer clear the conversation", async ({
  browser,
}, testInfo) => {
  await setLiveSessions([planner]);
  const plannerActor = { id: planner.session_id, kind: "session" as const };
  const first = await createAgentMessage(planner.session_id, {
    body: "First question",
    delivery: "btw",
  });
  await replyToMessageDelivery(first.id, { attempt: 1, body: "First answer" }, plannerActor);
  await createAgentMessage(planner.session_id, { body: "Second question", delivery: "btw" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const width = testInfo.project.name === "iphone" ? "390" : "1280";
    const plannerCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
    const conversation = plannerCard.getByRole("list", { name: "Conversation with Planner" });
    const expand = async () => {
      await plannerCard.getByRole("button", { exact: true, name: "Planner" }).click();
    };
    await expand();

    // Only the newest exchange is open; the rest sit behind one fold.
    await expect(conversation).toContainText("Second question");
    await expect(conversation).not.toContainText("First question");
    const older = plannerCard.getByRole("button", { name: "Show 1 older" });
    await expect(older).toHaveAttribute("aria-expanded", "false");
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-fold-${width}.png`),
    });
    await older.click();
    await expect(older).toHaveAttribute("aria-expanded", "true");
    await expect(conversation).toContainText("First question");
    await expect(conversation).toContainText("First answer");
    await older.click();
    await expect(conversation).not.toContainText("First question");

    // Clear hides everything so far for this viewer and keeps the cutoff on the server.
    const clearButton = plannerCard.getByRole("button", { name: "Clear conversation" });
    if (testInfo.project.name === "iphone") {
      const box = await clearButton.boundingBox();
      expect(box?.height ?? 0).toBeGreaterThanOrEqual(44);
    }
    const saved = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        response.url().endsWith(`/api/v1/me/agents/${planner.session_id}/state`) &&
        response.ok()
    );
    await clearButton.click();
    const state = (await (await saved).json()) as { cleared_before: string };
    expect(Date.parse(state.cleared_before)).toBeLessThanOrEqual(Date.now());
    await expect(conversation).toHaveCount(0);
    await expect(clearButton).toHaveCount(0);
    await expect(plannerCard.getByText(/^Cleared/)).toContainText("just now");
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-cleared-${width}.png`),
    });

    // Looking back is a local toggle; the history is intact and folded as before.
    const showAnyway = plannerCard.getByRole("button", { name: "Show anyway" });
    await showAnyway.click();
    await expect(conversation).toContainText("Second question");
    await expect(plannerCard.getByRole("button", { name: "Show 1 older" })).toBeVisible();
    await plannerCard.getByRole("button", { name: "Hide again" }).click();
    await expect(conversation).toHaveCount(0);

    // A message after the Clear is news and renders normally; the cleared ones stay hidden.
    const composer = plannerCard.getByRole("textbox", { name: "Message" });
    await composer.fill("Third question");
    await composer.press("Control+Enter");
    await expect(conversation).toContainText("Third question");
    await expect(conversation).not.toContainText("Second question");
    await expect(plannerCard.getByRole("button", { name: /older$/ })).toHaveCount(0);
    await expect(showAnyway).toBeVisible();
    await expect(clearButton).toBeVisible();

    // The cutoff follows the viewer: a fresh load (another device) sees the same view.
    await page.reload();
    await expand();
    await expect(conversation).toContainText("Third question");
    await expect(conversation).not.toContainText("Second question");
    await expect(showAnyway).toBeVisible();
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-after-clear-${width}.png`),
    });
  } finally {
    await alice.close();
  }
});
