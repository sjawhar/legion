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
import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { subscribeSession } from "./agents";
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
  proseOnlyDocument,
  proseOpening,
  sectionHeading,
  smallDocument,
} from "./large-documents";
import {
  longTaskTargetMs,
  measureDocument,
  type PageRead,
  printMeasurement,
  readTheReads,
  throttleTheCpu,
  watchTheDocument,
  watchTheReads,
} from "./large-documents-page";
import { openCommentThread, settledScrollTop, withoutScrollAnchoring } from "./mermaid-helpers";
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

/** What one task the page handed the browser while the editor was being built cost it: when
 *  the browser was given it, and how long it then waited before it ran. `whatTheBuildServes`
 *  records one of these per timer and per socket message, and the row below reads them. */
interface ServedLate {
  /** `performance.now()` when the browser was given the task: when `setTimeout` was called
   *  for a timer, and the message event's own `timeStamp` - the moment the browser made the
   *  event, which is when it queued the task that delivers it - for a socket message. */
  readonly handedAt: number;
  /** How many top-level blocks the editor held when it was handed over, for a timer the probe
   *  below hands over; `undefined` for a socket message, which arrives when it arrives. */
  readonly builtThen?: number;
  /** How long it waited from then until it ran. */
  readonly waitedMs: number;
}

/** The build's own window on the page's clock, as `whatTheBuildServes` sees it: the frame its
 *  first batch was found in, and the frame the finished editor was named in. */
interface TheBuildRan {
  firstBatchMs?: number;
  openedMs?: number;
}

/** Where the editor was when another person's word first showed up in its first block: how
 *  many blocks it held, and whether its build was over by then. */
