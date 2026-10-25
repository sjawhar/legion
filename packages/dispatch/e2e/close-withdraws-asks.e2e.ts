import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import type { Actor } from "../web/src/api/types";
import {
  createAsk,
  createIssue,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactVersion,
  getIssue,
  patchIssue,
  requestApproval,
} from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// Closing an issue withdraws each of its open asks, naming the closer, and reopening it brings
// back exactly those asks. Every row seeds an issue holding a plain ask, a decision block in its
// spec and an approval request on the spec, opens the issue page and the Inbox, and only then
// closes it, so what is checked is what a reader already looking at the issue sees.

test.beforeEach(async () => {
  await resetDatabase();
});

/** The agent that asks: every seeded ask waits on a person, so each is in alice's Inbox. */
const asker = { actor: { id: "e2e-seed", kind: "session" } as Actor, as: "agent" as const };

const decisionBlock =
  ':::ask{#decision urgency="high" multiple="false" state="open"}\nShip now?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n';

interface Seeded {
  /** Every open ask of the issue: the plain ask, the decision block's and the approval request. */
  askIds: string[];
  artifactId: string;
  key: string;
}

async function seedIssue(project: string): Promise<Seeded> {
  await createProject({ key: project, name: project });
  const issue = await createIssue({
    project,
    spec: "## Plan\n\nContext.\n",
    title: "Close withdraws asks",
  });
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ after: "end", markdown: decisionBlock, op: "insert" }] },
    asker
  );
  // The block is an ask once settlement has indexed it.
  await expect
    .poll(async () =>
      (await getIssue(issue.key)).open_asks.some((ask) => ask.block_id === "decision")
    )
    .toBe(true);
  const block = (await getIssue(issue.key)).open_asks.find((ask) => ask.block_id === "decision");
  if (block === undefined) {
    throw new Error("the decision block was not indexed");
  }
  const plain = await createAsk(issue.key, { question: "Which region first?" }, asker);
  const approval = await requestApproval(issue.primary_artifact_id, {}, asker);
  return {
    askIds: [plain.id, block.id, approval.ask.id],
    artifactId: issue.primary_artifact_id,
    key: issue.key,
  };
}

interface Opened {
  decision: Locator;
  inbox: Page;
  page: Page;
}

