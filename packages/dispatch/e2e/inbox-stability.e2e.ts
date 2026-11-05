import { expect, type Locator, type Page, test } from "@playwright/test";

import type { Ask, Issue } from "../web/src/api/types";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  getAsk,
  patchIssue,
} from "./api";
import {
  askCard,
  centreRow,
  hasReadingPane,
  inboxRow,
  listScrollTop,
  selectedRow,
} from "./inbox-pane";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const session = {
  actor: {
    kind: "session" as const,
    id: "e2e-session",
    origin: { session_title: "e2e-session-title", tmux: "dispatch:1.2" },
  },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

// Issue titles sharing no terms, so the server's duplicate check never refuses a seed. There
// are enough of them to fill a list past the bottom of the viewport at 1280 px, where the rows
// are short and scroll inside the list column: a row can only be held still where the list has
// somewhere to scroll.
const TITLES = [
  "Anchor",
  "Bridge",
  "Cedar",
  "Dune",
  "Ember",
  "Fjord",
  "Glass",
  "Harbor",
  "Island",
  "Jetty",
  "Kelp",
  "Lantern",
  "Marsh",
  "Nectar",
  "Obsidian",
  "Prairie",
  "Quartz",
  "Ridge",
  "Summit",
  "Thicket",
];

/** One seeded ask and the issue it was asked on. */
interface SeededAsk {
  ask: Ask;
  issue: Issue;
}

/** `count` open asks, each on its own issue, oldest first; `from` picks the first title. */
async function seedAsks(prefix: string, count: number, from = 0): Promise<SeededAsk[]> {
  // The issues are independent of one another - their titles share no terms - so they are created
  // together; the asks stay serial because their order is what the Inbox lists.
  const issues = await Promise.all(
    Array.from({ length: count }, (_value, index) => {
      const title = TITLES[from + index];
      if (title === undefined) throw new Error("out of issue titles");
      return createIssue({ project: "CORE", title });
    })
  );
  const asks: SeededAsk[] = [];
  for (const [index, issue] of issues.entries()) {
    asks.push({
      ask: await createAsk(
        issue.key,
        {
          options: [{ label: "Ship" }, { label: "Hold" }],
          question: `${prefix} ask ${index + 1}`,
        },
        session
      ),
      issue,
    });
  }
  return asks;
}

/** Newest first in the Inbox, so a row's place is counted back from the last ask seeded. */
function askAtRow(asks: SeededAsk[], fromTop: number): SeededAsk {
  const ask = asks[asks.length - 1 - fromTop];
  if (ask === undefined) throw new Error("ask missing");
  return ask;
}

async function topOf(locator: Locator): Promise<number> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error("row has no box");
  return box.y;
}

async function nextFrame(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const frame = Promise.withResolvers<void>();
    requestAnimationFrame(() => frame.resolve());
    await frame.promise;
  });
}

/**
 * The row the viewport anchor's centre fallback lands on, read the way the anchor reads it
 * (`rowAtCenter`): the probe is the centre of what the list shows, which is the list column's
 * own box where the rows scroll inside one and the window's where they do not.
 */
async function rowAtCentre(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const list = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
    const root = list?.firstElementChild;
    if (list === null || !(root instanceof HTMLElement)) return null;
    const rect = root.getBoundingClientRect();
    const box = list.scrollHeight > list.clientHeight ? list.getBoundingClientRect() : null;
    const top = Math.max(rect.top, box === null ? 0 : box.top);
    const bottom = Math.min(rect.bottom, box === null ? window.innerHeight : box.bottom);
    return (
      document
        .elementFromPoint(rect.left + rect.width / 2, (top + bottom) / 2)
        ?.closest("[data-inbox-row]")
        ?.getAttribute("data-inbox-row") ?? null
    );
  });
}

/** Twelve asks waiting on the reader, then four handed to agents, so Waiting on agents has rows
 *  below the one that will join it and the list scrolls at every width; the third of the
 *  reader's is the one under test. */
const BOTH_SECTIONS_ROWS = 16;
async function seedBothSections() {
  await createProject({ key: "CORE", name: "Core" });
  const waiting = await seedAsks("Yours", 12);
  const target = waiting[2];
  if (target === undefined) throw new Error("target ask missing");
  // The four clarifications are independent of one another and all land before the browser opens:
  // they are what puts their asks under Waiting on agents.
  await Promise.all(
    (await seedAsks("Theirs", 4, 12)).map(({ ask, issue }) =>
      createComment(issue.key, { ask_id: ask.id, body: "Clarify first." })
    )
  );
  return target;
}