interface TheirWordLanded {
  readonly blocks: number;
  readonly open: boolean;
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
    /** What the no-pixels row records about the words its reader is looking at: how far they
     *  ever moved on the screen, and whether the page above them ever changed height at all,
     *  which is what makes the row's answer mean anything. `started` is false where the row
     *  never found its heading. */
    __readersPlace?: { grew: boolean; moved: number; started: boolean };
    /** What `whatTheBuildServes` recorded: how long each timer the page handed the browser
     *  and each message the document's socket received waited to be run, and the build's own
     *  window, which says which of them were handed over while it ran. */
    __servedTimers?: ServedLate[];
    __servedMessages?: ServedLate[];
    __theBuildRan?: TheBuildRan;
    /** Where the editor was when another person's word reached its first block
     *  (`whenTheirWordLands`). */
    __theirWord?: TheirWordLanded;
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
//
// It is also the one row whose build the measurement could not read through, which is what
// `listenForThePage` in `large-documents-page.ts` is there for: holding the socket this long
// leaves the reader's own draw of the whole document on the page - every one of its 986 blocks
// and 140 diagrams - so the editor builds over all of it and takes minutes, where the two rows
// above pay about 35 s for the same build over a page holding one screenful. From a build's
// first batch to its last the page answers nothing the run asks it, so a number the run had to
// read was a number this row could not have until the build was over, and the wait for the
// editor's first block ran out on a block the page had recorded three seconds into it. The
// numbers are announcements the page makes now, which the rows above never needed.
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
 * The reading is the page's own rather than a poll from here, because a poll reads the build
 * from outside it: a `page.evaluate` is one task among the page's own, and the count it comes
 * back with is whatever the batches had reached by the time that task ran, never the frame a
 * reader was shown. A frame is the reading a reader gets, and a frame that holds part of a
 * document is the proof this row wants, since a build that froze the page would have drawn no
 * frame at all. Install before the navigation; read once the editor is open.
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
  test.setTimeout(900_000);
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
    // The same 900 s the other rows that read a build of this document carry, in place of the
    // 240 s that stood here, because most of what this row waits for is its own reading of
    // the build. Measured on this pod at 4x CPU throttling, with the socket released at the
    // draw's first block as above: the build itself - the editor's root going live until the
    // finished editor is named - is 34 s on `iphone` and 73 s on `chromium`; the per-frame
    // count of `framesOfTheBuild`, which scans the editor's 7,036 block elements in every
    // frame the build lets the browser draw, and this wait, whose role selector Playwright
    // runs over that same document again on every mutation the batches make, take the whole
    // to 100 s and 173 s. The 240 s had room for neither, and before the build's frontier was
    // fixed (`packages/proof-editor/src/progressive-build.ts`) it had room for nothing at
    // all: the same two numbers were 173 s and 281 s then.
    await expect(documentEditor(page)).toBeVisible({ timeout: 900_000 });

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
 * The edits are made from a frame of the page rather than driven from here, because where in
 * a build each one lands is what these rows are about and this side cannot say. A locator's
 * click is a task among the page's own: it is served between batches now
 * (`endOfTask` in `packages/proof-editor/src/progressive-build.ts`), but which batch it falls
 * between is a round trip's business, and an edit that landed after the last batch would
 * prove nothing. A frame is where a reader's keystroke lands anyway, and from a frame the row
 * reads how much of the document the editor held as it made each one.
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

// The next three rows are about everything on the page that is not the build, for as long as
// the build lasts. A batch hands the thread back so the page stays usable, and a hand-back the
// page is served nothing in is no hand-back at all: the build used to resume through
// `scheduler.yield()`, whose continuation the browser serves ahead of any posted task, so from
// a build's first batch to its last no socket message, no fetch callback and no timer ran - and
// React, whose scheduler posts one, drew nothing. Measured on this pod, `iphone` at 4x CPU
// throttling, a `page.evaluate` poll was answered twice and then not again for 209 s, and
// `scheduler.postTask()` at its default priority served a socket message in 7 ms while a
// timer waited 19,945 ms. It hands the thread back as a timer now (`endOfTask` in
// `packages/proof-editor/src/progressive-build.ts`), which queues the continuation behind
// everything already waiting, and the three rows are the three things a reader loses while
// nothing else runs: the page's own clock, a decision they answer, and another person's edit.

/** The longest a message on the document's socket may wait to be served while the editor is
 *  built. The same half-second R11 holds a main-thread task to: Chromium runs a networking
 *  task ahead of a timer, so a socket message waits only for whatever the thread is in the
 *  middle of, and the worst one measured across a build was 45 ms. */
const aMessageWaitsAtMostMs = 500;

/** How long a timer the page sets may wait, beyond the two longest main-thread tasks of the
 *  build it was handed over inside.
 *
 *  A timer is not held to the half-second above, and the measurement is why. It waits for the
 *  task in flight when it was handed over; then for the browser to lay out and paint a
 *  document of 406,319 characters, which is not a task and so is in no main-thread task
 *  number; then, where the batch's own timer was queued first, for that batch and the paint
 *  after it as well. So two of the build's tasks, and this is the room left for the painting
 *  between and after them. Measured on this pod at 4x CPU throttling, with one timer
 *  outstanding at a time: on `iphone`, 67 to 1,194 ms through the batches against a longest
 *  task of 564 ms, 2,941 and 4,437 ms for the two turns after the last batch where the
 *  binding adopts the document and the decision cards draw, and 2,650, 3,463 and 4,161 ms
 *  against longest tasks of 414 to 472 ms; on `chromium`, whose wider page lays out and
 *  paints more of the document at once and whose longest task inside a build is 3,713 to
 *  4,963 ms, 2,697, 8,473 and 9,073 ms. The most any of those waited past two of its build's
 *  longest tasks is 3,747 ms.
 *
 *  It is read against the build's own tasks rather than against the build's length, which is
 *  what stood here, because the length of the build is not what the hand-back buys: the same
 *  9 s wait is a fifth of a build of 45 s and two fifths of the 20 s build left once the
 *  frontier was fixed (`packages/proof-editor/src/progressive-build.ts`), so a share of the
 *  build failed this row for the build having got faster. What the hand-back does buy is that
 *  a timer waits for a turn of the page's loop rather than for the build, and that is what
 *  this holds it to at any speed: `scheduler.yield()` served a timer set at the first batch
 *  when the build ended - a wait of the whole build, against batches of 120 ms each - and
 *  `scheduler.postTask()` left one waiting 19,945 ms, each of them far past two tasks of its
 *  own build. */
const aTimerWaitsAtMostThisPastTwoTasksMs = 5_000;

/** How much build each timer served stands for, at worst: a page whose clock keeps running
 *  serves far more than this, and a page that served one timer and then nothing until the
 *  build ended serves far fewer. */
const aTimerEveryMs = 2_000;

/** The editor a reader already has while the rest of its blocks are built, as a selector an
 *  init script can carry (`editorBeingBuilt` is the same root as a locator). */
const liveEditorRoot = '.proof-editor[data-write-path="live"]';

/** The finished editor's own name, which the host writes once `createProofEditor` has answered
 *  its handle - a task after the binding connected - so an editor wearing it is one whose
 *  build is over (`web/src/features/doc/editor.ts`). */
const namedEditor = '[role="textbox"][aria-label="Document editor"]';

/** How often the probe below hands the browser a timer. Often enough that a build of a few
 *  seconds is covered by a dozen of them, rarely enough that the probe is not itself the thing
 *  keeping the page busy. */
const timerEveryMs = 250;

/**
 * Records what the page's own posted tasks cost it while the editor is being built: every
 * timer the page hands the browser, every message the document's socket receives, and the
 * build's own window, so the row can read the tasks handed over inside it.
 *
 * A timer is handed over from an animation frame, because a frame is the one thing that does
 * run through a starved build and so the one place a probe can be handed over from. A socket
 * message is timed from the message event's own `timeStamp`, which the browser sets when it
 * makes the event - that is, when it queues the task that delivers it - so the difference is
 * what the page's own thread made that message wait and nothing of the network's. The socket
 * is wrapped as a `construct` proxy rather than a subclass for the reason
 * `large-documents-page.ts` gives: the page's transport subclasses `globalThis.WebSocket` too.
 *
 * Install before the navigation; read once the editor has opened.
 */
async function whatTheBuildServes(page: Page): Promise<void> {
  await page.addInitScript(
    ({ every, live, named }: { every: number; live: string; named: string }) => {
      const timers: ServedLate[] = [];
      const messages: ServedLate[] = [];
      const ran: TheBuildRan = {};
      window.__servedTimers = timers;
      window.__servedMessages = messages;
      window.__theBuildRan = ran;
      window.WebSocket = new Proxy(window.WebSocket, {
        construct(made, args: [string | URL, (string | string[])?], asked) {
          const socket = Reflect.construct(made, args, asked) as WebSocket;
          if (!String(args[0]).includes("/ws/doc/")) {
            return socket;
          }
          socket.addEventListener("message", (event) => {
            messages.push({
              handedAt: event.timeStamp,
              waitedMs: performance.now() - event.timeStamp,
            });
          });
          return socket;
        },
      });
      let handedLast = Number.NEGATIVE_INFINITY;
      /** Whether a timer the probe handed over has still to run. One at a time, because what
       *  this row is about is how long the page waits for a timer, not how deep a queue of
       *  them grows: a frame of this page costs hundreds of milliseconds, so a probe handing
       *  one over every frame outruns the browser and measures its own backlog. A reader's
       *  page posts one and waits for it - React's scheduler, and the one-card-per-task timer
       *  of `useAskCardsDrawnInTasks` - which is what this does too. */
      let owed = false;
      const look = () => {
        const now = performance.now();
        const root = document.querySelector(live);
        if (ran.firstBatchMs === undefined && root !== null) {
          ran.firstBatchMs = now;
        }
        if (!owed && now - handedLast >= every) {
          const handedAt = now;
          handedLast = now;
          owed = true;
          // How far the build had got, read as the top-level child count of the editable host
          // rather than a query over the whole document, so the probe costs the page nothing.
          const built = root?.querySelector('[contenteditable="true"]')?.childElementCount ?? 0;
          setTimeout(() => {
            owed = false;
            timers.push({ builtThen: built, handedAt, waitedMs: performance.now() - handedAt });
          }, 0);
        }
        if (document.querySelector(named) !== null) {
          ran.openedMs = now;
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    { every: timerEveryMs, live: liveEditorRoot, named: namedEditor }
  );
}

/** The tasks of `served` the browser was given while the build ran, which are the ones the
 *  build could have starved. A task handed over inside the window and run after it is one of
 *  them, which is why they are read by when each was handed over. */
function handedToTheBuild(served: readonly ServedLate[], ran: TheBuildRan): ServedLate[] {
  const from = ran.firstBatchMs ?? Number.POSITIVE_INFINITY;
  const to = ran.openedMs ?? Number.POSITIVE_INFINITY;
  return served.filter((task) => task.handedAt >= from && task.handedAt <= to);
}

/** The longest any of them waited, and 0 where there were none. */
function longestWait(served: readonly ServedLate[]): number {
  return served.reduce((worst, task) => Math.max(worst, task.waitedMs), 0);
}

test(`a page timer and a socket message are served while ${documentNames.large}'s editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);
  const title = documentTitle(documentDiagrams.large);

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    // Bob has the document open with his caret in its first block before Alice loads hers, so
    // the only thing left for him to do while she builds is press the keys: every word he
    // types is a message on her socket.
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whatTheBuildServes(page);
    await page.goto(path, { waitUntil: "commit" });

    // From Alice's first batch until her editor opens, so his words cover her whole build
    // rather than a moment this row has to guess at. Both moments are announcements the page
    // makes rather than readings this side takes (`large-documents-page.ts`). He writes at a
    // person's pace - a word every second or two, a few dozen of them - rather than as fast
    // as the protocol allows, as the row above him does and for the same reason: four words a
    // second into a document of 986 blocks is a load no reader puts on another, and what this
    // row reads is what the build costs the page, not what a second client can.
    //
    // His words are short because it is his own page that they cost. He holds the same
    // 986-block document, and on a desktop width each of his keystrokes costs a layout of it
    // and of the margin beside it, so `Pangolin0` was three to four seconds of his and he
    // wrote three words into a build of thirteen - which is what a build of this document now
    // lasts, the frontier having stopped it rewriting the Yjs document a batch at a time
    // (`packages/proof-editor/src/progressive-build.ts`). What covers a build is how often he
    // writes, not how much.
    await reached.firstBlock;
    let open = false;
    const opened = reached.editorOpened.then((at) => {
      open = true;
      return at;
    });
    let words = 0;
    while (!open && words < 60) {
      await bobPage.keyboard.type(` Pan${words}`, { delay: 25 });
      await bobPage.waitForTimeout(500);
      words += 1;
    }
    const openedMs = await opened;

    const ran = (await page.evaluate(() => window.__theBuildRan)) ?? {};
    const everyTimer = (await page.evaluate(() => window.__servedTimers)) ?? [];
    const timers = handedToTheBuild(everyTimer, ran);
    const messages = handedToTheBuild(
      (await page.evaluate(() => window.__servedMessages)) ?? [],
      ran
    );
    const built = (ran.openedMs ?? 0) - (ran.firstBatchMs ?? 0);
    // Every timer of the load, not only the ones inside the build: what says whether a long
    // wait is the build's doing is where the long ones fall.
    const began = ran.firstBatchMs ?? 0;
    const spread = everyTimer
      .map(
        (timer) =>
          `${Math.round(timer.handedAt - began)}@${timer.builtThen ?? 0}:` +
          `${Math.round(timer.waitedMs)}`
      )
      .join(" ");
    // What one turn of the page's own loop cost while all that ran: a task the page was given
    // waits behind whatever the thread is in the middle of, so the longest of those says what
    // the waits above are made of.
    const tasks = ((await page.evaluate(() => window.__largeDocumentLongTasks)) ?? []).filter(
      (task) => task.at >= began && task.at <= (ran.openedMs ?? 0)
    );
    const longestTask = tasks.reduce((worst, task) => Math.max(worst, task.ms), 0);
    // Printed before any of them is read against its target, as every other number in this
    // file is: a run that missed one says what it measured rather than only what it wanted.
    console.log(
      `R11 posted tasks · a build of ${Math.round(built)} ms from its first batch, ` +
        `${timers.length} timer(s) handed over inside it one at a time, worst wait ` +
        `${Math.round(longestWait(timers))} ms (target ` +
        `${aTimerWaitsAtMostThisPastTwoTasksMs} ms past two of the longest task below, and one ` +
        `served every ${aTimerEveryMs} ms of the build); ${messages.length} socket message(s), ` +
        `worst wait ${Math.round(longestWait(messages))} ms (target ${aMessageWaitsAtMostMs} ms). ` +
        `${tasks.length} main-thread task(s) over 50 ms inside the build, longest ` +
        `${Math.round(longestTask)} ms. The editor opened at ${Math.round(openedMs)} ms, with ` +
        `${words} word(s) written into it while it was built (target more than 3). ` +
        `Every timer of the load, as handed-over@blocks-built:waited, counted from the ` +
        `first batch: ${spread}`
    );

    expect(words, "Bob never wrote while Alice's editor was being built").toBeGreaterThan(3);

    // The window is a real build: its first batch landed well before the editor opened, so
    // the tasks counted below were handed over while the batches held the thread.
    expect(ran.firstBatchMs ?? 0).toBeGreaterThan(0);
    expect(built).toBeGreaterThan(5_000);
    // And the page was given both kinds of task inside it, so neither number below is the
    // target met by nothing having happened.
    expect(messages.length, "no socket message arrived while the build ran").toBeGreaterThan(3);

    // The page's own clock ran right through the build: it was handed a timer, served it, was
    // handed the next, over and over. A build that served one and then nothing until it ended
    // is what this fails on.
    expect(
      timers.length,
      `the page served ${timers.length} timer(s) across a build of ${Math.round(built)} ms: ` +
        "its own clock stopped for the build rather than ticking through it"
    ).toBeGreaterThanOrEqual(Math.floor(built / aTimerEveryMs));
    expect(
      Math.round(longestWait(timers)),
      `a timer the page set during the build waited ${Math.round(longestWait(timers))} ms, ` +
        `with the longest main-thread task inside that build ${Math.round(longestTask)} ms: ` +
        "it is waiting for the build rather than for a turn of the page's own loop"
    ).toBeLessThanOrEqual(Math.round(longestTask * 2 + aTimerWaitsAtMostThisPastTwoTasksMs));
    expect(
      Math.round(longestWait(messages)),
      "a message on the document's socket waited longer than a reader feels as a freeze " +
        "while the editor was being built: another person's edit, or a reconnect, is held " +
        "for as long as the build lasts"
    ).toBeLessThanOrEqual(aMessageWaitsAtMostMs);
  } finally {
    await bob.close();
    await alice.close();
  }
});

test(`a decision answered while ${documentNames.large}'s editor is built is stored before it opens @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const block = decisionBlockId(1);
  const { issueKey, path } = await seedDocument("specs-large.md", largeDocument);
  const indexed = (await listIssueAsks(issueKey)).find((ask) => ask.block_id === block);
  expect(
    indexed,
    "the server indexed no ask for the document's first decision block"
  ).toBeDefined();

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await page.goto(path, { waitUntil: "commit" });
    // The page's clock against this run's, read before the first batch, so the reading is not
    // itself one of the things the build has to serve. `timeOrigin` never moves afterwards.
    const timeOrigin = await page.evaluate(() => performance.timeOrigin);
    await reached.firstBlock;

    // The card the editor hosts in the block the build has reached, answered while the rest of
    // the document is still being built: the radio is React's own state, the button's enabled
    // state is React's, and the answer is a fetch, so every step of this is a posted task the
    // build has to let through.
    const card = page
      .locator(`${liveEditorRoot} [data-dispatch-ask-block="${block}"]`)
      .locator("article[data-testid^=ask-]");
    await card.getByRole("radio", { name: "The next window" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();

    let storedAt = 0;
    await expect
      .poll(
        async () => {
          const state = (await listIssueAsks(issueKey)).find(
            (ask) => ask.block_id === block
          )?.state;
          if (state === "answered" && storedAt === 0) {
            storedAt = Date.now();
          }
          return state;
        },
        {
          intervals: [250],
          message: "the answer never reached the server",
          timeout: 600_000,
        }
      )
      .toBe("answered");

    const openedMs = await reached.editorOpened;
    const storedMs = storedAt - timeOrigin;
    console.log(
      `R11 decision during the build · answered and stored at ${Math.round(storedMs)} ms, ` +
        `editor open at ${Math.round(openedMs)} ms, on the page's own clock.`
    );
    expect(
      Math.round(storedMs),
      "the answer reached the server only once the build was over: a reader who answers a " +
        "decision while their editor is being built is answering nothing until it ends"
    ).toBeLessThan(Math.round(openedMs));
  } finally {
    await alice.close();
  }
});

/**
 * Records the first animation frame in which the editor's own first block holds `word`, with
 * how many blocks the editor held then and whether its build was over.
 *
 * Only the first block is read, because that is the block the other person writes in and
 * reading the whole of a 406,000-character document every frame would be the thing slowing the
 * page down. The loop does not stop when the editor is named: a word that lands only once the
 * build is over is the failure this row is about, and the row says so rather than finding
 * nothing recorded.
 */
async function whenTheirWordLands(page: Page, word: string): Promise<void> {
  await page.addInitScript(
    ({ live, named, wanted }: { live: string; named: string; wanted: string }) => {
      const look = () => {
        const root = document.querySelector(live);
        const first = root === null ? null : root.querySelector("[data-block-id]");
        if (root !== null && first !== null && (first.textContent ?? "").includes(wanted)) {
          window.__theirWord = {
            blocks: root.querySelectorAll("[data-block-id]").length,
            open: document.querySelector(named) !== null,
          };
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    { live: liveEditorRoot, named: namedEditor, wanted: word }
  );
}

test(`another person's edit reaches ${documentNames.large} while its editor is built @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const title = documentTitle(documentDiagrams.large);
  const word = "Pangolin.";

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const bobPage = await bob.newPage();
    await bobPage.goto(path);
    await expect(documentEditor(bobPage)).toContainText(title, { timeout: 240_000 });
    await expect(connectedDot(bobPage)).toHaveText("connected");
    await placeCaret(bobPage, "after", title);

    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    await whenTheirWordLands(page, word);
    await page.goto(path, { waitUntil: "commit" });

    // Bob writes one word into the document's first block, which Alice's first batch has
    // already built, the moment she has that batch: it is in her Yjs document before his
    // keystroke is over, and the only thing between it and her editor is the socket message
    // that carries it.
    await reached.firstBlock;
    await bobPage.keyboard.type(` ${word}`, { delay: 25 });

    const openedMs = await reached.editorOpened;
    const whole = await blocksBuilt(page);
    const landed = await page.evaluate(() => window.__theirWord);
    console.log(
      `R11 their edit during the build · the word reached the editor with ` +
        `${landed?.blocks ?? 0} of ${whole} blocks built, build ` +
        `${landed?.open === true ? "over" : "running"}, editor open at ${Math.round(openedMs)} ms.`
    );

    expect(landed, "the editor never held the word the other person wrote").toBeDefined();
    expect(landed?.blocks ?? 0).toBeGreaterThan(0);
    expect(
      landed?.open,
      "the word reached the editor only once its build was over: another person's edit waits " +
        "for the whole build before the reader sees it"
    ).toBe(false);
    expect(
      landed?.blocks ?? 0,
      "the editor held the whole document when the word reached it, so nothing about this run " +
        "crossed a build"
    ).toBeLessThan(whole);

    // And it is the document's own word afterwards, not a redraw the binding undid.
    await expect(documentEditor(page)).toContainText(`${title} ${word}`, { timeout: 240_000 });
    await everythingOwedFinished("the word another person wrote during the build");
    const { markdown } = await getArtifactText(artifactId);
    expect(markdown).toContain(`# ${title} ${word}`);
  } finally {
    await bob.close();
    await alice.close();
  }
});

// What the editor's build costs must be the document's business, not the page's. The same 986
// top-level blocks took about 35 s to build over a page holding one screenful and about 200 s
// over a page holding the whole drawn document - the same blocks, the same batches, and
// minutes that scaled with what was already in the DOM (dispatch://INFRA-341, R11). The cost
// was a forced layout of the whole page in each batch (`ProofDocument`'s `onBatch` read the
// draw's box) over a page the build never let the browser render, so the layout was never
// clean when it was read: 147 ms of each batch over the whole drawn document against 11 ms
// over one screenful. That is more than the 120 ms a batch is budgeted, so the build cut every
// batch to one or two blocks and paid it 414 times instead of 101. The build hands the thread
// back as a posted task now, so the browser renders between batches and the read is of a
// layout it has just done, and the draw is indexed once rather than scanned in each batch
// (`takesTheBuiltBlocks`). This row is what keeps that: the build's own time over the whole
// drawn document, against the build's own time over a screenful, measured one after the other
// in one run so the pod's own load is the same for both.

/** How many times longer the build over a page holding the whole drawn document may take than
 *  the build over a page holding one screenful. A page carrying the whole draw beside the
 *  editor carries about twice the boxes for the browser to lay out on its way through, which
 *  no build can give back; what this rules out is the quadratic above, which was seven times
 *  on this pod. Measured at 1.6 to 2.2 times over the runs this row was written against. */
const theSameRangeTimes = 3;

/** How long one editor's build took, on the page's own clock. */
interface TheBuildsTime {
  /** When the reader first had a block of the editor's own in the viewport, which is its
   *  first batch. */
  readonly firstBlockMs: number;
  /** When the editor opened: its last batch in and its binding live. */
  readonly openedMs: number;
  /** The build itself: the time between the two. */
  readonly buildMs: number;
}

/**
 * Opens `path` on a page of its own and answers how long the editor's build took there.
 *
 * With `overTheWholeDraw`, the document's socket is held until the page's own draw holds that
 * many of the document's top-level blocks, so the build runs over a page already holding the
 * whole document; without it the build runs over the screenful the draw has drawn by then,
 * which is what a reader whose socket answers at once gets. Both numbers are announcements
 * the page makes, so neither is a reading this side takes.
 */
async function theBuildsTime(
  browser: Browser,
  path: string,
  { overTheWholeDraw }: { overTheWholeDraw?: number } = {}
): Promise<TheBuildsTime> {
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    const reached = await watchTheDocument(page);
    const transport =
      overTheWholeDraw === undefined ? undefined : await documentTransport(page, { holding: true });
    await page.goto(path, { waitUntil: "commit" });
    if (transport !== undefined) {
      await expect
        .poll(() => page.locator(drawnBlocks).count(), {
          intervals: [1_000],
          message: "the page never drew the whole document, so no build ran over one",
          timeout: 300_000,
        })
        .toBeGreaterThanOrEqual(overTheWholeDraw ?? 0);
      await transport.release();
    }
    const firstBlock = await reached.firstBlock;
    const openedMs = await reached.editorOpened;
    return { buildMs: openedMs - firstBlock.at, firstBlockMs: firstBlock.at, openedMs };
  } finally {
    await alice.close();
  }
}

test(`${documentNames.large}'s editor builds over the whole drawn document in the time it takes over one screenful @phone`, async ({
  browser,
}) => {
  test.setTimeout(1_800_000);
  const { artifactId, path } = await seedDocument("specs-large.md", largeDocument);
  const whole = await topLevelBlocks(artifactId);
  expect(whole).toBeGreaterThan(900);

  // One screenful first, because that is the build the two measuring rows are held to: a pod
  // that has grown slower since would show it there before it showed it in the ratio.
  const screenful = await theBuildsTime(browser, path);
  const drawn = await theBuildsTime(browser, path, { overTheWholeDraw: whole });
  console.log(
    `R11 build time · over one screenful ${Math.round(screenful.buildMs)} ms ` +
      `(first block ${Math.round(screenful.firstBlockMs)} ms, editor open ` +
      `${Math.round(screenful.openedMs)} ms); over the whole drawn document ` +
      `(${whole} blocks) ${Math.round(drawn.buildMs)} ms (first block ` +
      `${Math.round(drawn.firstBlockMs)} ms, editor open ${Math.round(drawn.openedMs)} ms); ` +
      `${(drawn.buildMs / screenful.buildMs).toFixed(2)} times, against ${theSameRangeTimes}.`
  );

  expect(screenful.buildMs).toBeGreaterThan(0);
  expect(drawn.buildMs).toBeGreaterThan(0);
  expect(
    drawn.buildMs,
    "the editor took far longer to build over a page holding the whole drawn document than " +
      "over one holding a screenful, for the same blocks in the same batches: the build is " +
      "paying for what the page already holds once per batch again"
  ).toBeLessThanOrEqual(screenful.buildMs * theSameRangeTimes);
});

// The three rows below are about what the page does in front of the document's own draw,
// which is what the measured numbers above are mostly made of. The document's own reads are
// started from the address, the first of them before any of the page's own code has
// evaluated, so they wait for nothing the entry chunk or the issue page does; and the margin
// beside the document draws no card a reader cannot see. None of the three is timed: each
// reads what the page asked for and what it drew, never how long either took.

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
// So the engine left the boot task, and then the surface stopped asking for it at all while an
// editor is on its way (`DocumentPreview`'s `waitsForTheBuildMs`): on such a page the pass it
// would build the engine for is dropped at the build every time - the sync landed 0.5 to 1.1 s
// after the surface mounted in every run measured on this pod, and the parse worker answers the
// whole document about 16 s later - so the engine was a second headless Proof editor, 294 to
// 546 ms of the main thread, built for a draw that never reached the reader.
//
// This row reads the browser's own resource timing after the reader has an editor they can type
// in: neither the engine's chunk nor the parse worker's was ever asked for.
test("a page the server drew a document into builds no Markdown engine while its editor comes @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the engine's one reader here is the document's own preview, which is what a phone draws: " +
      "at a width that opens the margin, its cards draw Markdown of their own"
  );
  test.setTimeout(300_000);
  // Plain prose, well over the parse worker's threshold and well over the first screen the
  // server draws: nothing on this page but the document's own draw has any use for the engine,
  // so a chunk asked for is that draw asking for it. A decision block would bring its own card
  // and a diagram its own renderer, and neither is what this row is about.
  const prose = `# The lease ledger\n\n${"The paragraph a reader meets. ".repeat(8)}\n\n`.concat(
    Array.from({ length: 120 }, (_, index) => `## Section ${index}\n\nWhat it says.`).join("\n\n"),
    "\n"
  );
  const { path } = await seedDocument("ledger.md", prose);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(path, { waitUntil: "commit" });

    // The reader's own block: the editor's first, under a root that takes edits, which is what
    // the wait is for. Until it is there, nothing says the engine was skipped rather than
    // merely late.
    await expect(
      page.locator('.proof-editor[data-write-path="live"] [data-block-id]').first()
    ).toBeVisible({ timeout: 180_000 });

    // What the browser itself timed. The engine's chunk is the headless editor's entry
    // (`@legion/proof-editor/headless`, `packages/proof-editor/src/lib-headless.ts`), which the
    // build names after that module, and the parse worker is the chunk the queue creates for a
    // body over the threshold; nothing else on this page asks for either.
    const asked = await page.evaluate(() => {
      const resources = performance.getEntriesByType("resource") as PerformanceResourceTiming[];
      const paths = resources.map((resource) => new URL(resource.name).pathname);
      return {
        engine: paths.filter((name) => /^\/assets\/lib-headless-[^/]*\.js$/.test(name)),
        worker: paths.filter((name) => /^\/assets\/markdown-parse\.worker-[^/]*\.js$/.test(name)),
        text: paths.filter((name) =>
          /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/.test(name)
        ),
      };
    });

    expect(
      asked.text.length,
      "the document's text was never read, so this page never drew the document at all"
    ).toBeGreaterThan(0);
    expect(
      asked.engine,
      "the page built its own Markdown engine while the editor was on its way: a second " +
        "headless Proof editor for a draw the build replaces before it can paint " +
        "(`DocumentPreview`'s `waitsForTheBuildMs`)"
    ).toEqual([]);
    expect(
      asked.worker,
      "the page posted the whole document to the parse worker while the editor was on its way"
    ).toEqual([]);
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

// --- The reads the first editable block needs none of ------------------------------------
//
// A document address spends its first seconds on one thing, and the three rows below are about
// everything else the page asks Dispatch for while it does. Measured on this pod, `iphone` at
// 4x CPU throttling, seven reads went out in front of the first block a reader can type in -
// 4,328 to 5,493 ms on a load whose preview mounted at 5,611 - and the block needs none of
// them: they fill the issue page's non-document panels (dispatch://INFRA-341, R11). They are
// made after the editor's first batch now (`web/src/features/doc/first-batch.ts`).
//
// Each row holds the document's socket (`documentTransport`), so the first batch happens when
// the row says so rather than when a loaded machine gets round to it, and reads the order off
// the browser's own resource timing (`watchTheReads`). None of them is a measurement: what
// they prove is which side of the batch a read falls on.
//
// Their document holds no decision block (`proseOnlyDocument`), because a decision block in
// the reader's own draw is a card they can answer before any editor exists and it reads the
// document's asks and its recipients to do so: a reader who has one on the screen is waiting
// for those two reads, so they go out there rather than at the batch, which is the rule
// working rather than failing.

/** The seven, by the path each goes out on. `/references` is the spec's `part_of` read, which
 *  the page makes only for a reader who has read the document before, so the row that reads it
 *  opens the spec twice. */
function readsTheFirstBlockNeedsNone(issueKey: string, project: string): readonly string[] {
  return [
    `/issues/${issueKey}/comments`,
    `/issues/${issueKey}/asks`,
    "/agents",
    "/settings/repo-projects",
    `/projects/${project}/architecture-sources`,
    `/issues/${issueKey}/subscribers`,
    "/references",
  ];
}

/** Every read the page made on one of `paths`, with its query, in the order the browser timed
 *  them: what a row names when it says a read went out it should not have. */
function readsAmong(reads: readonly PageRead[], paths: readonly string[]): PageRead[] {
  return reads.filter((read) => paths.includes(read.path.split("?")[0] ?? read.path));
}

/** When each of `paths` went out, in order, by path: every read the page made on that path
 *  with its query dropped, so one entry means one request. */
function whenEachWentOut(
  reads: readonly PageRead[],
  paths: readonly string[]
): Map<string, number[]> {
  const when = new Map<string, number[]>(paths.map((path) => [path, []]));
  for (const read of readsAmong(reads, paths)) {
    when.get(read.path.split("?")[0] ?? read.path)?.push(read.at);
  }
  return when;
}

/** Puts the order a row read on its annotations, which the report keeps with the result, and on
 *  stdout, which `--reporter line` prints as the row runs, exactly as `printMeasurement` does:
 *  a report says which read went out when rather than only that the row was green. */
function printTheOrder(
  what: string,
  firstBatchMs: number | undefined,
  when: Map<string, number[]>
): void {
  const line = `first batch ${
    firstBatchMs === undefined ? "never" : `${Math.round(firstBatchMs)} ms`
  }; ${[...when]
    .map(
      ([path, at]) => `${path} ${at.length === 0 ? "never" : `${at.map(Math.round).join(", ")} ms`}`
    )
    .join("; ")}`;
  test.info().annotations.push({ description: line, type: what });
  console.log(`${what} · ${line}`);
}

/** How long a row waits, with the socket held and the page settled, before it decides a read
 *  the page was going to make has not been made. The page's own draw of the document is on the
 *  screen well inside it, and every panel of the header has rendered. */
const settledWithTheSocketHeldMs = 10_000;

test("the reads the first editable block needs none of go out after the editor's first batch @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-order.md", proseOnlyDocument);
  const paths = readsTheFirstBlockNeedsNone(issueKey, "BIG");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The page is up and the reader has the document's own draw; the header has rendered every
    // panel the held reads belong to; no editor is being built.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: 120_000,
    });
    await expect(page.getByRole("button", { name: /^Subscribers/ })).toBeVisible();
    await expect(page.locator('[data-testid="issue-components"]')).toBeVisible();
    await page.waitForTimeout(settledWithTheSocketHeldMs);

    const held = await readTheReads(page);
    expect(held.firstBatchMs).toBeUndefined();
    expect(
      readsAmong(held.reads, paths),
      "a read the first editable block needs none of went out before the editor's first batch"
    ).toEqual([]);

    // Released, the editor builds, and every one of them goes out behind its first batch.
    await transport.release();
    await expect(editorBeingBuilt(page)).toBeAttached({ timeout: 120_000 });
    await expect
      .poll(
        async () =>
          [...whenEachWentOut((await readTheReads(page)).reads, paths)].filter(
            ([, at]) => at.length === 0
          ).length,
        { message: "a read the page held behind the first batch never went out", timeout: 120_000 }
      )
      .toBe(0);

    const after = await readTheReads(page);
    const when = whenEachWentOut(after.reads, paths);
    printTheOrder("reads after the first batch", after.firstBatchMs, when);
    const firstBatchMs = after.firstBatchMs ?? 0;
    expect(firstBatchMs).toBeGreaterThan(0);
    for (const [read, at] of when) {
      expect(at, `${read} never went out`).not.toHaveLength(0);
      for (const went of at) {
        expect(went, `${read} went out before the first batch`).toBeGreaterThan(firstBatchMs);
      }
    }
    // And none of them was made twice: one request per address, whichever panel wanted it.
    const addresses = readsAmong(after.reads, paths).map((read) => read.path);
    expect(addresses, "a read went out twice").toHaveLength(new Set(addresses).size);
  } finally {
    await alice.close();
  }
});

