import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions, setSessionLive } from "./agents";
import {
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  mintAgentToken,
  patchIssue,
} from "./api";
import { marginCard, openedThreadCard, setSheet } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// One agent component draws every agent the app shows (dispatch://INFRA-440): its current name,
// whether it is running, and a link to its live conversation. These rows hold it to that in the
// four places the issue names - "Claimed by", a Conversation turn, an Inbox ask card and a margin
// comment - for a running agent and one that is not running, at desktop and phone width.

// Ids that differ in their first eight characters, so their short ids differ too.
const runId = "e2e-run-agent";
const stopId = "e2e-stop-agent";
const untitledId = "e2e-untitled";

const runStamped = "Running agent (stamped)";
const runCurrent = "Running agent (current)";
const stopStamped = "Stopped agent (stamped)";
const untitledShort = "session:e2e-unti…";
const stopShort = "session:e2e-stop…";

function writer(id: string, title?: string) {
  return {
    actor: {
      id,
      kind: "session" as const,
      origin: title === undefined ? { tmux: "dev:1.1" } : { session_title: title, tmux: "dev:1.1" },
    },
    as: "agent" as const,
  };
}

const liveSessions: FakeSession[] = [
  { session_id: runId, title: runCurrent },
  { session_id: stopId, title: stopStamped },
  { session_id: untitledId, title: "" },
];

interface Seeded {
  runIssue: string;
  stopIssue: string;
  runAsk: string;
  stopAsk: string;
  runMessage: string;
  stopMessage: string;
  untitledMessage: string;
  runComment: string;
  stopComment: string;
}

/** Real session writes from three sessions while all three are listed, then two of them leave
 *  the listener's list before any page loads. The running agent's claim is written with a
 *  personal token, so its label carries the owner suffix. */
async function seed(): Promise<Seeded> {
  await createProject({ key: "AGT", name: "Agents" });
  const runIssue = await createIssue({
    project: "AGT",
    spec: "The running agent comments here. The stopped agent comments there.",
    title: "Held by a running agent",
  });
  const stopIssue = await createIssue({ project: "AGT", title: "Held by a stopped agent" });
  await Promise.all([
    patchIssue(runIssue.key, { status: "todo" }),
    patchIssue(stopIssue.key, { status: "todo" }),
  ]);
  await setLiveSessions(liveSessions);
  const token = await mintAgentToken("agent-name-e2e", "alice");
  await claimIssue(runIssue.key, { ...writer(runId, runStamped), token });
  await claimIssue(stopIssue.key, writer(stopId, stopStamped));
  const runMessage = await createMessage(
    runIssue.key,
    { body: "Message from the running agent" },
    writer(runId, runStamped)
  );
  const stopMessage = await createMessage(
    runIssue.key,
    { body: "Message from the stopped agent" },
    writer(stopId, stopStamped)
  );
  const untitledMessage = await createMessage(
    runIssue.key,
    { body: "Message from the untitled agent" },
    writer(untitledId)
  );
  const runAsk = await createAsk(
    runIssue.key,
    { question: "Question from the running agent" },
    writer(runId, runStamped)
  );
  const stopAsk = await createAsk(
    runIssue.key,
    { question: "Question from the stopped agent" },
    writer(stopId, stopStamped)
  );
  const runComment = await createComment(
    runIssue.key,
    { anchor: { artifact: "spec", quote: "comments here" }, body: "Margin from the running agent" },
    writer(runId, runStamped)
  );
  const stopComment = await createComment(
    runIssue.key,
    {
      anchor: { artifact: "spec", quote: "comments there" },
      body: "Margin from the stopped agent",
    },
    writer(stopId, stopStamped)
  );
  await setSessionLive(stopId, false);
  await setSessionLive(untitledId, false);
  return {
    runAsk: runAsk.id,
    runComment: runComment.id,
    runIssue: runIssue.key,
    runMessage: runMessage.id,
    stopAsk: stopAsk.id,
    stopComment: stopComment.id,
    stopIssue: stopIssue.key,
    stopMessage: stopMessage.id,
    untitledMessage: untitledMessage.id,
  };
}

function livePath(id: string): string {
  return `/agents/${encodeURIComponent(id)}/live`;
}

function agentIn(scope: Locator, id: string): Locator {
  return scope.locator(`[data-agent-name="${id}"]`).first();
}

/** The component in `scope` for `id`: the name it shows, its state mark, and its live link. */
async function expectAgent(
  scope: Locator,
  id: string,
  shown: { name: string; hidden?: string; running: boolean; link?: "name" | "beside" }
): Promise<void> {
  const agent = agentIn(scope, id);
  await expect(agent).toBeVisible();
  await expect(agent).toContainText(shown.name);
  if (shown.hidden !== undefined) {
    await expect(agent).not.toContainText(shown.hidden);
  }
  await expect(agent).toHaveAttribute(
    "data-agent-state",
    shown.running ? "running" : "not-running"
  );
  await expect(
    agent.getByRole("img", { exact: true, name: shown.running ? "Running" : "Not running" })
  ).toBeVisible();
  await expect(
    agent.getByRole("img", { exact: true, name: shown.running ? "Not running" : "Running" })
  ).toHaveCount(0);
  await expect(agent.locator(`a[href="${livePath(id)}"]`)).toHaveCount(1);
}

