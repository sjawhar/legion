// What a large document costs a reader on a phone, measured under the set-up
// dispatch://INFRA-341/spec names: the `iphone` project under 4x CPU throttling, the time from
// the start of navigation until the document's first block is in the viewport, and the longest
// main-thread task until the page has scrolled to the end. The two documents are
// `large-documents.ts`: one shaped like dispatch://AHXV6-1/artifact/intermediate-specs-md, and
// one five times as large.
//
// The two measuring rows fail when a target of "Targets for large documents" is missed: the
// first block within 3 s for the smaller document and 5 s for the larger, and no main-thread
// task longer than 500 ms for either. Every number is still printed, since a target met says
// nothing about how much room is left.
//
// The rest of acceptance A7 is here too, and each of those rows reads a build rather than a
// clock: a link to the decision block at the end of the larger document lands on that block, a
// reader answers that decision, and a comment body of twenty diagrams draws only the diagrams
// the reader has reached. They carry no throttling: what they prove is what the page does, not
// how long it takes.
import { expect, type Locator, test } from "@playwright/test";

import { createComment, createIssue, createIssueArtifact, createProject } from "./api";
import { documentEditor } from "./editor";
import {
  commentDiagrams,
  decisionBlockId,
  decisionBlocks,
  diagramComment,
  diagramsIn,
  documentDiagrams,
  documentNames,
  largeDocument,
  smallDocument,
} from "./large-documents";
import { measureDocument, printMeasurement } from "./large-documents-page";
import { openCommentThread, settledScrollTop } from "./mermaid-helpers";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const agent = {
  actor: { id: "e2e-large-documents-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The longest main-thread task either document may cost a reader ("Targets for large
 *  documents", dispatch://INFRA-341/spec): a task over half a second is a freeze a person sees. */
const longestTaskTargetMs = 500;

/**
 * Resolves once the server owes nothing this seed left it: no document settlement queued or
 * running, and no document owed a block index. A document of this size defers its block index
 * (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`), and that work costs the same machine
 * the browser runs on, so a measurement taken while it runs measures the reconciliation too.
 * The same wait as `heavy-document-writes.e2e.ts`'s, against the one database this run holds.
 */
async function everythingOwedFinished(what: string): Promise<void> {
  await expect
    .poll(
      () =>
        sql(
          "select (select count(*) from doc_settlements_pending) || ' settlements, ' || " +
            "(select count(*) from document_block_versions where version is null) || ' block indexes'"
        ),
      {
        intervals: [500],
        message: `${what}: the write's own work never finished`,
        timeout: 180_000,
      }
    )
    .toBe("0 settlements, 0 block indexes");
}

/**
 * Writes `content` as an issue document and answers the path a reader opens it at.
 *
 * The issue's title and the document's file name are both short, and both rows use names of the
 * same length. At 390 px the document's first block begins 650 px down a 664 px viewport, and
 * every line either of them wraps to raises the header by about 30 px: a two-line title or file
 * name puts the first block under the fold, where it never comes into the viewport without a
 * scroll and there is no first number to read. Which document a row measures is in the row's
 * own title, not in what it seeds.
 */
async function seedDocument(name: string, content: string): Promise<string> {
  await resetDatabase();
  await createProject({ key: "BIG", name: "Big documents" });
  const issue = await createIssue({ project: "BIG", title: "A document" }, agent);
  const uploaded = await createIssueArtifact(issue.key, { content, name }, agent);
  await everythingOwedFinished(name);
  return `/issues/${issue.key}/artifacts/${uploaded.artifact.slug}`;
}

interface Row {
  /** The document an agent writes. */
  readonly content: string;
  /** How many diagrams it holds, which the row proves before it measures anything. */
  readonly diagrams: number;
  /** The target the first block has to be in the viewport within. */
  readonly firstBlockTargetMs: number;
  /** How long the row waits for the first block. A build that draws it at all is read against
   *  the target; this is the wait for one that never draws it, so a failure reads as the miss it
   *  is rather than as a hang. */
  readonly firstBlockTimeoutMs: number;
  /** The file it is written under. */
  readonly name: string;
  /** The row's own timeout, since the suite sets none and Playwright's 30 s default is under
   *  what the seed, the throttled draw and 120 scroll steps cost. */
  readonly timeoutMs: number;
  /** What the row calls it in the numbers it prints. */
  readonly what: string;
}

const rows: readonly [Row, Row] = [
  {
    content: smallDocument,
    diagrams: documentDiagrams.small,
    firstBlockTargetMs: 3_000,
    firstBlockTimeoutMs: 120_000,
    name: "specs-small.md",
    timeoutMs: 300_000,
    what: documentNames.small,
  },
  {
    content: largeDocument,
    diagrams: documentDiagrams.large,
    firstBlockTargetMs: 5_000,
    firstBlockTimeoutMs: 240_000,
    name: "specs-large.md",
    timeoutMs: 600_000,
    what: documentNames.large,
  },
];

for (const row of rows) {
  test(`measures ${row.what} on a phone under 4x CPU throttling @phone`, async ({
    browser,
  }, testInfo) => {
    test.skip(
      testInfo.project.name !== "iphone",
      "the measurement set-up is the iphone project's viewport, touch and Chromium"
    );
    test.setTimeout(row.timeoutMs);
    // The document the run measures is the one this module spells, so a change to the generator
    // that moved either size would be read here rather than in a number that quietly moved.
    expect(diagramsIn(row.content)).toBe(row.diagrams);
    const path = await seedDocument(row.name, row.content);

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const measured = await measureDocument(page, path, row.firstBlockTimeoutMs);
      printMeasurement(
        `${row.what}, ${row.content.length} characters, ${row.diagrams} diagrams`,
        measured
      );

      // What a measured number needs to mean anything: the document really drew, and the page
      // really scrolled, so the longest task covers a scroll through the whole document.
      await expect(documentEditor(page)).toBeVisible();
      expect(measured.firstBlockMs).toBeGreaterThan(0);
      expect(measured.scrollSteps).toBeGreaterThan(0);
      expect(measured.longestTaskMs).toBeGreaterThan(0);

      expect(measured.firstBlockMs).toBeLessThanOrEqual(row.firstBlockTargetMs);
      expect(measured.longestTaskMs).toBeLessThanOrEqual(longestTaskTargetMs);
    } finally {
      await alice.close();
    }
  });
}

test(`a link to the decision block at the end of ${documentNames.large} lands on that block @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const block = decisionBlockId(decisionBlocks);
  const path = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`${path}#b-${block}`);
    const linked = documentEditor(page).locator(`[data-block-id="${block}"]`);
    // The landing is in two parts: the focus the editor runs as it binds, and the one it runs
    // again once the diagrams around the target have drawn and moved it
    // (`features/doc/refocus-after-diagrams.ts`). A block behind `contain-intrinsic-size` has a
    // box before it has painted, so the first focus lands on an estimate; the block has to be in
    // the viewport once the page has stopped moving, not only before it.
    await expect(linked).toBeInViewport({ timeout: 240_000 });
    await settledScrollTop(page);
    await expect(linked).toBeInViewport();
  } finally {
    await alice.close();
  }
});

