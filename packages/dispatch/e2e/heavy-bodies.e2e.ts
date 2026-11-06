// What a heavy body costs a reader's own thread, in the surfaces that draw one: the Conversation,
// the Inbox's ask card and a hover card. The same shapes written as documents through the API are
// `heavy-document-writes.e2e.ts`, and both specs read the bodies from `heavy-bodies.ts`, so the
// two measure the very same bytes. The one surface where a body grows under a paint - the live
// view, where a streaming turn arrives whole with every frame - is `heavy-streamed-turn.e2e.ts`,
// which reads a drawn body through the same `heavy-bodies-page.ts` as the rows here
// (sjawhar/legion#2519). What a row here holds the main thread for is measured by
// `main-thread-hold.ts`, which `nested-directive.e2e.ts` measures its own two rows with, so one
// instrument and one bound answer for both (sjawhar/legion#2612).
import { expect, type Locator, type Page, test } from "@playwright/test";

// `markdownWorkerThreshold` comes from the module that carries it and imports nothing: the
// engine around it (`markdown-engine.ts`) is a browser module this spec's process cannot load.
// It is no rule over the rows below: every body here is a stored comment, message or reply, and
// a stored text is drawn from the tree the server read it into and parsed nowhere, whatever its
// length (sjawhar/legion#2712). What it still gives the table is a size. The two shortest rows
// sit one character under it, which is the longest body the page would ever have parsed on its
// own thread, so the table spans from there to the comment cap.
import { markdownWorkerThreshold } from "../web/src/features/refs/markdown-worker-threshold";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  getIssue,
  putIssueState,
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
import { expectSettled, markdownBody } from "./heavy-bodies-page";
import { expectUnderTheBound, measureDraw, record } from "./main-thread-hold";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

declare global {
  interface Window {
    /** How far the reader's own scroll moved the page, once `scrollOnTheFirstBatch` has run it;
     *  0 until then. */
    __heavyBodiesReaderScrolled?: number;
    /** When each batch of a sliced paint reached the page, on the page's own clock
     *  (`watchTheFill`). */
    __heavyBodiesBatches?: number[];
    /** Where Jump to latest placed the newest card, read in the page the moment the reader took
     *  it (`jumpToLatestOnTheFirstBatch`); `undefined` until then. */
    __heavyBodiesJumpPlaced?: number;
    /** When the reader pressed the mouse, on the page's own clock (`recordTheReadersPress`);
     *  0 until they do. */
    __heavyBodiesPressedAt?: number;
  }
}

const agent = {
  actor: { id: "e2e-heavy-bodies-agent", kind: "session" as const },
  as: "agent" as const,
};

/**
 * The element a sliced paint is filling: it carries the pending marker from before its first
 * batch until after its last, whether or not the reader was ever shown pending text
 * (`useRenderedMarkdown.tsx`). A stored body is never shown as text - the server's tree is drawn
 * in the commit that mounts it - so this mark is all that says a body is still landing.
 */
const fillingBody = "[data-markdown-pending]";

/** How many items `- x\n` repeated to one character under the worker's threshold spells: the
 *  last one is cut short of its newline and is a list item all the same. Derived, so the row
 *  that counts them follows `markdownWorkerThreshold` wherever it is set. */
const subThresholdItems = Math.ceil((markdownWorkerThreshold - 1) / "- x\n".length);

/** Go's nesting bound, which the browser now counts the same way (`pmdoc/nesting.go`
 *  `maxNesting`): a list and its item each count one, so 50 levels stand and the 51st reads as
 *  the text it is. */
const deepestListLevels = 50;

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

/**
 * Records when each batch of a sliced paint reached the page, on the page's own clock, so a row
 * can say that the body landed across tasks and name the window it measured.
 *
 * A `MutationObserver` is what sees a batch rather than a poll from this process: each batch is
 * a task of its own (`paintAcrossTasks` in `useRenderedMarkdown.tsx`), and the whole fill of an
 * 8,000-item list is a few hundred milliseconds. It watches the document, not the body: a body
 * that has yet to arrive is not in the page when this is installed, and a stored one starts
 * filling in the tasks after the commit that mounted it, with nothing to hold it up.
 */
