import { expect, type Locator, type Page, test } from "@playwright/test";
import type { Message } from "../web/src/api/types";
import {
  agentRow,
  type FakeSession,
  getSentMessages,
  postedBroadcasts,
  refuseBroadcasts,
  setLiveSessions,
  setSessionLive,
  shownAgentRows,
} from "./agents";
import {
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  disconnectAllStreams,
  getAgentStates,
  listAgentMessages,
  putAgentState,
  replyToMessageDelivery,
} from "./api";
import { recordClipboard } from "./clipboard";
import { resetDatabase, setCreatedAt } from "./seed";
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
    // Each card is its row by session, shown or hidden, so every assertion below says which.
    const plannerCard = agentRow(page, planner.session_id);
    const reviewerCard = agentRow(page, reviewer.session_id);
    await expect(plannerCard).toBeVisible();
    await expect(reviewerCard).toBeVisible();

    // Who needs you comes first, whatever Dispatch heard most recently; a live session Dispatch
    // never heard from and a session unseen for ten minutes each sit under a collapsed fold.
    const silentCard = agentRow(page, silent.session_id);
    const archivistCard = agentRow(page, archivist.session_id);
    const quietToggle = agents.getByRole("button", { name: "No Dispatch activity (1)" });
    const inactiveToggle = agents.getByRole("button", { name: "Inactive (1)" });
    await expect(quietToggle).toHaveAttribute("aria-expanded", "false");
    await expect(inactiveToggle).toHaveAttribute("aria-expanded", "false");
    // A closed fold's rows stay in the one list, mounted and hidden, not gone: `toBeHidden` alone
    // would also pass for a row that is not there, so each is counted too.
    await expect(silentCard).toHaveCount(1);
    await expect(silentCard).toBeHidden();
    await expect(archivistCard).toHaveCount(1);
    await expect(archivistCard).toBeHidden();
    const shownTitles = shownAgentRows(page).locator("h2");
    await expect(shownTitles).toHaveText(["Planner", "Reviewer"]);
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
    await expect(shownTitles).toHaveText(["Planner", "Reviewer", "Silent"]);
    await expect(silentCard.getByText("No Dispatch activity", { exact: true })).toBeVisible();
    await expect(
      silentCard.getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeVisible();
    await quietToggle.click();
    await expect(silentCard).toHaveCount(1);
    await expect(silentCard).toBeHidden();

    await inactiveToggle.click();
    await expect(inactiveToggle).toHaveAttribute("aria-expanded", "true");
    await expect(shownTitles).toHaveText(["Planner", "Reviewer", "Archivist"]);
    await expect(
      archivistCard.getByRole("status", { name: "Seen 10 minutes ago or longer" })
    ).toBeVisible();
    await archivistCard.getByRole("button", { exact: true, name: "Archivist" }).click();
    await expect(archivistCard.getByRole("textbox", { name: "Comment" })).toBeVisible();
    await inactiveToggle.click();
    await expect(archivistCard).toHaveCount(1);
    await expect(archivistCard).toBeHidden();
    await expect(
      plannerCard.getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeVisible();

    // Collapsed by default: no conversation or composer on screen until a card's title is
    // expanded. The Archivist's, opened above and folded away with its row, is kept - with any
    // draft in it - hidden; no card that was never opened has one at all.
    const composers = agents.getByRole("textbox", { includeHidden: true, name: "Comment" });
    await expect(composers).toHaveCount(1);
    const archivistComposer = archivistCard.getByRole("textbox", {
      includeHidden: true,
      name: "Comment",
    });
    await expect(archivistComposer).toHaveCount(1);
    await expect(archivistComposer).toBeHidden();
    await page.screenshot({
      fullPage: true,
      path: testInfo.outputPath(`agents-collapsed-${width}.png`),
    });
    const reviewerToggle = reviewerCard.getByRole("button", { exact: true, name: "Reviewer" });
    await expect(reviewerToggle).toHaveAttribute("aria-expanded", "false");
    await reviewerToggle.click();
    await expect(reviewerToggle).toHaveAttribute("aria-expanded", "true");
    await expect(reviewerCard.getByRole("form", { name: "Comment composer" })).toBeVisible();
    await expect(
      plannerCard.getByRole("textbox", { includeHidden: true, name: "Comment" })
    ).toHaveCount(0);
    await reviewerToggle.click();
    // A card opened once keeps its composer - and its draft - when it collapses, hidden.
    const reviewerComposer = reviewerCard.getByRole("textbox", {
      includeHidden: true,
      name: "Comment",
    });
    await expect(reviewerComposer).toHaveCount(1);
    await expect(reviewerComposer).toBeHidden();

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
    // Every card carries the Open action; what this card must not carry is a whose-turn pill.
    await expect(
      reviewerCard.getByRole("link", { name: /Needs you|Waiting on agent/ })
    ).toHaveCount(0);
    await expect(reviewerCard.getByText(/^(Needs you|Waiting on agent|Open asks)/)).toHaveCount(0);

    await reviewerCard.getByRole("button", { name: "Pin Reviewer" }).click();
    await expect(reviewerCard.getByRole("button", { name: "Unpin Reviewer" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(shownTitles).toHaveText(["Reviewer", "Planner"]);

    const plannerToggle = plannerCard.getByRole("button", { exact: true, name: "Planner" });
    await plannerToggle.click();
    await expect(plannerCard.getByRole("form", { name: "Comment composer" })).toBeVisible();
    // Until a message is typed the hint's line says why Send waits; then it says how to send.
    await expect(plannerCard).toContainText("Type a message first.");
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
    await expect(plannerCard).toContainText("Ctrl/Cmd+Enter to send · Enter for a new line");
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
  const firstAnswer = (await replyToMessageDelivery(
    first.id,
    { attempt: 1, body: "First answer" },
    plannerActor
  )) as Message;
  await createAgentMessage(planner.session_id, { body: "Second question", delivery: "btw" });
  // Alice has read the first answer (on another device, say): an exchange holding an unread
  // reply opens unfolded, so the fold is shown over one already read.
  await putAgentState(planner.session_id, { read_through: firstAnswer.created_at });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const width = testInfo.project.name === "iphone" ? "390" : "1280";
    const plannerCard = agentRow(page, planner.session_id);
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
    const plannerCard = agentRow(page, planner.session_id);
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
    const plannerCard = agentRow(page, planner.session_id);
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

// The server's times carry microseconds. Opening a row marks read through the newest reply, so of
// two replies written in one millisecond it names the newer, or that one would stay unread.
test("opening a row marks read the newer of two replies written in one millisecond", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  const plannerActor = { id: planner.session_id, kind: "session" as const };
  const askA = await createAgentMessage(planner.session_id, {
    body: "Question A",
    delivery: "aside",
  });
  const answerA = (await replyToMessageDelivery(
    askA.id,
    { attempt: 1, body: "Answer A" },
    plannerActor
  )) as Message;
  const askB = await createAgentMessage(planner.session_id, {
    body: "Question B",
    delivery: "aside",
  });
  const answerB = (await replyToMessageDelivery(
    askB.id,
    { attempt: 1, body: "Answer B" },
    plannerActor
  )) as Message;
  // Alice's follow-up moves exchange A above B in the server's order, so the page meets the older
  // reply first: a comparison to the millisecond would keep it.
  const followUp = await createAgentMessage(planner.session_id, {
    body: "Follow-up on A",
    delivery: "aside",
    in_reply_to: answerA.id,
  });
  const second = Math.floor((Date.now() - 120_000) / 1000) * 1000;
  const at = (offsetSeconds: number, fraction = "") =>
    `${new Date(second + offsetSeconds * 1000).toISOString().slice(0, 19)}${fraction}Z`;
  const replyA = at(0, ".123456");
  const replyB = at(0, ".1235");
  await setCreatedAt("messages", askA.id, at(-60));
  await setCreatedAt("messages", askB.id, at(-30));
  await setCreatedAt("messages", answerA.id, replyA);
  await setCreatedAt("messages", answerB.id, replyB);
  await setCreatedAt("messages", followUp.id, at(30));
  // The fixture holds only if the server lists A first, with both replies unread.
  expect(
    (await listAgentMessages(planner.session_id)).map((read) => [
      read.message.body,
      read.replies
        .filter((reply) => reply.author.id === planner.session_id)
        .map((reply) => reply.created_at),
      read.unread,
    ])
  ).toEqual([
    ["Question A", [replyA], true],
    ["Question B", [replyB], true],
  ]);
  expect((await getAgentStates())[planner.session_id]?.unread_replies).toBe(2);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(page.getByText("New replies 2").first()).toBeVisible();
    const plannerCard = page
      .locator("article")
      .filter({ has: page.getByRole("heading", { level: 2, name: "Planner" }) });
    const marked = page.waitForRequest(
      (request) =>
        request.method() === "PUT" &&
        request.url().endsWith(`/api/v1/me/agents/${planner.session_id}/state`)
    );
    await plannerCard.getByRole("button", { name: "Planner replied: 2 unread" }).click();
    expect((await marked).postDataJSON()).toEqual({ read_through: replyB });
    await expect(
      plannerCard.getByRole("list", { name: "Conversation with Planner" })
    ).toContainText("Answer B");
    await expect(page.getByText(/^New repl/)).toHaveCount(0);
    await expect
      .poll(async () => (await getAgentStates())[planner.session_id]?.unread_replies)
      .toBe(0);
  } finally {
    await alice.close();
  }
});

test("the header checkbox selects and clears only the rows the filters match, by pointer and by Space", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the selection rule");
  // No `last_seen`: the fixture answers every read with the current time, so none of them ages
  // into Inactive.
  const session = (id: string, title: string, role: string): FakeSession => ({
    capabilities: ["aside", "btw"],
    dir: `/workspaces/${id}`,
    machine_id: "build-host",
    roles: [role],
    session_id: `${id}-session`,
    title,
  });
  await setLiveSessions([
    session("planner-a", "Planner A", "planner"),
    session("planner-b", "Planner B", "planner"),
    session("reviewer", "Reviewer", "reviewer"),
    session("tester", "Tester", "tester"),
  ]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const header = agents.getByRole("checkbox", { name: "Select all matching agents" });
    const row = (title: string) =>
      agents.getByRole("checkbox", { name: `Select ${title} for broadcast` });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const chips = composer.getByRole("list", { name: "Selected agents" }).getByRole("button");
    // The count is the header checkbox's own description, so these reads are exact and cannot
    // match the composer's `Broadcast to X of Y selected` heading.
    const count = (text: string) => expect(header).toHaveAccessibleDescription(text);

    // Every session is silent, so every row is under the collapsed fold, and the header still
    // counts them: matching means what the filters match, folded or not. Selecting them says so
    // on the fold, so nothing is picked out of sight.
    await count("4 matching");
    await expect(agents.getByRole("button", { name: /^Select all/ })).toHaveCount(0);
    await header.click();
    await count("4 of 4 matching selected");
    await expect(
      agents.getByRole("button", { name: "No Dispatch activity (4, 4 selected)" })
    ).toBeVisible();
    await agents.getByRole("button", { name: "Clear selection" }).click();
    await count("4 matching");
    await agents.getByRole("button", { name: "No Dispatch activity (4)" }).click();

    // A row ticked under no filter, which the role filter below hides.
    await row("Reviewer").check();
    await expect(
      agents.getByRole("button", { name: "No Dispatch activity (4, 1 selected)" })
    ).toBeVisible();
    await agents.getByRole("combobox", { name: "Role" }).selectOption("planner");
    await expect(row("Reviewer")).toBeHidden();
    await count("2 matching · 1 selected outside the filter");
    await expect(header).not.toBeChecked();

    await header.click();
    // The send set is the two matching planners plus the hand-ticked Reviewer, never the Tester
    // the filter hides: a select-all over the unfiltered list would read `Send to 4` here.
    await expect(composer.getByRole("button", { name: "Send to 3" })).toBeVisible();
    await expect(row("Planner A")).toBeChecked();
    await expect(row("Planner B")).toBeChecked();
    await expect(header).toBeChecked();
    await count("2 of 2 matching selected · 1 more selected outside the filter");
    await expect(chips).toHaveText(["Reviewer ✕", "Planner A ✕", "Planner B ✕"]);

    await row("Planner B").uncheck();
    await expect(header).toBeChecked({ indeterminate: true });
    await expect(header).toHaveJSProperty("indeterminate", true);
    await count("1 of 2 matching selected · 1 more selected outside the filter");

    // Space on the focused header toggles it like a click: mixed becomes all matching.
    await header.focus();
    await page.keyboard.press("Space");
    await expect(header).toBeChecked();
    await expect(row("Planner B")).toBeChecked();
    await count("2 of 2 matching selected · 1 more selected outside the filter");

    // Once more clears the matching rows and nothing else: the Reviewer stays selected and named.
    await page.keyboard.press("Space");
    await expect(header).not.toBeChecked();
    await expect(row("Planner A")).not.toBeChecked();
    await expect(row("Planner B")).not.toBeChecked();
    await count("2 matching · 1 selected outside the filter");
    await expect(chips).toHaveText(["Reviewer ✕"]);

    // Clear selection takes the hidden row too.
    await agents.getByRole("button", { name: "Clear selection" }).click();
    await expect(composer).toHaveCount(0);

    // Build the filtered selection again and send it. The set the server receives is pinned by
    // name, not by the `Send to N` count the page computes: the matching planners plus the
    // hand-ticked Reviewer, never the Tester the filter hides.
    await agents.getByRole("combobox", { name: "Role" }).selectOption("");
    await row("Reviewer").check();
    await agents.getByRole("combobox", { name: "Role" }).selectOption("planner");
    await header.click();
    await expect(chips).toHaveText(["Reviewer ✕", "Planner A ✕", "Planner B ✕"]);
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");
    await composer.getByRole("button", { name: "Send to 3" }).click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(view.getByRole("heading", { name: "Broadcast to 3 agents" })).toBeVisible();
    for (const title of ["Reviewer", "Planner A", "Planner B"]) {
      await expect(view.getByRole("article", { name: title })).toBeVisible();
    }
    await expect(view.getByRole("article", { name: "Tester" })).toHaveCount(0);
    await expect
      .poll(async () => (await getSentMessages()).map((entry) => entry.target_session).sort())
      .toEqual(["planner-a-session", "planner-b-session", "reviewer-session"]);
  } finally {
    await alice.close();
  }
});

// Select-all ticks rows in `toggleMatching`'s order, and the chips name the selection in the order
// it was ticked, so the two agree only if the set it walks is ordered the way the page shows it.
// The registry's own order is not that order, which is what this seeds.
test("select-all ticks the rows in the order the page shows them, not the order the registry lists them", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the order rule");
  const session = (id: string, title: string): FakeSession => ({
    capabilities: ["aside", "btw"],
    dir: `/workspaces/${id}`,
    machine_id: "build-host",
    roles: ["planner"],
    session_id: `${id}-session`,
    title,
  });
  // Each speaks on an issue, oldest first, so the page lists them newest-activity-first - Alpha,
  // Mid, Zeta - while the registry is seeded in the opposite order, which is the order select-all
  // used to walk.
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ordering" });
  for (const id of ["zeta", "mid", "alpha"]) {
    await createComment(
      issue.key,
      { body: `${id} reporting.` },
      { actor: { id: `${id}-session`, kind: "session" }, as: "agent" }
    );
  }
  await setLiveSessions([
    session("zeta", "Zeta"),
    session("mid", "Mid"),
    session("alpha", "Alpha"),
  ]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Alpha", "Mid", "Zeta"]);

    // A pin is the reader's own, held in the browser, so it reorders the rows and nothing the
    // server sends knows about it: the rendered order and the listed order now differ for sure.
    await agents.getByRole("button", { name: "Pin Zeta" }).click();
    await expect(shownAgentRows(page).locator("h2")).toHaveText(["Zeta", "Alpha", "Mid"]);

    await agents.getByRole("checkbox", { name: "Select all matching agents" }).check();
    const chips = page
      .getByRole("region", { name: "Broadcast" })
      .getByRole("list", { name: "Selected agents" })
      .getByRole("button");
    await expect(chips).toHaveText(["Zeta ✕", "Alpha ✕", "Mid ✕"]);

    // The composer speaks for the same set, in the same order the rows are in.
    await expect(
      page.getByRole("region", { name: "Broadcast" }).getByRole("button", { name: "Send to 3" })
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("the header checkbox stays under the pointer on a phone when its click opens the composer", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
  await setLiveSessions(
    ["a", "b", "c"].map((id) => ({
      capabilities: ["aside", "btw"],
      dir: `/workspaces/${id}`,
      machine_id: "build-host",
      roles: ["planner"],
      session_id: `${id}-session`,
      title: `Planner ${id.toUpperCase()}`,
    }))
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 664, width: 390 });
    await page.goto("/agents");
    const header = page
      .getByRole("region", { name: "Agents" })
      .getByRole("checkbox", { name: "Select all matching agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    await expect(header).toBeVisible();
    const before = await header.boundingBox();
    expect(before).not.toBeNull();

    // The composer appears and goes away with alternate clicks; the header never moves.
    for (const opened of [true, false, true]) {
      await header.click();
      await expect(composer).toHaveCount(opened ? 1 : 0);
      expect(await header.boundingBox()).toEqual(before);
    }
    await expect(header).toBeInViewport();
  } finally {
    await alice.close();
  }
});

/** `count` silent planners, `capable` of them advertising BTW and the rest `aside` only,
 *  numbered to `count`'s width so the list sorts in order. Each has a role and a directory, so
 *  every card at 390 px is 166 px tall; a fixture with shorter cards gives different row counts. */
function planners(count: number, capable: number): FakeSession[] {
  return Array.from({ length: count }, (_, index) => {
    const number = String(index + 1).padStart(String(count).length, "0");
    return {
      capabilities: index < capable ? ["aside", "btw"] : ["aside"],
      dir: `/workspaces/planner-${number}`,
      machine_id: "build-host",
      roles: ["planner"],
      session_id: `planner-${number}-session`,
      title: `Planner ${number}`,
    };
  });
}

/** Types lines into the message box until two consecutive heights agree: the composer at its
 *  tallest, the way it is while someone writes a long message. A box that is not there is the
 *  degenerate case, and it throws: defaulting its height to 0 would agree with the next 0 and
 *  report stability, leaving every caller to measure an unfilled composer and pass. */
async function fillUntilStable(textarea: Locator): Promise<void> {
  let previous = -1;
  // From three lines, past the two rows the empty box already holds.
  for (let lines = 3; lines <= 40; lines += 1) {
    await textarea.fill(Array.from({ length: lines }, (_, line) => `Line ${line + 1}`).join("\n"));
    const box = await textarea.boundingBox();
    if (box === null) throw new Error("the message box is not on screen to measure");
    if (box.height === previous) return;
    previous = box.height;
  }
  throw new Error("the message box never stopped growing");
}

/** Whether a box sits wholly above the composer: top >= 0 and bottom <= the composer's top edge. */
function wholeAbove(box: { y: number; height: number } | null, composerTop: number): boolean {
  return box !== null && box.y >= 0 && box.y + box.height <= composerTop;
}

/** Whole cards above the composer, of the rows a reader sees. */
async function wholeCardsAbove(agents: Locator, composer: Locator): Promise<number> {
  const composerTop = (await composer.boundingBox())?.y ?? 0;
  const boxes = await Promise.all(
    (await shownAgentRows(agents.page()).all()).map((card) => card.boundingBox())
  );
  return boxes.filter((box) => wholeAbove(box, composerTop)).length;
}

test("on a phone the open composer keeps its height budget at forty recipients, empty or filled, keeps its message box steady, and keeps every chip reachable", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
  await setLiveSessions(planners(40, 40));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const viewport = { height: 664, width: 390 };
    await page.setViewportSize(viewport);
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    const row = (index: number) =>
      agents.getByRole("checkbox", {
        name: `Select Planner ${String(index + 1).padStart(2, "0")} for broadcast`,
      });
    const composerHeight = async () => (await composer.boundingBox())?.height ?? Infinity;
    const composerTop = async () => (await composer.boundingBox())?.y ?? 0;
    const checkboxAbove = async (index: number) =>
      wholeAbove(await row(index).boundingBox(), await composerTop());
    const scrollFirstCardToTop = () =>
      row(0).evaluate((element) => element.closest("article")?.scrollIntoView({ block: "start" }));

    const fold = agents.getByRole("button", { name: /^No Dispatch activity \(40/ });
    await fold.click();
    await expect(fold).toHaveAttribute("aria-expanded", "true");
    await row(0).check();
    await row(1).check();
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" })
    ).toBeVisible();
    const heightAtTwo = await composerHeight();
    expect(heightAtTwo).toBeLessThanOrEqual(viewport.height * 0.4);

    // Forty recipients: the count stays exact, and one line of chips keeps the composer at the
    // height two gave it (222.0 px, 33.43%).
    await agents.getByRole("checkbox", { name: "Select all matching agents" }).click();
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 40 of 40 selected" })
    ).toBeVisible();
    expect(await composerHeight()).toBe(heightAtTwo);

    // The message box keeps its height when the first character goes in.
    const emptyMessage = (await message.boundingBox())?.height;
    await message.fill("S");
    expect((await message.boundingBox())?.height).toBe(emptyMessage);
    await message.fill("");

    // At the top of the page the composer covers none of the selection header.
    await page.evaluate(() => window.scrollTo(0, 0));
    for (const control of [
      agents.getByRole("checkbox", { name: "Select all matching agents" }),
      agents.getByRole("button", { name: "Clear selection" }),
    ]) {
      const box = await control.boundingBox();
      expect((box?.y ?? Infinity) + (box?.height ?? 0)).toBeLessThanOrEqual(await composerTop());
    }

    // Row counts below are whole `article` cards with top >= 0 and bottom <= the composer's top
    // edge, with the fold expanded and forty selected; the height clause is the discriminating
    // one, the row clauses record intent. A third whole card is bounded by the 166 px card, not
    // by the composer.
    await expect(shownAgentRows(page)).toHaveCount(40);

    // The defined offset: the first card scrolled to the top of the viewport (scrollY 431),
    // message empty. Two whole cards and the third card's checkbox are above the composer.
    await scrollFirstCardToTop();
    expect(await wholeCardsAbove(agents, composer)).toBeGreaterThanOrEqual(2);
    expect(await checkboxAbove(2)).toBe(true);

    // The operator's moment: from that offset, a message filled until the box stops growing,
    // with no re-scroll (focusing and filling the box scrolls the page, to scrollY 461 here).
    // The composer is then at its tallest, 286 px or 43.1% of 664, under a 45% budget; filling
    // lifts its top by exactly the 64 px the box grew (442 to 378). One whole card is above it,
    // and the next card's checkbox.
    await fillUntilStable(message);
    expect(await composerHeight()).toBeLessThanOrEqual(viewport.height * 0.45);
    expect(await wholeCardsAbove(agents, composer)).toBeGreaterThanOrEqual(1);
    expect(await checkboxAbove(2)).toBe(true);

    // The defined offset again, filled (scrollY 431, cards at 0-166 and 178-344, composer top
    // 378). This clause has 34 px of slack and no more: anything that later adds composer height
    // (another chrome line, taller chips, an inline error) turns it red, and that is the budget
    // working, not a flaky test. Empty, the height clause does the discriminating.
    await scrollFirstCardToTop();
    expect(await wholeCardsAbove(agents, composer)).toBeGreaterThanOrEqual(2);

    // The recipients are one line that scrolls sideways to its last chip.
    const chips = composer.getByRole("list", { name: "Selected agents" });
    expect(await chips.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);
    const last = chips.getByRole("button", { name: /^Planner 40/ });
    await last.scrollIntoViewIfNeeded();
    await expect(last).toBeInViewport();
    const [lastBox, lineBox] = [await last.boundingBox(), await chips.boundingBox()];
    expect(lastBox?.y).toBe(lineBox?.y);
    expect((lastBox?.x ?? Infinity) + (lastBox?.width ?? 0)).toBeLessThanOrEqual(
      (lineBox?.x ?? 0) + (lineBox?.width ?? 0) + 0.5
    );
  } finally {
    await alice.close();
  }
});

test("a narrow or short screen gets the compact composer, filled within 45% of its height", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
  await setLiveSessions(planners(40, 40));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 664, width: 390 });
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    await agents.getByRole("checkbox", { name: "Select all matching agents" }).click();
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 40 of 40 selected" })
    ).toBeVisible();
    const phoneMessage = (await message.boundingBox())?.height;

    // 640 and 700 are the band below `md`; 844x390 and 667x375 are phones in landscape, where
    // height is the constraint.
    // Soft, so each viewport reports on its own.
    for (const size of [
      { height: 664, width: 640 },
      { height: 664, width: 700 },
      { height: 390, width: 844 },
      { height: 375, width: 667 },
    ]) {
      await page.setViewportSize(size);
      await message.fill("");
      expect
        .soft((await message.boundingBox())?.height, `${size.width}x${size.height}`)
        .toBe(phoneMessage);
      await fillUntilStable(message);
      expect
        .soft((await composer.boundingBox())?.height ?? Infinity, `${size.width}x${size.height}`)
        .toBeLessThanOrEqual(size.height * 0.45);
    }
  } finally {
    await alice.close();
  }
});

