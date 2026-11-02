// How long the page's own work holds the main thread while a card draws, told apart from the wall
// time the machine took from the renderer around it. Two specs assert a hold through this module -
// `heavy-bodies.e2e.ts`, whose rows draw the bodies of `heavy-bodies.ts` in the Conversation, the
// Inbox's ask card and a hover card, and `nested-directive.e2e.ts`, whose rows draw the two bodies
// that froze a reader's tab - so both read a hold the same way and against the same bound
// (sjawhar/legion#2612).
import { expect, type Page, test } from "@playwright/test";

declare global {
  interface Window {
    /** The largest gap, in milliseconds, between two ticks of the page's main-thread heartbeat. */
    __mainThreadLongestGap?: number;
    /** What the witness thread and the yardstick have to say about the record so far: the
     *  longest gap the witness counted while it was itself running, the yardstick's own best
     *  time on this page and what it has cost over the record (`runs` of them, `cost` in all). */
    __mainThreadWitnessed?: () => Promise<{
      best: number;
      cost: number;
      runs: number;
      witnessed: number;
    }>;
    /** `performance.now()` when the record last started, so a row can time the draw from it. */
    __mainThreadResetAt?: number;
    /** Starts the record again: no gap so far, the last tick now, nothing witnessed, and what
     *  the yardstick has cost so far forgotten - its best is the thread's own and stands. */
    __mainThreadResetGap?: () => void;
  }
}

/** The longest the page's own work may hold the main thread while the card draws, counted in
 *  the milliseconds of a main thread running at its own best (`measureDraw`). */
const longestHoldMs = 1_000;

export interface Measured {
  /** Milliseconds from the record's start until this row saw the formatted body; the row's own
   *  polling rounds it up. */
  readonly drawn: number;
  /** The largest gap, in milliseconds, between two ticks of the main-thread heartbeat since the
   *  record started: the page's own work and everything the machine did to the renderer around
   *  it. */
  readonly gap: number;
  /** The longest the page's own work held the main thread, in the milliseconds of a main thread
   *  running at its own best: the longest gap the witness thread attests to, paced by `speed`.
   *  This is what a row asserts. */
  readonly held: number;
  /** What the main thread ran at over the record, as a share of its own best: 1 on a machine
   *  with nothing else on it, a third of that where the suite's other lanes took the cores. */
  readonly speed: number;
}

/**
 * Installs a heartbeat before the page's own scripts - a `setTimeout(0)` loop whose gap between
 * ticks a parse holding the main thread widens to the parse's length - and the two instruments
 * that tell the page's own work from the machine's. The record starts again when the page first
 * asks for the data the body comes in, before that request goes out: the bundle has been
 * evaluated and the tab drawn by then, and the body cannot draw before the answer.
 *
 * A gap on its own counts three things, and only the first is what these rows are about: the
 * page's own work on its own thread; wall time in which the operating system did not schedule
 * this renderer at all; and wall time in which it did schedule it, slower than this machine can
 * run it, because the suite's other lanes wanted the same cores. One browser at a time left the
 * last two near nothing. A Playwright worker per core does not: in CI run 41370867411 the row
 * for `49 list markers on one line ...`, whose whole draw is about 300 ms of work, reported a
 * gap of 1,632 ms (sjawhar/legion#2612). So the gap is read through two instruments a renderer
 * that was not running cannot forge:
 *
 * - A witness thread: a Worker of the page's own, and so a thread of the renderer's own
 *   process, which ticks every `periodMs` and counts a gap only in the time it was itself
 *   running, since each of its ticks attests its own period and no more. Wall time the machine
 *   took from the whole process - this thread with it - is attested by nobody and counts as
 *   nothing.
 * - A yardstick: `jobSteps` of integer arithmetic, about 3.3 ms on an idle core of an issue
 *   pod, timed on the main thread itself every `yardstickEveryMs`. The least it has ever cost
 *   this page is that thread's own best, and what it cost on average while the body drew is
 *   what the thread was running at. A hold is then counted in that thread's best milliseconds:
 *   900 ms of witnessed gap on a thread running at a third of its best is 300 ms of the page's
 *   own work.
 *
 * What the pacing cannot see is the contention inside the hold itself: the yardstick's job is
 * 3.3 ms long and a hold is hundreds of milliseconds, and a short job escapes preemption that a
 * long run of work cannot, so the pace it reports is the kinder one and the hold it leaves is
 * too high, never too low. Measured over the twelve measuring rows of `heavy-bodies.e2e.ts` in
 * an issue pod of two cores, which four lanes crowd twice as hard as they crowd CI's four: at
 * one lane the longest hold was 400 ms, and over three four-lane runs of that whole file, where
 * gaps reached 1,877 ms, it was 859 ms - against the 1,000 ms bound. With `MarkdownBody`'s
 * sliced painter taken out, so that the 16,000 elements of an 8,000-item list are built and laid
 * out in one task, that file's three rows that draw one read 592, 868 and 1,028 ms at one lane
 * against the 267, 279 and 328 ms they read with it: the heaviest crosses the bound, and all
 * three are read in the thread's own milliseconds rather than the machine's.
 */
