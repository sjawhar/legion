// What a large document costs a reader on a phone, measured under the set-up
// dispatch://INFRA-341/spec names: the `iphone` project under 4x CPU throttling, the time from
// the start of navigation until the document's first block is in the viewport, and the longest
// main-thread task until the page has scrolled to the end. The two documents are
// `large-documents.ts`: one shaped like dispatch://AHXV6-1/artifact/intermediate-specs-md, and
// one five times as large.
//
// These rows print their numbers and assert no target. The targets of "Targets for large
// documents" (3 s, 5 s and 500 ms) are read against these runs at the merge-base, which is what
// says which of them the unchanged build already meets (INFRA-341, stage 10a); the rows that
// assert them are stage 10b's.
import { expect, test } from "@playwright/test";

import { createIssue, createIssueArtifact, createProject } from "./api";
import { documentEditor } from "./editor";
import {
  diagramsIn,
  documentDiagrams,
  documentNames,
  largeDocument,
  smallDocument,
} from "./large-documents";
import { measureDocument, printMeasurement } from "./large-documents-page";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

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
  /** How long the row waits for the first block. An unthrottled build draws either document in
   *  seconds and the merge-base takes tens of them under the throttle, so this is a wait for a
   *  build that never draws at all rather than a target in disguise. */
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
    firstBlockTimeoutMs: 120_000,
    name: "specs-small.md",
    timeoutMs: 300_000,
    what: documentNames.small,
  },
  {
    content: largeDocument,
    diagrams: documentDiagrams.large,
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

      // What a printed number needs to mean anything: the document really drew, and the page
      // really scrolled, so the longest task covers a scroll through the whole document.
      await expect(documentEditor(page)).toBeVisible();
      expect(measured.firstBlockMs).toBeGreaterThan(0);
      expect(measured.scrollSteps).toBeGreaterThan(0);
      expect(measured.longestTaskMs).toBeGreaterThan(0);
    } finally {
      await alice.close();
    }
  });
}
