// What a heavy body costs the server when it is written as a document: the API answers every one
// of these shapes in under a second, stored or refused, and the cost grows with the body rather
// than faster than it. The same shapes drawn in a browser are `heavy-bodies.e2e.ts`, and both
// specs read them from one module, so the two measure the very same bytes. This one drives no
// browser, so it runs in one Playwright project and nowhere else: every project of a run shares
// the one server process, so a second project would time these writes against a server that
// already holds what each shape touches, and what the row says below about the first document
// write of its shape would be false there (sjawhar/legion#2519).
import { cpus } from "node:os";
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

/** One turn of the hog below: 20,000 integer steps, about a third of a millisecond, allocating
 *  nothing. Short enough that the answer to a write is never kept waiting by a turn in progress,
 *  long enough that the event loop around it costs nothing beside it. */
const hogSteps = 20_000;

/** One turn of the hog: arithmetic no runtime can elide and no allocation, so what it takes from
 *  the machine is processor time and nothing else. */
function burnATurn(): void {
  let step = 1;
  for (let left = hogSteps; left > 0; left -= 1) {
    step = (step * 48_271) % 2_147_483_647;
  }
  // A Lehmer generator seeded at 1 never reaches 0, and reading what it reached is what keeps a
  // runtime from finding that the loop has no effect and dropping it.
  if (step === 0) {
    throw new Error("the hog's own arithmetic went wrong");
  }
}

/** How long the hog asks for a core at a stretch, and how long it leaves the machine alone
 *  between stretches. A stretch is long enough for the machine's books, which move in steps of
 *  10 to 20 ms, to say something about it; the rest between them is what keeps the instrument
 *  from charging the write a quarter of a core for being measured (`Written` below). */
const hogSliceMs = 80;
const hogRestMs = 240;

/** What the hog was given, and what the machine handed out in all, over the stretches the hog
 *  asked for a core - and how many cores it has to hand out. */
interface Hogged {
  readonly busy: number;
  readonly cores: number;
  readonly ours: number;
}

/** Back to the event loop and straight on again: the answer to a write in flight is read as it
 *  lands, and the process is never asleep when the scheduler looks at it. */
