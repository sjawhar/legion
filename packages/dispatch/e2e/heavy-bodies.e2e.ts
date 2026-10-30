import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { type BrowserContext, expect, type Locator, type Page, test } from "@playwright/test";

// `markdownWorkerThreshold` comes from the module that carries it and imports nothing: the
// engine around it (`markdown-engine.ts`) is a browser module this spec's process cannot load.
// The two shortest rows below sit one character under it, so they prove the body the page parses
// on its own thread stays inside the same bound as the ones the worker takes.
import { markdownWorkerThreshold } from "../web/src/features/refs/markdown-worker-threshold";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
} from "./api";
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

/** `head` followed by `filler` repeated, cut to exactly `length` characters. */
function padTo(head: string, filler: string, length: number): string {
  const padding = filler.repeat(Math.ceil((length - head.length) / filler.length));
  return (head + padding).slice(0, length);
}

/** `count` list items, each nested one level deeper than the last by its indentation. */
function nestedByIndentation(count: number): string {
  let body = "";
  for (let item = 0; item < count; item += 1) {
    body += `${"  ".repeat(item)}- a\n`;
  }
  return body;
}

/** 8,000 list items: the shape a reader meets most, and the one no parse fix brings under a
 *  second on the page's own thread. */
const plainList = "- x\n".repeat(8_000);
const nestedThenBlankLines = padTo(nestedByIndentation(100), "\n", DISPATCH_BODY_MAX);
const nestedThenTextLines = padTo(nestedByIndentation(100), "x\n", DISPATCH_BODY_MAX);
const lazyQuote = `> x\n${"x\n".repeat(15_998)}`;
const lazyListItem = `- x\n${"x\n".repeat(15_998)}`;
const bracketRun = "[".repeat(15_000) + "]".repeat(15_000);
const calloutBracketRun = `:::callout[${"[".repeat(15_000)}${"]".repeat(14_989)}`;
const markerChain = `${"- ".repeat(15_999)}x`;
/** 49 items nested on one line, which Go accepts (49 lists and 49 items is 98 of its 100
 *  blocks), then blank lines to the cap. */
const nested49ThenBlankLines = padTo(`${"- ".repeat(49)}x\n`, "\n", DISPATCH_BODY_MAX);

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
      await expect(body.locator("li")).toHaveCount(1_000);
    },
    name: "1,000 list items, just under the worker's threshold",
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
const documentRows: readonly { body: string; name: string; stored: boolean; warm?: true }[] = [
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

test("a document write of each heavy body answers in under a second, stored or refused", async () => {
  const issue = await seedIssue("Heavy documents");

  /** Writes one row's body as a document of its own and answers what the write cost and refused. */
  async function write(
    row: (typeof documentRows)[number],
    name: string
  ): Promise<{ refusal: unknown; took: number }> {
    const started = performance.now();
    const refusal = await createIssueArtifact(issue.key, { content: row.body, name }, agent).then(
      () => undefined,
      (error: unknown) => error
    );
    return { refusal, took: performance.now() - started };
  }

  for (const [index, row] of documentRows.entries()) {
    // A document takes far more than a comment does, so each row is held to the comment cap: the
    // browser rows above and this one then read the very same shapes.
    expect(
      row.body.length,
      `${row.name}: the row is longer than a comment's body cap`
    ).toBeLessThanOrEqual(DISPATCH_BODY_MAX);
    if (row.warm === true) {
      // The first parse of this shape in a server process pays for the memory it touches, not for
      // the parsing (the table above): write it once, record that, and hold the bound against the
      // write after it, which is what every later writer of such a body meets.
      const first = await write(row, `heavy-${index}-first.md`);
      expect(first.refusal, `${row.name}: the warming write was refused`).toBeUndefined();
      test.info().annotations.push({
        description: `${Math.round(first.took)} ms`,
        type: `${row.name}: the first write of this shape in a fresh server`,
      });
    }
    const { refusal, took } = await write(row, `heavy-${index}.md`);

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
    const before = (await reading.boundingBox())?.y;
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
    const placed = await underTheComposer(page);

    held.release();
    await expect(markdownBody(newest).locator("li")).toHaveCount(8_000);
    await expectSettled(newest);

    expect(
      Math.abs((await underTheComposer(page)) - placed),
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
