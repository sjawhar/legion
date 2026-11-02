// What a heavy body costs the server when it is written as a document: the API answers every one
// of these shapes in under a second, stored or refused, and the cost grows with the body rather
// than faster than it. The same shapes drawn in a browser are `heavy-bodies.e2e.ts`, and both
// specs read them from one module, so the two measure the very same bytes. This one drives no
// browser, so it runs in one Playwright project and nowhere else: every project of a run shares
// the one server process, so a second project would time these writes against a server that
// already holds what each shape touches, and what the row says below about the first document
// write of its shape would be false there (sjawhar/legion#2519).
import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, test } from "@playwright/test";
import { createIssue, createIssueArtifact, createProject } from "./api";
import {
  bracketRunSizes,
  calloutBracketRun,
  lazyListItem,
  lazyQuote,
  markerChain,
  nested49ThenBlankLinesSizes,
  nestedThenBlankLines,
  plainListSizes,
} from "./heavy-bodies";
import { sql } from "./psql";
import { resetDatabase } from "./seed";

const agent = {
  actor: { id: "e2e-heavy-documents-agent", kind: "session" as const },
  as: "agent" as const,
};

async function seedIssue(title: string): Promise<{ key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  return await createIssue({ project: "CORE", title });
}

test.beforeEach(async () => {
  await resetDatabase();
});

interface HeavyDocument {
  /** The body an agent writes as a document of its own. */
  readonly body: string;
  /** What the row is called, in its assertions and in its annotations. */
  readonly name: string;
  /** The one shape whose first write in a server process pays for the memory it touches rather
   *  than for the parse, which the row writes once before it measures anything (the table's own
   *  comment below). */
  readonly warm?: true;
}

/** A row carries `half` where its write is the parse: the same shape at half the count its body
 *  holds, built by the one generator that built the body (`atBothSizes`). The write row times
 *  that write too and holds the body's own write against it, so a parse that grows faster than
 *  the body it reads is caught whatever the node costs a write. Which rows carry one, and why
 *  the others carry none, is on the write row. */
type DocumentRow =
  | (HeavyDocument & { readonly half?: string; readonly stored: true })
  | (HeavyDocument & { readonly half?: undefined; readonly stored: false });

/** Each heavy shape written as an issue document through the API, with the verdict Go gives it:
 *  stored, or refused as invalid markdown because it nests past `pmdoc/nesting.go`'s bound.
 *
 *  `warm` names the one shape whose cost is the parse itself rather than anything the write does
 *  with it, and whose first parse in a server process costs far more than its later ones. Its 49
 *  markers hold 50 blocks open down every one of the ~31,900 blank lines under them, and
 *  goldmark records a `lineStat` for each block on each line
 *  (`goldmark@v1.8.6/parser/parser.go:1106`), so the slice it appends into grows with the depth
 *  times the lines - about 3.1 million entries and ~75 MB for a body at the cap. Touching that
 *  much fresh memory for the first time in a process is page faults, not parsing: the same write
 *  is 316-343 ms once the process has done it and up to about 1.3 s the first time. The row
 *  writes such a body once to warm the process, records that first write's time, and holds the
 *  bound against the second, which is the steady state a reader's writes meet. */
const documentRows: readonly DocumentRow[] = [
  {
    body: plainListSizes.full,
    half: plainListSizes.half,
    name: "8,000 list items",
    stored: true,
  },
  { body: lazyQuote, name: "a quote followed by lines without its marker", stored: true },
  { body: lazyListItem, name: "a list item followed by lines without its marker", stored: true },
  {
    body: bracketRunSizes.full,
    half: bracketRunSizes.half,
    name: "15,000 opening brackets then 15,000 closing ones",
    stored: true,
  },
  {
    body: nested49ThenBlankLinesSizes.full,
    half: nested49ThenBlankLinesSizes.half,
    name: "49 list markers on one line then blank lines",
    stored: true,
    warm: true,
  },
  {
    body: nestedThenBlankLines,
    name: "100 list items nested by indentation then blank lines",
    stored: false,
  },
  { body: markerChain, name: "15,999 list markers on one line", stored: false },
  { body: calloutBracketRun, name: "a callout opener holding 15,000 brackets", stored: false },
];