/**
 * Takes the reader's hand off the row, so the list stops holding it where they last saw it.
 *
 * With the reading column on, the hand is the selection: Escape brings the focus back from the
 * column to the row it is showing, and `j` hands the selection to the next row, leaving the
 * list to place the one let go. Without the column the hand is focus and the pointer, so the
 * pointer goes to the page corner and the focus to nothing - not onto another row, which would
 * make that row the anchor, and not by Escape, whose step back to the row would scroll a tall
 * row into view on its own.
 */
async function leaveRow(page: Page): Promise<void> {
  if (hasReadingPane(page)) {
    await page.keyboard.press("Escape");
    await page.keyboard.press("j");
    return;
  }
  await page.mouse.move(0, 0);
  await page.evaluate(() => {
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
  });
  await expect
    .poll(() => page.evaluate(() => document.activeElement?.closest("[data-inbox-row]") === null))
    .toBe(true);
}

/** The address both turn-hold rows below open: the Inbox banded by whose turn it is
 *  (`?group=waiting-on`), which is the banding a flipped turn moves a row between. The list's
 *  own default bands by priority, where a progress note moves no row at all, so the hold under
 *  the reader's hand would have nothing to hold. */
const WAITING_ON_INBOX = "/?group=waiting-on";

test("a row being typed into keeps its draft when an agent's progress note flips its turn, and holds its place only while the reader's hand is on it @phone", async ({
  browser,
}) => {
  const target = await seedBothSections();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(WAITING_ON_INBOX);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(BOTH_SECTIONS_ROWS);
    const row = inboxRow(page, target.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    await centreRow(row);
    // The question's own card, where this width draws it: in the row, or in the reading column
    // once the row is selected - which is also what holds the row there.
    const field = (await askCard(page, target.ask.id)).getByLabel("Your answer");
    await field.click();
    await field.fill("Ship it once the audit lands");
    const before = await topOf(row);
    const scrolled = await listScrollTop(page);

    await createComment(
      target.issue.key,
      { ask_id: target.ask.id, body: "Checking the audit now.", turn: "agent" },
      session
    );

    // The card says whose turn it is now, and what the reader is typing is untouched by it.
    await expect(page.getByTestId(`turn-${target.ask.id}`)).toHaveText(
      "Waiting on e2e-session-title"
    );
    await expect(field).toHaveValue("Ship it once the audit lands");
    await expect(field).toBeFocused();
    if (hasReadingPane(page)) {
      // The question is in the column, so the reader's hand is on no row at all: the row joins
      // Waiting on agents at once, telling the truth about whose turn it is, and the column
      // keeps the question and the half-written answer where they are (INFRA-575). The view
      // does not follow the row down.
      await expect(row).toHaveAttribute("data-inbox-section", "agent");
      expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
      return;
    }
    // Without the column the card is in the row, under the reader's hand: the row stays where
    // they are typing.
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
    expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);

    await leaveRow(page);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
    // The card never left the row it moved with, so what was typed into it is still there.
    await expect(field).toHaveValue("Ship it once the audit lands");
  } finally {
    await alice.close();
  }
});

