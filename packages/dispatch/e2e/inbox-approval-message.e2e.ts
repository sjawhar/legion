import { expect, test } from "@playwright/test";

import {
  answerAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { holdFirstRequest } from "./editor";
import { inboxPane, paneScroller, selectedRow } from "./inbox-pane";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The question an approval request asks, in the Inbox's reading column (INFRA-963): the
 * requester's own words, rendered as markdown above the document inside the column's one
 * scroller, and drawn by the question's own card instead wherever the column cannot open that
 * document - so the question is on screen once either way.
 *
 * Every document here is made up: this repository is public, so nothing names a real person, host
 * or project. The seed is built from `e2e/api.ts`, `e2e/seed.ts` and `e2e/users.ts` alone, so this
 * file runs against the commit before the block and fails there.
 *
 * No row is tagged `@phone`: the column is drawn from `READING_PANE_WIDTH`, which the `chromium`
 * project's own 1280 px meets and the `iphone` project's 390 px never does.
 */

const session = {
  actor: { id: "e2e-approval-message", kind: "session" as const },
  as: "agent" as const,
};

const PROJECT = "MSG";

/** The list every row reads: the cleared assignee filter, so a question nobody holds is listed. */
const INBOX = "/?assignee=everyone";

/** The address the Inbox writes for a selected row (`buildInboxPath`: the item, then the filter). */
function inboxAt(askId: string): string {
  return `/?item=${askId}&assignee=everyone`;
}

/** The column's own read of the document, which is the read every row here holds or refuses: the
 *  document's metadata, not its text, and no other route under that id. */
function documentRead(artifactId: string): RegExp {
  return new RegExp(`/api/v1/artifacts/${artifactId}$`, "u");
}

// ---------------------------------------------------------------------------
// The document every row approves, and the three questions asked about it.
// ---------------------------------------------------------------------------

const WORDS_BEFORE = "The retry budget is three attempts per lease.";
const WORDS_AFTER = "The retry budget is seven attempts per lease.";
const REMOVED_WORD = "three";
const ADDED_WORD = "seven";
const SPEC = [
  "# Mirror rollout",
  "",
  "The mirror replays what the ledger wrote while it was away.",
  "",
  WORDS_BEFORE,
  "",
].join("\n");

/** A question of two paragraphs, with a list and a link in it: what a plain-text block would
 *  show as its own markup, and a rendered one shows as list items and an anchor. */
const RICH_OPENING = "The ledger moves onto the mirror clock this week, in two steps:";
const RICH_STEPS = [
  "the ledger stops writing its own clock;",
  "the mirror answers for it from then on.",
];
const RICH_LINK_TEXT = "the rollout note";
const RICH_LINK_HREF = "https://example.test/rollout-note";
const RICH_CLOSING = `The window and the rollback are written down in ${RICH_LINK_TEXT}.`;
const RICH_SUMMARY = [
  RICH_OPENING,
  "",
  ...RICH_STEPS.map((step) => `- ${step}`),
  "",
  `The window and the rollback are written down in [${RICH_LINK_TEXT}](${RICH_LINK_HREF}).`,
].join("\n");

/** The question the held row asks: one line, since that row is about when the block is drawn. */
const HELD_SUMMARY = "The mirror takes the clock on Thursday.";

/** The question the refused row asks. Its mark sits in the second paragraph on purpose: a short
 *  row shows the question's first line alone (`features/inbox/question-line.ts`), so a count of
 *  the mark over the whole page counts the drawn question body and nothing else. */
const FALLBACK_MARK = "The fallback note names the window and who signs it off.";
const FALLBACK_SUMMARY = `The export window moves to Sunday.\n\n${FALLBACK_MARK}`;

/** The question the two-document row asks. */
const TWO_DOCUMENT_SUMMARY = "The live sync replaces the nightly export.";

const VERIFICATION = [
  "# Verification",
  "",
  "The mirror answers a replay request within one minute of the lease moving.",
  "",
].join("\n");

interface Seed {
  artifactId: string;
  askId: string;
  /** The question the server wrote: its own `Approve …?` prefix, then the summary. */
  question: string;
  specName: string;
}

/** One issue, one spec at version 1, and one open request to approve it with `summary`. */
async function seedApproval(summary: string): Promise<Seed> {
  await createProject({ key: PROJECT, name: "Mirror rollout" });
  const issue = await createIssue({ project: PROJECT, spec: SPEC, title: "Mirror rollout" });
  const artifactId = issue.primary_artifact_id;
  const opened = withOpenedAsk(await requestApproval(artifactId, { summary }, session));
  expect(opened.version).toBe(1);
  const spec = await getArtifact(artifactId);
  expect(opened.ask.question).toBe(`Approve ${spec.name} (version 1)? ${summary}`);
  return {
    artifactId,
    askId: opened.ask.id,
    question: opened.ask.question,
    specName: spec.name,
  };
}

/**
 * The same issue with a Verification document beside its spec, the spec approved at version 1 and
 * edited since, and one request - opened with `include_verification` - to approve version 2 of the
 * spec and version 1 of the Verification document together.
 */
async function seedTwoDocuments(): Promise<Seed> {
  await createProject({ key: PROJECT, name: "Mirror rollout" });
  const issue = await createIssue({ project: PROJECT, spec: SPEC, title: "Mirror rollout" });
  const artifactId = issue.primary_artifact_id;

  // An approval of version 1 is what the request below is marked against, so the document the
  // column draws beside the question carries the marks of what changed since.
  const first = withOpenedAsk(
    await requestApproval(artifactId, { summary: "The first draft." }, session)
  );
  await answerAsk(
    first.ask.id,
    { expected_edited_at: null, selected: ["Approve"] },
    { login: "alice" }
  );
  await editArtifact(
    artifactId,
    {
      ops: [{ find: WORDS_BEFORE, op: "replace", with: WORDS_AFTER }],
      summary: "The longer retry budget",
    },
    session
  );

  const verification = await createIssueArtifact(issue.key, {
    content: VERIFICATION,
    name: "verification.md",
  });
  const opened = withOpenedAsk(
    await requestApproval(
      artifactId,
      { include_verification: true, summary: TWO_DOCUMENT_SUMMARY },
      session
    )
  );
  expect(opened.version).toBe(2);
  const spec = await getArtifact(artifactId);
  // `docs.ApprovalQuestion` writes every document the request names, each with its own version.
  expect(opened.ask.question).toBe(
    `Approve ${spec.name} (version 2) and ${verification.artifact.name} (version 1)? ` +
      TWO_DOCUMENT_SUMMARY
  );
  return {
    artifactId,
    askId: opened.ask.id,
    question: opened.ask.question,
    specName: spec.name,
  };
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("the reading pane draws the request's whole question as markdown above the document", async ({
  browser,
}) => {
  const seed = await seedApproval(RICH_SUMMARY);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pane = inboxPane(page);

    // Nothing is clicked: the Inbox selects the first row when it opens, and this list holds one.
    await page.goto(INBOX);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.askId);

    // The whole question, not the one line a short row shows: the server's own prefix with the
    // summary's first paragraph, and then the parts that only a renderer draws - the list as list
    // items, and the link as an anchor to where it points.
    const question = pane.getByTestId("pane-approval-question");
    await expect(question).toContainText(`Approve ${seed.specName} (version 1)? ${RICH_OPENING}`);
    await expect(question.getByRole("listitem")).toHaveText(RICH_STEPS);
    await expect(question.getByRole("link", { name: RICH_LINK_TEXT })).toHaveAttribute(
      "href",
      RICH_LINK_HREF
    );
    await expect(question).toContainText(RICH_CLOSING);

    // In the column's one scroller, above the document: a reader who scrolls past the question
    // has the whole column for the document under it.
    await expect(paneScroller(page).getByTestId("pane-approval-question")).toHaveCount(1);
    const drawn = pane.getByRole("region", { name: "Document version 1" });
    await expect(drawn).toBeVisible();
    const questionFirst = await drawn.evaluate((version) => {
      const block = document.querySelector('[data-testid="pane-approval-question"]');
      if (block === null) {
        throw new Error("the pane drew no approval question beside the document it drew");
      }
      return (block.compareDocumentPosition(version) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    });
    expect(questionFirst).toBe(true);
  } finally {
    await alice.close();
  }
});

test("the approval question stands while the document's skeleton does", async ({ browser }) => {
  const seed = await seedApproval(HELD_SUMMARY);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pane = inboxPane(page);

    // Installed before the navigation, so the column's read of the document is held from the
    // first attempt it makes and the document's place holds the skeleton.
    const read = await holdFirstRequest(page, documentRead(seed.artifactId));
    await page.goto(inboxAt(seed.askId));

    const opening = pane.getByRole("status", { name: `Opening ${seed.specName}` });
    await expect(opening).toBeVisible();
    const question = pane.getByTestId("pane-approval-question");
    await expect(question).toHaveText(seed.question);
    await expect(paneScroller(page).getByTestId("pane-approval-question")).toHaveCount(1);
    // Both at once: the question is the pane's own and waits on none of the document's reads.
    await expect(opening).toBeVisible();
    await expect(pane.getByRole("region", { name: /^Document version/u })).toHaveCount(0);

    // The held read is let through at the end of the row, so no request is left hanging, and the
    // document takes the skeleton's place under the question that stood all along.
    await (await read.held).continue();
    await expect(pane.getByRole("region", { name: "Document version 1" })).toBeVisible();
    await expect(question).toHaveText(seed.question);
  } finally {
    await alice.close();
  }
});