/**
 * Resolves once this server owes nothing an earlier write left it: no settlement of a document
 * queued or running, and no document owed a block index.
 *
 * A durable document write records the settlement it owes in `doc_settlements_pending` within its
 * own transaction, and the settlement that has read every update deletes that row in the
 * transaction committing its writes (`packages/envoy/internal/dispatch/docs/persistence.go`,
 * `clearSettlementPending`). A version of more than 2,000 blocks leaves its blocks to the
 * block-index reconciliation instead of indexing them in the request
 * (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`), and migration 0122's trigger owes that
 * work by leaving `document_block_versions.version` null, which the reconciliation reads and sets
 * again when it has indexed the document (`docs/block_index.go`,
 * `RunBlockIndexReconciliation`). An empty `doc_settlements_pending` and no null version in
 * `document_block_versions` is therefore the server with neither piece of work outstanding.
 *
 * The harness runs one worker against one database (`e2e/playwright.config.ts`) and
 * `resetDatabase` empties it before each row, so every owed row of either table is this row's own.
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
        intervals: [200],
        message: `${what}: work queued before this write never finished`,
        timeout: 60_000,
      }
    )
    .toBe("0 settlements, 0 block indexes");
}

/**
 * One write at a time, with the server otherwise idle. Each write leaves the server two pieces of
 * work, and both are waited out before the next write is timed. The first is the document's own
 * settlement, which the server runs two seconds later (the document service's own default
 * `settle`, `packages/envoy/internal/dispatch/docs/service.go`; `e2e/run-server.sh` sets no
 * `DISPATCH_TEST_SETTLE_DELAY`) and which costs about what the write did. The second is the block
 * index of a body of more than 2,000 blocks, which the write defers (`deferBlockIndexBlocks`,
 * `docs/reference_excerpts.go`) and the block-index reconciliation indexes on a tick of its own,
 * up to 30 s later (`docs/block_index.go`, `BlockIndexReconcileInterval`). Of the bodies here
 * only the 8,000-item list and its 4,000-item half pass that bound - a list item is a block, so
 * they hold 8,001 and 4,001 of them, against one for the bracket run, two for each lazy body and
 * fifty for the 49 markers (`document_blocks` holds them all) - and the server says so as it
 * writes them: "a document version left its blocks to the block-index reconciliation ...
 * blocks=8001 bound=2000". Indexing 8,001 blocks is work that would otherwise land on whichever
 * write the tick caught. `write` therefore waits for both to be finished before it starts its
 * clock - no settlement queued and no document owed a block index (`everythingOwedFinished`) -
 * the warming write and the half-size write included, and what the bounds below hold is one
 * write alone. Those two ticks are what the row now spends most of its time waiting for: about
 * 50 s of the 1.3 min it takes in Chromium on this pod. The twenty runs quoted further down were
 * measured before this wait took in the block index, so a tick of it could still land on a write
 * there.
 *
 * So what a write costs while another document of the same server settles beside it is not
 * measured here: measured with these writes back to back it reached 1,322 ms, which
 * sjawhar/legion#2519 names under Not proven / risk.
 *
 * Two bounds hold each body, and the row asserts both of them. The absolute bound is that the
 * write answers in under 1,000 ms: it is asserted on every row, stored or refused, under no
 * condition, and it is never skipped, widened or stood down for a slow node, because a second is
 * what the writer waits and CI's runners are where that is judged. The second bound is
 * linearity: for each shape whose write is its parse, the row writes that same shape at half its
 * count, times that write exactly as it times the body, and holds the body's write to no more
 * than 3.5x it, so a parse whose cost grows faster than the body it reads reds this row on any
 * node, the fast ones CI runs on included, where the body alone would still answer in time.
 * Both numbers and the ratio between them are annotated before either bound is asserted, so a
 * run that reds reports what the body cost beside its half, whichever of the two bounds it was.
 * The absolute bound is asserted first, because a write the node itself stalled - 1,986 ms for a
 * body that costs 750 ms here - is no measurement of how the cost grows, and reading linearity
 * out of one would name a regression that is not there.
 *
 * The body is written before its half on each of the three rows held to the ratio. On the two
 * that carry no `warm` - the 8,000 items and the bracket run - that body is the first document
 * write of its shape in the server process, which is what the absolute bound has always measured
 * over them, with nothing but the drain above before it. It is not the first parse of those
 * bytes: in the one project this spec runs in, `heavy-bodies.e2e.ts` runs first on the same
 * server (one worker, files in name order) and creates comments holding the same two bodies,
 * whose create parses each of them once. The 49-marker row is the exception: `warm`
 * has the row write that shape once before the pair, so the write the absolute bound reads there
 * is the second of its shape - the steady state every later writer of such a body meets - and
 * the first write's own time is annotated beside it and held to no bound (the table above). A
 * first write pays for the memory its shape touches, about 200 ms on this pod whichever size
 * pays it, so the half that follows runs warm and a linear path reads around 2x: 1.22x to 3.11x
 * over twenty runs of this row here, every one of which held this bound, and 3.4x to 3.6x where
 * the node stalled the body's write past 1,100 ms and red the absolute bound first - against the
 * 1.99x the same halving costs the server-side write path at this head and 2.06x at
 * `main@origin` (sjawhar/legion#2519). The bound is 3.5x: above every ratio twenty runs have
 * shown, with room for a two-core node's jitter, and below the 4x a parse quadratic in its body
 * costs - nearer 5x once that warm-up is counted into it - so the bound still names the
 * regression this row was written for.
 *
 * The ratio is asserted on the three rows whose write is its parse: the 8,000 items, the bracket
 * run and the 49 markers, whose bodies cost 630-1,000 ms here. The two lazy rows carry no half
 * body, because their whole write is 88-274 ms, nearly all of it the round trip and the
 * document's own row, so a ratio over them measures jitter - 1.05x, 1.65x, 1.73x and 2.64x for
 * one and the same shape, measured while they carried one - and nothing of a parse. A refused
 * body is further from the claim still: two of the three are turned away before any parse of
 * them finishes (20-41 ms for the 15,999 markers, 6-18 ms for the callout, on a pod that is not
 * stalling), and the third, 100 items nested by indentation, is parsed before its nesting is
 * refused (406-1,746 ms here) but is refused at every size, so no reader ever stores it. Every
 * row keeps the absolute bound all the same.
 */