test("the reader's own Ask back leaves the view still and holds the row only where their hand is @phone", async ({
  browser,
}) => {
  const target = await seedBothSections();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(WAITING_ON_INBOX);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(BOTH_SECTIONS_ROWS);
    const row = inboxRow(page, target.ask.id);
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    // Scrolled, so the centre fallback is live too: on release nothing may follow the row down.
    await centreRow(row);
    const card = await askCard(page, target.ask.id);
    const field = card.getByLabel("Your answer");
    await field.click();
    await field.fill("Ship what, exactly?");
    const askBack = card.getByRole("button", { name: "Ask back" });
    // Measured with the button already in view, so the click itself scrolls nothing.
    await askBack.hover();
    const before = await topOf(row);
    const scrolled = await listScrollTop(page);
    expect(scrolled).toBeGreaterThan(0);

    await askBack.click();
    await expect(
      card.getByTestId(`thread-${target.ask.id}`).getByText("Ship what, exactly?")
    ).toBeVisible();
    await expect.poll(() => getAsk(target.ask.id)).toMatchObject({ ask: { waiting_on: "agent" } });
    await expect(page.getByTestId(`turn-${target.ask.id}`)).toHaveText(
      "Waiting on e2e-session-title"
    );

    if (hasReadingPane(page)) {
      // The question is in the column, which stays on it: the selection is the reader's and
      // only they move it. The row beside it joins Waiting on agents at once - their hand is in
      // the column, on no row (INFRA-575) - and the view does not follow it.
      await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", target.ask.id);
      await expect(row).toHaveAttribute("data-inbox-section", "agent");
      expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
      return;
    }
    // Without the column the row is still under Waiting on you, where the reader's hand is -
    // the focus Ask back handed back to the row as it disabled itself - and the view has not
    // moved after it.
    await expect(row).toBeFocused();
    await expect(row).toHaveAttribute("data-inbox-section", "human");
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
    const afterAskBack = await listScrollTop(page);
    expect(Math.abs(afterAskBack - scrolled)).toBeLessThanOrEqual(2);

    // Letting go of it: it joins Waiting on agents below; the view stays where the reader was.
    await leaveRow(page);
    await expect(row).toHaveAttribute("data-inbox-section", "agent");
    expect(Math.abs((await listScrollTop(page)) - scrolled)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

test("the row at the viewport centre stays put when a P0 ask arrives above it @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const asks = await seedAsks("Queue", 16);
  const sixth = asks[5];
  if (sixth === undefined) throw new Error("sixth ask missing");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(16);
    const row = inboxRow(page, sixth.ask.id);
    await centreRow(row);
    const before = await topOf(row);
    expect(before).toBeGreaterThan(0);

    const urgent = await createIssue({ project: "CORE", title: "Meridian" });
    await patchIssue(urgent.key, { priority: 0 });
    const p0 = await createAsk(urgent.key, { question: "Urgent ask" }, session);

    const arrived = inboxRow(page, p0.id);
    await expect(arrived).toBeAttached();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(17);
    expect(await topOf(arrived)).toBeLessThan(before);
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a twenty-row Inbox seeded ask by ask, two answers and two scroll-and-measure
// cycles.
test("the row under the pointer stays put when a row above it is answered elsewhere, and is let go once the pointer leaves the list @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Long enough to scroll in whichever box holds it, with rows above and below each of the four
  // below, so every one of them can be brought to the centre of what is visible.
  const asks = await seedAsks("Queue", 20);
  const above = askAtRow(asks, 4);
  const centreAsk = askAtRow(asks, 9);
  const between = askAtRow(asks, 10);
  const held = askAtRow(asks, 11);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(20);
    const row = inboxRow(page, held.ask.id);
    await centreRow(row);
    const box = await row.boundingBox();
    if (box === null) throw new Error("row has no box");
    await page.mouse.move(box.x + box.width / 2, box.y + 8);
    const before = box.y;

    await answerAsk(
      above.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );

    await expect(inboxRow(page, above.ask.id)).toHaveCount(0);
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);

    // With the pointer off the list, the anchor falls back to the row nearest the centre. That
    // row sits above the one the pointer left with one row between them: answering the row
    // between leaves the centre row still and the row the pointer left rising into the gap -
    // were the pointer still counted, it would hold and the centre row drop.
    await page.mouse.move(0, 0);
    const centred = inboxRow(page, centreAsk.ask.id);
    await centreRow(centred);
    await expect.poll(() => rowAtCentre(page)).toBe(centreAsk.ask.id);
    const leftTop = await topOf(row);
    const centreTop = await topOf(centred);
    await answerAsk(
      between.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );
    await expect(inboxRow(page, between.ask.id)).toHaveCount(0);
    expect(Math.abs((await topOf(centred)) - centreTop)).toBeLessThanOrEqual(2);
    expect(leftTop - (await topOf(row))).toBeGreaterThan(2);
  } finally {
    await alice.close();
  }
});

test("a band's header stays with its pointer-held row while a P0 ask arrives", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Older rows under the pair, so the list runs past the bottom of the viewport and has
  // somewhere to scroll when the arrival pushes the pair down. They carry no priority, so the
  // list's own banding puts them in the band under the P1 pair.
  await seedAsks("Below", 14);
  const banded = await createIssue({ project: "CORE", title: "Banded stability" });
  await patchIssue(banded.key, { priority: 1 });
  await createAsk(banded.key, { question: "Banded first" }, session);
  const held = await createAsk(banded.key, { question: "Banded held" }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = inboxRow(page, held.id);
    // The P1 band's own heading, which the arriving P0 band pushes down with its rows.
    const header = page.getByRole("button", { name: "P1 (2)" });
    await expect(row).toBeVisible();
    await expect(header).toBeVisible();
    const box = await row.boundingBox();
    if (box === null) throw new Error("held row has no box");
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    const rowBefore = await topOf(row);
    const headerBefore = await topOf(header);

    const urgent = await createIssue({ project: "CORE", title: "Ungrouped P0 arrival" });
    await patchIssue(urgent.key, { priority: 0 });
    const p0 = await createAsk(urgent.key, { question: "Urgent arrival" }, session);

    await expect(inboxRow(page, p0.id)).toBeAttached();
    expect(Math.abs((await topOf(row)) - rowBefore)).toBeLessThanOrEqual(2);
    expect(Math.abs((await topOf(header)) - headerBefore)).toBeLessThanOrEqual(2);

    await leaveRow(page);
  } finally {
    await alice.close();
  }
});

