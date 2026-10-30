import { expect, type Page, test } from "@playwright/test";
import type { InboxRow } from "../web/src/api/types";

import {
  answerAsk,
  createAsk,
  createIssue,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getInbox,
  patchIssue,
  requestApproval,
  resolveAsk,
} from "./api";
import { inboxList, inboxPane, selectedRow } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox's reading column (INFRA-575): a list of short rows beside the selected question's own
 * document, from 1280 px, with today's card below that width and in the drawer.
 *
 * Every document here is made up: this repository is public, so nothing names a real person, host
 * or project. The seed is built from `e2e/api.ts`, `e2e/seed.ts` and `e2e/users.ts` alone, so this
 * file runs against the commit before the column and fails there.
 */

const session = {
  actor: { id: "e2e-reading-pane", kind: "session" as const },
  as: "agent" as const,
};

/** The list the whole file reads: the cleared assignee filter, so the project document's own
 *  question - which nobody holds, a document having no assignee - is listed beside the rest. */
const INBOX = "/?assignee=everyone";

/** The address the Inbox writes for a selected row (`buildInboxPath`: the item, then the filter). */
function inboxAt(askId: string): string {
  return `/?item=${askId}&assignee=everyone`;
}

// ---------------------------------------------------------------------------
// The first spec: one section of 36 lines above the decision block.
// ---------------------------------------------------------------------------

const SECTION_HEADING = "Rollout order";
const DECISION_BLOCK = "rollout-order";
const DECISION_QUESTION = "Which service moves first?";
const sectionLines = Array.from(
  { length: 36 },
  (_, index) =>
    `Rollout note ${index + 1}: the ledger and the mirror each keep a clock of their own.`
);
const decisionSpec = [
  `# ${SECTION_HEADING}`,
  "",
  ...sectionLines.flatMap((line) => [line, ""]),
  `:::ask{#${DECISION_BLOCK} urgency="med" multiple="false" state="open"}`,
  DECISION_QUESTION,
  "",
  "- Ledger first",
  "- Mirror first",
  ":::",
  "",
].join("\n");

// ---------------------------------------------------------------------------
// The second spec: versions 3 and 4, 22 blocks apart, approved at 3.
// ---------------------------------------------------------------------------

const APPROVAL_HEADING = "Ledger rollout";
const PLACEHOLDER = "Placeholder for the rollout body.";
/** The two paragraphs both versions hold, which the removed block sat between. */
const SCOPE_ANCHOR = "Scope anchor: the ledger keeps a clock of its own in both versions.";
const WINDOW_ANCHOR = "Window anchor: the mirror keeps its retry budget in both versions.";
/** The block version 4 no longer holds. */
const RETIRED_BLOCK = "Retired note: the nightly mirror job runs at 02:00 every day.";
/** The one changed paragraph that kept all but one of its words. */
const WORDS_BEFORE = "The retry budget is three attempts per lease.";
const WORDS_AFTER = "The retry budget is seven attempts per lease.";
const REMOVED_WORD = "three";
const ADDED_WORD = "seven";
/** How many blocks version 4 changed since version 3. */
const CHANGED_BLOCKS = 22;
/** Which of them is the one that changed only some of its words. */
const WORD_LEVEL_STEP = 10;
const changedParagraphs = Array.from({ length: CHANGED_BLOCKS }, (_, index) =>
  index === WORD_LEVEL_STEP
    ? { after: WORDS_AFTER, before: WORDS_BEFORE }
    : {
        after: `Step ${index + 1}: the mirror reads the clock before the ledger writes it.`,
        before: `Step ${index + 1}: the ledger writes the clock before the mirror reads it.`,
      }
);
/** Version 3's body, inserted under the heading in one edit. */
const baselineBody = `${[
  SCOPE_ANCHOR,
  RETIRED_BLOCK,
  WINDOW_ANCHOR,
  ...changedParagraphs.map(({ before }) => before),
].join("\n\n")}\n`;

