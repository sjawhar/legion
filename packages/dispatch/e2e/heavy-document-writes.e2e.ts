// What a heavy body costs the server when it is written as a document: the API answers every one
// of these shapes in under a second, stored or refused. That the cost grows with the body rather
// than faster than it is held in Go over these same shapes, in CPU time
// (`packages/envoy/internal/dispatch/pmdoc/heavy_bodies_test.go`). The same shapes drawn in a
// browser are `heavy-bodies.e2e.ts`, and both specs read them from one module, so the two measure
// the very same bytes. This one drives no browser, so it runs in one Playwright project and
// nowhere else: every project of a run shares the one server process, so a second project would
// time these writes against a server that already holds what each shape touches, and what the row
// says below about the first document write of its shape would be false there
// (sjawhar/legion#2519). That project is `chromium-alone` (e2e/playwright.config.ts), which
// Playwright starts only once every other project has finished and which runs one row at a time:
// a second read from the wall clock is a second only where nothing else holds the machine
// (sjawhar/legion#2612).
import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { expect, test } from "@playwright/test";
import { createIssue, createIssueArtifact, createProject } from "./api";
import {
  bracketRun,
  calloutBracketRun,
  lazyListItem,
  lazyQuote,
  markerChain,
  nested49ThenBlankLines,
  nestedThenBlankLines,
  plainList,
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
  /** Whether the API stores a write of this body, or refuses it as invalid markdown. */
  readonly stored: boolean;
  /** The one shape whose first write in a server process pays for the memory it touches rather
   *  than for the parse, which the row writes once before it measures anything (the table's own
   *  comment below). */
  readonly warm?: true;
}

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
const documentRows: readonly HeavyDocument[] = [
  { body: plainList, name: "8,000 list items", stored: true },
  { body: lazyQuote, name: "a quote followed by lines without its marker", stored: true },
  { body: lazyListItem, name: "a list item followed by lines without its marker", stored: true },
  { body: bracketRun, name: "15,000 opening brackets then 15,000 closing ones", stored: true },
  {
    body: nested49ThenBlankLines,
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
 * warming write included - and what the bound below holds is one write alone.
 *
 * So what a write costs while another document of the same server settles beside it is not
 * measured here: measured with these writes back to back it reached 1,322 ms, which
 * sjawhar/legion#2519 names under Not proven / risk.
 *
 * One bound holds each body: the write answers in under 1,000 ms. It is asserted on every row,
 * stored or refused, under no condition, and it is never skipped, widened or stood down for a
 * slow node, because a second is what the writer waits and CI's runners are where that is
 * judged. What it is held against is the round trip itself, which is what a writer waits: the
 * row runs with the machine to itself (`Written` above), so no share of it is read and nothing
 * is corrected for. The round trip is annotated before the bound is asserted, so a run that
 * reds reports the number it read.
 *
 * That the cost grows with the body rather than faster than it is held in Go instead, over
 * these same shapes at an eightfold step and in the CPU time the work itself spends
 * (`TestHeavyBodiesAtTheBodyCapParseAndEncodeInTimeProportionalToTheirLength`,
 * `packages/envoy/internal/dispatch/pmdoc/heavy_bodies_test.go`). This row used to write three
 * of the shapes at half their count as well and hold the body's write to 3.5x that half. That
 * bound could not separate the two things it was asked to tell apart, whatever the machine was
 * doing: over eight `dispatch` CI jobs the server's own `elapsed_ms` for these writes read
 * 1.90x to 2.36x on the honest runs and 3.57x on the one that red, while a parse gone quadratic
 * reads about 3.5x to 4.0x at a 2x step (`growthStep`,
 * `packages/envoy/internal/dispatch/pmdoc/linear_test.go`, has that arithmetic). Giving the row
 * the machine to itself (sjawhar/legion#2612) did not close that gap: it red again at 5.44x in
 * run 41921860163, out of the merge queue, taking a whole batch with it. Two wall times a
 * doubling apart cannot separate a regression from a runner's own noise; eight times the input,
 * measured in CPU time, can (INFRA-985).
 *
 * On every row but the 49-marker one the write the bound reads is the first document write of
 * its shape in the server process, which is what the bound has always measured over them, with
 * nothing but an emptied database before it. On two of them it is not the first parse of those
 * bytes: in the one project this spec runs in, `heavy-bodies.e2e.ts` runs first on the same
 * server (one worker, files in name order) and creates comments holding the 8,000 items and the
 * bracket run, whose create parses each of them once. The 49-marker row is the exception:
 * `warm` has the row write that shape once before the write the bound reads, so what the bound
 * reads there is the second write of its shape - the steady state every later writer of such a
 * body meets, with the page faults of a first parse out of it - and the first write's own time
 * is annotated beside it and held to no bound (the table above).
 */
// SLOW(>=2s): nine real writes, eight of them held to a second of their own and the warming
// write to none, on this pod - 0.6 to 1.2 s for each of the heaviest four. What the row proves
// is what one such write costs a writer, so no write can come out of it. What could is gone: it
// used to wait out two ticks of the block-index reconciliation, 41 s of the 50 s it took here
// (`anIdleServer`), and to write three of the shapes a second time at half their count.
test("a document write of each heavy body answers in under a second, stored or refused", async () => {
  // Nine writes - one of every body and the warming write - are about 4 s of measured writing,
  // and the emptied database and fresh issue each of them starts from are about 100 ms more
  // apiece, in Chromium on this pod with the machine to itself, which is what this row always
  // has (`Written` above). The deadline is the room a node slower than this one needs; nothing
  // the row waits for is a tick of the server's own.
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
    // The measurement is the run's evidence, kept on the result even when the row passes: what
    // the write cost a writer, so a reader of a CI run sees the number and not only that the
    // row passed.
    test.info().annotations.push({
      description: `${Math.round(took)} ms`,
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
  }
});
