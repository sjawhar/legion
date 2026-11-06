// What a large document costs a reader on a phone, measured under the set-up
// dispatch://INFRA-341/spec names: the `iphone` project under 4x CPU throttling, the time from
// the start of navigation until the editor's first visible block is in the viewport, and the
// longest main-thread task until the page has scrolled to the end. The two documents are
// `large-documents.ts`: one shaped like dispatch://AHXV6-1/artifact/intermediate-specs-md, and
// one five times as large.
//
// The two measuring rows fail when a target of "Targets for large documents" is missed: the
// editor's first visible block within 3 s for the smaller document and 5 s for the larger, and
// no main-thread task longer than 500 ms for either. That block is the one the reader can type
// in - the editor's own, while its root carries `data-write-path="live"` - and never the
// server's draw or the page's own in front of it, which each row reads off the timed block
// itself and holds it to (`large-documents-page.ts`). Those two draws are printed as numbers of
// their own, `first painted` and the page's own draw, against no target. Every number is
// printed whether or not its target was met, since a target met says nothing about how much
// room is left.
//
// The rest of acceptance A7 is here too, and each of those rows reads a build rather than a
// clock: a link to the decision block at the end of the larger document lands on that block, a
// reader answers that decision, and a comment body of twenty diagrams draws only the diagrams
// the reader has reached. Two rows of the draw that makes those targets reachable at all are
// here as well: the document on the screen before its editor exists, and the editor opening
// where its reader had got to. None of them carries throttling: what they prove is what the
// page does, not how long it takes.
import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifactBlocks,
  getArtifactText,
  listIssueAsks,
} from "./api";
import {
  connectedDot,
  documentEditor,
  documentTransport,
  editorBeingBuilt,
  placeCaret,
  selectTextIn,
} from "./editor";
import {
  commentDiagrams,
  decisionBlockId,
  decisionBlocks,
  diagramComment,
  diagramsIn,
  documentDiagrams,
  documentNames,
  documentTitle,
  largeDocument,
  openingDecisionBlockId,
  openingDecisionDocument,
  plainDocument,
  proseOpening,
  sectionHeading,
  smallDocument,
} from "./large-documents";
import {
  longTaskTargetMs,
  measureDocument,
  printMeasurement,
  throttleTheCpu,
} from "./large-documents-page";
import { openCommentThread, settledScrollTop } from "./mermaid-helpers";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** What one animation frame of the build held: how many of the document's top-level blocks
 *  stood on the screen across the three surfaces that draw them, how many of those the editor
 *  had built, and any block id two surfaces held at once. `framesOfTheBlocks` records one of
 *  these per frame and the count row reads them. */
interface BlockFrame {
  readonly blocks: number;
  readonly built: number;
  readonly twice: readonly string[];
}

/** What one task of the page drew: how many top-level blocks the editor gained in it, and how
 *  many decision cards were mounted into the shells of their blocks. A `MutationObserver`
 *  delivers every record of one task in one callback, at that task's own microtask checkpoint,
 *  so one of these is one task's work and nothing else's. `drawsOfEachTask` records them and
 *  the row below reads them. */
interface DrawnInATask {
  /** When the task's changes were read, counted from the start of navigation. */
  readonly at: number;
  readonly blocks: number;
  readonly cards: number;
}

declare global {
  interface Window {
    /** How many of the document's blocks the editor's root held at each animation frame, which
     *  `framesOfTheBuild` records and the two progressive-build rows read. */
    __blocksBuilt?: number[];
    /** What each animation frame of the build held, which `framesOfTheBlocks` records and the
     *  count row reads. */
    __blockFrames?: BlockFrame[];
    /** What each task of the build drew, which `drawsOfEachTask` records and the decision-card
     *  row reads. */
    __drawnInTasks?: DrawnInATask[];
    /** What the page made of the edits `readerEditsTheBuild` gave it, and the refusal where
     *  the editor would not take one. */
    __readerEdits?: EditMade[];
    __readerEditsRefused?: string;
  }
}

const agent = {
  actor: { id: "e2e-large-documents-agent", kind: "session" as const },
  as: "agent" as const,
};

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
 * Writes `content` as an issue document and answers the issue it belongs to and the path a
 * reader opens it at.
 *
 * The issue's title and the document's file name are both short, and both rows use names of the
 * same length. At 390 px the document's first block begins 650 px down a 664 px viewport, and
 * every line either of them wraps to raises the header by about 30 px: a two-line title or file
 * name puts the first block under the fold, where it never comes into the viewport without a
 * scroll and there is no first number to read. Which document a row measures is in the row's
 * own title, not in what it seeds.
 */
async function seedDocument(
  name: string,
  content: string
): Promise<{ artifactId: string; issueKey: string; path: string }> {
  await resetDatabase();
  await createProject({ key: "BIG", name: "Big documents" });
  const issue = await createIssue({ project: "BIG", title: "A document" }, agent);
  const uploaded = await createIssueArtifact(issue.key, { content, name }, agent);
  await everythingOwedFinished(name);
  return {
    artifactId: uploaded.artifact.id,
    issueKey: issue.key,
    path: `/issues/${issue.key}/artifacts/${uploaded.artifact.slug}`,
  };
}

interface Row {
  /** How long the row waits for the editor to open, which is the end of its build and the end
   *  of the window the longest task is read over. */
  readonly buildTimeoutMs: number;
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
   *  what the seed, the throttled draw, the build and 120 scroll steps cost. */
  readonly timeoutMs: number;
  /** What the row calls it in the numbers it prints. */
  readonly what: string;
}