async function watchTheFill(page: Page): Promise<void> {
  await page.addInitScript((filling) => {
    const batches: number[] = [];
    window.__heavyBodiesBatches = batches;
    new MutationObserver((records) => {
      for (const record of records) {
        if (
          record.addedNodes.length > 0 &&
          record.target instanceof Element &&
          record.target.closest(filling) !== null
        ) {
          batches.push(Date.now());
          return;
        }
      }
    }).observe(document, { childList: true, subtree: true });
  }, fillingBody);
}

/** Every batch of a sliced paint the page has seen, as `watchTheFill` recorded it. */
function batchesOfTheFill(page: Page): Promise<readonly number[]> {
  return page.evaluate(() => window.__heavyBodiesBatches ?? []);
}

/**
 * The body landed across tasks, which is the window each row below is about: the reader's place,
 * their own scrolling and their selection all have to survive the batches a long body arrives
 * in. A document of fewer nodes than one batch reaches the page in a single task and leaves no
 * such window (`paintsAcrossTasks` in `useRenderedMarkdown.tsx`), so a row that met none of it
 * measured nothing and reds here rather than passing quietly. The count and the span are kept on
 * the result either way: a runner that lands an 8,000-item list in two batches is read there
 * rather than as a silent green.
 */
async function expectTheBodyFilledAcrossTasks(page: Page): Promise<void> {
  const batches = await batchesOfTheFill(page);
  test.info().annotations.push({
    description:
      batches.length === 0
        ? "no batch of a sliced paint reached the page"
        : `${batches.length} batches over ${(batches.at(-1) ?? 0) - (batches[0] ?? 0)} ms`,
    type: "the long body's fill",
  });
  expect(
    batches.length,
    "the long body reached the page in one task, so none of the reader's own work happened " +
      "while it was landing: a stored body of 8,000 items is painted in slices " +
      "(`paintsAcrossTasks` in `useRenderedMarkdown.tsx`)"
  ).toBeGreaterThan(1);
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
// waits for: both of the card's draws of the body, preview and body, from the first comments GET.
for (const row of rows) {
  // SLOW(>=2s): the draw is the measurement. Each row of the table seeds its own body, opens a
  // page that has loaded nothing and waits for the whole formatted shape - the comments GET of
  // a body up to the comment cap, the draw of the tree it arrived with - no body here is parsed,
  // in the worker or on the page's own thread - and the paint that attaches as many as 16,000
  // elements in batches - and the hold it asserts is read over exactly that. The heaviest body
  // is 32,000 characters, which is what a reader is given to draw and what the bound is about,
  // so no part of this is fixture a faster seed could take out.
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

// SLOW(>=2s): the 8,000 items are drawn before anything is read off them - the messages GET, the
// worker's parse and the paint of 16,000 elements - and the hold this row bounds is read over
// that draw, which is the row.
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

// The ask card draws a long body in its thread, not in its question: the server holds a question
// and its options together to `DISPATCH_EVENT_BODY_MAX` (32,000 UTF-16 units), which this list
// fills on its own, so the list reaches the card as a reply. The surface under test is the same
// one - the Inbox's ask card, drawing a body through `MarkdownBody` - and its reply is the
// longest body it ever draws.
// SLOW(>=2s): the reply's 8,000 items are drawn in the card before the hold is read, and the
// Inbox pays for them where a reader does: the inbox GET brings the card and its reply together,
// so the parse and the paint of 16,000 elements are what this row waits out.
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

/**
 * How many short turns the reader has read before the long one arrives. Enough that the long
 * turn, which is the newest and so the first card in the document, stays wholly above the
 * reader's window even after they scroll back towards it by 200 px
 * (`scrollOnTheFirstBatch`): a body the reader can see is left where it is as it fills, since
 * its own top does not move and that is where they read from, and only a body above them is put
 * back for (`keepTheReadersPlace` in `useRenderedMarkdown.tsx`). Twelve turns leave 724 px
 * between the two in Chromium at 1280x720 (measured), 80 px of it above the window, which a
 * reader scrolling back crosses; these leave some 900 px above it.
 */
const readTurns = 24;

/**
 * A Conversation of `readTurns` short turns the reader has already read, with every card of it in
 * the page: what the long turn each row below publishes arrives above. Answers the issue and the
 * turn cards, newest first.
 *
 * They are messages rather than comments because the Conversation draws a message's body whole,
 * where a comment card shows a two-line preview until its thread is opened (`ThreadCard`), and
 * the body drawn whole is the one that fills in slices. The conversation is marked read before
 * the page opens it, so no card ever carries a `New` mark the page would take back off it: the
 * turn that arrives next is the only unread one, and its own mark is inside the card that
 * arrives with it (`e2e/conversation.e2e.ts` reads the same shape).
 */
async function aConversationTheReaderHasRead(
  page: Page,
  title: string
): Promise<{ readonly cards: Locator; readonly issue: { readonly key: string } }> {
  const issue = await seedIssue(title);
  for (let turn = 1; turn <= readTurns; turn += 1) {
    await createMessage(issue.key, { body: `Turn ${turn} is short and already formatted.` }, agent);
  }
  await putIssueState(issue.key, { last_read_seq: (await getIssue(issue.key)).last_seq });
  await page.goto(`/issues/${issue.key}/conversation`);
  const cards = page.locator('[data-turn^="message:"]');
  await expect(cards).toHaveCount(readTurns);
  return { cards, issue };
}

// The reader's place is proven in Chromium and WebKit: the compensation reads layout the two
// engines measure and anchor differently. This file runs in each engine's own `-alone` project,
// where the clock rows have the machine to themselves (`e2e/playwright.config.ts`), so those are
// the project names a row of it can run under.
// SLOW(>=2s): the row loads a Conversation of `readTurns` turns, waits for it to come to rest -
// 600 ms of one answer (`atRest`) - and only then lets a message of 8,000 items arrive above the
// reader. What it is about is that message filling, so the row pays a whole draw of 16,000
// elements after a whole load.
test("a long message arriving above the reader keeps the turn they are reading where it was while it fills", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium-alone" && testInfo.project.name !== "webkit-alone",
    "the reader's place is proven in Chromium and WebKit"
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheFill(page);
    const { cards, issue } = await aConversationTheReaderHasRead(
      page,
      "A late fill above the reader"
    );

    const reading = cards.last();
    await reading.scrollIntoViewIfNeeded();
    await expect(reading).toBeVisible();
    const before = await atRest(
      async () => (await reading.boundingBox())?.y,
      "the turn the reader is on"
    );
    expect(before, "the turn the reader is on is on screen").not.toBeUndefined();

    // The newest card is the first in the document, so the arriving body sits above every turn
    // the reader has read; its 8,000 items land in the tasks after the commit that inserted it,
    // which is what the reader's place has to survive.
    await createMessage(issue.key, { body: plainList }, agent);
    const longCard = cards.first();
    await expect(cards).toHaveCount(readTurns + 1);
    await expect(markdownBody(longCard).locator("li")).toHaveCount(8_000);
    await expectSettled(longCard);
    await expectTheBodyFilledAcrossTasks(page);

    const after = (await reading.boundingBox())?.y;
    expect(after, "the turn the reader is on is still on screen").not.toBeUndefined();
    expect(
      Math.abs((after ?? 0) - (before ?? 0)),
      "the turn the reader was on moved while the long body above it filled"
    ).toBeLessThan(2);
  } finally {
    await alice.close();
  }
});