async function seedTravellingPointerBand() {
  await createProject({ key: "CORE", name: "Core" });
  const banded = await createIssue({ project: "CORE", title: "Banded travelling pointer asks" });
  await patchIssue(banded.key, { priority: 1 });
  await createAsk(banded.key, { question: "Travelling pointer banded first" }, session);
  await createAsk(banded.key, { question: "Travelling pointer banded second" }, session);
  await seedAsks("Travelling pointer", 6);
}

/**
 * The reader's hand, travelling across the list in the page's own time.
 *
 * `page.mouse.move` moves the pointer only when the test's next round trip reaches the browser,
 * so between two of them the page sees a pointer standing perfectly still. On a loaded machine
 * that stillness is long enough to end the travel this row is about in two ways, both measured
 * here: `useMotionHold`'s own settle (`web/src/features/inbox/motion-hold.ts`, 400 ms with no
 * `pointermove` on the list root) adopts the arrival mid-journey, and a rest of
 * `REF_PREVIEW_OPEN_DELAY_MS` (300 ms) over a row opens that row's reference card over the
 * pointer, whose `pointerleave` on the same root drops the hold outright.
 *
 * A hand that is actually travelling does neither, so the travel is driven from inside the
 * page: a timer walks the pointer along the segment the test is driving it through and
 * dispatches a real `pointermove` at each step, on the element at that point, so it bubbles to
 * the same root listener `useMotionHold` binds - counted there, and asserted by the caller, so
 * a move that stopped short of that root cannot pass for travel. The real pointer still makes
 * the journey, since it alone hovers rows and answers `elementFromPoint`; both walk one column
 * no point of which carries a reference trigger (`columnWithoutACard`), so no card is ever
 * drawn over the line they are on, wherever the hand happens to pause.
 */
declare global {
  interface Window {
    /** The travel `startTravelling` runs until `stopTravelling` ends it. */
    __travellingPointer?: {
      /** The keeper's own moves that reached the list root. */
      arrived: number;
      /** The keeper's moves, wherever they landed. */
      dispatched: number;
      headFor: (y: number) => void;
      stop: () => void;
    };
    /** One entry per time the held row heard the pointer leave it, true where it left for the
     *  one hover card (the last row below). */
    __leftTheHeldRow?: boolean[];
  }
}

/** How far the hand travels between two moves, and how often it moves: about a short row every
 *  half second, which is a hand crossing the list rather than flicking across it. */
const TRAVEL_STEP_PX = 4;
const TRAVEL_EVERY_MS = 16;

/** Starts the travel at `from`, heading down the column at `x` for `to`, sweeping that segment
 *  until it is pointed somewhere else or stopped. */
async function startTravelling(page: Page, x: number, from: number, to: number): Promise<void> {
  await page.evaluate(
    ({ every, from, step, to, x }) => {
      const root = document.querySelector('[data-testid="inbox-list"]')?.firstElementChild;
      if (!(root instanceof HTMLElement)) throw new Error("the Inbox list has no motion-hold root");
      let y = from;
      let aim = to;
      let origin = from;
      const travel = {
        arrived: 0,
        dispatched: 0,
        headFor: (next: number) => {
          origin = y;
          aim = next;
        },
        stop: () => {
          window.clearInterval(timer);
          root.removeEventListener("pointermove", count);
        },
      };
      // Only the keeper's own moves: a real one the test made is the browser's, and trusted.
      const count = (event: Event) => {
        if (!event.isTrusted) travel.arrived += 1;
      };
      root.addEventListener("pointermove", count);
      const timer = window.setInterval(() => {
        const left = aim - y;
        if (Math.abs(left) <= step) {
          // The end of this leg: the hand sweeps back along it rather than coming to rest.
          y = aim;
          [aim, origin] = [origin, aim];
        } else {
          y += Math.sign(left) * step;
        }
        const under = document.elementFromPoint(x, y) ?? root;
        under.dispatchEvent(
          new PointerEvent("pointermove", {
            bubbles: true,
            clientX: x,
            clientY: y,
            isPrimary: true,
            pointerId: 1,
            pointerType: "mouse",
            view: window,
          })
        );
        travel.dispatched += 1;
      }, every);
      window.__travellingPointer = travel;
    },
    { every: TRAVEL_EVERY_MS, from, step: TRAVEL_STEP_PX, to, x }
  );
}

