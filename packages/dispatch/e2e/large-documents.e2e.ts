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
} from "./api";
import { connectedDot, documentEditor, documentTransport, placeCaret } from "./editor";
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
  sectionHeading,
  smallDocument,
} from "./large-documents";
import { measureDocument, printMeasurement, throttleTheCpu } from "./large-documents-page";
import { openCommentThread, settledScrollTop } from "./mermaid-helpers";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** What one animation frame of the swap held: how many of the document's blocks the editor's
 *  root had built, whether the document's own draw was on the screen, and whether the editor
 *  was. `framesOfTheSwap` records one of these per frame and the swap row reads them. */
interface SwapFrame {
  readonly blocks: number;
  readonly drawn: boolean;
  readonly shown: boolean;
}

declare global {
  interface Window {
    /** How many of the document's blocks the editor's root held at each animation frame, which
     *  `framesOfTheBuild` records and the two progressive-build rows read. */
    __blocksBuilt?: number[];
    /** What each animation frame of the swap held, which `framesOfTheSwap` records and the swap
     *  row reads. */
    __swapFrames?: SwapFrame[];
  }
}

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
        measured
      );

      // What a measured number needs to mean anything: the document really drew, the editor
      // really opened, and the page really scrolled afterwards, so the longest task covers the
      // whole build and then a scroll through the whole document.
      await expect(documentEditor(page)).toBeVisible();
      expect(measured.firstBlockMs).toBeGreaterThan(0);
      expect(measured.editorOpenedMs ?? 0).toBeGreaterThan(0);
      expect(measured.windowClosedMs).toBeGreaterThan(measured.editorOpenedMs ?? 0);
      expect(measured.scrollSteps).toBeGreaterThan(0);
      expect(measured.longestTaskMs).toBeGreaterThan(0);

      expect(measured.firstBlockMs).toBeLessThanOrEqual(row.firstBlockTargetMs);
      expect(measured.longestTaskMs).toBeLessThanOrEqual(longestTaskTargetMs);
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

    // The same first block the measurement reads (`large-documents-page.ts`), drawn with no
    // socket, no sync and no editor: it carries `data-block-id` as the editor's blocks do.
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

/**
 * Records, every animation frame until the editor is on the screen, what that frame held: how
 * many of the document's blocks the editor's root had built, whether the document's own draw
 * was on the screen, and whether the editor was. Install before the navigation.
 *
 * The editor's root carries `proof-editor` from the moment it is created and is hidden until
 * the swap shows it, so a box of its own is exactly what the reader can see. The reading is the
 * page's own rather than a poll from here, for the reason `framesOfTheBuild` gives: a poll
 * cannot land between the tasks of the build, and a frame is what the reader is shown anyway.
 */
async function framesOfTheSwap(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const frames: SwapFrame[] = [];
    window.__swapFrames = frames;
    const look = () => {
      const editor = document.querySelector(".proof-editor");
      const draw = document.querySelector('[aria-label="Document text"]');
      const shown = (editor?.getBoundingClientRect().height ?? 0) > 0;
      frames.push({
        blocks: editor === null ? 0 : editor.querySelectorAll("[data-block-id]").length,
        drawn: (draw?.getBoundingClientRect().height ?? 0) > 0,
        shown,
      });
      // The frame the editor is first shown in is the swap, and the last one worth reading.
      if (shown) return;
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  });
}

test(`no frame of ${documentNames.large}'s swap holds both documents or neither @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const { path } = await seedDocument("specs-large.md", largeDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await throttleTheCpu(page);
    await framesOfTheSwap(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeAttached(
      { timeout: 240_000 }
    );

    await transport.release();
    await expect(documentEditor(page)).toBeVisible({ timeout: 480_000 });
    const whole = await blocksBuilt(page);
    const frames = await page.evaluate(() => window.__swapFrames ?? []);
    const drawnAt = frames.findIndex((frame) => frame.drawn);
    expect(drawnAt, "the document's own draw was never on the screen").toBeGreaterThanOrEqual(0);

    // The invariant the swap keeps: from the frame the reader first has a document in, every
    // frame holds exactly one of the two - never the draw and the editor together, and never
    // the page with neither of them on it.
    const swapped = frames.slice(drawnAt);
    expect(
      swapped.filter((frame) => frame.drawn === frame.shown),
      "a frame held both the document's own draw and its editor, or neither of them"
    ).toEqual([]);
    expect(swapped.at(-1)).toMatchObject({ blocks: whole, drawn: false, shown: true });

    // And the swap is a task of its own, after the last batch and after the binding's connect:
    // the page drew frames holding the whole document built and the editor still hidden, which
    // a build that ended in one task - the last batch, the connect and the swap together, 1,370
    // ms of it on this phone - never shows.
    expect(
      swapped.filter((frame) => frame.blocks === whole && !frame.shown).length,
      "no frame was drawn with the document built and the editor not yet shown: the last " +
        "batch, the binding's connect and the swap were one task again"
    ).toBeGreaterThan(0);
  } finally {
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