const rows: readonly [Row, Row] = [
  {
    buildTimeoutMs: 240_000,
    content: smallDocument,
    diagrams: documentDiagrams.small,
    firstBlockTargetMs: 3_000,
    firstBlockTimeoutMs: 120_000,
    name: "specs-small.md",
    timeoutMs: 300_000,
    what: documentNames.small,
  },
  {
    buildTimeoutMs: 480_000,
    content: largeDocument,
    diagrams: documentDiagrams.large,
    firstBlockTargetMs: 5_000,
    firstBlockTimeoutMs: 240_000,
    name: "specs-large.md",
    timeoutMs: 900_000,
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
    const { path } = await seedDocument(row.name, row.content);

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const measured = await measureDocument(page, path, {
        buildTimeoutMs: row.buildTimeoutMs,
        firstBlockTimeoutMs: row.firstBlockTimeoutMs,
      });
      printMeasurement(
        `${row.what}, ${row.content.length} characters, ${row.diagrams} diagrams`,
        measured,
        row.firstBlockTargetMs
      );

      // The number the targets are read against is the editor's block, not the draw in front of
      // it: the block that was timed stood in an editor root carrying `data-write-path="live"`,
      // it was inside an editable host, and it was in neither the blocks the server drew nor
      // the page's own draw of the document's text. A rig reading the preview's block fails
      // here rather than printing that draw as the editor's.
      expect(measured.firstBlock).toEqual({
        editable: true,
        inLiveEditor: true,
        inPreview: false,
      });

      // What a measured number needs to mean anything: the document really drew, the editor
      // really opened, and the page really scrolled afterwards, so the longest task covers the
      // whole build and then a scroll through the whole document.
      await expect(documentEditor(page)).toBeVisible();
      expect(measured.firstBlockMs).toBeGreaterThan(0);
      expect(measured.editorOpenedMs ?? 0).toBeGreaterThan(0);
      expect(measured.windowClosedMs).toBeGreaterThan(measured.editorOpenedMs ?? 0);
      expect(measured.scrollSteps).toBeGreaterThan(0);
      expect(measured.longestTaskMs).toBeGreaterThan(0);
      // And the editor's block came no earlier than either draw in front of it: the first block
      // on the screen, which is the server's own where it drew one, and the first block the
      // page's own code drew, which is the preview's and which this rig read as the editor's
      // until INFRA-341. A number at or under that one is the preview's block timed again.
      expect(measured.firstBlockMs).toBeGreaterThanOrEqual(measured.firstPaintedMs ?? 0);
      expect(measured.firstBlockMs).toBeGreaterThan(measured.pageDrawnMs ?? 0);

      expect(measured.firstBlockMs).toBeLessThanOrEqual(row.firstBlockTargetMs);
      expect(measured.longestTaskMs).toBeLessThanOrEqual(longTaskTargetMs);
    } finally {
      await alice.close();
    }
  });
}

/** How long the row below holds the socket after the document's own draw is on the screen.
 *  Well past the moment the page's own end is reached - the draw is a screenful and a few
 *  steps of 100 ms exhaust it - so a window that closed there closed inside this wait. */
const heldAfterTheDrawMs = 20_000;

// What the two rows above read their longest task over has to cover the build, and the page is
// not the document until the editor is in it: the draw in front of the reader is the blocks the
// document opens with (`large-documents-page.ts`'s module comment), so a window that ended
// where that page ends ended four scroll steps in, with most of the build still to come. This
// row holds the document's socket, so the editor is built only when it says so, and reads
// whether the measurement closed its window meanwhile.
test(`the measurement's window stays open until ${documentNames.large}'s editor opens @phone`, async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the measurement set-up is the iphone project's viewport, touch and Chromium"
  );
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page, { holding: true });
    const measuring = measureDocument(page, path, {
      buildTimeoutMs: 480_000,
      firstBlockTimeoutMs: 240_000,
    });
    let closed = false;
    const noteClosed = () => {
      closed = true;
    };
    void measuring.then(noteClosed, noteClosed);

    // The reader has the document's own draw, and no editor is being built behind it.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );
    await expect(documentEditor(page)).toHaveCount(0);
    await page.waitForTimeout(heldAfterTheDrawMs);
    expect(
      closed,
      "the measurement closed its window with no editor on the page: it read the longest task " +
        "of the draw's own screenful and none of the build"
    ).toBe(false);

    // Released, the build runs, the editor opens, and only then does the window close.
    await transport.release();
    const measured = await measuring;
    expect(measured.editorOpenedMs ?? 0).toBeGreaterThan(heldAfterTheDrawMs);
    expect(measured.windowClosedMs).toBeGreaterThan(measured.editorOpenedMs ?? 0);
    // And what it scrolled was the editor's own page, not the draw's screenful.
    expect(measured.scrollSteps).toBeGreaterThan(10);
  } finally {
    await alice.close();
  }
});

test(`a link to the decision block at the end of ${documentNames.large} lands on that block @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const block = decisionBlockId(decisionBlocks);
  const { path } = await seedDocument("specs-large.md", largeDocument);

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

// The two rows below are about the draw that makes the numbers above reachable at all: the
// document is drawn from its own markdown, through the read-only headless engine, before the
// editor exists (`web/src/features/doc/DocumentPreview.tsx`). Each holds the document's socket
// open-ended (`documentTransport`), so no sync ever reaches the page and no editor is ever
// built: what is left on the screen is what a reader has while one is being built for them.

test(`${documentNames.small} is on the screen before its editor is @phone`, async ({ browser }) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-small.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await documentTransport(page, { holding: true });
    await page.goto(path);

    // The first block the measurement records as `first painted`, drawn with no socket, no sync
    // and no editor (`large-documents-page.ts`): it carries `data-block-id` as the editor's
    // blocks do, and it is not the block the measurement's targets are read against.
    const firstBlock = page.locator('[aria-label="Document text"] [data-block-id]').first();
    await expect(firstBlock).toBeInViewport({ timeout: 120_000 });
    await expect(firstBlock).toContainText(documentTitle(documentDiagrams.small));
    // And the sections below it are drawn too, not only the block the measurement reads.
    await expect(page.getByRole("heading", { name: sectionHeading(0) })).toBeVisible();
    // And the editor really is not there: the page holds no document textbox at all.
    await expect(documentEditor(page)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test(`the editor opens ${documentNames.large} where its reader had got to @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);
  // A section half-way down the document: far enough that a swap which kept the page's scroll
  // offset rather than the reader's place would leave them sections away from these words.
  const landmarkText = sectionHeading(Math.floor(documentDiagrams.large / 2));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    const landmark = page.getByRole("heading", { name: landmarkText });
    await expect(landmark).toBeAttached({ timeout: 240_000 });
    // The reader stops with this section's heading at the top of their screen - the block the
    // swap reads their place from is the one the screen starts with, so this is the block the
    // row is about. The blocks the scroll passed paint as it goes and the page settles under
    // them, which moves the heading, so it is put back at the top once that is over. A page
    // scrolled this far is 120,000 px from the top, so a swap that kept the page's offset
    // rather than the reader's place would land sections away.
    await landmark.scrollIntoViewIfNeeded();
    await settledScrollTop(page);
    await landmark.evaluate((heading) => heading.scrollIntoView({ block: "start" }));
    const scrolled = await settledScrollTop(page);
    expect(scrolled).toBeGreaterThan(10_000);
    const where = await landmark.boundingBox();
    expect(where?.y ?? Number.NaN).toBeLessThan(200);

    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 240_000 });
    // The editor has replaced the draw the reader was reading, and left their own words on the
    // same line of the screen. Read once the page has stopped moving: the editor's diagrams
    // draw for seconds after it opens and its skipped blocks paint at their own heights, and
    // the page is put back under the reader after each of those (`refocusAfterDiagrams`). Not
    // to the pixel: the two draws wear different stylesheets, so the block they were reading is
    // a few pixels taller or shorter in one than in the other.
    await settledScrollTop(page);
    const landed = await documentEditor(page)
      .getByRole("heading", { name: landmarkText })
      .boundingBox();
    expect(Math.abs((landed?.y ?? Number.NaN) - (where?.y ?? Number.NaN))).toBeLessThan(200);
  } finally {
    await alice.close();
  }
});