/** The four classes `features/doc/change-marks.ts` draws an approval's marks with. */
const CHANGE_CLASSES = [
  "dispatch-change-block",
  "dispatch-change-added",
  "dispatch-change-removed",
  "dispatch-change-removed-words",
];

// ---------------------------------------------------------------------------
// The project document's own decision, and the two questions that are cards.
// ---------------------------------------------------------------------------

const DOCUMENT_BLOCK = "mirror-window";
const documentContent = [
  "# Mirror runbook",
  "",
  "The mirror replays what the ledger wrote while it was away.",
  "",
  `:::ask{#${DOCUMENT_BLOCK} urgency="med" multiple="false" state="open"}`,
  "Which window does the mirror replay?",
  "",
  "- The last hour",
  "- The last day",
  ":::",
  "",
].join("\n");

const ANCHOR_QUOTE = "replays the ledger";
const ANCHORED_QUESTION = "Does a cold start replay the whole ledger?";
const PLAIN_QUESTION = "Should the mirror keep retrying after a refused lease?";

interface Seed {
  anchoredAskId: string;
  approvalArtifactId: string;
  approvalAskId: string;
  decisionAskId: string;
  decisionIssueKey: string;
  documentAskId: string;
  firstApprovalAskId: string;
  plainAskId: string;
}

/** The Inbox row `match` names, once the server has indexed the block or opened the question. */
async function indexedRow(match: (row: InboxRow) => boolean): Promise<InboxRow> {
  await expect.poll(async () => (await getInbox()).some(match)).toBe(true);
  const row = (await getInbox()).find(match);
  if (row === undefined) {
    throw new Error("the seeded question left the Inbox before it was read");
  }
  return row;
}

/**
 * Six made-up questions, in the order the Inbox lists them (the issue's priority first): the
 * decision block of a spec, the request to approve version 4 of a spec approved at version 3, a
 * first request to approve another document, an anchored question, a question in no document, and
 * the decision block of a project document, which no issue priority orders.
 */
async function seedInbox(): Promise<Seed> {
  await createProject({ key: "PANE", name: "Pane rollout" });

  const decisionIssue = await createIssue({
    project: "PANE",
    spec: decisionSpec,
    title: "Rollout order",
  });
  await patchIssue(decisionIssue.key, { priority: 0 });
  const decision = await indexedRow((row) => row.block_id === DECISION_BLOCK);

  const approvalIssue = await createIssue({
    project: "PANE",
    spec: `# ${APPROVAL_HEADING}\n\n${PLACEHOLDER}\n`,
    title: "Ledger rollout",
  });
  await patchIssue(approvalIssue.key, { priority: 1 });
  const approvalArtifactId = approvalIssue.primary_artifact_id;
  await editArtifact(
    approvalArtifactId,
    { ops: [{ after: "end", markdown: baselineBody, op: "insert" }], summary: "The rollout body" },
    session
  );
  await editArtifact(
    approvalArtifactId,
    { ops: [{ find: PLACEHOLDER, op: "delete" }], summary: "Drop the placeholder" },
    session
  );
  const atThree = await requestApproval(
    approvalArtifactId,
    { summary: "The ledger-first rollout." },
    session
  );
  expect(atThree.version).toBe(3);
  await answerAsk(
    atThree.ask.id,
    { expected_edited_at: null, selected: ["Approve"] },
    { login: "alice" }
  );
  await editArtifact(
    approvalArtifactId,
    {
      ops: [
        { find: RETIRED_BLOCK, op: "delete" as const },
        ...changedParagraphs.map(({ after, before }) => ({
          find: before,
          op: "replace" as const,
          with: after,
        })),
      ],
      summary: "The mirror-first rollout",
    },
    session
  );
  const atFour = await requestApproval(
    approvalArtifactId,
    { summary: "The mirror-first rollout." },
    session
  );
  expect(atFour.version).toBe(4);

  const firstApprovalIssue = await createIssue({
    project: "PANE",
    spec: "# Lease handover\n\nThe lease moves with the ledger, never with the mirror.\n",
    title: "Lease handover",
  });
  await patchIssue(firstApprovalIssue.key, { priority: 2 });
  const firstApproval = await requestApproval(
    firstApprovalIssue.primary_artifact_id,
    { summary: "The first handover draft." },
    session
  );
  expect(firstApproval.version).toBe(1);

  const anchoredIssue = await createIssue({
    project: "PANE",
    spec: "The mirror replays the ledger on a cold start.\n",
    title: "Cold start",
  });
  await patchIssue(anchoredIssue.key, { priority: 3 });
  const anchored = await createAsk(
    anchoredIssue.key,
    { anchor: { artifact: "spec", quote: ANCHOR_QUOTE }, question: ANCHORED_QUESTION },
    session
  );

  const plainIssue = await createIssue({ project: "PANE", title: "Retry budget" });
  await patchIssue(plainIssue.key, { priority: 3 });
  const plain = await createAsk(
    plainIssue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: PLAIN_QUESTION },
    session
  );

  await createProjectDocument("PANE", { content: documentContent, name: "mirror-runbook.md" });
  const documentDecision = await indexedRow((row) => row.block_id === DOCUMENT_BLOCK);

  return {
    anchoredAskId: anchored.id,
    approvalArtifactId,
    approvalAskId: atFour.ask.id,
    decisionAskId: decision.id,
    decisionIssueKey: decisionIssue.key,
    documentAskId: documentDecision.id,
    firstApprovalAskId: firstApproval.ask.id,
    plainAskId: plain.id,
  };
}