/**
 * Scrolls the page once, the moment the first batch of a filling body reaches it, so the
 * reader's scroll lands between two batches of the paint rather than before or after the whole
 * of it. A `MutationObserver` is what sees that batch, and it watches the document rather than
 * the body: the turn whose body this is has not arrived when the row installs it. `window.scrollBy`
 * moves the page exactly as a wheel, a key or a drag of the scrollbar does, and what the page
 * is given to tell them apart by is the same in each case: its own scroll position changed, and
 * the content under the filling body with it.
 */
async function scrollOnTheFirstBatch(page: Page, by: number): Promise<void> {
  await page.evaluate(
    ({ distance, filling }) => {
      window.__heavyBodiesReaderScrolled = 0;
      const observer = new MutationObserver((records) => {
        for (const record of records) {
          if (
            record.addedNodes.length === 0 ||
            !(record.target instanceof Element) ||
            record.target.closest(filling) === null
          ) {
            continue;
          }
          observer.disconnect();
          const from = window.scrollY;
          window.scrollBy(0, distance);
          window.__heavyBodiesReaderScrolled = window.scrollY - from;
          return;
        }
      });
      observer.observe(document, { childList: true, subtree: true });
    },
    { distance: by, filling: fillingBody }
  );
}

// A reader does not sit still for the ~48 batches a body of 8,000 items lands in. With the
// reader's place read once before the first batch and put back after the last, a reader who
// scrolled by S while the body landed was carried back by exactly S - the correction for the
// body's growth swallowed their own scroll whole (sjawhar/legion#2519). The row scrolls between
// two batches and asks for exactly one thing: the card they are reading ends where their scroll
// put it, with the rest of the body still landing above it.
// SLOW(>=2s): the scroll has to land between two batches of the paint, so the row waits out the
// page coming to rest (600 ms, `atRest`), then the batches the 8,000 items arrive in: the window
// it measures exists only while that fill runs, and a shorter body has no such window.
test("a reader who scrolls while a long body above them fills keeps where they scrolled to", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium-alone" && testInfo.project.name !== "webkit-alone",
    "the reader's place is proven in Chromium and WebKit"
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheFill(page);
    const { cards, issue } = await aConversationTheReaderHasRead(
      page,
      "A reader scrolling through a late fill"
    );

    const reading = cards.last();
    await reading.scrollIntoViewIfNeeded();
    await expect(reading).toBeVisible();
    const before = await atRest(
      async () => (await reading.boundingBox())?.y,
      "the turn the reader is on"
    );
    expect(before, "the turn the reader is on is on screen").not.toBeUndefined();

    // Back up through the turns they have read: the reader is at the end of the page, where
    // scrolling further down moves nothing.
    await scrollOnTheFirstBatch(page, -200);
    await createMessage(issue.key, { body: plainList }, agent);
    const longCard = cards.first();
    await expect(cards).toHaveCount(readTurns + 1);
    await expect(markdownBody(longCard).locator("li")).toHaveCount(8_000);
    await expectSettled(longCard);
    await expectTheBodyFilledAcrossTasks(page);

    const scrolled = await page.evaluate(() => window.__heavyBodiesReaderScrolled ?? 0);
    expect(
      Math.abs(scrolled),
      "the reader scrolled while the body above them filled"
    ).toBeGreaterThan(0);
    const after = (await reading.boundingBox())?.y;
    expect(after, "the turn the reader is on is still on screen").not.toBeUndefined();
    expect(
      Math.abs((after ?? 0) - ((before ?? 0) - scrolled)),
      "the reader was carried off the place they scrolled to while the body above them filled"
    ).toBeLessThan(2);
  } finally {
    await alice.close();
  }
});