// The three rows below are about the build behind that swap. The editor is not drawn in one task
// any more: it is built a batch of blocks at a time, each batch its own task, the collab binding
// adopts the document and the mapping those batches made rather than drawing the whole document
// itself, and the last batch, that adoption and the swap are three tasks
// (`packages/proof-editor/src/progressive-build.ts`). Each holds the document's
// socket (`documentTransport`), so the build begins when the row releases it, and each throttles
// the renderer as the measurement does - not to time anything, but so the build lasts long
// enough for the page to draw frames while it runs, which is the only state these rows are about.

/** How many of the document's blocks the editor's root holds, at whatever depth. The view wears
 *  its `Document editor` name only once the build is over, and its root is hidden until then,
 *  so neither `documentEditor` nor a visibility check reads a build in progress. */
function blocksBuilt(page: Page): Promise<number> {
  return page.locator(".proof-editor [data-block-id]").count();
}

/**
 * Records, every animation frame until the editor is named, how many of the document's blocks
 * the editor's root holds, and answers what was recorded.
 *
 * The reading is the page's own rather than a poll from here, because a poll cannot see a build
 * in progress: the batches resume through `scheduler.yield()`, whose continuations outrank the
 * task a `page.evaluate` needs, so the first count a poll is answered with is the whole
 * document. The browser's own rendering runs between those batches, so a frame can read what a
 * poll cannot - and a frame that reads part of a document is also the proof a reader wants,
 * since a build that froze the page would have drawn no frame at all. Install before the
 * navigation; read once the editor is open.
 */
async function framesOfTheBuild(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const built: number[] = [];
    window.__blocksBuilt = built;
    const look = () => {
      const root = document.querySelector(".proof-editor");
      if (root !== null) {
        built.push(root.querySelectorAll("[data-block-id]").length);
        // The last frame worth reading is the first one the finished editor is named in: after
        // it every frame would pay for a scan of the whole document and read the same number.
        if (root.querySelector('[aria-label="Document editor"]') !== null) return;
      }
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  });
}

test(`the editor opens ${documentNames.large} with every block of it in the DOM @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const blocks = await getArtifactBlocks(artifactId);
  expect(blocks.length).toBeGreaterThan(100);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    await framesOfTheBuild(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    // The document's own draw is what the reader has while no socket has synced, and no editor
    // has been built from it yet.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );
    expect(await blocksBuilt(page)).toBe(0);

    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 240_000 });

    // Every block the server indexed the document under is in the editor's DOM, under the id a
    // `#b-<id>` link lands on, rather than only counted: two different blocks satisfy a count.
    // A code block is the one exception, and not a missing block: its node view builds its own
    // element and stamps no id on it
    // (`packages/proof-editor/src/mermaid-code-block-view.ts`), and this document's code blocks
    // are the mermaid source of each of its diagrams and a TypeScript listing per section.
    const identified = blocks
      .filter((block) => block.type !== "code_block")
      .map((block) => block.id);
    expect(identified.length).toBeGreaterThan(blocks.length / 2);
    const missing = await documentEditor(page).evaluate(
      (editor, ids) =>
        ids.filter((id) => editor.querySelector(`[data-block-id="${CSS.escape(id)}"]`) === null),
      identified
    );
    expect(missing).toEqual([]);

    // And the build really was in batches: the page drew frames holding part of the document
    // and not all of it, which a build done in one task never shows.
    const whole = await blocksBuilt(page);
    const frames = await page.evaluate(() => window.__blocksBuilt ?? []);
    expect(
      frames.filter((held) => held > 0 && held < whole),
      "no frame was drawn holding part of the document: it was built in one task, which is " +
        "the freeze this build exists to end"
    ).not.toEqual([]);
  } finally {
    await alice.close();
  }
});