test("a document write of each heavy body answers in under a second, stored or refused", async () => {
  // Twelve writes - one of every body, the warming write, and a half-size one after each of the
  // three bodies held to the ratio - each waiting out the settlement the write before it queued,
  // two seconds and the settlement's own run, take the row from about 7 s to about 29 s; waiting
  // out the block index the 8,000-item body and its half defer, a tick up to 30 s away apiece,
  // takes it to 1.3 min here, past the default deadline. The deadline below is twice that, so a
  // tick at the far end of its interval for both of them still leaves the row its measurements.
  test.setTimeout(180_000);
  const issue = await seedIssue("Heavy documents");

  /** Writes one body as a document of its own, with no settlement and no block index of an
   *  earlier write running beside it, and answers what the write cost and refused. */
  async function write(body: string, name: string): Promise<{ refusal: unknown; took: number }> {
    await everythingOwedFinished(name);
    const started = performance.now();
    const refusal = await createIssueArtifact(issue.key, { content: body, name }, agent).then(
      () => undefined,
      (error: unknown) => error
    );
    return { refusal, took: performance.now() - started };
  }

  for (const [index, row] of documentRows.entries()) {
    // A document takes far more than a comment does, so each row is held to the comment cap: the
    // browser rows of `heavy-bodies.e2e.ts` and this one then read the very same shapes.
    expect(
      row.body.length,
      `${row.name}: the row is longer than a comment's body cap`
    ).toBeLessThanOrEqual(DISPATCH_BODY_MAX);
    if (row.warm === true) {
      // The first parse of this shape in a server process pays for the memory it touches, not for
      // the parsing (the table above): write it once, record that, and hold the bound against the
      // write after it, which is what every later writer of such a body meets.
      const first = await write(row.body, `heavy-${index}-first.md`);
      expect(first.refusal, `${row.name}: the warming write was refused`).toBeUndefined();
      test.info().annotations.push({
        description: `${Math.round(first.took)} ms`,
        type: `${row.name}: the first write of this shape in a fresh server`,
      });
    }
    const { refusal, took } = await write(row.body, `heavy-${index}.md`);
    let half: number | undefined;
    if (row.half !== undefined) {
      // This row's own shape at half its count, written and timed as the body above it was, and
      // waited out the same way: the two together are the growth the ratio holds.
      const smaller = await write(row.half, `heavy-${index}-half.md`);
      expect(smaller.refusal, `${row.name}: the half-size body was refused`).toBeUndefined();
      half = smaller.took;
    }
    // Both numbers and the ratio between them, so a reader of a CI run sees the shape of the
    // cost and not only that the row passed.
    test.info().annotations.push({
      description:
        half === undefined
          ? `${Math.round(took)} ms`
          : `${Math.round(half)} ms at half this shape, ${Math.round(took)} ms whole, ` +
            `${(took / half).toFixed(2)}x`,
      type: `${row.name}: what the write cost`,
    });

    if (row.stored) {
      expect(refusal, `${row.name}: the write was refused`).toBeUndefined();
    } else {
      expect(refusal, `${row.name}: the write was accepted`).toBeInstanceOf(Error);
      const message = refusal instanceof Error ? refusal.message : "";
      expect(message, row.name).toContain(" failed: 400 ");
      expect(message, row.name).toContain('"code":"INVALID_MARKDOWN"');
    }
    expect(took, `${row.name}: the write took ${Math.round(took)} ms`).toBeLessThan(1_000);
    if (half !== undefined) {
      expect(
        took,
        `${row.name}: ${Math.round(took)} ms for the body and ${Math.round(half)} ms for half ` +
          `of it, ${(took / half).toFixed(2)}x`
      ).toBeLessThanOrEqual(half * 3.5);
    }
  }
});