/** Points the travel at a new end, so it follows the journey the test is driving. */
async function travelTowards(page: Page, to: number): Promise<void> {
  await page.evaluate((y) => {
    const travel = window.__travellingPointer;
    if (travel === undefined) throw new Error("no travel to point anywhere");
    travel.headFor(y);
  }, to);
}

/** Ends the travel, and says how many moves it made and how many of them reached the root the
 *  hold listens on. Ending one that is already over is nothing to do. */
async function stopTravelling(page: Page): Promise<{ arrived: number; dispatched: number }> {
  return page.evaluate(() => {
    const travel = window.__travellingPointer;
    if (travel === undefined) return { arrived: 0, dispatched: 0 };
    travel.stop();
    window.__travellingPointer = undefined;
    return { arrived: travel.arrived, dispatched: travel.dispatched };
  });
}

/** How many answers to `GET /api/v1/inbox` the page has finished reading: the arrival's own
 *  answer is in the page from the moment this grows, whether or not the list has adopted it. */
async function inboxAnswersRead(page: Page): Promise<number> {
  return page.evaluate(
    () =>
      performance
        .getEntriesByType("resource")
        .filter((entry) => new URL(entry.name).pathname === "/api/v1/inbox").length
  );
}

/**
 * The column the hand travels down, and where on it the journey ends: an `x` at which no point
 * of the journey carries a reference trigger, inside the list from end to end.
 *
 * A row's owner link is a reference trigger (`features/inbox/AskOwnerLink.tsx`), and a hand
 * that rests on one for `REF_PREVIEW_OPEN_DELAY_MS` has that row's card drawn `fixed` over the
 * page, outside the list, covering the column the keeper is walking down: from there
 * `elementFromPoint` hands the keeper's moves to the card, which is no child of the list root,
 * and they never reach the hold at all - the row would be deciding itself by where the rows
 * happened to fall rather than by the hold. The whole line is checked, not only the places the
 * route means to pause, because a `page.mouse.move` of many steps is as still between two of
 * them as it is at either end, and on a loaded machine one of those steps is a rest like any
 * other. Scanned outwards from `x`, so the hand still travels down the middle of the rows it
 * crosses, and the end of the journey must land in the row it set out for.
 */
async function columnWithoutACard(
  page: Page,
  { from, rowID, to, x }: { from: number; rowID: string; to: number; x: number }
): Promise<{ x: number; y: number }> {
  const column = await page.evaluate(
    ({ from, rowID, to, x }) => {
      const root = document.querySelector('[data-testid="inbox-list"]')?.firstElementChild;
      if (!(root instanceof HTMLElement)) throw new Error("the Inbox list has no motion-hold root");
      const clear = (at: number, y: number, row?: string) => {
        const under = document.elementFromPoint(at, y);
        if (under === null || !root.contains(under)) return false;
        if (under.closest("[data-dispatch-ref],[data-dispatch-href]") !== null) return false;
        const inRow = under.closest("[data-inbox-row]")?.getAttribute("data-inbox-row");
        return row === undefined || inRow === row;
      };
      const line: number[] = [];
      for (let y = Math.min(from, to); y <= Math.max(from, to); y += 4) line.push(y);
      for (let out = 0; out * 12 < window.innerWidth; out += 1)
        for (const at of out === 0 ? [x] : [x + out * 12, x - out * 12])
          if (line.every((y) => clear(at, y)) && clear(at, to, rowID)) return at;
      return null;
    },
    { from, rowID, to, x }
  );
  if (column === null) throw new Error("the journey has no column free of reference triggers");
  return { x: column, y: to };
}