/** Opens alice's Inbox and the issue page, and waits until both show every ask open. */
async function openIssueAndInbox(browser: Browser, seeded: Seeded): Promise<Opened> {
  const alice = await asUser(browser, "alice");
  const inbox = await alice.newPage();
  await inbox.goto("/");
  for (const id of seeded.askIds) {
    await expect(inbox.getByTestId(`ask-${id}`)).toBeVisible();
  }
  const page = await alice.newPage();
  await page.goto(`/issues/${seeded.key}`);
  const decision = documentEditor(page).locator('[data-dispatch-ask-block="decision"]');
  await expectOpenDecision(decision);
  await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (3)");
  await expect(page.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
  return { decision, inbox, page };
}

/** The block hosts the open, answerable card. */
async function expectOpenDecision(decision: Locator): Promise<void> {
  await expect(decision.getByRole("radio", { name: "Ship Release it" })).toBeVisible();
  await expect(
    decision.getByRole("button", { name: "Add a note or answer in your own words" })
  ).toBeVisible();
  await expect(decision.getByTestId("ask-resolution-badge")).toHaveCount(0);
}

/** What the issue page and the Inbox show once the close has withdrawn the asks. */
async function expectWithdrawn(
  { decision, inbox, page }: Opened,
  seeded: Seeded,
  closer: string
): Promise<void> {
  await expect(page.getByRole("button", { name: "Reopen" })).toBeVisible();
  await expect(page.getByTestId("issue-whose-turn")).toHaveCount(0);
  await expect(decision.getByTestId("ask-resolution-badge")).toHaveText("Withdrawn");
  await expect(decision).toContainText(`Withdrawn when ${closer} closed the issue`);
  // The server's reason repeats the sentence, so the card does not show it.
  await expect(decision).not.toContainText("Withdrawn by the close of");
  await expect(decision.getByRole("radio")).toHaveCount(0);
  await expect(
    decision.getByRole("button", { name: "Add a note or answer in your own words" })
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Awaiting approval" })).toHaveCount(0);
  for (const id of seeded.askIds) {
    await expect(inbox.getByTestId(`ask-${id}`)).toHaveCount(0);
  }
}

test("a person's Close withdraws the issue's asks, an earlier version keeps its open block, and Reopen brings the asks back", async ({
  browser,
}) => {
  const seeded = await seedIssue("CLOSE");
  const opened = await openIssueAndInbox(browser, seeded);
  const { decision, inbox, page } = opened;
  try {
    await page.getByRole("button", { name: "Close issue" }).click();
    await expectWithdrawn(opened, seeded, "alice");

    // The newest saved version predates the close: opened from the history, it still shows the
    // decision as it was then, open, and nothing of the withdrawal.
    const { versions } = await getArtifact(seeded.artifactId, { login: "alice" });
    const latest = Math.max(...versions.map((version) => version.number));
    expect((await getArtifactVersion(seeded.artifactId, latest)).markdown).toContain(
      ":::ask{#decision"
    );
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(latest));
    const saved = page
      .getByRole("region", { name: `Document version ${latest}` })
      .locator('[data-dispatch-ask-block="decision"]');
    await expect(saved).toContainText("Ship now?");
    await expect(saved.locator('ul[aria-label="Options"] li')).toHaveCount(2);
    await expect(saved.locator("[data-dispatch-ask-asked]")).toBeVisible();
    await expect(saved.getByTestId("ask-resolution-badge")).toHaveCount(0);
    await expect(saved).not.toContainText("Withdrawn");
    await expect(saved).not.toContainText("Resolved");
    await page.getByRole("combobox", { name: "Version" }).selectOption("");

    await page.getByRole("button", { name: "Reopen" }).click();
    await expect(page.getByRole("button", { name: "Close issue" })).toBeVisible();
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (3)");
    // The reopen wrote the block back to open, so the record is gone at once.
    await expect(decision.getByTestId("ask-resolution-badge")).toHaveCount(0);
    await expect(decision).not.toContainText("Withdrawn");
    await expect(decision.locator('ul[aria-label="Options"] li')).toHaveCount(2);
    await expect(page.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
    for (const id of seeded.askIds) {
      await expect(inbox.getByTestId(`ask-${id}`)).toBeVisible();
    }
    // The page admitted its document read-only while the issue was closed, and keeps that
    // admission until a reload; reloaded, the block hosts the open, answerable card again.
    await page.reload();
    await expectOpenDecision(decision);
  } finally {
    await page.context().close();
  }
});

test("an agent session's close names the session on the withdrawn decision", async ({
  browser,
}) => {
  const seeded = await seedIssue("AGENT");
  const opened = await openIssueAndInbox(browser, seeded);
  try {
    await patchIssue(
      seeded.key,
      { status: "done" },
      { actor: { id: "closer-session", kind: "session" }, as: "agent" }
    );
    await expectWithdrawn(opened, seeded, "session:closer-s…");
  } finally {
    await opened.page.context().close();
  }
});

test("the Legion daemon's close names the daemon whole on the withdrawn decision", async ({
  browser,
}) => {
  const seeded = await seedIssue("DAEMON");
  const opened = await openIssueAndInbox(browser, seeded);
  try {
    // The request the daemon's `SetStatus` sends: its bearer and its own session identity.
    await patchIssue(
      seeded.key,
      { status: "done" },
      { actor: { id: "legion-daemon:E2E", kind: "session" }, as: "agent" }
    );
    await expectWithdrawn(opened, seeded, "legion-daemon:E2E");
  } finally {
    await opened.page.context().close();
  }
});