/** How far the newest card's top sits under the docked composer, which is where Jump to latest
 *  places the reader. A card opened in the margin carries a reply composer of the same name, so
 *  the docked one - the first in the page - is the one named here. */
async function underTheComposer(page: Page): Promise<number> {
  const composer = await page.getByRole("form", { name: "Comment composer" }).first().boundingBox();
  const newest = await page.locator("[data-turn]").first().boundingBox();
  if (composer === null || newest === null) {
    throw new Error("the composer and the newest turn must both be on screen");
  }
  return newest.y - (composer.y + composer.height);
}

/**
 * Takes Jump to latest in the page the moment the first batch of a filling body reaches it, and
 * records where it placed the newest card - its top under the composer's bottom, which is what
 * `underTheComposer` reads from this process.
 *
 * The click is the page's own because the window is a few tasks wide: the whole fill of an
 * 8,000-item list is a few hundred milliseconds, and a click driven from here - which waits for
 * the button to be actionable and scrolls it into view first - would land once the body had
 * finished landing. What the row is about is the place a reader is given while the newest body
 * is still filling under it.
 */
async function jumpToLatestOnTheFirstBatch(page: Page): Promise<void> {
  await page.evaluate((filling) => {
    const observer = new MutationObserver((records) => {
      for (const record of records) {
        if (
          record.addedNodes.length === 0 ||
          !(record.target instanceof Element) ||
          record.target.closest(filling) === null
        ) {
          continue;
        }
        observer.disconnect();
        const jump = document.querySelector<HTMLElement>('[data-testid="jump-to-latest"]');
        jump?.click();
        const composer = document
          .querySelector('[aria-label="Comment composer"]')
          ?.getBoundingClientRect();
        const newest = document.querySelector("[data-turn]")?.getBoundingClientRect();
        if (jump !== null && composer !== undefined && newest !== undefined) {
          window.__heavyBodiesJumpPlaced = newest.y - (composer.y + composer.height);
        }
        return;
      }
    });
    observer.observe(document, { childList: true, subtree: true });
  }, fillingBody);
}