// The budget is what stays on screen, so this measures the outermost box that sticks, found by
// its computed style, not the composer by name: with a refused send's row present, which the rows
// above never have, a strip inside that box would count against the 45%. A `display: contents`
// element generates no box, so it sticks nothing.
test("on a narrow or short screen a refused send's row stays out of the sticky block, which holds 45% with the message filled", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
  await setLiveSessions(planners(40, 40));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 664, width: 390 });
    await refuseBroadcasts(page);
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const header = agents.getByRole("checkbox", { name: "Select all matching agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    const sends = page.getByRole("region", { name: "Sends" });
    await header.click();
    await message.fill("Refused before the measure.");
    await composer.getByRole("button", { name: "Send to 40" }).click();
    await expect(sends.getByRole("status")).toHaveText(/^Could not send to 40 agents: /);
    await header.click();

    for (const size of [
      { height: 664, width: 390 },
      { height: 664, width: 640 },
      { height: 664, width: 700 },
      { height: 390, width: 844 },
      { height: 375, width: 667 },
    ]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize(size);
      await fillUntilStable(message);
      const block = await composer.evaluate((section) => {
        let sticky: Element | null = null;
        for (let node: Element | null = section; node !== null; node = node.parentElement) {
          const style = getComputedStyle(node);
          if (style.position === "sticky" && style.display !== "contents") sticky = node;
        }
        const strip = document.querySelector('section[aria-label="Sends"]');
        return {
          height: sticky?.getBoundingClientRect().height ?? Number.POSITIVE_INFINITY,
          holdsStrip: strip === null || sticky === null || sticky.contains(strip),
          stripHeight: strip?.getBoundingClientRect().height ?? 0,
        };
      });
      expect.soft(block.stripHeight, `${at}: the refused row is on the page`).toBeGreaterThan(0);
      expect.soft(block.holdsStrip, `${at}: the strip is outside the sticky block`).toBe(false);
      expect.soft(block.height, at).toBeLessThanOrEqual(size.height * 0.45);
    }
  } finally {
    await alice.close();
  }
});

