// What a heavy body costs a reader's own thread, in the surfaces that draw one: the Conversation,
// the Inbox's ask card and a hover card. The same shapes written as documents through the API are
// `heavy-document-writes.e2e.ts`, and both specs read the bodies from `heavy-bodies.ts`, so the
// two measure the very same bytes (sjawhar/legion#2519).
import { type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

// `markdownWorkerThreshold` comes from the module that carries it and imports nothing: the
// engine around it (`markdown-engine.ts`) is a browser module this spec's process cannot load.
// The two shortest rows below sit one character under it, so they prove the body the page parses
// on its own thread stays inside the same bound as the ones the worker takes.
import { markdownWorkerThreshold } from "../web/src/features/refs/markdown-worker-threshold";
import { type FakeSession, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  publishAgentStreamFrame,
} from "./api";
import {
  bracketRun,
  calloutBracketRun,
  lazyQuote,
  markerChain,
  nestedThenBlankLines,
  nestedThenTextLines,
  padTo,
  plainList,
} from "./heavy-bodies";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

declare global {
  interface Window {
    /** The largest gap, in milliseconds, between two ticks of the page's main-thread heartbeat. */
    __heavyBodiesLongestGap?: number;
    /** `performance.now()` when the record last started, so a row can time the draw from it. */
    __heavyBodiesResetAt?: number;
    /** Starts the heartbeat's record again: no gap so far, and the last tick now. */
    __heavyBodiesResetGap?: () => void;
    /** How far the reader's own scroll moved the page, once `scrollOnTheFirstBatch` has run it;
     *  0 until then. */
    __heavyBodiesReaderScrolled?: number;
    /** Every sample the streamed turn's recorder has taken, in order. */
    __heavyBodiesStreamSamples?: StreamSample[];
    /** Told by the row that the turn's last frame is about to go out, so every sample after it
     *  is recorded with the turn over. */
    __heavyBodiesTurnEnded?: () => void;
  }
}

const agent = {
  actor: { id: "e2e-heavy-bodies-agent", kind: "session" as const },
  as: "agent" as const,
};

/** The longest the main thread may go without running a task while the card draws. */
const longestGapMs = 1_000;

/**
 * The parse worker's own chunk, as Vite names a module worker's bundle - the diagram renderer
 * already ships `mermaid-render.worker-<hash>.js` from
 * `new Worker(new URL("./mermaid-render.worker.ts", import.meta.url), { type: "module" })`. The
 * three rows that hold or refuse the worker's answer address the worker by this URL, so the
 * implementation must bundle the parse worker as its own chunk named from
 * `markdown-parse.worker.ts`; nothing else here depends on how the worker is built.
 */
const workerChunk = /markdown-parse\.worker[^/]*\.js(\?.*)?$/;

/** How many items `- x\n` repeated to one character under the worker's threshold spells: the
 *  last one is cut short of its newline and is a list item all the same. Derived, so the row
 *  that counts them follows `markdownWorkerThreshold` wherever it is set. */
const subThresholdItems = Math.ceil((markdownWorkerThreshold - 1) / "- x\n".length);

/** Go's nesting bound, which the browser now counts the same way (`pmdoc/nesting.go`
 *  `maxNesting`): a list and its item each count one, so 50 levels stand and the 51st reads as
 *  the text it is. */
const deepestListLevels = 50;

/**
 * The element a surface renders a body into (`markdownClassName` in `markdown-engine.ts`), which
 * is also the element that carries `data-markdown-pending` while the worker owes its formatting
 * and `data-markdown-fallback` when the parser itself refused the body.
 */
function markdownBody(card: Locator): Locator {
  return card.locator(".dispatch-markdown").first();
}

/** The deepest chain of `selector` elements inside `element`: 1 for a flat list, 50 for a list
 *  nested 50 deep. */
function deepestNesting(element: Locator, selector: string): Promise<number> {
  return element.evaluate((root, nested) => {
    let deepest = 0;
    for (const node of root.querySelectorAll(nested)) {
      let depth = 0;
      for (let step: Element | null = node; step !== null; step = step.parentElement) {
        if (step.matches(nested)) {
          depth += 1;
        }
        if (step === root) {
          break;
        }
      }
      deepest = Math.max(deepest, depth);
    }
    return deepest;
  }, selector);
}

/** How many characters `element` holds, so a row can assert a shape read as one run of text
 *  without shipping 30,000 characters into an assertion message. */
function textLength(element: Locator): Promise<number> {
  return element.evaluate((node) => node.textContent?.length ?? 0);
}

/** The characters standing in `element` itself rather than inside a block a paint attached:
 *  a body's own literal text, which a reader is shown while the worker owes its formatting and
 *  which a paint replaces rather than stands behind. */
function looseTextLength(element: Locator): Promise<number> {
  return element.evaluate((node) => {
    let loose = 0;
    for (const child of node.childNodes) {
      if (child.nodeType === Node.TEXT_NODE) {
        loose += child.textContent?.length ?? 0;
      }
    }
    return loose;
  });
}

/** The card ends with its body formatted: nothing is still waiting on the worker, and the parser
 *  refused nothing. */
async function expectSettled(card: Locator): Promise<void> {
  await expect(card.locator("[data-markdown-pending]")).toHaveCount(0);
  await expect(card.locator("[data-markdown-fallback]")).toHaveCount(0);
}

interface Measured {
  /** Milliseconds from the record's start until this row saw the formatted body; the row's own
   *  polling rounds it up. */
  readonly drawn: number;
  /** The largest gap, in milliseconds, between two ticks of the main-thread heartbeat since the
   *  record started. */
  readonly gap: number;
}

/**
 * Installs a heartbeat before the page's own scripts: a `setTimeout(0)` loop that records the
 * largest gap between its ticks, which a parse holding the main thread widens to the parse's
 * length. The record starts again when the page first asks for the data the body comes in,
 * before that request goes out: the bundle has been evaluated and the tab drawn by then, and the
 * body cannot draw before the answer.
 */
async function measureDraw(
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
    let last = performance.now();
    window.__heavyBodiesLongestGap = 0;
    window.__heavyBodiesResetAt = last;
    window.__heavyBodiesResetGap = () => {
      last = performance.now();
      window.__heavyBodiesLongestGap = 0;
      window.__heavyBodiesResetAt = last;
    };
    const tick = () => {
      const now = performance.now();
      window.__heavyBodiesLongestGap = Math.max(window.__heavyBodiesLongestGap ?? 0, now - last);
      last = now;
      setTimeout(tick, 0);
    };
    setTimeout(tick, 0);
  });
  // Only the first request resets: a later one could wait out the draw, then erase its gap.
  let started = false;
  await page.route(reset, async (route) => {
    if (!started && route.request().method() === "GET") {
      started = true;
      await page.evaluate(() => window.__heavyBodiesResetGap?.());
    }
    await route.continue();
  });

  await open();
  await drawn();
  if (!started) {
    throw new Error(`the page never asked for ${reset.source}`);
  }
  const measured = await page.evaluate(() => ({
    drawn: performance.now() - (window.__heavyBodiesResetAt ?? Number.NaN),
    gap: window.__heavyBodiesLongestGap,
  }));
  if (measured.gap === undefined) {
    throw new Error("the main-thread heartbeat never started");
  }
  return { drawn: measured.drawn, gap: measured.gap };
}