// SLOW(>=2s): a reader reaches Jump to latest only from a page of turns they have scrolled back
// through, so the row loads `readTurns` of them, scrolls back through them and waits for the page
// to come to rest (600 ms of one answer, `atRest`) before a longer one of 8,000 items arrives.
// What it proves - that the place Jump to latest gives survives the fill - is then those 8,000
// items landing under the composer, which is a whole draw of 16,000 elements.
test("Jump to latest keeps a newest long turn under the composer while it fills", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "chromium-alone" && testInfo.project.name !== "webkit-alone",
    "the reader's place is proven in Chromium and WebKit"
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheFill(page);
    const { cards, issue } = await aConversationTheReaderHasRead(
      page,
      "A late fill at the newest turn"
    );

    // The reader reads back through the older turns, which is where Jump to latest appears once
    // a newer one arrives.
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await atRest(
      () => page.evaluate(() => window.scrollY),
      "the page the reader scrolled back through"
    );

    await jumpToLatestOnTheFirstBatch(page);
    await createMessage(issue.key, { body: plainList }, agent);
    const newest = cards.first();
    await expect(cards).toHaveCount(readTurns + 1);
    await expect(markdownBody(newest).locator("li")).toHaveCount(8_000);
    await expectSettled(newest);
    await expectTheBodyFilledAcrossTasks(page);

    const placed = await page.evaluate(() => window.__heavyBodiesJumpPlaced);
    expect(
      placed,
      "the reader never took Jump to latest while the newest body was filling"
    ).not.toBeUndefined();
    expect(
      Math.abs((await underTheComposer(page)) - (placed ?? 0)),
      "the newest card left the place Jump to latest gave it while its body filled"
    ).toBeLessThan(2);
  } finally {
    await alice.close();
  }
});

/** When the reader pressed the mouse, on the page's own clock, so a row can say which batches of
 *  a fill landed after their drag had begun. */
async function recordTheReadersPress(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__heavyBodiesPressedAt = 0;
    document.addEventListener(
      "mousedown",
      () => {
        window.__heavyBodiesPressedAt = Date.now();
      },
      { capture: true, once: true }
    );
  });
}