const NOTICE = /^(At most \d+ recipients|Excluded:)/;
const NOTICE_SIZES = [
  { height: 664, width: 390 },
  { height: 390, width: 844 },
  { height: 375, width: 667 },
];

/**
 * At each compact size, with the message filled until it stops growing: the composer is within
 * 45% of the viewport, exactly one notice renders, and the notice adds no grid row on a narrow
 * screen (heading, chips, message, controls: four) and at most one on a short one (three).
 *
 * Every default here points at failure. `NOTICE` crosses into the page whole rather than as a
 * source string, so a pattern that matches nothing cannot collapse the count to a passing 0;
 * the count is `toBe(1)`, not a one-sided bound a 0 would satisfy; and the row budget asserts
 * the grid it is counting, since `gridTemplateRows` reads `none` off a non-grid element and
 * `none` splits to one passing token.
 */
async function expectNoticeWithinBudget(
  page: Page,
  composer: Locator,
  check: (at: string) => Promise<void>
): Promise<void> {
  const message = composer.getByRole("textbox", { name: "Broadcast message" });
  for (const size of NOTICE_SIZES) {
    const at = `${size.width}x${size.height}`;
    await page.setViewportSize(size);
    await fillUntilStable(message);
    expect
      .soft((await composer.boundingBox())?.height ?? Infinity, at)
      .toBeLessThanOrEqual(size.height * 0.45);
    const shape = await composer.evaluate(
      (section, notice) => ({
        display: getComputedStyle(section).display,
        notices: [...section.querySelectorAll("p")].filter((line) =>
          notice.test(line.textContent ?? "")
        ).length,
        rows: getComputedStyle(section).gridTemplateRows.split(" ").filter(Boolean).length,
      }),
      NOTICE
    );
    expect.soft(shape.notices, at).toBe(1);
    expect.soft(shape.display, at).toBe("grid");
    expect.soft(shape.rows, at).toBeLessThanOrEqual(size.height <= 500 ? 3 : 4);
    await check(at);
  }
}