async function travelPointerAcrossIncomingRow(page: Page): Promise<void> {
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(8);
  expect(await listScrollTop(page)).toBe(0);
  const sixth = rows.nth(5);
  const initialSixthBox = await sixth.boundingBox();
  const viewport = page.viewportSize();
  if (initialSixthBox === null || viewport === null) throw new Error("sixth row has no box");
  // Keep the destination on screen while leaving rows below it, so the anchor can still scroll.
  await page.setViewportSize({
    height: Math.ceil(initialSixthBox.y + initialSixthBox.height + 40),
    width: viewport.width,
  });
  const third = await rows.nth(2).boundingBox();
  const fourth = await rows.nth(3).boundingBox();
  const fifth = await rows.nth(4).boundingBox();
  const sixthBox = await sixth.boundingBox();
  if (third === null || fourth === null || fifth === null || sixthBox === null)
    throw new Error("Inbox row has no box");
  const sixthID = await sixth.getAttribute("data-inbox-row");
  if (sixthID === null) throw new Error("sixth row has no id");
  const destination = page.locator(`[data-inbox-row="${sixthID}"]`);
  const sixthBefore = await topOf(destination);
  const x = sixthBox.x + sixthBox.width / 2;
  // The journey: from the gap above the arrival's landing place, across the fourth and fifth
  // rows, to the middle of the destination row. Hand and keeper share one column, and it is a
  // column no part of which can open a card over them (`columnWithoutACard`), so where the
  // rows happen to fall decides nothing here.
  const gapAbove = (third.y + third.height + fourth.y) / 2;
  const gap = (fourth.y + fourth.height + fifth.y) / 2;
  const rest = await columnWithoutACard(page, {
    from: gapAbove,
    rowID: sixthID,
    to: sixthBox.y + sixthBox.height / 2,
    x,
  });
  const column = rest.x;
  const requested = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const delivered = Promise.withResolvers<void>();
  let hold = true;
  await page.route("**/api/v1/inbox", async (route) => {
    const response = await route.fetch();
    const listed = (await response.json()) as Array<{ question?: string }>;
    if (!hold || !listed.some((row) => row.question === "Travelling pointer arrival")) {
      await route.fulfill({ json: listed, response });
      return;
    }
    hold = false;
    requested.resolve();
    await release.promise;
    await route.fulfill({ json: listed, response });
    delivered.resolve();
  });

  try {
    // The hand sets out above the arrival's landing place and travels across the fourth row,
    // on its way down to the destination, and it keeps travelling from here to the last
    // assertion the hold has to survive.
    await page.mouse.move(column, gapAbove);
    await startTravelling(page, column, gapAbove, gap);
    const answersBefore = await inboxAnswersRead(page);

    const urgent = await createIssue({ project: "CORE", title: "Travelling pointer P0" });
    await patchIssue(urgent.key, { priority: 0 });
    const p0 = await createAsk(urgent.key, { question: "Travelling pointer arrival" }, session);
    await requested.promise;
    // Seeding can outlast a round trip, so the real pointer crosses the fourth row in one
    // journey of its own, and the refetch lands inside that travel.
    await page.mouse.move(column, gap, { steps: 16 });
    release.resolve();
    await delivered.promise;

    // The answer carrying the arrival is in the page, and the list is still showing the eight
    // rows the reader was travelling across: the hold is holding, which is the whole row.
    await expect.poll(() => inboxAnswersRead(page)).toBeGreaterThan(answersBefore);
    await nextFrame(page);
    expect(await rows.count()).toBe(8);

    // The last leg, down across the fifth row to the destination the hand set out for.
    await travelTowards(page, rest.y);
    await page.mouse.move(rest.x, rest.y, { steps: 24 });
    const travel = await stopTravelling(page);
    // What the keeper dispatched is what the hold heard: every one of its moves reached the
    // root `useMotionHold` listens on, and there were moves to hear.
    expect(travel.dispatched).toBeGreaterThan(0);
    expect(travel.arrived).toBe(travel.dispatched);

    await expect(page.locator(`[data-inbox-row="${p0.id}"]`)).toBeAttached();
    await expect(rows).toHaveCount(9);
    const underPointer = await page.evaluate(
      ({ x, y }) =>
        document
          .elementFromPoint(x, y)
          ?.closest("[data-inbox-row]")
          ?.getAttribute("data-inbox-row"),
      rest
    );
    expect(underPointer).toBe(sixthID);
    expect(Math.abs((await topOf(destination)) - sixthBefore)).toBeLessThanOrEqual(2);
  } finally {
    await stopTravelling(page);
    release.resolve();
    await page.unroute("**/api/v1/inbox");
  }
}