test(`an edit made while ${documentNames.large}'s editor is being built is in it when the build ends @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);
  const title = documentTitle(documentDiagrams.large);

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Bob has the document open and his caret in its first block before Alice's socket is let
    // through, so the only thing left for him to do afterwards is press the keys.
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    await framesOfTheBuild(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

    // Bob writes a word at a time, from the moment Alice's socket is let through until her
    // editor is open, so his writing covers the whole of her build rather than a moment the row
    // has to guess at. He writes at a person's pace (`delay`) rather than as fast as the
    // protocol allows, for the same reason: a hundred words typed in three seconds would all be
    // written before her socket had even finished syncing. Which of his words crossed the build
    // is not something either page can say; that his writing covered one is what the frames
    // below prove.
    await transport.release();
    const opened = documentEditor(page)
      .waitFor({ state: "visible", timeout: 240_000 })
      .then(() => true);
    const written: string[] = [];
    let open = false;
    void opened.then((it) => {
      open = it;
    });
    while (!open && written.length < 120) {
      const word = `Pangolin${written.length}`;
      await bobPage.keyboard.type(` ${word}`, { delay: 25 });
      written.push(word);
    }
    await opened;
    expect(written.length).toBeGreaterThan(3);

    // Alice's editor holds every word Bob wrote while it was being built, in the block he wrote
    // them in.
    await expect(documentEditor(page)).toContainText(written.join(" "), { timeout: 240_000 });
    const whole = await blocksBuilt(page);
    const frames = await page.evaluate(() => window.__blocksBuilt ?? []);
    expect(
      frames.filter((held) => held > 0 && held < whole),
      "no frame was drawn holding part of the document, so Bob's words crossed no build and " +
        "this run says nothing about an edit that does"
    ).not.toEqual([]);
  } finally {
    await bob.close();
    await alice.close();
  }
});

/** Each surface a top-level block of the document can stand on the screen in, in the order the
 *  page holds them: the blocks the server drew into the page it sent
 *  (`web/src/features/doc/first-screen.ts`), the page's own draw of the document's text, and
 *  the editor, whose root stands before that draw while the build fills it from the top. */
const serverDrawnBlocks = "#first-screen > * > [data-block-id]";
const drawnBlocks = '[aria-label="Document text"] > [data-block-id]';
const builtBlocks = '.proof-editor [contenteditable="true"] > [data-block-id]';

/**
 * Records, every animation frame until the editor is open, every top-level block of the
 * document on the screen: how many there are across the three surfaces, how many of them the
 * editor has built, and any `data-block-id` held twice. Install before the navigation.
 *
 * The reading is the page's own rather than a poll from here, for the reason `framesOfTheBuild`
 * gives: a poll cannot land between the tasks of the build, and a frame is what the reader is
 * shown anyway. The editor is named only once its last batch is in and its binding is live
 * (`web/src/features/doc/editor.ts`), which is the last frame worth reading.
 */
async function framesOfTheBlocks(page: Page): Promise<void> {
  await page.addInitScript(
    ({ built, editor, surfaces }: { built: string; editor: string; surfaces: string[] }) => {
      const frames: BlockFrame[] = [];
      window.__blockFrames = frames;
      const look = () => {
        const held = new Map<string, number>();
        let blocks = 0;
        for (const surface of surfaces) {
          for (const block of document.querySelectorAll(surface)) {
            const id = block.getAttribute("data-block-id") ?? "";
            held.set(id, (held.get(id) ?? 0) + 1);
            blocks += 1;
          }
        }
        frames.push({
          blocks,
          built: document.querySelectorAll(built).length,
          twice: [...held]
            .filter(([, count]) => count > 1)
            .map(([id]) => id)
            .slice(0, 5),
        });
        if (document.querySelector(editor) !== null) return;
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    {
      built: builtBlocks,
      editor: '[role="textbox"][aria-label="Document editor"]',
      surfaces: [serverDrawnBlocks, drawnBlocks, builtBlocks],
    }
  );
}

/**
 * How many top-level blocks the server's index of `artifactId` holds: every block whose range
 * no other block's range contains. That index is the server's own parse of the document, so the
 * count is the document's, not this module's reading of the generator that spelled it, and it is
 * the number of blocks each draw of the document draws.
 */
async function topLevelBlocks(artifactId: string): Promise<number> {
  const blocks = await getArtifactBlocks(artifactId);
  return blocks.filter(
    (block) =>
      !blocks.some(
        (other) =>
          other.id !== block.id &&
          other.from <= block.from &&
          other.to >= block.to &&
          (other.from < block.from || other.to > block.to)
      )
  ).length;
}

// Acceptance A7's own count: each block id across the server's draw, the page's own draw and
// the editor, on every animation frame, with every block on the screen exactly once. The
// document is the one with neither a code block nor a diagram in it, because a code block's
// node view builds its own element and stamps no id on it
// (`packages/proof-editor/src/mermaid-code-block-view.ts`), so in that one document every
// top-level block carries an id in every draw of it and the three surfaces are comparable
// block for block.
test("every block of a document is on the screen exactly once as its editor is built @phone", async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-plain.md", plainDocument);
  const whole = await topLevelBlocks(artifactId);
  expect(whole).toBeGreaterThan(100);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    await framesOfTheBlocks(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The whole document, drawn by the page itself with no editor behind it: the count every
    // frame from here on has to hold, whichever surface is holding the blocks.
    await expect
      .poll(() => page.locator(drawnBlocks).count(), {
        intervals: [500],
        message: "the page never drew the whole document, so no frame holds every block of it",
        timeout: 240_000,
      })
      .toBe(whole);
    const drewAt = await page.evaluate(() => window.__blockFrames?.length ?? 0);

    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 480_000 });
    const frames = (await page.evaluate(() => window.__blockFrames ?? [])).slice(drewAt);
    expect(frames.length).toBeGreaterThan(1);

    // Never twice: no frame held one block id on two surfaces at once, which is what a build
    // that revealed its blocks without taking the draw's copies away would show.
    expect(
      frames.filter((frame) => frame.twice.length > 0).map((frame) => frame.twice),
      "a frame held a block of the document twice: the editor built it and the page's own draw " +
        "still held it"
    ).toEqual([]);
    // And never missing: every frame held the whole document, whichever surface each block was
    // on, so no block of it was off the screen while the editor was being built.
    expect(
      frames.filter((frame) => frame.blocks !== whole).map((frame) => frame.blocks),
      `a frame held something other than the document's ${whole} blocks`
    ).toEqual([]);
    // And the build really was in batches behind that count: frames were drawn with part of the
    // document in the editor and the rest still in the page's own draw.
    expect(
      frames.filter((frame) => frame.built > 0 && frame.built < whole).length,
      "no frame was drawn with part of the document in the editor: it was built in one task, " +
        "which is the freeze this build exists to end"
    ).toBeGreaterThan(0);
    expect(frames.at(-1)?.built).toBe(whole);
  } finally {
    await alice.close();
  }
});

// The three rows below are about the editor a reader has before its last batch: its blocks are
// on the screen and they take edits, which reach the Yjs document through a window onto the
// blocks already built (`packages/proof-editor/src/progressive-build.ts`, acceptance A7). The
// first of them needs no editor at all - a decision the server drew is answered through the
// shared ask card in the page's own draw - and the two after it throttle the renderer as the
// measurement does, not to time anything but so the build lasts long enough for the page to
// draw frames while it runs, which is where a reader's edits land.

test("a reader answers a decision block the server drew, with the editor's socket held @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("opening-decision.md", openingDecisionDocument);
  // The ask the server indexed the block into, which the card in the draw is named for: the
  // text engine's own decision shell carries no `data-dispatch-ask-block` - that attribute is
  // the editor's node view's alone (`typed-block-drawing.ts`) - so the card is found by the
  // ask's id, which is what every surface names it by.
  const indexed = (await listIssueAsks(issueKey)).find(
    (ask) => ask.block_id === openingDecisionBlockId
  );
  expect(indexed, "the server indexed no ask for the document's decision block").toBeDefined();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Held open-ended, so no sync ever reaches the page and no editor is ever built: what the
    // reader answers is the page's own draw of the document's text.
    await documentTransport(page, { holding: true });
    await page.goto(path);

    const drawn = page.locator('[aria-label="Document text"]');
    const card = drawn.getByTestId(`ask-${indexed?.id}`);
    await expect(card).toBeVisible({ timeout: 120_000 });
    await expect(documentEditor(page)).toHaveCount(0);
    await expect(editorBeingBuilt(page)).toHaveCount(0);

    await card.getByRole("radio", { name: "The next window" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(drawn).toContainText("Answered by alice");
    // Answered with no editor on the page at all, which is the whole of this row.
    await expect(documentEditor(page)).toHaveCount(0);

    // And the server holds it: the issue's ask for that block reads answered.
    await expect
      .poll(
        async () =>
          (await listIssueAsks(issueKey)).find((ask) => ask.block_id === openingDecisionBlockId)
            ?.state,
        {
          intervals: [500],
          message: "the answer never reached the server",
          timeout: 60_000,
        }
      )
      .toBe("answered");
  } finally {
    await alice.close();
  }
});