/** The Agents page at 390x664 with every session selected, and the notice slot's parts. */
async function openNoticeFixture(page: Page, sessions: FakeSession[]) {
  const agents = page.getByRole("region", { name: "Agents" });
  const composer = page.getByRole("region", { name: "Broadcast" });
  await setLiveSessions(sessions);
  await page.setViewportSize({ height: 664, width: 390 });
  await page.goto("/agents");
  await agents.getByRole("checkbox", { name: "Select all matching agents" }).click();
  await expect(composer).toBeVisible();
  return {
    agents,
    composer,
    excludedChip: (title: string) =>
      composer.getByRole("button", { name: new RegExp(`^${title} · does not advertise BTW`) }),
    excludedLine: composer.getByText(/^Excluded:/),
    limit: composer.getByText(/^At most 100 recipients per broadcast/),
    notice: composer.getByText(NOTICE),
  };
}

test("with half the recipients unable to take the mode, the compact composer keeps its budget and every exclusion stays reachable on one line", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Planners 21 to 40 advertise `aside` only, so the default BTW mode excludes them. Every
    // earlier budget fixture excluded nobody, so the Excluded line never rendered there.
    const fixture = await openNoticeFixture(page, planners(40, 20));
    await expect(
      fixture.composer.getByRole("heading", { name: "Broadcast to 20 of 40 selected" })
    ).toBeVisible();
    // Tight at 667x375: 166 of 168.75. The short grid is rows 44 + 64 + 16, two 8 px gaps, 24 px
    // of padding and a 2 px border. A fourth row costs 24 px whatever its height, so the next
    // element added to the compact composer has no room: the budget goes red while the composer
    // is still inside its 50vh cap and nothing on screen looks wrong.
    await expectNoticeWithinBudget(page, fixture.composer, async (at) => {
      // All twenty stay named on the line, and its last name scrolls into view.
      for (let index = 21; index <= 40; index += 1) {
        await expect
          .soft(fixture.excludedLine, at)
          .toContainText(`Planner ${index} (does not advertise BTW)`);
      }
      const last = await fixture.excludedLine.evaluate((element) => {
        const lastName = "Planner 40";
        const scrolls = element.scrollWidth > element.clientWidth;
        const text = [...element.childNodes].find((node) => node.textContent?.includes(lastName));
        if (text === undefined) return { scrolls, visible: false };
        const range = document.createRange();
        const start = text.textContent?.indexOf(lastName) ?? 0;
        range.setStart(text, start);
        range.setEnd(text, start + lastName.length);
        // Scroll the line so the name's left edge meets the line's, then read both again.
        element.scrollLeft +=
          range.getBoundingClientRect().left - element.getBoundingClientRect().left;
        const [name, line] = [range.getBoundingClientRect(), element.getBoundingClientRect()];
        return {
          scrolls,
          visible: name.left >= line.left - 0.5 && name.right <= line.right + 0.5,
        };
      });
      expect.soft(last, at).toEqual({ scrolls: true, visible: true });
    });
  } finally {
    await alice.close();
  }
});