// The other half of the rule: a read is held from a reader who is not waiting for it, never
// from one who is. The Subscribers panel is the issue header's own, shut until the reader
// opens it, and opening it reads who is subscribed there and then - with the document's socket
// still held, so no first batch has happened and none is on its way.
test("a reader who opens a panel before the first batch has its read started then @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-reached.md", proseOnlyDocument);
  const subscribers = `/issues/${issueKey}/subscribers`;
  const others = readsTheFirstBlockNeedsNone(issueKey, "BIG").filter(
    (read) => read !== subscribers
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    const toggle = page.getByRole("button", { name: /^Subscribers/ });
    await expect(toggle).toBeVisible({ timeout: 120_000 });
    await page.waitForTimeout(settledWithTheSocketHeldMs);
    expect((await readTheReads(page)).reads.filter((read) => read.path === subscribers)).toEqual(
      []
    );

    await toggle.click();
    await expect
      .poll(
        async () =>
          (await readTheReads(page)).reads.filter((read) => read.path === subscribers).length,
        { message: "the panel the reader opened never read its subscribers", timeout: 30_000 }
      )
      .toBe(1);

    // And it was the reader's own panel that did it, with no first batch and none of the other
    // held reads behind it.
    const reached = await readTheReads(page);
    printTheOrder(
      "the reader's own panel",
      reached.firstBatchMs,
      whenEachWentOut(reached.reads, [subscribers, ...others])
    );
    expect(reached.firstBatchMs).toBeUndefined();
    expect(
      [...whenEachWentOut(reached.reads, others)].filter(([, at]) => at.length > 0),
      "opening one panel started a read belonging to another"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});