/** One edit the reader makes in an editor whose build is still running: where to put the
 *  selection, and what to do there. */
interface ReaderEdit {
  /** Where against `quote` the selection goes: over it, or collapsed at either end. */
  readonly edge: "after" | "before" | "whole";
  readonly kind: "backspace" | "enter" | "paste" | "type";
  /** The text the selection is found by, among the text a caret can enter. */
  readonly quote: string;
  /** What a type inserts, or what a paste carries. */
  readonly text?: string;
}

/** What the page recorded for one of those edits: what it did, how many blocks the editor held
 *  when it did it - which is what says the build was still running - and how many it held
 *  afterwards, which says what the edit did to the document's shape. */
interface EditMade {
  readonly after: number;
  readonly built: number;
  readonly what: string;
}

/**
 * Has the page itself make `edits` in the editor, in one frame, as soon as the editor is live
 * and holds every block they name. Install before the navigation; read what it made with
 * `editsMade` once the build is over.
 *
 * A reader's own edits cannot be driven from here while their editor is being built. The
 * batches resume through `scheduler.yield()`, whose continuations outrank the task a
 * `page.evaluate` or a locator's poll needs, so nothing this side asks is answered until the
 * last batch has landed - which `framesOfTheBuild` says of counting blocks and this row found
 * of typing. The browser's own rendering does run between those batches, so a frame is where a
 * reader's keystroke lands, and that is where these go.
 *
 * Each edit takes the editor's own path, never a shortcut into its state: the selection is a
 * DOM Range plus the `selectionchange` ProseMirror's DOMObserver reads, a type is
 * `execCommand("insertText")`, whose DOM mutation that observer picks up, Enter and Backspace
 * are `keydown` events its keymaps handle, and a paste is the `ClipboardEvent` its clipboard
 * plugin handles, the same event `e2e/editor.ts`'s `paste` dispatches.
 */
async function readerEditsTheBuild(page: Page, edits: readonly ReaderEdit[]): Promise<void> {
  await page.addInitScript((planned: readonly ReaderEdit[]) => {
    const select = (editor: Element, edit: ReaderEdit): void => {
      const runs: { node: Node; start: number; end: number }[] = [];
      const walker = document.createTreeWalker(editor, NodeFilter.SHOW_TEXT);
      let text = "";
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        if (node.parentElement?.isContentEditable !== true) continue;
        const start = text.length;
        text += node.textContent ?? "";
        runs.push({ end: text.length, node, start });
      }
      const at = text.indexOf(edit.quote);
      const first = runs.find((run) => run.start <= at && at < run.end);
      const last = runs.find(
        (run) => run.start < at + edit.quote.length && at + edit.quote.length <= run.end
      );
      if (at < 0 || first === undefined || last === undefined) {
        throw new Error(`the editor holds no ${JSON.stringify(edit.quote)}`);
      }
      const startAt = { node: first.node, offset: at - first.start };
      const endAt = { node: last.node, offset: at + edit.quote.length - last.start };
      const from = edit.edge === "after" ? endAt : startAt;
      const to = edit.edge === "before" ? startAt : endAt;
      const range = document.createRange();
      range.setStart(from.node, from.offset);
      range.setEnd(to.node, to.offset);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
    };
    const made: EditMade[] = [];
    window.__readerEdits = made;
    const look = () => {
      // One edit a frame, never four in one: an edit of this kind lands in the DOM, and
      // ProseMirror reads it back off the DOM in its own time, so a second edit made before
      // that read is placed against a document the editor has not caught up with yet.
      const edit = planned[made.length];
      if (edit === undefined) return;
      const root = document.querySelector(".proof-editor[data-write-path='live']");
      const editor = root?.querySelector<HTMLElement>('[contenteditable="true"]') ?? null;
      if (document.querySelector('[role="textbox"][aria-label="Document editor"]') !== null) return;
      // Each edit waits for the block it names, which the batches reach in their own time; an
      // edit before it in the list may have changed what the blocks after it read.
      if (root === null || editor === null || !(editor.textContent ?? "").includes(edit.quote)) {
        requestAnimationFrame(look);
        return;
      }
      try {
        editor.focus();
        select(editor, edit);
        const built = root.querySelectorAll("[data-block-id]").length;
        if (edit.kind === "type") {
          document.execCommand("insertText", false, edit.text);
        } else if (edit.kind === "paste") {
          const data = new DataTransfer();
          data.setData("text/plain", edit.text ?? "");
          editor.dispatchEvent(
            new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: data })
          );
        } else {
          editor.dispatchEvent(
            new KeyboardEvent("keydown", {
              bubbles: true,
              cancelable: true,
              key: edit.kind === "enter" ? "Enter" : "Backspace",
            })
          );
        }
        made.push({
          after: root.querySelectorAll("[data-block-id]").length,
          built,
          what: edit.kind,
        });
      } catch (error) {
        window.__readerEditsRefused = String(error);
        return;
      }
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  }, edits);
}

/** What the page made of the edits it was given, once the build is over. */
async function editsMade(page: Page, planned: readonly ReaderEdit[]): Promise<EditMade[]> {
  const refused = await page.evaluate(() => window.__readerEditsRefused);
  expect(refused, "the reader's edit was refused by the editor it was made in").toBeUndefined();
  const made = (await page.evaluate(() => window.__readerEdits)) ?? [];
  expect(
    made.map((edit) => edit.what),
    "the reader's edits never ran: the editor was open before the page could make them"
  ).toEqual(planned.map((edit) => edit.kind));
  return made;
}

test(`typing in ${documentNames.large}'s first block while it is built reaches the document @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const before = await getArtifactText(artifactId);
  // What the editor ends up holding is read after the build, not from the server's index: the
  // page counts every block of the editor at whatever depth, which is the same number the
  // reader's edits were recorded against.
  const title = documentTitle(documentDiagrams.large);
  const typing: readonly ReaderEdit[] = [
    { edge: "after", kind: "type", quote: title, text: " Pangolin." },
  ];

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    await readerEditsTheBuild(page, typing);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 900_000 });
    const whole = await blocksBuilt(page);
    const made = await editsMade(page, typing);
    expect(
      made.map((edit) => edit.built < whole),
      "the build was over before the word was typed, so this run says nothing about an edit " +
        "that crosses one"
    ).toEqual([true]);

    // The server holds the word in the block it was typed in, with every other block of the
    // document exactly as it was.
    await everythingOwedFinished("the word typed into the build");
    const after = await getArtifactText(artifactId);
    expect(after.markdown).toBe(before.markdown.replace(`# ${title}`, `# ${title} Pangolin.`));
  } finally {
    await alice.close();
  }
});