// SLOW(>=2s): nine seeded writes, a viewport resize and ~19 pointer-to-frame round trips.
test("a travelling pointer keeps its destination when a P0 row arrives @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await seedAsks("Travelling pointer", 8);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await travelPointerAcrossIncomingRow(page);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): eleven seeded writes, a viewport resize and ~19 pointer-to-frame round trips.
test("a travelling pointer crosses a band of its own without losing its destination @phone", async ({
  browser,
}) => {
  await seedTravellingPointerBand();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await travelPointerAcrossIncomingRow(page);
  } finally {
    await alice.close();
  }
});

/**
 * The hand that stops, which is what the travelling rows above leave out: they travel a column
 * no card can open on (`columnWithoutACard`), because they measure the hold under travel. A
 * hand that comes to rest on a row's own reference - its owner link, the one trigger a row
 * carries (`features/inbox/AskOwnerLink.tsx`) - has that row's card open over the page after
 * `REF_PREVIEW_OPEN_DELAY_MS`, and its next move lands inside the card, which is `fixed` and
 * outside the list. The reader has not left the list: the card is the row's own doing. So the
 * row stays the reader's place, and a refetch landing while they read the card leaves it where
 * they are looking at it.
 *
 * The row under the pointer is below the one at the centre of what is shown, with one row
 * between them, so the two answers differ: with the pointer counted the row under it stays and
 * the centre row drops, and with it dropped the centre row stays and the row under the pointer
 * rises into the answered row's place.
 */
test("the row whose reference card the pointer moved onto stays put when a row above it is answered elsewhere", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const asks = await seedAsks("Carded", 20);
  const centreAsk = askAtRow(asks, 9);
  const between = askAtRow(asks, 10);
  const held = askAtRow(asks, 11);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(20);
    const centred = inboxRow(page, centreAsk.ask.id);
    await centreRow(centred);
    await expect.poll(() => rowAtCentre(page)).toBe(centreAsk.ask.id);

    // The hand comes to rest on the row's owner link and waits for its card.
    const row = inboxRow(page, held.ask.id);
    const reference = row.locator("[data-dispatch-ref]").first();
    await reference.hover();
    const card = page.getByRole("tooltip");
    await expect(card).toBeVisible();
    await expect(reference).toHaveAttribute("aria-describedby", "ref-preview");
    // The card is open over the pointer, so the next move lands inside it: the list hears
    // `pointerleave`, and the pointer is now outside every row it draws.
    await card.hover();
    const heldBefore = await topOf(row);
    const centreBefore = await topOf(centred);

    await answerAsk(
      between.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );

    await expect(inboxRow(page, between.ask.id)).toHaveCount(0);
    expect(Math.abs((await topOf(row)) - heldBefore)).toBeLessThanOrEqual(2);
    // The list was held to the reader's row rather than to the centre, which dropped by the
    // answered row's height instead.
    expect((await topOf(centred)) - centreBefore).toBeGreaterThan(2);
  } finally {
    await alice.close();
  }
});

/**
 * The row the Inbox is holding for the hand that is on it, when that hand moves onto the card
 * the row's own reference opened.
 *
 * A held row is one the server has dropped - answered somewhere else - that the Inbox keeps
 * drawn where the reader last saw it for as long as their hand is on it (`heldRow`). The card
 * is `fixed` and portaled into `document.body`, outside the list, so a hand resting on the
 * row's owner link - the one reference trigger a row carries
 * (`features/inbox/AskOwnerLink.tsx`) - has that card open beside it after
 * `REF_PREVIEW_OPEN_DELAY_MS`, and its next move onto the card is a `pointerleave` on the row.
 * The reader has left nothing: the card is the row's own doing. So the held row stays on the
 * screen rather than being taken away while they read about it.
 *
 * The row hears that leave - `InboxRowShell`'s `onPointerLeave` - and what it does with it is
 * re-render, which has the Inbox choose the held row afresh from `ViewportAnchor.interacted()`.
 * That is the one place the decision lives, and it does not count the move onto a row's own
 * card as leaving the row, so `InboxRowShell` needs no card exception of its own: this row
 * fails the moment the anchor's does not hold.
 *
 * Then the real departure, which must still let it go: the hand leaves the card for the page
 * corner, the card closes behind it, and the next list the page reads is drawn without the row.
 */