test("an approval whose document cannot be read falls back to the card, which draws the question once", async ({
  browser,
}) => {
  const seed = await seedApproval(FALLBACK_SUMMARY);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pane = inboxPane(page);

    // Every attempt is refused, not the first alone: the shared client retries a 5xx twice, so a
    // route that lets the later attempts through never reaches the fallback at all.
    let refused = 0;
    await page.route(`**/api/v1/artifacts/${seed.artifactId}`, async (route) => {
      refused += 1;
      await route.fulfill({
        body: JSON.stringify({
          code: "DOC_SERVICE_UNAVAILABLE",
          error: "document service is unavailable",
        }),
        contentType: "application/json",
        status: 500,
      });
    });
    await page.goto(inboxAt(seed.askId));
    await expect.poll(() => refused).toBeGreaterThanOrEqual(3);

    // The card takes the document's place, and the block above it is not drawn: the card asks
    // the whole question itself, so the question is on screen once.
    const card = pane.getByTestId(`ask-${seed.askId}`);
    await expect(card).toBeVisible();
    await expect(pane.getByTestId("pane-approval-question")).toHaveCount(0);
    await expect(card.getByText(FALLBACK_MARK)).toHaveCount(1);
    await expect(page.getByText(FALLBACK_MARK)).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("a request naming the spec and the Verification document names both above the marked document", async ({
  browser,
}) => {
  const seed = await seedTwoDocuments();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const pane = inboxPane(page);
    await page.goto(inboxAt(seed.askId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.askId);

    // One question naming both documents, each at the version this request names.
    const question = pane.getByTestId("pane-approval-question");
    await expect(question).toHaveText(seed.question);
    await expect(question).toContainText("verification.md (version 1)");

    // The question displaced nothing: the document below it is the version Approve approves, and
    // it carries the marks of every word changed since the approved version 1.
    await expect(pane.getByRole("region", { name: "Document version 2" })).toBeVisible();
    const changed = pane.locator(".dispatch-change-block");
    await expect(changed).toHaveCount(1);
    await expect(changed.locator(".dispatch-change-added")).toHaveText([ADDED_WORD]);
    await expect(changed.locator(".dispatch-change-removed-words")).toHaveText([REMOVED_WORD]);
  } finally {
    await alice.close();
  }
});