test(`two people write in ${documentNames.large} while its editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  // The editor's own finished count is read after the build (the typing row says why).
  const title = documentTitle(documentDiagrams.large);
  // A word inside a block, Enter to split one, Backspace to join two, and a paste that runs
  // from one block into the next and lands three.
  const reading: readonly ReaderEdit[] = [
    { edge: "after", kind: "type", quote: proseOpening(4), text: " Beaver." },
    { edge: "after", kind: "enter", quote: proseOpening(2) },
    { edge: "before", kind: "backspace", quote: proseOpening(3) },
    {
      edge: "whole",
      kind: "paste",
      quote: `${sectionHeading(6)}${proseOpening(6)}`,
      text: "Otter one\n\nOtter two\n\nOtter three",
    },
  ];

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Bob has the document open and his caret in its first block before Alice's socket is let
    // through, so the only thing left for him to do afterwards is press the keys.
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    await framesOfTheBuild(page);
    await readerEditsTheBuild(page, reading);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

    // Bob writes from the moment Alice's socket is let through: a word in a block her batches
    // have built, a block inserted inside that built run, and that block taken out of it again
    // over the prose it was inserted above.
    await transport.release();
    await placeCaret(bobPage, "after", title);
    await bobPage.keyboard.type(" Pangolin.");
    await placeCaret(bobPage, "after", sectionHeading(0));
    await bobPage.keyboard.press("Enter");
    await bobPage.keyboard.type("Otter paragraph.");
    await selectTextIn(bobPage, documentEditor(bobPage), `Otter paragraph.${proseOpening(0)}`);
    await bobPage.keyboard.press("Backspace");

    await expect(documentEditor(page)).toBeVisible({ timeout: 900_000 });
    const whole = await blocksBuilt(page);
    // Alice's four edits, each made in a frame of her own build, with part of the document in
    // the editor and the rest still to come.
    const made = await editsMade(page, reading);
    expect(
      made.map((edit) => edit.built < whole),
      "the build was over before every edit was made, so this run says nothing about the edits " +
        "that cross one"
    ).toEqual([true, true, true, true]);
    // And each did what it is there to do, in the blocks the editor was holding at the time: a
    // word changes no count, Enter makes a block, Backspace unmakes one, and three blocks
    // pasted over a selection that runs from one into the next leave one more than there were.
    expect(made.map((edit) => edit.after - edit.built)).toEqual([0, 1, -1, 1]);
    // And Bob's writing crossed that build too: the page drew frames holding part of the
    // document while he was writing.
    const frames = await page.evaluate(() => window.__blocksBuilt ?? []);
    expect(
      frames.filter((held) => held > 0 && held < whole),
      "no frame was drawn holding part of the document, so Bob's writing crossed no build"
    ).not.toEqual([]);

    // The binding has taken over, and the document the server holds carries every edit of both
    // of them.
    await everythingOwedFinished("the edits made during the build");
    const { markdown } = await getArtifactText(artifactId);
    // What the markdown is read as: the document's own blocks, which is what `\n\n` separates
    // and what each of these edits made or unmade. A heading's own text is read inside it,
    // since remark escapes the stop after a section's number (`## 4\. The block index`).
    const blocks = markdown.split("\n\n");
    // Bob's: the word in the title, and the block he added taken out again over the prose it
    // stood above, which the one block left of the two no longer opens with.
    expect(markdown).toContain(`# ${title} Pangolin.`);
    expect(markdown).not.toContain("Otter paragraph.");
    expect(markdown).not.toContain(proseOpening(0));
    // Alice's: the word inside a block, Enter splitting one into two, Backspace joining a
    // section's prose onto its heading, and a paste that ran from one block into the next and
    // landed three.
    expect(markdown).toContain(`${proseOpening(4)} Beaver.`);
    expect(blocks.filter((block) => block.startsWith(proseOpening(2)))).toEqual([proseOpening(2)]);
    expect(markdown).toContain(`The block index${proseOpening(3)}`);
    expect(blocks).toContain("## Otter one");
    expect(markdown).toContain("Otter two");
    expect(markdown).toContain("Otter three");
  } finally {
    await bob.close();
    await alice.close();
  }
});

// The three rows below are about what the page does in front of that draw, which is what the
// numbers above are mostly made of. The document's own reads are started from the address, the
// first of them before any of the page's own code has evaluated, so they wait for nothing the
// entry chunk or the issue page does; and the margin beside the document draws no card a reader
// cannot see. None of the three is timed: each reads what the page asked for and what it drew,
// never how long either took.

/** A document of one short section, so these rows seed in a moment: what they read is the
 *  order the page does things in, which no length of document changes. */
const shortDocument = "# The lease ledger\n\nThe paragraph a reader meets first.\n";

/** The pathname of each request the page made, newest last, for a row that reads what the page
 *  asked for. */
function requestedPaths(page: Page): string[] {
  const paths: string[] = [];
  page.on("request", (request) => paths.push(new URL(request.url()).pathname));
  return paths;
}

/** Either route a document's markdown is read by: the one the address names it at, and the one
 *  the issue's own read names it at (`web/src/api/client.ts`). */
const documentTextRoute = /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/;

test("a document's own text is read before the sign-in check has answered @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const asked = requestedPaths(page);
    // The sign-in check, held: every page waits on it (`app.tsx`'s `AuthGate` shows its
    // skeleton until it answers), so while it is out nothing of the shell, the issue page or
    // the margin has rendered at all.
    const signInHeld = Promise.withResolvers<void>();
    await page.route("**/auth/whoami", async (route) => {
      await signInHeld.promise;
      await route.continue();
    });
    await page.goto(path, { waitUntil: "commit" });

    // The document's own text, asked for from the address alone
    // (`web/src/features/doc/open-document-early.ts`), while the check is still out. The
    // address names the document, so the read is the slug route's; a Spec tab's address names
    // no slug and its text is read by the id the issue answers with.
    await expect
      .poll(() => asked.filter((what) => documentTextRoute.test(what)), {
        intervals: [100],
        message:
          "the document's text was never asked for while the sign-in check was out: the read " +
          "is waiting for the shell again",
        timeout: 60_000,
      })
      .toEqual([`/api/v1/issues/${issueKey}/artifacts/notes-md/text`]);
    // And the shell really has not rendered: the Inbox read it makes is still unmade.
    expect(asked).not.toContain("/api/v1/inbox");

    // Released, the page shows the document it already holds the text of.
    signInHeld.resolve();
    const firstBlock = page
      .locator(
        '[aria-label="Document text"] [data-block-id], [aria-label="Document editor"] [data-block-id]'
      )
      .first();
    await expect(firstBlock).toBeVisible({ timeout: 60_000 });
    await expect(firstBlock).toContainText("The lease ledger");
  } finally {
    await alice.close();
  }
});