test("a held row stays drawn when the pointer moves onto the card its own reference opened", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const asks = await seedAsks("Holding", 8);
  const held = askAtRow(asks, 3);
  const other = askAtRow(asks, 6);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(8);
    // The one number the Inbox entry carries, read off the server's own list rather than the
    // list the Inbox draws: it says when the page has read the answer below.
    const inboxCount = page
      .getByRole("navigation", { name: "Navigation" })
      .getByRole("link", { name: "Inbox" });
    await expect(inboxCount).toContainText("8");

    // The hand comes to rest on the row's owner link and waits for its card.
    const row = inboxRow(page, held.ask.id);
    const reference = row.locator("[data-dispatch-ref]").first();
    await reference.hover();
    const card = page.getByRole("tooltip");
    await expect(card).toBeVisible();
    await expect(reference).toHaveAttribute("aria-describedby", "ref-preview");
    const before = await topOf(row);

    // Bob answers it in his own tab: the server drops it from the list, and the hand on it
    // holds it where the reader last saw it - seven listed rows drawn as eight.
    await answerAsk(
      held.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );
    await expect(inboxCount).toContainText("7");
    await expect(row).toBeVisible();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(8);

    // The row must really hear the pointer leave it for the card, or this row would pass
    // without ever reaching the rule it is about - the same guard the travelling rows keep over
    // their own moves. `pointerleave` is the event `InboxRowShell` hands the held row on.
    await page.evaluate((id) => {
      const node = document.querySelector<HTMLElement>(`[data-inbox-row="${id}"]`);
      if (node === null) throw new Error("held row missing");
      const left: boolean[] = [];
      window.__leftTheHeldRow = left;
      node.addEventListener("pointerleave", (event) => {
        const related = event.relatedTarget;
        left.push(related instanceof Element && related.closest("#ref-preview") !== null);
      });
    }, held.ask.id);

    // The move onto the card, which the row hears as the pointer leaving it.
    await card.hover();
    await nextFrame(page);
    expect(await page.evaluate(() => window.__leftTheHeldRow)).toEqual([true]);
    await expect(reference).toHaveAttribute("aria-describedby", "ref-preview");
    await expect(row).toBeVisible();
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(8);

    // The hand leaves for the page corner, which is neither the row nor its card.
    await page.mouse.move(0, 0);
    await expect(card).toHaveCount(0);
    await answerAsk(
      other.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );
    await expect(inboxRow(page, other.ask.id)).toHaveCount(0);
    await expect(row).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

/**
 * The Inbox drawer holds the row the reader's hand is on, as the Inbox page does.
 *
 * The drawer mounts the same `Inbox` with `pane={false}` (`InboxDrawer`), and the page passes
 * `ViewportAnchor` its scroller only while the reading pane is shown
 * (`scroller={paneShown ? listScroller : undefined}`, `Inbox.tsx`). In the drawer the rows
 * scroll inside the drawer's own `aside`, which the anchor is never given, so it reads
 * `window.scrollY` - 0 there - and `snapshot()` returns null: Dispatch's own hold is inert and
 * the reader is left with whatever the browser's scroll anchoring picks. Measured at 1280x720
 * with the hand resting on a row and a row above it answered elsewhere, the row under the hand
 * jumps one card height, 91 px, in five runs of eight; the drawer's `aside` compensates 425 px
 * where holding the row needs 516. The page, driven identically, never moved in twelve runs.
 *
 * The jump is not certain on any one run: the two outcomes are exactly "held" and "one card
 * out", and in this row's own shape it is "one card out" in three runs of four. Whoever fixes
 * it runs this row under `--repeat-each=8` rather than once, because one green run of it
 * proves nothing. Every failure measured fell on the first answer, so one answer is what the
 * row makes.
 */
test("the Inbox drawer holds the row the hand rests on when a row above it is answered", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const asks = await seedAsks("Drawered", 20);
  const held = askAtRow(asks, 11);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").click({ position: { x: 5, y: 400 } });
    await page.keyboard.press("i");
    const drawer = page.getByRole("dialog", { name: "Inbox" });
    await expect(drawer).toBeVisible();
    await expect(drawer.locator("[data-inbox-row]")).toHaveCount(20);

    const row = drawer.locator(`[data-inbox-row="${held.ask.id}"]`);
    await row.scrollIntoViewIfNeeded();

    await row.hover();
    const before = await topOf(row);
    const above = askAtRow(asks, 10);
    await answerAsk(
      above.ask.id,
      { expected_edited_at: null, selected: ["Ship"], text: "" },
      { login: "bob" }
    );
    await expect(drawer.locator(`[data-inbox-row="${above.ask.id}"]`)).toHaveCount(0, {
      timeout: 20_000,
    });
    expect(Math.abs((await topOf(row)) - before)).toBeLessThanOrEqual(2);
  } finally {
    await alice.close();
  }
});