// And a page with no document on it holds nothing: there is no first batch to wait for, so
// every one of those reads goes out as it did before INFRA-341. The issue's Conversation is
// that page - the same header, the same margin, no document surface - and the row reads it
// with no socket held at all, because there is no socket to hold.
test("a page with no document on it makes those reads as it always did @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey } = await seedDocument("specs-no-document.md", proseOnlyDocument);
  // Every one of the seven but the spec's `part_of` read, which belongs to the document this
  // page does not show.
  const paths = readsTheFirstBlockNeedsNone(issueKey, "BIG").filter(
    (read) => read !== "/references"
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await page.goto(`/issues/${issueKey}/conversation`);
    await expect(page.getByRole("button", { name: /^Subscribers/ })).toBeVisible({
      timeout: 120_000,
    });

    await expect
      .poll(
        async () =>
          [...whenEachWentOut((await readTheReads(page)).reads, paths)].filter(
            ([, at]) => at.length === 0
          ).length,
        { message: "a read of a page with no document on it never went out", timeout: 60_000 }
      )
      .toBe(0);

    const made = await readTheReads(page);
    printTheOrder("no document on the page", made.firstBatchMs, whenEachWentOut(made.reads, paths));
    // Nothing was ever built, so nothing was ever waited for.
    expect(made.firstBatchMs).toBeUndefined();
    await expect(page.locator('.proof-editor[data-write-path="live"]')).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// And the rule the three rows above are held to: a panel the page draws after the reader is in
// place moves what they are looking at by no pixels (`web/src/features/doc/readers-place.ts`).
// The panel here is the issue header's Subscribers list, whose read is one of the seven; the
// row holds that read at the server until the reader is deep in the document, so the panel
// lands under a page that has stopped moving rather than during the load, which is the state
// the rule is about. The browser's own scroll anchoring is off (`withoutScrollAnchoring`), as
// it is in Safari before 27, so what keeps the reader in place is the page's own doing.
test("a panel arriving while the reader is in a long document moves what they are looking at by no pixels @phone", async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const { issueKey, path } = await seedDocument("specs-large.md", largeDocument);
  await subscribeSession(issueKey);
  // A section well down the document: the heading the reader stops at, and the words the row
  // measures. Far enough in that the whole header, panel and all, is off their screen.
  const landmarkText = sectionHeading(Math.floor(documentDiagrams.large / 2));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const subscribers = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${issueKey}/subscribers`, async (route) => {
      await subscribers.promise;
      await route.continue();
    });
    await page.goto(path);
    await expect(documentEditor(page)).toBeVisible({ timeout: 240_000 });
    await withoutScrollAnchoring(page);

    // The reader opens the panel - its read is still held, so it is the empty frame of itself -
    // and reads their way down to the landmark, which is where they are when it lands.
    await page.getByRole("button", { name: "Subscribers", exact: true }).click();
    const landmark = documentEditor(page).getByRole("heading", { name: landmarkText });
    await landmark.scrollIntoViewIfNeeded();
    await settledScrollTop(page);
    await landmark.evaluate((heading) => heading.scrollIntoView({ block: "start" }));
    expect(await settledScrollTop(page)).toBeGreaterThan(10_000);

    // From here the row reads the landmark after every frame the browser paints - a task
    // scheduled from an animation frame runs once that frame is on the screen - so what it
    // records is what the reader saw, never a page the correction had not reached yet.
    await page.evaluate((text: string) => {
      const heading = [...document.querySelectorAll(".proof-editor h2")].find(
        (candidate) => candidate.textContent === text
      );
      window.__readersPlace = { grew: false, moved: 0, started: heading !== undefined };
      if (heading === undefined) return;
      const scrolled = document.scrollingElement?.scrollTop ?? 0;
      const started = heading.getBoundingClientRect().top;
      const sample = () => {
        const place = window.__readersPlace;
        if (place === undefined) return;
        const top = heading.getBoundingClientRect().top;
        const above = top + (document.scrollingElement?.scrollTop ?? 0) - scrolled;
        place.moved = Math.max(place.moved, Math.abs(top - started));
        if (Math.abs(above - started) > 0.5) place.grew = true;
        requestAnimationFrame(() => window.setTimeout(sample, 0));
      };
      sample();
    }, landmarkText);

    subscribers.resolve();
    await expect(page.getByRole("region", { name: "Subscribed agents" })).toContainText(
      "e2e-session-title",
      { timeout: 60_000 }
    );
    await expect
      .poll(async () => (await page.evaluate(() => window.__readersPlace))?.grew, {
        message: "the panel landing never changed how much page stands above the reader",
        timeout: 60_000,
      })
      .toBe(true);

    // The reader's words stay on the line they were on. Not to the exact float: a scroll offset
    // is a fraction of a pixel on a page this tall, so the correction lands within one, and a
    // pixel is the unit a reader has.
    const place = await page.evaluate(() => window.__readersPlace);
    expect(place?.started, "the row never found the heading it measures").toBe(true);
    expect(
      place?.moved ?? Number.NaN,
      "the panel moved the words the reader was looking at"
    ).toBeLessThan(1);
  } finally {
    await alice.close();
  }
});