/** The reading column's two columns, as rendered boxes: the list and the pane beside it. */
async function expectTwoColumns(page: Page): Promise<void> {
  const list = inboxList(page);
  const pane = inboxPane(page);
  await expect(list).toBeVisible();
  await expect(pane).toBeVisible();
  const [listBox, paneBox] = await Promise.all([list.boundingBox(), pane.boundingBox()]);
  if (listBox === null || paneBox === null) {
    throw new Error("the Inbox drew no box for one of its two columns");
  }
  expect(paneBox.x).toBeGreaterThanOrEqual(listBox.x + listBox.width);
}

/**
 * Where the pane's scrolled column, its drawn section heading and the block `blockId` stand, in
 * viewport pixels. The column is the scrolling ancestor of the drawn document inside `inbox-pane`:
 * the pane holds its head above it, so the top of the column is not the top of the pane.
 */
function paneOffsets(page: Page, blockId: string) {
  return page.evaluate((id) => {
    const pane = document.querySelector<HTMLElement>('[data-testid="inbox-pane"]');
    if (pane === null) {
      throw new Error("the reading pane is not on the page");
    }
    const block = pane.querySelector<HTMLElement>(`[data-dispatch-ask-block="${CSS.escape(id)}"]`);
    const heading = pane.querySelector<HTMLElement>("h1");
    if (block === null || heading === null) {
      throw new Error("the pane has not drawn the document's heading and its decision block");
    }
    let column = pane;
    for (let node = heading.parentElement; node !== null && node !== pane; ) {
      const overflow = window.getComputedStyle(node).overflowY;
      if (overflow === "auto" || overflow === "scroll") {
        column = node;
        break;
      }
      node = node.parentElement;
    }
    const columnBox = column.getBoundingClientRect();
    return {
      block: block.getBoundingClientRect().top,
      columnBottom: columnBox.bottom,
      columnTop: columnBox.top,
      heading: heading.getBoundingClientRect().top,
    };
  }, blockId);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the Inbox shows a decision block in the reading pane and answers it in two taps", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const list = inboxList(page);
    const pane = inboxPane(page);
    const block = pane.locator(`[data-dispatch-ask-block="${DECISION_BLOCK}"]`);

    // The column shows from 1280 px, so both widths draw the same two columns, select the same
    // first row, and place the same section heading at the top of the right column.
    for (const width of [1440, 1280]) {
      await page.setViewportSize({ height: 900, width });
      await page.goto(INBOX);
      await expectTwoColumns(page);
      await expect(list.locator("[data-inbox-row]").first()).toHaveAttribute(
        "data-inbox-selected",
        "true"
      );
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
      await expect(block).toContainText(DECISION_QUESTION);
      await expect(pane.getByTestId("pane-section-bar")).toBeVisible();
      // The column places the section's heading at its top, with the question below it. The
      // editor draws its blocks after the socket syncs, so the placement is polled for.
      await expect
        .poll(async () => {
          const offsets = await paneOffsets(page, DECISION_BLOCK);
          return offsets.heading - offsets.columnTop;
        })
        .toBeLessThanOrEqual(8);
      const placed = await paneOffsets(page, DECISION_BLOCK);
      expect(placed.heading - placed.columnTop).toBeGreaterThanOrEqual(0);
      expect(placed.block).toBeGreaterThan(placed.heading);
      // 36 lines stand between the heading and the question, so the answer is off the column.
      expect(placed.block).toBeGreaterThan(placed.columnBottom);
    }

    // "Skip to the answer" brings the decision block into the column it heads.
    await pane.getByRole("button", { name: "Skip to the answer" }).click();
    await expect
      .poll(async () => {
        const offsets = await paneOffsets(page, DECISION_BLOCK);
        return offsets.block >= offsets.columnTop - 1 && offsets.block < offsets.columnBottom;
      })
      .toBe(true);

    // Two taps answer it: the option, then Answer. The note disclosure stays shut - a reader who
    // picks an option is done - and nothing else is pressed.
    const card = block.locator("article[data-testid^=ask-]");
    const disclosure = card.getByRole("button", {
      name: "Add a note or answer in your own words",
    });
    await expect(disclosure).toHaveAttribute("aria-expanded", "false");
    await card.getByRole("radio", { exact: true, name: "Ledger first" }).click();
    await expect(disclosure).toHaveAttribute("aria-expanded", "false");
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    // The answered row leaves the list and the Inbox read behind it, and the next row is shown.
    await expect(list.locator(`[data-inbox-row="${seed.decisionAskId}"]`)).toHaveCount(0);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
    await expect
      .poll(async () => (await getInbox()).some((row) => row.id === seed.decisionAskId))
      .toBe(false);
  } finally {
    await alice.close();
  }
});