/** Both measurements are the run's evidence, so they are kept on the result even when it passes.
 *  Only the gap is asserted; the time to the formatted body is recorded, never a limit. */
function record({ drawn, gap }: Measured): void {
  test.info().annotations.push(
    { description: `${Math.round(gap)} ms`, type: "longest main-thread gap" },
    {
      description: `${Math.round(drawn)} ms, as this row polled for it`,
      type: "time to the formatted body",
    }
  );
}

function expectUnderTheBound({ gap }: Measured): void {
  expect(
    gap,
    `the main thread ran no task for ${Math.round(gap)} ms while the card drew`
  ).toBeLessThan(longestGapMs);
}

interface HeldWorker {
  /** Lets the held chunk through, so the worker loads and answers. */
  readonly release: () => void;
  /** Whether the page has asked for the worker's chunk at all. */
  readonly requested: () => boolean;
}

/**
 * Holds the parse worker's chunk in flight, so every long body on the page stays pending until
 * the row lets it through. The route is on the context, not the page: a module worker's own
 * script is fetched for the page that created it, and the context catches it whichever way the
 * engine attributes it.
 */
async function holdTheWorkerChunk(context: BrowserContext): Promise<HeldWorker> {
  let letThrough = (): void => {};
  const held = new Promise<void>((resolve) => {
    letThrough = resolve;
  });
  let asked = false;
  await context.route(workerChunk, async (route) => {
    asked = true;
    await held;
    await route.continue();
  });
  return { release: letThrough, requested: () => asked };
}

/** The page asked for the worker's chunk, which is what holding or refusing it can act on. */
async function expectTheWorkerWasAskedFor(held: HeldWorker): Promise<void> {
  await expect
    .poll(() => held.requested(), {
      message:
        "the page never asked for a markdown-parse.worker chunk: a long body is still parsed on " +
        "the reader's own thread",
    })
    .toBe(true);
}

async function seedIssue(title: string): Promise<{ key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  return await createIssue({ project: "CORE", title });
}

test.beforeEach(async () => {
  await resetDatabase();
});

interface Row {
  /** The body an agent posts. */
  readonly body: string;
  /** Waits until the card holds the body formatted, which is where the measurement ends. */
  readonly formatted: (body: Locator) => Promise<void>;
  /** What the row is called, in its own title. */
  readonly name: string;
}