function yieldToTheLoop(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** Asleep for `ms`, which is the hog's rest: here the write has the machine as it would with no
 *  instrument on it at all. */
function rest(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

/** Asks for a core in stretches for as long as `inFlight` says the write is, and answers the
 *  books for the stretches alone: what this process was given over them and what the machine
 *  handed out over the same stretches. A write the hog never got a whole stretch of answers
 *  nothing the books can speak for, which `write` reads as no share (`Written` below). */
async function hogACore(inFlight: () => boolean): Promise<Hogged> {
  let busy = 0;
  let cores = 0;
  let ours = 0;
  while (inFlight()) {
    const spentBefore = process.cpuUsage();
    const handedOutBefore = handedOut();
    const until = performance.now() + hogSliceMs;
    while (inFlight() && performance.now() < until) {
      burnATurn();
      await yieldToTheLoop();
    }
    const spent = process.cpuUsage(spentBefore);
    const machine = handedOut();
    busy += machine.busy - handedOutBefore.busy;
    cores = machine.cores;
    ours += (spent.user + spent.system) / 1_000;
    if (!inFlight()) {
      break;
    }
    await rest(hogRestMs);
  }
  return { busy, cores, ours };
}

/** The processor time this machine has handed out since it started, over every core it has, and
 *  how many cores that is. Idle time is not handed out; everything else is, whoever got it. */
function handedOut(): { busy: number; cores: number } {
  const cores = cpus();
  let busy = 0;
  for (const core of cores) {
    const { idle, ...running } = core.times;
    busy += Object.values(running).reduce((all, spent) => all + spent, 0);
  }
  return { busy, cores: cores.length };
}

/**
 * What one write cost, told apart from the wall time the machine spent on the suite's other
 * lanes while it ran.
 *
 * A round trip timed from here counts two things, and only the first is the claim this row
 * makes: the work the server did on the write, and the wall time in which the machine ran
 * something else instead. One Playwright worker left the second near nothing. Four do not: every
 * lane holds a browser, a Dispatch server and a Postgres of its own, so the server under this
 * row shares CI's four cores with three other lanes, and this pod's two cores with them twice
 * over. In CI run 125366889641 the 8,000-item write, whose own work is 630-700 ms, answered in
 * 1,471 ms and red this row; four lanes on this pod have read 2,779 ms for that same write
 * (sjawhar/legion#2612).
 *
 * So the round trip is paced into the milliseconds of a machine that would have given the write
 * a core, as the browser rows pace a main-thread hold into the milliseconds of a thread running
 * at its own best (`main-thread-hold.ts`). The instrument is a hog and the machine's own books.
 * While the write is in flight this process asks for a core in stretches of `hogSliceMs`, and
 * two numbers are read over those stretches: the processor time the machine gave this process
 * (`process.cpuUsage`) and the processor time it handed out in all (`os.cpus`). A machine the
 * suite's lanes have filled hands out every core it has, so the hog's share of all that time,
 * times the cores, is the share of one core a thread that wanted one was given - and that is
 * the share the goroutine parsing the body next door was given beside it. A write that took
 * 1,400 ms while the machine gave such a thread 45% of a core is 630 ms of work, and 630 ms is
 * what the absolute bound then holds.
 *
 * An instrument must not charge its subject for being measured, and this one is a thread that
 * wants a whole core beside a server that wants one too. It gives the machine back
 * `hogRestMs` between stretches, so three quarters of a write runs with no hog on the machine
 * at all, and the share it read over the stretches is then taken as the share over the write -
 * the suite's other lanes hold their load for whole tests, not for 80 ms at a time. And the
 * hog's own demand comes out of the figure (`shareOfACore`): what it read is the share with
 * itself on the machine, and what the write met is the share without it. With the machine to
 * itself that correction is what lifts a stretch's 86-96% to the whole core the write really
 * had, so the bound there holds the round trip as it always did.
 *
 * The share is read, never calibrated: no node is stood down for being slow, and a slow node's
 * own second is still a second. The two numbers are read together rather than the first alone
 * because a kernel's own accounting of processor time can undercount under load, and this
 * pod's does: a job of 505 ms of core time reads as 230 ms there while eight other threads
 * want the machine, and the machine, saturated, reports 1.43 of its 2 cores handed out. An
 * undercount that is in both numbers divides out of their ratio.
 *
 * The hog must want a whole core while it runs and must never sleep inside a stretch, because a
 * thread that sleeps is handed a core the moment it wakes however loaded the machine is: a
 * sampler that ran 3.2 ms every 50 ms read 57% of a machine that was giving this row's own
 * writes a third of it. It yields to the event loop between turns rather than sleeping, so the
 * answer to the write is read as it lands and the scheduler still sees a thread that wants to
 * run.
 *
 * What the share cannot see is that the server is a process of many threads where the hog is
 * one: a parse is one goroutine, but the Go runtime collects beside it, and a process keeping
 * two threads runnable is given more of a loaded machine than a thread keeping one. So the
 * share read here is the smaller one, and a bound held against it is the looser for that under
 * lanes, never the tighter: contention cannot push this figure up.
 *
 * A write the machine's books cannot speak for is paced by nothing at all. This pod's kernel
 * accounts processor time in steps of 10 to 20 ms - the three refusals here are answered in 4
 * to 27 ms with the machine to itself, and the shortest of them read as no processor time
 * whatever - so a write under `shareNeedsMs`, and one the books give no processor time at all,
 * is left unpaced and the bound holds its round trip, which for a write that short is the same
 * number and never the kinder one.
 */
interface Written {
  /** What the write cost a writer: wall time from this process, which holds whatever the machine
   *  did to the server while it ran. */
  readonly took: number;
  /** `took` in the milliseconds of a machine that would have given the write a core, which is
   *  what the absolute bound holds. */
  readonly paced: number;
  /** The error the API answered with, where it refused the body. */
  readonly refusal: unknown;
  /** The share of a core the machine gave a thread that wanted one while the write was in
   *  flight: 1 with nothing else on it, a third of that where the suite's other lanes took the
   *  cores. Unread, and `undefined`, where the write was answered in less than `shareNeedsMs`
   *  or the machine's books gave this process no processor time to read a share from. */
  readonly speed: number | undefined;
}

/** The least a write may take for a share to be read from it, which is what this machine's own
 *  accounting of processor time needs to say anything: in steps of 10 to 20 ms, a tenth of this
 *  is the most a share read here is wrong by, and the writes the bound is about are 600 ms and
 *  more. */
const shareNeedsMs = 100;

/**
 * The share of a core the machine was giving a thread that wanted one while a write of `took`
 * was in flight, or `undefined` where its books say nothing a share can be read from.
 *
 * What the hog read is the share with the hog itself on the machine, and the write the row is
 * timing ran beside it. So the hog's own demand comes back out: a machine of `cores` giving a
 * thread that wants one `read` of it was sharing them between `cores / read` such threads, one
 * of which was the hog, and without it the rest would have had `cores / (cores / read - 1)`
 * each. On a machine with a core to spare that is more than one, and a whole core is all a
 * thread can be given, so it caps at 1 and the bound then holds the round trip itself.
 */
function shareOfACore(took: number, { busy, cores, ours }: Hogged): number | undefined {
  const read = Math.min(1, (ours * cores) / busy);
  if (took < shareNeedsMs || !(read > 0)) {
    return undefined;
  }
  const others = cores / read - 1;
  return others <= 0 ? 1 : Math.min(1, cores / others);
}

/** What a write's cost reads as on a result and in a refusal: the round trip, and the share of
 *  a core it was paced by where one was read. */
function howItRan({ speed, took }: Written): string {
  return speed === undefined
    ? `${Math.round(took)} ms, with no share of a core to be read from the machine's books`
    : `${Math.round(took)} ms with the machine giving a thread that wanted a core ` +
        `${Math.round(speed * 100)}% of one`;
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
 * the round trip paced into the milliseconds of a machine that would have given the write a
 * core (`Written` above), since four lanes of this suite put four servers on one machine and the
 * wall time of a write there holds what the other three lanes took as surely as the work this
 * one did. A machine with nothing else on it gives a whole core, the share is 1, and the bound
 * is held against the round trip itself; the wall time and the share are annotated beside the
 * paced figure either way. The second bound is linearity: for each shape whose write is its
 * parse, the row writes that same shape at half its count, times that write exactly as it times
 * the body, and holds the body's write to no more than 3.5x it, so a parse whose cost grows
 * faster than the body it reads reds this row on any node, the fast ones CI runs on included,
 * where the body alone would still answer in time. The ratio is read from the two wall times
 * and no pace: they are two writes of one shape seconds apart on one machine, so a machine that
 * holds both back by the same share leaves their ratio where it was - 2.16x for the 8,000 items
 * at four lanes on this pod, in the run whose 2,779 ms red the absolute bound before it was
 * paced. Both numbers and the ratio between them are annotated before either bound is asserted,
 * so a run that reds reports what the body cost beside its half, whichever of the two bounds it
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
  // Chromium on this pod with the machine to itself, and 13.6 to 19.6 s of it at four lanes,
  // where every write waits on three other lanes as well. The deadline is the room a node
  // slower than this one needs; nothing the row waits for is a tick of the server's own any
  // more.
  test.setTimeout(90_000);

  /** Writes one body as a document of its own, into a fresh issue on a server left with no
   *  settlement and no deferred block index of an earlier write, and answers what the write cost,
   *  what the machine it ran on was running at, and what the API refused (`Written` above). */
  async function write(body: string, name: string): Promise<Written> {
    const issue = await anIdleServer();
    const started = performance.now();
    const answered = createIssueArtifact(issue.key, { content: body, name }, agent).then(
      () => undefined,
      (error: unknown) => error
    );
    // The hog asks for a core in stretches for as long as the write is in flight, and what this
    // process was given of what the machine handed out over those stretches is the share the
    // machine was giving a thread that wanted one (`Written` above).
    let inFlight = true;
    const hog = hogACore(() => inFlight);
    const refusal = await answered;
    const took = performance.now() - started;
    inFlight = false;
    const speed = shareOfACore(took, await hog);
    return { paced: took * (speed ?? 1), refusal, speed, took };
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
    const written = await write(row.body, `heavy-${index}.md`);
    const { paced, refusal, took } = written;
    let half: number | undefined;
    if (row.half !== undefined) {
      // This row's own shape at half its count, written and timed as the body above it was, and
      // waited out the same way: the two together are the growth the ratio holds.
      const smaller = await write(row.half, `heavy-${index}-half.md`);
      expect(smaller.refusal, `${row.name}: the half-size body was refused`).toBeUndefined();
      half = smaller.took;
    }
    // Every measurement is the run's evidence, kept on the result even when the row passes: what
    // the write cost a writer, what it cost on a machine that would have given it a core, the
    // share of one the machine was giving, and the growth the ratio holds - so a reader of a CI
    // run sees the shape of the cost and not only that the row passed.
    test.info().annotations.push(
      {
        description:
          half === undefined
            ? `${Math.round(took)} ms`
            : `${Math.round(half)} ms at half this shape, ${Math.round(took)} ms whole, ` +
              `${(took / half).toFixed(2)}x`,
        type: `${row.name}: what the write cost`,
      },
      {
        description: `${Math.round(paced)} ms, from ${howItRan(written)}`,
        type: `${row.name}: what the write cost the server`,
      }
    );

    if (row.stored) {
      expect(refusal, `${row.name}: the write was refused`).toBeUndefined();
    } else {
      expect(refusal, `${row.name}: the write was accepted`).toBeInstanceOf(Error);
      const message = refusal instanceof Error ? refusal.message : "";
      expect(message, row.name).toContain(" failed: 400 ");
      expect(message, row.name).toContain('"code":"INVALID_MARKDOWN"');
    }
    expect(
      paced,
      `${row.name}: the write took ${Math.round(paced)} ms (${howItRan(written)})`
    ).toBeLessThan(1_000);
    if (half !== undefined) {
      expect(
        took,
        `${row.name}: ${Math.round(took)} ms for the body and ${Math.round(half)} ms for half ` +
          `of it, ${(took / half).toFixed(2)}x`
      ).toBeLessThanOrEqual(half * 3.5);
    }
  }
});