test.describe("the composer's notices share one slot, highest first, within budget on compact screens", () => {
  test("the limit alone: 120 recipients", async ({ browser }, testInfo) => {
    test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const fixture = await openNoticeFixture(page, planners(120, 120));
      await expectNoticeWithinBudget(page, fixture.composer, async (at) => {
        await expect.soft(fixture.limit, at).toHaveCount(1);
        await expect.soft(fixture.notice, at).toHaveCount(1);
      });
    } finally {
      await alice.close();
    }
  });

  test("the limit over exclusions: 120 selected, 110 recipients, each excluded chip still names its reason", async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const fixture = await openNoticeFixture(page, planners(120, 110));
      await expectNoticeWithinBudget(page, fixture.composer, async (at) => {
        await expect.soft(fixture.limit, at).toHaveCount(1);
        await expect.soft(fixture.excludedLine, at).toHaveCount(0);
        await expect.soft(fixture.excludedChip("Planner 111"), at).toHaveCount(1);
      });
    } finally {
      await alice.close();
    }
  });

  test("the boundary with exclusions: 101 recipients show the limit, exactly 100 the Excluded line", async ({
    browser,
  }, testInfo) => {
    test.skip(testInfo.project.name !== "chromium", "the viewport is set here, not by the project");
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const fixture = await openNoticeFixture(page, planners(111, 101));
      await expectNoticeWithinBudget(page, fixture.composer, async (at) => {
        await expect.soft(fixture.limit, at).toHaveCount(1);
        await expect.soft(fixture.excludedLine, at).toHaveCount(0);
      });
      await page.setViewportSize({ height: 664, width: 390 });
      await fixture.agents.getByRole("button", { name: /^No Dispatch activity/ }).click();
      await fixture.agents
        .getByRole("checkbox", { name: "Select Planner 101 for broadcast" })
        .uncheck();
      await expect(
        fixture.composer.getByRole("heading", { name: "Broadcast to 100 of 110 selected" })
      ).toBeVisible();
      await expectNoticeWithinBudget(page, fixture.composer, async (at) => {
        await expect.soft(fixture.limit, at).toHaveCount(0);
        await expect.soft(fixture.excludedLine, at).toHaveCount(1);
      });
    } finally {
      await alice.close();
    }
  });
});