test("a document's text is asked for before the entry chunk has arrived @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(path, { waitUntil: "commit" });
    const firstBlock = page
      .locator(
        '[aria-label="Document text"] [data-block-id], [aria-label="Document editor"] [data-block-id]'
      )
      .first();
    await expect(firstBlock).toBeVisible({ timeout: 120_000 });

    // What the browser itself timed: when the page asked for the document's text, and when the
    // last byte of the entry chunk arrived. The entry is the page's one module script, and no
    // module of it can evaluate before all of its bytes are here, so a read that started before
    // that moment started before every static import of the entry evaluated - which is where
    // the page used to ask for it (`web/src/main.tsx`), about 1.4 s into the load on a throttled
    // phone. Asking for it from `web/index.html` is what moves it in front of the chunk. The two
    // times are the browser's own resource timing, so nothing this row does sits between them.
    const timed = await page.evaluate((textPath: string) => {
      const entry = document.querySelector<HTMLScriptElement>('script[type="module"][src]');
      const resources = performance.getEntriesByType("resource") as PerformanceResourceTiming[];
      return {
        chunk: entry?.src,
        chunkArrivedMs: resources.find((resource) => resource.name === entry?.src)?.responseEnd,
        textAskedMs: resources.find((resource) => new URL(resource.name).pathname === textPath)
          ?.startTime,
      };
    }, `/api/v1/issues/${issueKey}/artifacts/notes-md/text`);

    expect(timed.chunk, "the page has no module script to time the read against").toBeDefined();
    expect(timed.chunkArrivedMs, `the entry chunk ${timed.chunk} was never timed`).toBeDefined();
    expect(timed.textAskedMs, "the document's text was never asked for at all").toBeDefined();
    expect(
      timed.textAskedMs ?? Number.POSITIVE_INFINITY,
      "the document's text was asked for only after the entry chunk had arrived, so it waited " +
        "for the entry's own static imports to evaluate before it went out (`web/index.html`)"
    ).toBeLessThan(timed.chunkArrivedMs ?? 0);
  } finally {
    await alice.close();
  }
});

// The Markdown engine the page draws a document with - the headless Proof editor's chunk, 461 KB
// to download and compile, and the engine built over it - used to be loaded before the page had
// rendered anything: `openDocumentEarly` warmed it, and the shell warmed it again from its first
// commit. That buys a reader their first block where the page has to draw it. It buys nothing at
// all where the server drew the document's opening blocks into the page it sent
// (`web/src/features/doc/first-screen.ts`), because `DocumentPreview` adopts those blocks instead
// of parsing them - and on a phone at 4x CPU throttling that load is about a second and a half of
// the page's own thread, taken in front of the issue page's first render, which is the render
// that carries the document's surface, its socket and its editor (dispatch://INFRA-341, R11).
//
// So on such a page the engine waits for the document's own text, which is the first thing that
// needs it. This row holds that text back and reads the browser's own resource timing: a page
// that warmed the engine asked for its chunk seconds before the text answered.
test("a page the server drew a document into loads no Markdown engine before the document's text @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the engine's one reader here is the document's own preview, which is what a phone draws: " +
      "at a width that opens the margin, its cards draw Markdown of their own"
  );
  test.setTimeout(180_000);
  const { path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The document's text, held long enough that no order between it and the engine's chunk can
    // be read as noise. Both routes, since the page reads the text by whichever its address
    // named.
    const textHeld = Promise.withResolvers<void>();
    await page.route(
      (url) => documentTextRoute.test(url.pathname),
      async (route) => {
        await textHeld.promise;
        await route.continue();
      }
    );
    await page.goto(path, { waitUntil: "commit" });

    // The page has drawn everything it can without the text: the issue's own header, which waits
    // on the issue read alone. A page that warms the engine has asked for its chunk by now.
    await expect(page.locator("[data-issue-header]")).toBeVisible({ timeout: 60_000 });
    textHeld.resolve();

    const firstBlock = page
      .locator(
        '[aria-label="Document text"] [data-block-id], [aria-label="Document editor"] [data-block-id]'
      )
      .first();
    await expect(firstBlock).toBeVisible({ timeout: 60_000 });
    await expect(firstBlock).toContainText("The lease ledger");

    // What the browser itself timed. The engine's chunk is the headless editor's entry
    // (`@legion/proof-editor/headless`, `packages/proof-editor/src/lib-headless.ts`), which the
    // build names after that module; nothing else on this page imports it.
    const timed = await page.evaluate(() => {
      const resources = performance.getEntriesByType("resource") as PerformanceResourceTiming[];
      const pathOf = (resource: PerformanceResourceTiming) => new URL(resource.name).pathname;
      const text = /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/;
      return {
        engineAskedMs: resources.find((resource) =>
          /^\/assets\/lib-headless-[^/]*\.js$/.test(pathOf(resource))
        )?.startTime,
        textAnsweredMs: resources.find((resource) => text.test(pathOf(resource)))?.responseEnd,
      };
    });

    expect(
      timed.textAnsweredMs,
      "the document's text was never read, so there is no moment to read the engine against"
    ).toBeDefined();
    expect(
      timed.engineAskedMs,
      "the page drew this document without asking for the Markdown engine's chunk at all: " +
        "either the chunk is no longer named after `lib-headless.ts`, or nothing draws the " +
        "document's own text any more"
    ).toBeDefined();
    expect(
      timed.engineAskedMs ?? 0,
      "the Markdown engine was asked for before the document's text had even answered, so it " +
        "was warmed ahead of the render rather than loaded by the surface that draws the " +
        "document (`open-document-early.ts`, `DeploymentResilience.tsx`)"
    ).toBeGreaterThan(timed.textAnsweredMs ?? Number.POSITIVE_INFINITY);
  } finally {
    await alice.close();
  }
});