const rows: readonly Row[] = [
  {
    body: plainList,
    formatted: async (body) => {
      await expect(body.locator("li")).toHaveCount(8_000);
    },
    name: "8,000 list items",
  },
  {
    body: nestedThenBlankLines,
    formatted: async (body) => {
      await expect(body.locator("li")).toHaveCount(deepestListLevels);
      await expect.poll(() => deepestNesting(body, "ul")).toBe(deepestListLevels);
    },
    name: "100 list items nested by indentation then blank lines",
  },
  {
    body: nestedThenTextLines,
    formatted: async (body) => {
      await expect(body.locator("li")).toHaveCount(deepestListLevels);
      await expect.poll(() => deepestNesting(body, "ul")).toBe(deepestListLevels);
    },
    name: "100 list items nested by indentation then lines of text",
  },
  {
    body: lazyQuote,
    // The quote holds one paragraph: its own line and the 15,998 lazy lines under it, joined by
    // the soft-break policy a body reads with, so 15,999 `x` and the 15,998 spaces between them.
    formatted: async (body) => {
      await expect(body.locator("blockquote p")).toHaveCount(1);
      await expect.poll(() => textLength(body.locator("blockquote p"))).toBe(31_997);
    },
    name: "a quote followed by 15,998 lines without its marker",
  },
  {
    body: bracketRun,
    formatted: async (body) => {
      await expect(body.locator("p")).toHaveCount(1);
      await expect.poll(() => textLength(body.locator("p"))).toBe(30_000);
    },
    name: "15,000 opening brackets then 15,000 closing ones",
  },
  {
    body: calloutBracketRun,
    // The fence's label never closes, so the line is the paragraph its characters spell.
    formatted: async (body) => {
      await expect(body.locator("p")).toHaveCount(1);
      await expect.poll(() => textLength(body.locator("p"))).toBe(30_000);
    },
    name: "a callout opener holding 15,000 brackets",
  },
  {
    body: markerChain,
    // 50 levels stand; the 15,949 markers past the bound, and the `x`, read as the text they are.
    formatted: async (body) => {
      await expect(body.locator("li")).toHaveCount(deepestListLevels);
      await expect.poll(() => deepestNesting(body, "ul")).toBe(deepestListLevels);
      await expect.poll(() => textLength(body.locator("li").last())).toBe(31_899);
    },
    name: "15,999 list markers on one line",
  },
  {
    body: padTo(`${"- ".repeat(49)}x\n`, "\n", markdownWorkerThreshold - 1),
    formatted: async (body) => {
      await expect(body.locator("li")).toHaveCount(49);
      await expect.poll(() => deepestNesting(body, "ul")).toBe(49);
    },
    name: "49 list markers on one line then blank lines, just under the worker's threshold",
  },
  {
    body: padTo("", "- x\n", markdownWorkerThreshold - 1),
    formatted: async (body) => {
      await expect(body.locator("li")).toHaveCount(subThresholdItems);
    },
    name: `${subThresholdItems.toLocaleString("en-US")} list items, just under the worker's threshold`,
  },
];

// A comment card on the Conversation is collapsed until the reader opens its thread: collapsed it
// shows a two-line preview of the body (`ThreadCard`), open it shows the body itself. The
// reference a reader follows to a comment - `/issues/<key>/comments/<id>` - lands on the
// Conversation tab with that thread open, so these rows measure what a reader who followed one
// waits for: both of the card's parses of the body, preview and body, from the first comments GET.
for (const row of rows) {
  test(`Conversation draws a comment of ${row.name} without holding the main thread for a second`, async ({
    browser,
  }) => {
    const issue = await seedIssue("Heavy bodies");
    const comment = await createComment(issue.key, { body: row.body }, agent);

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      const card = page.locator('[data-turn^="comment:"]');
      const measured = await measureDraw(page, {
        drawn: () => row.formatted(markdownBody(card)),
        open: async () => {
          await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
          await expect(card).toHaveCount(1);
        },
        reset: new RegExp(`/api/v1/issues/${issue.key}/comments(\\?.*)?$`),
      });
      await expectSettled(card);
      record(measured);
      expectUnderTheBound(measured);
    } finally {
      await alice.close();
    }
  });
}