test("the reading pane approves a version and marks what changed since the last approval", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);
    await page.goto(inboxAt(seed.approvalAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);

    // The column shows the version Approve approves, with Approve at its head. The request is a
    // fresh one at version 4, so the chip is "Awaiting approval" and its action is plain Approve.
    const approve = pane.locator("header").getByRole("button", { exact: true, name: "Approve" });
    await expect(approve).toBeVisible();
    await expect(pane.getByRole("region", { name: "Document version 4" })).toBeVisible();

    // Every block changed since the approved version 3 carries the changed-block class.
    const changed = pane.locator(".dispatch-change-block");
    await expect(changed).toHaveCount(CHANGED_BLOCKS);

    // Inside one changed paragraph the word marks cover the words that changed, and nothing else.
    const words = pane.locator(".dispatch-change-block", { hasText: "The retry budget is" });
    await expect(words).toHaveCount(1);
    await expect(words.locator(".dispatch-change-added")).toHaveText([ADDED_WORD]);
    await expect(words.locator(".dispatch-change-removed-words")).toHaveText([REMOVED_WORD]);

    // The removed block is drawn struck through between the two blocks it sat between.
    const removed = pane.locator(".dispatch-change-removed");
    await expect(removed).toHaveText([RETIRED_BLOCK]);
    const placement = await removed.evaluate((node) => ({
      after: node.nextElementSibling?.textContent ?? null,
      before: node.previousElementSibling?.textContent ?? null,
      struck: window.getComputedStyle(node).textDecorationLine,
    }));
    expect(placement.before).toBe(SCOPE_ANCHOR);
    expect(placement.after).toBe(WINDOW_ANCHOR);
    expect(placement.struck).toContain("line-through");

    // A paragraph both versions hold carries no mark at all.
    const unchanged = pane.locator("p", { hasText: SCOPE_ANCHOR });
    await expect(unchanged).toHaveCount(1);
    await expect(unchanged).not.toHaveClass(/dispatch-change-block/);
    await expect(
      unchanged.locator(".dispatch-change-added, .dispatch-change-removed-words")
    ).toHaveCount(0);

    // One tap approves the version the column is showing.
    await approve.click();
    await expect
      .poll(async () => (await getArtifact(seed.approvalArtifactId, { login: "alice" })).approval)
      .toMatchObject({ state: "approved", version: 4 });
    await expect(inboxList(page).locator(`[data-inbox-row="${seed.approvalAskId}"]`)).toHaveCount(
      0
    );

    // A first request has no approval to compare against, so the column marks nothing.
    await page.goto(inboxAt(seed.firstApprovalAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.firstApprovalAskId);
    await expect(pane.getByRole("region", { name: "Document version 1" })).toBeVisible();
    for (const mark of CHANGE_CLASSES) {
      await expect(pane.locator(`.${mark}`)).toHaveCount(0);
    }
  } finally {
    await alice.close();
  }
});