test("a typed broadcast survives clearing the selection and picking again, and a refused send gives it back", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the draft rule");
  const pair = planners(40, 40).slice(0, 2);
  await setLiveSessions(pair);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const header = agents.getByRole("checkbox", { name: "Select all matching agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    const mode = composer.getByRole("combobox", { name: "Delivery mode" });
    const sends = page.getByRole("region", { name: "Sends" });

    await header.click();
    await message.fill("Keep this draft.");
    await mode.selectOption("aside");
    // Clearing the selection takes the composer away; picking again brings the draft back.
    await header.click();
    await expect(composer).toHaveCount(0);
    await header.click();
    await expect(message).toHaveValue("Keep this draft.");
    await expect(mode).toHaveValue("aside");

    // Send hands the draft to the queue, so the composer goes with the selection. The server
    // refuses it, and the send's row keeps it: Restore draft puts the message, its mode and its
    // recipients back.
    const refusal = await refuseBroadcasts(page);
    await composer.getByRole("button", { name: "Send to 2" }).click();
    await expect(composer).toHaveCount(0);
    const failed = sends.getByRole("listitem").filter({ hasText: "Keep this draft." });
    await expect(failed.getByRole("status")).toHaveText(
      "Could not send to 2 agents: Envoy listener unreachable"
    );
    // A message or a selection started since that press would be lost to Restore draft, so it
    // refuses, and says why under its row, until both are cleared. Playwright waits out a click
    // on an `aria-disabled` button, so the refused press is forced.
    const restore = failed.getByRole("button", { name: "Restore draft" });
    const why =
      "Restore draft would replace the broadcast you have started. Send it, or clear its message and selection, first.";
    await header.click();
    await message.fill("Started since.");
    await expect(restore).toBeDisabled();
    await expect(restore).toHaveAccessibleDescription(why);
    await expect(failed.getByText(why, { exact: true })).toBeVisible();
    await restore.click({ force: true });
    await expect(message).toHaveValue("Started since.");
    await message.fill("");
    await expect(restore).toBeDisabled();
    await restore.click({ force: true });
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" })
    ).toBeVisible();
    await expect(message).toHaveValue("");
    await header.click();
    await expect(composer).toHaveCount(0);
    await expect(restore).toBeEnabled();
    await restore.click();
    await expect(message).toHaveValue("Keep this draft.");
    await expect(mode).toHaveValue("aside");
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" })
    ).toBeVisible();
    await expect(sends).toHaveCount(0);

    // A single send that succeeds leaves the page; coming back finds an empty draft.
    refusal.allow();
    await composer.getByRole("button", { name: "Send to 2" }).click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    await page.goBack();
    await header.click();
    await expect(message).toHaveValue("");
  } finally {
    await alice.close();
  }
});