export async function measureDraw(
  page: Page,
  {
    drawn,
    open,
    reset,
  }: {
    /** Resolves once the formatted body is in the page. */
    readonly drawn: () => Promise<void>;
    /** What the reader does: open the page, and whatever else reaches the body. */
    readonly open: () => Promise<void>;
    /** The first GET of this request starts the record. */
    readonly reset: RegExp;
  }
): Promise<Measured> {
  await page.addInitScript(() => {
    // The witness is built from its own source because a page that has loaded nothing yet has
    // no script of its own to make a worker from.
    const witnessBody = () => {
      // Long enough that the slop between asking for a tick and getting one is a small part of
      // it: a tick attests its own period and no more, so a period near the slop would hand
      // back a fraction of every gap the main thread really was held through.
      const periodMs = 20;
      let silent = 0;
      let witnessed = 0;
      let last = performance.now();
      self.addEventListener("message", (event) => {
        const said = (event as MessageEvent).data as string;
        if (said === "report") {
          (self as unknown as Worker).postMessage(witnessed);
        } else if (said === "reset") {
          witnessed = 0;
        }
        // Any word from the main thread is the main thread running a task: the run ends here.
        silent = 0;
      });
      const tick = () => {
        const now = performance.now();
        // A tick attests its own period and no more: what this thread lost to the machine is
        // time nobody watched the main thread in.
        silent += Math.min(now - last, periodMs);
        last = now;
        witnessed = Math.max(witnessed, silent);
        setTimeout(tick, periodMs);
      };
      tick();
    };
    const witness = new Worker(
      URL.createObjectURL(new Blob([`(${witnessBody.toString()})()`], { type: "text/javascript" }))
    );

    // Enough arithmetic to outlast the coarsest clock a page is given - Firefox rounds
    // `performance.now()` to the millisecond - run often enough to say what the thread was
    // running at around a hold and seldom enough to leave the thread to the page.
    const jobSteps = 200_000;
    const yardstickEveryMs = 100;
    let best = Number.POSITIVE_INFINITY;
    let cost = 0;
    let runs = 0;
    let sampledAt = Number.NEGATIVE_INFINITY;
    const yardstick = (now: number) => {
      if (now - sampledAt < yardstickEveryMs) {
        return;
      }
      const started = performance.now();
      let step = 1;
      for (let left = jobSteps; left > 0; left -= 1) {
        step = (step * 48_271) % 2_147_483_647;
      }
      const took = performance.now() - started;
      // A Lehmer generator seeded at 1 never reaches 0, and reading what it reached is what
      // keeps an engine from finding that the loop has no effect and dropping it.
      if (step === 0) {
        throw new Error("the yardstick's own arithmetic went wrong");
      }
      sampledAt = performance.now();
      best = Math.min(best, took);
      cost += took;
      runs += 1;
    };

    let last = performance.now();
    window.__mainThreadLongestGap = 0;
    window.__mainThreadResetAt = last;
    window.__mainThreadResetGap = () => {
      last = performance.now();
      window.__mainThreadLongestGap = 0;
      window.__mainThreadResetAt = last;
      // What the thread can do is the thread's own and stands; what it did is the record's.
      cost = 0;
      runs = 0;
      witness.postMessage("reset");
    };
    window.__mainThreadWitnessed = () =>
      new Promise((resolve) => {
        const answer = (event: MessageEvent) => {
          witness.removeEventListener("message", answer);
          resolve({ best, cost, runs, witnessed: event.data as number });
        };
        witness.addEventListener("message", answer);
        witness.postMessage("report");
      });
    const tick = () => {
      const now = performance.now();
      window.__mainThreadLongestGap = Math.max(window.__mainThreadLongestGap ?? 0, now - last);
      last = now;
      yardstick(now);
      witness.postMessage("alive");
      setTimeout(tick, 0);
    };
    setTimeout(tick, 0);
  });
  // Only the first request resets: a later one could wait out the draw, then erase its gap.
  let started = false;
  await page.route(reset, async (route) => {
    if (!started && route.request().method() === "GET") {
      started = true;
      await page.evaluate(() => window.__mainThreadResetGap?.());
    }
    await route.continue();
  });

  await open();
  await drawn();
  if (!started) {
    throw new Error(`the page never asked for ${reset.source}`);
  }
  const measured = await page.evaluate(async () => ({
    drawn: performance.now() - (window.__mainThreadResetAt ?? Number.NaN),
    gap: window.__mainThreadLongestGap,
    seen: await window.__mainThreadWitnessed?.(),
  }));
  const seen = measured.seen;
  if (measured.gap === undefined || seen === undefined) {
    throw new Error("the main-thread heartbeat never started");
  }
  if (seen.runs === 0 || !Number.isFinite(seen.best)) {
    throw new Error("the yardstick never ran, so nothing says what this thread was running at");
  }
  const speed = seen.best / (seen.cost / seen.runs);
  return { drawn: measured.drawn, gap: measured.gap, held: seen.witnessed * speed, speed };
}

/** Every measurement is the run's evidence, so each is kept on the result even when it passes:
 *  the hold the row asserts, the gap and the pace it was read from, and the time to the
 *  formatted body, which is recorded and never a limit. */
export function record({ drawn, gap, held, speed }: Measured): void {
  test.info().annotations.push(
    { description: `${Math.round(held)} ms`, type: "longest main-thread hold" },
    {
      description: `${Math.round(gap)} ms, on a main thread running at ${Math.round(speed * 100)}% of its own best`,
      type: "longest main-thread gap",
    },
    {
      description: `${Math.round(drawn)} ms, as this row polled for it`,
      type: "time to the formatted body",
    }
  );
}

export function expectUnderTheBound({ gap, held, speed }: Measured): void {
  expect(
    held,
    `the page's own work held the main thread for ${Math.round(held)} ms while the card drew ` +
      `(a gap of ${Math.round(gap)} ms, witnessed by a thread of the same process and paced by ` +
      `a main thread running at ${Math.round(speed * 100)}% of its own best)`
  ).toBeLessThan(longestHoldMs);
}