/** How long the row below waits for a body to begin filling before it says none did. Well inside
 *  the suite's own test timeout, so a run where nothing fills reads as this row's reason rather
 *  than as a timeout in the middle of its drag. */
const REACH_THE_FILL_MS = 15_000;

// A reader who opens a long body reads the part of it that has landed while the rest is still
// landing: it is in the page as the list it is, and it is theirs to select and copy. Nothing
// covers it and nothing turns selection off, so a drag across a body still filling answers with
// the body's own words.
// SLOW(>=2s): the drag has to cross the body between two batches of its fill, so the row opens
// the card, waits for the first batch, drags from where that batch put the body, and then waits
// out the rest of the 8,000 items - the selection is only worth proving of a body that still
// ends formatted, and formatting 8,000 items is the draw it is.
test("a long body's text can be selected while its formatting is still landing", async ({
  browser,
}) => {
  const issue = await seedIssue("A filling body a reader copies");
  const comment = await createComment(issue.key, { body: plainList }, agent);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheFill(page);
    await recordTheReadersPress(page);
    await page.goto(`/issues/${issue.key}/comments/${comment.id}`);
    const card = page.locator('[data-turn^="comment:"]');
    await expect(card).toHaveCount(1);
    // Where the drag starts, answered by the page in the task the first batch landed in: every
    // read from this process costs a round trip, and what the drag has to land inside is the
    // rest of the fill. The body is taller than the window, so its top sits at the top of the
    // page, under the docked composer: the drag starts below that, inside the body itself. A
    // body that never fills leaves this waiting out the row's own deadline, so what it answers
    // then is the row's own reason rather than a bare timeout.
    const start = await page
      .waitForFunction(
        (filling) => {
          const body = document.querySelector(filling);
          if (body === null || body.firstElementChild === null) {
            return null;
          }
          const box = body.getBoundingClientRect();
          const composer = document
            .querySelector('[aria-label="Comment composer"]')
            ?.getBoundingClientRect();
          const under = composer === undefined ? 4 : composer.y + composer.height + 4;
          return { x: box.x + 4, y: Math.max(box.y + 4, under) };
        },
        fillingBody,
        { polling: "raf", timeout: REACH_THE_FILL_MS }
      )
      .catch(() => {
        throw new Error(
          "no body in the page was ever filling, so the drag had nothing to cross while it " +
            "landed: a stored body of 8,000 items is painted in slices (`paintsAcrossTasks` in " +
            "`useRenderedMarkdown.tsx`)"
        );
      });
    const from = await start.jsonValue();
    if (from === null) {
      throw new Error("the page answered no place to start the drag from");
    }
    await page.mouse.move(from.x, from.y);
    await page.mouse.down();
    await page.mouse.move(from.x + 120, from.y + 28, { steps: 8 });
    await page.mouse.up();

    const selected = await page.evaluate(() => window.getSelection()?.toString() ?? "");
    expect(selected, "a drag across the filling body selected none of it").toContain("x");
    expect(selected.trim(), "the selection holds something other than the body").toMatch(
      /^[-x\s]+$/
    );

    await expect(markdownBody(card).locator("li")).toHaveCount(8_000);
    await expectSettled(card);
    // The drag crossed a body that was still being added to, which is what the row is about: a
    // body already whole when the press landed answers with the same words and proves nothing of
    // them.
    const { batches, pressedAt } = await page.evaluate(() => ({
      batches: window.__heavyBodiesBatches ?? [],
      pressedAt: window.__heavyBodiesPressedAt ?? 0,
    }));
    expect(
      batches.filter((at) => at >= pressedAt).length,
      "the body had finished landing before the drag began, so the selection crossed a body " +
        `nothing was being added to: ${batches.length} batches landed, the last of them ` +
        `${pressedAt - (batches.at(-1) ?? 0)} ms before the press`
    ).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});