test("Conversation draws a message of 8,000 list items without holding the main thread for a second", async ({
  browser,
}) => {
  const issue = await seedIssue("A heavy message");
  await createMessage(issue.key, { body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const card = page.locator('[data-turn^="message:"]');
    const measured = await measureDraw(page, {
      drawn: async () => {
        await expect(markdownBody(card).locator("li")).toHaveCount(8_000);
      },
      open: async () => {
        await page.goto(`/issues/${issue.key}/conversation`);
        await expect(card).toHaveCount(1);
      },
      reset: new RegExp(`/api/v1/issues/${issue.key}/messages(\\?.*)?$`),
    });
    await expectSettled(card);
    record(measured);
    expectUnderTheBound(measured);
  } finally {
    await alice.close();
  }
});

// The ask card draws a long body in its thread, not in its question: the server caps a question
// at `contracts.AskQuestionMax` (2,000 UTF-16 units), under the worker's own threshold, so no ask
// question can carry this list. The surface under test is the same one - the Inbox's ask card,
// drawing a body through `MarkdownBody` - and its reply is the longest body it ever draws.
test("the Inbox's ask card draws a reply of 8,000 list items without holding the main thread for a second", async ({
  browser,
}) => {
  const issue = await seedIssue("A heavy ask thread");
  const ask = await createAsk(
    issue.key,
    { question: "Which shape should the list take?", urgency: "med" },
    agent
  );
  await createComment(issue.key, { ask_id: ask.id, body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const card = page.getByTestId(`ask-${ask.id}`);
    const measured = await measureDraw(page, {
      drawn: async () => {
        await expect(markdownBody(card.locator("[data-reply]").first()).locator("li")).toHaveCount(
          8_000
        );
      },
      open: async () => {
        await page.goto("/");
        await expect(card).toBeVisible();
      },
      reset: /\/api\/v1\/inbox(\?.*)?$/,
    });
    await expectSettled(card);
    record(measured);
    expectUnderTheBound(measured);
  } finally {
    await alice.close();
  }
});

// A hover card previews the target's body on one line (`MarkdownPreview`'s `paintOneLine`), so
// the 8,000 items read as their 8,000 `x` separated by 7,999 spaces. The body still has to be
// parsed whole, which is what froze the tab of a reader who only pointed at a reference.
test("a hover card of a comment of 8,000 list items draws without holding the main thread for a second", async ({
  browser,
}) => {
  const issue = await seedIssue("A heavy hover card");
  const list = await createComment(issue.key, { body: plainList }, agent);
  await createComment(
    issue.key,
    { body: `See dispatch://${issue.key}/comment/${list.id} for the list.` },
    agent
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const turns = page.getByRole("list", { name: "Conversation turns" });
    const preview = page.getByRole("tooltip").locator("[data-markdown-preview]");
    const measured = await measureDraw(page, {
      drawn: async () => {
        await expect.poll(() => textLength(preview)).toBe(15_999);
      },
      open: async () => {
        await page.goto(`/issues/${issue.key}/conversation`);
        const reference = turns.locator("[data-dispatch-ref]").first();
        await expect(reference).toBeVisible();
        await reference.hover();
        await expect(page.getByRole("tooltip")).toBeVisible();
      },
      reset: new RegExp(`/api/v1/comments/${list.id}(\\?.*)?$`),
    });
    await expectSettled(page.getByRole("tooltip"));
    record(measured);
    expectUnderTheBound(measured);
  } finally {
    await alice.close();
  }
});

/** How long one measurement must hold before a row reads it as where the page has come to rest. */
const AT_REST_MS = 600;

/**
 * `measure` once it has answered the same thing for `AT_REST_MS`, which is where the page has
 * come to rest; `undefined` where it never answered one.
 *
 * The two rows below measure a place, let a long body format above it, and measure again, so
 * anything else that moves the page between the two reads is read as the late paint's doing. The
 * deep link they land on marks the conversation read, and the `New` mark leaving each of the 13
 * cards once the server answers (`ConversationTab`'s `item.unread`) is 24 px a card: the document
 * loses 312 px and the browser's own scroll anchoring scrolls by it, leaving the card the reader
 * is on 24 px lower than it first rendered. Measured in WebKit on this pod
 * (sjawhar/legion#2519), that landed 600-700 ms after the first read in 2 runs of 21, and the
 * late paint itself moved the content under it by 0 px in the failing runs exactly as in the
 * passing ones. So the first read waits for the page to stop moving; what the paint does to it
 * is what these rows measure.
 */
async function atRest(
  measure: () => Promise<number | undefined>,
  what: string
): Promise<number | undefined> {
  let place: number | undefined;
  let since = 0;
  await expect
    .poll(
      async () => {
        const now = await measure();
        if (now === undefined) {
          return false;
        }
        if (now !== place) {
          place = now;
          since = Date.now();
          return false;
        }
        return Date.now() - since >= AT_REST_MS;
      },
      { intervals: [100], message: `${what} never came to rest` }
    )
    .toBe(true);
  return place;
}

// The reader's place is proven in Chromium and WebKit: the compensation reads layout the two
// engines measure and anchor differently.
test("a long comment above the reader keeps the comment they are reading where it was when it formats", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium" && testInfo.project.name !== "webkit",
    "the reader's place is proven in Chromium and WebKit"
  );
  const issue = await seedIssue("A late paint above the reader");
  for (let turn = 1; turn <= 12; turn += 1) {
    await createComment(issue.key, { body: `Turn ${turn} is short and already formatted.` }, agent);
  }
  // The newest card is the first in the document, so the long body sits above every other turn;
  // its own deep link lands with its thread open, which is where the body itself is drawn.
  const long = await createComment(issue.key, { body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const held = await holdTheWorkerChunk(alice);
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/comments/${long.id}`);
    const cards = page.locator('[data-turn^="comment:"]');
    await expect(cards).toHaveCount(13);
    const longCard = cards.first();
    await expect(longCard.locator("[data-markdown-pending]")).toHaveCount(1);
    await expectTheWorkerWasAskedFor(held);

    const reading = cards.last();
    await reading.scrollIntoViewIfNeeded();
    await expect(reading).toBeVisible();
    const before = await atRest(
      async () => (await reading.boundingBox())?.y,
      "the comment the reader is on"
    );
    expect(before, "the comment the reader is on is on screen").not.toBeUndefined();

    held.release();
    await expect(markdownBody(longCard).locator("li")).toHaveCount(8_000);
    await expectSettled(longCard);

    const after = (await reading.boundingBox())?.y;
    expect(after, "the comment the reader is on is still on screen").not.toBeUndefined();
    expect(
      Math.abs((after ?? 0) - (before ?? 0)),
      "the comment the reader was on moved when the long body above it formatted"
    ).toBeLessThan(2);
  } finally {
    await alice.close();
  }
});

/**
 * Scrolls the page once, the moment the first batch of the pending body reaches it, so the
 * reader's scroll lands between two batches of the paint rather than before or after the whole
 * of it. A `MutationObserver` on the pending element is what sees the batch; `window.scrollBy`
 * moves the page exactly as a wheel, a key or a drag of the scrollbar does, and what the page
 * is given to tell them apart by is the same in each case: its own scroll position changed, and
 * the content under the formatting body with it.
 */
async function scrollOnTheFirstBatch(page: Page, by: number): Promise<void> {
  await page.evaluate((distance) => {
    const pending = document.querySelector("[data-markdown-pending]");
    if (pending === null) {
      throw new Error("no body is waiting on the parse worker");
    }
    window.__heavyBodiesReaderScrolled = 0;
    const observer = new MutationObserver(() => {
      if (pending.childElementCount === 0) {
        return;
      }
      observer.disconnect();
      const from = window.scrollY;
      window.scrollBy(0, distance);
      window.__heavyBodiesReaderScrolled = window.scrollY - from;
    });
    observer.observe(pending, { childList: true });
  }, by);
}

// A reader does not sit still for the ~32 batches a body of 8,000 items lands in. With the
// reader's place read once before the first batch and put back after the last, a reader who
// scrolled by S while the body formatted was carried back by exactly S - the correction for the
// body's growth swallowed their own scroll whole (sjawhar/legion#2519). The row scrolls between
// two batches and asks for exactly one thing: the card they are reading ends where their scroll
// put it, with the rest of the body still landing above it.
test("a reader who scrolls while a long body above them formats keeps where they scrolled to", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium" && testInfo.project.name !== "webkit",
    "the reader's place is proven in Chromium and WebKit"
  );
  const issue = await seedIssue("A reader scrolling through a late paint");
  for (let turn = 1; turn <= 12; turn += 1) {
    await createComment(issue.key, { body: `Turn ${turn} is short and already formatted.` }, agent);
  }
  const long = await createComment(issue.key, { body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const held = await holdTheWorkerChunk(alice);
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/comments/${long.id}`);
    const cards = page.locator('[data-turn^="comment:"]');
    await expect(cards).toHaveCount(13);
    const longCard = cards.first();
    await expect(longCard.locator("[data-markdown-pending]")).toHaveCount(1);
    await expectTheWorkerWasAskedFor(held);

    const reading = cards.last();
    await reading.scrollIntoViewIfNeeded();
    await expect(reading).toBeVisible();
    const before = await atRest(
      async () => (await reading.boundingBox())?.y,
      "the comment the reader is on"
    );
    expect(before, "the comment the reader is on is on screen").not.toBeUndefined();

    // Back up through the turns they have read: the deep link left them at the end of the page,
    // where scrolling further down moves nothing.
    await scrollOnTheFirstBatch(page, -200);
    held.release();
    await expect(markdownBody(longCard).locator("li")).toHaveCount(8_000);
    await expectSettled(longCard);

    const scrolled = await page.evaluate(() => window.__heavyBodiesReaderScrolled ?? 0);
    expect(
      Math.abs(scrolled),
      "the reader scrolled while the body above them formatted"
    ).toBeGreaterThan(0);
    const after = (await reading.boundingBox())?.y;
    expect(after, "the comment the reader is on is still on screen").not.toBeUndefined();
    expect(
      Math.abs((after ?? 0) - ((before ?? 0) - scrolled)),
      "the reader was carried off the place they scrolled to while the body above them formatted"
    ).toBeLessThan(2);
  } finally {
    await alice.close();
  }
});

