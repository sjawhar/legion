import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { RECONNECT_SPREAD_MS } from "../web/src/api/live-socket";
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
import { connectedDot, countDocumentSockets, documentEditor } from "./editor";
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

/** Every fact this file polls for lands in milliseconds: Playwright's default 100/250/500/1000
 *  ladder would first see it a quarter-second late. */
const PROBE = { intervals: [50, 50, 100, 200, 500] };

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
    .poll(
      async () => (await getIssue(issue.key)).open_asks.some((ask) => ask.block_id === "decision"),
      PROBE
    )
    .toBe(true);
  const block = (await getIssue(issue.key)).open_asks.find((ask) => ask.block_id === "decision");
  if (block === undefined) {
    throw new Error("the decision block was not indexed");
  }
  // Neither write reads the other, and the ids they return are the only order the row keeps.
  const [plain, approval] = await Promise.all([
    createAsk(issue.key, { question: "Which region first?" }, asker),
    requestApproval(issue.primary_artifact_id, {}, asker),
  ]);
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
  /** The document sockets the issue page has opened for the issue's spec, open or closed. */
  sockets: () => number;
}

/** Opens alice's Inbox and the issue page, and waits until both show every ask open. */
async function openIssueAndInbox(
  browser: Browser,
  seeded: Seeded,
  { clock = false }: { clock?: boolean } = {}
): Promise<Opened> {
  const alice = await asUser(browser, "alice");
  const inbox = await alice.newPage();
  const page = await alice.newPage();
  const sockets = countDocumentSockets(page, seeded.artifactId);
  // Neither page waits on the other, and each keeps its own checks below.
  if (clock) await page.clock.install();
  await Promise.all([inbox.goto("/"), page.goto(`/issues/${seeded.key}`)]);
  for (const id of seeded.askIds) {
    await expect(inbox.getByTestId(`ask-${id}`)).toBeVisible();
  }
  const decision = documentEditor(page).locator('[data-dispatch-ask-block="decision"]');
  await expectOpenDecision(decision);
  await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (3)");
  await expect(page.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
  return { decision, inbox, page, sockets };
}

/** Spends the document socket's reconnect draw - a uniform [0, RECONNECT_SPREAD_MS) set at the
 *  previous open (`web/src/features/doc/connection.ts`) - on the page's own clock, until the
 *  issue page holds `count` document sockets. The close that arms the draw may not have reached
 *  the page when the first jump lands, so the jump repeats. */
async function reconnectedSockets({ page, sockets }: Opened, count: number): Promise<void> {
  await expect
    .poll(async () => {
      await page.clock.fastForward(RECONNECT_SPREAD_MS);
      return sockets();
    }, PROBE)
    .toBe(count);
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

// SLOW(>=2s): two pages of the issue, one of them the document editor with its own socket, then a
// close, the version history, a reopen and a reload.
test("a person's Close withdraws the issue's asks, an earlier version keeps its open block, and Reopen brings the asks back", async ({
  browser,
}) => {
  const seeded = await seedIssue("CLOSE");
  const opened = await openIssueAndInbox(browser, seeded, { clock: true });
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

    // The close dropped the document's socket, and its reconnect is admitted read-only while the
    // issue is closed: the page a person who reopens after the draw holds, which the Reopen below
    // must make writable. The document socket's first reconnect is a uniform draw from [0, 2 s)
    // (#2323) the page's own clock spends, so a row waits for the state the page settles in and
    // never depends on which side of the draw a run lands. A reload cannot make a closed issue's
    // document editable, so the page names none.
    await reconnectedSockets(opened, 2);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "false");
    await expect(page.getByText("Reload to edit.")).toHaveCount(0);
    await page.getByRole("button", { name: "Reopen" }).click();
    await expect(page.getByRole("button", { name: "Close issue" })).toBeVisible();
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (3)");
    // The reopen wrote the block back to open, so the record is gone at once, and it dropped the
    // document's socket again, so its reconnect, a draw of its own, is admitted writable: the row
    // waits for the page that settles, editable, with the block hosting the open, answerable card,
    // and no reload.
    await expect(decision.getByTestId("ask-resolution-badge")).toHaveCount(0);
    await expect(decision).not.toContainText("Withdrawn");
    await reconnectedSockets(opened, 3);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect(documentEditor(page)).toHaveAttribute("contenteditable", "true");
    await expect(page.getByText("Reload to edit.")).toHaveCount(0);
    await expectOpenDecision(decision);
    await expect(decision.getByRole("radio", { name: "Hold Wait for review" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Awaiting approval" })).toBeVisible();
    for (const id of seeded.askIds) {
      await expect(inbox.getByTestId(`ask-${id}`)).toBeVisible();
    }
    // Reloaded, the block still hosts the open, answerable card. The reload opens a document
    // socket of its own, whose draw the page's clock spends as it did the two before it.
    await page.reload();
    await reconnectedSockets(opened, 4);
    await expect(connectedDot(page)).toHaveText("connected");
    await expectOpenDecision(decision);
  } finally {
    await page.context().close();
  }
});

// SLOW(>=2s): the six-call seed and two pages, one of them the document editor with its own
// socket, before the close is made.
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

// SLOW(>=2s): the six-call seed and two pages, one of them the document editor with its own
// socket, before the close is made.
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

test("a closed issue's document names no unsent edit to a person who typed nothing", async ({
  browser,
}) => {
  // A closed issue whose spec an agent wrote, opened by a person who types nothing: the page must
  // not tell them an edit saved in their browser cannot be sent.
  await createProject({ key: "READ", name: "READ" });
  const issue = await createIssue(
    { project: "READ", spec: "## Plan\n\nContext.\n", title: "Closed" },
    asker
  );
  await patchIssue(issue.key, { status: "done" }, asker);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The second load reads what the first left in this browser's store of unsent edits.
    for (const load of ["first", "reload"]) {
      if (load === "first") {
        await page.goto(`/issues/${issue.key}/spec`);
      } else {
        await page.reload();
      }
      await expect(documentEditor(page)).toContainText("Context.");
      await expect(
        page.getByText("This issue is closed. Its document is read-only.")
      ).toBeVisible();
      await expect(connectedDot(page)).toHaveAccessibleName("connected");
    }
  } finally {
    await alice.close();
  }
});