test(`a reader answers a decision block in ${documentNames.large} @phone`, async ({ browser }) => {
  test.setTimeout(600_000);
  const block = decisionBlockId(decisionBlocks);
  const path = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`${path}#b-${block}`);
    const decision = documentEditor(page).locator(`[data-dispatch-ask-block="${block}"]`);
    // The block hosts the Inbox's own ask card once the server has indexed it.
    const card = decision.locator("article[data-testid^=ask-]");
    await expect(card).toBeVisible({ timeout: 240_000 });

    await card.getByRole("radio", { name: "The next window" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(decision).toContainText("Answered by alice");
    await expect(
      decision.locator('ul[aria-label="Options"] li[data-selected="true"]')
    ).toContainText("The next window");
  } finally {
    await alice.close();
  }
});

/** Each mermaid block of `body`, in document order: whether it has drawn (a non-empty shadow
 *  root, `packages/proof-editor/src/mermaid-svg.ts`), whether the page asked for it at all (the
 *  figure a drawing or a refusal leaves in place of the `pre`), and whether it is within one
 *  viewport height of the viewport, which is the margin a diagram draws inside
 *  (`MERMAID_DRAW_MARGIN`). */
function diagramBlocks(
  body: Locator
): Promise<{ asked: boolean; drawn: boolean; near: boolean }[]> {
  return body.evaluate((element) => {
    const margin = window.innerHeight;
    // One entry per source block: a drawn block keeps its `pre` inside the figure's disclosure,
    // so the `pre`s are the blocks however many of them have drawn.
    return Array.from(
      element.querySelectorAll<HTMLPreElement>('pre[data-language="mermaid"]'),
      (pre) => {
        const figure = pre.closest("figure.dispatch-mermaid");
        const box = (figure ?? pre).getBoundingClientRect();
        const preview = figure?.querySelector(".dispatch-mermaid-preview");
        return {
          asked: figure !== null,
          drawn: (preview?.shadowRoot?.childElementCount ?? 0) > 0,
          near: box.top < window.innerHeight + margin && box.bottom > -margin,
        };
      }
    );
  });
}

test(`a comment body of ${commentDiagrams} diagrams draws only the diagrams near the viewport @phone`, async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  await resetDatabase();
  await createProject({ key: "BIG", name: "Big documents" });
  const issue = await createIssue({ project: "BIG", title: "A body" }, agent);
  const body = diagramComment(commentDiagrams);
  expect(diagramsIn(body)).toBe(commentDiagrams);
  const comment = await createComment(issue.key, { body }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await openCommentThread(page, testInfo, comment.id, "Section 1 is about");

    // A diagram that is never asked for can only be shown by waiting: the drawn count is read
    // until it has stopped moving, so every block the margin reached has had its answer.
    let drawnBefore = -1;
    await expect
      .poll(
        async () => {
          const drawnNow = (await diagramBlocks(card)).filter((block) => block.drawn).length;
          const settled = drawnNow > 0 && drawnNow === drawnBefore;
          drawnBefore = drawnNow;
          return settled;
        },
        { intervals: [1_000], timeout: 120_000 }
      )
      .toBe(true);

    const blocks = await diagramBlocks(card);
    expect(blocks).toHaveLength(commentDiagrams);
    // Every block the page asked for drew: a refusal would mean the renderer ran for it after
    // all, which is the cost this rule exists to save.
    expect(blocks.filter((block) => block.asked !== block.drawn)).toEqual([]);
    // The reader has reached some of the diagrams and not the rest.
    const drawn = blocks.filter((block) => block.drawn).length;
    expect(drawn).toBeGreaterThan(0);
    expect(drawn).toBeLessThan(commentDiagrams);
    // Which ones: the margin is one stretch of the body around the reader, so the diagrams that
    // drew are one run of neighbours - the top of the body on a phone, its end on a desktop,
    // which opens the Conversation at its newest turn - and never a scattering.
    const first = blocks.findIndex((block) => block.drawn);
    expect(blocks.map((block) => block.drawn)).toEqual(
      blocks.map((_, at) => at >= first && at < first + drawn)
    );
    // And nothing within one viewport height of the reader is left undrawn.
    expect(blocks.filter((block) => block.near && !block.drawn)).toEqual([]);
  } finally {
    await alice.close();
  }
});