test("the reading pane shows an anchored question and a question in no document as cards", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.anchoredAskId));
    await expectTwoColumns(page);
    const anchoredCard = pane.getByTestId(`ask-${seed.anchoredAskId}`);
    await expect(anchoredCard).toContainText(ANCHORED_QUESTION);
    await expect(anchoredCard).toContainText(ANCHOR_QUOTE);
    await expect(anchoredCard.locator("[data-ask-answer]")).toBeVisible();

    await page.goto(inboxAt(seed.plainAskId));
    const plainCard = pane.getByTestId(`ask-${seed.plainAskId}`);
    await expect(plainCard).toContainText(PLAIN_QUESTION);
    await expect(plainCard.locator("[data-ask-answer]")).toBeVisible();
  } finally {
    await alice.close();
  }
});

test("the arrow keys, j, k, Enter and ?item= drive the reading pane", async ({ browser }) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);

    // A bare address selects the first row and writes it into the address.
    await page.goto(INBOX);
    await expect(page).toHaveURL(inboxAt(seed.decisionAskId));

    // `j` and `k` move the selection whether or not the list holds the focus.
    await page.keyboard.press("j");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
    await page.keyboard.press("k");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);

    // `?item=` carries the selection, and a reload keeps it.
    await page.goto(inboxAt(seed.plainAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.plainAskId);
    await page.reload();
    await expect(page).toHaveURL(inboxAt(seed.plainAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.plainAskId);

    // Enter puts the focus in the column's answer box; Escape hands it back to the row, where
    // the arrows move the selection.
    await page.keyboard.press("Enter");
    await expect(pane.locator("[data-ask-answer]")).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.locator(`[data-inbox-row="${seed.plainAskId}"]`)).toBeFocused();
    await page.keyboard.press("ArrowUp");
    await expect(selectedRow(page)).not.toHaveAttribute("data-inbox-row", seed.plainAskId);
    await page.keyboard.press("ArrowDown");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.plainAskId);

    // An `?item=` this list cannot answer leaves the first row selected and rewrites the address.
    await page.goto("/?assignee=everyone&item=00000000-0000-7000-8000-000000000000");
    await expect(page).toHaveURL(inboxAt(seed.decisionAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
  } finally {
    await alice.close();
  }
});

