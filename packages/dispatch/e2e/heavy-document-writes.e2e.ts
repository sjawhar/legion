// What a heavy body costs the server when it is written as a document: the API answers every one
// of these shapes in under a second, stored or refused, and the cost grows with the body rather
// than faster than it. The same shapes drawn in a browser are `heavy-bodies.e2e.ts`, and both
// specs read them from one module, so the two measure the very same bytes. This one drives no
// browser, so it runs in one Playwright project and nowhere else: every project of a run shares
// the one server process, so a second project would time these writes against a server that
// already holds what each shape touches, and what the row says below about the first document
// write of its shape would be false there (sjawhar/legion#2519). That project is
// `chromium-alone` (e2e/playwright.config.ts), which Playwright starts only once every other
// project has finished and which runs one row at a time: a second read from the wall clock is a
// second only where nothing else holds the machine (sjawhar/legion#2612).
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
 * An emptied database and the issue the write that follows goes into: what "the server is
 * otherwise idle" means here, and how a timed write is kept clear of the work the write before
 * it left behind.
 *
 * A durable document write leaves the server two pieces of work. The first is the document's own
 * settlement, which the write records in `doc_settlements_pending` inside its own transaction
 * (`packages/envoy/internal/dispatch/docs/persistence.go`) and the server runs a settle delay
 * later - 250 ms under this harness (`DISPATCH_TEST_SETTLE_DELAY`, `e2e/run-server.sh`), in
 * place of the document service's own 2 s - and which costs about what the write did. The
 * second is the block index of a body of more than 2,000 blocks, which the write defers
 * (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`) and the block-index reconciliation
 * indexes on a tick of its own, up to 30 s later (`docs/block_index.go`,
 * `BlockIndexReconcileInterval`). Of the bodies here only the 8,000-item list and its 4,000-item
 * half pass that bound - a list item is a block, so they hold 8,001 and 4,001 of them, against
 * one for the bracket run, two for each lazy body and fifty for the 49 markers
 * (`document_blocks` holds them all) - and the server says so as it writes them: "a document
 * version left its blocks to the block-index reconciliation ... blocks=8001 bound=2000".
 * Indexing 8,001 blocks is work that would otherwise land on whichever write the tick caught.
 *
 * `resetDatabase` ends both at once, in a fraction of a second: it quiesces the document service
 * first, which stops every settle timer, ends the settlement each armed and waits out the ones
 * in flight (`Quiesce`, `docs/service.go`), and then empties every table, which takes with it
 * the row a deferred index is owed from - `document_block_versions.version` left null by
 * migration 0122's trigger, which is what the reconciliation reads. A tick that lands beside the
 * write which follows therefore finds nothing owed, and reads nothing else for it: every owed
 * read goes through a partial index (`ReconcileBlockIndex`). Waiting those two ticks out instead
 * was 41 s of the 50 s this row took in Chromium on this pod, one wait of 11 s and one of 30 s,
 * and neither bought the row anything the emptied database does not (sjawhar/legion#2612).
 *
 * Each write therefore also writes into an issue of its own, holding no document - as the first
 * write always did - so what the bounds below compare is twelve writes made under one state of
 * the server rather than one that grows a document per write.
 */
async function anIdleServer(): Promise<{ key: string }> {
  await resetDatabase();
  return await seedIssue("Heavy documents");
}

/**
 * What one write cost: the round trip this process timed, and what the API answered where it
 * refused the body.
 *
 * A round trip timed from here counts two things, and only the first is the claim this row
 * makes: the work the server did on the write, and the wall time in which the machine ran
 * something else instead. One Playwright worker left the second near nothing. Four do not:
 * every lane holds a browser, a Dispatch server and a Postgres of its own, so a server under
 * four lanes shares CI's four cores with three other lanes, and this pod's two cores with them
 * twice over. In CI run 125366889641 the 8,000-item write, whose own work is 630-700 ms,
 * answered in 1,471 ms and red this row; four lanes on this pod have read 2,779 ms for that
 * same write (sjawhar/legion#2612).
 *
 * So the row no longer runs beside three other lanes: it runs in `chromium-alone` (the file's
 * head), which Playwright starts only once every other project has finished and which runs one
 * row at a time. The machine is the server's, the round trip is its work, and the bound holds
 * the number a writer waits, with nothing read off the machine and nothing corrected for. Two
 * instruments that corrected for the lanes instead stood here - a hog asking for a core while
 * the write was in flight, and the machine's own books beside it - and what they read was the
 * kinder share: a server keeping several threads runnable is given more of a loaded machine
 * than the one thread the hog kept, so the pace came out too low and the paced write too fast,
 * which is a 1.4x regression passing. They are gone with the contention they corrected for.
 */
interface Written {
  /** What the write cost a writer: the round trip from this process. */
  readonly took: number;
  /** The error the API answered with, where it refused the body. */
  readonly refusal: unknown;
}

/**
 * One write at a time, with the server otherwise idle: each write starts from an emptied
 * database and an issue of its own (`anIdleServer` above), so neither the settlement nor the
 * deferred block index the write before it left can run beside the one being timed - the
 * warming write and the half-size write included - and what the bounds below hold is one write
 * alone. The twenty runs quoted further down were measured while the row waited that work out
 * instead, and a block-index tick could still land on a write there.
 *
 * So what a write costs while another document of the same server settles beside it is not
 * measured here: measured with these writes back to back it reached 1,322 ms, which
 * sjawhar/legion#2519 names under Not proven / risk.
 *
 * Two bounds hold each body, and the row asserts both of them. The absolute bound is that the
 * write answers in under 1,000 ms: it is asserted on every row, stored or refused, under no
 * condition, and it is never skipped, widened or stood down for a slow node, because a second is
 * what the writer waits and CI's runners are where that is judged. What it is held against is
 * the round trip itself, which is what a writer waits: the row runs with the machine to itself
 * (`Written` above), so no share of it is read and nothing is corrected for, and the round trip
 * is annotated beside the bound either way. The second bound is linearity: for each shape whose
 * write is its parse, the row writes that same shape at half its count, times that write exactly
 * as it times the body, and holds the body's write to no more than 3.5x it, so a parse whose cost
 * grows faster than the body it reads reds this row on any node, the fast ones CI runs on
 * included, where the body alone would still answer in time. The ratio stands on a machine that
 * is not idle, which is what it was for: they are two writes of one shape seconds apart on one
 * machine, so a machine that holds both back by the same share leaves their ratio where it was -
 * 2.16x for the 8,000 items at four lanes on this pod, in the run whose 2,779 ms red the absolute
 * bound back when this row ran beside the other lanes. Both numbers and the ratio between them
 * are annotated before either bound is asserted, so a run that reds reports what the body cost
 * beside its half, whichever of the two bounds it
 * was. The absolute bound is asserted first, because a write the node itself stalled - 1,986 ms
 * for a body that costs 750 ms here - is no measurement of how the cost grows, and reading
 * linearity out of one would name a regression that is not there.
 *
 * The body is written before its half on each of the three rows held to the ratio. On the two
 * that carry no `warm` - the 8,000 items and the bracket run - that body is the first document
 * write of its shape in the server process, which is what the absolute bound has always measured
 * over them, with nothing but an emptied database before it. It is not the first parse of those
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
 * run and the 49 markers, whose bodies cost 630-1,000 ms of this server's own work here. The two
 * lazy rows carry no half body, because their whole write is 88-274 ms, nearly all of it the
 * round trip and the document's own row, so a ratio over them measures jitter - 1.05x, 1.65x,
 * 1.73x and 2.64x for one and the same shape, measured while they carried one - and nothing of
 * a parse. A refused body is further from the claim still: two of the three are turned away
 * before any parse of them finishes (20-41 ms for the 15,999 markers, 6-18 ms for the callout,
 * on a pod that is not stalling), and the third, 100 items nested by indentation, is parsed
 * before its nesting is refused (406-1,746 ms here) but is refused at every size, so no reader
 * ever stores it. Every row keeps the absolute bound all the same.
 */
// SLOW(>=2s): twelve real writes, eleven of them held to a second of their own and the warming
// write to none, cost 5.2 s together on this pod - 0.6 to 1.2 s for each of the heaviest four.
// What the row proves is what one such write costs a writer, so no write can come out of it.
// What could is gone: it used to wait out two ticks of the block-index reconciliation, 41 s of
// the 50 s it took here (`anIdleServer`).
test("a document write of each heavy body answers in under a second, stored or refused", async () => {
  // Twelve writes - one of every body, the warming write, and a half-size one after each of the
  // three bodies held to the ratio - are about 5 s of measured writing, and the emptied database
  // and fresh issue each of them starts from are about 100 ms more apiece: 6.6 to 7.6 s in
  // Chromium on this pod with the machine to itself, which is what this row now always has
  // (`Written` above). The deadline is the room a node slower than this one needs; nothing the
  // row waits for is a tick of the server's own any more.
  test.setTimeout(90_000);

  /** Writes one body as a document of its own, into a fresh issue on a server left with no
   *  settlement and no deferred block index of an earlier write, and answers what the write cost
   *  and what the API refused (`Written` above). */
  async function write(body: string, name: string): Promise<Written> {
    const issue = await anIdleServer();
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
    // Every measurement is the run's evidence, kept on the result even when the row passes: what
    // the write cost a writer and the growth the ratio holds, so a reader of a CI run sees the
    // shape of the cost and not only that the row passed.
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