/** How far the newest card's top sits under the docked composer, which is where Jump to latest
 *  places the reader. The deep link this row lands on also opens the comment in the margin, whose
 *  card carries a reply composer of the same name, so the docked one - the first in the page - is
 *  the one named here. */
async function underTheComposer(page: Page): Promise<number> {
  const composer = await page.getByRole("form", { name: "Comment composer" }).first().boundingBox();
  const newest = await page.locator("[data-turn]").first().boundingBox();
  if (composer === null || newest === null) {
    throw new Error("the composer and the newest turn must both be on screen");
  }
  return newest.y - (composer.y + composer.height);
}

test("Jump to latest keeps a newest long comment under the composer when it formats late", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium" && testInfo.project.name !== "webkit",
    "the reader's place is proven in Chromium and WebKit"
  );
  const issue = await seedIssue("A late paint at the newest turn");
  for (let turn = 1; turn <= 12; turn += 1) {
    await createComment(issue.key, { body: `Turn ${turn} is short and already formatted.` }, agent);
  }
  const long = await createComment(issue.key, { body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const held = await holdTheWorkerChunk(alice);
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/comments/${long.id}`);
    const newest = page.locator('[data-turn^="comment:"]').first();
    await expect(page.locator('[data-turn^="comment:"]')).toHaveCount(13);
    await expect(newest.locator("[data-markdown-pending]")).toHaveCount(1);
    await expectTheWorkerWasAskedFor(held);

    // The reader reads back through the older turns, which is where Jump to latest appears, and
    // then takes it: it places the newest card immediately under the docked composer.
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    const jump = page.getByTestId("jump-to-latest");
    await expect(jump).toBeVisible();
    await jump.click();
    const placed = await atRest(() => underTheComposer(page), "the newest card's place");
    expect(placed, "Jump to latest placed the newest card").not.toBeUndefined();

    held.release();
    await expect(markdownBody(newest).locator("li")).toHaveCount(8_000);
    await expectSettled(newest);

    expect(
      Math.abs((await underTheComposer(page)) - (placed ?? 0)),
      "the newest card left the place Jump to latest gave it when it formatted"
    ).toBeLessThan(2);
  } finally {
    await alice.close();
  }
});

// A reader who opens a long body reads its text while the worker is still parsing: it is in the
// page as text, and it is theirs to select and copy. Nothing covers it and nothing turns
// selection off, so a drag across the pending card answers with the body's own words.
test("a long body's text can be selected while the worker still owes its formatting", async ({
  browser,
}) => {
  const issue = await seedIssue("A pending body a reader copies");
  const comment = await createComment(issue.key, { body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const held = await holdTheWorkerChunk(alice);
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    const card = page.locator('[data-turn^="comment:"]');
    await expect(card).toHaveCount(1);
    const pending = card.locator("[data-markdown-pending]").first();
    await expect(pending).toHaveCount(1);
    await expectTheWorkerWasAskedFor(held);
    await expect(pending).toContainText("- x");

    await pending.scrollIntoViewIfNeeded();
    const box = await pending.boundingBox();
    if (box === null) {
      throw new Error("the pending body is not on screen");
    }
    // The pending body is one run of text taller than the window, so its top sits at the top of
    // the page, under the docked composer: the drag starts below that, inside the body itself.
    const composer = await page
      .getByRole("form", { name: "Comment composer" })
      .first()
      .boundingBox();
    const under = composer === null ? 4 : composer.y + composer.height + 4;
    const from = { x: box.x + 4, y: Math.max(box.y + 4, under) };
    await page.mouse.move(from.x, from.y);
    await page.mouse.down();
    await page.mouse.move(from.x + 120, from.y + 28, { steps: 8 });
    await page.mouse.up();

    const selected = await page.evaluate(() => window.getSelection()?.toString() ?? "");
    expect(selected, "a drag across the pending body selected none of it").toContain("x");
    expect(selected.trim(), "the selection holds something other than the body").toMatch(
      /^[-x\s]+$/
    );

    held.release();
    await expect(markdownBody(card).locator("li")).toHaveCount(8_000);
    await expectSettled(card);
  } finally {
    await alice.close();
  }
});

// A worker that cannot be created, whose chunk fails to load, that crashes or that misses its
// deadline falls back to parsing on the page, never to no formatting: the reader waits, and then
// reads the body as a list. `data-markdown-fallback` keeps meaning only that the parser itself
// refused the body, so it is absent here. This row measures no main-thread gap: the page is
// doing on its own thread what the worker could not.
test("a comment of 8,000 list items still ends formatted when the parse worker's chunk cannot load", async ({
  browser,
}) => {
  const issue = await seedIssue("A worker chunk that never loads");
  const comment = await createComment(issue.key, { body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    let asked = false;
    await alice.route(workerChunk, async (route) => {
      asked = true;
      await route.abort();
    });
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    const card = page.locator('[data-turn^="comment:"]');
    await expect(card).toHaveCount(1);
    await expect(markdownBody(card).locator("li")).toHaveCount(8_000);
    await expectSettled(card);
    expect(
      asked,
      "the page never asked for a markdown-parse.worker chunk, so none was refused"
    ).toBe(true);
  } finally {
    await alice.close();
  }
});

// The one surface where a body grows under a paint: the live session view, where a streaming
// turn arrives whole with every frame. A reader who opens it mid-turn gets a first render that
// is the turn's own text, pending on the worker; the answer to it is painted in batches across
// tasks, and the next frame lands while that paint is still attaching them. The element belongs
// to that paint until it ends - nothing but the paint that replaces it, or the unmount, stops
// one - so the arriving frame writes nothing into it. Written there, its text would stand in
// front of the blocks still arriving behind it, and that is what the reader would read until
// that frame's own answer and its whole paint had landed: the regression this change has now
// repeated twice (sjawhar/legion#2519, `useRenderedMarkdown`).
//
// A poll cannot see it. The state lasts one paint batch, so a 60 ms poll lands between them and
// finds a body that looks whole. The page records it instead, from a `MutationObserver`
// installed before the page's own scripts: on every mutation, the characters of the newest
// assistant turn's own text nodes and the blocks beside them. Neither number alone is the
// defect - a pending body is all text and no blocks, a painted one is all blocks and no text -
// so what the row counts is the samples holding both.
//
// The shape is the whole trick, and a row that shrinks it proves nothing. At 4,000 paragraphs
// and 80 ms a frame, this row reds in all three engines at the code before the fix - 36 of 142
// recorded samples held both in Chromium, 27 of 96 in WebKit, 17 of 156 in Firefox, the widest
// standing 63,864 characters in front of 250 blocks. At 900 paragraphs with 150 ms frames the
// pull request measured it green at both heads: that paint is two batches and no frame lands
// inside one.

/** The paragraphs the turn already holds when the reader opens it. Each frame after that is the
 *  whole turn again, one paragraph longer. */
const streamedParagraphs = 4_000;
/** Frames the session publishes while the reader watches, and the gap between them. */
const streamedFrames = 60;
const streamedFrameGapMs = 80;
/** The turn's last frame, the one that ends it, adds one paragraph like the frames before it. */
const wholeTurnParagraphs = streamedParagraphs + streamedFrames + 1;

/** Loose text shorter than this is not a frame standing in front of a body: a paint attaches
 *  blocks, never prose, so what this passes over is whitespace and nothing a reader reads. The
 *  text of one of these frames is some 60,000 characters. */
const looseTextIsAFrame = 100;

/** The session the turn streams from. Seeded with no `last_seen`, so the fake Envoy answers
 *  every read of it with the current time and the row never meets an aged-out session. */
const streamingSession: FakeSession = {
  capabilities: ["steer"],
  dir: "/workspaces/streaming",
  machine_id: "streaming-host",
  roles: [],
  session_id: "019a1c07-6d4b-7e80-bf39-4a1b5c8d9e07",
  title: "A session mid-turn",
};

/** What the recorder took of the newest assistant turn's body on one mutation of the page. */
interface StreamSample {
  /** The `p` and `li` under it: the blocks a paint has attached so far. */
  readonly blocks: number;
  /** The characters in the text nodes directly under it, which is where a frame's own text
   *  would stand. */
  readonly raw: number;
  /** Whether the turn was still streaming when this was taken. */
  readonly streaming: boolean;
}

/** `count` paragraphs, `paragraph 0` to `paragraph <count - 1>`, joined by blank lines. */
function streamedBody(count: number): string {
  return Array.from({ length: count }, (_, index) => `paragraph ${index}`).join("\n\n");
}

/** One snapshot of the one turn: every frame carries the same message id, so each replaces the
 *  turn's whole text, and `seq` orders the snapshots. */
function turnFrame(seq: number, paragraphs: number, streaming: boolean): object {
  return {
    kind: "message",
    message: {
      at: 1_000,
      id: "a-turn-a-reader-opened-while-it-streamed",
      parts: [{ text: streamedBody(paragraphs), type: "text" }],
      role: "assistant",
      streaming,
    },
    seq,
    v: 1,
  };
}

/**
 * Installs the recorder before the page's own scripts, so nothing of the first render happens
 * unwatched. It watches the document rather than its element, which a script running this early
 * can still find nothing of, and reads the newest assistant turn's body on every mutation
 * anywhere in the page: the body itself is not in the document yet when this runs.
 */
async function recordTheStreamedTurn(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const samples: StreamSample[] = [];
    let streaming = true;
    window.__heavyBodiesStreamSamples = samples;
    window.__heavyBodiesTurnEnded = () => {
      streaming = false;
    };
    const take = (): void => {
      const turns = document.querySelectorAll('[data-testid="agent-message-assistant"]');
      const body = turns[turns.length - 1]?.querySelector(".dispatch-markdown") ?? null;
      if (body === null) {
        return;
      }
      let raw = 0;
      for (const child of body.childNodes) {
        if (child.nodeType === Node.TEXT_NODE) {
          raw += child.textContent?.length ?? 0;
        }
      }
      samples.push({ blocks: body.querySelectorAll("p, li").length, raw, streaming });
    };
    new MutationObserver(take).observe(document, {
      characterData: true,
      childList: true,
      subtree: true,
    });
  });
}

/** Waits until `at`, so the frames keep their cadence whatever each publish costs: a publish
 *  that overran its slot is followed at once by the next rather than pushing every one after
 *  it back. */
function sleepUntil(at: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, Math.max(0, at - Date.now()));
  return promise;
}

test("a reader who opens the live view while a long turn streams is never shown a frame's text in front of the body", async ({
  browser,
}) => {
  const session = streamingSession.session_id;
  await setLiveSessions([streamingSession]);
  await publishAgentStreamFrame(
    session,
    { frames: [turnFrame(1, streamedParagraphs, true)], session_id: session, v: 1 },
    "replay"
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await recordTheStreamedTurn(page);
    await page.goto(`/agents/${session}/live`);
    const turns = page.getByTestId("agent-message-assistant");
    // The replay in the page is the viewer attached: a frame published before that reaches
    // nobody. The turn is the reader's own text here, with its paint still to come.
    await expect(turns).toHaveCount(1);
    const turn = turns.last();

    const firstFrameAt = Date.now();
    for (let frame = 0; frame < streamedFrames; frame += 1) {
      await sleepUntil(firstFrameAt + frame * streamedFrameGapMs);
      await publishAgentStreamFrame(
        session,
        turnFrame(frame + 2, streamedParagraphs + frame + 1, true)
      );
    }
    // Every sample after this is the turn over, so the guard below reads only what the
    // streaming window left.
    await page.evaluate(() => window.__heavyBodiesTurnEnded?.());
    await publishAgentStreamFrame(
      session,
      turnFrame(streamedFrames + 2, wholeTurnParagraphs, false)
    );

    const body = markdownBody(turn);
    await expect(body.locator("p")).toHaveCount(wholeTurnParagraphs);
    await expectSettled(turn);
    expect(
      await looseTextLength(body),
      "the turn ended with text of its own standing beside the body's blocks"
    ).toBe(0);

    const samples = await page.evaluate(() => window.__heavyBodiesStreamSamples ?? []);
    const inFront = samples.filter((sample) => sample.blocks > 0 && sample.raw > looseTextIsAFrame);
    // A body under construction: blocks in it, and not yet the turn's own. A sample holding the
    // whole turn is a paint that has landed, which says nothing about the window.
    const partBuilt = samples.filter(
      (sample) => sample.streaming && sample.blocks > 0 && sample.blocks < wholeTurnParagraphs
    );
    const sawAFrameText = samples.filter((sample) => sample.raw > looseTextIsAFrame);
    // Both counts are the run's evidence, kept on the result even when it passes: a runner too
    // fast or too slow to reach the window is read here rather than as a silently empty green.
    test.info().annotations.push({
      description:
        `${samples.length} samples, ${partBuilt.length} of them a part-built body mid-stream, ` +
        `${sawAFrameText.length} holding a frame's text`,
      type: "recorded while the turn streamed",
    });
    const worst = inFront.reduce((held, sample) => (sample.raw > held.raw ? sample : held), {
      blocks: 0,
      raw: 0,
      streaming: false,
    } satisfies StreamSample);
    expect(
      inFront.length,
      `the reader saw the frame's raw text in front of a body already holding blocks: ` +
        `${inFront.length} of ${samples.length} recorded samples held both, the widest ` +
        `${worst.raw} characters in front of ${worst.blocks} blocks`
    ).toBe(0);
    // The window the row is about was driven at all, which is two things and not one: the
    // reader saw a body under construction, and the reader saw a frame's text. A green run
    // means neither of them stood in front of the other; a run that measured neither is no
    // evidence of that, and on a node faster than this pod - one that paints 4,000 paragraphs
    // in two batches, where no frame can land inside one - this row would otherwise stay green
    // for as long as it is in the suite while proving nothing (the pull request measured
    // exactly that at 900 paragraphs and 150 ms frames, green at both heads). Either guard
    // failing names the shape as outgrown by the node, not the change as broken: raise the
    // paragraph count. One sample is as loose as each asks for; three runs of each engine on a
    // two-core pod left 19 to 102 part-built samples.
    expect(
      partBuilt.length,
      `nothing painted while the turn streamed, so this row measured none of the window it is ` +
        `about: no part-built body in ${samples.length} recorded samples`
    ).toBeGreaterThan(0);
    expect(
      sawAFrameText.length,
      `no frame's text ever reached the element, so this row measured none of the window it is ` +
        `about: no sample over ${looseTextIsAFrame} characters in ${samples.length} recorded ` +
        `samples`
    ).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});
