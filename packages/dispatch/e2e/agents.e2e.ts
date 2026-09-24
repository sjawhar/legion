import { expect, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  disconnectAllStreams,
  putAgentState,
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
const silent: FakeSession = {
  capabilities: ["aside", "btw"],
  dir: "/workspaces/silent",
  last_seen: Date.now() - 10_000,
  machine_id: "silent-host",
  roles: [],
  session_id: "silent-session",
  title: "Silent",
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

test("Agents puts who needs you first, folds silent and inactive sessions, shows one whose-turn pill per side, pins a card, copies identifiers, and holds an issue-less BTW conversation", async ({
  browser,
}, testInfo) => {
  await setLiveSessions([planner, reviewer, silent, archivist]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agent page activity" });
  const asPlanner = {
    actor: { id: planner.session_id, kind: "session" as const },
    as: "agent" as const,
  };
  await createAsk(issue.key, { question: "First" }, asPlanner);
  // Alice has replied to the second ask, so its next move is the Planner's.
  const answered = await createAsk(issue.key, { question: "Second" }, asPlanner);
  await createComment(issue.key, { ask_id: answered.id, body: "Which release?" });
  // The Reviewer's only Dispatch activity is newer than anything the Planner did: recency alone
  // would list it first.
  await createComment(
    issue.key,
    { body: "Reviewing the diff." },
    { actor: { id: reviewer.session_id, kind: "session" }, as: "agent" }
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

    // Who needs you comes first, whatever Dispatch heard most recently; a live session Dispatch
    // never heard from and a session unseen for ten minutes each sit under a collapsed fold.
    const silentCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Silent" }) });
    const archivistCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Archivist" }) });
    const quietToggle = agents.getByRole("button", { name: "No Dispatch activity (1)" });
    const inactiveToggle = agents.getByRole("button", { name: "Inactive (1)" });
    await expect(quietToggle).toHaveAttribute("aria-expanded", "false");
    await expect(inactiveToggle).toHaveAttribute("aria-expanded", "false");
    await expect(silentCard).toHaveCount(0);
    await expect(archivistCard).toHaveCount(0);
    await expect(page.locator("article h2").allTextContents()).resolves.toEqual([
      "Planner",
      "Reviewer",
    ]);
    // Each collapsed fold owns its own row: the second toggle starts below the first one, even
    // on the phone where the global inline-flex button rule would otherwise line them up.
    const quietBox = await quietToggle.boundingBox();
    const inactiveBox = await inactiveToggle.boundingBox();
    if (quietBox === null || inactiveBox === null) throw new Error("fold toggles not laid out");
    expect(inactiveBox.y).toBeGreaterThanOrEqual(quietBox.y + quietBox.height);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-folded-${width}.png`),
    });

    await quietToggle.click();
    await expect(quietToggle).toHaveAttribute("aria-expanded", "true");
    const quiet = agents.getByRole("region", { name: "No Dispatch activity" });
    await expect(quiet.locator("article h2")).toHaveText(["Silent"]);
    await expect(silentCard.getByText("No Dispatch activity", { exact: true })).toBeVisible();
    await expect(
      silentCard.getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeVisible();
    await quietToggle.click();
    await expect(silentCard).toHaveCount(0);

    await inactiveToggle.click();
    await expect(inactiveToggle).toHaveAttribute("aria-expanded", "true");
    const inactive = agents.getByRole("region", { name: "Inactive" });
    await expect(inactive.locator("article h2")).toHaveText(["Archivist"]);
    await expect(
      archivistCard.getByRole("status", { name: "Seen 10 minutes ago or longer" })
    ).toBeVisible();
    await archivistCard.getByRole("button", { exact: true, name: "Archivist" }).click();
    await expect(archivistCard.getByRole("textbox", { name: "Comment" })).toBeVisible();
    await inactiveToggle.click();
    await expect(archivistCard).toHaveCount(0);
    await expect(
      plannerCard.getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeVisible();

    // Collapsed by default: no conversation or composer until a card's title is expanded.
    await expect(agents.getByRole("textbox", { name: "Comment" })).toHaveCount(0);
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-collapsed-${width}.png`),
    });
    const reviewerToggle = reviewerCard.getByRole("button", { exact: true, name: "Reviewer" });
    await expect(reviewerToggle).toHaveAttribute("aria-expanded", "false");
    await reviewerToggle.click();
    await expect(reviewerToggle).toHaveAttribute("aria-expanded", "true");
    await expect(reviewerCard.getByRole("form", { name: "Comment composer" })).toBeVisible();
    await expect(plannerCard.getByRole("textbox", { name: "Comment" })).toHaveCount(0);
    await reviewerToggle.click();
    await expect(reviewerCard.getByRole("textbox", { name: "Comment" })).toHaveCount(0);

    // The identifiers copy from the collapsed row.
    await plannerCard.getByRole("button", { name: "Copy session ID planner-session" }).click();
    await expect(plannerCard.getByText("Copied", { exact: true })).toBeVisible();
    await plannerCard.getByRole("button", { name: "Copy session title Planner" }).click();
    await expect.poll(copied).toEqual(["planner-session", "Planner"]);

    // One pill per side of the turn: the asks waiting on the viewer, then the asks the viewer
    // has replied to; a card with no open asks shows neither.
    const needsYou = plannerCard.getByRole("link", { name: "Needs you 1" });
    await expect(needsYou).toHaveAttribute("href", "/?agent=planner-session&section=needs-you");
    const waitingOnAgent = plannerCard.getByRole("link", { name: "Waiting on agent 1" });
    await expect(waitingOnAgent).toHaveAttribute("href", "/?agent=planner-session");
    await expect(plannerCard.getByText(/^Open asks/)).toHaveCount(0);
    await expect(reviewerCard.getByRole("link")).toHaveCount(0);
    await expect(reviewerCard.getByText(/^(Needs you|Waiting on agent|Open asks)/)).toHaveCount(0);

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
    await expect(plannerCard.getByRole("form", { name: "Comment composer" })).toBeVisible();
    await expect(plannerCard).toContainText("Ctrl/Cmd+Enter to send · Enter for a new line");
    const body = "Please inspect the current implementation.";
    const sentBody = `${body}\n`;
    const sent = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        response.url().endsWith(`/api/v1/agents/${planner.session_id}/messages`) &&
        response.status() === 201
    );
    const composer = plannerCard.getByRole("textbox", { name: "Comment" });
    await composer.fill(`/btw ${body}`);
    await composer.press("Enter");
    await expect(composer).toHaveValue(`/btw ${sentBody}`);
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

    await waitingOnAgent.click();
    await expect(page).toHaveURL(/\/\?agent=planner-session$/);
    const inbox = page.locator("main");
    const chip = inbox.getByRole("link", { name: "Clear agent filter" });
    await expect(chip).toHaveText("Asks from Planner · clear");
    await expect(inbox.getByText("First", { exact: true })).toBeVisible();
    await expect(inbox.getByText("Second", { exact: true })).toBeVisible();
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
    const composer = plannerCard.getByRole("textbox", { name: "Comment" });
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

test("a session's untargeted reply lands in the open conversation", async ({ browser }) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Untargeted reply" });
  // Dispatch activity of its own, so the card sits in the open list rather than behind the
  // "No Dispatch activity" fold.
  await createComment(
    issue.key,
    { body: "Looking at it." },
    { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
  );
  const root = await createMessage(issue.key, {
    body: "Can this ship?",
    delivery: "btw",
    target: `session:${planner.session_id}`,
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const plannerCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
    await plannerCard.getByRole("button", { exact: true, name: "Planner" }).click();
    const conversation = plannerCard.getByRole("list", { name: "Conversation with Planner" });
    await expect(conversation).toContainText("Can this ship?");

    // The session answers on the issue with `in_reply_to` and no target: the target would be
    // itself, so the event names no session of its own and only the thread root does.
    await createMessage(
      issue.key,
      { body: "Once the build is green.", in_reply_to: root.id },
      { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
    );
    await expect(conversation).toContainText("Once the build is green.");
  } finally {
    await alice.close();
  }
});

test("a reconnect picks up a Clear made from another device", async ({ browser }) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Cleared elsewhere" });
  await createComment(
    issue.key,
    { body: "Looking at it." },
    { actor: { id: planner.session_id, kind: "session" }, as: "agent" }
  );
  await createMessage(issue.key, {
    body: "Can this ship?",
    delivery: "btw",
    target: `session:${planner.session_id}`,
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const plannerCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
    await plannerCard.getByRole("button", { exact: true, name: "Planner" }).click();
    const conversation = plannerCard.getByRole("list", { name: "Conversation with Planner" });
    await expect(conversation).toContainText("Can this ship?");

    // Another of alice's devices clears the conversation. No event describes her own per-agent
    // state, so this tab learns it only when the stream reopens - and a reconnect has to
    // refresh every query the app holds, not the ones some hand-written list remembered.
    await putAgentState(planner.session_id, { cleared_before: new Date().toISOString() });
    await disconnectAllStreams();
    await expect(plannerCard.getByRole("button", { name: "Show anyway" })).toBeVisible();
    await expect(conversation).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