test("a second broadcast pressed while the first is on the wire waits its turn, and each send keeps its own row", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the queue");
  const pair = planners(40, 40).slice(0, 2);
  await setLiveSessions(pair);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Each POST waits until the test lets it through, so each row's states can be read in turn
    // and the second press lands while the first is still on the wire.
    const held = [Promise.withResolvers<void>(), Promise.withResolvers<void>()];
    const posted: unknown[] = [];
    await page.route("**/api/v1/broadcasts", async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      const index = posted.push(route.request().postDataJSON()) - 1;
      await held[index]?.promise;
      return route.fallback();
    });
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const header = agents.getByRole("checkbox", { name: "Select all matching agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    const sends = page.getByRole("region", { name: "Sends" });
    const row = (body: string) => sends.getByRole("listitem").filter({ hasText: body });

    await header.click();
    await message.fill("First.");
    // Two clicks in one task, before React re-renders: one send, not two.
    await composer.getByRole("button", { name: "Send to 2" }).evaluate((button: HTMLElement) => {
      button.click();
      button.click();
    });
    // The press hands the message off: the draft and the selection go with it.
    await expect(composer).toHaveCount(0);
    await expect(row("First.").getByRole("status")).toHaveText("Sending to 2 agents…");
    await expect(sends.getByRole("listitem")).toHaveCount(1);

    await header.click();
    await expect(message).toHaveValue("");
    await message.fill("Second.");
    await composer.getByRole("button", { name: "Send to 2" }).click();
    await expect(row("Second.").getByRole("status")).toHaveText("Queued: to 2 agents");
    // One at a time: the second has not left the browser while the first is unanswered.
    expect(posted).toMatchObject([{ body: "First." }]);
    // The reader switches to another tab while the first is on the wire. The second still goes
    // the moment the first is answered: a hidden tab holds nothing back. This sets the two inputs
    // TanStack's `focusManager` reads.
    const setVisibility = (state: "hidden" | "visible") =>
      page.evaluate((value) => {
        Object.defineProperty(document, "visibilityState", { configurable: true, value });
        document.dispatchEvent(new Event("visibilitychange", { bubbles: true }));
      }, state);
    await setVisibility("hidden");
    held[0]?.resolve();
    await expect(row("First.").getByRole("link", { name: "Sent to 2 agents" })).toBeVisible();
    await expect(row("Second.").getByRole("status")).toHaveText("Sending to 2 agents…");
    await expect.poll(() => posted).toMatchObject([{ body: "First." }, { body: "Second." }]);
    // Nothing navigates while a send is still out.
    await expect(page).toHaveURL(/\/agents$/);
    await setVisibility("visible");

    held[1]?.resolve();
    await expect(row("Second.").getByRole("link", { name: "Sent to 2 agents" })).toBeVisible();
    // After more than one send the page stays, and each row links its own broadcast.
    await page.waitForTimeout(500);
    await expect(page).toHaveURL(/\/agents$/);
    await row("First.").getByRole("link", { name: "Sent to 2 agents" }).click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    const view = page.getByRole("region", { name: "Broadcast" });
    await expect(view.getByRole("heading", { name: "Broadcast to 2 agents" })).toBeVisible();
    await expect(view.locator("header")).toContainText("First.");
  } finally {
    await alice.close();
  }
});

test("a send the server refuses keeps its row after a later send succeeds, and Retry sends it again as it was", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the queue");
  const pair = planners(40, 40).slice(0, 2);
  await setLiveSessions(pair);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const header = agents.getByRole("checkbox", { name: "Select all matching agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    const sends = page.getByRole("region", { name: "Sends" });
    const row = (body: string) => sends.getByRole("listitem").filter({ hasText: body });

    await header.click();
    await message.fill("Refused first.");
    await composer.getByRole("combobox", { name: "Delivery mode" }).selectOption("aside");
    const refusal = await refuseBroadcasts(page);
    await composer.getByRole("button", { name: "Send to 2" }).click();
    const failed = row("Refused first.");
    await expect(failed.getByRole("status")).toHaveText(
      "Could not send to 2 agents: Envoy listener unreachable"
    );

    refusal.allow();
    await header.click();
    await message.fill("Second.");
    await composer.getByRole("button", { name: "Send to 2" }).click();
    await expect(row("Second.").getByRole("link", { name: "Sent to 2 agents" })).toBeVisible();
    // A later success neither hides the failure nor navigates away from it.
    await page.waitForTimeout(500);
    await expect(page).toHaveURL(/\/agents$/);
    await expect(failed.getByRole("status")).toHaveText(/^Could not send to 2 agents: /);
    await expect(failed.getByRole("button", { name: "Restore draft" })).toBeVisible();

    // Retry sends the very message again: the same body, mode and recipients.
    await failed.getByRole("button", { name: "Retry" }).click();
    await expect(
      row("Refused first.").getByRole("link", { name: "Sent to 2 agents" })
    ).toBeVisible();
    await expect(sends.getByText(/^Could not send/)).toHaveCount(0);
    expect(posted).toHaveLength(3);
    expect(posted[2]).toEqual(posted[0]);
    expect(posted[0]).toEqual({
      body: "Refused first.",
      delivery: "aside",
      idempotency_key: expect.any(String),
      session_ids: pair.map((session) => session.session_id),
    });
  } finally {
    await alice.close();
  }
});

test("a human-paced double click on one Retry sends that message once, and never the refused row beside it", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the queue");
  const pair = planners(40, 40).slice(0, 2);
  await setLiveSessions(pair);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posted = postedBroadcasts(page);
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const header = agents.getByRole("checkbox", { name: "Select all matching agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    const sends = page.getByRole("region", { name: "Sends" });
    const row = (body: string) => sends.getByRole("listitem").filter({ hasText: body });

    const refusal = await refuseBroadcasts(page);
    for (const body of ["Retried once.", "Never retried."]) {
      await header.click();
      await message.fill(body);
      await composer.getByRole("button", { name: "Send to 2" }).click();
      await expect(row(body).getByRole("status")).toHaveText(/^Could not send to 2 agents: /);
    }
    refusal.allow();

    // Newest first, so the row double-clicked is the lower one and the other refused row sits
    // right above it. Two clicks 120 ms apart at one point, as a hand makes them: the retried row
    // keeps its place rather than jumping to the top as a new press would, so nothing shifts down
    // into the pointer and the second click lands on that row again.
    await expect(sends.getByRole("listitem").first()).toContainText("Never retried.");
    const retry = row("Retried once.").getByRole("button", { name: "Retry" });
    const box = await retry.boundingBox();
    if (box === null) throw new Error("Retry has no box to click");
    const point = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
    await page.mouse.click(point.x, point.y);
    await page.waitForTimeout(120);
    await page.mouse.click(point.x, point.y);
    await expect(
      row("Retried once.").getByRole("link", { name: "Sent to 2 agents" })
    ).toBeVisible();
    await page.waitForTimeout(500);
    expect(posted.map((request) => request.body)).toEqual([
      "Retried once.",
      "Never retried.",
      "Retried once.",
    ]);
    await expect(sends.getByRole("listitem").last()).toContainText("Retried once.");
    await expect(row("Never retried.").getByRole("status")).toHaveText(
      /^Could not send to 2 agents: /
    );
  } finally {
    await alice.close();
  }
});