test("a phone draws the margin's ask cards only once its reader opens the review panel @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "above the phone layout the margin is a column of its own, which is always on screen"
  );
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);
  const question = "Which window should the lease ledger settle in?";
  await createAsk(
    issueKey,
    { options: [{ label: "The next window" }, { label: "The window after it" }], question },
    agent
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const asked = requestedPaths(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("The lease ledger", { timeout: 120_000 });

    // The whole page is up, and the shut sheet holds no card: a card behind it is 64 px of
    // `Review (1)`, and the thread read each card makes is a read the reader never sees the
    // answer to (dispatch://INFRA-341, R11).
    const sheet = page.getByTestId("margin-sheet");
    await expect(sheet.locator("[data-margin-item]")).toHaveCount(0);
    expect(asked.filter((what) => /^\/api\/v1\/asks\/[^/]+$/.test(what))).toHaveLength(0);
    // The count beside `Review` is the margin's own asks read, which it makes either way.
    const review = page.getByRole("button", { name: /^Open review panel/ });
    await expect(review).toHaveAccessibleName("Open review panel (1 open ask)");

    await review.click();
    await expect(sheet.getByText(question)).toBeVisible();
    await expect(sheet.locator("[data-margin-item]")).toHaveCount(1);
    await expect
      .poll(() => asked.filter((what) => /^\/api\/v1\/asks\/[^/]+$/.test(what)), {
        intervals: [100],
        message: "the opened card never read its own thread",
        timeout: 30_000,
      })
      .toHaveLength(1);
  } finally {
    await alice.close();
  }
});

/**
 * Records what each task of the page drew: the editor's top-level blocks and the decision
 * cards mounted into their shells. Install before the navigation it watches.
 *
 * A `MutationObserver` is what makes a task the unit: every record one task leaves is
 * delivered in a single callback, at that task's own microtask checkpoint, so a callback that
 * sees two cards appear is a task that drew two of them. The counts are read off the page
 * rather than taken from the records, because a card's own markup is a subtree the portal
 * brings in whole and the editor's blocks arrive as a batch's nodes: the count after each task
 * is the only thing either of them says plainly.
 *
 * A decision card is drawn when the shell's header slot - which `AskBlockView` leaves empty
 * (`web/src/features/doc/typed-block-drawing.ts`) - holds anything: that is `AskBlockCard`'s
 * portal, the first draw of the card and the one this row is about.
 */
async function drawsOfEachTask(page: Page): Promise<void> {
  await page.addInitScript(
    ({ blocks: blockSelector, headers }: { blocks: string; headers: string }) => {
      const drawn: DrawnInATask[] = [];
      window.__drawnInTasks = drawn;
      let blocks = 0;
      let cards = 0;
      const look = () => {
        const nowBlocks = document.querySelectorAll(blockSelector).length;
        const nowCards = [...document.querySelectorAll(headers)].filter(
          (header) => header.childElementCount > 0
        ).length;
        if (nowBlocks === blocks && nowCards === cards) return;
        drawn.push({
          at: Math.round(performance.now()),
          blocks: nowBlocks - blocks,
          cards: nowCards - cards,
        });
        blocks = nowBlocks;
        cards = nowCards;
      };
      // The document itself, because this script runs before the page has an element to watch.
      new MutationObserver(look).observe(document, { childList: true, subtree: true });
    },
    {
      blocks: builtBlocks,
      headers: "[data-dispatch-ask-block] > [data-dispatch-ask-header]",
    }
  );
}

// The five decision cards of a document are five React cards portalled into the editor's own
// shells, and their first draw is the dearest thing on the way to an open editor after the
// build itself: drawn together they were 470 ms of one 678 ms task on this pod, `iphone` at 4x
// CPU throttling, and that task was the one the host swaps the editor in with
// (dispatch://INFRA-341, R11). So each card is drawn in a task of its own, and no task both
// adopts a batch of the editor's blocks and draws a card. This row reads the seam, not the
// clock: it throttles nothing, because which task a draw lands in is the same however fast the
// machine is.
test(`each decision card of ${documentNames.small} is drawn in a task of its own @phone`, async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const { path } = await seedDocument("specs-small.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await drawsOfEachTask(page);
    await page.goto(path);
    await expect(documentEditor(page)).toBeVisible({ timeout: 240_000 });
    // Every decision of the document is hosting its ask card, so every card this row is about
    // has been drawn and the record below is complete.
    await expect(
      documentEditor(page).locator('[data-dispatch-ask-block] article[data-testid^="ask-"]')
    ).toHaveCount(decisionBlocks, { timeout: 240_000 });

    const drawn = await page.evaluate(() => window.__drawnInTasks ?? []);
    const drewACard = drawn.filter((task) => task.cards > 0);
    expect(
      drewACard.length,
      `the page drew its ${decisionBlocks} decision cards over ${drewACard.length} task(s): ` +
        "a card drawn in the same task as another is a freeze the reader feels as one"
    ).toBeGreaterThanOrEqual(decisionBlocks);
    expect(
      drewACard.filter((task) => task.cards > 1),
      "one task drew more than one decision card"
    ).toEqual([]);
    expect(
      drawn.filter((task) => task.cards > 0 && task.blocks !== 0),
      "one task both changed the editor's own blocks and drew a decision card: the build's " +
        "batch and the card's first draw are two tasks, never one"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});

test(`a reader answers a decision block in ${documentNames.large} @phone`, async ({ browser }) => {
  test.setTimeout(600_000);
  const block = decisionBlockId(decisionBlocks);
  const { path } = await seedDocument("specs-large.md", largeDocument);

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

/** The build's own budget of recovery reloads, spent before the page loads, so the row below
 *  reads one page rather than the three the shell's inline script would reload through when it
 *  sees every script of the build fail (`web/index.html`, `CHUNK_RELOADS_STORAGE_KEY` in
 *  `web/src/features/shell/DeploymentResilience.tsx`). */
const CHUNK_RELOADS_SPENT = "dispatch.chunk-reloads";

// The reader meets their document in the page's own bytes. The Go server draws the document's
// opening blocks into the page it sends a reader whose session may read it
// (`internal/dispatch/pmdoc/first_screen.go`, dispatch://INFRA-341 R11, acceptance A11), so the
// first block is on the screen with none of the app downloaded, parsed or run: this row blocks
// every script the build serves and finds it there. What it proves is the one thing no
// measurement can - that the first block owes nothing at all to the page's own code.
test("draws the document's first block with the app's code blocked @phone", async ({ browser }) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-blocked.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.addInitScript(
      ([key, now]) => {
        window.sessionStorage.setItem(key as string, JSON.stringify([now, now, now]));
      },
      [CHUNK_RELOADS_SPENT, Date.now()]
    );
    await page.route("**/*.js", (route) => route.abort());
    await page.goto(path);

    const first = page.locator('[aria-label="Document text"] [data-block-id]').first();
    await expect(first).toBeVisible();
    await expect(first).toHaveText(documentTitle(documentDiagrams.small));
    // The app never ran: its own root is empty, and the editor it would build is nowhere.
    await expect(page.locator("#root")).toBeEmpty();
    await expect(page.getByRole("textbox", { name: "Document editor" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