test("the Inbox keeps today's card below 1280 px and in the drawer @phone", async ({ browser }) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // One pixel below the column's width the Inbox is the list of cards it has always been.
    await page.setViewportSize({ height: 900, width: 1279 });
    await page.goto(INBOX);
    await expect(inboxList(page)).toBeVisible();
    await expect(inboxPane(page)).toHaveCount(0);
    const decisionCard = page.getByTestId(`ask-${seed.decisionAskId}`);
    await expect(decisionCard.locator("[data-ask-answer]")).toBeVisible();
    await expect(decisionCard.getByRole("link", { name: "Open in document" })).toBeVisible();

    // And so it is on a phone.
    await page.setViewportSize({ height: 844, width: 390 });
    await page.goto(INBOX);
    await expect(inboxPane(page)).toHaveCount(0);
    await expect(decisionCard.locator("[data-ask-answer]")).toBeVisible();
    await expect(decisionCard.getByRole("link", { name: "Open in document" })).toBeVisible();

    // The approval card's own diff is the change view, with the same marks the column draws.
    const approvalCard = page.getByTestId(`ask-${seed.approvalAskId}`);
    await approvalCard.getByRole("button", { name: "Show diff" }).click();
    const diff = approvalCard.getByTestId("change-view");
    const changed = diff.locator('[data-change="changed"]');
    await expect(changed).toHaveCount(CHANGED_BLOCKS);
    for (let index = 0; index < CHANGED_BLOCKS; index += 1) {
      await expect(changed.nth(index).locator("ins, del")).not.toHaveCount(0);
    }
    const words = diff.locator('[data-change="changed"]', { hasText: "The retry budget is" });
    await expect(words.locator("del")).toHaveText([REMOVED_WORD]);
    await expect(words.locator("ins")).toHaveText([ADDED_WORD]);
    const unchanged = diff.locator('[data-change="unchanged"]', { hasText: SCOPE_ANCHOR });
    await expect(unchanged.locator("ins, del")).toHaveCount(0);
    // The removed block is struck through where it stood: between the same two blocks.
    const blocks = await diff.locator("[data-change]").allTextContents();
    const at = blocks.indexOf(RETIRED_BLOCK);
    expect(at).toBeGreaterThan(0);
    expect(blocks[at - 1]).toBe(SCOPE_ANCHOR);
    expect(blocks[at + 1]).toBe(WINDOW_ANCHOR);
    await expect(diff.locator('[data-change="removed"] del')).toHaveText([RETIRED_BLOCK]);

    // The drawer is a peek beside the page it covers, so it keeps the card at every width.
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${seed.decisionIssueKey}`);
    await page
      .getByRole("navigation", { name: "Navigation" })
      .getByRole("button", { name: "Peek at the inbox without leaving this page" })
      .click();
    const drawer = page.getByRole("dialog", { name: "Inbox" });
    await expect(drawer).toBeVisible();
    await expect(drawer.getByTestId("inbox-pane")).toHaveCount(0);
    const drawerCard = drawer.getByTestId(`ask-${seed.decisionAskId}`);
    await expect(drawerCard.locator("[data-ask-answer]")).toBeVisible();
    await expect(drawerCard.getByRole("link", { name: "Open in document" })).toBeVisible();
  } finally {
    await alice.close();
  }
});

/**
 * Whether the reading column holds `text` where a reader can read and copy it: as drawn text, or
 * as the value of a box it keeps. The column may keep a reader's unsent answer either way, so the
 * row reads the behaviour rather than one markup.
 */
function paneHoldsText(page: Page, text: string): Promise<boolean> {
  return page.evaluate((wanted) => {
    const pane = document.querySelector<HTMLElement>('[data-testid="inbox-pane"]');
    if (pane === null) return false;
    if ((pane.textContent ?? "").includes(wanted)) return true;
    return [
      ...pane.querySelectorAll<HTMLTextAreaElement | HTMLInputElement>("textarea, input"),
    ].some((field) => field.value.includes(wanted));
  }, text);
}

test("a question the server withdraws leaves the list at once and the next row is selected", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const list = inboxList(page);
    const pane = inboxPane(page);
    await page.goto(INBOX);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);

    // The server takes the selected question away. The reader has typed nothing into it: the
    // Inbox selected its row for them when the page opened.
    await resolveAsk(
      seed.decisionAskId,
      { kind: "retracted", reason: "A newer question supersedes this one." },
      session
    );

    // It leaves at once, with no reload and no second look: a withdrawn question with a live
    // answer form invites an answer the server refuses. The next visible row in list order is
    // selected, as after an answer in the column, and the address follows it.
    await expect(list.locator(`[data-inbox-row="${seed.decisionAskId}"]`)).toHaveCount(0);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
    await expect(page).toHaveURL(inboxAt(seed.approvalAskId));
    await expect(
      pane.locator("header").getByRole("button", { exact: true, name: "Approve" })
    ).toBeVisible();
    await expect(pane.getByTestId(`ask-${seed.decisionAskId}`)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a withdrawn question the reader was answering keeps its text in the column, closed", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  const typed = "Only on a cold start, and only back to the last lease.";
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);
    await page.goto(inboxAt(seed.anchoredAskId));
    const card = pane.getByTestId(`ask-${seed.anchoredAskId}`);
    await expect(card).toContainText(ANCHORED_QUESTION);

    // The reader is writing an answer to this question when the server takes it away.
    await card.locator("[data-ask-answer]").fill(typed);
    await resolveAsk(
      seed.anchoredAskId,
      { kind: "retracted", reason: "Answered on another issue." },
      session
    );

    // The one exception to the rule above: the column keeps the question, shows its closed state
    // in place of the answer form, and keeps what the reader typed where they can copy it.
    await expect(card.getByTestId("ask-resolution-badge")).toBeVisible();
    await expect(card.getByRole("button", { exact: true, name: "Answer" })).toHaveCount(0);
    await expect(card.locator("[data-ask-answer]")).toHaveCount(0);
    await expect.poll(() => paneHoldsText(page, typed)).toBe(true);

    // Until the reader selects another row, which shows that row's question and nothing of this
    // one: the question is gone from the Inbox and its row with it.
    await page.keyboard.press("j");
    await expect(selectedRow(page)).not.toHaveAttribute("data-inbox-row", seed.anchoredAskId);
    await expect(pane.getByTestId(`ask-${seed.anchoredAskId}`)).toHaveCount(0);
    await expect(inboxList(page).locator(`[data-inbox-row="${seed.anchoredAskId}"]`)).toHaveCount(
      0
    );
  } finally {
    await alice.close();
  }
});

test("an options-only answer the server refuses keeps its question in the column, with a Retry", async ({
  browser,
}) => {
  const seed = await seedInbox();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const list = inboxList(page);
    const pane = inboxPane(page);
    await page.goto(INBOX);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);

    // The server refuses this one answer, in the words of its own route, and refuses it for a
    // reason the same answer may get past: the card offers Retry rather than closing its form.
    await page.route(`**/api/v1/asks/${seed.decisionAskId}/answer`, (route) =>
      route.fulfill({
        body: JSON.stringify({
          code: "ASK_REVISION_REQUIRED",
          error: "expected_edited_at is required",
        }),
        contentType: "application/json",
        status: 400,
      })
    );

    // The two taps of the row above, and nothing typed: with no draft to keep the question
    // here, the answer itself is what holds it.
    const block = pane.locator(`[data-dispatch-ask-block="${DECISION_BLOCK}"]`);
    await expect(block).toContainText(DECISION_QUESTION);
    await pane.getByRole("button", { name: "Skip to the answer" }).click();
    const card = block.locator("article[data-testid^=ask-]");
    await card.getByRole("radio", { exact: true, name: "Ledger first" }).click();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    // The refusal is in the card that wrote it, on the question it was written to, and the
    // reader can send the same answer again from there. The selection never moved on, and the
    // row the optimistic removal took is listed again - the server still has the question open.
    await expect(card.getByRole("alert")).toContainText("Could not save your answer.");
    await expect(card.getByRole("alert").getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(block).toContainText(DECISION_QUESTION);
    await expect(page).toHaveURL(inboxAt(seed.decisionAskId));
    await expect(list.locator(`[data-inbox-row="${seed.decisionAskId}"]`)).toHaveCount(1);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.decisionAskId);
    await expect
      .poll(async () => (await getInbox()).some((row) => row.id === seed.decisionAskId))
      .toBe(true);
  } finally {
    await alice.close();
  }
});