function messageTurn(page: Page, id: string): Locator {
  return page.locator(`[data-turn="message:${id}"]`);
}

async function openMarginThread(page: Page, project: string, commentId: string): Promise<Locator> {
  await setSheet(page, project, true);
  await marginCard(page, commentId).locator('button[aria-expanded="false"]').first().click();
  return openedThreadCard(page, project, commentId);
}

async function closeMarginThread(page: Page, project: string): Promise<void> {
  if (project === "iphone") {
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Thread" })).toHaveCount(0);
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("Claimed by, a Conversation turn, an Inbox ask card and a margin comment show the agent's current name, whether it runs, and a live link", async ({
  browser,
}, testInfo) => {
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // Claimed by: the running holder by its current title with the owner suffix, the stopped
    // holder by the title it stamped.
    await page.goto(`/issues/${seeded.runIssue}`);
    const runClaim = page.getByTestId("issue-claim");
    await expectAgent(runClaim, runId, { hidden: runStamped, name: runCurrent, running: true });
    await expect(agentIn(runClaim, runId)).toContainText(`${runCurrent} (for alice)`);
    await page.goto(`/issues/${seeded.stopIssue}`);
    await expectAgent(page.getByTestId("issue-claim"), stopId, {
      name: stopStamped,
      running: false,
    });

    // Conversation turns, the untitled agent by its short id.
    await page.goto(`/issues/${seeded.runIssue}/conversation`);
    await expectAgent(messageTurn(page, seeded.runMessage), runId, {
      hidden: runStamped,
      name: runCurrent,
      running: true,
    });
    await expectAgent(messageTurn(page, seeded.stopMessage), stopId, {
      name: stopStamped,
      running: false,
    });
    await expectAgent(messageTurn(page, seeded.untitledMessage), untitledId, {
      name: untitledShort,
      running: false,
    });

    // Inbox ask cards: the asker's copy-handles button stays, and the live link sits beside it,
    // never inside it.
    await page.goto("/");
    for (const [askId, id, name, running] of [
      [seeded.runAsk, runId, runCurrent, true],
      [seeded.stopAsk, stopId, stopStamped, false],
    ] as const) {
      const card = page.getByTestId(`ask-${askId}`);
      await expectAgent(card, id, { name, running });
      const live = card.getByRole("link", {
        exact: true,
        name: `Open live conversation with ${name}`,
      });
      await expect(live).toHaveAttribute("href", livePath(id));
      expect(await live.evaluate((link) => link.closest("button") === null)).toBe(true);
      await expect(
        card.getByRole("button", {
          expanded: false,
          name: new RegExp(name.replace(/[()]/g, "\\$&")),
        })
      ).toBeVisible();
    }

    // Margin comments, each in its opened thread.
    await page.goto(`/issues/${seeded.runIssue}/spec`);
    const runThread = await openMarginThread(page, testInfo.project.name, seeded.runComment);
    await expectAgent(runThread, runId, { hidden: runStamped, name: runCurrent, running: true });
    await closeMarginThread(page, testInfo.project.name);
    const stopThread = await openMarginThread(page, testInfo.project.name, seeded.stopComment);
    await expectAgent(stopThread, stopId, { name: stopStamped, running: false });
  } finally {
    await alice.close();
  }
});

test("the live link opens that agent's conversation, running or not", async ({
  browser,
}, testInfo) => {
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${seeded.runIssue}`);
    const runLink = agentIn(page.getByTestId("issue-claim"), runId).locator(
      `a[href="${livePath(runId)}"]`
    );
    await expect(runLink).toBeVisible();
    await runLink.click();
    await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(runCurrent);

    await page.goto(`/issues/${seeded.runIssue}/spec`);
    const stopThread = await openMarginThread(page, testInfo.project.name, seeded.stopComment);
    const stopLink = agentIn(stopThread, stopId).locator(`a[href="${livePath(stopId)}"]`);
    await expect(stopLink).toBeVisible();
    await stopLink.click();
    await expect(page).toHaveURL(new RegExp(`${livePath(stopId)}$`));
    // The live page knows only the route's id and the registry, so a stopped agent reads by its
    // short id there; the id in the URL is what says the right conversation opened.
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(stopShort);
  } finally {
    await alice.close();
  }
});

test("on a phone the ask card's asker still opens its copy handles, and the live link beside it opens the conversation", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "the kept action is checked at phone width");
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const card = page.getByTestId(`ask-${seeded.runAsk}`);
    const asker = card.getByRole("button", { expanded: false, name: /Running agent \(current\)/ });
    const live = card.getByRole("link", {
      exact: true,
      name: `Open live conversation with ${runCurrent}`,
    });
    await expect(asker).toBeVisible();
    await expect(live).toBeVisible();
    const [askerBox, liveBox] = await Promise.all([asker.boundingBox(), live.boundingBox()]);
    if (askerBox === null || liveBox === null) {
      throw new Error("the asker button or the live link has no box");
    }
    const overlaps =
      askerBox.x < liveBox.x + liveBox.width &&
      liveBox.x < askerBox.x + askerBox.width &&
      askerBox.y < liveBox.y + liveBox.height &&
      liveBox.y < askerBox.y + askerBox.height;
    expect(overlaps).toBe(false);
    expect(liveBox.height).toBeGreaterThanOrEqual(44);
    const widths = await page.evaluate(() => ({
      client: document.documentElement.clientWidth,
      scroll: document.documentElement.scrollWidth,
    }));
    expect(widths.scroll).toBe(widths.client);

    await asker.tap();
    await expect(
      card.getByRole("button", { name: `Copy session ID ${runId}`, exact: true })
    ).toBeVisible();
    await live.tap();
    await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
  } finally {
    await alice.close();
  }
});

test("the page reads the agent list once, asks nothing per agent, and shows a rename and a stop without a reload", async ({
  browser,
}, testInfo) => {
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const registryReads: string[] = [];
    const perAgentReads: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (path === "/api/v1/agents") {
        registryReads.push(path);
      } else if (path.startsWith("/api/v1/agents/")) {
        perAgentReads.push(path);
      }
    });
    await page.goto(`/issues/${seeded.runIssue}/conversation`);
    const claim = agentIn(page.getByTestId("issue-claim"), runId);
    await expect(claim).toHaveAttribute("data-agent-state", "running");
    await expect(agentIn(messageTurn(page, seeded.stopMessage), stopId)).toHaveAttribute(
      "data-agent-state",
      "not-running"
    );
    await expect(agentIn(messageTurn(page, seeded.untitledMessage), untitledId)).toBeVisible();
    expect(registryReads).toHaveLength(1);
    expect(perAgentReads).toEqual([]);
    if (testInfo.project.name === "iphone") {
      const widths = await page.evaluate(() => ({
        client: document.documentElement.clientWidth,
        scroll: document.documentElement.scrollWidth,
      }));
      expect(widths.scroll).toBe(widths.client);
    }

    // The listener renames the running agent, then drops it: the open page follows both.
    await setLiveSessions([{ session_id: runId, title: "Running agent (renamed)" }]);
    await expect(claim).toContainText("Running agent (renamed)", { timeout: 30_000 });
    await expect(agentIn(messageTurn(page, seeded.runMessage), runId)).toContainText(
      "Running agent (renamed)"
    );
    await setSessionLive(runId, false);
    await expect(claim).toHaveAttribute("data-agent-state", "not-running", { timeout: 30_000 });
    await expect(claim.getByRole("img", { exact: true, name: "Not running" })).toBeVisible();
    expect(perAgentReads).toEqual([]);
  } finally {
    await alice.close();
  }
});

test("a claimed List row and Board card open their issue by keyboard, and the claim's agent link opens the agent", async ({
  browser,
}, testInfo) => {
  const seeded = await seed();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const phone = testInfo.project.name === "iphone";
    if (!phone) {
      await page.setViewportSize({ height: 900, width: 1920 });
    }
    await page.goto("/projects/AGT");
    await page.getByRole("button", { name: "Board" }).click();
    const card = page.locator(`[data-board-card="${seeded.runIssue}"]`);
    const cardAgent = agentIn(card, runId);
    await expect(cardAgent).toHaveAttribute("data-agent-state", "running");
    const cardLink = cardAgent.locator(`a[href="${livePath(runId)}"]`);
    if (phone) {
      // A tap on the agent link navigates; it never starts a card drag.
      await cardLink.tap();
      await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
      return;
    }
    await card.focus();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.runIssue}$`));
    await page.goBack();
    await card.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.runIssue}$`));
    await page.goBack();
    await cardLink.click();
    await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));

    await page.goto("/projects/AGT");
    await page.getByRole("button", { name: "List" }).click();
    const row = page.locator(`[data-issue-row="${seeded.runIssue}"]`);
    await expect(agentIn(row, runId)).toHaveAttribute("data-agent-state", "running");
    await row.focus();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.runIssue}$`));
    await page.goBack();
    await row.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.runIssue}$`));
    await page.goBack();
    await agentIn(row, runId)
      .locator(`a[href="${livePath(runId)}"]`)
      .click();
    await expect(page).toHaveURL(new RegExp(`${livePath(runId)}$`));
  } finally {
    await alice.close();
  }
});