test("a selection over the broadcast limit says so and never asks the server", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the limit");
  await setLiveSessions(planners(101, 101));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = postedBroadcasts(page);
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    await agents.getByRole("checkbox", { name: "Select all matching agents" }).click();
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 101 of 101 selected" })
    ).toBeVisible();
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Too many.");
    const send = composer.getByRole("button", { name: "Send to 101" });
    await expect(send).toBeDisabled();
    await send.click({ force: true });
    await page.waitForTimeout(500);
    expect(posts).toEqual([]);
    expect(await getSentMessages()).toEqual([]);
    await expect(
      composer.getByText("At most 100 recipients per broadcast; this one would reach 101.")
    ).toBeVisible();

    // One fewer is within the limit: the reason goes and Send comes back.
    await agents.getByRole("button", { name: /^No Dispatch activity/ }).click();
    await agents.getByRole("checkbox", { name: "Select Planner 101 for broadcast" }).uncheck();
    await expect(composer.getByText(/^At most 100 recipients/)).toHaveCount(0);
    await expect(composer.getByRole("button", { name: "Send to 100" })).toBeEnabled();
  } finally {
    await alice.close();
  }
});

/** A live session advertising only `capabilities`, titled `Builder <ID>`. */
function builder(id: string, capabilities: string[]): FakeSession {
  return {
    capabilities,
    dir: `/workspaces/${id}`,
    machine_id: "build-host",
    roles: ["builder"],
    session_id: `${id}-session`,
    title: `Builder ${id.toUpperCase()}`,
  };
}

test("a mode no selected agent advertises leaves Send dead, and Send says so and names the mode that would reach them", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the reason");
  // Both advertise `steer` only, so the default BTW mode leaves every selected agent out.
  await setLiveSessions([builder("a", ["steer"]), builder("b", ["steer"])]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const posts = postedBroadcasts(page);
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    const mode = composer.getByRole("combobox", { name: "Delivery mode" });
    const message = composer.getByRole("textbox", { name: "Broadcast message" });
    await agents.getByRole("checkbox", { name: "Select all matching agents" }).click();
    await message.fill("Report status.");
    await expect(mode).toHaveValue("btw");

    // The label refuses rather than counting, and the reason rides the button itself: the
    // Excluded line, which names each agent, why, and the mode that would reach them.
    const dead = composer.getByRole("button", { name: "No recipient" });
    const why =
      "Excluded: Builder A (does not advertise BTW), Builder B (does not advertise BTW). Nothing is sent to them, and no other mode is substituted. Sending as Send would reach 2 of them.";
    await expect(dead).toBeDisabled();
    await expect(dead).toHaveAccessibleDescription(why);
    await expect(dead).toHaveAttribute("title", why);
    // A keyboard reaches it, so the reason does too: Tab from the message lands on Send.
    await message.press("Tab");
    await expect(dead).toBeFocused();
    // Refused, not merely grey: pressing it anyway asks nothing of the server.
    await dead.click({ force: true });
    await dead.press("Enter");
    await page.waitForTimeout(500);
    expect(posts).toEqual([]);
    expect(await getSentMessages()).toEqual([]);

    // The mode was the whole cause: switching to the one named brings Send back.
    await mode.selectOption("steer");
    const send = composer.getByRole("button", { name: "Send to 2" });
    await expect(send).toBeEnabled();
    await expect(send).not.toHaveAttribute("title");
    await expect(composer.getByText(/^Excluded:/)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("with one of two selected agents gone from the registry, Send reaches the live one and names the one left out", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the reason");
  // The page learns of a departure on its 15 s agent poll, which this row waits out.
  test.setTimeout(60_000);
  const [alpha, bravo] = [builder("alpha", ["btw"]), builder("bravo", ["btw"])];
  await setLiveSessions([alpha, bravo]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    await agents.getByRole("checkbox", { name: "Select all matching agents" }).click();
    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");
    await expect(composer.getByRole("button", { name: "Send to 2" })).toBeEnabled();

    // Bravo leaves; the page learns of it on its next agent poll, and names it by its ID since
    // the registry no longer holds its title.
    await setSessionLive(bravo.session_id, false);
    const send = composer.getByRole("button", { name: "Send to 1" });
    await expect(send).toBeEnabled({ timeout: 30_000 });
    await expect(send).toHaveAccessibleDescription(
      "Excluded: session:bravo-se… (no live session). Nothing is sent to them, and no other mode is substituted."
    );
    await expect(
      composer.getByRole("button", { name: /^session:bravo-se… · no live session/ })
    ).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("a session that registers under the filter after select-all is not swept into the send", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "one browser proves the selection rule");
  const planner = (id: string): FakeSession => ({
    capabilities: ["aside", "btw"],
    dir: `/workspaces/${id}`,
    machine_id: "build-host",
    roles: ["planner"],
    session_id: `${id}-session`,
    title: `Planner ${id.toUpperCase()}`,
  });
  await setLiveSessions([planner("a"), planner("b")]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    const agents = page.getByRole("region", { name: "Agents" });
    const header = agents.getByRole("checkbox", { name: "Select all matching agents" });
    const composer = page.getByRole("region", { name: "Broadcast" });
    await agents.getByRole("combobox", { name: "Role" }).selectOption("planner");
    await header.click();
    await expect(header).toHaveAccessibleDescription("2 of 2 matching selected");

    // A third planner registers; the page learns of it on its next agent poll.
    await setLiveSessions([planner("a"), planner("b"), planner("c")]);
    await expect(header).toHaveAccessibleDescription("2 of 3 matching selected", {
      timeout: 30_000,
    });
    await expect(header).toBeChecked({ indeterminate: true });
    await expect(
      composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" })
    ).toBeVisible();

    await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Only the two.");
    await composer.getByRole("button", { name: "Send to 2" }).click();
    await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
    await expect
      .poll(async () => (await getSentMessages()).map((entry) => entry.target_session).sort())
      .toEqual(["a-session", "b-session"]);
  } finally {
    await alice.close();
  }
});
